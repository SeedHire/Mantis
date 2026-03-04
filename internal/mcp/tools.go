package mcp

import (
	"encoding/json"
	"fmt"
	"strings"

	appcontext "github.com/seedhire/mantis/internal/context"
	"github.com/seedhire/mantis/internal/graph"
	"github.com/seedhire/mantis/internal/intel"
)

// ToolHandler dispatches MCP tool calls to Mantis graph intelligence functions.
type ToolHandler struct {
	querier *graph.Querier
	db      *graph.DB
	root    string
}

// NewToolHandler creates a handler wired to the given graph and project root.
func NewToolHandler(q *graph.Querier, db *graph.DB, root string) *ToolHandler {
	return &ToolHandler{querier: q, db: db, root: root}
}

// ListTools returns the MCP tool definitions.
func (h *ToolHandler) ListTools() []mcpTool {
	return []mcpTool{
		{
			Name:        "mantis_impact",
			Description: "Compute the blast radius of a symbol or file — all files transitively affected by a change.",
			InputSchema: mcpToolSchema{
				Type: "object",
				Properties: map[string]mcpToolProp{
					"target": {Type: "string", Description: "Symbol name or file path to analyze"},
					"depth":  {Type: "number", Description: "Max BFS depth (default 5)"},
				},
				Required: []string{"target"},
			},
		},
		{
			Name:        "mantis_find",
			Description: "Find a symbol by name in the codebase dependency graph. Returns definitions, importers, and file locations.",
			InputSchema: mcpToolSchema{
				Type: "object",
				Properties: map[string]mcpToolProp{
					"name": {Type: "string", Description: "Symbol name to search for"},
					"type": {Type: "string", Description: "Filter by type: function, class, interface, method, type_alias (optional)"},
				},
				Required: []string{"name"},
			},
		},
		{
			Name:        "mantis_hotspots",
			Description: "List the top N files by churn score — files changed most frequently in git history. Splits into refactor candidates (single author) and watch list (multi-author).",
			InputSchema: mcpToolSchema{
				Type: "object",
				Properties: map[string]mcpToolProp{
					"limit": {Type: "number", Description: "Number of hotspots to return (default 20)"},
					"days":  {Type: "number", Description: "Git history lookback in days (default 90)"},
				},
			},
		},
		{
			Name:        "mantis_coupling",
			Description: "Find files that frequently change together with the given file (temporal coupling from git history).",
			InputSchema: mcpToolSchema{
				Type: "object",
				Properties: map[string]mcpToolProp{
					"file":  {Type: "string", Description: "File path to analyze coupling for"},
					"limit": {Type: "number", Description: "Max coupled files to return (default 10)"},
					"days":  {Type: "number", Description: "Git history lookback in days (default 90)"},
				},
				Required: []string{"file"},
			},
		},
		{
			Name:        "mantis_dead",
			Description: "Detect dead code — exported symbols that are never imported or referenced anywhere in the codebase.",
			InputSchema: mcpToolSchema{
				Type: "object",
				Properties: map[string]mcpToolProp{
					"ignore": {Type: "string", Description: "Glob pattern to ignore (e.g. '*_test.go')"},
				},
			},
		},
		{
			Name:        "mantis_imports",
			Description: "List all direct imports of a file (outbound dependencies).",
			InputSchema: mcpToolSchema{
				Type: "object",
				Properties: map[string]mcpToolProp{
					"file": {Type: "string", Description: "File path to get imports for"},
				},
				Required: []string{"file"},
			},
		},
		{
			Name:        "mantis_importers",
			Description: "List all files that import the given file (inbound dependents / reverse dependencies).",
			InputSchema: mcpToolSchema{
				Type: "object",
				Properties: map[string]mcpToolProp{
					"file": {Type: "string", Description: "File path to find importers of"},
				},
				Required: []string{"file"},
			},
		},
		{
			Name:        "mantis_context",
			Description: "Run the full context bundler for a symbol — returns a ranked, token-budgeted list of relevant files with content snippets.",
			InputSchema: mcpToolSchema{
				Type: "object",
				Properties: map[string]mcpToolProp{
					"query":  {Type: "string", Description: "Symbol name or search query"},
					"depth":  {Type: "number", Description: "Max graph traversal depth (default 3)"},
					"tokens": {Type: "number", Description: "Token budget for context (default 8000)"},
				},
				Required: []string{"query"},
			},
		},
	}
}

