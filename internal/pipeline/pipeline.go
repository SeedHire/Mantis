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
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/term"

	"github.com/seedhire/mantis/internal/agent"
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

// planOpts and codeOpts set temperature and context window per stage.
// Low temperature improves determinism for code; 32768 context fits most projects.
var (
	planOpts = &ollama.ModelOptions{Temperature: 0.3, NumCtx: 32768}
	codeOpts = &ollama.ModelOptions{Temperature: 0.15, NumCtx: 32768}
)

// doneVerbs is a pool of fun completion words, shown at the end of a pipeline run.
var doneVerbs = []string{
	"Forged", "Crafted", "Assembled", "Brewed", "Conjured",
	"Sculpted", "Engineered", "Woven", "Distilled", "Architected",
	"Synthesized", "Manifested", "Composed", "Rendered", "Minted",
	"Smelted", "Tempered", "Polished", "Hammered", "Galvanized",
	"Concocted", "Machined", "Devised", "Fashioned", "Chiseled",
	"Spun up", "Whipped up", "Cooked up", "Stitched", "Welded",
}

// printDoneLine prints a fun completion line with elapsed time.
func printDoneLine(elapsed time.Duration) {
	verb := doneVerbs[rand.Intn(len(doneVerbs))]
	var timeStr string
	if elapsed >= time.Minute {
		m := int(elapsed.Minutes())
		s := int(elapsed.Seconds()) % 60
		timeStr = fmt.Sprintf("%dm %ds", m, s)
	} else {
		timeStr = fmt.Sprintf("%.1fs", elapsed.Seconds())
	}
	fmt.Printf("\n%s  ✻ %s in %s%s\n", pColorDim, verb, timeStr, pColorReset)
}

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

	var once sync.Once
	stop = func() time.Duration {
		once.Do(func() { close(done) })
		return time.Since(start)
	}
	return
}

// Options controls pipeline execution behaviour.
type Options struct {
	AvailableModels []ollama.ModelInfo
	SkipTests       bool          // skip test generation for faster turnaround
	Root            string        // project root for build verification; empty = skip
	MaxRetries      int           // max CODE stage retries on build failure (default 3)
	PlanOnly        bool          // stop after PLAN stage and return for user approval
	TaskTimeout     time.Duration // per-task timeout (default 8m); 0 = no individual deadline
}

// Result holds aggregated pipeline output and token counts.
type Result struct {
	PlanText  string
	CodeText  string
	TestText  string
	Combined  string // assembled markdown, ready for renderResponse + extractAndWriteFiles
	PromptTok int
	ComplTok  int
	Tasks     []Task // parsed tasks with status (for TUI display)
}

