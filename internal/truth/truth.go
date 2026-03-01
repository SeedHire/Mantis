// Package truth maintains GROUND_TRUTH.json — a live index of function signatures,
// file hashes, and exported symbols extracted by tree-sitter.
// Updated in <50ms whenever a file is saved (hooks into the graph watcher).
// Injected into every AI prompt so the model can never hallucinate function names.
package truth

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/seedhire/mantis/internal/parser"
)

const filename = "GROUND_TRUTH.json"

// FuncSig is a function/method signature.
type FuncSig struct {
	Name    string `json:"name"`
	Params  string `json:"params,omitempty"`
	Returns string `json:"returns,omitempty"`
}

// FileEntry is one file's entry in the ground truth index.
type FileEntry struct {
	Hash            string    `json:"hash"`
	LastModified    string    `json:"last_modified"`
	Functions       []FuncSig `json:"functions,omitempty"`
	ExportedSymbols []string  `json:"exported_symbols,omitempty"`
	Imports         []string  `json:"imports,omitempty"`
}

// Index is the full GROUND_TRUTH.json document.
type Index map[string]FileEntry // file path → entry

// Writer writes and updates GROUND_TRUTH.json inside .mantis/.
type Writer struct {
	brainDir string
	parsers  map[string]parser.Parser
	mu       sync.Mutex
	index    Index
}

// New creates a Writer for the given project root.
func New(projectRoot string) *Writer {
	w := &Writer{
		brainDir: filepath.Join(projectRoot, ".mantis"),
		index:    make(Index),
	}
	w.parsers = map[string]parser.Parser{}
	for _, p := range []parser.Parser{
		&parser.TypeScriptParser{},
		&parser.PythonParser{Root: projectRoot},
		&parser.GoParser{Root: projectRoot},
	} {
		for _, ext := range p.Extensions() {
			w.parsers[ext] = p
		}
	}
	_ = w.load()
	return w
}

// UpdateFile re-indexes a single file and flushes to disk.
// Called by the watcher after every save (debounced 200ms).
func (w *Writer) UpdateFile(path string) {
	content, err := os.ReadFile(path)
	if err != nil {
		return
	}
	ext := filepath.Ext(path)
	p, ok := w.parsers[ext]
	if !ok {
		return
	}
	result, err := p.ParseFile(path, content)
	if err != nil || result == nil {
		return
	}

	hash := fmt.Sprintf("%x", sha256.Sum256(content))[:16]
	entry := FileEntry{
		Hash:         hash,
		LastModified: time.Now().UTC().Format(time.RFC3339),
	}

	for _, sym := range result.Symbols {
		if sym.Type == "function" || sym.Type == "method" {
			// Go parser stashes params/returns in ID as "id|params|returns"
			parts := strings.SplitN(sym.ID, "|", 3)
			fn := FuncSig{Name: sym.Name}
			if len(parts) == 3 {
				fn.Params = parts[1]
				fn.Returns = parts[2]
			}
			entry.Functions = append(entry.Functions, fn)
		}
		if sym.Exported {
			entry.ExportedSymbols = append(entry.ExportedSymbols, sym.Name)
		}
	}
	for _, imp := range result.Imports {
		entry.Imports = append(entry.Imports, imp.RawPath)
	}

	w.mu.Lock()
	w.index[path] = entry
	w.mu.Unlock()
	_ = w.flush()
}

// RemoveFile removes a file entry from the index.
func (w *Writer) RemoveFile(path string) {
	w.mu.Lock()
	delete(w.index, path)
	w.mu.Unlock()
	_ = w.flush()
}

// BuildFull indexes all supported files under root.
// Called once during init to seed the index.
func (w *Writer) BuildFull(root string) error {
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			b := d.Name()
			if b == ".git" || b == "node_modules" || b == "vendor" ||
				b == ".mantis" || b == "__pycache__" || b == "dist" || b == "build" ||
				b == "archive" || b == "vscode-extension" || b == "supabase" {
				return filepath.SkipDir
			}
			return nil
		}
		w.UpdateFile(path)
		return nil
	})
	return w.flush()
}

