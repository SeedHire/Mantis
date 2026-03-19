// MCP client — connects to external MCP servers via stdio transport.
// Reads .mcp.json from project root for server configuration.
//
// Configuration format (.mcp.json):
//
//	{
//	  "mcpServers": {
//	    "filesystem": {
//	      "command": "npx",
//	      "args": ["-y", "@modelcontextprotocol/server-filesystem", "/tmp"]
//	    }
//	  }
//	}
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// MCPClient manages connections to external MCP servers.
type MCPClient struct {
	servers map[string]*serverConn
	mu      sync.RWMutex
}

// serverConn is a connection to a single MCP server process.
type serverConn struct {
	name    string
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	stdout  *bufio.Reader
	tools   []MCPToolDef
	nextID  atomic.Int64
	mu      sync.Mutex // serializes requests
}

// MCPToolDef describes a tool exposed by an MCP server.
type MCPToolDef struct {
	ServerName  string          `json:"-"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// MCPServerConfig describes how to launch an MCP server.
type MCPServerConfig struct {
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env,omitempty"`
}

// MCPConfigFile is the top-level .mcp.json structure.
type MCPConfigFile struct {
	MCPServers map[string]MCPServerConfig `json:"mcpServers"`
}

// NewMCPClient creates a new MCP client.
func NewMCPClient() *MCPClient {
	return &MCPClient{servers: make(map[string]*serverConn)}
}

// LoadConfig reads .mcp.json from root and starts all configured servers.
func (c *MCPClient) LoadConfig(ctx context.Context, root string) error {
	configPath := filepath.Join(root, ".mcp.json")
	data, err := os.ReadFile(configPath)
	if os.IsNotExist(err) {
		return nil // no config = no servers
	}
	if err != nil {
		return fmt.Errorf("read .mcp.json: %w", err)
	}

	var cfg MCPConfigFile
	if err := json.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("parse .mcp.json: %w", err)
	}

	for name, serverCfg := range cfg.MCPServers {
		if err := c.StartServer(ctx, name, serverCfg); err != nil {
			fmt.Fprintf(os.Stderr, "mcp: failed to start %s: %v\n", name, err)
		}
	}
	return nil
}

// StartServer launches a single MCP server process.
func (c *MCPClient) StartServer(ctx context.Context, name string, cfg MCPServerConfig) error {
	cmd := exec.CommandContext(ctx, cfg.Command, cfg.Args...)
	// Pass through environment with overrides.
	cmd.Env = os.Environ()
	for k, v := range cfg.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	cmd.Stderr = os.Stderr // forward errors

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", cfg.Command, err)
	}

	conn := &serverConn{
		name:   name,
		cmd:    cmd,
		stdin:  stdin,
		stdout: bufio.NewReader(stdout),
	}

	// Initialize the server.
	initCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	if err := conn.initialize(initCtx); err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		return fmt.Errorf("initialize %s: %w", name, err)
	}

	// List available tools.
	tools, err := conn.listTools(initCtx)
	if err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		return fmt.Errorf("list tools %s: %w", name, err)
	}
	for i := range tools {
		tools[i].ServerName = name
	}
	conn.tools = tools

	c.mu.Lock()
	c.servers[name] = conn
	c.mu.Unlock()

	return nil
}

// Tools returns all tools from all connected MCP servers.
func (c *MCPClient) Tools() []MCPToolDef {
	c.mu.RLock()
	defer c.mu.RUnlock()

	var all []MCPToolDef
	for _, conn := range c.servers {
		all = append(all, conn.tools...)
	}
	return all
}

// CallTool invokes a tool on the appropriate MCP server.
func (c *MCPClient) CallTool(ctx context.Context, serverName, toolName string, args json.RawMessage) (string, error) {
	c.mu.RLock()
	conn, ok := c.servers[serverName]
	c.mu.RUnlock()
	if !ok {
		return "", fmt.Errorf("mcp server %q not connected", serverName)
	}
	return conn.callTool(ctx, toolName, args)
}

// ServerNames returns the names of all connected servers.
func (c *MCPClient) ServerNames() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	names := make([]string, 0, len(c.servers))
	for name := range c.servers {
		names = append(names, name)
	}
	return names
}

// Close stops all MCP server processes.
func (c *MCPClient) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, conn := range c.servers {
		conn.stdin.Close()
		conn.cmd.Process.Kill()
		conn.cmd.Wait()
	}
	c.servers = make(map[string]*serverConn)
}

// ── serverConn methods ──────────────────────────────────────────────────────

func (s *serverConn) initialize(ctx context.Context) error {
	req := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      s.nextID.Add(1),
		"method":  "initialize",
		"params": map[string]interface{}{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]interface{}{},
			"clientInfo": map[string]string{
				"name":    "mantis",
				"version": "0.7",
			},
		},
	}
	resp, err := s.sendRequest(ctx, req)
	if err != nil {
		return err
	}
	_ = resp // we only care that it didn't error

	// Send initialized notification.
	notif := map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  "notifications/initialized",
	}
	data, _ := json.Marshal(notif)
	s.mu.Lock()
	_, err = fmt.Fprintf(s.stdin, "%s\n", data)
	s.mu.Unlock()
	return err
}

func (s *serverConn) listTools(ctx context.Context) ([]MCPToolDef, error) {
	req := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      s.nextID.Add(1),
		"method":  "tools/list",
	}
	resp, err := s.sendRequest(ctx, req)
	if err != nil {
		return nil, err
	}

	var result struct {
		Tools []MCPToolDef `json:"tools"`
	}
	raw, _ := json.Marshal(resp)
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("parse tools/list: %w", err)
	}
	return result.Tools, nil
}

func (s *serverConn) callTool(ctx context.Context, toolName string, args json.RawMessage) (string, error) {
	req := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      s.nextID.Add(1),
		"method":  "tools/call",
		"params": map[string]interface{}{
			"name":      toolName,
			"arguments": json.RawMessage(args),
		},
	}
	resp, err := s.sendRequest(ctx, req)
	if err != nil {
		return "", err
	}

	// MCP tools/call returns {content: [{type, text}]}.
	raw, _ := json.Marshal(resp)
	var result struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return string(raw), nil
	}
	var text string
	for _, c := range result.Content {
		if c.Type == "text" {
			text += c.Text
		}
	}
	if text == "" {
		return string(raw), nil
	}
	return text, nil
}

func (s *serverConn) sendRequest(ctx context.Context, req map[string]interface{}) (interface{}, error) {
	data, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	// Hold mutex only for the write+read pair, but release before blocking.
	// This prevents deadlock when the read hangs indefinitely.
	s.mu.Lock()
	if _, err := fmt.Fprintf(s.stdin, "%s\n", data); err != nil {
		s.mu.Unlock()
		return nil, fmt.Errorf("write: %w", err)
	}
	s.mu.Unlock()

	// Read response line (newline-delimited JSON-RPC) without holding the mutex.
	done := make(chan struct{})
	var line string
	var readErr error
	go func() {
		line, readErr = s.stdout.ReadString('\n')
		close(done)
	}()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-done:
		if readErr != nil {
			return nil, fmt.Errorf("read: %w", readErr)
		}
	}

	var resp struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  interface{}     `json:"result"`
		Error   *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(line), &resp); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("mcp error %d: %s", resp.Error.Code, resp.Error.Message)
	}
	return resp.Result, nil
}