// Task represents a single implementation task parsed from the plan.
type Task struct {
	ID        int
	Title     string
	Status    string // "pending", "running", "done", "failed"
	Output    string // generated code for this task
	FileCount int    // number of files written to disk for this task
	StartTime time.Time
	Elapsed   time.Duration
	Tokens    int   // final token count (prompt + completion)
	streamTok int64 // live streaming token counter (atomic)
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

	pipelineStart := time.Now()
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
	pt, ct, err := client.StreamChat(ctx, planModel, planMsgs, planOpts, func(c string) { planBuf.WriteString(c); planIncr() })
	planElapsed := planStop()
	if err != nil {
		// Plan model unavailable — fall back to code model so the pipeline still runs.
		fmt.Printf("%s  ⚠ plan model unavailable, falling back to %s%s\n", pColorDim, codeModel, pColorReset)
		planBuf.Reset()
		planIncr2, planStop2 := progressTicker("planning")
		pt, ct, err = client.StreamChat(ctx, codeModel, planMsgs, codeOpts, func(c string) { planBuf.WriteString(c); planIncr2() })
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
		printDoneLine(time.Since(pipelineStart))
		return res, nil
	}

	// ── Stage 2: CODE — task-based or monolithic ─────────────────────────────
	// Try to parse discrete tasks from the plan. If found, execute them
	// one-by-one with focused prompts and live TUI progress. Otherwise
	// fall back to the monolithic code stage.
	tasks := parseTasks(res.PlanText)

	if len(tasks) >= 2 {
		// Task-based execution: each task gets its own focused prompt.
		fmt.Printf("%s  ◆ coding   [%s] — %d tasks%s\n", pColorDim, codeModel, len(tasks), pColorReset)
		codeStart := time.Now()
		runTaskBased(ctx, client, codeModel, systemPrompt, userRequest, res.PlanText, tasks, res, opts.Root, opts.TaskTimeout)
		codeElapsed := time.Since(codeStart)

		done := 0
		for _, t := range tasks {
			if t.Status == "done" {
				done++
			}
		}
		fmt.Printf("%s  ✓ code ready  %s(%d/%d tasks · %.1fs · %d tokens)%s\n",
			pColorGold, pColorDim, done, len(tasks), codeElapsed.Seconds(), res.PromptTok+res.ComplTok, pColorReset)
	} else {
		// Monolithic fallback for plans without clear task breakdown.
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
			pt, ct, err := client.StreamChat(ctx, codeModel, codeMsgs, codeOpts, func(c string) { codeBuf.WriteString(c); codeIncr() })
			codeStop()
			if partial := codeBuf.String(); strings.TrimSpace(partial) != "" {
				res.CodeText = partial
			}
			if err != nil {
				if strings.Contains(err.Error(), "deadline exceeded") && len(res.CodeText) > 500 {
					res.PromptTok += pt
					res.ComplTok += ct
					codeTokTotal += pt + ct
					fmt.Printf("%s  ⚠ code stage timed out but captured %d chars of partial output%s\n",
						pColorDim, len(res.CodeText), pColorReset)
					break
				}
				return res, fmt.Errorf("code stage: %w", err)
			}
			res.CodeText = codeBuf.String()
			res.PromptTok += pt
			res.ComplTok += ct
			codeTokTotal += pt + ct

			if opts.Root == "" || attempt == maxRetries {
				break
			}
			written := extractAndApplyChanges(res.CodeText, opts.Root)
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
				"Build failed with the following error:\n\n```\n%s\n```\n\n"+
					"Fix only the affected function(s). Use ```edit:filepath blocks with <<<SEARCH/===/ >>>SEARCH markers "+
					"to patch existing files. Only use ```lang:filepath for brand new files.",
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
	}

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
		pt, ct, err := client.StreamChat(ctx, codeModel, msgs, codeOpts, func(c string) { buf.WriteString(c); testIncr() })
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

	// ── Stage 4: TEST VERIFICATION (optional) ────────────────────────────────
	// If files were written to disk, run the test suite and iteratively fix
	// failures. Only runs when root is set and a test runner is detected.
	if opts.Root != "" && !opts.SkipTests {
		runPipelineTestLoop(ctx, client, opts.Root, res)
	}

	res.Combined = assemble(res.PlanText, res.CodeText, res.TestText)
	printDoneLine(time.Since(pipelineStart))
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
	pipelineStart := time.Now()
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
		pt, ct, err := client.StreamChat(ctx, codeModel, codeMsgs, codeOpts, func(c string) { codeBuf.WriteString(c); codeIncr() })
		codeStop()
		if partial := codeBuf.String(); strings.TrimSpace(partial) != "" {
			res.CodeText = partial
		}
		if err != nil {
			if strings.Contains(err.Error(), "deadline exceeded") && len(res.CodeText) > 500 {
				res.PromptTok += pt
				res.ComplTok += ct
				codeTokTotal += pt + ct
				fmt.Printf("%s  ⚠ code stage timed out but captured %d chars of partial output%s\n",
					pColorDim, len(res.CodeText), pColorReset)
				break
			}
			return res, fmt.Errorf("code stage: %w", err)
		}
		res.CodeText = codeBuf.String()
		res.PromptTok += pt
		res.ComplTok += ct
		codeTokTotal += pt + ct

		if opts.Root == "" || attempt == maxRetries {
			break
		}
		written := extractAndApplyChanges(res.CodeText, opts.Root)
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
			"Build failed with the following error:\n\n```\n%s\n```\n\n"+
				"Fix only the affected function(s). Use ```edit:filepath blocks with <<<SEARCH/===/ >>>SEARCH markers.",
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
		pt, ct, err := client.StreamChat(ctx, codeModel, msgs, codeOpts, func(c string) { buf.WriteString(c); testIncr() })
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

	// ── Stage 4: TEST VERIFICATION (optional) ────────────────────────────────
	if opts.Root != "" && !opts.SkipTests {
		runPipelineTestLoop(ctx, client, opts.Root, res)
	}

	res.Combined = assemble(res.PlanText, res.CodeText, res.TestText)
	printDoneLine(time.Since(pipelineStart))
	return res, nil
}

// ── Task-based execution ──────────────────────────────────────────────────────

// parseTasks extracts individual tasks from the plan's "### Task Breakdown" section.
// Supports numbered lists (1. ...), bullet lists (- ...), and checkbox lists (- [ ] ...).
func parseTasks(planText string) []Task {
	section := extractSection(planText, "Task Breakdown")
	if section == "" {
		return nil
	}

	var tasks []Task
	taskRe := regexp.MustCompile(`(?m)^(?:\d+[\.\)]\s*|\-\s*(?:\[[ x]\]\s*)?|\*\s*)(.+)$`)
	for _, m := range taskRe.FindAllStringSubmatch(section, -1) {
		title := strings.TrimSpace(m[1])
		if title == "" {
			continue
		}
		// Skip sub-items (indented lines) — they're details, not top-level tasks.
		lineIdx := strings.Index(section, m[0])
		if lineIdx > 0 && (section[lineIdx-1] == ' ' || section[lineIdx-1] == '\t') {
			continue
		}
		tasks = append(tasks, Task{
			ID:     len(tasks) + 1,
			Title:  title,
			Status: "pending",
		})
	}
	return tasks
}

// taskIcon returns the display icon, icon color, and title color for a task status.
func taskIcon(status string, spinFrame int) (string, string, string) {
	frames := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	switch status {
	case "running":
		return frames[spinFrame%len(frames)], pColorGold, "\033[37m" // white title
	case "done":
		return "✓", "\033[38;5;70m", pColorDim // green check, gray title
	case "failed":
		return "✗", "\033[38;5;196m", "\033[38;5;196m" // red
	default:
		return "○", pColorDim, pColorDim // gray
	}
}

