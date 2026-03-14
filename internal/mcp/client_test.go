package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestNewMCPClient(t *testing.T) {
	c := NewMCPClient()
	if c == nil {
		t.Fatal("expected non-nil client")
	}
	if len(c.Tools()) != 0 {
		t.Error("expected no tools initially")
	}
	if len(c.ServerNames()) != 0 {
		t.Error("expected no servers initially")
	}
}

func TestLoadConfig_NoFile(t *testing.T) {
	c := NewMCPClient()
	err := c.LoadConfig(context.Background(), t.TempDir())
	if err != nil {
		t.Errorf("expected nil error for missing .mcp.json, got %v", err)
	}
}

func TestLoadConfig_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, ".mcp.json"), []byte("not json"), 0o644)
	c := NewMCPClient()
	err := c.LoadConfig(context.Background(), dir)
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestCallTool_UnknownServer(t *testing.T) {
	c := NewMCPClient()
	_, err := c.CallTool(context.Background(), "nonexistent", "tool", json.RawMessage("{}"))
	if err == nil {
		t.Error("expected error for unknown server")
	}
}

func TestClose_Empty(t *testing.T) {
	c := NewMCPClient()
	c.Close() // should not panic
}

func TestMCPConfigFile_Parse(t *testing.T) {
	data := `{
		"mcpServers": {
			"filesystem": {
				"command": "npx",
				"args": ["-y", "@modelcontextprotocol/server-filesystem", "/tmp"],
				"env": {"DEBUG": "1"}
			}
		}
	}`
	var cfg MCPConfigFile
	if err := json.Unmarshal([]byte(data), &cfg); err != nil {
		t.Fatalf("parse: %v", err)
	}
	fs, ok := cfg.MCPServers["filesystem"]
	if !ok {
		t.Fatal("missing filesystem server")
	}
	if fs.Command != "npx" {
		t.Errorf("command = %q, want npx", fs.Command)
	}
	if len(fs.Args) != 3 {
		t.Errorf("args len = %d, want 3", len(fs.Args))
	}
	if fs.Env["DEBUG"] != "1" {
		t.Error("env DEBUG should be 1")
	}
}
