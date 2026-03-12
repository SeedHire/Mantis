package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// TestFailure represents a single parsed test failure with actionable context.
type TestFailure struct {
	TestName string // e.g. "TestRouterClassify" or "test_login_flow"
	File     string // relative file path where the failure occurred
	Line     int    // line number (0 if unknown)
	Message  string // the error/assertion message
}

// String returns a human-readable summary of the failure.
func (f TestFailure) String() string {
	loc := f.File
	if f.Line > 0 {
		loc = fmt.Sprintf("%s:%d", f.File, f.Line)
	}
	if loc == "" {
		loc = "unknown"
	}
	return fmt.Sprintf("%s (%s): %s", f.TestName, loc, f.Message)
}

// TestRunner identifies which test framework to use for a project.
type TestRunner string

const (
	RunnerGo      TestRunner = "go"
	RunnerNode    TestRunner = "node"
	RunnerPython  TestRunner = "python"
	RunnerRust    TestRunner = "rust"
	RunnerUnknown TestRunner = "unknown"
)

// DetectTestRunner scans the project root for build files to determine the test framework.
func DetectTestRunner(root string) (TestRunner, string) {
	checks := []struct {
		file   string
		runner TestRunner
		cmd    string
	}{
		{"go.mod", RunnerGo, "go test ./..."},
		{"package.json", RunnerNode, "npm test"},
		{"Cargo.toml", RunnerRust, "cargo test"},
		{"pyproject.toml", RunnerPython, "python -m pytest -v"},
		{"setup.py", RunnerPython, "python -m pytest -v"},
		{"setup.cfg", RunnerPython, "python -m pytest -v"},
		{"requirements.txt", RunnerPython, "python -m pytest -v"},
	}
	for _, c := range checks {
		if _, err := os.Stat(filepath.Join(root, c.file)); err == nil {
			return c.runner, c.cmd
		}
	}
	return RunnerUnknown, ""
}

// ParseTestOutput dispatches to the appropriate language-specific parser.
func ParseTestOutput(runner TestRunner, output string) []TestFailure {
	switch runner {
	case RunnerGo:
		return parseGoTestOutput(output)
	case RunnerNode:
		return parseNodeTestOutput(output)
	case RunnerPython:
		return parsePythonTestOutput(output)
	case RunnerRust:
		return parseRustTestOutput(output)
	default:
		return parseGenericTestOutput(output)
	}
}

// ── Go test parser ────────────────────────────────────────────────────────────

var (
	// --- FAIL: TestName (0.00s)
	goFailRe = regexp.MustCompile(`(?m)^--- FAIL: (\S+)\s`)
	// file.go:42: error message
	goLocRe = regexp.MustCompile(`(?m)^\s+(\S+\.go):(\d+):\s+(.+)`)
)

func parseGoTestOutput(output string) []TestFailure {
	var failures []TestFailure

	// Split output into sections by test failure markers.
	failNames := goFailRe.FindAllStringSubmatchIndex(output, -1)
	if len(failNames) == 0 {
		// Try fallback: FAIL markers without "---" prefix (package-level failures).
		return parseGoPackageFailures(output)
	}

	for _, idx := range failNames {
		testName := output[idx[2]:idx[3]]

		// Search backwards from the FAIL line for file:line references belonging to this test.
		section := output[:idx[0]]
		locs := goLocRe.FindAllStringSubmatch(section, -1)

		if len(locs) > 0 {
			// Take the last location reference (closest to the failure).
			last := locs[len(locs)-1]
			line, _ := strconv.Atoi(last[2])
			failures = append(failures, TestFailure{
				TestName: testName,
				File:     last[1],
				Line:     line,
				Message:  strings.TrimSpace(last[3]),
			})
		} else {
			failures = append(failures, TestFailure{
				TestName: testName,
				Message:  "test failed (no location info in output)",
			})
		}
	}
	return failures
}

// parseGoPackageFailures handles cases where Go outputs "FAIL package" without individual test markers.
func parseGoPackageFailures(output string) []TestFailure {
	// Look for compilation errors: file.go:line:col: error
	compileRe := regexp.MustCompile(`(?m)^(\S+\.go):(\d+):\d+:\s+(.+)`)
	matches := compileRe.FindAllStringSubmatch(output, -1)
	var failures []TestFailure
	for _, m := range matches {
		line, _ := strconv.Atoi(m[2])
		failures = append(failures, TestFailure{
			TestName: "compilation",
			File:     m[1],
			Line:     line,
			Message:  strings.TrimSpace(m[3]),
		})
	}
	if len(failures) > 0 {
		return failures
	}

	// Generic FAIL line.
	failPkgRe := regexp.MustCompile(`(?m)^FAIL\s+(\S+)`)
	for _, m := range failPkgRe.FindAllStringSubmatch(output, -1) {
		failures = append(failures, TestFailure{
			TestName: "package:" + m[1],
			Message:  "package tests failed",
		})
	}
	return failures
}

