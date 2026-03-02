// Package agent implements the AgentToolkit (the 5 core ACI tools) and the
// multi-agent orchestrator (fan-out → parallel workers → synthesizer).
//
// Design follows SWE-agent's Agent-Computer Interface paper:
//   1. read_file    — partial file view, stays within token budget
//   2. write_file   — create or overwrite a file
//   3. run_bash     — shell with allowlist, structured output, output cap
//   4. search_codebase — semantic search over graph + embeddings
//   5. finish       — explicit done signal, prevents runaway loops
package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/seedhire/mantis/internal/embeddings"
	"github.com/seedhire/mantis/internal/graph"
	"github.com/seedhire/mantis/internal/ollama"
)

const (
	maxBashOutput = 8000 // characters (~2000 tokens)
	defaultTimeout = 30  // seconds
)

// ErrFinished is returned by Dispatch when the agent calls the "finish" tool.
// The summary is stored in the FinishedError value.
var ErrFinished = errors.New("agent finished")

// FinishedError wraps ErrFinished with the agent's summary message.
type FinishedError struct{ Summary string }

func (e *FinishedError) Error() string  { return "agent finished: " + e.Summary }
func (e *FinishedError) Unwrap() error  { return ErrFinished }
func (e *FinishedError) Is(t error) bool { return t == ErrFinished }

// allowedPrefixes is the bash command allowlist (prefix matching).
// Prevents arbitrary shell execution while still supporting all dev workflows.
var allowedPrefixes = []string{
	"go build", "go test", "go vet", "go fmt", "go run",
	"npm run", "npm test", "npm install",
	"cargo check", "cargo build", "cargo test",
	"python -m pytest", "python3 -m pytest",
	"git diff", "git status", "git log",
}

// AgentToolkit provides typed tool access for coding agents.
// All file and bash operations are scoped to projectRoot for safety.
type AgentToolkit struct {
	projectRoot string
	querier     *graph.Querier
	embStore    *embeddings.Store
	fileMu      sync.Mutex // guards WriteFile against parallel worker races
}

// NewToolkit creates a toolkit bound to the given project root.
// querier and embStore may be nil; their tools return graceful errors.
func NewToolkit(projectRoot string, querier *graph.Querier, embStore *embeddings.Store) *AgentToolkit {
	return &AgentToolkit{
		projectRoot: projectRoot,
		querier:     querier,
		embStore:    embStore,
	}
}

// ── Core tool implementations ────────────────────────────────────────────────

// ReadFile reads lines [startLine, endLine] (1-based, inclusive) from path.
// If endLine <= 0, reads to EOF. Path must be relative to projectRoot.
func (t *AgentToolkit) ReadFile(path string, startLine, endLine int) (string, error) {
	abs, err := t.safePath(path)
	if err != nil {
		return "", err
	}
	f, err := os.Open(abs)
	if err != nil {
		return "", err
	}
	defer f.Close()

	var sb strings.Builder
	scanner := bufio.NewScanner(f)
	line := 0
	for scanner.Scan() {
		line++
		if startLine > 0 && line < startLine {
			continue
		}
		if endLine > 0 && line > endLine {
			break
		}
		sb.WriteString(scanner.Text())
		sb.WriteByte('\n')
	}
	return sb.String(), scanner.Err()
}

// WriteFile creates or overwrites path with content.
// Path must be relative to projectRoot.
// Protected by a mutex so parallel workers don't corrupt shared files.
func (t *AgentToolkit) WriteFile(path, content string) error {
	abs, err := t.safePath(path)
	if err != nil {
		return err
	}
	t.fileMu.Lock()
	defer t.fileMu.Unlock()
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(abs), err)
	}
	return os.WriteFile(abs, []byte(content), 0o644)
}

