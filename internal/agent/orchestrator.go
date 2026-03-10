package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/seedhire/mantis/internal/intel"
	"github.com/seedhire/mantis/internal/ollama"
	"github.com/seedhire/mantis/internal/router"
)

const (
	scratchFile   = "AGENT_SCRATCH.json"
	workerMaxIter = 5 // hard cap on tool-call iterations per worker
)

// WorkerResult is the output of a single worker agent.
type WorkerResult struct {
	Package string `json:"package"`
	Summary string `json:"summary"`
	Code    string `json:"code"`
	Err     string `json:"error,omitempty"` // non-empty on failure
}

// agentScratch is the shared state file written by workers and read by the synthesizer.
type agentScratch struct {
	Task    string                  `json:"task"`
	Workers map[string]WorkerResult `json:"workers"`
}

// ShouldRunMultiAgent returns true when impact analysis suggests multi-agent is beneficial.
// Gate: TotalFiles >= 4 AND distinct packages >= 2.
func ShouldRunMultiAgent(impact *intel.ImpactResult) bool {
	if impact == nil {
		return false
	}
	return impact.TotalFiles >= 4 && distinctPackages(impact) >= 2
}

// DistinctPackages returns the count of unique directory paths across all impacted nodes.
func DistinctPackages(impact *intel.ImpactResult) int {
	return distinctPackages(impact)
}

// distinctPackages returns the count of unique directory paths across all impacted nodes.
func distinctPackages(impact *intel.ImpactResult) int {
	seen := map[string]struct{}{}
	for _, nodes := range impact.ByDepth {
		for _, n := range nodes {
			seen[filepath.Dir(n.FilePath)] = struct{}{}
		}
	}
	return len(seen)
}

// Run executes the multi-agent fan-out pipeline:
//
//	Step 1: Orchestrator (TierReason) decomposes task into per-package sub-tasks.
//	Step 2: Workers (TierCode) run in parallel goroutines, one per package.
//	Step 3: Synthesizer (TierReason) assembles worker results into a unified output.
//
// Returns the synthesized combined output (markdown).
func Run(
	ctx context.Context,
	task string,
	impact *intel.ImpactResult,
	toolkit *AgentToolkit,
	client *ollama.Client,
	systemPrompt string,
	scratchDir string,
) (string, error) {
	planModel := router.ModelFor(router.TierReason)
	codeModel := router.ModelFor(router.TierCode)

	packages := collectPackages(impact)
	if len(packages) == 0 {
		return "", fmt.Errorf("no packages found in impact analysis")
	}

	// ── Step 1: Orchestrate ───────────────────────────────────────────────────
	fmt.Printf("\033[38;5;244m  ◆ orchestrating [%s] across %d packages\033[0m\n",
		planModel, len(packages))

	decomposition, err := orchestrate(ctx, client, planModel, systemPrompt, task, packages)
	if err != nil {
		return "", fmt.Errorf("orchestrate: %w", err)
	}

	// ── Step 1b: Self-critique ────────────────────────────────────────────────
	// Before spawning workers, ask the planner to review its own decomposition
	// for circular dependencies, missing packages, or imbalanced sub-tasks.
	// This is Phase 2 of the OPENDEV ReAct loop: evaluate before acting.
	decomposition = critiqueDecomposition(ctx, client, planModel, systemPrompt, task, packages, decomposition)

	// ── Step 1c: Build dependency DAG ────────────────────────────────────────
	// Ask the planner which sub-tasks depend on others to prevent parallel conflicts.
	// arXiv 2511.10037: global DAG planning outperforms greedy ReAct on multi-tool tasks.
	dag := buildTaskDAG(ctx, client, planModel, systemPrompt, task, decomposition)

	// ── Step 2: Fan-out workers (DAG-ordered) ─────────────────────────────────
	// Execute independent tasks in parallel, dependent tasks wait for prerequisites.
	fmt.Printf("\033[38;5;244m  ◆ spawning %d worker(s) [%s] (DAG-ordered)\033[0m\n",
		len(decomposition), codeModel)

	scratch := &agentScratch{
		Task:    task,
		Workers: map[string]WorkerResult{},
	}
	var mu sync.Mutex

	levels := topoSort(decomposition, dag) // [][]string — each level runs in parallel
	for levelIdx, level := range levels {
		var wg sync.WaitGroup
		fmt.Printf("\033[38;5;244m    · level %d: %s\033[0m\n", levelIdx+1, strings.Join(level, ", "))
		for _, pkg := range level {
			subTask := decomposition[pkg]
			wg.Add(1)
			go func(pkg, subTask string) {
				defer wg.Done()
				result := runWorker(ctx, client, codeModel, systemPrompt, task, pkg, subTask, toolkit)
				mu.Lock()
				scratch.Workers[pkg] = result
				mu.Unlock()
			}(pkg, subTask)
		}
		wg.Wait() // wait for entire level before starting the next
	}

	// Persist scratch to disk (best-effort — used for debugging).
	writeScratch(scratchDir, scratch)

	// ── Step 3: Synthesize ────────────────────────────────────────────────────
	fmt.Printf("\033[38;5;244m  ◆ synthesizing [%s]\033[0m\n", planModel)

	combined, err := synthesize(ctx, client, planModel, systemPrompt, task, scratch)
	if err != nil {
		// Fall back to concatenating worker outputs.
		combined = fallbackAssemble(scratch)
	}

	return combined, nil
}

