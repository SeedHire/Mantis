package context

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/seedhire/mantis/internal/graph"
	"github.com/seedhire/mantis/internal/parser"
)

// BundleFile represents a single file in a context bundle.
type BundleFile struct {
	Path    string
	Depth   int
	Content string
}

// Bundle is the result of bundling a symbol's context.
type Bundle struct {
	Symbol   string
	Files    []BundleFile
	Tokens   int
	MaxDepth int
}

// Bundler assembles context bundles for symbols.
type Bundler struct {
	db      *graph.DB
	querier *graph.Querier
	root    string
}

// NewBundler creates a new Bundler.
func NewBundler(db *graph.DB, root string) *Bundler {
	return &Bundler{
		db:      db,
		querier: graph.NewQuerier(db),
		root:    root,
	}
}

// Bundle assembles a context bundle for the given symbol name.
func (b *Bundler) Bundle(symbolName string, maxDepth, tokenBudget int) (*Bundle, error) {
	nodes, err := b.querier.FindNodeByName(symbolName)
	if err != nil {
		return nil, err
	}
	if len(nodes) == 0 {
		return nil, fmt.Errorf("symbol %q not found", symbolName)
	}

	// Use the first match
	sym := nodes[0]

	// Get file node for the symbol
	fileNode, err := b.querier.GetFileNode(sym.FilePath)
	if err != nil || fileNode == nil {
		// Try using the file path directly
		fileNode = &graph.Node{
			ID:       parser.FileNodeID(sym.FilePath),
			FilePath: sym.FilePath,
		}
	}

	// BFS import traversal
	depthMap, err := b.querier.BFSImports(fileNode.ID, maxDepth)
	if err != nil {
		return nil, err
	}

	// Get importers (referenced-by files)
	importers, err := b.querier.GetImporters(fileNode.ID)
	if err != nil {
		return nil, err
	}

	// Build sections with multi-signal scoring.
	var sections []Section
	filesByID := map[string]BundleFile{}

	// Entry file base name for test co-location scoring.
	entryBase := baseWithoutExt(sym.FilePath)

	// Add BFS files
	for nodeID, depth := range depthMap {
		n, nErr := b.querier.GetNodeByID(nodeID)
		if nErr != nil || n == nil {
			continue
		}
		content, _ := os.ReadFile(n.FilePath)
		priority := scoreFile(n.FilePath, depth, string(content), n.LastModified, entryBase)
		bf := BundleFile{
			Path:    n.FilePath,
			Depth:   depth,
			Content: string(content),
		}
		filesByID[nodeID] = bf
		sections = append(sections, Section{
			Content:  string(content),
			Priority: priority,
			Label:    n.FilePath,
		})
	}

	// Add referenced-by files at depth maxDepth+1
	for _, imp := range importers {
		if _, already := depthMap[imp.ID]; already {
			continue
		}
		content, _ := os.ReadFile(imp.FilePath)
		bf := BundleFile{
			Path:    imp.FilePath,
			Depth:   maxDepth + 1,
			Content: string(content),
		}
		filesByID[imp.ID] = bf
		sections = append(sections, Section{
			Content:  string(content),
			Priority: scoreFile(imp.FilePath, maxDepth+1, string(content), imp.LastModified, entryBase),
			Label:    imp.FilePath,
		})
	}

	// Apply token budget
	trimmed := TrimToTokenBudget(sections, tokenBudget)
	trimmedSet := map[string]string{}
	for _, s := range trimmed {
		trimmedSet[s.Label] = s.Content
	}

	var files []BundleFile
	totalTokens := 0
	for _, bf := range filesByID {
		if content, ok := trimmedSet[bf.Path]; ok {
			bf.Content = content
			files = append(files, bf)
			totalTokens += EstimateTokens(content)
		}
	}
	return &Bundle{
		Symbol:   symbolName,
		Files:    files,
		Tokens:   totalTokens,
		MaxDepth: maxDepth,
	}, nil
}

