package viz

import (
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/seedhire/mantis/internal/graph"
)

type jsNode struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Type  string `json:"type"`
	Path  string `json:"path"`
}

type jsEdge struct {
	Source string `json:"source"`
	Target string `json:"target"`
	Type   string `json:"type"`
}

// GenerateHTML produces a self-contained D3 force-directed graph as an HTML string.
func GenerateHTML(nodes []*graph.Node, edges []graph.Edge, focusModule string, depth int) string {
	// Build set of file node IDs.
	fileIDs := map[string]bool{}
	for _, n := range nodes {
		if n.Type == graph.NodeTypeFile {
			fileIDs[n.ID] = true
		}
	}

	// Filter: only file nodes (and import edges between them).
	var jsNodes []jsNode
	for _, n := range nodes {
		if n.Type != graph.NodeTypeFile {
			continue
		}
		if focusModule != "" {
			// Only include nodes whose path contains focusModule.
			if !containsStr(n.FilePath, focusModule) {
				continue
			}
		}
		jsNodes = append(jsNodes, jsNode{
			ID:    n.ID,
			Label: filepath.Base(n.FilePath),
			Type:  string(n.Type),
			Path:  n.FilePath,
		})
	}

	// Build set of included node IDs for edge filtering.
	includedIDs := map[string]bool{}
	for _, jn := range jsNodes {
		includedIDs[jn.ID] = true
	}

	var jsEdges []jsEdge
	for _, e := range edges {
		if e.Type != graph.EdgeTypeImport {
			continue
		}
		if !includedIDs[e.FromID] || !includedIDs[e.ToID] {
			continue
		}
		jsEdges = append(jsEdges, jsEdge{
			Source: e.FromID,
			Target: e.ToID,
			Type:   string(e.Type),
		})
	}

	nodesJSON, _ := json.Marshal(jsNodes)
	edgesJSON, _ := json.Marshal(jsEdges)

	return fmt.Sprintf(`<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <title>Mantis — Dependency Graph</title>
  <style>
    * { margin: 0; padding: 0; box-sizing: border-box; }
    body { background: #1a1a2e; color: #fff; font-family: monospace; overflow: hidden; }
    svg { width: 100vw; height: 100vh; }
    .link { stroke: #444; stroke-opacity: 0.6; }
    .link.highlighted { stroke: #fff; stroke-opacity: 1; stroke-width: 2; }
    .node circle { cursor: pointer; }
    .node text { font-size: 10px; fill: #ccc; pointer-events: none; }
    #info {
      position: absolute; top: 16px; left: 16px;
      background: rgba(0,0,0,0.6); padding: 10px 14px;
      border-radius: 6px; font-size: 13px; line-height: 1.6;
    }
  </style>
</head>
<body>
  <div id="info">
    <b>Mantis — Dependency Graph</b><br>
    Nodes: %d &nbsp;|&nbsp; Edges: %d
  </div>
  <svg id="graph">
    <defs>
      <marker id="arrow" markerWidth="8" markerHeight="8" refX="16" refY="3" orient="auto">
        <path d="M0,0 L0,6 L8,3 z" fill="#888"/>
      </marker>
    </defs>
  </svg>
  <script src="https://d3js.org/d3.v7.min.js"></script>
  <script>
    const nodesData = %s;
    const edgesData = %s;

    const colorMap = {
      file: "#4A9EFF",
      function: "#52C41A",
      class: "#FA8C16",
      interface: "#722ED1",
      type_alias: "#13C2C2"
    };

    const svg = d3.select("#graph");
    const width = window.innerWidth;
    const height = window.innerHeight;
    const g = svg.append("g");

    svg.call(d3.zoom().scaleExtent([0.1, 5]).on("zoom", e => g.attr("transform", e.transform)));

    const simulation = d3.forceSimulation(nodesData)
      .force("link", d3.forceLink(edgesData).id(d => d.id).distance(80))
      .force("charge", d3.forceManyBody().strength(-300))
      .force("center", d3.forceCenter(width / 2, height / 2))
      .force("collide", d3.forceCollide(30));

    const link = g.append("g").selectAll("line")
      .data(edgesData).join("line")
      .attr("class", "link")
      .attr("marker-end", "url(#arrow)");

    const node = g.append("g").selectAll("g")
      .data(nodesData).join("g")
      .attr("class", "node")
      .call(d3.drag()
        .on("start", (e, d) => { if (!e.active) simulation.alphaTarget(0.3).restart(); d.fx = d.x; d.fy = d.y; })
        .on("drag", (e, d) => { d.fx = e.x; d.fy = e.y; })
        .on("end", (e, d) => { if (!e.active) simulation.alphaTarget(0); d.fx = null; d.fy = null; }));

    node.append("circle")
      .attr("r", d => d.type === "file" ? 8 : 5)
      .attr("fill", d => colorMap[d.type] || "#fff")
      .on("click", (e, d) => {
        link.classed("highlighted", l => l.source.id === d.id || l.target.id === d.id);
      });

    node.append("title").text(d => d.path || d.label);

    simulation.on("tick", () => {
      link
        .attr("x1", d => d.source.x).attr("y1", d => d.source.y)
        .attr("x2", d => d.target.x).attr("y2", d => d.target.y);
      node.attr("transform", d => "translate(" + d.x + "," + d.y + ")");
    });
  </script>
</body>
</html>`, len(jsNodes), len(jsEdges), string(nodesJSON), string(edgesJSON))
}

func containsStr(s, substr string) bool {
	return len(substr) == 0 || (len(s) >= len(substr) && func() bool {
		for i := 0; i <= len(s)-len(substr); i++ {
			if s[i:i+len(substr)] == substr {
				return true
			}
		}
		return false
	}())
}
