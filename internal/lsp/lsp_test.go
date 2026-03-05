package lsp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/seedhire/mantis/internal/graph"
)

// mockServer creates a Server with in-memory reader/writer and nil querier
// for protocol-level testing.
func mockServer(input string) (*Server, *bytes.Buffer) {
	out := &bytes.Buffer{}
	s := &Server{
		in:      strings.NewReader(input),
		out:     out,
		version: "test-1.0",
		root:    "/tmp/test-project",
	}
	return s, out
}

// lspMessage wraps a JSON body with Content-Length framing.
func lspMessage(body string) string {
	return fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(body), body)
}

// readResponses parses all Content-Length framed responses from the output.
func readResponses(t *testing.T, output string) []jsonRPCResponse {
	t.Helper()
	var responses []jsonRPCResponse
	remaining := output
	for len(remaining) > 0 {
		idx := strings.Index(remaining, "Content-Length: ")
		if idx < 0 {
			break
		}
		remaining = remaining[idx:]
		headerEnd := strings.Index(remaining, "\r\n\r\n")
		if headerEnd < 0 {
			break
		}
		var contentLen int
		_, err := fmt.Sscanf(remaining, "Content-Length: %d", &contentLen)
		if err != nil {
			t.Fatalf("failed to parse Content-Length: %v", err)
		}
		bodyStart := headerEnd + 4
		if bodyStart+contentLen > len(remaining) {
			t.Fatalf("body shorter than Content-Length: want %d, have %d", contentLen, len(remaining)-bodyStart)
		}
		body := remaining[bodyStart : bodyStart+contentLen]
		var resp jsonRPCResponse
		if err := json.Unmarshal([]byte(body), &resp); err != nil {
			t.Fatalf("failed to parse response JSON: %v\nraw: %s", err, body)
		}
		responses = append(responses, resp)
		remaining = remaining[bodyStart+contentLen:]
	}
	return responses
}

