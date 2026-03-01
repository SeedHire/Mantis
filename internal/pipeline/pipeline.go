// Package pipeline implements the multi-stage SWE pipeline for complex development requests.
//
// For requests like "build an app", "implement a REST API with auth", "create a CLI tool",
// it routes each cognitive phase to the model tier best suited for it:
//
//	Stage 1: PLAN  (TierReason) — decomposes task, identifies files, risks, assumptions
//	Stage 2: CODE  (TierCode)   — implements based on plan, emits lang:filepath blocks   ┐ parallel
//	Stage 3: TESTS (TierCode)   — writes tests based on plan, emits lang:filepath blocks ┘
//
// Parallel CODE+TESTS means the user waits for the longest of the two, not both combined.
// Each stage receives only the context it needs — no bloated full-history re-send.
package pipeline

import (
	"context"
	"fmt"
	"strings"

	"github.com/seedhire/mantis/internal/ollama"
	"github.com/seedhire/mantis/internal/router"
)

// Terminal colours (local copy to avoid circular import with repl).
const (
	pColorReset = "\033[0m"
	pColorGold  = "\033[38;5;220m"
	pColorDim   = "\033[38;5;244m"
)

// Options controls pipeline execution behaviour.
type Options struct {
	AvailableModels []ollama.ModelInfo
	SkipTests       bool // skip test generation for faster turnaround
}

// Result holds aggregated pipeline output and token counts.
type Result struct {
	PlanText  string
	CodeText  string
	TestText  string
	Combined  string // assembled markdown, ready for renderResponse + extractAndWriteFiles
	PromptTok int
	ComplTok  int
}

// ShouldRun returns true when the message is complex enough to warrant the pipeline.
// Only triggers on multi-component build/implement requests; simple fixes use the
// normal single-model path.
func ShouldRun(intent router.Intent, message string) bool {
	switch intent.Tier {
	case router.TierMax, router.TierTrivial, router.TierFast, router.TierVision:
		return false
	}
	return isComplexBuild(strings.ToLower(message))
}

// Run executes the 3-stage pipeline and returns the assembled result.
// systemPrompt is the full base prompt (identity + skills + brain context).
func Run(
	ctx context.Context,
	client *ollama.Client,
	userRequest string,
	systemPrompt string,
	opts Options,
) (*Result, error) {

	planModel := router.ModelFor(router.TierReason)
	codeModel := router.ModelFor(router.TierCode)
	res := &Result{}

	// ── Stage 1: PLAN ─────────────────────────────────────────────────────────
	fmt.Printf("%s  ◆ planning   [%s]%s\n", pColorDim, planModel, pColorReset)

	planMsgs := []interface{}{
		ollama.Message{Role: "system", Content: systemPrompt + planStageSuffix},
		ollama.Message{Role: "user", Content: planUserPrompt(userRequest)},
	}
	var planBuf strings.Builder
	pt, ct, err := client.StreamChat(ctx, planModel, planMsgs, nil, func(c string) { planBuf.WriteString(c) })
	if err != nil {
		// Plan model unavailable — fall back to code model so the pipeline still runs.
		fmt.Printf("%s  ⚠ plan model unavailable, falling back to %s%s\n", pColorDim, codeModel, pColorReset)
		planBuf.Reset()
		pt, ct, err = client.StreamChat(ctx, codeModel, planMsgs, nil, func(c string) { planBuf.WriteString(c) })
		if err != nil {
			return nil, fmt.Errorf("plan stage: %w", err)
		}
	}
	res.PlanText = planBuf.String()
	res.PromptTok += pt
	res.ComplTok += ct
	fmt.Printf("%s  ✓ plan ready%s\n", pColorGold, pColorReset)

	// ── Stage 2 + 3: CODE and TESTS in parallel ───────────────────────────────
	fmt.Printf("%s  ◆ coding + testing   [%s · parallel]%s\n", pColorDim, codeModel, pColorReset)

	type stageOut struct {
		text   string
		pt, ct int
		err    error
	}
	codeCh := make(chan stageOut, 1)
	testCh := make(chan stageOut, 1)

	go func() {
		msgs := []interface{}{
			ollama.Message{Role: "system", Content: systemPrompt + codeStageSuffix},
			ollama.Message{Role: "user", Content: codeUserPrompt(userRequest, res.PlanText)},
		}
		var buf strings.Builder
		pt, ct, err := client.StreamChat(ctx, codeModel, msgs, nil, func(c string) { buf.WriteString(c) })
		codeCh <- stageOut{buf.String(), pt, ct, err}
	}()

	go func() {
		if opts.SkipTests {
			testCh <- stageOut{}
			return
		}
		msgs := []interface{}{
			ollama.Message{Role: "system", Content: systemPrompt + testStageSuffix},
			ollama.Message{Role: "user", Content: testUserPrompt(userRequest, res.PlanText)},
		}
		var buf strings.Builder
		pt, ct, err := client.StreamChat(ctx, codeModel, msgs, nil, func(c string) { buf.WriteString(c) })
		testCh <- stageOut{buf.String(), pt, ct, err}
	}()

	codeOut := <-codeCh
	testOut := <-testCh

	if codeOut.err != nil {
		return nil, fmt.Errorf("code stage: %w", codeOut.err)
	}
	res.CodeText = codeOut.text
	res.PromptTok += codeOut.pt
	res.ComplTok += codeOut.ct

	if testOut.err == nil && strings.TrimSpace(testOut.text) != "" {
		res.TestText = testOut.text
		res.PromptTok += testOut.pt
		res.ComplTok += testOut.ct
		fmt.Printf("%s  ✓ code ready  ✓ tests ready%s\n", pColorGold, pColorReset)
	} else {
		fmt.Printf("%s  ✓ code ready%s\n", pColorGold, pColorReset)
	}

	res.Combined = assemble(res.PlanText, res.CodeText, res.TestText)
	return res, nil
}

