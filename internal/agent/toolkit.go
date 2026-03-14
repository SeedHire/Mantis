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

// shellMetachars are characters that allow chaining arbitrary commands after
// an allowed prefix. Any command containing these is rejected.
var shellMetachars = []string{";", "&&", "||", "|", "$(", "`", "\n"}

// containsShellMeta returns true if cmd contains shell metacharacters that
// could be used to chain arbitrary commands after an allowed prefix.
func containsShellMeta(cmd string) bool {
	for _, mc := range shellMetachars {
		if strings.Contains(cmd, mc) {
			return true
		}
	}
	return false
}

// destructiveGitPatterns are git commands that can cause data loss.
// These are blocked even though they match allowedPrefixes.
var destructiveGitPatterns = []string{
	"git push --force", "git push -f ",
	"git reset --hard",
	"git checkout .", "git checkout -- .",
	"git restore .", "git restore --staged .",
	"git clean -f", "git clean -fd", "git clean -fdx",
	"git branch -D ",
	"git stash drop",
}

// gitWarningPatterns trigger a warning message (not a hard block).
var gitWarningPatterns = []string{
	"--no-verify",
	"--amend",
	"git add -A", "git add .",
}

// isDestructiveGit returns (blocked, warning) for a git command.
func isDestructiveGit(cmd string) (bool, string) {
	trimmed := strings.TrimSpace(cmd)
	for _, p := range destructiveGitPatterns {
		if strings.Contains(trimmed, p) {
			return true, fmt.Sprintf("error: destructive git command blocked: %q — this can cause data loss. Use a safer alternative.", cmd)
		}
	}
	for _, p := range gitWarningPatterns {
		if strings.Contains(trimmed, p) {
			switch {
			case strings.Contains(trimmed, "--no-verify"):
				return false, "warning: --no-verify skips pre-commit hooks. Fix the hook issue instead."
			case strings.Contains(trimmed, "--amend"):
				return false, "warning: --amend rewrites the previous commit. If a hook just failed, the commit didn't happen — use a NEW commit instead of amending."
			case strings.Contains(trimmed, "git add -A") || strings.Contains(trimmed, "git add ."):
				return false, "warning: 'git add -A' / 'git add .' may stage sensitive files (.env, credentials). Prefer adding specific files by name."
			}
		}
	}
	return false, ""
}

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
	projectRoot    string
	querier        *graph.Querier
	embStore       *embeddings.Store
	fileMu         sync.Mutex            // guards WriteFile against parallel worker races
	ApproveFunc    ApprovalFunc          // if set, git write ops require approval
	readTimes      map[string]time.Time  // stale-read detection: path → mtime at last read
	staleMu        sync.Mutex            // guards readTimes
	bashFailCount  int                   // 7.3: consecutive bash failure counter
	bashFailMu     sync.Mutex            // guards bashFailCount
	readFiles      map[string]bool       // 7J: tracks which files have been read (read-before-write gate)
	readFilesMu    sync.Mutex            // guards readFiles
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
		readTimes:   make(map[string]time.Time),
		readFiles:   make(map[string]bool),
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

	// Record mtime for stale-read detection on subsequent writes.
	if fi, statErr := f.Stat(); statErr == nil {
		t.staleMu.Lock()
		t.readTimes[abs] = fi.ModTime()
		t.staleMu.Unlock()
	}
	// 7J: Track that this file has been read (read-before-write gate).
	t.readFilesMu.Lock()
	t.readFiles[abs] = true
	t.readFilesMu.Unlock()

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
	// 7J: read-before-write gate — if the file already exists, must read it first.
	// New files (don't exist yet) are allowed without reading.
	if _, statErr := os.Stat(abs); statErr == nil {
		t.readFilesMu.Lock()
		hasRead := t.readFiles[abs]
		t.readFilesMu.Unlock()
		if !hasRead {
			return fmt.Errorf("write_file: file %q already exists — use read_file first to see its contents, then use edit_file for targeted changes", path)
		}
	}
	t.fileMu.Lock()
	defer t.fileMu.Unlock()
	if err := t.checkStale(abs); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(abs), err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		return err
	}
	// Update readTimes so the same worker can write again without stale-read error.
	if info, statErr := os.Stat(abs); statErr == nil {
		t.staleMu.Lock()
		t.readTimes[abs] = info.ModTime()
		t.staleMu.Unlock()
	}
	return nil
}

