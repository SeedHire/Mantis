// Package nl (natural language) maps user intent to internal codebase tools.
// When the AI detects a query needs graph intelligence, it routes through here.
// The user never calls these directly — the AI calls them automatically.
package nl

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	appcontext "github.com/seedhire/mantis/internal/context"
	"github.com/seedhire/mantis/internal/graph"
	"github.com/seedhire/mantis/internal/intel"
	"github.com/seedhire/mantis/internal/config"
)

// ToolResult is the text output of an internal codebase tool call,
// ready to be injected into the AI's context.
type ToolResult struct {
	Tool    string // "context" | "impact" | "find" | "dead" | "circular"
	Summary string // short description of what was found
	Content string // full text to inject into the AI prompt
}

// Dispatcher holds a reference to the project root and graph DB.
type Dispatcher struct {
	root string
	db   *graph.DB
}

// New returns a Dispatcher for the given project root.
// Returns nil if no graph DB exists (mantis not initialized in this project).
func New(root string) *Dispatcher {
	dbPath := config.DefaultDBPath(root)
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil
	}
	db, err := graph.Open(dbPath)
	if err != nil {
		return nil
	}
	return &Dispatcher{root: root, db: db}
}

// Close releases the graph DB connection.
func (d *Dispatcher) Close() {
	if d != nil && d.db != nil {
		d.db.Close()
	}
}

// IsAvailable reports whether the graph DB is ready.
func (d *Dispatcher) IsAvailable() bool {
	return d != nil && d.db != nil
}

// Querier returns a graph.Querier for direct graph queries.
// Returns nil if the dispatcher is unavailable.
func (d *Dispatcher) Querier() *graph.Querier {
	if !d.IsAvailable() {
		return nil
	}
	return graph.NewQuerier(d.db)
}

// Dispatch analyses the user message and runs whichever internal tools are relevant.
// Returns a slice of ToolResult to inject into the AI's context before it answers.
func (d *Dispatcher) Dispatch(message string) []ToolResult {
	if !d.IsAvailable() {
		return nil
	}

	lower := strings.ToLower(message)
	var results []ToolResult

	// Impact query: "what breaks if I change X?" / "affect" / "blast radius"
	if target := extractImpactTarget(lower, message); target != "" {
		if r := d.runImpact(target); r != nil {
			results = append(results, *r)
		}
	}

	// Context bundle: "fix X" / "explain X" / "how does X work"
	if symbol := extractSymbol(lower, message); symbol != "" {
		if r := d.runContext(symbol); r != nil {
			results = append(results, *r)
		}
	}

	// Dead code query
	if strings.Contains(lower, "dead code") || strings.Contains(lower, "unused") {
		if r := d.runDead(); r != nil {
			results = append(results, *r)
		}
	}

	// Circular dependency query
	if strings.Contains(lower, "circular") || strings.Contains(lower, "cycle") {
		if r := d.runCircular(); r != nil {
			results = append(results, *r)
		}
	}

	// Code review / bug-finding query
	if isBugQuery(lower) {
		if r := d.runCodeReview(); r != nil {
			results = append(results, *r)
		}
	}

	return results
}