// assemble stitches the three stage outputs into clean markdown.
func assemble(plan, code, tests string) string {
	var sb strings.Builder
	sb.WriteString("## Plan\n\n")
	sb.WriteString(strings.TrimSpace(plan))
	sb.WriteString("\n\n---\n\n## Implementation\n\n")
	sb.WriteString(strings.TrimSpace(code))
	if strings.TrimSpace(tests) != "" {
		sb.WriteString("\n\n---\n\n## Tests\n\n")
		sb.WriteString(strings.TrimSpace(tests))
	}
	return sb.String()
}

// ── Complexity detector ────────────────────────────────────────────────────────

func isComplexBuild(lower string) bool {
	buildVerbs := []string{"build", "create", "develop", "implement", "make", "write"}
	hasBuild := false
	for _, v := range buildVerbs {
		if strings.Contains(lower, v) {
			hasBuild = true
			break
		}
	}
	if !hasBuild {
		return false
	}
	for _, s := range complexSignals {
		if strings.Contains(lower, s) {
			return true
		}
	}
	// Implicit: two or more distinct code-domain components present.
	count := 0
	for _, c := range codeComponents {
		if strings.Contains(lower, c) {
			count++
		}
	}
	return count >= 2
}

var complexSignals = []string{
	"app", "application", "system", "service",
	"from scratch", "full stack", "fullstack", "full-stack",
	"complete", "entire project", "whole project",
	"with auth", "with database", "with tests", "with frontend",
	"rest api", "graphql api", "grpc", "microservice",
	"cli tool", "web app", "web server",
}

var codeComponents = []string{
	"api", "endpoint", "route", "handler",
	"database", "db", "schema", "model", "migration",
	"auth", "authentication", "jwt", "session", "oauth",
	"frontend", "backend", "ui", "server", "client",
	"test", "spec", "validation", "middleware", "config",
}

// ── Stage system-prompt suffixes ──────────────────────────────────────────────
// Appended to the base system prompt so each stage inherits full project
// context + skills but gets precise, role-specific instructions.

const planStageSuffix = `

## Your role: ARCHITECT
Analyze the development request and produce a structured implementation plan.
Do NOT write any implementation code. Focus entirely on planning.

Use these exact headers:
### Overview
### Files
### Architecture
### Task Breakdown
### Risks & Edge Cases
### Assumptions
`

const codeStageSuffix = `

## Your role: IMPLEMENTER
Write the complete, production-ready implementation based on the plan.
- Use ` + "```lang:filepath" + ` fences for EVERY file (e.g. ` + "```python:app.py" + `).
- Handle all error cases. No stubs. No TODOs left unimplemented.
- Validate inputs at boundaries. Return structured errors.
`

const testStageSuffix = `

## Your role: TEST ENGINEER
Write comprehensive tests based on the implementation plan.
- Use ` + "```lang:filepath" + ` fences for every test file.
- Cover: happy path, edge cases, error cases, boundary values.
- Descriptive test names: test_<scenario>_<expected_behaviour>.
- Mock external dependencies. Test behaviour, not implementation.
`

// ── User prompts per stage ─────────────────────────────────────────────────────

func planUserPrompt(req string) string {
	return "Analyze this development request and produce a structured implementation plan:\n\n" + req
}

func codeUserPrompt(req, plan string) string {
	return fmt.Sprintf(
		"Implement the following based on the plan provided.\n\n"+
			"## Original Request\n%s\n\n"+
			"## Implementation Plan\n%s\n\n"+
			"Write the complete implementation now. Every file must use `lang:filepath` fences.",
		req, plan,
	)
}

func testUserPrompt(req, plan string) string {
	return fmt.Sprintf(
		"Write comprehensive tests based on the following.\n\n"+
			"## Original Request\n%s\n\n"+
			"## Implementation Plan\n%s\n\n"+
			"Write all test files now. Every test file must use `lang:filepath` fences.",
		req, plan,
	)
}
