package agent

import (
	"os"
	"path/filepath"
	"testing"
)

// ── DetectTestRunner ──────────────────────────────────────────────────────────

func TestDetectTestRunner_Go(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runner, cmd := DetectTestRunner(root)
	if runner != RunnerGo {
		t.Errorf("expected RunnerGo, got %v", runner)
	}
	if cmd != "go test ./..." {
		t.Errorf("expected 'go test ./...', got %q", cmd)
	}
}

func TestDetectTestRunner_Node(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "package.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	runner, cmd := DetectTestRunner(root)
	if runner != RunnerNode {
		t.Errorf("expected RunnerNode, got %v", runner)
	}
	if cmd != "npm test" {
		t.Errorf("expected 'npm test', got %q", cmd)
	}
}

func TestDetectTestRunner_Python(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "pyproject.toml"), []byte("[tool.pytest]"), 0o644); err != nil {
		t.Fatal(err)
	}
	runner, cmd := DetectTestRunner(root)
	if runner != RunnerPython {
		t.Errorf("expected RunnerPython, got %v", runner)
	}
	if cmd != "python -m pytest -v" {
		t.Errorf("expected 'python -m pytest -v', got %q", cmd)
	}
}

func TestDetectTestRunner_Rust(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "Cargo.toml"), []byte("[package]"), 0o644); err != nil {
		t.Fatal(err)
	}
	runner, cmd := DetectTestRunner(root)
	if runner != RunnerRust {
		t.Errorf("expected RunnerRust, got %v", runner)
	}
	if cmd != "cargo test" {
		t.Errorf("expected 'cargo test', got %q", cmd)
	}
}

func TestDetectTestRunner_Unknown(t *testing.T) {
	root := t.TempDir()
	runner, cmd := DetectTestRunner(root)
	if runner != RunnerUnknown {
		t.Errorf("expected RunnerUnknown, got %v", runner)
	}
	if cmd != "" {
		t.Errorf("expected empty cmd, got %q", cmd)
	}
}

func TestDetectTestRunner_GoPriority(t *testing.T) {
	// When both go.mod and package.json exist, Go should win (checked first).
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "go.mod"), []byte("module test\n"), 0o644)
	os.WriteFile(filepath.Join(root, "package.json"), []byte("{}"), 0o644)
	runner, _ := DetectTestRunner(root)
	if runner != RunnerGo {
		t.Errorf("expected RunnerGo when both exist, got %v", runner)
	}
}

// ── Go test parser ────────────────────────────────────────────────────────────

func TestParseGoTestOutput_SingleFailure(t *testing.T) {
	output := `=== RUN   TestRouterClassify
    router_test.go:142: expected TierCode, got TierFast
--- FAIL: TestRouterClassify (0.00s)
FAIL
`
	failures := parseGoTestOutput(output)
	if len(failures) != 1 {
		t.Fatalf("expected 1 failure, got %d", len(failures))
	}
	f := failures[0]
	if f.TestName != "TestRouterClassify" {
		t.Errorf("TestName = %q", f.TestName)
	}
	if f.File != "router_test.go" {
		t.Errorf("File = %q", f.File)
	}
	if f.Line != 142 {
		t.Errorf("Line = %d", f.Line)
	}
	if f.Message != "expected TierCode, got TierFast" {
		t.Errorf("Message = %q", f.Message)
	}
}

func TestParseGoTestOutput_MultipleFailures(t *testing.T) {
	output := `=== RUN   TestA
    a_test.go:10: wrong value
--- FAIL: TestA (0.01s)
=== RUN   TestB
    b_test.go:25: unexpected nil
--- FAIL: TestB (0.00s)
FAIL
`
	failures := parseGoTestOutput(output)
	if len(failures) != 2 {
		t.Fatalf("expected 2 failures, got %d", len(failures))
	}
	if failures[0].TestName != "TestA" {
		t.Errorf("[0] TestName = %q", failures[0].TestName)
	}
	if failures[1].TestName != "TestB" {
		t.Errorf("[1] TestName = %q", failures[1].TestName)
	}
}

