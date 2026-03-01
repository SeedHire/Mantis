package intel

import "github.com/seedhire/mantis/internal/graph"

// FindResult holds the result of a symbol find operation.
type FindResult struct {
	Symbol      string
	Importers   []*graph.Node
	Definitions []*graph.Node
	Type        string
}

// Find locates a symbol and returns its importers and definition nodes.
func Find(q *graph.Querier, symbolName, findType string) (*FindResult, error) {
	nodes, err := q.FindNodeByName(symbolName)
	if err != nil {
		return nil, err
	}

	// Collect non-file symbol definitions.
	seen := map[string]bool{}
	var definitions []*graph.Node
	filesSeen := map[string]bool{}
	var importers []*graph.Node

	for _, n := range nodes {
		if n.Type == graph.NodeTypeFile {
			continue
		}
		definitions = append(definitions, n)
		if filesSeen[n.FilePath] {
			continue
		}
		filesSeen[n.FilePath] = true

		fileNode, err := q.GetFileNode(n.FilePath)
		if err != nil || fileNode == nil {
			continue
		}
		imps, err := q.GetImporters(fileNode.ID)
		if err != nil {
			return nil, err
		}
		for _, imp := range imps {
			if !seen[imp.ID] {
				seen[imp.ID] = true
				importers = append(importers, imp)
			}
		}
	}

	return &FindResult{
		Symbol:      symbolName,
		Importers:   importers,
		Definitions: definitions,
		Type:        findType,
	}, nil
}
