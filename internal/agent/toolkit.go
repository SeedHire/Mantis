// Package agent implements the AgentToolkit (the 5 core ACI tools) and the
// multi-agent orchestrator (fan-out → parallel workers → synthesizer).
//
// Design follows SWE-agent's Agent-Computer Interface paper:
//  1. read_file    — partial file view, stays within token budget
//  2. write_file   — create or overwrite a file
//  3. run_bash     — shell with allowlist, structured output, output cap
//  4. search_codebase — semantic search over graph + embeddings
//  5. finish       — explicit done signal, prevents runaway loops
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
	maxBashOutput  = 8000 // characters (~2000 tokens)
	defaultTimeout = 30   // seconds
)

// ErrFinished is returned by Dispatch when the agent calls the "finish" tool.
// The summary is stored in the FinishedError value.
var ErrFinished = errors.New("agent finished")

// FinishedError wraps ErrFinished with the agent's summary message.
type FinishedError struct{ Summary string }

func (e *FinishedError) Error() string   { return "agent finished: " + e.Summary }
func (e *FinishedError) Unwrap() error   { return ErrFinished }
func (e *FinishedError) Is(t error) bool { return t == ErrFinished }

// allowedPrefixes is the bash command allowlist (prefix matching).
// Covers build tools, package managers, Docker, Make, diagnostics, and VCS.
var allowedPrefixes = []string{
	// Go
	"go build", "go test", "go vet", "go fmt", "go run", "go mod",
	// Node
	"npm run", "npm test", "npm install", "npm ci", "npm start",
	"npx ", "yarn ", "pnpm ",
	// Rust
	"cargo check", "cargo build", "cargo test", "cargo run",
	// Python
	"python -m", "python3 -m", "python ", "python3 ",
	"pip install", "pip3 install", "pip list", "pip3 list",
	// Docker
	"docker build", "docker compose", "docker-compose",
	"docker run", "docker ps", "docker logs", "docker images",
	"docker exec", "docker stop", "docker rm", "docker inspect",
	// Make
	"make",
	// Kubernetes
	"kubectl ",
	// Git (read)
	"git diff", "git status", "git log", "git show",
	// Git (write — gated by approval in dedicated tools)
	"git add", "git commit", "git checkout -b", "git reset HEAD",
	// Shell diagnostics
	"cat ", "head ", "tail ", "ls ", "find ", "grep ",
	"pwd", "which ", "echo ", "wc ", "env",
}

// ApprovalFunc asks the user for confirmation. Returns true if approved.
type ApprovalFunc func(prompt string) bool

// AgentToolkit provides typed tool access for coding agents.
// All file and bash operations are scoped to projectRoot for safety.
type AgentToolkit struct {
	projectRoot string
	querier     *graph.Querier
	embStore    *embeddings.Store
	fileMu      sync.Mutex   // guards WriteFile against parallel worker races
	ApproveFunc ApprovalFunc // if set, git write ops require approval
}

