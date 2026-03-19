// codeindex.go — Semantic code indexing using tree-sitter symbol boundaries.
//
// 7.7: Extends the embeddings store to index source code by function/class/method.
// Each chunk is a symbol's signature + body (capped at 500 chars).
// Content hash guard prevents re-indexing unchanged files.
//
// Source: Cline three-tier retrieval + Aider repo-map. AST-based chunking
// outperforms character-based splitting by 15-30% on code retrieval tasks.
package embeddings

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/seedhire/mantis/internal/parser"
)

// defaultCodeIgnore directories skipped during source indexing.
var defaultCodeIgnore = map[string]bool{
	"vendor": true, "node_modules": true, ".git": true, ".mantis": true,
	"__pycache__": true, "dist": true, "build": true, ".next": true,
	"target": true, // Rust/Java
}

// IndexSourceFiles walks the project root, parses source files using the
// provided parsers, and indexes each symbol (function, class, method) into
// the embeddings store. Skips vendor/, generated/, and test files by default.
//
// Returns the count of symbols indexed.
func (s *Store) IndexSourceFiles(ctx context.Context, root string, parsers []parser.Parser, ignoreTests bool) (int, error) {
	// Build extension → parser map.
	extMap := map[string]parser.Parser{}
	for _, p := range parsers {
		for _, ext := range p.Extensions() {
			extMap[ext] = p
		}
	}

	var indexed int
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if d.IsDir() {
			if defaultCodeIgnore[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}

		// Skip test files if requested.
		if ignoreTests && isTestFile(path) {
			return nil
		}

		ext := filepath.Ext(path)
		p, ok := extMap[ext]
		if !ok {
			return nil
		}

		content, err := os.ReadFile(path)
		if err != nil {
			return nil
		}

		// Check if file has changed since last index via content hash.
		fileHash := fmt.Sprintf("%x", sha256.Sum256(content))
		rel, _ := filepath.Rel(root, path)
		if rel == "" {
			rel = path
		}
		hashKey := "codeidx:" + rel
		var existingHash string
		_ = s.db.QueryRow(`SELECT content_hash FROM chunks WHERE id = ?`, hashKey).Scan(&existingHash)
		if existingHash == fileHash {
			return nil // unchanged
		}

		// Parse symbols.
		result, parseErr := p.ParseFile(path, content)
		if parseErr != nil || result == nil {
			return nil
		}

		// Delete old chunks for this file before re-indexing.
		_, _ = s.db.Exec(`DELETE FROM chunks WHERE source = ? AND id LIKE 'code:%'`, rel)

		// Index each symbol.
		lines := strings.Split(string(content), "\n")
		var firstErr error
		for _, sym := range result.Symbols {
			if sym.LineStart <= 0 {
				continue
			}
			// Extract the symbol's source code (capped).
			chunk := extractSymbolChunk(lines, sym.LineStart, sym.LineEnd, 500)
			if chunk == "" {
				continue
			}

			id := fmt.Sprintf("code:%s#%s", rel, sym.Name)
			label := fmt.Sprintf("%s:%s", sym.Type, sym.Name)

			if err := s.Add(ctx, id, rel, label, chunk); err != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("embedding %s: %w", id, err)
				}
				continue
			}
			indexed++
		}
		// Surface first embedding error so caller can warn the user.
		if indexed == 0 && firstErr != nil {
			return firstErr
		}

		// Store file-level hash marker so we can skip unchanged files on next run.
		_, _ = s.db.Exec(
			`INSERT OR REPLACE INTO chunks (id, source, section_label, content_hash, text, embedding)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			hashKey, rel, "file-hash", fileHash, "", []byte{},
		)

		return nil
	})

	return indexed, err
}

// extractSymbolChunk extracts source lines for a symbol, capped at maxChars.
func extractSymbolChunk(lines []string, startLine, endLine, maxChars int) string {
	if startLine > len(lines) {
		return ""
	}
	start := startLine - 1 // 0-based
	end := endLine
	if end <= 0 || end > len(lines) {
		end = len(lines)
	}
	if end <= start {
		end = start + 1
	}

	var sb strings.Builder
	for i := start; i < end; i++ {
		if sb.Len()+len(lines[i]) > maxChars {
			remaining := maxChars - sb.Len()
			if remaining < 0 {
				remaining = 0
			}
			sb.WriteString(lines[i][:min(remaining, len(lines[i]))])
			sb.WriteString("\n// ... truncated")
			break
		}
		sb.WriteString(lines[i])
		sb.WriteByte('\n')
	}
	return sb.String()
}

// isTestFile returns true if the file path looks like a test file.
func isTestFile(path string) bool {
	base := strings.ToLower(filepath.Base(path))
	return strings.HasSuffix(base, "_test.go") ||
		strings.HasSuffix(base, ".test.ts") ||
		strings.HasSuffix(base, ".test.js") ||
		strings.HasSuffix(base, ".spec.ts") ||
		strings.HasSuffix(base, ".spec.js") ||
		strings.HasPrefix(base, "test_") || // Python
		strings.HasSuffix(base, "_test.py")
}
