package mcp

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// mockServer creates a Server with a bytes.Buffer as output and the given
// input. Since we can't easily create a real graph.DB in unit tests, we test
// the protocol layer with a nil querier and check error handling.
func mockServer(input string) (*Server, *bytes.Buffer) {
	out := &bytes.Buffer{}
	s := &Server{
		in:      strings.NewReader(input),
		out:     out,
		version: "test-1.0",
		tools:   &ToolHandler{},
	}
	return s, out
}

func readResponse(t *testing.T, output string) jsonRPCResponse {
	t.Helper()
	var resp jsonRPCResponse
	if err := json.Unmarshal([]byte(strings.TrimSpace(output)), &resp); err != nil {
		t.Fatalf("failed to parse response: %v\nraw: %s", err, output)
	}
	return resp
}

func TestInitialize(t *testing.T) {
	req := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`
	s, out := mockServer(req)
	_ = s.Run()

	resp := readResponse(t, out.String())
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}

	data, _ := json.Marshal(resp.Result)
	var result mcpInitResult
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("unmarshal init result: %v", err)
	}
	if result.ServerInfo.Name != "mantis-graph" {
		t.Errorf("server name = %q, want mantis-graph", result.ServerInfo.Name)
	}
	if result.Capabilities.Tools == nil {
		t.Error("expected tools capability to be non-nil")
	}
	if result.ProtocolVersion != "2024-11-05" {
		t.Errorf("protocol version = %q, want 2024-11-05", result.ProtocolVersion)
	}
}

func TestToolsList(t *testing.T) {
	req := `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`
	s, out := mockServer(req)
	_ = s.Run()

	resp := readResponse(t, out.String())
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}

	data, _ := json.Marshal(resp.Result)
	var result mcpToolsListResult
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("unmarshal tools list: %v", err)
	}

	expectedTools := []string{
		"mantis_impact", "mantis_find", "mantis_hotspots", "mantis_coupling",
		"mantis_dead", "mantis_imports", "mantis_importers", "mantis_context",
	}
	if len(result.Tools) != len(expectedTools) {
		t.Fatalf("got %d tools, want %d", len(result.Tools), len(expectedTools))
	}

	toolNames := make(map[string]bool)
	for _, tool := range result.Tools {
		toolNames[tool.Name] = true
		if tool.Description == "" {
			t.Errorf("tool %s has empty description", tool.Name)
		}
		if tool.InputSchema.Type != "object" {
			t.Errorf("tool %s schema type = %q, want object", tool.Name, tool.InputSchema.Type)
		}
	}
	for _, name := range expectedTools {
		if !toolNames[name] {
			t.Errorf("missing tool: %s", name)
		}
	}
}

func TestToolsCallUnknown(t *testing.T) {
	req := `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"unknown_tool","arguments":{}}}`
	s, out := mockServer(req)
	_ = s.Run()

	resp := readResponse(t, out.String())
	if resp.Error != nil {
		t.Fatalf("unknown tool should return tool-level error, not JSON-RPC error: %v", resp.Error)
	}

	data, _ := json.Marshal(resp.Result)
	var result mcpToolCallResult
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("unmarshal call result: %v", err)
	}
	if !result.IsError {
		t.Error("expected isError=true for unknown tool")
	}
	if len(result.Content) == 0 || !strings.Contains(result.Content[0].Text, "unknown tool") {
		t.Errorf("expected error about unknown tool, got: %v", result.Content)
	}
}

func TestToolsCallMissingParam(t *testing.T) {
	// mantis_impact requires "target"
	req := `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"mantis_impact","arguments":{}}}`
	s, out := mockServer(req)
	_ = s.Run()

	resp := readResponse(t, out.String())
	data, _ := json.Marshal(resp.Result)
	var result mcpToolCallResult
	_ = json.Unmarshal(data, &result)
	if !result.IsError {
		t.Error("expected isError=true for missing required param")
	}
}

func TestPing(t *testing.T) {
	req := `{"jsonrpc":"2.0","id":5,"method":"ping","params":{}}`
	s, out := mockServer(req)
	_ = s.Run()

	resp := readResponse(t, out.String())
	if resp.Error != nil {
		t.Fatalf("ping should succeed: %v", resp.Error)
	}
}

func TestMethodNotFound(t *testing.T) {
	req := `{"jsonrpc":"2.0","id":6,"method":"nonexistent","params":{}}`
	s, out := mockServer(req)
	_ = s.Run()

	resp := readResponse(t, out.String())
	if resp.Error == nil {
		t.Fatal("expected error for nonexistent method")
	}
	if resp.Error.Code != errCodeMethodNotF {
		t.Errorf("error code = %d, want %d", resp.Error.Code, errCodeMethodNotF)
	}
}

func TestParseError(t *testing.T) {
	s, out := mockServer("not valid json")
	_ = s.Run()

	resp := readResponse(t, out.String())
	if resp.Error == nil {
		t.Fatal("expected parse error")
	}
	if resp.Error.Code != errCodeParse {
		t.Errorf("error code = %d, want %d", resp.Error.Code, errCodeParse)
	}
}

func TestInvalidJSONRPCVersion(t *testing.T) {
	req := `{"jsonrpc":"1.0","id":7,"method":"initialize"}`
	s, out := mockServer(req)
	_ = s.Run()

	resp := readResponse(t, out.String())
	if resp.Error == nil {
		t.Fatal("expected error for invalid jsonrpc version")
	}
	if resp.Error.Code != errCodeInvalidReq {
		t.Errorf("error code = %d, want %d", resp.Error.Code, errCodeInvalidReq)
	}
}

func TestNotificationNoResponse(t *testing.T) {
	// "initialized" is a notification — no response should be sent
	req := `{"jsonrpc":"2.0","method":"initialized","params":{}}`
	s, out := mockServer(req)
	_ = s.Run()

	if out.Len() != 0 {
		t.Errorf("notification should produce no output, got: %s", out.String())
	}
}

func TestMultipleMessages(t *testing.T) {
	lines := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","method":"initialized","params":{}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`,
		`{"jsonrpc":"2.0","id":3,"method":"ping","params":{}}`,
	}, "\n")

	s, out := mockServer(lines)
	_ = s.Run()

	// Should get 3 responses (initialize, tools/list, ping) — no response for notification
	responses := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(responses) != 3 {
		t.Errorf("expected 3 responses, got %d:\n%s", len(responses), out.String())
	}
}

func TestEmptyLines(t *testing.T) {
	input := "\n\n" + `{"jsonrpc":"2.0","id":1,"method":"ping","params":{}}` + "\n\n"
	s, out := mockServer(input)
	_ = s.Run()

	responses := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(responses) != 1 {
		t.Errorf("expected 1 response, got %d", len(responses))
	}
}

func TestIntArg(t *testing.T) {
	args := map[string]interface{}{
		"int_val":   float64(42),
		"str_val":   "hello",
		"nil_val":   nil,
	}

	if got := intArg(args, "int_val", 0); got != 42 {
		t.Errorf("intArg(int_val) = %d, want 42", got)
	}
	if got := intArg(args, "missing", 99); got != 99 {
		t.Errorf("intArg(missing) = %d, want 99", got)
	}
	if got := intArg(args, "str_val", 7); got != 7 {
		t.Errorf("intArg(str_val) = %d, want 7 (fallback)", got)
	}
}