// RunBash executes cmd in projectRoot with a timeout.
// Returns combined stdout+stderr (capped at maxBashOutput chars) and exit code.
// Only commands matching allowedPrefixes are permitted.
func (t *AgentToolkit) RunBash(cmd string, timeoutSec int) (output string, exitCode int) {
	if !isAllowedCmd(cmd) {
		return fmt.Sprintf("error: command not in allowlist: %q", cmd), 1
	}
	if timeoutSec <= 0 {
		timeoutSec = defaultTimeout
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutSec)*time.Second)
	defer cancel()

	c := exec.CommandContext(ctx, "sh", "-c", cmd)
	c.Dir = t.projectRoot
	var buf bytes.Buffer
	c.Stdout = &buf
	c.Stderr = &buf

	err := c.Run()
	exitCode = 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = 1
		}
	}

	out := buf.String()
	if len(out) > maxBashOutput {
		out = out[:maxBashOutput] + "\n[output truncated]"
	}
	return out, exitCode
}

// SearchCodebase performs hybrid semantic + BM25 search over the codebase.
func (t *AgentToolkit) SearchCodebase(ctx context.Context, query string, limit int) ([]embeddings.Chunk, error) {
	if t.embStore == nil {
		return nil, fmt.Errorf("embeddings store not available")
	}
	if limit <= 0 {
		limit = 5
	}
	return t.embStore.SearchHybrid(ctx, query, limit)
}

// FindSymbol looks up a symbol by name in the dependency graph.
func (t *AgentToolkit) FindSymbol(name string) ([]*graph.Node, error) {
	if t.querier == nil {
		return nil, fmt.Errorf("graph querier not available")
	}
	return t.querier.FindNodeByName(name)
}

// ── Ollama tool definitions ───────────────────────────────────────────────────

// Tools returns the Ollama tool definitions for this toolkit.
// Send these to ChatWithTools so the model knows what it can call.
func (t *AgentToolkit) Tools() []ollama.Tool {
	return []ollama.Tool{
		{
			Type: "function",
			Function: ollama.ToolFunction{
				Name:        "read_file",
				Description: "Read lines from a file in the project. Use start_line/end_line to stay within token budget.",
				Parameters: rawJSON(`{
					"type": "object",
					"properties": {
						"path":       {"type":"string","description":"Relative file path from project root"},
						"start_line": {"type":"integer","description":"First line to read (1-based). 0 = from start."},
						"end_line":   {"type":"integer","description":"Last line to read (inclusive). 0 = to EOF."}
					},
					"required": ["path"]
				}`),
			},
		},
		{
			Type: "function",
			Function: ollama.ToolFunction{
				Name:        "write_file",
				Description: "Create or overwrite a file with the given content.",
				Parameters: rawJSON(`{
					"type": "object",
					"properties": {
						"path":    {"type":"string","description":"Relative file path from project root"},
						"content": {"type":"string","description":"Complete file content to write"}
					},
					"required": ["path","content"]
				}`),
			},
		},
		{
			Type: "function",
			Function: ollama.ToolFunction{
				Name:        "run_bash",
				Description: "Run a shell command in the project root. Allowed: go build/test/vet/fmt, npm run/test, cargo check/build/test, python -m pytest, git diff/status/log.",
				Parameters: rawJSON(`{
					"type": "object",
					"properties": {
						"command":     {"type":"string","description":"Shell command to execute"},
						"timeout_sec": {"type":"integer","description":"Timeout in seconds (default 30, max 120)"}
					},
					"required": ["command"]
				}`),
			},
		},
		{
			Type: "function",
			Function: ollama.ToolFunction{
				Name:        "search_codebase",
				Description: "Semantic + keyword search over the codebase. Returns relevant file snippets.",
				Parameters: rawJSON(`{
					"type": "object",
					"properties": {
						"query": {"type":"string","description":"Natural language or keyword search query"},
						"limit": {"type":"integer","description":"Max results to return (default 5)"}
					},
					"required": ["query"]
				}`),
			},
		},
		{
			Type: "function",
			Function: ollama.ToolFunction{
				Name:        "finish",
				Description: "Signal that the task is complete. Provide a brief summary of what was done.",
				Parameters: rawJSON(`{
					"type": "object",
					"properties": {
						"summary": {"type":"string","description":"Brief summary of completed work"}
					},
					"required": ["summary"]
				}`),
			},
		},
	}
}