// taskSuffix builds the trailing info string (tokens, time, files) for a task line.
func taskSuffix(t *Task) string {
	switch t.Status {
	case "running":
		tok := atomic.LoadInt64(&t.streamTok)
		elapsed := time.Since(t.StartTime).Seconds()
		if tok > 0 {
			return fmt.Sprintf("  %s%d tokens · %.1fs%s", pColorDim, tok, elapsed, pColorReset)
		}
		return fmt.Sprintf("  %s%.1fs%s", pColorDim, elapsed, pColorReset)
	case "done":
		parts := []string{}
		if t.FileCount > 0 {
			parts = append(parts, fmt.Sprintf("%d files", t.FileCount))
		}
		if t.Tokens > 0 {
			parts = append(parts, fmt.Sprintf("%d tokens", t.Tokens))
		}
		if t.Elapsed > 0 {
			parts = append(parts, fmt.Sprintf("%.1fs", t.Elapsed.Seconds()))
		}
		if len(parts) > 0 {
			return fmt.Sprintf("  %s(%s)%s", pColorDim, strings.Join(parts, " · "), pColorReset)
		}
		return ""
	case "failed":
		if t.Elapsed > 0 {
			return fmt.Sprintf("  %s(%.1fs)%s", pColorDim, t.Elapsed.Seconds(), pColorReset)
		}
		return ""
	default:
		return ""
	}
}

// termWidth returns the current terminal width, defaulting to 80.
func termWidth() int {
	w, _, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || w <= 0 {
		return 80
	}
	return w
}

// truncateTitle shortens a task title so the full line fits in one terminal row.
// overhead accounts for "  X " prefix (4 chars) + suffix (up to ~30 chars).
func truncateTitle(title string, maxWidth int) string {
	// Reserve space for: "  X " prefix (4) + suffix stats (~35) + safety margin
	available := maxWidth - 40
	if available < 20 {
		available = 20
	}
	if len(title) <= available {
		return title
	}
	return title[:available-1] + "…"
}

// printTaskList renders the current task status to the terminal.
func printTaskList(tasks []Task) {
	w := termWidth()
	for i := range tasks {
		icon, iconColor, titleColor := taskIcon(tasks[i].Status, 0)
		title := truncateTitle(tasks[i].Title, w)
		fmt.Printf("  %s%s %s%s%s%s\n", iconColor, icon, titleColor, title, pColorReset, taskSuffix(&tasks[i]))
	}
}

// updateTaskLine reprints a single task line in-place using ANSI cursor movement.
// Cursor sits after the blank line below the task list, so we need +2:
// +1 for the target line itself, +1 for the blank line separator.
func updateTaskLine(tasks []Task, idx, totalTasks, spinFrame int) {
	if idx < 0 || idx >= len(tasks) || totalTasks <= 0 {
		return
	}
	t := &tasks[idx]
	icon, iconColor, titleColor := taskIcon(t.Status, spinFrame)
	title := truncateTitle(t.Title, termWidth())
	linesUp := totalTasks - t.ID + 2 // +2: blank line + 1-based ID offset
	fmt.Printf("\033[%dA\r\033[K  %s%s %s%s%s%s\033[%dB\r",
		linesUp, iconColor, icon, titleColor, title, pColorReset, taskSuffix(t), linesUp)
}

// taskSpinner starts a background goroutine that animates spinner icons
// on all "running" tasks. Returns a stop function.
// outMu protects stdout writes so ANSI escape sequences don't interleave
// between the spinner goroutine and the main task goroutines.
func taskSpinner(tasks []Task, totalTasks int, mu *sync.Mutex, outMu *sync.Mutex) func() {
	done := make(chan struct{})
	go func() {
		frame := 0
		ticker := time.NewTicker(120 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				frame++
				mu.Lock()
				outMu.Lock()
				for i := range tasks {
					if tasks[i].Status == "running" {
						updateTaskLine(tasks, i, totalTasks, frame)
					}
				}
				outMu.Unlock()
				mu.Unlock()
			}
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { close(done) }) }
}

