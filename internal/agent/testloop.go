package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/seedhire/mantis/internal/ollama"
	"github.com/seedhire/mantis/internal/router"
)

const (
	defaultMaxTestIter = 5
	testContextLines   = 20 // lines above/below failure to show model
)

// Terminal colours (local copy to avoid circular import with repl).
const (
	tlColorReset = "\033[0m"
	tlColorGold  = "\033[38;5;220m"
	tlColorDim   = "\033[38;5;244m"
	tlColorGreen = "\033[38;5;70m"
	tlColorRed   = "\033[38;5;196m"
)

// TestLoopResult holds the outcome of an iterative test loop run.
type TestLoopResult struct {
	Passed      bool           // all tests passed
	Iterations  int            // how many fix iterations were run
	Failures    []TestFailure  // remaining failures (empty if Passed)
	FixSummary  string         // model's summary of what was fixed
	StuckReason string         // non-empty if loop exited due to stuck detection
}

// TestLoop runs an iterative test→fix→retest cycle.
type TestLoop struct {
	Toolkit  *AgentToolkit
	Client   *ollama.Client
	Model    string     // model to use for fixes; defaults to TierCode
	Root     string     // project root
	MaxIter  int        // max fix iterations; defaults to 5
	Runner   TestRunner // auto-detected if empty
	TestCmd  string     // auto-detected if empty
	Packages string     // optional: specific package/path to test (e.g. "./internal/router/...")
}

// Run executes the test loop: run tests → parse failures → fix → re-run → repeat.
// Returns when tests pass, the loop gets stuck, or maxIter is reached.
func (tl *TestLoop) Run(ctx context.Context) (*TestLoopResult, error) {
	if err := tl.init(); err != nil {
		return nil, err
	}

	result := &TestLoopResult{}
	var lastFailuresKey string
	var fixSummaries []string

	for iter := 0; iter < tl.MaxIter; iter++ {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		result.Iterations = iter + 1

		// ── Run tests ────────────────────────────────────────────────────
		testCmd := tl.testCommand()
		fmt.Printf("%s  [test %d/%d] running: %s%s\n", tlColorDim, iter+1, tl.MaxIter, testCmd, tlColorReset)

		output, exitCode := tl.Toolkit.RunBash(testCmd, 120)

		if exitCode == 0 {
			fmt.Printf("%s  ✓ all tests pass%s\n", tlColorGreen, tlColorReset)
			result.Passed = true
			result.Failures = nil
			if len(fixSummaries) > 0 {
				result.FixSummary = strings.Join(fixSummaries, "\n")
			}
			return result, nil
		}

		// ── Parse failures ───────────────────────────────────────────────
		failures := ParseTestOutput(tl.Runner, output)
		if len(failures) == 0 {
			// Tests failed but parser found nothing — surface raw output.
			fmt.Printf("%s  ⚠ tests failed but could not parse failures%s\n", tlColorDim, tlColorReset)
			result.Failures = []TestFailure{{
				TestName: "unknown",
				Message:  truncate(output, 2000),
			}}
			result.StuckReason = "could not parse test failures from output"
			return result, nil
		}

		fmt.Printf("%s  ✗ %d failing test(s)%s\n", tlColorRed, len(failures), tlColorReset)
		for _, f := range failures {
			fmt.Printf("%s    • %s%s\n", tlColorDim, f.String(), tlColorReset)
		}

		// ── Stuck detection ──────────────────────────────────────────────
		currentKey := FailuresKey(failures)
		if currentKey == lastFailuresKey {
			fmt.Printf("%s  [stuck: same failures after fix — stopping]%s\n", tlColorDim, tlColorReset)
			result.Failures = failures
			result.StuckReason = "same test failures appeared after fix attempt"
			if len(fixSummaries) > 0 {
				result.FixSummary = strings.Join(fixSummaries, "\n")
			}
			return result, nil
		}
		lastFailuresKey = currentKey
		result.Failures = failures

		// ── Read failing source files ────────────────────────────────────
		sourceContext := tl.readFailureContext(failures)

		// ── Prompt model for fix ─────────────────────────────────────────
		fmt.Printf("%s  [fixing %d failure(s) with %s]%s\n", tlColorDim, len(failures), tl.Model, tlColorReset)

		fixSummary, err := tl.promptFix(ctx, failures, sourceContext, output)
		if err != nil {
			return nil, fmt.Errorf("fix prompt failed: %w", err)
		}
		if fixSummary != "" {
			fixSummaries = append(fixSummaries, fixSummary)
		}
	}

	// Max iterations reached.
	fmt.Printf("%s  [max iterations (%d) reached — stopping]%s\n", tlColorDim, tl.MaxIter, tlColorReset)
	result.StuckReason = fmt.Sprintf("max iterations (%d) reached", tl.MaxIter)
	if len(fixSummaries) > 0 {
		result.FixSummary = strings.Join(fixSummaries, "\n")
	}
	return result, nil
}