// NewToolkit creates a toolkit bound to the given project root.
// querier and embStore may be nil; their tools return graceful errors.
func NewToolkit(projectRoot string, querier *graph.Querier, embStore *embeddings.Store) *AgentToolkit {
	// Resolve symlinks on the root itself so the prefix check in safePath is
	// consistent on macOS (/var/folders → /private/var/folders).
	if resolved, err := filepath.EvalSymlinks(projectRoot); err == nil {
		projectRoot = resolved
	}
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

// EditFile applies a precise old→new replacement to an existing file.
// Fails if old_string is not found exactly once (safe by default).
// Use write_file only for new files; prefer edit_file for existing files.
func (t *AgentToolkit) EditFile(path, oldString, newString string) error {
	abs, err := t.safePath(path)
	if err != nil {
		return err
	}
	t.fileMu.Lock()
	defer t.fileMu.Unlock()

	data, err := os.ReadFile(abs)
	if err != nil {
		return err
	}
	content := string(data)
	count := strings.Count(content, oldString)
	if count == 0 {
		return fmt.Errorf("edit_file: old_string not found in %s", path)
	}
	if count > 1 {
		return fmt.Errorf("edit_file: old_string matches %d times in %s — be more specific", count, path)
	}
	return os.WriteFile(abs, []byte(strings.Replace(content, oldString, newString, 1)), 0o644)
}

// RunTests detects the project's test runner and executes tests, returning
// structured failure output instead of raw stdout.
func (t *AgentToolkit) RunTests(packages string, timeoutSec int) (string, int) {
	runner, cmd := DetectTestRunner(t.projectRoot)
	if runner == RunnerUnknown {
		return "error: could not detect test runner (no go.mod, package.json, Cargo.toml, or pyproject.toml)", 1
	}

	// Scope to specific packages if requested.
	testCmd := cmd
	if packages != "" {
		if runner == RunnerGo {
			testCmd = "go test " + packages
		} else {
			testCmd = cmd + " " + packages
		}
	}

	if timeoutSec <= 0 {
		timeoutSec = 120
	}

	output, exitCode := t.RunBash(testCmd, timeoutSec)

	// If tests failed, parse and format structured output.
	if exitCode != 0 {
		failures := ParseTestOutput(runner, output)
		if len(failures) > 0 {
			var sb strings.Builder
			fmt.Fprintf(&sb, "exit %d — %d test failure(s):\n\n", exitCode, len(failures))
			for i, f := range failures {
				fmt.Fprintf(&sb, "%d. %s\n", i+1, f.String())
			}
			sb.WriteString("\n--- Raw output (last 4000 chars) ---\n")
			if len(output) > 4000 {
				output = output[len(output)-4000:]
			}
			sb.WriteString(output)
			return sb.String(), exitCode
		}
	}

	return output, exitCode
}

// FindSymbol looks up a symbol by name in the dependency graph.
func (t *AgentToolkit) FindSymbol(name string) ([]*graph.Node, error) {
	if t.querier == nil {
		return nil, fmt.Errorf("graph querier not available")
	}
	return t.querier.FindNodeByName(name)
}

// ── Git write tools ───────────────────────────────────────────────────────────

// GitStage stages files for commit. Requires approval.
func (t *AgentToolkit) GitStage(paths []string) (string, error) {
	if len(paths) == 0 {
		return "", fmt.Errorf("no paths specified")
	}
	desc := fmt.Sprintf("stage %d file(s): %s", len(paths), strings.Join(paths, ", "))
	if !t.approve(desc) {
		return "denied by user", nil
	}
	args := append([]string{"add"}, paths...)
	out, code := t.runGit(args...)
	if code != 0 {
		return out, fmt.Errorf("git add failed (exit %d): %s", code, out)
	}
	return fmt.Sprintf("staged %d file(s)", len(paths)), nil
}

// GitCommit creates a commit with the given message. Requires approval.
func (t *AgentToolkit) GitCommit(message string) (string, error) {
	if message == "" {
		return "", fmt.Errorf("empty commit message")
	}
	// Show what will be committed.
	staged, _ := t.runGit("diff", "--cached", "--stat")
	desc := fmt.Sprintf("commit with message: %q\n  staged:\n%s", message, staged)
	if !t.approve(desc) {
		return "denied by user", nil
	}
	out, code := t.runGit("commit", "-m", message)
	if code != 0 {
		return out, fmt.Errorf("git commit failed (exit %d): %s", code, out)
	}
	return out, nil
}

// GitBranch creates and switches to a new branch. Requires approval.
func (t *AgentToolkit) GitBranch(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("empty branch name")
	}
	desc := fmt.Sprintf("create branch: %s", name)
	if !t.approve(desc) {
		return "denied by user", nil
	}
	out, code := t.runGit("checkout", "-b", name)
	if code != 0 {
		return out, fmt.Errorf("git checkout -b failed (exit %d): %s", code, out)
	}
	return out, nil
}

