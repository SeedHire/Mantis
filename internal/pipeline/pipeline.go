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
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/seedhire/mantis/internal/autofix"
	"github.com/seedhire/mantis/internal/ollama"
	"github.com/seedhire/mantis/internal/router"
)

// Terminal colours (local copy to avoid circular import with repl).
const (
	pColorReset = "\033[0m"
	pColorGold  = "\033[38;5;220m"
	pColorDim   = "\033[38;5;244m"
)

// progressTicker prints a live token counter + elapsed time on the current line while a stage runs.
// Call the returned stop function when the stage completes; it returns elapsed time.
func progressTicker(stage string) (incr func(), stop func() time.Duration) {
	var count int64
	done := make(chan struct{})
	start := time.Now()
	frames := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

	incr = func() { atomic.AddInt64(&count, 1) }

	go func() {
		i := 0
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				fmt.Printf("\r\033[K")
				return
			case <-ticker.C:
				n := atomic.LoadInt64(&count)
				elapsed := time.Since(start).Seconds()
				fmt.Printf("\r%s  %s %s  %d tokens · %.1fs%s", pColorDim, frames[i%len(frames)], stage, n, elapsed, pColorReset)
				i++
			}
		}
	}()

	stop = func() time.Duration {
		close(done)
		return time.Since(start)
	}
	return
}

