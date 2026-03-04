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
	"sync"
	"sync/atomic"
	"time"

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
			pt, ct, err := client.StreamChat(ctx, codeModel, codeMsgs, nil, func(c string) { codeBuf.WriteString(c); codeIncr() })
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

	// ── Stage 4: TEST VERIFICATION (optional) ────────────────────────────────
	// If files were written to disk, run the test suite and iteratively fix
	// failures. Only runs when root is set and a test runner is detected.
	if opts.Root != "" && !opts.SkipTests {
		runPipelineTestLoop(ctx, client, opts.Root, res)
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

	// ── Stage 4: TEST VERIFICATION (optional) ────────────────────────────────
	if opts.Root != "" && !opts.SkipTests {
		runPipelineTestLoop(ctx, client, opts.Root, res)
	}

	res.Combined = assemble(res.PlanText, res.CodeText, res.TestText)
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

// taskIcon returns the display icon and color for a task status.
func taskIcon(status string, spinFrame int) (string, string) {
	frames := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	switch status {
	case "running":
		return frames[spinFrame%len(frames)], pColorGold
	case "done":
		return "✓", "\033[38;5;70m"
	case "failed":
		return "✗", "\033[38;5;196m"
	default:
		return "○", pColorDim
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

// printTaskList renders the current task status to the terminal.
func printTaskList(tasks []Task) {
	for i := range tasks {
		icon, color := taskIcon(tasks[i].Status, 0)
		fmt.Printf("  %s%s %s%s%s\n", color, icon, tasks[i].Title, pColorReset, taskSuffix(&tasks[i]))
	}
}

// updateTaskLine reprints a single task line in-place using ANSI cursor movement.
func updateTaskLine(tasks []Task, idx, totalTasks, spinFrame int) {
	t := &tasks[idx]
	icon, color := taskIcon(t.Status, spinFrame)
	linesUp := totalTasks - t.ID + 1
	fmt.Printf("\033[%dA\r\033[K  %s%s %s%s%s\033[%dB\r",
		linesUp, color, icon, t.Title, pColorReset, taskSuffix(t), linesUp)
}

// taskSpinner starts a background goroutine that animates spinner icons
// on all "running" tasks. Returns a stop function.
func taskSpinner(tasks []Task, totalTasks int, mu *sync.Mutex) func() {
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
				for i := range tasks {
					if tasks[i].Status == "running" {
						updateTaskLine(tasks, i, totalTasks, frame)
					}
				}
				mu.Unlock()
			}
		}
	}()
	return func() { close(done) }
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
	var allCode strings.Builder
	var totalPT, totalCT int

	// Start the spinner animation for running tasks.
	stopSpinner := taskSpinner(tasks, len(tasks), &mu)
	defer stopSpinner()

	// defaultTaskTimeout is 8 minutes per task — generous but bounded.
	if taskTimeout <= 0 {
		taskTimeout = 8 * time.Minute
	}

	// runOneTask executes a single task and updates its status.
	runOneTask := func(i int, priorCode string) {
		mu.Lock()
		tasks[i].Status = "running"
		tasks[i].StartTime = time.Now()
		atomic.StoreInt64(&tasks[i].streamTok, 0)
		updateTaskLine(tasks, i, len(tasks), 0)
		mu.Unlock()

		// Each task gets its own deadline so a slow task doesn't starve siblings.
		taskCtx, taskCancel := context.WithTimeout(ctx, taskTimeout)
		defer taskCancel()

		taskPrompt := taskCodePrompt(userRequest, planText, tasks[i].Title, priorCode)
		msgs := []interface{}{
			ollama.Message{Role: "system", Content: systemPrompt + taskStageSuffix},
			ollama.Message{Role: "user", Content: taskPrompt},
		}

		var buf strings.Builder
		pt, ct, err := client.StreamChat(taskCtx, codeModel, msgs, nil, func(c string) {
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
					tasks[i].FileCount = len(extractAndApplyChanges(partial, root))
				}
			}
			tasks[i].Status = "failed"
			updateTaskLine(tasks, i, len(tasks), 0)
			return
		}

		code := buf.String()
		tasks[i].Output = code
		tasks[i].Status = "done"
		allCode.WriteString(code)
		allCode.WriteString("\n\n")
		totalPT += pt
		totalCT += ct

		// Write files to disk immediately so user sees progress.
		if root != "" {
			tasks[i].FileCount = len(extractAndApplyChanges(code, root))
		}
		updateTaskLine(tasks, i, len(tasks), 0)
	}

	// Phase 1: Run first task sequentially (setup/config — others depend on it).
	runOneTask(0, "")
	foundationCode := allCode.String()

	if len(tasks) > 1 {
		// Phase 2: Run remaining tasks in parallel batches.
		// Batch size limited to avoid overwhelming the API.
		const maxParallel = 3
		remaining := tasks[1:]
		for batchStart := 0; batchStart < len(remaining); batchStart += maxParallel {
			batchEnd := batchStart + maxParallel
			if batchEnd > len(remaining) {
				batchEnd = len(remaining)
			}
			batch := remaining[batchStart:batchEnd]

			// Snapshot prior code for this batch (all prior batches + foundation).
			mu.Lock()
			priorSnapshot := allCode.String()
			mu.Unlock()

			var wg sync.WaitGroup
			for j := range batch {
				taskIdx := 1 + batchStart + j // index into original tasks slice
				wg.Add(1)
				go func(idx int, prior string) {
					defer wg.Done()
					runOneTask(idx, prior)
				}(taskIdx, priorSnapshot)
			}
			wg.Wait()
		}
	}

	// Use foundation code if nothing else succeeded.
	_ = foundationCode

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
- If prior tasks already created files you need to import from, assume they exist.

CRITICAL: Begin IMMEDIATELY with code blocks. No preamble.
`

func taskCodePrompt(request, plan, taskTitle, priorCode string) string {
	var sb strings.Builder
	sb.WriteString("## Original Request\n")
	sb.WriteString(request)
	sb.WriteString("\n\n## Full Plan\n")
	sb.WriteString(plan)
	sb.WriteString("\n\n## YOUR CURRENT TASK\nImplement ONLY this task: **")
	sb.WriteString(taskTitle)
	sb.WriteString("**\n\nOutput only the files needed for this specific task.")

	if priorCode != "" {
		// Give context of what's already been implemented (file list only, not full content).
		fileRe := regexp.MustCompile("(?m)^```[a-zA-Z0-9_+-]*[:/ ]([^\\s`]+)")
		var priorFiles []string
		seen := map[string]bool{}
		for _, m := range fileRe.FindAllStringSubmatch(priorCode, -1) {
			if len(m) >= 2 && !seen[m[1]] {
				priorFiles = append(priorFiles, m[1])
				seen[m[1]] = true
			}
		}
		if len(priorFiles) > 0 {
			sb.WriteString("\n\n## Already implemented files (import from these, don't recreate):\n")
			for _, f := range priorFiles {
				sb.WriteString("- ")
				sb.WriteString(f)
				sb.WriteString("\n")
			}
		}
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

IMPORTANT: The "### Task Breakdown" section MUST use a numbered list of discrete, implementable tasks.
Each task should be a single sentence describing ONE unit of work. Example:
1. Set up project config files (package.json, tsconfig.json, .env.example)
2. Create database schema and models
3. Implement user authentication (register, login, JWT)
4. Build expense CRUD API endpoints
5. Implement debt simplification algorithm
6. Create frontend components and pages
7. Add error handling and validation

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