// runTaskBased executes the code stage as individual task-by-task prompts
// instead of one monolithic generation. The first task (usually setup/config)
// runs sequentially, then remaining tasks run in parallel batches.
// The TUI shows live animated spinners on running tasks and writes files immediately.
func runTaskBased(
	ctx context.Context,
	client *ollama.Client,
	codeModel string,
	systemPrompt string,
	userRequest string,
	planText string,
	tasks []Task,
	res *Result,
	root string,
	taskTimeout time.Duration,
) {
	fmt.Printf("\n%s  ── tasks ──%s\n", pColorDim, pColorReset)
	printTaskList(tasks)
	fmt.Println() // blank line below task list for cursor math

	var mu sync.Mutex
	var outMu sync.Mutex // protects stdout ANSI writes from interleaving
	var allCode strings.Builder
	var totalPT, totalCT int
	var allWrittenFiles []string // tracks all files written across tasks

	// Start the spinner animation for running tasks.
	stopSpinner := taskSpinner(tasks, len(tasks), &mu, &outMu)
	defer stopSpinner()

	// defaultTaskTimeout is 8 minutes per task — generous but bounded.
	if taskTimeout <= 0 {
		taskTimeout = 8 * time.Minute
	}

	// runOneTask executes a single task and updates its status.
	runOneTask := func(i int, writtenFiles []string) {
		mu.Lock()
		tasks[i].Status = "running"
		tasks[i].StartTime = time.Now()
		atomic.StoreInt64(&tasks[i].streamTok, 0)
		outMu.Lock()
		updateTaskLine(tasks, i, len(tasks), 0)
		outMu.Unlock()
		mu.Unlock()

		// Each task gets its own deadline so a slow task doesn't starve siblings.
		taskCtx, taskCancel := context.WithTimeout(ctx, taskTimeout)
		defer taskCancel()

		taskPrompt := taskCodePrompt(userRequest, planText, tasks[i].Title, root, writtenFiles)
		msgs := []interface{}{
			ollama.Message{Role: "system", Content: systemPrompt + taskStageSuffix},
			ollama.Message{Role: "user", Content: taskPrompt},
		}

		var buf strings.Builder
		pt, ct, err := client.StreamChat(taskCtx, codeModel, msgs, codeOpts, func(c string) {
			buf.WriteString(c)
			atomic.AddInt64(&tasks[i].streamTok, 1)
		})

		mu.Lock()
		defer mu.Unlock()

		tasks[i].Elapsed = time.Since(tasks[i].StartTime)
		tasks[i].Tokens = pt + ct

		if err != nil {
			if partial := buf.String(); strings.TrimSpace(partial) != "" {
				tasks[i].Output = partial
				allCode.WriteString(partial)
				allCode.WriteString("\n\n")
				if root != "" {
					written := extractAndApplyChanges(partial, root)
					tasks[i].FileCount = len(written)
					allWrittenFiles = append(allWrittenFiles, written...)
				}
			}
			tasks[i].Status = "failed"
			outMu.Lock()
			updateTaskLine(tasks, i, len(tasks), 0)
			outMu.Unlock()
			return
		}

		code := buf.String()
		tasks[i].Output = code
		allCode.WriteString(code)
		allCode.WriteString("\n\n")
		totalPT += pt
		totalCT += ct

		// Write files to disk immediately so user sees progress.
		var written []string
		if root != "" {
			written = extractAndApplyChanges(code, root)
			tasks[i].FileCount = len(written)
			allWrittenFiles = append(allWrittenFiles, written...)
		}

		// Iterative fix loop: build check + content validation, up to 3 retries.
		// Mirrors the monolithic path: retry with error context, stuck detection
		// (same error twice = stop), always try to fix rather than fail.
		if root != "" && len(written) > 0 {
			const maxTaskRetries = 3
			var lastBuildErr string
			lastCode := code

			for attempt := 0; attempt < maxTaskRetries; attempt++ {
				// Check build.
				buildResult := autofix.Check(root, written)
				buildPassed := buildResult == nil || buildResult.Passed

				// Check content quality.
				valWarnings, stubRatio := validateContent(written)
				qualityOK := stubRatio <= 0.3

				if buildPassed && qualityOK {
					break // all good
				}

				// Build the retry prompt with all issues.
				var retryReason strings.Builder
				if !buildPassed {
					buildErrStr := buildResult.Output
					// Stuck detection: same build error twice means model can't fix it.
					if buildErrStr == lastBuildErr {
						errPreview := buildErrStr
						if len(errPreview) > 200 {
							errPreview = errPreview[:200] + "…"
						}
						fmt.Printf("%s  [task %d: stuck on same error — moving on]%s\n", pColorDim, tasks[i].ID, pColorReset)
						fmt.Printf("%s  %s%s\n", pColorDim, errPreview, pColorReset)
						break
					}
					lastBuildErr = buildErrStr
					retryReason.WriteString(fmt.Sprintf("Build failed:\n```\n%s\n```\n\n", buildErrStr))
				}
				if !qualityOK && len(valWarnings) > 0 {
					retryReason.WriteString("Code quality issues found:\n")
					for _, w := range valWarnings {
						retryReason.WriteString("- " + w + "\n")
					}
					retryReason.WriteString("\nReplace ALL placeholders and stubs with real implementations.\n")
				}

				fmt.Printf("%s  [task %d: fixing issues — attempt %d/%d]%s\n",
					pColorDim, tasks[i].ID, attempt+1, maxTaskRetries, pColorReset)

				retryMsgs := []interface{}{
					msgs[0], msgs[1], // system + original user prompt
					ollama.Message{Role: "assistant", Content: lastCode},
					ollama.Message{Role: "user", Content: retryReason.String() +
						"Fix ONLY the broken parts. Use ```edit:filepath blocks with <<<SEARCH/===/>>>SEARCH markers for existing files. " +
						"Only use ```lang:filepath for brand new files."},
				}

				var retryBuf strings.Builder
				rpt, rct, rerr := client.StreamChat(taskCtx, codeModel, retryMsgs, codeOpts, func(c string) {
					retryBuf.WriteString(c)
					atomic.AddInt64(&tasks[i].streamTok, 1)
				})
				if rerr != nil || strings.TrimSpace(retryBuf.String()) == "" {
					break // stream failed, keep what we have
				}

				retryCode := retryBuf.String()
				retryWritten := extractAndApplyChanges(retryCode, root)
				allWrittenFiles = append(allWrittenFiles, retryWritten...)
				tasks[i].FileCount += len(retryWritten)
				allCode.WriteString(retryCode)
				allCode.WriteString("\n\n")
				totalPT += rpt
				totalCT += rct
				tasks[i].Tokens += rpt + rct
				lastCode = retryCode

				// Update written files list for next iteration's check.
				if len(retryWritten) > 0 {
					written = append(written, retryWritten...)
				}
			}
		}

		tasks[i].Status = "done"
		outMu.Lock()
		updateTaskLine(tasks, i, len(tasks), 0)
		outMu.Unlock()
	}

	// Phase 1: Run first two tasks sequentially.
	// Task 0: project setup (package.json, config, tsconfig)
	// Task 1: data models / shared types — all parallel tasks depend on these.
	// Running them sequentially ensures stable type definitions before parallel work starts.
	seqCount := 2
	if len(tasks) < seqCount {
		seqCount = len(tasks)
	}

	for i := 0; i < seqCount; i++ {
		mu.Lock()
		snapshot := make([]string, len(allWrittenFiles))
		copy(snapshot, allWrittenFiles)
		mu.Unlock()
		runOneTask(i, snapshot)

		// Install dependencies after the first task (setup/config task).
		if i == 0 && root != "" {
			installDeps(ctx, root)
		}
	}

	if len(tasks) > seqCount {
		// Phase 2: Run remaining tasks in parallel batches.
		// Batch size limited to avoid overwhelming the API.
		const maxParallel = 3
		remaining := tasks[seqCount:]
		for batchStart := 0; batchStart < len(remaining); batchStart += maxParallel {
			batchEnd := batchStart + maxParallel
			if batchEnd > len(remaining) {
				batchEnd = len(remaining)
			}
			batch := remaining[batchStart:batchEnd]

			// Snapshot written files for this batch.
			mu.Lock()
			snapshot := make([]string, len(allWrittenFiles))
			copy(snapshot, allWrittenFiles)
			mu.Unlock()

			var wg sync.WaitGroup
			for j := range batch {
				taskIdx := seqCount + batchStart + j // index into original tasks slice
				wg.Add(1)
				go func(idx int, files []string) {
					defer wg.Done()
					runOneTask(idx, files)
				}(taskIdx, snapshot)
			}
			wg.Wait()
		}
	}

	res.PromptTok += totalPT
	res.ComplTok += totalCT
	res.CodeText = allCode.String()
	res.Tasks = tasks
}