// serverPatterns are command prefixes that indicate long-running server processes.
// These are backgrounded automatically instead of blocking until timeout.
var serverPatterns = []string{
	"go run ", "npm start", "npm run dev", "npm run serve", "npm run watch",
	"yarn start", "yarn dev", "pnpm start", "pnpm dev",
	"python -m uvicorn", "python3 -m uvicorn",
	"python manage.py runserver", "python3 manage.py runserver",
	"python -m flask run", "python3 -m flask run",
	"cargo run", "air ", // Go hot-reload tool
}

// isServerCmd returns true when cmd looks like a long-running server process.
func isServerCmd(cmd string) bool {
	trimmed := strings.TrimSpace(cmd)
	for _, p := range serverPatterns {
		if strings.HasPrefix(trimmed, p) {
			return true
		}
	}
	return false
}

// RunBash executes cmd in projectRoot with a timeout.
// Returns combined stdout+stderr (capped at maxBashOutput chars) and exit code.
// Only commands matching allowedPrefixes are permitted.
//
// Background promotion: commands matching server patterns (npm start, go run,
// uvicorn, etc.) are automatically backgrounded instead of blocking until timeout.
func (t *AgentToolkit) RunBash(cmd string, timeoutSec int) (output string, exitCode int) {
	if !isAllowedCmd(cmd) {
		return fmt.Sprintf("error: command not in allowlist: %q", cmd), 1
	}
	// 7L: Block destructive git commands that can cause data loss.
	if blocked, msg := isDestructiveGit(cmd); blocked {
		return msg, 1
	} else if msg != "" {
		// Non-blocking warning — prepend to output so model sees it.
		defer func() { output = msg + "\n" + output }()
	}
	if timeoutSec <= 0 {
		timeoutSec = defaultTimeout
	}

	// Phase 6.10: background promotion — server commands never block the agent.
	if isServerCmd(cmd) {
		c := exec.Command("sh", "-c", cmd)
		c.Dir = t.projectRoot
		if err := c.Start(); err != nil {
			return fmt.Sprintf("error: failed to start background process: %v", err), 1
		}
		// Reap the child process in a background goroutine to prevent zombies.
		go func() { _ = c.Wait() }()
		return fmt.Sprintf("started in background (PID %d) — use 'ps' or logs to monitor", c.Process.Pid), 0
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

// SearchCodebase dispatches to one of three retrieval tiers based on searchType:
//   - "symbol"   → graph querier symbol lookup (finds structs/functions by name)
//   - "path"     → glob file search by path pattern
//   - "semantic" → hybrid BM25 + cosine embedding search (default)
//   - "auto"     → heuristically picks the best tier from the query shape
//
// Source: Cline/Roo-Code three-tier retrieval architecture.
func (t *AgentToolkit) SearchCodebase(ctx context.Context, query, searchType string, limit int) ([]embeddings.Chunk, error) {
	if limit <= 0 {
		limit = 5
	}

	// Auto-detect: symbol name (no spaces, PascalCase/snake_case) → symbol tier;
	// path pattern (contains / or *) → path tier; else → semantic.
	if searchType == "" || searchType == "auto" {
		switch {
		case strings.Contains(query, "/") || strings.Contains(query, "*"):
			searchType = "path"
		case !strings.Contains(query, " ") && len(query) > 2:
			searchType = "symbol"
		default:
			searchType = "semantic"
		}
	}

	switch searchType {
	case "symbol":
		if t.querier == nil {
			// Graceful fallback to semantic if graph unavailable.
			return t.searchSemantic(ctx, query, limit)
		}
		nodes, err := t.querier.FindNodeByName(query)
		if err != nil || len(nodes) == 0 {
			return t.searchSemantic(ctx, query, limit) // fallback
		}
		var chunks []embeddings.Chunk
		for _, n := range nodes {
			if len(chunks) >= limit {
				break
			}
			src := ""
			if data, err := os.ReadFile(n.FilePath); err == nil {
				src = string(data)
				if len(src) > 1000 {
					src = src[:1000]
				}
			}
			chunks = append(chunks, embeddings.Chunk{
				ID:           n.ID,
				Source:       n.FilePath,
				SectionLabel: string(n.Type) + ":" + n.Name,
				Text:         src,
				Score:        1.0,
			})
		}
		return chunks, nil

	case "path":
		pattern := query
		if !filepath.IsAbs(pattern) {
			// BUG-18: filepath.Glob does NOT support "**" double-star globs.
			// Use filepath.WalkDir for recursive matching instead.
			pattern = filepath.Join(t.projectRoot, pattern)
		}
		matches, err := filepath.Glob(pattern)
		if err != nil || len(matches) == 0 {
			// Glob failed or returned nothing — try a recursive walk to find files
			// matching the base pattern anywhere in the tree.
			basePat := filepath.Base(query)
			matches = walkGlob(t.projectRoot, basePat, limit)
		}
		var chunks []embeddings.Chunk
		for i, m := range matches {
			if i >= limit {
				break
			}
			data, err := os.ReadFile(m)
			if err != nil {
				continue
			}
			preview := string(data)
			if len(preview) > 600 {
				preview = preview[:600]
			}
			rel, _ := filepath.Rel(t.projectRoot, m)
			chunks = append(chunks, embeddings.Chunk{
				Source: rel,
				Text:   preview,
				Score:  1.0,
			})
		}
		return chunks, nil

	default: // "semantic"
		return t.searchSemantic(ctx, query, limit)
	}
}

// searchSemantic is the existing hybrid BM25+cosine search path.
func (t *AgentToolkit) searchSemantic(ctx context.Context, query string, limit int) ([]embeddings.Chunk, error) {
	if t.embStore == nil {
		return nil, fmt.Errorf("embeddings store not available → run 'mantis init' to enable semantic search")
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
	// 7J: read-before-write gate — must read file first to know its actual contents.
	t.readFilesMu.Lock()
	hasRead := t.readFiles[abs]
	t.readFilesMu.Unlock()
	if !hasRead {
		return fmt.Errorf("edit_file: you must read_file(%q) first before editing it — read the file to see its actual contents", path)
	}
	t.fileMu.Lock()
	defer t.fileMu.Unlock()
	if err := t.checkStale(abs); err != nil {
		return err
	}

	data, err := os.ReadFile(abs)
	if err != nil {
		return err
	}
	content := string(data)
	count := strings.Count(content, oldString)
	if count == 0 {
		return fmt.Errorf("edit_file: old_string not found in %s → re-read the file first to confirm the exact text", path)
	}
	if count > 1 {
		return fmt.Errorf("edit_file: old_string matches %d times in %s — be more specific", count, path)
	}
	if err := os.WriteFile(abs, []byte(strings.Replace(content, oldString, newString, 1)), 0o644); err != nil {
		return err
	}
	// Update readTimes so the same worker can write again without stale-read error.
	if info, statErr := os.Stat(abs); statErr == nil {
		t.staleMu.Lock()
		t.readTimes[abs] = info.ModTime()
		t.staleMu.Unlock()
	}
	return nil
}

// RunTests detects the project's test runner and executes tests, returning
// structured failure output instead of raw stdout.
func (t *AgentToolkit) RunTests(packages string, timeoutSec int) (string, int) {
	runner, cmd := DetectTestRunner(t.projectRoot)
	if runner == RunnerUnknown {
		return "error: could not detect test runner (no go.mod, package.json, Cargo.toml, or pyproject.toml)", 1
	}

	// Scope to specific packages if requested.
	// Sanitize packages to prevent command injection via shell metacharacters.
	testCmd := cmd
	if packages != "" {
		if containsShellMeta(packages) {
			return "error: packages parameter contains unsafe characters", 1
		}
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
		return nil, fmt.Errorf("graph not available → run 'mantis init' in the project root to build the dependency graph")
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

// checkStale returns an error if the file at abs has been modified since the
// agent last read it. This prevents multi-agent data loss (Worker A reads at
// t=0, Worker B writes at t=1, Worker A overwrites at t=2 — now caught).
// Files never read by this toolkit are always allowed through.
func (t *AgentToolkit) checkStale(abs string) error {
	t.staleMu.Lock()
	readAt, ok := t.readTimes[abs]
	t.staleMu.Unlock()
	if !ok {
		return nil // never read by this toolkit — allow write
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return nil // file doesn't exist yet — allow create
	}
	if fi.ModTime().After(readAt) {
		return fmt.Errorf("stale-read: %s was modified since last read (read at %s, mtime %s) — re-read before writing",
			filepath.Base(abs), readAt.Format(time.RFC3339), fi.ModTime().Format(time.RFC3339))
	}
	return nil
}

// ── 7.3: ACI guardrails ───────────────────────────────────────────────────────

// lintBeforeWrite runs a quick syntax check on content based on file extension.
// Returns a user-friendly error message if syntax is invalid, empty string if OK.
// Source: SWE-agent ACI pattern — catch syntax errors before disk write.
func lintBeforeWrite(path, content string) string {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".go":
		return lintGo(path, content)
	case ".json":
		return lintJSON(path, content)
	default:
		return "" // no linter available — allow write
	}
}

// lintGo shells out to `gofmt` to check Go syntax without modifying the file.
func lintGo(path, content string) string {
	cmd := exec.Command("gofmt", "-e")
	cmd.Stdin = strings.NewReader(content)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = nil // discard formatted output
	if err := cmd.Run(); err != nil {
		msg := stderr.String()
		if msg == "" {
			msg = err.Error()
		}
		// Extract first error line for the model.
		lines := strings.SplitN(msg, "\n", 4)
		preview := strings.Join(lines, "\n")
		return fmt.Sprintf("SYNTAX_ERROR in %s:\n%s\nFix the syntax error before writing.", path, preview)
	}
	return ""
}

// lintJSON validates JSON syntax.
func lintJSON(path, content string) string {
	if !json.Valid([]byte(content)) {
		return fmt.Sprintf("SYNTAX_ERROR in %s: invalid JSON — check for missing commas, brackets, or trailing commas.", path)
	}
	return ""
}

// structuredBashError wraps raw bash stderr with a structured hint.
// Source: SWE-agent ACI — structured error feedback reduces compounding errors.
func structuredBashError(cmd string, exitCode int, output string) string {
	var hint string
	lower := strings.ToLower(output)

	switch {
	case strings.Contains(lower, "undefined:") || strings.Contains(lower, "undeclared name"):
		hint = "HINT: undefined symbol — check for missing import or typo in variable/function name"
	case strings.Contains(lower, "cannot find module") || strings.Contains(lower, "module not found"):
		hint = "HINT: missing module — run 'go mod tidy' or check the import path"
	case strings.Contains(lower, "syntax error") || strings.Contains(lower, "expected"):
		hint = "HINT: syntax error — check for missing brackets, semicolons, or mismatched delimiters"
	case strings.Contains(lower, "permission denied"):
		hint = "HINT: permission denied — check file permissions or try a different path"
	case strings.Contains(lower, "no such file or directory"):
		hint = "HINT: file not found — verify the path exists (use read_file or search_codebase to confirm)"
	case strings.Contains(lower, "connection refused") || strings.Contains(lower, "timeout"):
		hint = "HINT: network error — the service may not be running or the port may be wrong"
	case strings.Contains(lower, "already exists"):
		hint = "HINT: resource already exists — check if you need to update instead of create"
	case strings.Contains(lower, "type mismatch") || strings.Contains(lower, "cannot use"):
		hint = "HINT: type error — check that argument types match the function signature"
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "COMMAND_FAILED: exit %d\n", exitCode)
	if hint != "" {
		sb.WriteString(hint)
		sb.WriteString("\n")
	}
	sb.WriteString("OUTPUT:\n")
	sb.WriteString(output)
	return sb.String()
}

// bashFailureGuard checks consecutive bash failures and returns a stop prompt
// if the threshold is reached. Returns empty string if under threshold.
func (t *AgentToolkit) bashFailureGuard(failed bool) string {
	t.bashFailMu.Lock()
	defer t.bashFailMu.Unlock()
	if failed {
		t.bashFailCount++
		if t.bashFailCount >= 3 {
			t.bashFailCount = 0 // reset so model gets another chance
			return "STOP: 3 consecutive bash commands have failed. Before taking another action, explain what you are trying to accomplish and what went wrong. Then try a different approach."
		}
	} else {
		t.bashFailCount = 0
	}
	return ""
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
				Description: "Search the codebase using one of three tiers: symbol (find structs/functions by name), path (glob file search), semantic (natural language). Auto-detects the best tier when search_type is omitted.",
				Parameters: rawJSON(`{
					"type": "object",
					"properties": {
						"query":       {"type":"string","description":"Search query: symbol name, path glob, or natural language"},
						"search_type": {"type":"string","enum":["auto","semantic","symbol","path"],"description":"Search tier. auto (default) picks based on query shape."},
						"limit":       {"type":"integer","description":"Max results to return (default 5)"}
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

// ReadOnlyTools returns a filtered subset of Tools() that excludes all write
// operations. Used by the orchestrator's decompose phase so the planning model
// is physically unable to write files — schema-level enforcement, not runtime.
func (t *AgentToolkit) ReadOnlyTools() []ollama.Tool {
	writeNames := map[string]bool{
		"write_file": true,
		"edit_file":  true,
		"git_stage":  true,
		"git_commit": true,
		"git_branch": true,
	}
	var readOnly []ollama.Tool
	for _, tool := range t.Tools() {
		if !writeNames[tool.Function.Name] {
			readOnly = append(readOnly, tool)
		}
	}
	return readOnly
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
		// 7.3: Lint before write — catch syntax errors before disk write.
		if lintErr := lintBeforeWrite(args.Path, args.Content); lintErr != "" {
			return lintErr, nil // return as tool output, not error — model can fix and retry
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
			// 7.3: Structured error hints + consecutive failure tracking.
			structured := structuredBashError(args.Command, code, out)
			if guard := t.bashFailureGuard(true); guard != "" {
				structured += "\n\n" + guard
			}
			return structured, nil
		}
		t.bashFailureGuard(false) // reset on success
		return out, nil

	case "search_codebase":
		var args struct {
			Query      string `json:"query"`
			SearchType string `json:"search_type"`
			Limit      int    `json:"limit"`
		}
		if err := json.Unmarshal(argsRaw, &args); err != nil {
			return "", fmt.Errorf("bad args: %w", err)
		}
		chunks, err := t.SearchCodebase(ctx, args.Query, args.SearchType, args.Limit)
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
		// 7.3: Lint after edit — verify the resulting file is syntactically valid.
		if abs, pathErr := t.safePath(args.Path); pathErr == nil {
			if data, readErr := os.ReadFile(abs); readErr == nil {
				if lintErr := lintBeforeWrite(args.Path, string(data)); lintErr != "" {
					return fmt.Sprintf("edited %s — WARNING: %s", args.Path, lintErr), nil
				}
			}
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
	// Reject commands containing shell metacharacters that allow chaining
	// arbitrary commands after an allowed prefix (e.g. "git diff; rm -rf /").
	if containsShellMeta(trimmed) {
		return false
	}
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

// walkGlob recursively walks root and returns up to maxResults files whose
// base name matches the given glob pattern. This replaces the broken
// filepath.Glob("**/pattern") which Go does not support (BUG-18).
func walkGlob(root, pattern string, maxResults int) []string {
	var results []string
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable dirs
		}
		if d.IsDir() {
			base := d.Name()
			if base == ".git" || base == "node_modules" || base == "vendor" || base == ".mantis" {
				return filepath.SkipDir
			}
			return nil
		}
		matched, _ := filepath.Match(pattern, d.Name())
		if matched {
			results = append(results, path)
			if len(results) >= maxResults {
				return filepath.SkipAll
			}
		}
		return nil
	})
	return results
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