// Options controls pipeline execution behaviour.
type Options struct {
	AvailableModels []ollama.ModelInfo
	SkipTests       bool   // skip test generation for faster turnaround
	Root            string // project root for build verification; empty = skip
	MaxRetries      int    // max CODE stage retries on build failure (default 3)
	PlanOnly        bool   // stop after PLAN stage and return for user approval
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
	planIncr, planStop := progressTicker("planning")
	pt, ct, err := client.StreamChat(ctx, planModel, planMsgs, nil, func(c string) { planBuf.WriteString(c); planIncr() })
	planElapsed := planStop()
	if err != nil {
		// Plan model unavailable — fall back to code model so the pipeline still runs.
		fmt.Printf("%s  ⚠ plan model unavailable, falling back to %s%s\n", pColorDim, codeModel, pColorReset)
		planBuf.Reset()
		planIncr2, planStop2 := progressTicker("planning")
		pt, ct, err = client.StreamChat(ctx, codeModel, planMsgs, nil, func(c string) { planBuf.WriteString(c); planIncr2() })
		planElapsed = planStop2()
		if err != nil {
			return nil, fmt.Errorf("plan stage: %w", err)
		}
	}
	res.PlanText = planBuf.String()
	res.PromptTok += pt
	res.ComplTok += ct
	fmt.Printf("%s  ✓ plan ready  %s(%.1fs · %d tokens)%s\n", pColorGold, pColorDim, planElapsed.Seconds(), pt+ct, pColorReset)

	// Plan Mode: stop after PLAN stage for user review.
	if opts.PlanOnly {
		res.Combined = res.PlanText
		return res, nil
	}

	// ── Stage 2: CODE — bounded agentic retry loop ────────────────────────────
	fmt.Printf("%s  ◆ coding   [%s]%s\n", pColorDim, codeModel, pColorReset)

	maxRetries := opts.MaxRetries
	if maxRetries <= 0 {
		maxRetries = 3
	}

	codeMsgs := []interface{}{
		ollama.Message{Role: "system", Content: systemPrompt + codeStageSuffix},
		ollama.Message{Role: "user", Content: codeUserPrompt(userRequest, res.PlanText)},
	}

	var lastBuildErr string
	codeStart := time.Now()
	var codeTokTotal int
	for attempt := 0; attempt <= maxRetries; attempt++ {
		var codeBuf strings.Builder
		codeIncr, codeStop := progressTicker("coding")
		pt, ct, err := client.StreamChat(ctx, codeModel, codeMsgs, nil, func(c string) { codeBuf.WriteString(c); codeIncr() })
		codeStop()
		if err != nil {
			return nil, fmt.Errorf("code stage: %w", err)
		}
		res.CodeText = codeBuf.String()
		res.PromptTok += pt
		res.ComplTok += ct
		codeTokTotal += pt + ct

		// Write files and verify build (only if Root provided).
		if opts.Root == "" || attempt == maxRetries {
			break
		}
		written := writeCodeFiles(res.CodeText, opts.Root)
		if len(written) == 0 {
			break // no files to check
		}
		buildResult := autofix.Check(opts.Root, written)
		if buildResult == nil || buildResult.Passed {
			break // success
		}

		buildErrStr := buildResult.Output
		if buildErrStr == lastBuildErr {
			// Truncate for display but keep it informative.
			errPreview := buildErrStr
			if len(errPreview) > 200 {
				errPreview = errPreview[:200] + "…"
			}
			fmt.Printf("%s  [stuck: same build error twice — stopping retry]%s\n", pColorDim, pColorReset)
			fmt.Printf("%s  %s%s\n", pColorDim, errPreview, pColorReset)
			break
		}
		lastBuildErr = buildErrStr
		fmt.Printf("%s  [build error — retry %d/%d]%s\n", pColorDim, attempt+1, maxRetries, pColorReset)

		retryMsg := fmt.Sprintf(
			"Build failed with the following error:\n\n```\n%s\n```\n\n"+
				"Fix only the affected function(s). Do not rewrite unchanged parts. "+
				"Output the corrected file(s) using ```lang:filepath fences.",
			buildErrStr)
		// Keep messages bounded: system + original user + latest assistant + latest error.
		// Trim any prior retry turns before appending the new ones.
		if len(codeMsgs) > 2 {
			codeMsgs = codeMsgs[:2]
		}
		codeMsgs = append(codeMsgs,
			ollama.Message{Role: "assistant", Content: res.CodeText},
			ollama.Message{Role: "user", Content: retryMsg},
		)
	}
	codeElapsed := time.Since(codeStart)
	fmt.Printf("%s  ✓ code ready  %s(%.1fs · %d tokens)%s\n", pColorGold, pColorDim, codeElapsed.Seconds(), codeTokTotal, pColorReset)

	// ── Stage 3: TESTS in parallel ────────────────────────────────────────────
	type stageOut struct {
		text    string
		pt, ct  int
		elapsed time.Duration
		err     error
	}
	testCh := make(chan stageOut, 1)

	go func() {
		if opts.SkipTests {
			testCh <- stageOut{}
			return
		}
		fmt.Printf("%s  ◆ testing   [%s]%s\n", pColorDim, codeModel, pColorReset)
		msgs := []interface{}{
			ollama.Message{Role: "system", Content: systemPrompt + testStageSuffix},
			ollama.Message{Role: "user", Content: testUserPrompt(userRequest, res.PlanText)},
		}
		var buf strings.Builder
		testIncr, testStop := progressTicker("testing")
		pt, ct, err := client.StreamChat(ctx, codeModel, msgs, nil, func(c string) { buf.WriteString(c); testIncr() })
		testElapsed := testStop()
		testCh <- stageOut{buf.String(), pt, ct, testElapsed, err}
	}()

	testOut := <-testCh

	if testOut.err == nil && strings.TrimSpace(testOut.text) != "" {
		res.TestText = testOut.text
		res.PromptTok += testOut.pt
		res.ComplTok += testOut.ct
		fmt.Printf("%s  ✓ tests ready  %s(%.1fs · %d tokens)%s\n", pColorGold, pColorDim, testOut.elapsed.Seconds(), testOut.pt+testOut.ct, pColorReset)
	}

	res.Combined = assemble(res.PlanText, res.CodeText, res.TestText)
	return res, nil
}

