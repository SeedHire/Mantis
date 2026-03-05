package lsp

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/seedhire/mantis/internal/graph"
	"github.com/seedhire/mantis/internal/intel"
)

// ── Document sync handlers ──────────────────────────────────────────────────

func (s *Server) handleDidOpen(params json.RawMessage) {
	var p DidOpenTextDocumentParams
	if err := json.Unmarshal(params, &p); err != nil {
		return
	}
	s.publishDiagnosticsFor(p.TextDocument.URI)
}

func (s *Server) handleDidSave(params json.RawMessage) {
	var p DidSaveTextDocumentParams
	if err := json.Unmarshal(params, &p); err != nil {
		return
	}
	s.publishDiagnosticsFor(p.TextDocument.URI)
}

// ── Hover ───────────────────────────────────────────────────────────────────

func (s *Server) handleHover(id json.RawMessage, params json.RawMessage) *jsonRPCResponse {
	var p TextDocumentPositionParams
	if err := json.Unmarshal(params, &p); err != nil {
		return makeError(id, errCodeInvalidParm, "invalid params")
	}

	if s.querier == nil {
		return &jsonRPCResponse{JSONRPC: "2.0", ID: id, Result: nil}
	}

	path := uriToPath(p.TextDocument.URI)
	relPath := s.relPath(path)

	fileNode, err := s.querier.GetFileNode(relPath)
	if err != nil {
		return &jsonRPCResponse{JSONRPC: "2.0", ID: id, Result: nil}
	}

	symbols, err := s.querier.FindSymbolsInFile(fileNode.ID)
	if err != nil || len(symbols) == 0 {
		return &jsonRPCResponse{JSONRPC: "2.0", ID: id, Result: nil}
	}

	// Find the symbol at the requested line.
	var match *graph.Node
	for _, sym := range symbols {
		if p.Position.Line >= sym.LineStart-1 && p.Position.Line <= sym.LineEnd-1 {
			match = sym
			break
		}
	}
	if match == nil {
		return &jsonRPCResponse{JSONRPC: "2.0", ID: id, Result: nil}
	}

	// Count importers for this symbol's file.
	importerCount := 0
	importers, err := s.querier.GetImporters(fileNode.ID)
	if err == nil {
		importerCount = len(importers)
	}

	var md strings.Builder
	fmt.Fprintf(&md, "**%s** `%s`\n\n", match.Name, match.Type)
	if match.Complexity > 0 {
		fmt.Fprintf(&md, "Complexity: %d\n\n", match.Complexity)
	}
	fmt.Fprintf(&md, "File importers: %d\n", importerCount)
	if match.Language != "" {
		fmt.Fprintf(&md, "\nLanguage: %s", match.Language)
	}

	hover := Hover{
		Contents: MarkupContent{Kind: "markdown", Value: md.String()},
		Range: &Range{
			Start: Position{Line: match.LineStart - 1, Character: 0},
			End:   Position{Line: match.LineEnd - 1, Character: 0},
		},
	}
	return &jsonRPCResponse{JSONRPC: "2.0", ID: id, Result: hover}
}

// ── Definition ──────────────────────────────────────────────────────────────

func (s *Server) handleDefinition(id json.RawMessage, params json.RawMessage) *jsonRPCResponse {
	var p TextDocumentPositionParams
	if err := json.Unmarshal(params, &p); err != nil {
		return makeError(id, errCodeInvalidParm, "invalid params")
	}
	if s.querier == nil {
		return &jsonRPCResponse{JSONRPC: "2.0", ID: id, Result: nil}
	}

	path := uriToPath(p.TextDocument.URI)
	relPath := s.relPath(path)

	fileNode, err := s.querier.GetFileNode(relPath)
	if err != nil {
		return &jsonRPCResponse{JSONRPC: "2.0", ID: id, Result: nil}
	}

	symbols, err := s.querier.FindSymbolsInFile(fileNode.ID)
	if err != nil {
		return &jsonRPCResponse{JSONRPC: "2.0", ID: id, Result: nil}
	}

	// Find the symbol at the cursor line.
	var symbolName string
	for _, sym := range symbols {
		if p.Position.Line >= sym.LineStart-1 && p.Position.Line <= sym.LineEnd-1 {
			symbolName = sym.Name
			break
		}
	}
	if symbolName == "" {
		return &jsonRPCResponse{JSONRPC: "2.0", ID: id, Result: nil}
	}

	// Find all definitions of this symbol across the codebase.
	nodes, err := s.querier.FindNodeByName(symbolName)
	if err != nil || len(nodes) == 0 {
		return &jsonRPCResponse{JSONRPC: "2.0", ID: id, Result: nil}
	}

	var locations []Location
	for _, n := range nodes {
		if n.Type == graph.NodeTypeFile {
			continue
		}
		locations = append(locations, Location{
			URI: pathToURI(filepath.Join(s.root, n.FilePath)),
			Range: Range{
				Start: Position{Line: n.LineStart - 1, Character: 0},
				End:   Position{Line: n.LineEnd - 1, Character: 0},
			},
		})
	}

	if len(locations) == 1 {
		return &jsonRPCResponse{JSONRPC: "2.0", ID: id, Result: locations[0]}
	}
	return &jsonRPCResponse{JSONRPC: "2.0", ID: id, Result: locations}
}

