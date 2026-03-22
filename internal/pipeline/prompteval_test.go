//go:build benchmark

package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/seedhire/mantis/internal/ollama"
	"github.com/seedhire/mantis/internal/router"
)

// ═══════════════════════════════════════════════════════════════════════════════
// Phase 14E — Prompt Tuning Loop
//
// Runs canary prompts with different prompt mutations to find the configuration
// that maximizes benchmark score. Run with:
//   go test -tags benchmark -run TestPromptTuning -timeout 2h ./internal/pipeline/...
// ═══════════════════════════════════════════════════════════════════════════════

// CanaryPrompt is a small, fast-to-generate task with clear pass/fail criteria.
type CanaryPrompt struct {
	ID       string
	Language string
	Prompt   string
	// Checks are grep patterns that must be present in the generated output.
	MustContain    []string
	MustNotContain []string
	MustBuild      bool
}

var canaryPrompts = []CanaryPrompt{
	{
		ID: "go-hello", Language: "go",
		Prompt:      "Write a Go program that prints 'Hello, World!' to stdout.",
		MustContain: []string{"fmt.Println", "func main"},
		MustBuild:   true,
	},
	{
		ID: "go-fibonacci", Language: "go",
		Prompt:      "Write a Go function that returns the Nth Fibonacci number iteratively. Include a main function that prints fib(10).",
		MustContain: []string{"func ", "main"},
		MustBuild:   true,
	},
	{
		ID: "go-http-handler", Language: "go",
		Prompt:      "Write a Go HTTP handler that returns JSON {\"status\": \"ok\"} on GET /health. Use stdlib only.",
		MustContain: []string{"net/http", "json"},
		MustBuild:   true,
	},
	{
		ID: "py-hello", Language: "python",
		Prompt:      "Write a Python script that prints 'Hello, World!'.",
		MustContain: []string{"print"},
		MustBuild:   false,
	},
	{
		ID: "py-factorial", Language: "python",
		Prompt:      "Write a Python function that computes factorial recursively with type hints.",
		MustContain: []string{"def factorial", "int"},
		MustBuild:   false,
	},
	{
		ID: "ts-hello", Language: "typescript",
		Prompt:      "Write a TypeScript function that greets a user by name and returns the greeting string.",
		MustContain: []string{"function", "string"},
		MustBuild:   false,
	},
	{
		ID: "ts-fetch", Language: "typescript",
		Prompt:      "Write a TypeScript async function that fetches a URL and returns the response as JSON. Handle errors.",
		MustContain: []string{"async", "fetch", "catch"},
		MustBuild:   false,
	},
	{
		ID: "go-sort", Language: "go",
		Prompt:      "Write a Go function that sorts a slice of integers using quicksort. Include a main function that demonstrates it.",
		MustContain: []string{"func ", "int"},
		MustBuild:   true,
	},
	{
		ID: "go-reader", Language: "go",
		Prompt:      "Write a Go program that reads a file line by line and prints each line with its number. Use stdlib only.",
		MustContain: []string{"bufio", "os.Open"},
		MustBuild:   true,
	},
	{
		ID: "py-dataclass", Language: "python",
		Prompt:      "Write a Python dataclass for a User with name, email, and age fields. Include a method to greet.",
		MustContain: []string{"dataclass", "class User"},
		MustBuild:   false,
	},
}

// PromptMutation describes a change to the system prompt.
type PromptMutation struct {
	ID          string
	Description string
	Apply       func(base string) string
}

var promptMutations = []PromptMutation{
	{
		ID:          "baseline",
		Description: "No changes — current default",
		Apply:       func(base string) string { return base },
	},
	{
		ID:          "terse-rules",
		Description: "Trim rules to half length",
		Apply: func(base string) string {
			lines := strings.Split(base, "\n")
			if len(lines) > 20 {
				lines = lines[:len(lines)/2]
			}
			return strings.Join(lines, "\n")
		},
	},
	{
		ID:          "emphasis-critical",
		Description: "Add CRITICAL emphasis to key rules",
		Apply: func(base string) string {
			base = strings.ReplaceAll(base, "NEVER", "CRITICAL: NEVER")
			base = strings.ReplaceAll(base, "MUST", "CRITICAL: MUST")
			return base
		},
	},
	{
		ID:          "code-first",
		Description: "Prepend 'Start coding immediately'",
		Apply: func(base string) string {
			return "IMPORTANT: Start coding immediately. Do not explain or plan.\n\n" + base
		},
	},
	{
		ID:          "security-emphasis",
		Description: "Double security rules",
		Apply: func(base string) string {
			return base + "\n\nSECURITY IS PARAMOUNT:\n- NEVER hardcode secrets\n- NEVER use SQL string concatenation\n- ALWAYS validate input\n- ALWAYS use parameterized queries\n"
		},
	},
}

