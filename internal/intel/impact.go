package intel

import (
	"strings"

	"github.com/seedhire/mantis/internal/graph"
)

// ImpactResult holds the result of an impact analysis.
type ImpactResult struct {
	Target     string
	TotalFiles int
	ByDepth    map[int][]*graph.Node
	RiskScores map[string]int // fileID -> risk score (1-10)
}

// Impact returns all files transitively impacted by changes to target.
func Impact(q *graph.Querier, target string, maxDepth int) (*ImpactResult, error) {
	// Resolve target to a file node.
	fileNode, err := q.GetFileNode(target)
	if err != nil {
		return nil, err
	}
	if fileNode == nil {
		// Try finding a file node whose path ends with target.
		fileNode, err = findFileNodeBySuffix(q, target)
		if err != nil {
			return nil, err
		}
	}
	if fileNode == nil {
		// Try by symbol name.
		nodes, err := q.FindNodeByName(target)
		if err != nil {
			return nil, err
		}
		for _, n := range nodes {
			if n.Type != graph.NodeTypeFile {
				fileNode, err = q.GetFileNode(n.FilePath)
				if err == nil && fileNode != nil {
					break
				}
			}
		}
	}
	if fileNode == nil {
		return nil, &notFoundError{name: target}
	}

	depthMap, err := q.BFSReverse(fileNode.ID, maxDepth)
	if err != nil {
		return nil, err
	}

	byDepth := map[int][]*graph.Node{}
	riskScores := map[string]int{}
	total := 0

	for nodeID, depth := range depthMap {
		if nodeID == fileNode.ID {
			continue
		}
		node, err := q.GetNodeByID(nodeID)
		if err != nil || node == nil {
			continue
		}
		// C4: skip vendor, node_modules, and generated files — they inflate blast radius.
		if isIgnoredPath(node.FilePath) {
			continue
		}
		byDepth[depth] = append(byDepth[depth], node)
		total++

		// Risk score based on direct importer count.
		importers, err := q.GetImporters(nodeID)
		if err != nil {
			importers = nil
		}
		score := len(importers)*2 + 1
		if score > 10 {
			score = 10
		}
		riskScores[nodeID] = score
	}

	return &ImpactResult{
		Target:     target,
		TotalFiles: total,
		ByDepth:    byDepth,
		RiskScores: riskScores,
	}, nil
}

// isIgnoredPath returns true for vendor, generated, and tooling paths that
// should not appear in impact blast-radius reports.
func isIgnoredPath(path string) bool {
	lower := strings.ToLower(strings.ReplaceAll(path, "\\", "/"))
	for _, seg := range []string{
		"vendor/", "/vendor/",
		"node_modules/", "/node_modules/",
		".gen.", "_gen.", ".generated.", "_generated.",
		"/mock/", "/mocks/", "mock_",
		"/__generated__/",
	} {
		if strings.Contains(lower, seg) {
			return true
		}
	}
	return false
}

type notFoundError struct{ name string }

func (e *notFoundError) Error() string { return "not found: " + e.name }

// findFileNodeBySuffix finds a file node whose path ends with suffix.
func findFileNodeBySuffix(q *graph.Querier, suffix string) (*graph.Node, error) {
	allFiles, err := q.GetAllFiles()
	if err != nil {
		return nil, err
	}
	for _, f := range allFiles {
		if strings.HasSuffix(f.FilePath, suffix) {
			return f, nil
		}
	}
	return nil, nil
}