// ── References ──────────────────────────────────────────────────────────────

func (s *Server) handleReferences(id json.RawMessage, params json.RawMessage) *jsonRPCResponse {
	var p TextDocumentPositionParams
	if err := json.Unmarshal(params, &p); err != nil {
		return makeError(id, errCodeInvalidParm, "invalid params")
	}
	if s.querier == nil {
		return &jsonRPCResponse{JSONRPC: "2.0", ID: id, Result: []Location{}}
	}

	path := uriToPath(p.TextDocument.URI)
	relPath := s.relPath(path)

	fileNode, err := s.querier.GetFileNode(relPath)
	if err != nil {
		return &jsonRPCResponse{JSONRPC: "2.0", ID: id, Result: []Location{}}
	}

	// Find symbol at cursor.
	symbols, err := s.querier.FindSymbolsInFile(fileNode.ID)
	if err != nil {
		return &jsonRPCResponse{JSONRPC: "2.0", ID: id, Result: []Location{}}
	}

	var symbolName string
	for _, sym := range symbols {
		if p.Position.Line >= sym.LineStart-1 && p.Position.Line <= sym.LineEnd-1 {
			symbolName = sym.Name
			break
		}
	}
	if symbolName == "" {
		return &jsonRPCResponse{JSONRPC: "2.0", ID: id, Result: []Location{}}
	}

	// Get importers of this file + files containing the symbol.
	var locations []Location

	importers, err := s.querier.GetImporters(fileNode.ID)
	if err == nil {
		for _, imp := range importers {
			locations = append(locations, Location{
				URI: pathToURI(filepath.Join(s.root, imp.FilePath)),
				Range: Range{
					Start: Position{Line: 0, Character: 0},
					End:   Position{Line: 0, Character: 0},
				},
			})
		}
	}

	fileNodes, err := s.querier.FindFilesBySymbol(symbolName)
	if err == nil {
		seen := make(map[string]bool)
		for _, loc := range locations {
			seen[loc.URI] = true
		}
		for _, fn := range fileNodes {
			uri := pathToURI(filepath.Join(s.root, fn.FilePath))
			if !seen[uri] {
				locations = append(locations, Location{
					URI: uri,
					Range: Range{
						Start: Position{Line: 0, Character: 0},
						End:   Position{Line: 0, Character: 0},
					},
				})
			}
		}
	}

	return &jsonRPCResponse{JSONRPC: "2.0", ID: id, Result: locations}
}

// ── Document Symbol ─────────────────────────────────────────────────────────

func (s *Server) handleDocumentSymbol(id json.RawMessage, params json.RawMessage) *jsonRPCResponse {
	var p DocumentSymbolParams
	if err := json.Unmarshal(params, &p); err != nil {
		return makeError(id, errCodeInvalidParm, "invalid params")
	}
	if s.querier == nil {
		return &jsonRPCResponse{JSONRPC: "2.0", ID: id, Result: []DocumentSymbol{}}
	}

	path := uriToPath(p.TextDocument.URI)
	relPath := s.relPath(path)

	fileNode, err := s.querier.GetFileNode(relPath)
	if err != nil {
		return &jsonRPCResponse{JSONRPC: "2.0", ID: id, Result: []DocumentSymbol{}}
	}

	symbols, err := s.querier.FindSymbolsInFile(fileNode.ID)
	if err != nil {
		return &jsonRPCResponse{JSONRPC: "2.0", ID: id, Result: []DocumentSymbol{}}
	}

	var docSymbols []DocumentSymbol
	for _, sym := range symbols {
		r := Range{
			Start: Position{Line: sym.LineStart - 1, Character: 0},
			End:   Position{Line: sym.LineEnd - 1, Character: 0},
		}
		docSymbols = append(docSymbols, DocumentSymbol{
			Name:           sym.Name,
			Detail:         string(sym.Type),
			Kind:           nodeTypeToSymbolKind(sym.Type),
			Range:          r,
			SelectionRange: r,
		})
	}

	return &jsonRPCResponse{JSONRPC: "2.0", ID: id, Result: docSymbols}
}

// ── Code Lens ───────────────────────────────────────────────────────────────