// ContinuePlan runs CODE+TESTS stages using a previously approved plan.
func ContinuePlan(
	ctx context.Context,
	client *ollama.Client,
	userRequest string,
	planText string,
	systemPrompt string,
	opts Options,
) (*Result, error) {
	res := &Result{PlanText: planText}
	codeModel := router.ModelFor(router.TierCode)

	// ── CODE stage ────────────────────────────────────────────────────────────
	fmt.Printf("%s  ◆ coding   [%s]%s\n", pColorDim, codeModel, pColorReset)

	maxRetries := opts.MaxRetries
	if maxRetries <= 0 {
		maxRetries = 3
	}

	codeMsgs := []interface{}{
		ollama.Message{Role: "system", Content: systemPrompt + codeStageSuffix},
		ollama.Message{Role: "user", Content: codeUserPrompt(userRequest, planText)},
	}

	var lastBuildErr string
	codeStart := time.Now()
	var codeTokTotal int
	for attempt := 0; attempt <= maxRetries; attempt++ {
		var codeBuf strings.Builder
		codeIncr, codeStop := progressTicker("coding")
		pt, ct, err := client.StreamChat(ctx, codeModel, codeMsgs, nil, func(c string) { codeBuf.WriteString(c); codeIncr() })
		codeStop()
		if err != nil {
			return nil, fmt.Errorf("code stage: %w", err)
		}
		res.CodeText = codeBuf.String()
		res.PromptTok += pt
		res.ComplTok += ct
		codeTokTotal += pt + ct

		if opts.Root == "" || attempt == maxRetries {
			break
		}
		written := writeCodeFiles(res.CodeText, opts.Root)
		if len(written) == 0 {
			break
		}
		buildResult := autofix.Check(opts.Root, written)
		if buildResult == nil || buildResult.Passed {
			break
		}
		buildErrStr := buildResult.Output
		if buildErrStr == lastBuildErr {
			errPreview := buildErrStr
			if len(errPreview) > 200 {
				errPreview = errPreview[:200] + "…"
			}
			fmt.Printf("%s  [stuck: same build error twice — stopping retry]%s\n", pColorDim, pColorReset)
			fmt.Printf("%s  %s%s\n", pColorDim, errPreview, pColorReset)
			break
		}
		lastBuildErr = buildErrStr
		fmt.Printf("%s  [build error — retry %d/%d]%s\n", pColorDim, attempt+1, maxRetries, pColorReset)
		retryMsg := fmt.Sprintf(
			"Build failed with the following error:\n\n```\n%s\n```\n\nFix only the affected function(s).",
			buildErrStr)
		if len(codeMsgs) > 2 {
			codeMsgs = codeMsgs[:2]
		}
		codeMsgs = append(codeMsgs,
			ollama.Message{Role: "assistant", Content: res.CodeText},
			ollama.Message{Role: "user", Content: retryMsg},
		)
	}
	codeElapsed := time.Since(codeStart)
	fmt.Printf("%s  ✓ code ready  %s(%.1fs · %d tokens)%s\n", pColorGold, pColorDim, codeElapsed.Seconds(), codeTokTotal, pColorReset)

	// ── TESTS stage ───────────────────────────────────────────────────────────
	type stageOut struct {
		text    string
		pt, ct  int
		elapsed time.Duration
		err     error
	}
	testCh := make(chan stageOut, 1)
	go func() {
		if opts.SkipTests {
			testCh <- stageOut{}
			return
		}
		fmt.Printf("%s  ◆ testing   [%s]%s\n", pColorDim, codeModel, pColorReset)
		msgs := []interface{}{
			ollama.Message{Role: "system", Content: systemPrompt + testStageSuffix},
			ollama.Message{Role: "user", Content: testUserPrompt(userRequest, planText)},
		}
		var buf strings.Builder
		testIncr, testStop := progressTicker("testing")
		pt, ct, err := client.StreamChat(ctx, codeModel, msgs, nil, func(c string) { buf.WriteString(c); testIncr() })
		testElapsed := testStop()
		testCh <- stageOut{buf.String(), pt, ct, testElapsed, err}
	}()
	testOut := <-testCh
	if testOut.err == nil && strings.TrimSpace(testOut.text) != "" {
		res.TestText = testOut.text
		res.PromptTok += testOut.pt
		res.ComplTok += testOut.ct
		fmt.Printf("%s  ✓ tests ready  %s(%.1fs · %d tokens)%s\n", pColorGold, pColorDim, testOut.elapsed.Seconds(), testOut.pt+testOut.ct, pColorReset)
	}

	res.Combined = assemble(res.PlanText, res.CodeText, res.TestText)
	return res, nil
}