// ── DAG planning (8.4.1) ─────────────────────────────────────────────────────

// buildTaskDAG asks the planner to produce a dependency map between packages.
// Returns map[pkg][]prerequisitePkgs. On failure returns an empty DAG (no deps assumed).
// Source: arXiv 2511.10037 — global DAG planning before worker fan-out.
func buildTaskDAG(
	ctx context.Context,
	client *ollama.Client,
	model, systemPrompt, task string,
	decomposition map[string]string,
) map[string][]string {
	pkgs := make([]string, 0, len(decomposition))
	for p := range decomposition {
		pkgs = append(pkgs, p)
	}

	prompt := fmt.Sprintf(
		"Given these sub-tasks for packages, identify which packages depend on the output of others.\n\n"+
			"## Task\n%s\n\n"+
			"## Sub-tasks\n%s\n\n"+
			"Output ONLY a JSON object where keys are package paths and values are arrays of prerequisite package paths.\n"+
			"Independent packages have an empty array. Example: {\"internal/auth\": [], \"internal/api\": [\"internal/auth\"]}\n"+
			"Use only the exact package paths listed above. Output nothing but the JSON object.",
		task,
		func() string {
			var sb strings.Builder
			for _, p := range pkgs {
				fmt.Fprintf(&sb, "%s: %s\n", p, decomposition[p])
			}
			return sb.String()
		}(),
	)

	ctx2, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	var buf strings.Builder
	_, _, err := client.StreamChat(ctx2, model, []interface{}{
		ollama.Message{Role: "system", Content: systemPrompt},
		ollama.Message{Role: "user", Content: prompt},
	}, &ollama.ModelOptions{Temperature: 0.1, NumCtx: 8192}, func(c string) { buf.WriteString(c) })

	dag := map[string][]string{}
	if err != nil {
		return dag
	}

	// Extract JSON from response (may have surrounding text).
	raw := buf.String()
	start, end := strings.Index(raw, "{"), strings.LastIndex(raw, "}")
	if start < 0 || end <= start {
		return dag
	}
	_ = json.Unmarshal([]byte(raw[start:end+1]), &dag)

	// Remove any deps that reference unknown packages (safety net).
	known := map[string]bool{}
	for _, p := range pkgs {
		known[p] = true
	}
	for pkg, deps := range dag {
		var valid []string
		for _, d := range deps {
			if known[d] && d != pkg {
				valid = append(valid, d)
			}
		}
		dag[pkg] = valid
	}
	return dag
}

