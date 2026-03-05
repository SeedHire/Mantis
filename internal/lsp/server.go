package lsp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/seedhire/mantis/internal/graph"
	"github.com/seedhire/mantis/internal/intel"
)

// Server is an LSP server that communicates over stdio using Content-Length framing.
type Server struct {
	querier *graph.Querier
	db      *graph.DB
	root    string
	version string

	in  io.Reader
	out io.Writer
	mu  sync.Mutex // protects writes to out

	// Cached temporal stats (refreshed every 5 min).
	temporalMu    sync.RWMutex
	temporalStats *intel.TemporalStats
	temporalTime  time.Time
}

// NewServer creates an LSP server backed by the given graph database.
func NewServer(db *graph.DB, root, version string) *Server {
	return &Server{
		querier: graph.NewQuerier(db),
		db:      db,
		root:    root,
		version: version,
		in:      os.Stdin,
		out:     os.Stdout,
	}
}

// Run starts the server, reading Content-Length framed JSON-RPC messages from
// stdin and writing responses to stdout. It blocks until EOF or an error.
func (s *Server) Run() error {
	reader := bufio.NewReader(s.in)
	for {
		data, err := s.readMessage(reader)
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		if len(data) == 0 {
			continue
		}
		responses := s.handleMessage(data)
		for _, resp := range responses {
			s.writeMessage(resp)
		}
	}
}

// readMessage reads a Content-Length framed LSP message.
func (s *Server) readMessage(reader *bufio.Reader) ([]byte, error) {
	contentLength := -1
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break // end of headers
		}
		if strings.HasPrefix(line, "Content-Length: ") {
			val := strings.TrimPrefix(line, "Content-Length: ")
			n, err := strconv.Atoi(strings.TrimSpace(val))
			if err != nil {
				return nil, fmt.Errorf("invalid Content-Length: %w", err)
			}
			contentLength = n
		}
		// Ignore other headers (Content-Type, etc.)
	}
	if contentLength < 0 {
		return nil, fmt.Errorf("missing Content-Length header")
	}
	body := make([]byte, contentLength)
	if _, err := io.ReadFull(reader, body); err != nil {
		return nil, err
	}
	return body, nil
}

// writeMessage sends a JSON-RPC response with Content-Length framing.
func (s *Server) writeMessage(resp interface{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := json.Marshal(resp)
	if err != nil {
		return
	}
	header := fmt.Sprintf("Content-Length: %d\r\n\r\n", len(data))
	_, _ = io.WriteString(s.out, header)
	_, _ = s.out.Write(data)
}

// sendNotification sends a JSON-RPC notification (no ID).
func (s *Server) sendNotification(method string, params interface{}) {
	msg := struct {
		JSONRPC string      `json:"jsonrpc"`
		Method  string      `json:"method"`
		Params  interface{} `json:"params"`
	}{
		JSONRPC: "2.0",
		Method:  method,
		Params:  params,
	}
	s.writeMessage(msg)
}

func (s *Server) handleMessage(data []byte) []*jsonRPCResponse {
	var req jsonRPCRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return []*jsonRPCResponse{{
			JSONRPC: "2.0",
			Error:   &jsonRPCError{Code: errCodeParse, Message: "parse error"},
		}}
	}
	if req.JSONRPC != "2.0" {
		return []*jsonRPCResponse{{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error:   &jsonRPCError{Code: errCodeInvalidReq, Message: "invalid jsonrpc version"},
		}}
	}

	isNotification := req.ID == nil || string(req.ID) == "null"

	switch req.Method {
	case "initialize":
		return []*jsonRPCResponse{s.handleInitialize(req.ID)}
	case "initialized":
		return nil
	case "shutdown":
		return []*jsonRPCResponse{{JSONRPC: "2.0", ID: req.ID, Result: nil}}
	case "exit":
		os.Exit(0)
		return nil

	// Document sync
	case "textDocument/didOpen":
		s.handleDidOpen(req.Params)
		return nil
	case "textDocument/didSave":
		s.handleDidSave(req.Params)
		return nil

	// Language features
	case "textDocument/hover":
		return []*jsonRPCResponse{s.handleHover(req.ID, req.Params)}
	case "textDocument/definition":
		return []*jsonRPCResponse{s.handleDefinition(req.ID, req.Params)}
	case "textDocument/references":
		return []*jsonRPCResponse{s.handleReferences(req.ID, req.Params)}
	case "textDocument/documentSymbol":
		return []*jsonRPCResponse{s.handleDocumentSymbol(req.ID, req.Params)}
	case "textDocument/codeLens":
		return []*jsonRPCResponse{s.handleCodeLens(req.ID, req.Params)}

	// Custom Mantis methods
	case "mantis/hotspots":
		return []*jsonRPCResponse{s.handleMantisHotspots(req.ID, req.Params)}
	case "mantis/deadCode":
		return []*jsonRPCResponse{s.handleMantisDeadCode(req.ID)}
	case "mantis/impact":
		return []*jsonRPCResponse{s.handleMantisImpact(req.ID, req.Params)}
	case "mantis/coupling":
		return []*jsonRPCResponse{s.handleMantisCoupling(req.ID, req.Params)}

	default:
		if isNotification {
			return nil
		}
		return []*jsonRPCResponse{{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error:   &jsonRPCError{Code: errCodeMethodNotF, Message: fmt.Sprintf("method not found: %s", req.Method)},
		}}
	}
}

func (s *Server) handleInitialize(id json.RawMessage) *jsonRPCResponse {
	ver := s.version
	if ver == "" {
		ver = "dev"
	}
	result := InitializeResult{
		Capabilities: ServerCapabilities{
			HoverProvider:          true,
			DefinitionProvider:     true,
			ReferencesProvider:     true,
			DocumentSymbolProvider: true,
			CodeLensProvider: &struct {
				ResolveProvider bool `json:"resolveProvider"`
			}{ResolveProvider: false},
			TextDocumentSync: &TextDocumentSyncOptions{
				OpenClose: true,
				Save: &struct {
					IncludeText bool `json:"includeText"`
				}{IncludeText: false},
			},
		},
	}
	result.ServerInfo.Name = "mantis-lsp"
	result.ServerInfo.Version = ver
	return &jsonRPCResponse{JSONRPC: "2.0", ID: id, Result: result}
}

// getTemporalStats returns cached temporal stats, refreshing if stale.
func (s *Server) getTemporalStats() *intel.TemporalStats {
	s.temporalMu.RLock()
	if s.temporalStats != nil && time.Since(s.temporalTime) < 5*time.Minute {
		stats := s.temporalStats
		s.temporalMu.RUnlock()
		return stats
	}
	s.temporalMu.RUnlock()

	s.temporalMu.Lock()
	defer s.temporalMu.Unlock()
	// Double-check after acquiring write lock.
	if s.temporalStats != nil && time.Since(s.temporalTime) < 5*time.Minute {
		return s.temporalStats
	}
	stats, err := intel.Temporal(s.root, 90)
	if err != nil {
		return nil
	}
	s.temporalStats = stats
	s.temporalTime = time.Now()
	return stats
}

func makeError(id json.RawMessage, code int, msg string) *jsonRPCResponse {
	return &jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &jsonRPCError{Code: code, Message: msg},
	}
}