// Call dispatches a tool call by name and returns the text result.
func (h *ToolHandler) Call(name string, argsRaw json.RawMessage) (string, error) {
	var args map[string]interface{}
	if len(argsRaw) > 0 {
		if err := json.Unmarshal(argsRaw, &args); err != nil {
			return "", fmt.Errorf("invalid arguments: %w", err)
		}
	}
	if args == nil {
		args = map[string]interface{}{}
	}

	switch name {
	case "mantis_impact":
		return h.callImpact(args)
	case "mantis_find":
		return h.callFind(args)
	case "mantis_hotspots":
		return h.callHotspots(args)
	case "mantis_coupling":
		return h.callCoupling(args)
	case "mantis_dead":
		return h.callDead(args)
	case "mantis_imports":
		return h.callImports(args)
	case "mantis_importers":
		return h.callImporters(args)
	case "mantis_context":
		return h.callContext(args)
	default:
		return "", fmt.Errorf("unknown tool: %s", name)
	}
}

// ── Tool implementations ────────────────────────────────────────────────────

func (h *ToolHandler) callImpact(args map[string]interface{}) (string, error) {
	target, _ := args["target"].(string)
	if target == "" {
		return "", fmt.Errorf("missing required parameter: target")
	}
	depth := intArg(args, "depth", 5)

	result, err := intel.Impact(h.querier, target, depth)
	if err != nil {
		return "", fmt.Errorf("impact analysis failed: %w", err)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Impact analysis for %q\n", result.Target)
	fmt.Fprintf(&b, "Total affected files: %d\n\n", result.TotalFiles)

	for d := 1; d <= depth; d++ {
		nodes := result.ByDepth[d]
		if len(nodes) == 0 {
			continue
		}
		fmt.Fprintf(&b, "Depth %d (%d files):\n", d, len(nodes))
		for _, n := range nodes {
			risk := result.RiskScores[n.ID]
			fmt.Fprintf(&b, "  %s  (risk: %d/10)\n", n.FilePath, risk)
		}
		b.WriteString("\n")
	}
	return b.String(), nil
}

func (h *ToolHandler) callFind(args map[string]interface{}) (string, error) {
	name, _ := args["name"].(string)
	if name == "" {
		return "", fmt.Errorf("missing required parameter: name")
	}
	findType, _ := args["type"].(string)

	result, err := intel.Find(h.querier, name, findType)
	if err != nil {
		return "", fmt.Errorf("find failed: %w", err)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Symbol: %s\n", result.Symbol)
	if result.Type != "" {
		fmt.Fprintf(&b, "Type: %s\n", result.Type)
	}

	if len(result.Definitions) > 0 {
		b.WriteString("\nDefinitions:\n")
		for _, n := range result.Definitions {
			fmt.Fprintf(&b, "  %s:%d  (%s, %s)\n", n.FilePath, n.LineStart, n.Type, n.Language)
		}
	}

	if len(result.Importers) > 0 {
		b.WriteString("\nUsed by:\n")
		for _, n := range result.Importers {
			fmt.Fprintf(&b, "  %s\n", n.FilePath)
		}
	}

	if len(result.Definitions) == 0 && len(result.Importers) == 0 {
		fmt.Fprintf(&b, "\nNo results found for %q", name)
		if findType != "" {
			fmt.Fprintf(&b, " with type %q", findType)
		}
		b.WriteString("\n")
	}
	return b.String(), nil
}

func (h *ToolHandler) callHotspots(args map[string]interface{}) (string, error) {
	limit := intArg(args, "limit", 20)
	days := intArg(args, "days", 90)

	stats, err := intel.Temporal(h.root, days)
	if err != nil {
		return "", fmt.Errorf("temporal analysis failed: %w", err)
	}

	hotspots := intel.Hotspots(stats, limit)
	if len(hotspots) == 0 {
		return "No hotspots found in the last " + fmt.Sprint(days) + " days.\n", nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Top %d hotspots (last %d days):\n\n", len(hotspots), days)

	// Split into refactor candidates (single author) and watch list
	var refactor, watch []intel.FileChurn
	for _, h := range hotspots {
		if h.Authors <= 1 && h.Commits >= 3 {
			refactor = append(refactor, h)
		} else if h.Authors > 1 {
			watch = append(watch, h)
		}
	}

	if len(refactor) > 0 {
		b.WriteString("Refactor candidates (single author, 3+ commits):\n")
		for _, f := range refactor {
			fmt.Fprintf(&b, "  %-50s  %d commits  churn=%.1f  by %s\n",
				f.Path, f.Commits, f.ChurnScore, f.LastAuthor)
		}
		b.WriteString("\n")
	}

	if len(watch) > 0 {
		b.WriteString("Watch list (multiple authors):\n")
		for _, f := range watch {
			fmt.Fprintf(&b, "  %-50s  %d commits  %d authors  churn=%.1f\n",
				f.Path, f.Commits, f.Authors, f.ChurnScore)
		}
		b.WriteString("\n")
	}

	// Show all if neither category matched
	if len(refactor) == 0 && len(watch) == 0 {
		for _, f := range hotspots {
			fmt.Fprintf(&b, "  %-50s  %d commits  %d authors  churn=%.1f\n",
				f.Path, f.Commits, f.Authors, f.ChurnScore)
		}
	}
	return b.String(), nil
}

func (h *ToolHandler) callCoupling(args map[string]interface{}) (string, error) {
	file, _ := args["file"].(string)
	if file == "" {
		return "", fmt.Errorf("missing required parameter: file")
	}
	limit := intArg(args, "limit", 10)
	days := intArg(args, "days", 90)

	stats, err := intel.Temporal(h.root, days)
	if err != nil {
		return "", fmt.Errorf("temporal analysis failed: %w", err)
	}

	coupled := intel.CouplingFor(stats, file, limit)
	if len(coupled) == 0 {
		return fmt.Sprintf("No temporal coupling found for %q in the last %d days.\n", file, days), nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Files coupled with %s (last %d days):\n\n", file, days)
	for _, c := range coupled {
		other := c.FileB
		if other == file {
			other = c.FileA
		}
		fmt.Fprintf(&b, "  %-50s  %d co-changes  coupling=%.0f%%\n",
			other, c.CoChanges, c.Coupling*100)
	}
	return b.String(), nil
}

func (h *ToolHandler) callDead(args map[string]interface{}) (string, error) {
	ignore, _ := args["ignore"].(string)

	result, err := intel.FindDead(h.querier, ignore)
	if err != nil {
		return "", fmt.Errorf("dead code detection failed: %w", err)
	}

	if result.Total == 0 {
		return "No dead code detected.\n", nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Dead code: %d unused exported symbols\n\n", result.Total)
	for _, sym := range result.Symbols {
		fmt.Fprintf(&b, "  %s  %s:%d  (%s)\n", sym.Name, sym.FilePath, sym.LineStart, sym.Type)
	}
	return b.String(), nil
}

func (h *ToolHandler) callImports(args map[string]interface{}) (string, error) {
	file, _ := args["file"].(string)
	if file == "" {
		return "", fmt.Errorf("missing required parameter: file")
	}

	fileNode, err := h.querier.GetFileNode(file)
	if err != nil {
		return "", fmt.Errorf("file not found in graph: %s", file)
	}

	imports, err := h.querier.GetImportDeps(fileNode.ID)
	if err != nil {
		return "", fmt.Errorf("failed to get imports: %w", err)
	}

	if len(imports) == 0 {
		return fmt.Sprintf("%s has no imports in the graph.\n", file), nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Imports of %s (%d):\n\n", file, len(imports))
	for _, n := range imports {
		fmt.Fprintf(&b, "  %s\n", n.FilePath)
	}
	return b.String(), nil
}

func (h *ToolHandler) callImporters(args map[string]interface{}) (string, error) {
	file, _ := args["file"].(string)
	if file == "" {
		return "", fmt.Errorf("missing required parameter: file")
	}

	fileNode, err := h.querier.GetFileNode(file)
	if err != nil {
		return "", fmt.Errorf("file not found in graph: %s", file)
	}

	importers, err := h.querier.GetImporters(fileNode.ID)
	if err != nil {
		return "", fmt.Errorf("failed to get importers: %w", err)
	}

	if len(importers) == 0 {
		return fmt.Sprintf("No files import %s.\n", file), nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Files that import %s (%d):\n\n", file, len(importers))
	for _, n := range importers {
		fmt.Fprintf(&b, "  %s\n", n.FilePath)
	}
	return b.String(), nil
}

func (h *ToolHandler) callContext(args map[string]interface{}) (string, error) {
	query, _ := args["query"].(string)
	if query == "" {
		return "", fmt.Errorf("missing required parameter: query")
	}
	depth := intArg(args, "depth", 3)
	tokens := intArg(args, "tokens", 8000)

	bundler := appcontext.NewBundler(h.db, h.root)
	bundle, err := bundler.Bundle(query, depth, tokens)
	if err != nil {
		return "", fmt.Errorf("context bundling failed: %w", err)
	}

	if len(bundle.Files) == 0 {
		return fmt.Sprintf("No context found for %q.\n", query), nil
	}

	return bundler.RenderMarkdown(bundle), nil
}

// ── helpers ─────────────────────────────────────────────────────────────────

func intArg(args map[string]interface{}, key string, fallback int) int {
	v, ok := args[key]
	if !ok {
		return fallback
	}
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	default:
		return fallback
	}
}