const taskStageSuffix = `

## Your role: IMPLEMENTER (single task)
You are implementing ONE specific task from a larger plan.
- Output ONLY the files needed for THIS task — do not implement other tasks.
- For NEW files: use ` + "```lang:filepath" + ` fences with full content.
- For EXISTING files: use ` + "```edit:filepath" + ` fences with SEARCH/REPLACE markers:
  ` + "```edit:path/to/file.go" + `
  <<<SEARCH
  exact old text
  ===
  exact new text
  >>>SEARCH
  ` + "```" + `
- Never output the full content of an existing file — only the changed sections.
- Include config/manifest files ONLY if this is the setup/config task.
- Handle all error cases. No stubs. No TODOs.
- If prior tasks already created files you need to import from, assume they exist and IMPORT from them.

CRITICAL RULES to avoid breaking parallel tasks:
1. NEVER redefine interfaces, types, or enums that belong to a prior task's files — only IMPORT them.
2. NEVER recreate a file that the "Already implemented files" list shows — only EDIT it if you must.
3. Use CONSISTENT constructor signatures: if prior tasks use dependency injection (passing db/repo as args), do the same.
4. If you need a type from another file, import it — do NOT copy-paste or redefine it inline.

ABSOLUTE RULES — violation means the code is REJECTED:
1. Every function body MUST have real logic — never return nil/undefined as a stub.
2. Every import MUST match exact exports shown in "Already implemented files" below.
3. If referencing a type from a prior file, use the EXACT name and field names shown.
4. NEVER write "// TODO", "// FIXME", "throw new Error('not implemented')", or any placeholder.
5. If you cannot fully implement something, OMIT it entirely rather than stubbing it.
6. Route files MUST wire to actual controller methods — NEVER use res.status(501) or "not implemented" placeholders.
7. When prior files show 'export default new ClassName()', import the DEFAULT EXPORT (the instance), do NOT call static methods on the class.
8. Use the EXACT property names, constructor parameters, and method signatures shown in the "Exports" summary — do NOT invent alternatives.

CRITICAL: Begin IMMEDIATELY with code blocks. No preamble.
`

