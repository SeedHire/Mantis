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

	// ── Step 2: Fan-out workers ───────────────────────────────────────────────
	fmt.Printf("\033[38;5;244m  ◆ spawning %d worker(s) [%s]\033[0m\n",
		len(decomposition), codeModel)

	scratch := &agentScratch{
		Task:    task,
		Workers: map[string]WorkerResult{},
	}
	var mu sync.Mutex
	var wg sync.WaitGroup

	for pkg, subTask := range decomposition {
		wg.Add(1)
		go func(pkg, subTask string) {
			defer wg.Done()
			result := runWorker(ctx, client, codeModel, systemPrompt, task, pkg, subTask, toolkit)
			mu.Lock()
			scratch.Workers[pkg] = result
			mu.Unlock()
		}(pkg, subTask)
	}
	wg.Wait()

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
func fallbackAssemble(scratch *agentScratch) string {
	var sb strings.Builder
	for pkg, r := range scratch.Workers {
		fmt.Fprintf(&sb, "## %s\n\n", pkg)
		if r.Err != "" {
			fmt.Fprintf(&sb, "Error: %s\n\n", r.Err)
		} else {
			sb.WriteString(strings.TrimSpace(r.Code))
			sb.WriteString("\n\n")
		}
	}
	return sb.String()
}

// ── helpers ───────────────────────────────────────────────────────────────────

// collectPackages extracts the unique directory paths from an ImpactResult.
func collectPackages(impact *intel.ImpactResult) []string {
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
