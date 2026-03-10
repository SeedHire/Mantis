package repl

// Benchmark runs a fixed set of coding tasks against the current model configuration
// and scores the responses. Results are appended to .mantis/benchmark-history.jsonl.
//
// Scoring criteria (per task):
//   - Response is non-empty                     +1
//   - Contains a code block                     +1
//   - No stub markers (TODO, FIXME, placeholder) +1
//   - No "I cannot" / "I don't know" hedges     +1
//
// Max score per task: 4. Overall: (total / maxTotal) as a percentage.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/seedhire/mantis/internal/ollama"
	"github.com/seedhire/mantis/internal/router"
)

// benchmarkTask is one item in the fixed benchmark suite.
type benchmarkTask struct {
	ID    string
	Tier  router.Tier
	Prompt string
}

// benchmarkTasks is the fixed 10-task suite.
var benchmarkTasks = []benchmarkTask{
	{
		ID:    "rest-endpoint",
		Tier:  router.TierCode,
		Prompt: "Write a Go HTTP handler that accepts POST /users with JSON body {name, email}, validates both fields are non-empty, and returns 201 with the created user or 400 on validation failure. Include the full handler function.",
	},
	{
		ID:    "fix-nil-deref",
		Tier:  router.TierCode,
		Prompt: "Fix this Go code that panics on nil pointer dereference:\nfunc getUser(users map[string]*User, id string) string {\n    return users[id].Name\n}\nReturn the fixed function only.",
	},
	{
		ID:    "write-unit-test",
		Tier:  router.TierCode,
		Prompt: "Write a Go unit test for this function:\nfunc Add(a, b int) int { return a + b }\nInclude table-driven tests with at least 4 cases including negative numbers.",
	},
	{
		ID:    "explain-function",
		Tier:  router.TierFast,
		Prompt: "Explain what this function does in 3 bullet points:\nfunc binarySearch(arr []int, target int) int {\n    lo, hi := 0, len(arr)-1\n    for lo <= hi {\n        mid := (lo + hi) / 2\n        if arr[mid] == target { return mid }\n        if arr[mid] < target { lo = mid + 1 } else { hi = mid - 1 }\n    }\n    return -1\n}",
	},
	{
		ID:    "sql-query",
		Tier:  router.TierCode,
		Prompt: "Write a SQL query that returns the top 5 users by order count from tables: users(id, name, email) and orders(id, user_id, created_at). Include only users with more than 3 orders.",
	},
	{
		ID:    "typescript-interface",
		Tier:  router.TierFast,
		Prompt: "Write a TypeScript interface for a User with: id (number), email (string), role ('admin' | 'user' | 'guest'), createdAt (Date), and an optional profile object with firstName, lastName, and avatarUrl.",
	},
	{
		ID:    "refactor-switch",
		Tier:  router.TierCode,
		Prompt: "Refactor this Go switch statement into a map-based dispatch:\nfunc handle(op string, a, b int) int {\n    switch op {\n    case \"add\": return a + b\n    case \"sub\": return a - b\n    case \"mul\": return a * b\n    default: return 0\n    }\n}\nReturn the complete refactored function.",
	},
	{
		ID:    "error-wrapping",
		Tier:  router.TierFast,
		Prompt: "Show the correct way to wrap and unwrap errors in Go using fmt.Errorf and errors.Is. Give one concrete example with a custom sentinel error.",
	},
	{
		ID:    "middleware-pattern",
		Tier:  router.TierCode,
		Prompt: "Write a Go HTTP middleware function that adds a request ID header to every request and logs method + path + status code. Show the complete middleware and how to use it with http.Handle.",
	},
	{
		ID:    "goroutine-pattern",
		Tier:  router.TierCode,
		Prompt: "Write a Go function that fetches 5 URLs concurrently using goroutines and returns all results (or errors) once all are complete. Use WaitGroup or errgroup. Show the complete function.",
	},
}

// BenchmarkResult holds the score for one task.
type BenchmarkResult struct {
	TaskID   string    `json:"task_id"`
	Score    int       `json:"score"`    // 0–4
	MaxScore int       `json:"max_score"` // always 4
	Passed   bool      `json:"passed"`   // score >= 3
	Duration float64   `json:"duration_sec"`
	Reason   string    `json:"reason,omitempty"`
}

// BenchmarkRun is one complete benchmark execution.
type BenchmarkRun struct {
	Timestamp   time.Time         `json:"ts"`
	TotalScore  int               `json:"total_score"`
	MaxTotal    int               `json:"max_total"`
	Pct         int               `json:"pct"`
	DurationSec float64           `json:"duration_sec"`
	Results     []BenchmarkResult `json:"results"`
}