// PromptExperimentResult records one experiment run.
type PromptExperimentResult struct {
	MutationID  string  `json:"mutation_id"`
	Description string  `json:"description"`
	Passed      int     `json:"passed"`
	Total       int     `json:"total"`
	Pct         float64 `json:"pct"`
	AvgTokens   int     `json:"avg_tokens"`
}

// TestPromptTuning runs all canary prompts against each mutation and finds the best.
func TestPromptTuning(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping prompt tuning in short mode")
	}

	client := ollama.NewFromEnv()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	models, err := client.ListModels(ctx)
	cancel()
	if err != nil || len(models) == 0 {
		t.Skip("no models available")
	}
	router.ResolveAll(models)

	basePrompt := "You are a senior software engineer. Write clean, production-quality code."
	var results []PromptExperimentResult

	for _, mut := range promptMutations {
		t.Run(mut.ID, func(t *testing.T) {
			mutatedPrompt := mut.Apply(basePrompt)
			passed, total, avgTok := runCanaries(t, client, mutatedPrompt)
			pct := 0.0
			if total > 0 {
				pct = float64(passed) / float64(total) * 100
			}
			result := PromptExperimentResult{
				MutationID:  mut.ID,
				Description: mut.Description,
				Passed:      passed,
				Total:       total,
				Pct:         pct,
				AvgTokens:   avgTok,
			}
			results = append(results, result)
			t.Logf("mutation %q: %d/%d (%.1f%%) avg_tokens=%d", mut.ID, passed, total, pct, avgTok)
		})
	}

	// Find best mutation.
	printTuningResults(t, results)

	// Save experiments to file.
	saveExperiments(results)
}

func runCanaries(t *testing.T, client *ollama.Client, systemPrompt string) (passed, total, avgTokens int) {
	t.Helper()
	totalTokens := 0

	for _, cp := range canaryPrompts {
		total++

		dir := t.TempDir()
		switch cp.Language {
		case "go":
			os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module canary\n\ngo 1.22\n"), 0o644)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		result, err := Run(ctx, client, cp.Prompt, systemPrompt, Options{
			Root:       dir,
			MaxRetries: 1,
			SkipTests:  true,
		})
		cancel()

		if err != nil {
			t.Logf("  %s: pipeline error: %v", cp.ID, err)
			continue
		}

		totalTokens += result.ComplTok

		// Check criteria.
		output := result.Combined
		ok := true

		for _, pattern := range cp.MustContain {
			if !strings.Contains(output, pattern) {
				t.Logf("  %s: missing %q", cp.ID, pattern)
				ok = false
				break
			}
		}
		for _, pattern := range cp.MustNotContain {
			if strings.Contains(output, pattern) {
				t.Logf("  %s: found forbidden %q", cp.ID, pattern)
				ok = false
				break
			}
		}

		if cp.MustBuild && ok {
			buildOK, buildOut := checkBuild(dir, cp.Language)
			if !buildOK {
				t.Logf("  %s: build failed: %s", cp.ID, truncateBench(buildOut, 100))
				ok = false
			}
		}

		if ok {
			passed++
		}
	}

	if total > 0 {
		avgTokens = totalTokens / total
	}
	return
}

func printTuningResults(t *testing.T, results []PromptExperimentResult) {
	t.Log("")
	t.Log("═══════════════════════════════════════════════════")
	t.Log("  Prompt Tuning Results")
	t.Log("═══════════════════════════════════════════════════")

	bestPct := 0.0
	bestID := ""
	for _, r := range results {
		marker := "  "
		if r.Pct > bestPct {
			bestPct = r.Pct
			bestID = r.MutationID
		}
		t.Logf("%s %-20s %5.1f%% (%d/%d) avg=%d tok | %s",
			marker, r.MutationID, r.Pct, r.Passed, r.Total, r.AvgTokens, r.Description)
	}

	t.Log("───────────────────────────────────────────────────")
	t.Logf("  Best: %s (%.1f%%)", bestID, bestPct)
	t.Log("═══════════════════════════════════════════════════")
}

func saveExperiments(results []PromptExperimentResult) {
	mantisDir := ".mantis"
	os.MkdirAll(mantisDir, 0o755)
	path := filepath.Join(mantisDir, "prompt-experiments.json")

	type experimentLog struct {
		Date    string                   `json:"date"`
		Results []PromptExperimentResult `json:"results"`
	}

	var history []experimentLog
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &history)
	}

	history = append(history, experimentLog{
		Date:    time.Now().Format("2006-01-02 15:04"),
		Results: results,
	})

	data, _ := json.MarshalIndent(history, "", "  ")
	os.WriteFile(path, data, 0o644)
}

// Ensure unused imports are consumed.
var _ = fmt.Sprintf