// GitUnstage unstages files. Requires approval.
func (t *AgentToolkit) GitUnstage(paths []string) (string, error) {
	if len(paths) == 0 {
		return "", fmt.Errorf("no paths specified")
	}
	desc := fmt.Sprintf("unstage %d file(s): %s", len(paths), strings.Join(paths, ", "))
	if !t.approve(desc) {
		return "denied by user", nil
	}
	args := append([]string{"reset", "HEAD"}, paths...)
	out, code := t.runGit(args...)
	if code != 0 {
		return out, fmt.Errorf("git reset HEAD failed (exit %d): %s", code, out)
	}
	return fmt.Sprintf("unstaged %d file(s)", len(paths)), nil
}

// GitDiff returns the current diff (staged + unstaged).
func (t *AgentToolkit) GitDiff() string {
	staged, _ := t.runGit("diff", "--cached")
	unstaged, _ := t.runGit("diff")
	var sb strings.Builder
	if staged != "" {
		sb.WriteString("=== Staged changes ===\n")
		sb.WriteString(staged)
		sb.WriteString("\n")
	}
	if unstaged != "" {
		sb.WriteString("=== Unstaged changes ===\n")
		sb.WriteString(unstaged)
	}
	return sb.String()
}

func (t *AgentToolkit) approve(desc string) bool {
	if t.ApproveFunc == nil {
		return false // deny by default — caller must set ApproveFunc to enable git writes
	}
	return t.ApproveFunc(desc)
}

