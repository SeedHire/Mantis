// Package graph — repomap.go provides a ranked symbol index from graph.db using PageRank.
//
// Source: Aider's highest-ROI technique. PageRank over call-graph edges → ranked list
// of most-referenced symbols. GPT-4 Turbo benchmark improved from 20% → 61% when
// given repo-map vs. no map.
//
// Output format: one line per symbol —
//
//	filepath:ClassName.methodName(args) returnType
//
// Token-budgeted: binary search for max tags that fit in N tokens.
package graph

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

const (
	defaultMaxTokens = 2048
	pageRankDamping  = 0.85
	pageRankIter     = 20
)

// RepoMapEntry is a single ranked symbol in the repo map.
type RepoMapEntry struct {
	FilePath string
	Symbol   string // e.g. "ClassName.methodName" or "functionName"
	NodeType NodeType
	Score    float64
	Line     int
}

// RepoMap builds a ranked symbol index from the dependency graph using PageRank.
// Returns entries sorted by descending score, capped to fit within maxTokens.
// If maxTokens is 0, defaults to 2048.
func (q *Querier) RepoMap(maxTokens int) ([]RepoMapEntry, error) {
	if maxTokens <= 0 {
		maxTokens = defaultMaxTokens
	}

	// Get all non-file nodes (symbols).
	nodes, err := q.getAllSymbolNodes()
	if err != nil {
		return nil, fmt.Errorf("repomap: get symbols: %w", err)
	}
	if len(nodes) == 0 {
		return nil, nil
	}

	// Get all edges.
	edges, err := q.GetAllEdges()
	if err != nil {
		return nil, fmt.Errorf("repomap: get edges: %w", err)
	}

	// Build adjacency: nodeID → list of outgoing target IDs.
	outgoing := map[string][]string{}
	incoming := map[string][]string{}
	nodeSet := map[string]*Node{}
	for _, n := range nodes {
		nodeSet[n.ID] = n
	}
	for _, e := range edges {
		// Only consider edges where both endpoints are symbols (not files).
		if _, ok := nodeSet[e.FromID]; !ok {
			continue
		}
		if _, ok := nodeSet[e.ToID]; !ok {
			continue
		}
		outgoing[e.FromID] = append(outgoing[e.FromID], e.ToID)
		incoming[e.ToID] = append(incoming[e.ToID], e.FromID)
	}

	// Run PageRank.
	scores := pageRank(nodes, outgoing, incoming)

	// Build entries sorted by score.
	entries := make([]RepoMapEntry, 0, len(nodes))
	for _, n := range nodes {
		if n.Type == NodeTypeFile {
			continue
		}
		score := scores[n.ID]
		if score <= 0 {
			score = 1.0 / float64(len(nodes)) // minimum baseline
		}
		entries = append(entries, RepoMapEntry{
			FilePath: n.FilePath,
			Symbol:   formatSymbol(n),
			NodeType: n.Type,
			Score:    score,
			Line:     n.LineStart,
		})
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Score > entries[j].Score
	})

	// Binary search: find max entries that fit in token budget.
	entries = fitToTokenBudget(entries, maxTokens)

	return entries, nil
}

// RepoMapString formats the repo map as a string suitable for LLM context injection.
// Groups by file path, shows ranked symbols with line numbers.
func RepoMapString(entries []RepoMapEntry) string {
	if len(entries) == 0 {
		return ""
	}

	// Group by file path, preserving rank order.
	type fileGroup struct {
		path    string
		symbols []RepoMapEntry
	}
	seen := map[string]int{} // path → index in groups
	var groups []fileGroup

	for _, e := range entries {
		if idx, ok := seen[e.FilePath]; ok {
			groups[idx].symbols = append(groups[idx].symbols, e)
		} else {
			seen[e.FilePath] = len(groups)
			groups = append(groups, fileGroup{
				path:    e.FilePath,
				symbols: []RepoMapEntry{e},
			})
		}
	}

	var sb strings.Builder
	for _, g := range groups {
		sb.WriteString(g.path)
		sb.WriteByte('\n')
		for _, sym := range g.symbols {
			fmt.Fprintf(&sb, "  %s:%d %s\n", typeAbbrev(sym.NodeType), sym.Line, sym.Symbol)
		}
	}
	return sb.String()
}

// ── PageRank implementation ──────────────────────────────────────────────────

func pageRank(nodes []*Node, outgoing, incoming map[string][]string) map[string]float64 {
	n := len(nodes)
	if n == 0 {
		return nil
	}

	scores := make(map[string]float64, n)
	initial := 1.0 / float64(n)
	for _, node := range nodes {
		scores[node.ID] = initial
	}

	for iter := 0; iter < pageRankIter; iter++ {
		newScores := make(map[string]float64, n)
		// Base score for all nodes (from damping).
		base := (1.0 - pageRankDamping) / float64(n)

		for _, node := range nodes {
			sum := 0.0
			for _, fromID := range incoming[node.ID] {
				outDegree := len(outgoing[fromID])
				if outDegree > 0 {
					sum += scores[fromID] / float64(outDegree)
				}
			}
			newScores[node.ID] = base + pageRankDamping*sum
		}
		scores = newScores
	}

	return scores
}

// ── Helpers ──────────────────────────────────────────────────────────────────

// getAllSymbolNodes returns all non-file nodes from the graph.
func (q *Querier) getAllSymbolNodes() ([]*Node, error) {
	rows, err := q.db.conn.Query(
		`SELECT `+nodeSelectCols+` FROM nodes WHERE type != ?`,
		string(NodeTypeFile))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNodes(rows)
}

// formatSymbol produces a display string like "ClassName.methodName" for a node.
func formatSymbol(n *Node) string {
	switch n.Type {
	case NodeTypeMethod:
		// Methods often have "Receiver.Method" in the name already.
		return n.Name
	case NodeTypeFunction:
		return n.Name + "()"
	case NodeTypeClass, NodeTypeInterface:
		return n.Name
	case NodeTypeTypeAlias:
		return "type " + n.Name
	default:
		return n.Name
	}
}

// typeAbbrev returns a short type indicator for display.
func typeAbbrev(t NodeType) string {
	switch t {
	case NodeTypeFunction:
		return "fn"
	case NodeTypeMethod:
		return "mt"
	case NodeTypeClass:
		return "cl"
	case NodeTypeInterface:
		return "if"
	case NodeTypeTypeAlias:
		return "ty"
	default:
		return "??"
	}
}

// fitToTokenBudget trims entries to fit within a token budget.
// Uses binary search. Approximate token count: ~4 chars per token.
func fitToTokenBudget(entries []RepoMapEntry, maxTokens int) []RepoMapEntry {
	if len(entries) == 0 {
		return entries
	}

	// Estimate tokens for N entries.
	estimateTokens := func(n int) int {
		total := 0
		for i := 0; i < n && i < len(entries); i++ {
			// filepath + symbol + line number + type + whitespace ≈ line length / 4
			lineLen := len(entries[i].FilePath) + len(entries[i].Symbol) + 10
			total += int(math.Ceil(float64(lineLen) / 4.0))
		}
		return total
	}

	// Binary search for largest N that fits.
	lo, hi := 1, len(entries)
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if estimateTokens(mid) <= maxTokens {
			lo = mid
		} else {
			hi = mid - 1
		}
	}

	return entries[:lo]
}