// topoSort returns packages grouped into execution levels.
// Packages in the same level have no inter-dependencies and can run in parallel.
// Packages in level N+1 depend on results from level N.
// Falls back to a single level (all parallel) on cycle detection.
func topoSort(decomposition map[string]string, dag map[string][]string) [][]string {
	// Kahn's algorithm: iteratively peel nodes with in-degree 0.
	remaining := map[string][]string{} // pkg → prerequisites still pending
	for pkg := range decomposition {
		deps := make([]string, len(dag[pkg]))
		copy(deps, dag[pkg])
		remaining[pkg] = deps
	}

	var levels [][]string
	for len(remaining) > 0 {
		// Find all packages with no remaining prerequisites.
		var ready []string
		for pkg, deps := range remaining {
			if len(deps) == 0 {
				ready = append(ready, pkg)
			}
		}
		if len(ready) == 0 {
			// Cycle detected — put all remaining packages in one level.
			all := make([]string, 0, len(remaining))
			for pkg := range remaining {
				all = append(all, pkg)
			}
			levels = append(levels, all)
			break
		}
		levels = append(levels, ready)
		// Remove completed packages from remaining prerequisite lists.
		done := map[string]bool{}
		for _, p := range ready {
			done[p] = true
			delete(remaining, p)
		}
		for pkg, deps := range remaining {
			var still []string
			for _, d := range deps {
				if !done[d] {
					still = append(still, d)
				}
			}
			remaining[pkg] = still
		}
	}
	return levels
}

// ── Step 1: Orchestrator ──────────────────────────────────────────────────────

// orchestrate asks the TierReason model to decompose the task into per-package sub-tasks.
// Returns a map[package]subTask.
func orchestrate(
	ctx context.Context,
	client *ollama.Client,
	model, systemPrompt, task string,
	packages []string,
) (map[string]string, error) {
	prompt := fmt.Sprintf(
		"You are decomposing a software task into per-package sub-tasks.\n\n"+
			"## Task\n%s\n\n"+
			"## Affected packages\n%s\n\n"+
			"Output ONLY a JSON object mapping package path to a one-sentence sub-task description.\n"+
			"Example: {\"internal/auth\": \"Refactor JWT validation to use the new key store\", ...}\n"+
			"Do not include any explanation outside the JSON.",
		task, strings.Join(packages, "\n"),
	)

	msgs := []interface{}{
		ollama.Message{Role: "system", Content: systemPrompt},
		ollama.Message{Role: "user", Content: prompt},
	}

	var sb strings.Builder
	_, _, err := client.StreamChat(ctx, model, msgs, nil, func(c string) { sb.WriteString(c) })
	if err != nil {
		return nil, err
	}

	// Extract JSON from response (model may wrap it in markdown fences).
	raw := extractJSON(sb.String())
	result := map[string]string{}
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		// Fall back: assign the full task to each package.
		fmt.Printf("  [orchestrator] decomposition parse failed (%v) — replicating task to all packages\n", err)
		for _, pkg := range packages {
			result[pkg] = task
		}
	}
	return result, nil
}

// ── Step 1b: Decomposition self-critique ──────────────────────────────────────