// assemble stitches the three stage outputs into clean markdown.
func assemble(plan, code, tests string) string {
	var sb strings.Builder
	sb.WriteString("## Plan\n\n")
	sb.WriteString(strings.TrimSpace(stripStagePreamble(plan, "###")))
	sb.WriteString("\n\n---\n\n## Implementation\n\n")
	sb.WriteString(strings.TrimSpace(stripStagePreamble(code, "```")))
	if strings.TrimSpace(tests) != "" {
		sb.WriteString("\n\n---\n\n## Tests\n\n")
		sb.WriteString(strings.TrimSpace(stripStagePreamble(tests, "```")))
	}
	return sb.String()
}

// SaveOutput writes the full pipeline result to .mantis/last-pipeline.md for reference.
func SaveOutput(root, combined string) {
	dir := filepath.Join(root, ".mantis")
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(filepath.Join(dir, "last-pipeline.md"), []byte(combined), 0o644)
}

// CompactSummary returns a short CLI-friendly summary of the pipeline result.
// It extracts the plan overview + file list, omitting verbose sections like
// Architecture, Task Breakdown, Risks, and Assumptions.
func CompactSummary(res *Result) string {
	var sb strings.Builder

	// Extract plan overview (first ### section only).
	if overview := extractSection(res.PlanText, "Overview"); overview != "" {
		sb.WriteString(overview)
		sb.WriteString("\n")
	}

	// List files from the plan's "### Files" section.
	if files := extractSection(res.PlanText, "Files"); files != "" {
		sb.WriteString("\n### Files\n\n")
		sb.WriteString(files)
		sb.WriteString("\n")
	}

	// Count code files written.
	codeFiles := countCodeFences(res.CodeText)
	testFiles := countCodeFences(res.TestText)
	if codeFiles > 0 || testFiles > 0 {
		sb.WriteString("\n---\n\n")
		if codeFiles > 0 {
			sb.WriteString(fmt.Sprintf("**%d implementation file(s)** written\n", codeFiles))
		}
		if testFiles > 0 {
			sb.WriteString(fmt.Sprintf("**%d test file(s)** written\n", testFiles))
		}
		sb.WriteString(fmt.Sprintf("\n> Full output saved to `.mantis/last-pipeline.md`\n"))
	}

	return sb.String()
}

// extractSection pulls the content of a ### Section from plan text.
func extractSection(plan, heading string) string {
	marker := "### " + heading
	idx := strings.Index(plan, marker)
	if idx < 0 {
		return ""
	}
	content := plan[idx+len(marker):]
	// Find the next ### heading or end.
	nextIdx := strings.Index(content, "\n### ")
	if nextIdx >= 0 {
		content = content[:nextIdx]
	}
	return strings.TrimSpace(content)
}

// countCodeFences counts ```lang:filepath fenced blocks in text.
func countCodeFences(text string) int {
	re := regexp.MustCompile("(?m)^```[a-zA-Z0-9_+-]*[:/ ][^\\s`]+")
	return len(re.FindAllString(text, -1))
}

// stripStagePreamble removes any chain-of-thought preamble the model writes before
// the expected first token. For plan stages, content should start at "###". For
// code/test stages, content should start at "```". Anything before that first
// marker is stripped.
func stripStagePreamble(text, marker string) string {
	idx := strings.Index(text, marker)
	if idx <= 0 {
		return text
	}
	// Only strip if the preamble is prose (not code-fence context).
	before := text[:idx]
	// If the preamble contains a newline followed by the marker, it's likely
	// legitimate content (e.g. a code block inside a plan). Only strip if the
	// preamble is purely on the first few lines (less than 8 lines).
	lines := strings.Split(strings.TrimSpace(before), "\n")
	if len(lines) > 20 {
		return text
	}
	return text[idx:]
}

// ── Complexity detector ────────────────────────────────────────────────────────