// readPriorContext reads actual file content from disk for files written by prior tasks.
// This gives downstream tasks exact type signatures, interfaces, and exports to import from.
// Prioritizes model/type files first. Caps total output at maxChars.
func readPriorContext(root string, writtenFiles []string, maxChars int) string {
	if root == "" || len(writtenFiles) == 0 {
		return ""
	}

	// Sort: prioritize files with model/type/interface in path.
	type fileEntry struct {
		path     string
		priority int
	}
	entries := make([]fileEntry, 0, len(writtenFiles))
	for _, f := range writtenFiles {
		lower := strings.ToLower(f)
		p := 0
		for _, kw := range []string{"model", "type", "interface", "schema", "entity"} {
			if strings.Contains(lower, kw) {
				p = 1
				break
			}
		}
		entries = append(entries, fileEntry{path: f, priority: p})
	}
	// Stable sort: priority files first.
	for i := 1; i < len(entries); i++ {
		for j := i; j > 0 && entries[j].priority > entries[j-1].priority; j-- {
			entries[j], entries[j-1] = entries[j-1], entries[j]
		}
	}

	var sb strings.Builder
	sb.WriteString("\n\n## Already implemented files (import from these, don't recreate):\n")
	totalChars := 0
	const maxLinesPerFile = 60

	for _, e := range entries {
		if totalChars >= maxChars {
			break
		}
		absPath := e.path
		if !filepath.IsAbs(absPath) {
			absPath = filepath.Join(root, absPath)
		}

		// Skip binary/generated/vendor paths.
		lower := strings.ToLower(absPath)
		if strings.Contains(lower, "node_modules") || strings.Contains(lower, ".git/") {
			continue
		}

		data, err := os.ReadFile(absPath)
		if err != nil {
			continue
		}
		content := string(data)

		// Skip likely binary files.
		if len(content) > 0 && strings.ContainsRune(content[:min(len(content), 512)], 0) {
			continue
		}

		// Take first N lines.
		lines := strings.SplitN(content, "\n", maxLinesPerFile+1)
		if len(lines) > maxLinesPerFile {
			lines = lines[:maxLinesPerFile]
		}
		snippet := strings.Join(lines, "\n")

		// Determine relative path for display.
		relPath := e.path
		if filepath.IsAbs(relPath) {
			if rel, err := filepath.Rel(root, relPath); err == nil {
				relPath = rel
			}
		}

		exportSummary := extractExportSummary(content)
		block := fmt.Sprintf("\n### %s\n```\n%s\n```\n%s", relPath, snippet, exportSummary)
		if totalChars+len(block) > maxChars {
			break
		}
		sb.WriteString(block)
		totalChars += len(block)
	}

	return sb.String()
}

// exportRe matches lines that export symbols in JS/TS/Go/Python.
var exportRe = regexp.MustCompile(`(?m)^(?:export\s|func\s+[A-Z]|type\s+[A-Z]|class\s+\w|def\s+\w)`)

// extractExportSummary scans file content for export/public declarations
// and returns a formatted summary. Helps downstream tasks use exact names.
func extractExportSummary(content string) string {
	lines := strings.Split(content, "\n")
	var exports []string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if exportRe.MatchString(trimmed) {
			exports = append(exports, trimmed)
			if len(exports) >= 20 {
				break
			}
		}
	}
	if len(exports) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("**Exports (use these exact names):**\n")
	for _, e := range exports {
		sb.WriteString("- `")
		sb.WriteString(e)
		sb.WriteString("`\n")
	}
	sb.WriteString("\n")
	return sb.String()
}

func taskCodePrompt(request, plan, taskTitle, root string, writtenFiles []string) string {
	var sb strings.Builder
	sb.WriteString("## Original Request\n")
	sb.WriteString(request)
	sb.WriteString("\n\n## Full Plan\n")
	sb.WriteString(plan)
	sb.WriteString("\n\n## YOUR CURRENT TASK\nImplement ONLY this task: **")
	sb.WriteString(taskTitle)
	sb.WriteString("**\n\nOutput only the files needed for this specific task.")

	priorCtx := readPriorContext(root, writtenFiles, 12000)
	if priorCtx != "" {
		sb.WriteString(priorCtx)
	}
	return sb.String()
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
// Returns an error if the write fails; callers should surface this to the user.
func SaveOutput(root, combined string) error {
	dir := filepath.Join(root, ".mantis")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "last-pipeline.md"), []byte(combined), 0o644)
}

