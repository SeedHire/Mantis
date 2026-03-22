//go:build benchmark

package pipeline

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

// ═══════════════════════════════════════════════════════════════════════════════
// Phase 14C — LLM-as-Judge Quality Scoring
//
// Sends generated code to a judge model with a structured rubric.
// Returns scores for correctness, completeness, idiomaticity, security, simplicity.
// ═══════════════════════════════════════════════════════════════════════════════

// JudgeScore holds per-criterion scores from the LLM judge.
type JudgeScore struct {
	Correctness  int    `json:"correctness"`  // 0-3: Does it solve the stated task?
	Completeness int    `json:"completeness"` // 0-3: Are all requested features present?
	Idiomaticity int    `json:"idiomaticity"` // 0-3: Does it follow language conventions?
	Security     int    `json:"security"`     // 0-3: Any obvious vulnerabilities?
	Simplicity   int    `json:"simplicity"`   // 0-3: Over-engineered or appropriately scoped?
	Reasoning    string `json:"reasoning"`    // Brief explanation
}

// Total returns the sum of all criteria (max 15).
func (s JudgeScore) Total() int {
	return s.Correctness + s.Completeness + s.Idiomaticity + s.Security + s.Simplicity
}

// Grade returns a letter grade based on the total score.
func (s JudgeScore) Grade() string {
	total := s.Total()
	switch {
	case total >= 13:
		return "A"
	case total >= 10:
		return "B"
	case total >= 8:
		return "C"
	default:
		return "D"
	}
}

const judgeRubric = `You are a senior code reviewer. Score the generated code on these 5 criteria (0-3 each):

1. **Correctness** (0-3): Does the code solve the stated task? Would it work if run?
   - 0: Completely wrong or won't run
   - 1: Partially correct, major issues
   - 2: Mostly correct, minor issues
   - 3: Fully correct, would work as-is

2. **Completeness** (0-3): Are all requested features present?
   - 0: Missing most features
   - 1: Has some features, major gaps
   - 2: Most features present, minor omissions
   - 3: All requested features implemented

3. **Idiomaticity** (0-3): Does it follow language conventions and best practices?
   - 0: Non-idiomatic, looks like translated from another language
   - 1: Some conventions followed, many violations
   - 2: Mostly idiomatic, minor style issues
   - 3: Fully idiomatic, expert-level style

4. **Security** (0-3): Any obvious vulnerabilities?
   - 0: Critical vulnerabilities (SQL injection, hardcoded secrets)
   - 1: Significant security issues
   - 2: Minor concerns but no critical issues
   - 3: Secure by design, follows security best practices

5. **Simplicity** (0-3): Is it appropriately scoped?
   - 0: Massively over-engineered or absurdly under-engineered
   - 1: Significant scope mismatch
   - 2: Mostly appropriate, minor excess
   - 3: Clean, focused, right-sized

Respond ONLY with valid JSON matching this schema:
{"correctness": N, "completeness": N, "idiomaticity": N, "security": N, "simplicity": N, "reasoning": "brief explanation"}
`

// JudgeCode sends generated code to an LLM judge and returns structured scores.
func JudgeCode(ctx context.Context, client *ollama.Client, prompt, generatedCode, language string) (*JudgeScore, error) {
	model := router.ModelFor(router.TierReason)
	if model == "" {
		return nil, fmt.Errorf("no reason-tier model available for judging")
	}

	userMsg := fmt.Sprintf(
		"## Original Prompt\n%s\n\n## Language\n%s\n\n## Generated Code\n```\n%s\n```\n\nScore this code according to the rubric.",
		prompt, language, generatedCode,
	)

	messages := []interface{}{
		ollama.Message{Role: "system", Content: judgeRubric},
		ollama.Message{Role: "user", Content: userMsg},
	}

	var rb strings.Builder
	opts := &ollama.ModelOptions{Temperature: 0.1}
	_, _, err := client.StreamChat(ctx, model, messages, opts, func(content string) {
		rb.WriteString(content)
	})
	if err != nil {
		return nil, fmt.Errorf("judge model call failed: %w", err)
	}

	// Extract JSON from response (may have markdown fences).
	rawJSON := extractJSON(rb.String())

	var score JudgeScore
	if err := json.Unmarshal([]byte(rawJSON), &score); err != nil {
		return nil, fmt.Errorf("failed to parse judge response: %w (raw: %s)", err, truncateJudge(rb.String(), 200))
	}

	// Clamp values to 0-3.
	score.Correctness = clamp(score.Correctness, 0, 3)
	score.Completeness = clamp(score.Completeness, 0, 3)
	score.Idiomaticity = clamp(score.Idiomaticity, 0, 3)
	score.Security = clamp(score.Security, 0, 3)
	score.Simplicity = clamp(score.Simplicity, 0, 3)

	return &score, nil
}

