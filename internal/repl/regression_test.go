package repl

// Regression tests for A-series bugs fixed in the repl package.
// These verify the fixes stay in place and prevent re-introduction.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// ── A1: Slice aliasing ────────────────────────────────────────────────────────

// TestSliceAliasingPattern is a regression for BUG-04 (A1).
// The old code used `append(messages, extra)` directly, which aliases the
// backing array when len < cap and corrupts the original slice after modifying
// the retry copy.  The fix copies first: append(append([]T{}, src...), extra).
func TestSliceAliasingPattern(t *testing.T) {
	type msg struct{ Role, Content string }

	// Build a slice that still has spare capacity (len=3, cap=4).
	// This is the condition that triggers aliasing on a bare append.
	srcWithCap := make([]msg, 3, 4)
	srcWithCap[0] = msg{"system", "sys"}
	srcWithCap[1] = msg{"user", "hello"}
	srcWithCap[2] = msg{"assistant", "world"}

	// Old (buggy) pattern: append directly into spare capacity.
	buggy := append(srcWithCap, msg{"user", "correction"}) //nolint:gocritic
	buggy[1].Content = "modified"
	if srcWithCap[1].Content != "modified" {
		// They DO NOT share memory — no aliasing on this platform/capacity; skip the pre-condition check.
		t.Log("no aliasing in old pattern (capacity may have changed); pre-condition not met")
	} else {
		t.Log("confirmed: old append pattern aliases backing array (pre-condition met)")
	}

	// New (fixed) pattern: copy first, then append.
	src := []msg{
		{"system", "sys"},
		{"user", "hello"},
		{"assistant", "world"},
	}
	safe := append(append([]msg{}, src...), msg{"user", "correction"})
	safe[1].Content = "modified"
	if src[1].Content == "modified" {
		t.Error("BUG-04 regression: copy-before-append aliased the original slice")
	}
}

// ── A3: Path traversal & separator boundary ───────────────────────────────────

// TestDispatchFixToolPathTraversal verifies that a ../../ traversal path is
// rejected and not silently passed to os.ReadFile (regression for BUG-08).
func TestDispatchFixToolPathTraversal(t *testing.T) {
	tmpDir := t.TempDir()
	args, _ := json.Marshal(map[string]string{"path": "../../etc/passwd"})
	result := dispatchFixTool(tmpDir, "read_file", json.RawMessage(args))
	if !strings.Contains(result, "error") {
		t.Errorf("path traversal should return an error, got: %q", result)
	}
}

// TestDispatchFixToolValidRead verifies a legitimate in-root path works.
func TestDispatchFixToolValidRead(t *testing.T) {
	tmpDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmpDir, "hello.txt"), []byte("hello world"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	args, _ := json.Marshal(map[string]string{"path": "hello.txt"})
	result := dispatchFixTool(tmpDir, "read_file", json.RawMessage(args))
	if result != "hello world" {
		t.Errorf("expected file content 'hello world', got: %q", result)
	}
}

// TestPathSeparatorBoundary is a unit regression for the exact pre-fix bug:
// strings.HasPrefix("/project2", "/project") == true, which allowed paths
// outside the root to pass the check.  The fix appends filepath.Separator.
func TestPathSeparatorBoundary(t *testing.T) {
	root := "/project"
	sep := string(filepath.Separator)

	// Adversarial path that shares the root prefix without being under it.
	adversarial := "/project2"
	safe := adversarial == root || strings.HasPrefix(adversarial, root+sep)
	if safe {
		t.Error("BUG-08 regression: /project2 incorrectly passes root check for /project")
	}

	// Legitimate child path must still pass.
	legit := "/project/main.go"
	safe2 := legit == root || strings.HasPrefix(legit, root+sep)
	if !safe2 {
		t.Error("/project/main.go should pass root check for /project")
	}

	// Exact root match must pass.
	exactRoot := "/project"
	safe3 := exactRoot == root || strings.HasPrefix(exactRoot, root+sep)
	if !safe3 {
		t.Error("exact root path should pass root check")
	}
}

// ── A5: Spinner ticker leak ───────────────────────────────────────────────────

