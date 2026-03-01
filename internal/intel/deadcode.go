package intel

import (
	"path/filepath"
	"strings"

	"github.com/seedhire/mantis/internal/graph"
)

// DeadResult holds the result of dead code analysis.
type DeadResult struct {
	Symbols []*graph.Node
	Total   int
}

// FindDead returns exported symbols with no inbound edges.
// ignoreGlob is a comma-separated list of glob patterns to exclude.
func FindDead(q *graph.Querier, ignoreGlob string) (*DeadResult, error) {
	symbols, err := q.FindDeadSymbols()
	if err != nil {
		return nil, err
	}

	var patterns []string
	if ignoreGlob != "" {
		for _, p := range strings.Split(ignoreGlob, ",") {
			p = strings.TrimSpace(p)
			if p != "" {
				patterns = append(patterns, p)
			}
		}
	}

	var filtered []*graph.Node
	for _, sym := range symbols {
		if matchesAny(sym.FilePath, patterns) {
			continue
		}
		filtered = append(filtered, sym)
	}

	return &DeadResult{Symbols: filtered, Total: len(filtered)}, nil
}

func matchesAny(filePath string, patterns []string) bool {
	for _, pattern := range patterns {
		if ok, _ := filepath.Match(pattern, filePath); ok {
			return true
		}
		// Also match against the base name.
		if ok, _ := filepath.Match(pattern, filepath.Base(filePath)); ok {
			return true
		}
	}
	return false
}