// JudgeDir reads all source files in a directory and judges them together.
func JudgeDir(ctx context.Context, client *ollama.Client, prompt, dir, language string) (*JudgeScore, error) {
	var sb strings.Builder
	exts := langExtensions(language)

	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		for _, ext := range exts {
			if strings.HasSuffix(path, ext) {
				data, err := os.ReadFile(path)
				if err != nil {
					continue
				}
				rel, _ := filepath.Rel(dir, path)
				sb.WriteString(fmt.Sprintf("// === %s ===\n", rel))
				sb.WriteString(string(data))
				sb.WriteString("\n\n")
				break
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	if sb.Len() == 0 {
		return &JudgeScore{Reasoning: "no source files found"}, nil
	}

	return JudgeCode(ctx, client, prompt, sb.String(), language)
}

// JudgeHistoryEntry records a single judge evaluation.
type JudgeHistoryEntry struct {
	Date     string     `json:"date"`
	Commit   string     `json:"commit"`
	PromptID string     `json:"prompt_id"`
	Score    JudgeScore `json:"score"`
	Model    string     `json:"model"`
}

// SaveJudgeHistory appends a judge result to .mantis/judge-history.json.
func SaveJudgeHistory(mantisDir string, entry JudgeHistoryEntry) error {
	path := filepath.Join(mantisDir, "judge-history.json")
	var history []JudgeHistoryEntry

	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &history)
	}

	history = append(history, entry)

	data, err := json.MarshalIndent(history, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// PrintJudgeScorecard prints a formatted scorecard for multiple judge results.
func PrintJudgeScorecard(scores map[string]*JudgeScore) {
	totalScore := 0
	maxScore := len(scores) * 15

	fmt.Println("\n═══════════════════════════════════════════════════")
	fmt.Printf("  LLM Judge Scorecard | %s\n", time.Now().Format("2006-01-02"))
	fmt.Println("═══════════════════════════════════════════════════")

	for id, s := range scores {
		fmt.Printf("  %-25s %s %2d/15\n", id, makeJudgeBar(s.Total(), 15), s.Total())
		if s.Reasoning != "" {
			fmt.Printf("    %s\n", truncateJudge(s.Reasoning, 70))
		}
	}

	pct := float64(totalScore) / float64(maxScore) * 100
	fmt.Println("───────────────────────────────────────────────────")
	fmt.Printf("  Overall: %d/%d = %.1f%% (Grade: %s)\n", totalScore, maxScore, pct, overallGrade(pct))
	fmt.Println("═══════════════════════════════════════════════════")
}

// ── helpers ─────────────────────────────────────────────────────────────────

func extractJSON(s string) string {
	// Strip markdown fences.
	s = strings.TrimSpace(s)
	if idx := strings.Index(s, "```json"); idx >= 0 {
		s = s[idx+7:]
		if end := strings.Index(s, "```"); end >= 0 {
			s = s[:end]
		}
	} else if idx := strings.Index(s, "```"); idx >= 0 {
		s = s[idx+3:]
		if end := strings.Index(s, "```"); end >= 0 {
			s = s[:end]
		}
	}
	// Find JSON object boundaries.
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start >= 0 && end > start {
		s = s[start : end+1]
	}
	return strings.TrimSpace(s)
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func langExtensions(lang string) []string {
	switch lang {
	case "go":
		return []string{".go"}
	case "typescript":
		return []string{".ts", ".tsx"}
	case "python":
		return []string{".py"}
	case "rust":
		return []string{".rs"}
	default:
		return []string{".go", ".ts", ".py", ".rs"}
	}
}

func makeJudgeBar(score, max int) string {
	filled := score * 20 / max // scale to 20 chars
	empty := 20 - filled
	return "[" + strings.Repeat("█", filled) + strings.Repeat("░", empty) + "]"
}

func truncateJudge(s string, max int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > max {
		return s[:max-3] + "..."
	}
	return s
}

func overallGrade(pct float64) string {
	switch {
	case pct >= 83:
		return "A"
	case pct >= 67:
		return "B"
	case pct >= 50:
		return "C"
	default:
		return "D"
	}
}
