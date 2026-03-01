package intel

import (
	"sort"

	"github.com/seedhire/mantis/internal/graph"
)

// Cycle represents a circular dependency chain.
type Cycle struct {
	Nodes  []string // file paths forming the cycle
	Length int
}

// CircularResult holds all detected circular dependencies.
type CircularResult struct {
	Cycles []Cycle
	Total  int
}

const (
	colorWhite = 0
	colorGray  = 1
	colorBlack = 2
)

// FindCircular detects circular import dependencies using DFS.
func FindCircular(q *graph.Querier) (*CircularResult, error) {
	files, err := q.GetAllFiles()
	if err != nil {
		return nil, err
	}
	edges, err := q.GetAllEdges()
	if err != nil {
		return nil, err
	}

	// Build adjacency list (only import edges between file nodes).
	fileSet := map[string]string{} // nodeID -> filePath
	for _, f := range files {
		fileSet[f.ID] = f.FilePath
	}

	adj := map[string][]string{}
	for _, e := range edges {
		if e.Type != graph.EdgeTypeImport {
			continue
		}
		if _, ok := fileSet[e.FromID]; !ok {
			continue
		}
		if _, ok := fileSet[e.ToID]; !ok {
			continue
		}
		adj[e.FromID] = append(adj[e.FromID], e.ToID)
	}

	color := map[string]int{}
	parent := map[string]string{}
	var cycles []Cycle
	seen := map[string]bool{}

	var dfs func(nodeID string, path []string)
	dfs = func(nodeID string, path []string) {
		color[nodeID] = colorGray
		path = append(path, nodeID)

		for _, neighbor := range adj[nodeID] {
			if color[neighbor] == colorGray {
				// Found a cycle — extract the path from neighbor to current.
				cycleNodes := []string{}
				for i := len(path) - 1; i >= 0; i-- {
					cycleNodes = append([]string{path[i]}, cycleNodes...)
					if path[i] == neighbor {
						break
					}
				}
				// Convert to file paths and normalize.
				fps := make([]string, len(cycleNodes))
				for i, id := range cycleNodes {
					fps[i] = fileSet[id]
				}
				normalized := normalizeCycle(fps)
				key := cyclKey(normalized)
				if !seen[key] {
					seen[key] = true
					cycles = append(cycles, Cycle{Nodes: normalized, Length: len(normalized)})
				}
			} else if color[neighbor] == colorWhite {
				parent[neighbor] = nodeID
				dfs(neighbor, path)
			}
		}
		color[nodeID] = colorBlack
	}

	for _, f := range files {
		if color[f.ID] == colorWhite {
			dfs(f.ID, nil)
		}
	}

	// Sort cycles by length, then by first node.
	sort.Slice(cycles, func(i, j int) bool {
		if cycles[i].Length != cycles[j].Length {
			return cycles[i].Length < cycles[j].Length
		}
		if len(cycles[i].Nodes) > 0 && len(cycles[j].Nodes) > 0 {
			return cycles[i].Nodes[0] < cycles[j].Nodes[0]
		}
		return false
	})

	if len(cycles) > 50 {
		cycles = cycles[:50]
	}

	_ = parent // suppress unused warning
	return &CircularResult{Cycles: cycles, Total: len(cycles)}, nil
}

// normalizeCycle rotates the cycle to start with the lexicographically smallest element.
func normalizeCycle(fps []string) []string {
	if len(fps) == 0 {
		return fps
	}
	minIdx := 0
	for i, fp := range fps {
		if fp < fps[minIdx] {
			minIdx = i
		}
	}
	result := make([]string, len(fps))
	for i, fp := range fps {
		result[(i-minIdx+len(fps))%len(fps)] = fp
	}
	return result
}

func cyclKey(fps []string) string {
	key := ""
	for _, fp := range fps {
		key += fp + "|"
	}
	return key
}
