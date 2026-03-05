// Package lsp implements a minimal LSP server that augments standard language
// servers (gopls, tsserver) with Mantis graph intelligence — hover enrichment,
// dead-code diagnostics, hotspot indicators, and code lens for reference counts.
package lsp

import (
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"runtime"
	"strings"
)

// ── JSON-RPC 2.0 types (shared with MCP but redefined to avoid coupling) ────

type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  interface{}     `json:"result,omitempty"`
	Error   *jsonRPCError   `json:"error,omitempty"`
}

type jsonRPCError struct {
	Code    int         `json:"code"`
	Message string      `json:"message"`
	Data    interface{} `json:"data,omitempty"`
}

// JSON-RPC error codes
const (
	errCodeParse       = -32700
	errCodeInvalidReq  = -32600
	errCodeMethodNotF  = -32601
	errCodeInvalidParm = -32602
	errCodeInternal    = -32603
)

// ── LSP protocol types ──────────────────────────────────────────────────────

// Position in a text document (0-based line and character).
type Position struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

// Range in a text document.
type Range struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}

// Location represents a location in a resource.
type Location struct {
	URI   string `json:"uri"`
	Range Range  `json:"range"`
}

// MarkupContent represents a string value with a markup kind.
type MarkupContent struct {
	Kind  string `json:"kind"` // "plaintext" or "markdown"
	Value string `json:"value"`
}

// Hover result returned by textDocument/hover.
type Hover struct {
	Contents MarkupContent `json:"contents"`
	Range    *Range        `json:"range,omitempty"`
}

// SymbolKind constants (subset).
type SymbolKind int

const (
	SymbolKindFile      SymbolKind = 1
	SymbolKindModule    SymbolKind = 2
	SymbolKindClass     SymbolKind = 5
	SymbolKindMethod    SymbolKind = 6
	SymbolKindFunction  SymbolKind = 12
	SymbolKindVariable  SymbolKind = 13
	SymbolKindInterface SymbolKind = 11
	SymbolKindStruct    SymbolKind = 23
)

// DocumentSymbol represents a symbol in a document.
type DocumentSymbol struct {
	Name           string           `json:"name"`
	Detail         string           `json:"detail,omitempty"`
	Kind           SymbolKind       `json:"kind"`
	Range          Range            `json:"range"`
	SelectionRange Range            `json:"selectionRange"`
	Children       []DocumentSymbol `json:"children,omitempty"`
}

// DiagnosticSeverity constants.
type DiagnosticSeverity int

const (
	SeverityError       DiagnosticSeverity = 1
	SeverityWarning     DiagnosticSeverity = 2
	SeverityInformation DiagnosticSeverity = 3
	SeverityHint        DiagnosticSeverity = 4
)

// Diagnostic represents an issue in a document.
type Diagnostic struct {
	Range    Range              `json:"range"`
	Severity DiagnosticSeverity `json:"severity"`
	Source   string             `json:"source,omitempty"`
	Message  string             `json:"message"`
}

// PublishDiagnosticsParams is sent as a notification from server to client.
type PublishDiagnosticsParams struct {
	URI         string       `json:"uri"`
	Diagnostics []Diagnostic `json:"diagnostics"`
}

// CodeLens represents a command that should be shown along with source text.
type CodeLens struct {
	Range   Range    `json:"range"`
	Command *Command `json:"command,omitempty"`
}

// Command represents a reference to a command.
type Command struct {
	Title   string        `json:"title"`
	Command string        `json:"command"`
	Args    []interface{} `json:"arguments,omitempty"`
}

// ── Initialize types ────────────────────────────────────────────────────────

// ServerCapabilities declares what the server can do.
type ServerCapabilities struct {
	HoverProvider          bool `json:"hoverProvider,omitempty"`
	DefinitionProvider     bool `json:"definitionProvider,omitempty"`
	ReferencesProvider     bool `json:"referencesProvider,omitempty"`
	DocumentSymbolProvider bool `json:"documentSymbolProvider,omitempty"`
	CodeLensProvider       *struct {
		ResolveProvider bool `json:"resolveProvider"`
	} `json:"codeLensProvider,omitempty"`
	TextDocumentSync *TextDocumentSyncOptions `json:"textDocumentSync,omitempty"`
}

// TextDocumentSyncOptions specifies how the server wants documents synced.
type TextDocumentSyncOptions struct {
	OpenClose bool `json:"openClose"`
	Save      *struct {
		IncludeText bool `json:"includeText"`
	} `json:"save,omitempty"`
}

// InitializeResult is returned from initialize.
type InitializeResult struct {
	Capabilities ServerCapabilities `json:"capabilities"`
	ServerInfo   struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"serverInfo"`
}

// ── Common request param types ──────────────────────────────────────────────

// TextDocumentIdentifier identifies a text document.
type TextDocumentIdentifier struct {
	URI string `json:"uri"`
}

// TextDocumentPositionParams is used for hover, definition, references.
type TextDocumentPositionParams struct {
	TextDocument TextDocumentIdentifier `json:"textDocument"`
	Position     Position               `json:"position"`
}

// DocumentSymbolParams for documentSymbol requests.
type DocumentSymbolParams struct {
	TextDocument TextDocumentIdentifier `json:"textDocument"`
}

// CodeLensParams for codeLens requests.
type CodeLensParams struct {
	TextDocument TextDocumentIdentifier `json:"textDocument"`
}

// DidOpenTextDocumentParams for textDocument/didOpen.
type DidOpenTextDocumentParams struct {
	TextDocument struct {
		URI string `json:"uri"`
	} `json:"textDocument"`
}

// DidSaveTextDocumentParams for textDocument/didSave.
type DidSaveTextDocumentParams struct {
	TextDocument TextDocumentIdentifier `json:"textDocument"`
}

// ── URI helpers ─────────────────────────────────────────────────────────────

// uriToPath converts a file:// URI to a filesystem path.
func uriToPath(uri string) string {
	if !strings.HasPrefix(uri, "file://") {
		return uri
	}
	u, err := url.Parse(uri)
	if err != nil {
		return strings.TrimPrefix(uri, "file://")
	}
	path := u.Path
	// On Windows, the path starts with /C: — strip the leading slash.
	if runtime.GOOS == "windows" && len(path) > 2 && path[0] == '/' && path[2] == ':' {
		path = path[1:]
	}
	return filepath.FromSlash(path)
}

// pathToURI converts a filesystem path to a file:// URI.
func pathToURI(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	abs = filepath.ToSlash(abs)
	if !strings.HasPrefix(abs, "/") {
		abs = "/" + abs
	}
	return fmt.Sprintf("file://%s", abs)
}