func isComplexBuild(lower string) bool {
	buildVerbs := []string{"build", "create", "develop", "implement", "make", "write", "scaffold", "set up", "setup", "generate", "design"}
	hasBuild := false
	for _, v := range buildVerbs {
		if strings.Contains(lower, v) {
			hasBuild = true
			break
		}
	}

	// Complex signals always trigger pipeline regardless of build verb.
	for _, s := range complexSignals {
		if strings.Contains(lower, s) {
			return true
		}
	}

	if !hasBuild {
		return false
	}

	// Long requests (>15 words) with a build verb likely need planning.
	wordCount := len(strings.Fields(lower))
	if wordCount > 15 {
		return true
	}

	// Two or more distinct code-domain components present.
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

// writeCodeFiles extracts ```lang:filepath code blocks from text and writes
// them to disk under root. Returns the list of file paths written.
// Used by the agentic retry loop to verify a build after each CODE iteration.
func writeCodeFiles(text, root string) []string {
	re := regexp.MustCompile("(?m)^```[a-zA-Z0-9_+-]*[:/ ]([^\\s`]+)\\n([\\s\\S]*?)\\n```")
	var paths []string
	seen := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(text, -1) {
		if len(m) < 3 {
			continue
		}
		relPath := strings.TrimSpace(m[1])
		content := m[2]
		if relPath == "" || seen[relPath] {
			continue
		}
		if filepath.IsAbs(relPath) || strings.HasPrefix(filepath.Clean(relPath), "..") {
			continue
		}
		seen[relPath] = true
		dest := filepath.Join(root, filepath.Clean(relPath))
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			continue
		}
		if err := os.WriteFile(dest, []byte(content+"\n"), 0o644); err != nil {
			continue
		}
		paths = append(paths, dest)
	}
	return paths
}

// ── Stage system-prompt suffixes ──────────────────────────────────────────────
// Appended to the base system prompt so each stage inherits full project
// context + skills but gets precise, role-specific instructions.

const planStageSuffix = `

## Your role: ARCHITECT
Analyze the development request and produce a structured implementation plan.
Do NOT write any implementation code or file content — list file NAMES only.

CRITICAL: Begin your response IMMEDIATELY with "### Overview" — no preamble, no "Let me...", no "The user is...", no analysis sentences before the header. Jump straight to the structured output.

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
- Use ` + "```lang:filepath" + ` fences for EVERY file — source files AND config/manifest files.
- This explicitly includes: package.json, requirements.txt, Cargo.toml, go.mod, go.sum, Makefile, .env.example, tsconfig.json, vite.config.ts, docker-compose.yml, Dockerfile, pyproject.toml — EVERY file that must exist on disk.
- NEVER output JSON, YAML, TOML, or any config content as bare text or bare ` + "```json" + ` blocks. Every file MUST have a filepath: ` + "```json:backend/package.json" + `.
- Handle all error cases. No stubs. No TODOs left unimplemented.
- Validate inputs at boundaries. Return structured errors.

CRITICAL: Begin your response IMMEDIATELY with the first ` + "```lang:filepath" + ` code block. Do NOT write any preamble, "I'll implement...", "Let me create...", "I need to...", or any explanation before the first code block. Code first, nothing else before it.
`

const testStageSuffix = `

## Your role: TEST ENGINEER
Write comprehensive tests based on the implementation plan.
- Use ` + "```lang:filepath" + ` fences for every test file.
- Cover: happy path, edge cases, error cases, boundary values.
- Descriptive test names: test_<scenario>_<expected_behaviour>.
- Mock external dependencies. Test behaviour, not implementation.

CRITICAL: Begin your response IMMEDIATELY with the first ` + "```lang:filepath" + ` test file block. Do NOT write any preamble, "I'll create...", "Let me analyze...", or any sentences before the first code block.
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
			"Write the complete implementation now. Every file — including package.json, tsconfig.json, "+
			"requirements.txt, Makefile, .env.example, and any other config/manifest file — must use "+
			"`lang:filepath` fences (e.g. ```json:backend/package.json). No bare JSON or YAML blocks.",
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