// init sets defaults and auto-detects test runner if needed.
func (tl *TestLoop) init() error {
	if tl.MaxIter <= 0 {
		tl.MaxIter = defaultMaxTestIter
	}
	if tl.Model == "" {
		tl.Model = router.ModelFor(router.TierCode)
	}
	if tl.Root == "" {
		tl.Root = tl.Toolkit.projectRoot
	}
	if tl.Runner == "" || tl.TestCmd == "" {
		runner, cmd := DetectTestRunner(tl.Root)
		if runner == RunnerUnknown {
			return fmt.Errorf("could not detect test runner — no go.mod, package.json, Cargo.toml, or pyproject.toml found")
		}
		tl.Runner = runner
		tl.TestCmd = cmd
	}
	return nil
}

// testCommand returns the test command, optionally scoped to specific packages.
func (tl *TestLoop) testCommand() string {
	if tl.Packages == "" {
		return tl.TestCmd
	}
	// For Go: replace "./..." with the specific package path.
	if tl.Runner == RunnerGo {
		return "go test " + tl.Packages
	}
	// For others, append the package/path filter.
	return tl.TestCmd + " " + tl.Packages
}

// readFailureContext reads source code around each failure location.
func (tl *TestLoop) readFailureContext(failures []TestFailure) string {
	var sb strings.Builder
	seen := map[string]bool{}

	for _, f := range failures {
		if f.File == "" {
			continue
		}
		// Deduplicate by file+line.
		key := fmt.Sprintf("%s:%d", f.File, f.Line)
		if seen[key] {
			continue
		}
		seen[key] = true

		startLine := 1
		endLine := 0 // 0 = EOF (for small files)
		if f.Line > 0 {
			startLine = f.Line - testContextLines
			if startLine < 1 {
				startLine = 1
			}
			endLine = f.Line + testContextLines
		}

		content, err := tl.Toolkit.ReadFile(f.File, startLine, endLine)
		if err != nil {
			continue
		}
		fmt.Fprintf(&sb, "\n### %s (lines %d–%d)\n```\n%s\n```\n", f.File, startLine, endLine, content)
	}
	return sb.String()
}

// promptFix sends the failure context to the model and executes tool calls to fix the code.
func (tl *TestLoop) promptFix(ctx context.Context, failures []TestFailure, sourceContext, rawOutput string) (string, error) {
	// Build structured failure description.
	var failureDesc strings.Builder
	for i, f := range failures {
		fmt.Fprintf(&failureDesc, "%d. Test: %s\n", i+1, f.TestName)
		if f.File != "" {
			fmt.Fprintf(&failureDesc, "   File: %s", f.File)
			if f.Line > 0 {
				fmt.Fprintf(&failureDesc, ":%d", f.Line)
			}
			failureDesc.WriteString("\n")
		}
		fmt.Fprintf(&failureDesc, "   Error: %s\n\n", f.Message)
	}

	systemPrompt := fmt.Sprintf(
		"You are a test fixer. Your job is to fix failing tests by making targeted code changes.\n\n"+
			"RULES:\n"+
			"- Use edit_file to make precise old→new replacements. NEVER use write_file to rewrite entire files.\n"+
			"- Fix the actual bug in the source code, not the test (unless the test itself is wrong).\n"+
			"- Make the MINIMUM change needed to fix the failure.\n"+
			"- After making changes, call finish with a brief summary.\n"+
			"- If you cannot determine the fix, call finish with an explanation of what you found.\n"+
			"- Project root: %s\n"+
			"- Test runner: %s",
		tl.Root, string(tl.Runner),
	)

	userPrompt := fmt.Sprintf(
		"The following tests are failing. Fix them.\n\n"+
			"## Failing Tests\n%s\n"+
			"## Source Context\n%s\n"+
			"## Raw Test Output (last 3000 chars)\n```\n%s\n```\n\n"+
			"Read the relevant files, identify the root cause, and use edit_file to fix it.",
		failureDesc.String(), sourceContext, truncate(rawOutput, 3000),
	)

	msgs := []interface{}{
		ollama.Message{Role: "system", Content: systemPrompt},
		ollama.Message{Role: "user", Content: userPrompt},
	}

	tools := tl.Toolkit.Tools()
	var summary string
	const maxToolIter = 8 // allow more iterations for reading + fixing
	toolErrCount := 0

	for iter := 0; iter < maxToolIter; iter++ {
		result, err := tl.Client.ChatWithTools(ctx, tl.Model, msgs, tools, nil)
		if err != nil {
			return "", err
		}

		if len(result.ToolCalls) == 0 {
			summary = result.Content
			break
		}

		msgs = append(msgs, ollama.Message{Role: "assistant", Content: result.Content})

		finished := false
		for _, tc := range result.ToolCalls {
			out, dispErr := tl.Toolkit.Dispatch(ctx, tc.Function.Name, tc.Function.Arguments)
			if dispErr != nil {
				if errors.Is(dispErr, ErrFinished) {
					summary = out
					finished = true
				} else {
					toolErrCount++
					out = fmt.Sprintf("error: %s", dispErr)
				}
			}
			msgs = append(msgs, ollama.ToolMessage{
				Role:    "tool",
				Content: out,
			})
		}
		if finished {
			break
		}
		if toolErrCount >= 3 {
			return "aborted: too many tool errors", nil
		}
	}

	return summary, nil
}

// truncate shortens s to maxLen characters, appending "[truncated]" if needed.
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[len(s)-maxLen:] + "\n[truncated — showing last " + fmt.Sprintf("%d", maxLen) + " chars]"
}
