package repl

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/seedhire/mantis/internal/graph"
)

// filePathRe matches file paths like router.go, internal/auth/handler.go, ./cmd/main.go, src/utils.ts.
var filePathRe = regexp.MustCompile(`(?:^|\s|["'` + "`" + `(])((?:\./)?[a-zA-Z0-9_][\w/.\-]*\.(?:go|ts|tsx|js|jsx|py|rs|java|rb|c|cpp|h|css|scss|html|vue|svelte))(?:[:)\s"'` + "`" + `]|$)`)

// errorPathRe matches error output paths like internal/router/router.go:142: undefined: Foo
var errorPathRe = regexp.MustCompile(`([\w/.\-]+\.(?:go|ts|tsx|js|jsx|py|rs|java)):(\d+):`)

// symbolRe matches "the Classify function", "function Classify", "Classify()", etc.
var symbolRe = regexp.MustCompile(`(?:the\s+|function\s+|method\s+|type\s+|struct\s+|class\s+|interface\s+)([A-Z]\w+)|([A-Z]\w+)\(\)`)

// extractMentionedFiles detects file paths and symbol references in user input.
// Returns a deduplicated list of relative file paths mentioned or referenced.
func extractMentionedFiles(input string, querier *graph.Querier) []string {
	seen := map[string]bool{}
	var files []string

	add := func(path string) {
		path = filepath.Clean(path)
		if !seen[path] {
			seen[path] = true
			files = append(files, path)
		}
	}

	// 1. Explicit file paths.
	for _, m := range filePathRe.FindAllStringSubmatch(input, -1) {
		if len(m) >= 2 {
			add(m[1])
		}
	}

	// 2. Error output paths (file.go:line:).
	for _, m := range errorPathRe.FindAllStringSubmatch(input, -1) {
		if len(m) >= 2 {
			add(m[1])
		}
	}

	// 3. Symbol mentions → resolve to files via graph.
	if querier != nil {
		for _, m := range symbolRe.FindAllStringSubmatch(input, -1) {
			name := m[1]
			if name == "" {
				name = m[2]
			}
			if name == "" {
				continue
			}
			nodes, err := querier.FindNodeByName(name)
			if err != nil || len(nodes) == 0 {
				continue
			}
			for _, n := range nodes {
				if n.FilePath != "" {
					add(n.FilePath)
				}
			}
		}
	}

	return files
}

// graphContextFor builds a graph context string for the mentioned files.
// It looks up each file's imports and importers (1 hop) in the graph,
// then reads the first 30 lines of each neighbor to get package/type/function signatures.
// Returns the context string and a list of related file paths (for display).
// Caps total output at ~2K tokens (~8K chars).
func graphContextFor(files []string, root string, querier *graph.Querier) (context string, related []string) {
	if querier == nil || len(files) == 0 {
		return "", nil
	}

	const maxChars = 8000
	seen := map[string]bool{}
	for _, f := range files {
		seen[f] = true
	}

	type neighbor struct {
		path     string
		relation string // "imports" or "imported by"
	}
	var neighbors []neighbor

	for _, filePath := range files {
		fileID := "file:" + filePath

		// Outbound imports.
		if deps, err := querier.GetImportDeps(fileID); err == nil {
			for _, dep := range deps {
				if dep.FilePath != "" && !seen[dep.FilePath] {
					seen[dep.FilePath] = true
					neighbors = append(neighbors, neighbor{dep.FilePath, "imports"})
				}
			}
		}

		// Inbound importers.
		if importers, err := querier.GetImporters(fileID); err == nil {
			for _, imp := range importers {
				if imp.FilePath != "" && !seen[imp.FilePath] {
					seen[imp.FilePath] = true
					neighbors = append(neighbors, neighbor{imp.FilePath, "imported by"})
				}
			}
		}
	}

	if len(neighbors) == 0 {
		return "", nil
	}

	// Cap at top-3 neighbors per direction (6 total max).
	if len(neighbors) > 6 {
		neighbors = neighbors[:6]
	}

	var sb strings.Builder
	sb.WriteString("<graph_context>\n")

	for _, n := range neighbors {
		abs := filepath.Join(root, n.path)
		snippet := readFileHead(abs, 30)
		if snippet == "" {
			continue
		}

		sb.WriteString(fmt.Sprintf("## %s (%s)\n", n.path, n.relation))
		sb.WriteString(snippet)
		sb.WriteString("\n\n")
		related = append(related, n.path)

		if sb.Len() > maxChars {
			break
		}
	}

	sb.WriteString("</graph_context>")
	return sb.String(), related
}

// readFileHead reads the first n lines of a file, returning the text.
func readFileHead(path string, maxLines int) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	lines := strings.SplitN(string(data), "\n", maxLines+1)
	if len(lines) > maxLines {
		lines = lines[:maxLines]
	}
	return strings.Join(lines, "\n")
}