func (s *Server) handleCodeLens(id json.RawMessage, params json.RawMessage) *jsonRPCResponse {
	var p CodeLensParams
	if err := json.Unmarshal(params, &p); err != nil {
		return makeError(id, errCodeInvalidParm, "invalid params")
	}
	if s.querier == nil {
		return &jsonRPCResponse{JSONRPC: "2.0", ID: id, Result: []CodeLens{}}
	}

	path := uriToPath(p.TextDocument.URI)
	relPath := s.relPath(path)

	fileNode, err := s.querier.GetFileNode(relPath)
	if err != nil {
		return &jsonRPCResponse{JSONRPC: "2.0", ID: id, Result: []CodeLens{}}
	}

	symbols, err := s.querier.FindSymbolsInFile(fileNode.ID)
	if err != nil {
		return &jsonRPCResponse{JSONRPC: "2.0", ID: id, Result: []CodeLens{}}
	}

	var lenses []CodeLens
	for _, sym := range symbols {
		if !sym.Exported {
			continue
		}
		// Count files that use this symbol.
		refFiles, err := s.querier.FindFilesBySymbol(sym.Name)
		refCount := 0
		if err == nil {
			refCount = len(refFiles)
		}

		title := fmt.Sprintf("%d references", refCount)
		if refCount == 1 {
			title = "1 reference"
		}

		lenses = append(lenses, CodeLens{
			Range: Range{
				Start: Position{Line: sym.LineStart - 1, Character: 0},
				End:   Position{Line: sym.LineStart - 1, Character: 0},
			},
			Command: &Command{
				Title:   title,
				Command: "mantis.showReferences",
				Args:    []interface{}{sym.Name},
			},
		})
	}

	return &jsonRPCResponse{JSONRPC: "2.0", ID: id, Result: lenses}
}

// ── Diagnostics ─────────────────────────────────────────────────────────────

func (s *Server) publishDiagnosticsFor(uri string) {
	if s.querier == nil {
		return
	}

	path := uriToPath(uri)
	relPath := s.relPath(path)

	var diagnostics []Diagnostic

	// Check for dead code in this file.
	deadResult, err := intel.FindDead(s.querier, "")
	if err == nil {
		for _, sym := range deadResult.Symbols {
			if sym.FilePath == relPath {
				diagnostics = append(diagnostics, Diagnostic{
					Range: Range{
						Start: Position{Line: sym.LineStart - 1, Character: 0},
						End:   Position{Line: sym.LineEnd - 1, Character: 0},
					},
					Severity: SeverityWarning,
					Source:   "mantis",
					Message:  fmt.Sprintf("Dead code: %s %q is exported but never referenced", sym.Type, sym.Name),
				})
			}
		}
	}

	// Check if this file is a hotspot.
	stats := s.getTemporalStats()
	if stats != nil {
		hotspots := intel.Hotspots(stats, 20)
		for _, h := range hotspots {
			if h.Path == relPath {
				diagnostics = append(diagnostics, Diagnostic{
					Range: Range{
						Start: Position{Line: 0, Character: 0},
						End:   Position{Line: 0, Character: 0},
					},
					Severity: SeverityInformation,
					Source:   "mantis",
					Message:  fmt.Sprintf("Hotspot: %d commits, churn=%.1f — consider refactoring", h.Commits, h.ChurnScore),
				})
				break
			}
		}

		// Check for high coupling.
		coupled := intel.CouplingFor(stats, relPath, 5)
		for _, c := range coupled {
			if c.Coupling >= 0.7 {
				other := c.FileB
				if other == relPath {
					other = c.FileA
				}
				diagnostics = append(diagnostics, Diagnostic{
					Range: Range{
						Start: Position{Line: 0, Character: 0},
						End:   Position{Line: 0, Character: 0},
					},
					Severity: SeverityHint,
					Source:   "mantis",
					Message:  fmt.Sprintf("High coupling (%.0f%%) with %s", c.Coupling*100, other),
				})
			}
		}
	}

	s.sendNotification("textDocument/publishDiagnostics", PublishDiagnosticsParams{
		URI:         uri,
		Diagnostics: diagnostics,
	})
}

// ── Custom Mantis methods ───────────────────────────────────────────────────

func (s *Server) handleMantisHotspots(id json.RawMessage, params json.RawMessage) *jsonRPCResponse {
	var args struct {
		Limit int `json:"limit"`
		Days  int `json:"days"`
	}
	args.Limit = 20
	args.Days = 90
	if len(params) > 0 {
		_ = json.Unmarshal(params, &args)
	}

	stats, err := intel.Temporal(s.root, args.Days)
	if err != nil {
		return makeError(id, errCodeInternal, err.Error())
	}

	hotspots := intel.Hotspots(stats, args.Limit)

	type hotspotItem struct {
		Path       string  `json:"path"`
		Commits    int     `json:"commits"`
		Authors    int     `json:"authors"`
		ChurnScore float64 `json:"churnScore"`
		LastAuthor string  `json:"lastAuthor"`
	}
	items := make([]hotspotItem, len(hotspots))
	for i, h := range hotspots {
		items[i] = hotspotItem{
			Path:       h.Path,
			Commits:    h.Commits,
			Authors:    h.Authors,
			ChurnScore: h.ChurnScore,
			LastAuthor: h.LastAuthor,
		}
	}

	return &jsonRPCResponse{JSONRPC: "2.0", ID: id, Result: items}
}