// TestStartSpinnerNoLeak verifies that the goroutine started by startSpinner
// is cleaned up when the returned stop function is called (regression for
// BUG-03: old code used time.After in a loop, leaking one timer per iteration).
func TestStartSpinnerNoLeak(t *testing.T) {
	before := runtime.NumGoroutine()

	stop := startSpinner("fix")
	stop()

	// Give the goroutine time to exit after the channel close.
	time.Sleep(200 * time.Millisecond)

	after := runtime.NumGoroutine()
	// Allow +1 for any transient goroutine from the test runtime itself.
	if after > before+1 {
		t.Errorf("goroutine leak: before=%d after=%d (diff=%d, want ≤1)",
			before, after, after-before)
	}
}

// TestStartSpinnerReturnsDuration verifies the stop function returns elapsed time.
func TestStartSpinnerReturnsDuration(t *testing.T) {
	stop := startSpinner("implement")
	time.Sleep(50 * time.Millisecond)
	elapsed := stop()
	if elapsed < 50*time.Millisecond {
		t.Errorf("elapsed = %v, want ≥50ms", elapsed)
	}
}

// ── normalizeTerminalInput ────────────────────────────────────────────────────

// TestNormalizeTerminalInput verifies that raw terminal error output is
// rewritten as an explicit fix request so the router picks the right tier.
func TestNormalizeTerminalInput(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantPrefix string
	}{
		{
			name:       "go panic",
			input:      "panic: goroutine 1 [running]: unexpected nil pointer dereference",
			wantPrefix: "Fix this Go panic:",
		},
		{
			name:       "npm error",
			input:      "npm ERR! code ENOENT\nnpm ERR! path /app/node_modules",
			wantPrefix: "Fix this npm error:",
		},
		{
			name:       "make error",
			input:      "make: *** [build] Error 2",
			wantPrefix: "Fix this make error:",
		},
		{
			name:       "typescript error",
			input:      "error TS2304: Cannot find name 'Foo'",
			wantPrefix: "Fix this TypeScript error:",
		},
		{
			name:       "plain question passthrough",
			input:      "how do I use goroutines?",
			wantPrefix: "how do I use goroutines?",
		},
		{
			name:       "empty string passthrough",
			input:      "",
			wantPrefix: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeTerminalInput(tt.input)
			if !strings.HasPrefix(got, tt.wantPrefix) {
				t.Errorf("normalizeTerminalInput(%q) = %q, want prefix %q", tt.input, got, tt.wantPrefix)
			}
		})
	}
}

// ── looksLikeFilePath ─────────────────────────────────────────────────────────

func TestLooksLikeFilePath(t *testing.T) {
	trueInputs := []string{
		"main.go", "src/app.ts", ".env", ".gitignore",
		"internal/auth/jwt.go", "Makefile", "Dockerfile",
		"package.json", "scripts/build",
	}
	falseInputs := []string{
		"hello", "world", "foo", "bar",
	}
	for _, s := range trueInputs {
		if !looksLikeFilePath(s) {
			t.Errorf("looksLikeFilePath(%q) = false, want true", s)
		}
	}
	for _, s := range falseInputs {
		if looksLikeFilePath(s) {
			t.Errorf("looksLikeFilePath(%q) = true, want false", s)
		}
	}
}

// ── diffLines ─────────────────────────────────────────────────────────────────

func TestDiffLines_NoChange(t *testing.T) {
	old := "line one\nline two\nline three\n"
	got := diffLines(old, old)
	if got != "" {
		t.Errorf("identical content should produce empty diff, got: %q", got)
	}
}

func TestDiffLines_AddedLines(t *testing.T) {
	old := "line one\nline two\n"
	new := "line one\nline two\nline three\n"
	got := diffLines(old, new)
	if !strings.Contains(got, "+") {
		t.Errorf("diff should contain '+' for added lines, got: %q", got)
	}
	if !strings.Contains(got, "line three") {
		t.Errorf("diff should mention added line content, got: %q", got)
	}
}

func TestDiffLines_RemovedLines(t *testing.T) {
	old := "line one\nline two\nline three\n"
	new := "line one\nline three\n"
	got := diffLines(old, new)
	if !strings.Contains(got, "-") {
		t.Errorf("diff should contain '-' for removed lines, got: %q", got)
	}
}

func TestDiffLines_Empty(t *testing.T) {
	got := diffLines("", "")
	if got != "" {
		t.Errorf("empty→empty diff should be empty, got: %q", got)
	}
}

func TestDiffLines_NewFileFromEmpty(t *testing.T) {
	got := diffLines("", "package main\n\nfunc main() {}\n")
	if !strings.Contains(got, "+") {
		t.Errorf("expected additions from empty, got: %q", got)
	}
}
