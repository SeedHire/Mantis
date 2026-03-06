// Package mcp implements a Model Context Protocol (MCP) server over stdio.
// It exposes Mantis's graph intelligence as tools that any MCP-compatible
// client (Claude Code, Cursor, Zed, etc.) can call.
//
// Usage:
//
//	mantis mcp
//
// Configure in .claude/settings.json:
//
//	{ "mcpServers": { "mantis": { "command": "mantis", "args": ["mcp"] } } }
package mcp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/seedhire/mantis/internal/graph"
)

// ── JSON-RPC 2.0 types ──────────────────────────────────────────────────────

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

// MCP error codes
const (
	errCodeParse       = -32700
	errCodeInvalidReq  = -32600
	errCodeMethodNotF  = -32601
	errCodeInvalidParm = -32602
	errCodeInternal    = -32603
)

// ── MCP protocol types ──────────────────────────────────────────────────────

type mcpServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type mcpCapabilities struct {
	Tools *struct{} `json:"tools,omitempty"`
}

type mcpInitResult struct {
	ProtocolVersion string          `json:"protocolVersion"`
	Capabilities    mcpCapabilities `json:"capabilities"`
	ServerInfo      mcpServerInfo   `json:"serverInfo"`
}

type mcpToolSchema struct {
	Type       string                 `json:"type"`
	Properties map[string]mcpToolProp `json:"properties,omitempty"`
	Required   []string               `json:"required,omitempty"`
}

type mcpToolProp struct {
	Type        string `json:"type"`
	Description string `json:"description"`
}

type mcpTool struct {
	Name        string        `json:"name"`
	Description string        `json:"description"`
	InputSchema mcpToolSchema `json:"inputSchema"`
}

type mcpToolsListResult struct {
	Tools []mcpTool `json:"tools"`
}

type mcpToolCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type mcpContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type mcpToolCallResult struct {
	Content []mcpContentBlock `json:"content"`
	IsError bool              `json:"isError,omitempty"`
}

// ── Server ──────────────────────────────────────────────────────────────────

// Server is an MCP server that communicates over stdio.
type Server struct {
	querier *graph.Querier
	db      *graph.DB
	root    string
	version string

	in  io.Reader
	out io.Writer
	mu  sync.Mutex // protects writes to out

	tools *ToolHandler
}

// NewServer creates an MCP server backed by the given graph database.
func NewServer(db *graph.DB, root, version string) *Server {
	q := graph.NewQuerier(db)
	return &Server{
		querier: q,
		db:      db,
		root:    root,
		version: version,
		in:      os.Stdin,
		out:     os.Stdout,
		tools:   NewToolHandler(q, db, root),
	}
}

// Run starts the server, reading JSON-RPC messages from stdin and writing
// responses to stdout. It blocks until EOF or an error.
func (s *Server) Run() error {
	scanner := bufio.NewScanner(s.in)
	// MCP messages can be large (context bundles, etc.)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		resp := s.handleMessage(line)
		if resp != nil {
			s.send(resp)
		}
	}
	return scanner.Err()
}

func (s *Server) handleMessage(data []byte) *jsonRPCResponse {
	var req jsonRPCRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return &jsonRPCResponse{
			JSONRPC: "2.0",
			Error:   &jsonRPCError{Code: errCodeParse, Message: "parse error"},
		}
	}
	if req.JSONRPC != "2.0" {
		return &jsonRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error:   &jsonRPCError{Code: errCodeInvalidReq, Message: "invalid jsonrpc version"},
		}
	}

	// Notifications (no ID) don't get responses.
	isNotification := req.ID == nil || string(req.ID) == "null"

	switch req.Method {
	case "initialize":
		return s.handleInitialize(req.ID)
	case "initialized":
		// Client acknowledgement — no response needed.
		return nil
	case "tools/list":
		return s.handleToolsList(req.ID)
	case "tools/call":
		return s.handleToolsCall(req.ID, req.Params)
	case "ping":
		return &jsonRPCResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]interface{}{}}
	default:
		if isNotification {
			return nil
		}
		return &jsonRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error:   &jsonRPCError{Code: errCodeMethodNotF, Message: fmt.Sprintf("method not found: %s", req.Method)},
		}
	}
}

func (s *Server) handleInitialize(id json.RawMessage) *jsonRPCResponse {
	ver := s.version
	if ver == "" {
		ver = "dev"
	}
	return &jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result: mcpInitResult{
			ProtocolVersion: "2024-11-05",
			Capabilities:    mcpCapabilities{Tools: &struct{}{}},
			ServerInfo:      mcpServerInfo{Name: "mantis-graph", Version: ver},
		},
	}
}

func (s *Server) handleToolsList(id json.RawMessage) *jsonRPCResponse {
	return &jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result:  mcpToolsListResult{Tools: s.tools.ListTools()},
	}
}

func (s *Server) handleToolsCall(id json.RawMessage, params json.RawMessage) *jsonRPCResponse {
	var call mcpToolCallParams
	if err := json.Unmarshal(params, &call); err != nil {
		return &jsonRPCResponse{
			JSONRPC: "2.0",
			ID:      id,
			Error:   &jsonRPCError{Code: errCodeInvalidParm, Message: "invalid tool call params"},
		}
	}

	result, toolErr := s.tools.Call(call.Name, call.Arguments)
	if toolErr != nil {
		return &jsonRPCResponse{
			JSONRPC: "2.0",
			ID:      id,
			Result: mcpToolCallResult{
				Content: []mcpContentBlock{{Type: "text", Text: toolErr.Error()}},
				IsError: true,
			},
		}
	}

	return &jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result: mcpToolCallResult{
			Content: []mcpContentBlock{{Type: "text", Text: result}},
		},
	}
}

func (s *Server) send(resp *jsonRPCResponse) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := json.Marshal(resp)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(s.out, "%s\n", data)
}