// critiqueDecomposition is Phase 2 of the ReAct loop: the planner reviews its
// own decomposition before workers are spawned. If the critique identifies a
// concrete issue (missing package, circular dependency, imbalanced tasks), an
// adjusted decomposition is requested and returned. Otherwise the original is
// returned unchanged. The critique is best-effort — failures never block.
func critiqueDecomposition(
	ctx context.Context,
	client *ollama.Client,
	model, systemPrompt, task string,
	packages []string,
	decomposition map[string]string,
) map[string]string {
	// Render the decomposition as a readable list for the model.
	var decompLines strings.Builder
	for pkg, sub := range decomposition {
		fmt.Fprintf(&decompLines, "  %s: %s\n", pkg, sub)
	}

	critiquePrompt := fmt.Sprintf(
		"Review this task decomposition and identify any concrete issues.\n\n"+
			"## Original task\n%s\n\n"+
			"## Affected packages\n%s\n\n"+
			"## Proposed decomposition\n%s\n"+
			"Check for: (1) packages in the affected list that are missing from the decomposition, "+
			"(2) sub-tasks that have implicit dependencies on each other that would cause parallel conflicts, "+
			"(3) sub-tasks that are so broad they cover multiple packages.\n\n"+
			"If the decomposition looks correct, respond with exactly: OK\n"+
			"If there are issues, respond with: REVISED\n"+
			"followed by a corrected JSON object in the same format as the original decomposition.\n"+
			"Do not include any other text.",
		task, strings.Join(packages, "\n"), decompLines.String(),
	)

	msgs := []interface{}{
		ollama.Message{Role: "system", Content: systemPrompt},
		ollama.Message{Role: "user", Content: critiquePrompt},
	}

	var sb strings.Builder
	critiqueCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	_, _, err := client.StreamChat(critiqueCtx, model, msgs, nil, func(c string) { sb.WriteString(c) })
	if err != nil {
		return decomposition // critique failed — use original
	}

	resp := strings.TrimSpace(sb.String())
	if strings.HasPrefix(resp, "OK") {
		return decomposition // no issues found
	}

	// Model identified issues and provided a revised decomposition.
	if strings.HasPrefix(resp, "REVISED") {
		raw := extractJSON(resp[len("REVISED"):])
		revised := map[string]string{}
		if err := json.Unmarshal([]byte(raw), &revised); err == nil && len(revised) > 0 {
			fmt.Printf("\033[38;5;244m  ◆ decomposition revised by self-critique\033[0m\n")
			return revised
		}
	}

	return decomposition // parse failed — keep original
}

// ── Step 2: Worker agent loop ─────────────────────────────────────────────────