// ── Node/Jest parser ──────────────────────────────────────────────────────────

var (
	// FAIL src/utils.test.ts
	jestFailFileRe = regexp.MustCompile(`(?m)^FAIL\s+(\S+)`)
	// ● test suite name › test name
	jestTestNameRe = regexp.MustCompile(`(?m)^\s*●\s+(.+)`)
	// Expected: X / Received: Y or expect(...).toBe(...)
	jestExpectRe = regexp.MustCompile(`(?m)^\s+(Expected|Received|expect\()(.*)`)
	// at Object.<anonymous> (src/file.ts:42:5)
	jestLocRe = regexp.MustCompile(`(?m)at\s+\S+\s+\(([^:]+):(\d+):\d+\)`)
)

func parseNodeTestOutput(output string) []TestFailure {
	var failures []TestFailure

	// Find test name markers (● lines).
	testNames := jestTestNameRe.FindAllStringSubmatch(output, -1)
	testNameIdxs := jestTestNameRe.FindAllStringIndex(output, -1)

	if len(testNames) == 0 {
		// Fallback: look for FAIL file markers.
		for _, m := range jestFailFileRe.FindAllStringSubmatch(output, -1) {
			failures = append(failures, TestFailure{
				TestName: filepath.Base(m[1]),
				File:     m[1],
				Message:  "test suite failed",
			})
		}
		if len(failures) == 0 {
			return parseGenericTestOutput(output)
		}
		return failures
	}

	for i, nameMatch := range testNames {
		testName := strings.TrimSpace(nameMatch[1])

		// Get the section after this test name until the next one.
		start := testNameIdxs[i][1]
		end := len(output)
		if i+1 < len(testNameIdxs) {
			end = testNameIdxs[i+1][0]
		}
		section := output[start:end]

		// Extract error message.
		msg := "test failed"
		if m := jestExpectRe.FindStringSubmatch(section); len(m) > 0 {
			msg = strings.TrimSpace(m[0])
		}

		// Extract file location.
		var file string
		var line int
		if m := jestLocRe.FindStringSubmatch(section); len(m) > 0 {
			file = m[1]
			line, _ = strconv.Atoi(m[2])
		}

		failures = append(failures, TestFailure{
			TestName: testName,
			File:     file,
			Line:     line,
			Message:  msg,
		})
	}
	return failures
}

// ── Python/pytest parser ──────────────────────────────────────────────────────

var (
	// FAILED test_file.py::TestClass::test_method - AssertionError: ...
	pytestFailRe = regexp.MustCompile(`(?m)^FAILED\s+(\S+?)(?:\s+-\s+(.+))?$`)
	// test_file.py:42: AssertionError
	pytestLocRe = regexp.MustCompile(`(?m)^(\S+\.py):(\d+):\s+(\w+(?:Error|Exception).*)`)
	// >       assert result == expected
	pytestAssertRe = regexp.MustCompile(`(?m)^>\s+assert\s+(.+)`)
	// E       AssertionError: ...
	pytestERe = regexp.MustCompile(`(?m)^E\s+(.+)`)
)

func parsePythonTestOutput(output string) []TestFailure {
	var failures []TestFailure

	for _, m := range pytestFailRe.FindAllStringSubmatch(output, -1) {
		testPath := m[1] // e.g. test_file.py::TestClass::test_method
		errMsg := m[2]

		// Parse test path into file and test name.
		parts := strings.SplitN(testPath, "::", 2)
		file := parts[0]
		testName := testPath
		if len(parts) > 1 {
			testName = parts[1]
		}

		// Try to find a more specific error in the output.
		if errMsg == "" {
			// Search for E lines near this test.
			if em := pytestERe.FindStringSubmatch(output); len(em) > 0 {
				errMsg = strings.TrimSpace(em[1])
			}
		}
		if errMsg == "" {
			errMsg = "test failed"
		}

		// Try to find line number.
		var line int
		if lm := pytestLocRe.FindStringSubmatch(output); len(lm) > 0 && lm[1] == file {
			line, _ = strconv.Atoi(lm[2])
		}

		failures = append(failures, TestFailure{
			TestName: testName,
			File:     file,
			Line:     line,
			Message:  errMsg,
		})
	}

	if len(failures) > 0 {
		return failures
	}

	// Fallback: look for assertion lines.
	if m := pytestAssertRe.FindStringSubmatch(output); len(m) > 0 {
		failures = append(failures, TestFailure{
			TestName: "unknown",
			Message:  "assert " + strings.TrimSpace(m[1]),
		})
	}

	if len(failures) == 0 {
		return parseGenericTestOutput(output)
	}
	return failures
}