func (d *Dispatcher) runImpact(target string) *ToolResult {
	q := graph.NewQuerier(d.db)
	result, err := intel.Impact(q, target, 4)
	if err != nil || result.TotalFiles == 0 {
		return nil
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Impact analysis for `%s`: %d files affected.\n", target, result.TotalFiles))
	for depth := 1; depth <= 3; depth++ {
		nodes, ok := result.ByDepth[depth]
		if !ok || len(nodes) == 0 {
			continue
		}
		label := "indirect"
		if depth == 1 {
			label = "direct importers"
		}
		sb.WriteString(fmt.Sprintf("  depth %d (%s): ", depth, label))
		paths := make([]string, len(nodes))
		for i, n := range nodes {
			paths[i] = n.FilePath
		}
		sb.WriteString(strings.Join(paths, ", ") + "\n")
	}

	return &ToolResult{
		Tool:    "impact",
		Summary: fmt.Sprintf("%d files affected by changes to %s", result.TotalFiles, target),
		Content: sb.String(),
	}
}

func (d *Dispatcher) runContext(symbol string) *ToolResult {
	bundler := appcontext.NewBundler(d.db, d.root)
	bundle, err := bundler.Bundle(symbol, 3, 6000)
	if err != nil {
		return nil
	}
	md := bundler.RenderMarkdown(bundle)
	if md == "" {
		return nil
	}
	return &ToolResult{
		Tool:    "context",
		Summary: fmt.Sprintf("context bundle for %s", symbol),
		Content: md,
	}
}

func (d *Dispatcher) runDead() *ToolResult {
	q := graph.NewQuerier(d.db)
	result, err := intel.FindDead(q, "")
	if err != nil || result.Total == 0 {
		return nil
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Dead code analysis: %d unused exported symbols.\n", result.Total))
	for i, sym := range result.Symbols {
		if i >= 10 {
			sb.WriteString(fmt.Sprintf("  ... and %d more\n", result.Total-10))
			break
		}
		sb.WriteString(fmt.Sprintf("  %s %s (%s:%d)\n",
			sym.Type, sym.Name, sym.FilePath, sym.LineStart))
	}
	return &ToolResult{
		Tool:    "dead",
		Summary: fmt.Sprintf("%d unused symbols found", result.Total),
		Content: sb.String(),
	}
}

func (d *Dispatcher) runCircular() *ToolResult {
	q := graph.NewQuerier(d.db)
	result, err := intel.FindCircular(q)
	if err != nil || result.Total == 0 {
		return nil
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Circular dependencies: %d cycle(s) found.\n", result.Total))
	for i, cycle := range result.Cycles {
		if i >= 5 {
			break
		}
		sb.WriteString(fmt.Sprintf("  cycle %d: %s\n", i+1, strings.Join(cycle.Nodes, " → ")))
	}
	return &ToolResult{
		Tool:    "circular",
		Summary: fmt.Sprintf("%d circular dependency cycles", result.Total),
		Content: sb.String(),
	}
}

// isBugQuery returns true for code review / bug-finding queries.
func isBugQuery(lower string) bool {
	triggers := []string{
		"find bug", "find all bug", "review code", "code review",
		"audit", "check for bug", "look for bug", "any bug",
		"find issue", "find problem", "check code", "review all",
		"scan for", "find error", "all issues",
	}
	for _, t := range triggers {
		if strings.Contains(lower, t) {
			return true
		}
	}
	return false
}

// runCodeReview reads actual source files and injects them for AI analysis.
// Caps total content at ~6000 chars to stay within model context windows.
func (d *Dispatcher) runCodeReview() *ToolResult {
	const maxTotal = 6000
	const maxPerFile = 1500

	var sb strings.Builder
	sb.WriteString("Source files for review:\n\n")
	total := 0

	// Walk the project, prioritising internal/ and cmd/
	priority := []string{
		filepath.Join(d.root, "internal"),
		filepath.Join(d.root, "cmd"),
		d.root,
	}

	seen := map[string]bool{}
	for _, dir := range priority {
		_ = filepath.WalkDir(dir, func(path string, de os.DirEntry, err error) error {
			if err != nil || de.IsDir() {
				if de != nil && de.IsDir() {
					base := de.Name()
					if base == ".git" || base == "vendor" || base == "node_modules" ||
						base == ".mantis" || base == "archive" {
						return filepath.SkipDir
					}
				}
				return nil
			}
			if seen[path] {
				return nil
			}
			if !strings.HasSuffix(path, ".go") {
				return nil
			}
			if total >= maxTotal {
				return filepath.SkipAll
			}

			data, err := os.ReadFile(path)
			if err != nil {
				return nil
			}
			seen[path] = true

			rel, _ := filepath.Rel(d.root, path)
			content := string(data)
			if len(content) > maxPerFile {
				content = content[:maxPerFile] + "\n// … (truncated)"
			}

			chunk := fmt.Sprintf("// %s\n%s\n\n", rel, content)
			if total+len(chunk) > maxTotal {
				return filepath.SkipAll
			}
			sb.WriteString(chunk)
			total += len(chunk)
			return nil
		})
		if total >= maxTotal {
			break
		}
	}

	if total == 0 {
		return nil
	}
	return &ToolResult{
		Tool:    "review",
		Summary: "source code loaded for bug analysis",
		Content: sb.String(),
	}
}
func extractImpactTarget(lower, original string) string {
	triggers := []string{
		"if i change ", "if i modify ", "if i delete ", "if i remove ",
		"when i change ", "change ", "modify ", "impact of ", "blast radius of ",
		"what breaks if", "breaks if",
	}
	for _, t := range triggers {
		if idx := strings.Index(lower, t); idx != -1 {
			rest := strings.TrimSpace(original[idx+len(t):])
			word := firstWord(rest)
			if word != "" && looksLikeSymbol(word) {
				return word
			}
		}
	}
	// Look for .go / .py / .ts file references
	for _, word := range strings.Fields(original) {
		if looksLikeFile(word) {
			return word
		}
	}
	return ""
}

// extractSymbol looks for a symbol or file name in explain/fix queries.
func extractSymbol(lower, original string) string {
	triggers := []string{
		"fix ", "explain ", "how does ", "what does ", "refactor ",
		"in ", "for ", "about ", "look at ",
	}
	for _, t := range triggers {
		if idx := strings.Index(lower, t); idx != -1 {
			rest := strings.TrimSpace(original[idx+len(t):])
			word := firstWord(rest)
			if word != "" && (looksLikeSymbol(word) || looksLikeFile(word)) {
				return word
			}
		}
	}
	return ""
}

func firstWord(s string) string {
	s = strings.TrimLeft(s, "\"'`")
	idx := strings.IndexAny(s, " \t\n\"'`?.,")
	if idx == -1 {
		return s
	}
	return s[:idx]
}

func looksLikeFile(s string) bool {
	exts := []string{".go", ".py", ".ts", ".tsx", ".js", ".jsx", ".rs"}
	for _, ext := range exts {
		if strings.HasSuffix(s, ext) {
			return true
		}
	}
	return false
}

func looksLikeSymbol(s string) bool {
	if len(s) < 2 || len(s) > 80 {
		return false
	}
	// Must start with letter, contain only valid identifier chars or path separators
	if s[0] < 'A' || (s[0] > 'Z' && s[0] < 'a') || s[0] > 'z' {
		return false
	}
	// Check it's not a stop word
	stopWords := map[string]bool{
		"the": true, "a": true, "an": true, "this": true, "that": true,
		"it": true, "i": true, "you": true, "my": true, "your": true,
		"when": true, "why": true, "how": true, "what": true, "where": true,
	}
	return !stopWords[strings.ToLower(s)]
}

// ContextDir returns the path to the hidden brain dir.
func ContextDir(root string) string {
	return filepath.Join(root, ".mantis")
}