// runWorker runs an AgentLoop for a single package sub-task.
func runWorker(
	ctx context.Context,
	client *ollama.Client,
	model, systemPrompt, fullTask, pkg, subTask string,
	toolkit *AgentToolkit,
) WorkerResult {
	workerSystem := fmt.Sprintf(
		"%s\n\n## Your role: WORKER for package %q\n"+
			"Focus only on this package. Use the provided tools to read files, write code, and verify with go build.\n"+
			"FILE EDITING RULES: Use edit_file to modify existing files (precise old→new replacement). Use write_file ONLY to create new files. Never rewrite an entire existing file with write_file.\n"+
			"When done, call finish(summary) with a brief description of your changes.",
		systemPrompt, pkg,
	)

	userMsg := fmt.Sprintf(
		"## Overall task\n%s\n\n## Your sub-task (package: %s)\n%s\n\n"+
			"Use the tools to implement the required changes. Call finish when done.",
		fullTask, pkg, subTask,
	)

	msgs := []interface{}{
		ollama.Message{Role: "system", Content: workerSystem},
		ollama.Message{Role: "user", Content: userMsg},
	}

	tools := toolkit.Tools()
	var summary, codeOutput strings.Builder
	const maxToolErrors = 3
	toolErrCount := 0

	for iter := 0; iter < workerMaxIter; iter++ {
		result, err := client.ChatWithTools(ctx, model, msgs, tools, nil)
		if err != nil {
			return WorkerResult{Package: pkg, Err: err.Error()}
		}

		// Accumulate any generated code content.
		if result.Content != "" {
			codeOutput.WriteString(result.Content)
		}

		if len(result.ToolCalls) == 0 {
			// No tool calls — treat as final answer.
			summary.WriteString(result.Content)
			break
		}

		// Append assistant's turn (with tool calls).
		msgs = append(msgs, ollama.Message{Role: "assistant", Content: result.Content})

		// Execute each tool call, append results.
		finished := false
		for _, tc := range result.ToolCalls {
			out, dispErr := toolkit.Dispatch(ctx, tc.Function.Name, tc.Function.Arguments)
			if dispErr != nil {
				if errors.Is(dispErr, ErrFinished) {
					summary.WriteString(out)
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
		// Abort if tools keep failing — prevents error-noise flooding context.
		if toolErrCount >= maxToolErrors {
			return WorkerResult{Package: pkg, Err: fmt.Sprintf("aborted: %d consecutive tool errors", toolErrCount)}
		}

		// 7.4: Interleaved thinking — force model to reason before next tool call.
		// Source: GLM-4.7 pattern (73.8% SWE-bench). ~50 tokens per turn but
		// measurably reduces stuck loops and wrong tool selection.
		msgs = append(msgs, ollama.Message{
			Role:    "user",
			Content: "Before your next action, briefly state:\n1. What you learned from the tool result(s) above\n2. What you will do next and why",
		})
	}

	return WorkerResult{
		Package: pkg,
		Summary: summary.String(),
		Code:    codeOutput.String(),
	}
}

// ── Step 3: Synthesizer ───────────────────────────────────────────────────────

// synthesize asks the TierReason model to combine all worker outputs.
func synthesize(
	ctx context.Context,
	client *ollama.Client,
	model, systemPrompt, task string,
	scratch *agentScratch,
) (string, error) {
	var workerSummaries strings.Builder
	for pkg, r := range scratch.Workers {
		if r.Err != "" {
			fmt.Fprintf(&workerSummaries, "### %s — FAILED\n%s\n\n", pkg, r.Err)
		} else {
			fmt.Fprintf(&workerSummaries, "### %s\n%s\n\n", pkg, r.Summary)
		}
	}

	prompt := fmt.Sprintf(
		"Multiple worker agents have completed sub-tasks for the following request:\n\n"+
			"## Original task\n%s\n\n"+
			"## Worker results\n%s\n\n"+
			"Produce a unified summary of all changes made, followed by any combined implementation "+
			"notes. Do not re-emit full code — just the summary and integration guidance.",
		task, workerSummaries.String(),
	)

	msgs := []interface{}{
		ollama.Message{Role: "system", Content: systemPrompt},
		ollama.Message{Role: "user", Content: prompt},
	}

	var sb strings.Builder
	_, _, err := client.StreamChat(ctx, model, msgs, nil, func(c string) { sb.WriteString(c) })
	if err != nil {
		return "", err
	}

	// Prepend each worker's code blocks.
	var out strings.Builder
	out.WriteString("## Summary\n\n")
	out.WriteString(strings.TrimSpace(sb.String()))
	out.WriteString("\n\n---\n\n## Implementation\n\n")
	for _, r := range scratch.Workers {
		if strings.TrimSpace(r.Code) != "" {
			out.WriteString(strings.TrimSpace(r.Code))
			out.WriteString("\n\n")
		}
	}
	return out.String(), nil
}

// fallbackAssemble concatenates worker outputs when synthesis fails.
// Shows explicit ✓/✗ status per worker so callers know what succeeded.
func fallbackAssemble(scratch *agentScratch) string {
	var sb strings.Builder
	for pkg, r := range scratch.Workers {
		if r.Err != "" {
			fmt.Fprintf(&sb, "## ✗ %s (failed: %s)\n\n", pkg, r.Err)
		} else {
			fmt.Fprintf(&sb, "## ✓ %s\n\n", pkg)
			sb.WriteString(strings.TrimSpace(r.Code))
			sb.WriteString("\n\n")
		}
	}
	return sb.String()
}

// ── helpers ───────────────────────────────────────────────────────────────────

// collectPackages extracts the unique directory paths from an ImpactResult.
func collectPackages(impact *intel.ImpactResult) []string {
	if impact == nil {
		return nil
	}
	seen := map[string]struct{}{}
	var pkgs []string
	for _, nodes := range impact.ByDepth {
		for _, n := range nodes {
			dir := filepath.Dir(n.FilePath)
			if _, ok := seen[dir]; !ok {
				seen[dir] = struct{}{}
				pkgs = append(pkgs, dir)
			}
		}
	}
	return pkgs
}

// writeScratch persists the agent scratch to disk (best-effort).
func writeScratch(dir string, scratch *agentScratch) {
	if dir == "" {
		return
	}
	b, err := json.MarshalIndent(scratch, "", "  ")
	if err != nil {
		return
	}
	path := filepath.Join(dir, scratchFile)
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(path, b, 0o644)
}

// extractJSON attempts to extract a JSON object from model output that may
// include markdown code fences or surrounding prose.
func extractJSON(s string) string {
	// Try to find first '{' and last '}'
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start >= 0 && end > start {
		return s[start : end+1]
	}
	return s
}