// scoreFile computes a multi-signal relevance score for context ranking.
// Higher score = more likely to be kept under the token budget.
//
// Formula (inspired by Sourcegraph Cody weights):
//
//	score = depth_signal + size_signal + recency_signal + test_colocation + type_boost
func scoreFile(path string, depth int, content string, lastModifiedUnix int64, entryBase string) int {
	score := 0

	// Depth signal: closer = higher (10 → 3).
	switch depth {
	case 0:
		score += 10
	case 1:
		score += 8
	case 2:
		score += 5
	default:
		score += 3
	}

	// File size penalty: large files are less focused.
	size := len(content)
	switch {
	case size > 50000:
		score -= 3
	case size > 20000:
		score -= 2
	case size > 10000:
		score -= 1
	case size < 2000:
		score += 1 // small, focused files are good
	}

	// Recency signal: recently modified files are more relevant.
	// Ranges ~+2 for brand-new files to ~0 for files untouched for 3+ months.
	if lastModifiedUnix > 0 {
		daysSince := time.Since(time.Unix(lastModifiedUnix, 0)).Hours() / 24
		recency := int(2.0 / (1.0 + daysSince/30.0))
		score += recency
	}

	// Test file handling: co-located test gets a boost; unrelated tests are demoted.
	base := filepath.Base(path)
	isTest := strings.Contains(base, "_test.") || strings.Contains(base, ".test.") ||
		strings.Contains(base, ".spec.") || strings.HasSuffix(base, "_test.go")
	if isTest {
		fileBase := baseWithoutExt(path)
		// Strip _test suffix to get the base: auth_test → auth
		fileBase = strings.TrimSuffix(fileBase, "_test")
		if entryBase != "" && fileBase == entryBase {
			score += 3 // co-located test: relevant
		} else {
			score -= 4 // unrelated test: demote
		}
	}

	// Config/generated file demotion.
	lower := strings.ToLower(base)
	if lower == "package-lock.json" || lower == "yarn.lock" || lower == "go.sum" ||
		strings.HasSuffix(lower, ".min.js") || strings.HasSuffix(lower, ".generated.go") {
		score -= 5
	}

	// Interface/type files boost (likely important for understanding).
	if strings.Contains(lower, "types") || strings.Contains(lower, "interface") ||
		strings.Contains(lower, "model") || strings.Contains(lower, "schema") {
		score += 2
	}

	if score < 1 {
		score = 1
	}
	return score
}

// baseWithoutExt returns the file base name without directory or extension.
func baseWithoutExt(path string) string {
	base := filepath.Base(path)
	ext := filepath.Ext(base)
	return strings.TrimSuffix(base, ext)
}

// RenderMarkdown renders a context bundle as a Markdown document.
func (b *Bundler) RenderMarkdown(bundle *Bundle) string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("# Context Bundle: %s\n", bundle.Symbol))
	sb.WriteString(fmt.Sprintf("Generated: %s  |  Files: %d  |  Tokens: ~%d\n\n",
		time.Now().Format("2006-01-02 15:04:05"), len(bundle.Files), bundle.Tokens))

	type group struct {
		label string
		files []BundleFile
	}

	entry := group{label: "## Entry Point"}
	depth1 := group{label: "## Direct Dependencies (depth 1)"}
	depth2plus := group{label: "## Indirect Dependencies (depth 2+)"}
	refBy := group{label: "## Referenced By"}

	for _, f := range bundle.Files {
		switch f.Depth {
		case 0:
			entry.files = append(entry.files, f)
		case 1:
			depth1.files = append(depth1.files, f)
		default:
			if f.Depth > bundle.MaxDepth {
				refBy.files = append(refBy.files, f)
			} else {
				depth2plus.files = append(depth2plus.files, f)
			}
		}
	}

	// Most relevant context last (Lost in the Middle — Liu et al. 2023).
	// LLMs have recency bias; placing the entry point closest to the query improves utilisation.
	for _, g := range []group{refBy, depth2plus, depth1, entry} {
		if len(g.files) == 0 {
			continue
		}
		sb.WriteString(g.label + "\n\n")
		for _, f := range g.files {
			rel, err := filepath.Rel(b.root, f.Path)
			if err != nil {
				rel = f.Path
			}
			ext := strings.TrimPrefix(filepath.Ext(f.Path), ".")
			sb.WriteString(fmt.Sprintf("`%s`\n\n```%s\n%s\n```\n\n", rel, ext, f.Content))
		}
	}

	return sb.String()
}