// Dispatch executes a tool call by name and returns its text output.
// Returns ErrFinished (wrapped in *FinishedError) when the agent calls "finish".
func (t *AgentToolkit) Dispatch(ctx context.Context, toolName string, argsRaw json.RawMessage) (string, error) {
	switch toolName {
	case "read_file":
		var args struct {
			Path      string `json:"path"`
			StartLine int    `json:"start_line"`
			EndLine   int    `json:"end_line"`
		}
		if err := json.Unmarshal(argsRaw, &args); err != nil {
			return "", fmt.Errorf("bad args: %w", err)
		}
		return t.ReadFile(args.Path, args.StartLine, args.EndLine)

	case "write_file":
		var args struct {
			Path    string `json:"path"`
			Content string `json:"content"`
		}
		if err := json.Unmarshal(argsRaw, &args); err != nil {
			return "", fmt.Errorf("bad args: %w", err)
		}
		if err := t.WriteFile(args.Path, args.Content); err != nil {
			return "", err
		}
		return fmt.Sprintf("wrote %s", args.Path), nil

	case "run_bash":
		var args struct {
			Command    string `json:"command"`
			TimeoutSec int    `json:"timeout_sec"`
		}
		if err := json.Unmarshal(argsRaw, &args); err != nil {
			return "", fmt.Errorf("bad args: %w", err)
		}
		out, code := t.RunBash(args.Command, args.TimeoutSec)
		if code != 0 {
			return fmt.Sprintf("exit %d\n%s", code, out), nil
		}
		return out, nil

	case "search_codebase":
		var args struct {
			Query string `json:"query"`
			Limit int    `json:"limit"`
		}
		if err := json.Unmarshal(argsRaw, &args); err != nil {
			return "", fmt.Errorf("bad args: %w", err)
		}
		chunks, err := t.SearchCodebase(ctx, args.Query, args.Limit)
		if err != nil {
			return "", err
		}
		if len(chunks) == 0 {
			return "no results found", nil
		}
		var sb strings.Builder
		for i, c := range chunks {
			fmt.Fprintf(&sb, "[%d] source=%s section=%s score=%.3f\n%s\n\n",
				i+1, c.Source, c.SectionLabel, c.Score, c.Text)
		}
		return sb.String(), nil

	case "finish":
		var args struct {
			Summary string `json:"summary"`
		}
		if err := json.Unmarshal(argsRaw, &args); err != nil {
			return "", fmt.Errorf("bad args: %w", err)
		}
		return args.Summary, &FinishedError{Summary: args.Summary}

	default:
		return "", fmt.Errorf("unknown tool: %s", toolName)
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

// safePath resolves a relative path against projectRoot, rejecting traversals.
func (t *AgentToolkit) safePath(rel string) (string, error) {
	abs := filepath.Join(t.projectRoot, filepath.Clean(rel))
	if !strings.HasPrefix(abs, t.projectRoot+string(filepath.Separator)) &&
		abs != t.projectRoot {
		return "", fmt.Errorf("path %q escapes project root", rel)
	}
	return abs, nil
}

// isAllowedCmd returns true if cmd starts with any of the allowed prefixes.
func isAllowedCmd(cmd string) bool {
	trimmed := strings.TrimSpace(cmd)
	for _, prefix := range allowedPrefixes {
		if strings.HasPrefix(trimmed, prefix) {
			return true
		}
	}
	return false
}

// rawJSON converts a JSON string literal to json.RawMessage.
func rawJSON(s string) json.RawMessage {
	// Compact to remove whitespace that might confuse some parsers.
	var buf bytes.Buffer
	if err := json.Compact(&buf, []byte(s)); err != nil {
		return json.RawMessage(s)
	}
	return buf.Bytes()
}