func TestParseGoTestOutput_CompilationError(t *testing.T) {
	output := `# github.com/example/pkg
./handler.go:15:2: undefined: DoSomething
FAIL	github.com/example/pkg [build failed]
`
	failures := parseGoTestOutput(output)
	if len(failures) == 0 {
		t.Fatal("expected at least 1 failure for compilation error")
	}
	if failures[0].File != "./handler.go" {
		t.Errorf("File = %q", failures[0].File)
	}
	if failures[0].Line != 15 {
		t.Errorf("Line = %d", failures[0].Line)
	}
}

func TestParseGoTestOutput_NoLocation(t *testing.T) {
	output := `--- FAIL: TestSomething (0.00s)
FAIL
`
	failures := parseGoTestOutput(output)
	if len(failures) != 1 {
		t.Fatalf("expected 1 failure, got %d", len(failures))
	}
	if failures[0].TestName != "TestSomething" {
		t.Errorf("TestName = %q", failures[0].TestName)
	}
	if failures[0].File != "" {
		t.Errorf("expected empty file, got %q", failures[0].File)
	}
}

// ── Node/Jest parser ──────────────────────────────────────────────────────────

func TestParseNodeTestOutput_JestFailure(t *testing.T) {
	output := `FAIL src/utils.test.ts
  ● validateEmail › should reject invalid emails

    expect(received).toBe(expected)

    Expected: false
    Received: true

      at Object.<anonymous> (src/utils.test.ts:42:5)
`
	failures := parseNodeTestOutput(output)
	if len(failures) == 0 {
		t.Fatal("expected at least 1 failure")
	}
	f := failures[0]
	if f.TestName != "validateEmail › should reject invalid emails" {
		t.Errorf("TestName = %q", f.TestName)
	}
	if f.File != "src/utils.test.ts" {
		t.Errorf("File = %q", f.File)
	}
	if f.Line != 42 {
		t.Errorf("Line = %d", f.Line)
	}
}

func TestParseNodeTestOutput_FileOnly(t *testing.T) {
	output := `FAIL src/app.test.tsx
Test Suites: 1 failed
`
	failures := parseNodeTestOutput(output)
	if len(failures) == 0 {
		t.Fatal("expected at least 1 failure")
	}
	if failures[0].File != "src/app.test.tsx" {
		t.Errorf("File = %q", failures[0].File)
	}
}

// ── Python/pytest parser ──────────────────────────────────────────────────────

func TestParsePythonTestOutput_SingleFailure(t *testing.T) {
	output := `========================= FAILURES ==========================
___________________ TestAuth.test_login ____________________
test_auth.py:28: AssertionError
FAILED test_auth.py::TestAuth::test_login - AssertionError: expected 200, got 401
========================= short test summary info ========================
1 failed
`
	failures := parsePythonTestOutput(output)
	if len(failures) == 0 {
		t.Fatal("expected at least 1 failure")
	}
	f := failures[0]
	if f.TestName != "TestAuth::test_login" {
		t.Errorf("TestName = %q", f.TestName)
	}
	if f.File != "test_auth.py" {
		t.Errorf("File = %q", f.File)
	}
	if f.Line != 28 {
		t.Errorf("Line = %d", f.Line)
	}
}

func TestParsePythonTestOutput_MultipleFailures(t *testing.T) {
	output := `FAILED test_api.py::test_create_user - ValueError: invalid email
FAILED test_api.py::test_delete_user - PermissionError: forbidden
`
	failures := parsePythonTestOutput(output)
	if len(failures) != 2 {
		t.Fatalf("expected 2 failures, got %d", len(failures))
	}
	if failures[0].TestName != "test_create_user" {
		t.Errorf("[0] TestName = %q", failures[0].TestName)
	}
	if failures[1].TestName != "test_delete_user" {
		t.Errorf("[1] TestName = %q", failures[1].TestName)
	}
}

// ── Rust/cargo test parser ────────────────────────────────────────────────────

func TestParseRustTestOutput_PanicWithLocation(t *testing.T) {
	output := `running 3 tests
test module::test_add ... ok
test module::test_subtract ... FAILED
test module::test_multiply ... ok

failures:

---- module::test_subtract stdout ----
thread 'module::test_subtract' panicked at 'assertion failed: 5 - 3 == 1', src/lib.rs:42:5
`
	failures := parseRustTestOutput(output)
	if len(failures) == 0 {
		t.Fatal("expected at least 1 failure")
	}
	f := failures[0]
	if f.TestName != "module::test_subtract" {
		t.Errorf("TestName = %q", f.TestName)
	}
	if f.File != "src/lib.rs" {
		t.Errorf("File = %q", f.File)
	}
	if f.Line != 42 {
		t.Errorf("Line = %d", f.Line)
	}
	if f.Message != "assertion failed: 5 - 3 == 1" {
		t.Errorf("Message = %q", f.Message)
	}
}