func (t *AgentToolkit) runGit(args ...string) (string, int) {
	cmd := exec.Command("git", args...)
	cmd.Dir = t.projectRoot
	out, err := cmd.CombinedOutput()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return strings.TrimSpace(string(out)), exitErr.ExitCode()
		}
		return err.Error(), 1
	}
	return strings.TrimSpace(string(out)), 0
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
				Description: "Run a shell command in the project root. Allowed: go, npm, yarn, pnpm, cargo, make, docker, docker compose, pip, python, kubectl, git, cat, head, tail, ls, find, grep, pwd, which, echo.",
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
				Name:        "edit_file",
				Description: "Apply a precise old→new replacement to an existing file. Fails if old_string is not found exactly once. Use this instead of write_file when modifying existing files.",
				Parameters: rawJSON(`{
					"type": "object",
					"properties": {
						"path":       {"type":"string","description":"Relative file path from project root"},
						"old_string": {"type":"string","description":"Exact text to replace (must appear exactly once)"},
						"new_string": {"type":"string","description":"Replacement text"}
					},
					"required": ["path","old_string","new_string"]
				}`),
			},
		},
		{
			Type: "function",
			Function: ollama.ToolFunction{
				Name:        "run_tests",
				Description: "Run the project's test suite and return structured failure output. Auto-detects test runner (go test, npm test, pytest, cargo test).",
				Parameters: rawJSON(`{
					"type": "object",
					"properties": {
						"packages":    {"type":"string","description":"Optional: specific package/path to test (e.g. './internal/router/...'). Empty = run all tests."},
						"timeout_sec": {"type":"integer","description":"Timeout in seconds (default 120)"}
					}
				}`),
			},
		},
		{
			Type: "function",
			Function: ollama.ToolFunction{
				Name:        "git_stage",
				Description: "Stage files for the next git commit.",
				Parameters: rawJSON(`{
					"type": "object",
					"properties": {
						"paths": {"type":"array","items":{"type":"string"},"description":"File paths to stage (relative to project root)"}
					},
					"required": ["paths"]
				}`),
			},
		},
		{
			Type: "function",
			Function: ollama.ToolFunction{
				Name:        "git_commit",
				Description: "Create a git commit with the staged changes.",
				Parameters: rawJSON(`{
					"type": "object",
					"properties": {
						"message": {"type":"string","description":"Commit message"}
					},
					"required": ["message"]
				}`),
			},
		},
		{
			Type: "function",
			Function: ollama.ToolFunction{
				Name:        "git_branch",
				Description: "Create and switch to a new git branch.",
				Parameters: rawJSON(`{
					"type": "object",
					"properties": {
						"name": {"type":"string","description":"Branch name to create"}
					},
					"required": ["name"]
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

	case "edit_file":
		var args struct {
			Path      string `json:"path"`
			OldString string `json:"old_string"`
			NewString string `json:"new_string"`
		}
		if err := json.Unmarshal(argsRaw, &args); err != nil {
			return "", fmt.Errorf("bad args: %w", err)
		}
		if err := t.EditFile(args.Path, args.OldString, args.NewString); err != nil {
			return "", err
		}
		return fmt.Sprintf("edited %s", args.Path), nil

	case "run_tests":
		var args struct {
			Packages   string `json:"packages"`
			TimeoutSec int    `json:"timeout_sec"`
		}
		if err := json.Unmarshal(argsRaw, &args); err != nil {
			return "", fmt.Errorf("bad args: %w", err)
		}
		out, code := t.RunTests(args.Packages, args.TimeoutSec)
		if code != 0 {
			return fmt.Sprintf("exit %d\n%s", code, out), nil
		}
		return out, nil

	case "git_stage":
		var args struct {
			Paths []string `json:"paths"`
		}
		if err := json.Unmarshal(argsRaw, &args); err != nil {
			return "", fmt.Errorf("bad args: %w", err)
		}
		return t.GitStage(args.Paths)

	case "git_commit":
		var args struct {
			Message string `json:"message"`
		}
		if err := json.Unmarshal(argsRaw, &args); err != nil {
			return "", fmt.Errorf("bad args: %w", err)
		}
		return t.GitCommit(args.Message)

	case "git_branch":
		var args struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(argsRaw, &args); err != nil {
			return "", fmt.Errorf("bad args: %w", err)
		}
		return t.GitBranch(args.Name)

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
// BUG-08: also resolves symlinks so a symlink pointing outside the root is blocked.
func (t *AgentToolkit) safePath(rel string) (string, error) {
	abs := filepath.Join(t.projectRoot, filepath.Clean(rel))
	if abs != t.projectRoot && !strings.HasPrefix(abs, t.projectRoot+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes project root", rel)
	}
	// Resolve symlinks to catch traversal via symlink indirection.
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		// File doesn't exist yet (write case) — just return the cleaned abs path.
		return abs, nil
	}
	if resolved != t.projectRoot && !strings.HasPrefix(resolved, t.projectRoot+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q resolves outside project root via symlink", rel)
	}
	return resolved, nil
}

// isAllowedCmd returns true if cmd starts with any of the allowed prefixes.
func isAllowedCmd(cmd string) bool {
	trimmed := strings.TrimSpace(cmd)
	for _, prefix := range allowedPrefixes {
		if strings.HasPrefix(trimmed, prefix) {
			// Block shell diagnostic commands targeting sensitive system paths
			if blockSensitivePath(trimmed) {
				return false
			}
			return true
		}
	}
	return false
}

// blockSensitivePath returns true if the command accesses files outside the
// project (e.g. /etc/passwd). It applies only to file-reading diagnostics.
// BUG-09: "less " removed — it is not in allowedPrefixes so isAllowedCmd
// already rejects it before blockSensitivePath is ever reached.
func blockSensitivePath(cmd string) bool {
	for _, diag := range []string{"cat ", "head ", "tail "} {
		if strings.HasPrefix(cmd, diag) {
			rest := strings.TrimSpace(cmd[len(diag):])
			if strings.HasPrefix(rest, "/etc") || strings.HasPrefix(rest, "/proc") ||
				strings.HasPrefix(rest, "/sys") || strings.HasPrefix(rest, "/dev") ||
				strings.HasPrefix(rest, "/var/log") || strings.HasPrefix(rest, "/root") ||
				strings.Contains(rest, "..") {
				return true
			}
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