// ── Rust/cargo test parser ────────────────────────────────────────────────────

var (
	// test module::test_name ... FAILED
	rustFailRe = regexp.MustCompile(`(?m)^test\s+(\S+)\s+\.\.\.\s+FAILED`)
	// thread 'test_name' panicked at 'message', file.rs:42:5
	rustPanicRe = regexp.MustCompile(`(?m)thread\s+'([^']+)'\s+panicked\s+at\s+'([^']*)',\s+(\S+):(\d+):\d+`)
	// thread 'test_name' panicked at file.rs:42:5:\nmessage
	rustPanicRe2 = regexp.MustCompile(`(?m)thread\s+'([^']+)'\s+panicked\s+at\s+(\S+):(\d+):\d+`)
)

func parseRustTestOutput(output string) []TestFailure {
	var failures []TestFailure

	// Try panic messages first (more info).
	panicMatches := rustPanicRe.FindAllStringSubmatch(output, -1)
	seen := map[string]bool{}
	for _, m := range panicMatches {
		testName := m[1]
		if seen[testName] {
			continue
		}
		seen[testName] = true
		line, _ := strconv.Atoi(m[4])
		failures = append(failures, TestFailure{
			TestName: testName,
			File:     m[3],
			Line:     line,
			Message:  m[2],
		})
	}

	// Try alternate panic format.
	if len(failures) == 0 {
		for _, m := range rustPanicRe2.FindAllStringSubmatch(output, -1) {
			testName := m[1]
			if seen[testName] {
				continue
			}
			seen[testName] = true
			line, _ := strconv.Atoi(m[3])
			failures = append(failures, TestFailure{
				TestName: testName,
				File:     m[2],
				Line:     line,
				Message:  "panicked",
			})
		}
	}

	// Fallback to FAILED lines.
	if len(failures) == 0 {
		for _, m := range rustFailRe.FindAllStringSubmatch(output, -1) {
			failures = append(failures, TestFailure{
				TestName: m[1],
				Message:  "test failed",
			})
		}
	}

	if len(failures) == 0 {
		return parseGenericTestOutput(output)
	}
	return failures
}

// ── Generic fallback parser ───────────────────────────────────────────────────

var (
	genericFailRe = regexp.MustCompile(`(?mi)^.*(?:FAIL|ERROR|FAILED|error)[:]\s*(.+)`)
	genericLocRe  = regexp.MustCompile(`(\S+\.\w+):(\d+)`)
)

func parseGenericTestOutput(output string) []TestFailure {
	var failures []TestFailure
	seen := map[string]bool{}

	for _, m := range genericFailRe.FindAllStringSubmatch(output, 10) {
		msg := strings.TrimSpace(m[1])
		if seen[msg] || msg == "" {
			continue
		}
		seen[msg] = true

		var file string
		var line int
		if loc := genericLocRe.FindStringSubmatch(m[0]); len(loc) > 0 {
			file = loc[1]
			line, _ = strconv.Atoi(loc[2])
		}

		failures = append(failures, TestFailure{
			TestName: "unknown",
			File:     file,
			Line:     line,
			Message:  msg,
		})
	}
	return failures
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// FailureKey returns a stable string key for deduplication / stuck detection.
// Intentionally excludes Message so that flaky tests with varying assertion values
// (e.g. "expected 5 got 3" vs "expected 5 got 4") are still detected as stuck.
func (f TestFailure) FailureKey() string {
	return fmt.Sprintf("%s|%s|%d", f.TestName, f.File, f.Line)
}

// FailuresKey returns a combined key for a set of failures (for stuck detection).
func FailuresKey(failures []TestFailure) string {
	keys := make([]string, len(failures))
	for i, f := range failures {
		keys[i] = f.FailureKey()
	}
	sort.Strings(keys) // Go randomizes test order; sort for stable stuck detection
	return strings.Join(keys, "\n")
}