// RunBenchmark executes all benchmark tasks and prints a summary.
func RunBenchmark(client *ollama.Client, mantisDir string) {
	fmt.Printf("\n\033[38;5;214m◈ Mantis Benchmark\033[0m  %d tasks\n\n", len(benchmarkTasks))
	start := time.Now()

	run := BenchmarkRun{Timestamp: start}
	passed := 0

	for i, task := range benchmarkTasks {
		model := router.ModelFor(task.Tier)
		fmt.Printf("  [%2d/%d] %-20s  [%s] ", i+1, len(benchmarkTasks), task.ID, model)

		taskStart := time.Now()
		response, score, reason := runBenchmarkTask(client, task, model)
		dur := time.Since(taskStart).Seconds()

		ok := score >= 3
		if ok {
			passed++
			fmt.Printf("\033[38;5;70m✓ %d/4\033[0m  (%.1fs)\n", score, dur)
		} else {
			fmt.Printf("\033[38;5;196m✗ %d/4\033[0m  (%.1fs) %s\n", score, dur, reason)
		}
		_ = response

		run.Results = append(run.Results, BenchmarkResult{
			TaskID:   task.ID,
			Score:    score,
			MaxScore: 4,
			Passed:   ok,
			Duration: dur,
			Reason:   reason,
		})
		run.TotalScore += score
		run.MaxTotal += 4
	}

	run.DurationSec = time.Since(start).Seconds()
	if run.MaxTotal > 0 {
		run.Pct = run.TotalScore * 100 / run.MaxTotal
	}

	fmt.Printf("\n\033[38;5;214m◈ Mantis benchmark: %d/%d tasks passed (%d%%) in %.0fs\033[0m\n\n",
		passed, len(benchmarkTasks), run.Pct, run.DurationSec)

	// Persist to benchmark-history.jsonl.
	if mantisDir != "" {
		_ = os.MkdirAll(mantisDir, 0o755)
		line, err := json.Marshal(run)
		if err == nil {
			f, err := os.OpenFile(filepath.Join(mantisDir, "benchmark-history.jsonl"),
				os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
			if err == nil {
				_, _ = fmt.Fprintf(f, "%s\n", line)
				f.Close()
			}
		}
	}
}

// runBenchmarkTask sends one benchmark prompt and scores the response.
func runBenchmarkTask(client *ollama.Client, task benchmarkTask, model string) (string, int, string) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	msgs := []interface{}{
		ollama.Message{Role: "system", Content: "You are a senior software engineer. Answer concisely and correctly. Output complete, working code. No stubs, no TODOs."},
		ollama.Message{Role: "user", Content: task.Prompt},
	}
	opts := &ollama.ModelOptions{Temperature: 0.2, NumCtx: 8192}

	var buf strings.Builder
	_, _, err := client.StreamChat(ctx, model, msgs, opts, func(chunk string) {
		buf.WriteString(chunk)
	})
	if err != nil {
		return "", 0, "model error: " + err.Error()
	}

	resp := buf.String()
	score, reason := scoreBenchmarkResponse(resp)
	return resp, score, reason
}

// scoreBenchmarkResponse scores 0–4 based on response quality signals.
func scoreBenchmarkResponse(resp string) (int, string) {
	if strings.TrimSpace(resp) == "" {
		return 0, "empty response"
	}

	score := 1 // non-empty = 1 point
	var reasons []string

	lower := strings.ToLower(resp)

	// +1: contains a code block
	if strings.Contains(resp, "```") || strings.Contains(resp, "\tfunc ") || strings.Contains(resp, "\tfor ") {
		score++
	} else {
		reasons = append(reasons, "no code block")
	}

	// +1: no stub/placeholder markers
	stubMarkers := []string{"todo", "fixme", "placeholder", "your code here", "implement this", "add implementation"}
	hasStub := false
	for _, m := range stubMarkers {
		if strings.Contains(lower, m) {
			hasStub = true
			reasons = append(reasons, "contains stub marker")
			break
		}
	}
	if !hasStub {
		score++
	}

	// +1: no hedge phrases
	hedges := []string{"i cannot", "i don't know", "i'm unable", "i am unable", "as an ai", "i apologize"}
	hasHedge := false
	for _, h := range hedges {
		if strings.Contains(lower, h) {
			hasHedge = true
			reasons = append(reasons, "contains hedge")
			break
		}
	}
	if !hasHedge {
		score++
	}

	return score, strings.Join(reasons, "; ")
}