// CompactSummary returns a short CLI-friendly summary of the pipeline result.
// It extracts the plan overview + file list, omitting verbose sections like
// Architecture, Task Breakdown, Risks, and Assumptions.
// When task-based execution was used, it includes per-task status.
func CompactSummary(res *Result) string {
	var sb strings.Builder

	// Extract plan overview (first ### section only).
	if overview := extractSection(res.PlanText, "Overview"); overview != "" {
		sb.WriteString(overview)
		sb.WriteString("\n")
	}

	// List files from the plan's "### Files" section (capped at 20 for readability).
	if files := extractSection(res.PlanText, "Files"); files != "" {
		lines := strings.Split(strings.TrimSpace(files), "\n")
		const maxFileLines = 20
		sb.WriteString("\n### Files\n\n")
		shown := 0
		for _, l := range lines {
			if strings.TrimSpace(l) == "" {
				continue
			}
			if shown >= maxFileLines {
				sb.WriteString(fmt.Sprintf("  … and %d more (see .mantis/last-pipeline.md)\n", len(lines)-maxFileLines))
				break
			}
			sb.WriteString(l)
			sb.WriteString("\n")
			shown++
		}
	}

	// Task-based execution: show per-task breakdown.
	if len(res.Tasks) > 0 {
		sb.WriteString("\n---\n\n### Tasks\n\n")
		done, failed, totalFiles := 0, 0, 0
		for _, t := range res.Tasks {
			switch t.Status {
			case "done":
				done++
				sb.WriteString(fmt.Sprintf("- ✓ %s", t.Title))
			case "failed":
				failed++
				sb.WriteString(fmt.Sprintf("- ✗ %s", t.Title))
			default:
				sb.WriteString(fmt.Sprintf("- ○ %s", t.Title))
			}
			if t.FileCount > 0 {
				sb.WriteString(fmt.Sprintf(" (%d files)", t.FileCount))
				totalFiles += t.FileCount
			}
			sb.WriteString("\n")
		}
		sb.WriteString(fmt.Sprintf("\n**%d/%d tasks completed** · **%d file(s)** written\n", done, len(res.Tasks), totalFiles))
		if failed > 0 {
			sb.WriteString(fmt.Sprintf("⚠ %d task(s) failed\n", failed))
		}
		sb.WriteString(fmt.Sprintf("\n> Full output saved to `.mantis/last-pipeline.md`\n"))
	} else {
		// Monolithic path: count code fences.
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

// ── Stage 4: Test verification ─────────────────────────────────────────────────

// runPipelineTestLoop runs the iterative test loop after pipeline code generation.
// It auto-detects the test runner and attempts to fix any failing tests.
func runPipelineTestLoop(ctx context.Context, client *ollama.Client, root string, res *Result) {
	runner, _ := agent.DetectTestRunner(root)
	if runner == agent.RunnerUnknown {
		return // no test runner detected — skip silently
	}

	fmt.Printf("%s  ◆ verifying   [test loop]%s\n", pColorDim, pColorReset)

	toolkit := agent.NewToolkit(root, nil, nil)
	tl := &agent.TestLoop{
		Toolkit: toolkit,
		Client:  client,
		Root:    root,
		MaxIter: 3, // keep pipeline test loop tighter than standalone
	}

	loopCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	result, err := tl.Run(loopCtx)
	if err != nil {
		fmt.Printf("%s  ⚠ test loop error: %v%s\n", pColorDim, err, pColorReset)
		return
	}

	if result.Passed {
		fmt.Printf("%s  ✓ tests verified  %s(%d iteration(s))%s\n", pColorGold, pColorDim, result.Iterations, pColorReset)
	} else {
		fmt.Printf("%s  ⚠ tests still failing after %d iteration(s)%s\n", pColorDim, result.Iterations, pColorReset)
		if result.StuckReason != "" {
			fmt.Printf("%s    %s%s\n", pColorDim, result.StuckReason, pColorReset)
		}
	}
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

// validateContent scans written files for placeholder/stub code.
// Returns warnings and a stubRatio (files with issues / total files).
func validateContent(files []string) (warnings []string, stubRatio float64) {
	if len(files) == 0 {
		return nil, 0
	}

	placeholderPatterns := []string{
		"// TODO", "# TODO", "// FIXME", "# FIXME",
		"// placeholder", "// implement me", "# implement me",
		"throw new Error(\"not implemented\")", "throw new Error('not implemented')",
		"NotImplementedError", "pass  # stub", "pass # stub",
		"not yet implemented", "not implemented yet",
	}

	// 501 patterns only checked in route/controller/handler files.
	stub501Patterns := []string{
		"res.status(501)", "status(501)",
	}
	stubPatterns := []string{
		"return nil", "return undefined", "return {}", "return []",
		"return null", "return None",
	}

	issueCount := 0
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		content := strings.ToLower(string(data))
		hasIssue := false
		for _, p := range placeholderPatterns {
			if strings.Contains(content, strings.ToLower(p)) {
				warnings = append(warnings, fmt.Sprintf("%s: contains '%s'", filepath.Base(f), p))
				hasIssue = true
				break
			}
		}
		// Check 501 patterns only in route/controller/handler files.
		if !hasIssue {
			baseLower := strings.ToLower(filepath.Base(f))
			if strings.Contains(baseLower, "route") || strings.Contains(baseLower, "controller") || strings.Contains(baseLower, "handler") {
				for _, p := range stub501Patterns {
					if strings.Contains(content, strings.ToLower(p)) {
						warnings = append(warnings, fmt.Sprintf("%s: contains stub '%s'", filepath.Base(f), p))
						hasIssue = true
						break
					}
				}
			}
		}
		if !hasIssue {
			// Check for stub-only functions (very short functions that just return).
			for _, p := range stubPatterns {
				if strings.Count(content, strings.ToLower(p)) > 2 {
					warnings = append(warnings, fmt.Sprintf("%s: multiple stub returns ('%s')", filepath.Base(f), p))
					hasIssue = true
					break
				}
			}
		}
		if hasIssue {
			issueCount++
		}
	}
	stubRatio = float64(issueCount) / float64(len(files))
	return
}

// installDeps runs dependency installation after the setup task completes.
// Best effort — logs but doesn't fail the pipeline on install error.
func installDeps(ctx context.Context, root string) {
	type depCmd struct {
		check   string // file that must exist
		noDir   string // directory that must NOT exist (skip if present)
		cmd     string
		args    []string
		timeout time.Duration
	}

	cmds := []depCmd{
		{"package.json", "node_modules", "npm", []string{"install", "--prefer-offline"}, 60 * time.Second},
		{"go.mod", "", "go", []string{"mod", "tidy"}, 30 * time.Second},
		{"requirements.txt", "", "pip", []string{"install", "-r", "requirements.txt"}, 60 * time.Second},
		{"pyproject.toml", "", "pip", []string{"install", "-e", "."}, 60 * time.Second},
	}

	for _, dc := range cmds {
		checkPath := filepath.Join(root, dc.check)
		if _, err := os.Stat(checkPath); err != nil {
			continue
		}
		if dc.noDir != "" {
			if _, err := os.Stat(filepath.Join(root, dc.noDir)); err == nil {
				continue // already installed
			}
		}
		fmt.Printf("%s  ◆ installing dependencies...%s\n", pColorDim, pColorReset)

		ctx, cancel := context.WithTimeout(ctx, dc.timeout)
		cmd := execCommand(ctx, dc.cmd, dc.args...)
		cmd.Dir = root
		out, err := cmd.CombinedOutput()
		cancel()
		if err != nil {
			preview := string(out)
			if len(preview) > 200 {
				preview = preview[:200] + "…"
			}
			fmt.Printf("%s  ⚠ install warning: %s%s\n", pColorDim, preview, pColorReset)
		}
		break // run first matching installer, then check post-install hooks
	}

	// Post-install: Prisma generate if schema exists.
	schemaPath := filepath.Join(root, "prisma", "schema.prisma")
	if _, err := os.Stat(schemaPath); err == nil {
		fmt.Printf("%s  ◆ validating Prisma schema...%s\n", pColorDim, pColorReset)
		vCtx, vCancel := context.WithTimeout(ctx, 30*time.Second)
		vCmd := execCommand(vCtx, "npx", "prisma", "validate")
		vCmd.Dir = root
		vOut, vErr := vCmd.CombinedOutput()
		vCancel()
		if vErr != nil {
			preview := string(vOut)
			if len(preview) > 200 {
				preview = preview[:200] + "…"
			}
			fmt.Printf("%s  ⚠ prisma validate warning: %s%s\n", pColorDim, preview, pColorReset)
		}

		fmt.Printf("%s  ◆ generating Prisma client...%s\n", pColorDim, pColorReset)
		gCtx, gCancel := context.WithTimeout(ctx, 60*time.Second)
		gCmd := execCommand(gCtx, "npx", "prisma", "generate")
		gCmd.Dir = root
		gOut, gErr := gCmd.CombinedOutput()
		gCancel()
		if gErr != nil {
			preview := string(gOut)
			if len(preview) > 200 {
				preview = preview[:200] + "…"
			}
			fmt.Printf("%s  ⚠ prisma generate warning: %s%s\n", pColorDim, preview, pColorReset)
		}
	}
}

// execCommand wraps exec.CommandContext so tests can stub it.
var execCommand = exec.CommandContext

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

IMPORTANT: The "### Task Breakdown" section MUST use a numbered list of 6–10 discrete, implementable tasks.
Keep task count LOW — group related files into one task (e.g. "all models", "all repositories", "all services").
NEVER create separate tasks for each individual file — that causes parallel conflicts.

The first two tasks MUST be:
1. Project setup (config, package.json, tsconfig, env, docker)
2. Data models and types (all shared interfaces, enums, type definitions — nothing else imports from this)

Remaining tasks are grouped by layer, and each must explicitly import from task 2's types:
3. Database migrations and schema
4. All repositories (data access layer — imports types from task 2)
5. All services (business logic — imports repos from task 4, types from task 2)
6. API controllers and routes (imports services from task 5)
7. Middleware, validation, error handling
8. Tests and documentation

Maximum 10 tasks. If you need more, merge them.

### Framework rules:
- TypeScript: tsconfig.json MUST NOT enable "noUnusedLocals" or "noUnusedParameters" (causes cascading errors in Express/middleware handlers).
- Prisma: if using Prisma, all @relation fields with multiple references to the same model MUST have explicit relation names.

### Risks & Edge Cases
### Assumptions
`

const codeStageSuffix = `

## Your role: IMPLEMENTER
Write the complete, production-ready implementation based on the plan.

### File output formats:
- For NEW files: use ` + "```lang:filepath" + ` fences with full content.
- For EXISTING files: use ` + "```edit:filepath" + ` fences with SEARCH/REPLACE markers:
  ` + "```edit:path/to/file.go" + `
  <<<SEARCH
  exact old text to find
  ===
  exact replacement text
  >>>SEARCH
  ` + "```" + `
  Multiple <<<SEARCH...>>>SEARCH sections per block are allowed for multiple edits to the same file.
- NEVER output the full content of an existing file. Only output the changed sections using edit blocks.
- Config/manifest files (package.json, tsconfig.json, etc.) that are NEW use ` + "```lang:filepath" + ` with full content.
- Handle all error cases. No stubs. No TODOs left unimplemented.
- Validate inputs at boundaries. Return structured errors.

CRITICAL: Begin your response IMMEDIATELY with the first code block. Do NOT write any preamble.
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
			"Write the implementation now. For NEW files use `lang:filepath` fences. "+
			"For EXISTING files use `edit:filepath` fences with <<<SEARCH/===/>>>SEARCH markers. "+
			"Never output the full content of an existing file.",
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