func TestLSPInitialize(t *testing.T) {
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"capabilities":{}}}`
	s, out := mockServer(lspMessage(body))
	_ = s.Run()

	responses := readResponses(t, out.String())
	if len(responses) != 1 {
		t.Fatalf("expected 1 response, got %d", len(responses))
	}

	resp := responses[0]
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}

	data, _ := json.Marshal(resp.Result)
	var result InitializeResult
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("unmarshal init result: %v", err)
	}
	if result.ServerInfo.Name != "mantis-lsp" {
		t.Errorf("server name = %q, want mantis-lsp", result.ServerInfo.Name)
	}
	if result.ServerInfo.Version != "test-1.0" {
		t.Errorf("version = %q, want test-1.0", result.ServerInfo.Version)
	}
	if !result.Capabilities.HoverProvider {
		t.Error("expected hoverProvider = true")
	}
	if !result.Capabilities.DefinitionProvider {
		t.Error("expected definitionProvider = true")
	}
	if !result.Capabilities.ReferencesProvider {
		t.Error("expected referencesProvider = true")
	}
	if !result.Capabilities.DocumentSymbolProvider {
		t.Error("expected documentSymbolProvider = true")
	}
	if result.Capabilities.CodeLensProvider == nil {
		t.Error("expected codeLensProvider to be non-nil")
	}
	if result.Capabilities.TextDocumentSync == nil {
		t.Error("expected textDocumentSync to be non-nil")
	}
}

func TestLSPShutdown(t *testing.T) {
	body := `{"jsonrpc":"2.0","id":2,"method":"shutdown","params":{}}`
	s, out := mockServer(lspMessage(body))
	_ = s.Run()

	responses := readResponses(t, out.String())
	if len(responses) != 1 {
		t.Fatalf("expected 1 response, got %d", len(responses))
	}
	if responses[0].Error != nil {
		t.Fatalf("unexpected error on shutdown: %v", responses[0].Error)
	}
}

func TestLSPMethodNotFound(t *testing.T) {
	body := `{"jsonrpc":"2.0","id":3,"method":"nonexistent","params":{}}`
	s, out := mockServer(lspMessage(body))
	_ = s.Run()

	responses := readResponses(t, out.String())
	if len(responses) != 1 {
		t.Fatalf("expected 1 response, got %d", len(responses))
	}
	if responses[0].Error == nil {
		t.Fatal("expected error for nonexistent method")
	}
	if responses[0].Error.Code != errCodeMethodNotF {
		t.Errorf("error code = %d, want %d", responses[0].Error.Code, errCodeMethodNotF)
	}
}

func TestLSPParseError(t *testing.T) {
	s, out := mockServer(lspMessage("not valid json"))
	_ = s.Run()

	responses := readResponses(t, out.String())
	if len(responses) != 1 {
		t.Fatalf("expected 1 response, got %d", len(responses))
	}
	if responses[0].Error == nil {
		t.Fatal("expected parse error")
	}
	if responses[0].Error.Code != errCodeParse {
		t.Errorf("error code = %d, want %d", responses[0].Error.Code, errCodeParse)
	}
}

func TestLSPInvalidVersion(t *testing.T) {
	body := `{"jsonrpc":"1.0","id":4,"method":"initialize"}`
	s, out := mockServer(lspMessage(body))
	_ = s.Run()

	responses := readResponses(t, out.String())
	if len(responses) != 1 {
		t.Fatalf("expected 1 response, got %d", len(responses))
	}
	if responses[0].Error == nil {
		t.Fatal("expected error for invalid jsonrpc version")
	}
	if responses[0].Error.Code != errCodeInvalidReq {
		t.Errorf("error code = %d, want %d", responses[0].Error.Code, errCodeInvalidReq)
	}
}

func TestLSPNotificationNoResponse(t *testing.T) {
	body := `{"jsonrpc":"2.0","method":"initialized","params":{}}`
	s, out := mockServer(lspMessage(body))
	_ = s.Run()

	if out.Len() != 0 {
		t.Errorf("notification should produce no output, got: %s", out.String())
	}
}

func TestLSPMultipleMessages(t *testing.T) {
	msg1 := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"capabilities":{}}}`
	msg2 := `{"jsonrpc":"2.0","method":"initialized","params":{}}`
	msg3 := `{"jsonrpc":"2.0","id":2,"method":"shutdown","params":{}}`

	input := lspMessage(msg1) + lspMessage(msg2) + lspMessage(msg3)
	s, out := mockServer(input)
	_ = s.Run()

	responses := readResponses(t, out.String())
	// Should get 2 responses: initialize + shutdown (not initialized notification)
	if len(responses) != 2 {
		t.Errorf("expected 2 responses, got %d", len(responses))
	}
}

func TestLSPHoverNoQuerier(t *testing.T) {
	// With nil querier, hover should return null result (not crash).
	body := `{"jsonrpc":"2.0","id":5,"method":"textDocument/hover","params":{"textDocument":{"uri":"file:///tmp/test.go"},"position":{"line":0,"character":0}}}`
	s, out := mockServer(lspMessage(body))
	_ = s.Run()

	responses := readResponses(t, out.String())
	if len(responses) != 1 {
		t.Fatalf("expected 1 response, got %d", len(responses))
	}
	// Should return null result (file not in graph), not an error.
	if responses[0].Error != nil {
		t.Errorf("hover should return null result, not error: %v", responses[0].Error)
	}
}

func TestLSPDefinitionNoQuerier(t *testing.T) {
	body := `{"jsonrpc":"2.0","id":6,"method":"textDocument/definition","params":{"textDocument":{"uri":"file:///tmp/test.go"},"position":{"line":0,"character":0}}}`
	s, out := mockServer(lspMessage(body))
	_ = s.Run()

	responses := readResponses(t, out.String())
	if len(responses) != 1 {
		t.Fatalf("expected 1 response, got %d", len(responses))
	}
	if responses[0].Error != nil {
		t.Errorf("definition should return null result, not error: %v", responses[0].Error)
	}
}