func (s *Server) handleMantisDeadCode(id json.RawMessage) *jsonRPCResponse {
	if s.querier == nil {
		return makeError(id, errCodeInternal, "graph not initialized")
	}
	result, err := intel.FindDead(s.querier, "")
	if err != nil {
		return makeError(id, errCodeInternal, err.Error())
	}

	type deadItem struct {
		Name     string `json:"name"`
		Type     string `json:"type"`
		FilePath string `json:"filePath"`
		Line     int    `json:"line"`
	}
	items := make([]deadItem, len(result.Symbols))
	for i, sym := range result.Symbols {
		items[i] = deadItem{
			Name:     sym.Name,
			Type:     string(sym.Type),
			FilePath: sym.FilePath,
			Line:     sym.LineStart,
		}
	}

	return &jsonRPCResponse{JSONRPC: "2.0", ID: id, Result: items}
}

func (s *Server) handleMantisImpact(id json.RawMessage, params json.RawMessage) *jsonRPCResponse {
	var args struct {
		Target string `json:"target"`
		Depth  int    `json:"depth"`
	}
	args.Depth = 5
	if len(params) > 0 {
		_ = json.Unmarshal(params, &args)
	}
	if args.Target == "" {
		return makeError(id, errCodeInvalidParm, "missing target parameter")
	}
	if s.querier == nil {
		return makeError(id, errCodeInternal, "graph not initialized")
	}

	result, err := intel.Impact(s.querier, args.Target, args.Depth)
	if err != nil {
		return makeError(id, errCodeInternal, err.Error())
	}

	type impactFile struct {
		FilePath string `json:"filePath"`
		Depth    int    `json:"depth"`
		Risk     int    `json:"risk"`
	}

	var files []impactFile
	for depth, nodes := range result.ByDepth {
		for _, n := range nodes {
			files = append(files, impactFile{
				FilePath: n.FilePath,
				Depth:    depth,
				Risk:     result.RiskScores[n.ID],
			})
		}
	}

	resp := struct {
		Target     string       `json:"target"`
		TotalFiles int          `json:"totalFiles"`
		Files      []impactFile `json:"files"`
	}{
		Target:     result.Target,
		TotalFiles: result.TotalFiles,
		Files:      files,
	}

	return &jsonRPCResponse{JSONRPC: "2.0", ID: id, Result: resp}
}

func (s *Server) handleMantisCoupling(id json.RawMessage, params json.RawMessage) *jsonRPCResponse {
	var args struct {
		File  string `json:"file"`
		Limit int    `json:"limit"`
		Days  int    `json:"days"`
	}
	args.Limit = 10
	args.Days = 90
	if len(params) > 0 {
		_ = json.Unmarshal(params, &args)
	}
	if args.File == "" {
		return makeError(id, errCodeInvalidParm, "missing file parameter")
	}

	stats, err := intel.Temporal(s.root, args.Days)
	if err != nil {
		return makeError(id, errCodeInternal, err.Error())
	}

	coupled := intel.CouplingFor(stats, args.File, args.Limit)

	type coupledItem struct {
		File      string  `json:"file"`
		CoChanges int     `json:"coChanges"`
		Coupling  float64 `json:"coupling"`
	}
	items := make([]coupledItem, len(coupled))
	for i, c := range coupled {
		other := c.FileB
		if other == args.File {
			other = c.FileA
		}
		items[i] = coupledItem{
			File:      other,
			CoChanges: c.CoChanges,
			Coupling:  c.Coupling,
		}
	}

	return &jsonRPCResponse{JSONRPC: "2.0", ID: id, Result: items}
}

// ── Helpers ─────────────────────────────────────────────────────────────────

func (s *Server) relPath(absPath string) string {
	rel, err := filepath.Rel(s.root, absPath)
	if err != nil {
		return absPath
	}
	return rel
}

func nodeTypeToSymbolKind(nt graph.NodeType) SymbolKind {
	switch nt {
	case graph.NodeTypeFunction:
		return SymbolKindFunction
	case graph.NodeTypeMethod:
		return SymbolKindMethod
	case graph.NodeTypeClass:
		return SymbolKindClass
	case graph.NodeTypeInterface:
		return SymbolKindInterface
	case graph.NodeTypeTypeAlias:
		return SymbolKindStruct
	case graph.NodeTypeFile:
		return SymbolKindFile
	default:
		return SymbolKindVariable
	}
}