// ContextSnippet returns a compact text summary for AI system prompt injection.
// Uses default caps suitable for fast models.
func (w *Writer) ContextSnippet() string {
	return w.ContextSnippetN(15, 2000)
}

// ContextSnippetForTier returns a snippet scaled to the model tier's context capacity.
// Larger tiers get more symbols to reduce hallucination.
func (w *Writer) ContextSnippetForTier(tier string) string {
	switch tier {
	case "trivial", "fast":
		return w.ContextSnippetN(15, 2000)
	case "code":
		return w.ContextSnippetN(30, 4000)
	case "reason":
		return w.ContextSnippetN(50, 8000)
	case "heavy", "max":
		return w.ContextSnippetN(80, 16000)
	default:
		return w.ContextSnippetN(15, 2000)
	}
}

// ContextSnippetN returns a snippet capped to maxFiles files and maxChars characters.
func (w *Writer) ContextSnippetN(maxFiles, maxChars int) string {
	w.mu.Lock()
	defer w.mu.Unlock()

	if len(w.index) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("Live codebase symbols:\n")
	count := 0
	for file, entry := range w.index {
		if len(entry.Functions) == 0 {
			continue
		}
		if count >= maxFiles || sb.Len() >= maxChars {
			sb.WriteString("  ... (truncated)\n")
			break
		}
		sb.WriteString(fmt.Sprintf("  %s:\n", filepath.Base(file)))
		for _, fn := range entry.Functions {
			if sb.Len() >= maxChars {
				break
			}
			if fn.Params != "" || fn.Returns != "" {
				sb.WriteString(fmt.Sprintf("    %s%s %s\n", fn.Name, fn.Params, fn.Returns))
			} else {
				sb.WriteString(fmt.Sprintf("    %s\n", fn.Name))
			}
		}
		count++
	}
	return sb.String()
}

// SymbolExists reports whether a symbol name is present in any indexed file.
func (w *Writer) SymbolExists(name string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, entry := range w.index {
		for _, fn := range entry.Functions {
			if fn.Name == name {
				return true
			}
		}
		for _, sym := range entry.ExportedSymbols {
			if sym == name {
				return true
			}
		}
	}
	return false
}

// FindClosest returns the closest matching symbol names for a given unknown symbol.
// Uses simple prefix and substring matching to suggest corrections.
func (w *Writer) FindClosest(name string, limit int) []string {
	w.mu.Lock()
	defer w.mu.Unlock()

	lower := strings.ToLower(name)
	var matches []string
	seen := map[string]bool{}

	// Pass 1: prefix match (highest quality)
	for _, entry := range w.index {
		for _, fn := range entry.Functions {
			if !seen[fn.Name] && strings.HasPrefix(strings.ToLower(fn.Name), lower[:min(len(lower), 3)]) {
				matches = append(matches, fn.Name)
				seen[fn.Name] = true
			}
		}
		for _, sym := range entry.ExportedSymbols {
			if !seen[sym] && strings.HasPrefix(strings.ToLower(sym), lower[:min(len(lower), 3)]) {
				matches = append(matches, sym)
				seen[sym] = true
			}
		}
	}

	// Pass 2: substring match
	if len(matches) < limit {
		for _, entry := range w.index {
			for _, fn := range entry.Functions {
				if !seen[fn.Name] && strings.Contains(strings.ToLower(fn.Name), lower) {
					matches = append(matches, fn.Name)
					seen[fn.Name] = true
				}
			}
		}
	}

	if len(matches) > limit {
		matches = matches[:limit]
	}
	return matches
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// FileCount returns the number of indexed files.
func (w *Writer) FileCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.index)
}

func (w *Writer) flush() error {
	if err := os.MkdirAll(w.brainDir, 0o755); err != nil {
		return err
	}
	w.mu.Lock()
	data, err := json.MarshalIndent(w.index, "", "  ")
	w.mu.Unlock()
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(w.brainDir, filename), data, 0o644)
}

func (w *Writer) load() error {
	data, err := os.ReadFile(filepath.Join(w.brainDir, filename))
	if err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return json.Unmarshal(data, &w.index)
}