func TestLSPDocumentSymbolNoQuerier(t *testing.T) {
	body := `{"jsonrpc":"2.0","id":7,"method":"textDocument/documentSymbol","params":{"textDocument":{"uri":"file:///tmp/test.go"}}}`
	s, out := mockServer(lspMessage(body))
	_ = s.Run()

	responses := readResponses(t, out.String())
	if len(responses) != 1 {
		t.Fatalf("expected 1 response, got %d", len(responses))
	}
	if responses[0].Error != nil {
		t.Errorf("documentSymbol should return empty array, not error: %v", responses[0].Error)
	}
}

func TestLSPCodeLensNoQuerier(t *testing.T) {
	body := `{"jsonrpc":"2.0","id":8,"method":"textDocument/codeLens","params":{"textDocument":{"uri":"file:///tmp/test.go"}}}`
	s, out := mockServer(lspMessage(body))
	_ = s.Run()

	responses := readResponses(t, out.String())
	if len(responses) != 1 {
		t.Fatalf("expected 1 response, got %d", len(responses))
	}
	if responses[0].Error != nil {
		t.Errorf("codeLens should return empty array, not error: %v", responses[0].Error)
	}
}

func TestLSPReferencesNoQuerier(t *testing.T) {
	body := `{"jsonrpc":"2.0","id":9,"method":"textDocument/references","params":{"textDocument":{"uri":"file:///tmp/test.go"},"position":{"line":0,"character":0}}}`
	s, out := mockServer(lspMessage(body))
	_ = s.Run()

	responses := readResponses(t, out.String())
	if len(responses) != 1 {
		t.Fatalf("expected 1 response, got %d", len(responses))
	}
	if responses[0].Error != nil {
		t.Errorf("references should return empty array, not error: %v", responses[0].Error)
	}
}

func TestLSPContentLengthFraming(t *testing.T) {
	// Verify output uses Content-Length framing.
	body := `{"jsonrpc":"2.0","id":1,"method":"shutdown","params":{}}`
	s, out := mockServer(lspMessage(body))
	_ = s.Run()

	raw := out.String()
	if !strings.HasPrefix(raw, "Content-Length: ") {
		t.Errorf("response should start with Content-Length header, got: %q", raw[:min(50, len(raw))])
	}
	if !strings.Contains(raw, "\r\n\r\n") {
		t.Error("response should contain \\r\\n\\r\\n header separator")
	}
}

// ── URI helper tests ────────────────────────────────────────────────────────

func TestURIToPath(t *testing.T) {
	cases := []struct {
		uri  string
		want string
	}{
		{"file:///tmp/test.go", "/tmp/test.go"},
		{"file:///home/user/project/main.go", "/home/user/project/main.go"},
		{"/tmp/test.go", "/tmp/test.go"}, // passthrough for non-URI
	}
	for _, tc := range cases {
		got := uriToPath(tc.uri)
		if got != tc.want {
			t.Errorf("uriToPath(%q) = %q, want %q", tc.uri, got, tc.want)
		}
	}
}

func TestPathToURI(t *testing.T) {
	got := pathToURI("/tmp/test.go")
	if got != "file:///tmp/test.go" {
		t.Errorf("pathToURI(/tmp/test.go) = %q, want file:///tmp/test.go", got)
	}
}

func TestNodeTypeToSymbolKind(t *testing.T) {
	cases := []struct {
		nt   string
		want SymbolKind
	}{
		{"function", SymbolKindFunction},
		{"method", SymbolKindMethod},
		{"class", SymbolKindClass},
		{"interface", SymbolKindInterface},
		{"type_alias", SymbolKindStruct},
		{"file", SymbolKindFile},
	}
	for _, tc := range cases {
		got := nodeTypeToSymbolKind(graph.NodeType(tc.nt))
		if got != tc.want {
			t.Errorf("nodeTypeToSymbolKind(%q) = %d, want %d", tc.nt, got, tc.want)
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