func TestParseRustTestOutput_FailedOnly(t *testing.T) {
	output := `test basic::test_connect ... FAILED
test basic::test_disconnect ... FAILED

test result: FAILED. 0 passed; 2 failed
`
	failures := parseRustTestOutput(output)
	if len(failures) != 2 {
		t.Fatalf("expected 2 failures, got %d", len(failures))
	}
}

// ── Generic parser ────────────────────────────────────────────────────────────

func TestParseGenericTestOutput(t *testing.T) {
	output := `Running tests...
ERROR: test_something failed at app.rb:55
FAIL: another test
`
	failures := parseGenericTestOutput(output)
	if len(failures) == 0 {
		t.Fatal("expected at least 1 failure")
	}
}

// ── Dispatch routing ──────────────────────────────────────────────────────────

func TestParseTestOutput_Dispatch(t *testing.T) {
	goOutput := `--- FAIL: TestX (0.00s)
FAIL
`
	if failures := ParseTestOutput(RunnerGo, goOutput); len(failures) == 0 {
		t.Error("Go dispatch returned no failures")
	}

	nodeOutput := `FAIL src/test.ts
`
	if failures := ParseTestOutput(RunnerNode, nodeOutput); len(failures) == 0 {
		t.Error("Node dispatch returned no failures")
	}
}

// ── FailuresKey (stuck detection) ─────────────────────────────────────────────

func TestFailuresKey_StuckDetection(t *testing.T) {
	f1 := []TestFailure{{TestName: "TestA", File: "a.go", Line: 10, Message: "err"}}
	f2 := []TestFailure{{TestName: "TestA", File: "a.go", Line: 10, Message: "err"}}
	f3 := []TestFailure{{TestName: "TestA", File: "a.go", Line: 10, Message: "different"}}

	if FailuresKey(f1) != FailuresKey(f2) {
		t.Error("identical failures should produce same key")
	}
	if FailuresKey(f1) == FailuresKey(f3) {
		t.Error("different failures should produce different keys")
	}
}

func TestFailuresKey_Empty(t *testing.T) {
	key := FailuresKey(nil)
	if key != "" {
		t.Errorf("expected empty key for nil failures, got %q", key)
	}
}

// ── TestFailure.String ────────────────────────────────────────────────────────

func TestFailureString(t *testing.T) {
	f := TestFailure{TestName: "TestFoo", File: "foo.go", Line: 42, Message: "bad value"}
	s := f.String()
	if s != "TestFoo (foo.go:42): bad value" {
		t.Errorf("String() = %q", s)
	}

	f2 := TestFailure{TestName: "TestBar", Message: "error"}
	s2 := f2.String()
	if s2 != "TestBar (unknown): error" {
		t.Errorf("String() = %q", s2)
	}
}

// ── truncate ──────────────────────────────────────────────────────────────────

func TestTruncate(t *testing.T) {
	short := "hello"
	if truncate(short, 100) != short {
		t.Error("short string should not be truncated")
	}

	long := "abcdefghij"
	result := truncate(long, 5)
	if len(result) == 0 {
		t.Error("truncated result should not be empty")
	}
}

// ── TestLoop.testCommand ──────────────────────────────────────────────────────

func TestTestLoopTestCommand(t *testing.T) {
	tl := &TestLoop{
		Runner:  RunnerGo,
		TestCmd: "go test ./...",
	}

	// No package filter.
	if cmd := tl.testCommand(); cmd != "go test ./..." {
		t.Errorf("expected default cmd, got %q", cmd)
	}

	// With package filter.
	tl.Packages = "./internal/router/..."
	if cmd := tl.testCommand(); cmd != "go test ./internal/router/..." {
		t.Errorf("expected scoped cmd, got %q", cmd)
	}

	// Non-Go runner appends.
	tl.Runner = RunnerNode
	tl.TestCmd = "npm test"
	tl.Packages = "src/utils"
	if cmd := tl.testCommand(); cmd != "npm test src/utils" {
		t.Errorf("expected appended cmd, got %q", cmd)
	}
}
