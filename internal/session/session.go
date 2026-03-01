// Package session tracks token usage for a single Mantis chat session
// and produces an end-of-session cost comparison report.
package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/seedhire/mantis/internal/router"
)

// Turn records one exchange in a session.
type Turn struct {
	Model            string      `json:"model"`
	Tier             router.Tier `json:"tier"`
	PromptTokens     int         `json:"prompt_tokens"`
	CompletionTokens int         `json:"completion_tokens"`
	HasImage         bool        `json:"has_image"`
	Timestamp        time.Time   `json:"timestamp"`
}

// Session tracks the whole conversation.
type Session struct {
	StartTime time.Time `json:"start_time"`
	Turns     []Turn    `json:"turns"`
	Warnings  []string  `json:"warnings,omitempty"`
}

// SavedSession is the on-disk format for session persistence.
type SavedSession struct {
	Session
	Topic   string `json:"topic"`
	Summary string `json:"summary,omitempty"`
}

// New creates a fresh session.
func New() *Session {
	return &Session{StartTime: time.Now()}
}

// Add records a completed turn.
func (s *Session) Add(model string, tier router.Tier, prompt, completion int, hasImage bool) {
	s.Turns = append(s.Turns, Turn{
		Model:            model,
		Tier:             tier,
		PromptTokens:     prompt,
		CompletionTokens: completion,
		HasImage:         hasImage,
		Timestamp:        time.Now(),
	})
}

// WarnWaste appends a token waste warning shown in the final report.
func (s *Session) WarnWaste(msg string) {
	s.Warnings = append(s.Warnings, msg)
}

// Totals returns aggregate token counts.
func (s *Session) Totals() (prompt, completion, total int) {
	for _, t := range s.Turns {
		prompt += t.PromptTokens
		completion += t.CompletionTokens
	}
	total = prompt + completion
	return
}

// TierCounts returns how many turns ran at each tier.
func (s *Session) TierCounts() map[router.Tier]int {
	counts := map[router.Tier]int{}
	for _, t := range s.Turns {
		counts[t.Tier]++
	}
	return counts
}

// imageCalls returns the number of vision turns.
func (s *Session) imageCalls() int {
	n := 0
	for _, t := range s.Turns {
		if t.HasImage {
			n++
		}
	}
	return n
}

// pricing constants in USD per 1k tokens (approximate, 2025).
const (
	gpt4oInputPer1k    = 0.0025
	gpt4oOutputPer1k   = 0.010
	sonnetInputPer1k   = 0.003
	sonnetOutputPer1k  = 0.015
	opusInputPer1k     = 0.015
	opusOutputPer1k    = 0.075
)

func estimateCost(promptTok, completionTok int, inputRate, outputRate float64) float64 {
	return float64(promptTok)/1000*inputRate + float64(completionTok)/1000*outputRate
}

// Report renders the ASCII session summary box.
func (s *Session) Report() string {
	prompt, completion, total := s.Totals()
	tiers := s.TierCounts()
	images := s.imageCalls()
	duration := time.Since(s.StartTime).Round(time.Second)

	gpt4oCost := estimateCost(prompt, completion, gpt4oInputPer1k, gpt4oOutputPer1k)
	sonnetCost := estimateCost(prompt, completion, sonnetInputPer1k, sonnetOutputPer1k)
	opusCost := estimateCost(prompt, completion, opusInputPer1k, opusOutputPer1k)

	w := 54
	line := strings.Repeat("─", w)

	var sb strings.Builder
	sb.WriteString("\n")
	sb.WriteString("╭" + line + "╮\n")
	sb.WriteString(padLine("  SESSION SUMMARY — mantis", w) + "\n")
	sb.WriteString("├" + line + "┤\n")
	sb.WriteString(padLine(fmt.Sprintf("  Duration          %s", duration), w) + "\n")
	sb.WriteString(padLine(fmt.Sprintf("  Total tokens      %s", formatTokens(total)), w) + "\n")
	sb.WriteString(padLine(fmt.Sprintf("  Turns             %d", len(s.Turns)), w) + "\n")
	if images > 0 {
		sb.WriteString(padLine(fmt.Sprintf("  Vision calls      %d", images), w) + "\n")
	}
	sb.WriteString(padLine(fmt.Sprintf("  Route  trivial×%d  fast×%d  code×%d  reason×%d  heavy×%d  max×%d",
		tiers[router.TierTrivial], tiers[router.TierFast], tiers[router.TierCode],
		tiers[router.TierReason], tiers[router.TierHeavy], tiers[router.TierMax]), w) + "\n")
	sb.WriteString("├" + line + "┤\n")
	sb.WriteString(padLine("  WHAT THIS WOULD HAVE COST WITH PAID APIs", w) + "\n")
	sb.WriteString(padLine(fmt.Sprintf("  GPT-4o             $%.2f", gpt4oCost), w) + "\n")
	sb.WriteString(padLine(fmt.Sprintf("  Claude Sonnet      $%.2f", sonnetCost), w) + "\n")
	sb.WriteString(padLine(fmt.Sprintf("  Claude Opus        $%.2f", opusCost), w) + "\n")
	sb.WriteString(padLine("  Mantis cost        $0.00 ✓", w) + "\n")

	if len(s.Warnings) > 0 {
		sb.WriteString("├" + line + "┤\n")
		sb.WriteString(padLine("  TOKEN WASTE DETECTED 🟡", w) + "\n")
		for _, w2 := range s.Warnings {
			sb.WriteString(padLine("  · "+w2, w) + "\n")
		}
	}

	sb.WriteString("╰" + line + "╯\n")
	return sb.String()
}

func padLine(s string, width int) string {
	runes := []rune(s)
	if len(runes) >= width {
		return "│" + string(runes[:width]) + "│"
	}
	return "│" + s + strings.Repeat(" ", width-len(runes)) + "│"
}

func formatTokens(n int) string {
	if n >= 1000 {
		return fmt.Sprintf("%d,%.3d", n/1000, n%1000)
	}
	return fmt.Sprintf("%d", n)
}

// Save persists the session to .mantis/sessions/{timestamp}.json.
// topic is a short description extracted from the first user message.
func (s *Session) Save(mantisDir, topic, summary string) error {
	sessDir := filepath.Join(mantisDir, "sessions")
	if err := os.MkdirAll(sessDir, 0o755); err != nil {
		return err
	}

	saved := SavedSession{
		Session: *s,
		Topic:   topic,
		Summary: summary,
	}
	data, err := json.MarshalIndent(saved, "", "  ")
	if err != nil {
		return err
	}

	filename := s.StartTime.Format("2006-01-02_15-04-05") + ".json"
	path := filepath.Join(sessDir, filename)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return err
	}

	// Prune old sessions — keep at most 10.
	return pruneOldSessions(sessDir, 10)
}

// LoadRecent returns the most recent saved session within maxAge, or nil.
func LoadRecent(mantisDir string, maxAge time.Duration) (*SavedSession, error) {
	sessDir := filepath.Join(mantisDir, "sessions")
	entries, err := os.ReadDir(sessDir)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, nil
	}

	// Sort by name descending (timestamp-based names → newest first).
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() > entries[j].Name()
	})

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(sessDir, e.Name()))
		if err != nil {
			continue
		}
		var saved SavedSession
		if err := json.Unmarshal(data, &saved); err != nil {
			continue
		}
		if time.Since(saved.StartTime) <= maxAge {
			return &saved, nil
		}
		break // oldest-first after sort, so if this one is too old, all are
	}
	return nil, nil
}

func pruneOldSessions(sessDir string, keep int) error {
	entries, err := os.ReadDir(sessDir)
	if err != nil {
		return err
	}

	var jsonFiles []os.DirEntry
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			jsonFiles = append(jsonFiles, e)
		}
	}

	if len(jsonFiles) <= keep {
		return nil
	}

	// Sort ascending by name (oldest first).
	sort.Slice(jsonFiles, func(i, j int) bool {
		return jsonFiles[i].Name() < jsonFiles[j].Name()
	})

	for i := 0; i < len(jsonFiles)-keep; i++ {
		_ = os.Remove(filepath.Join(sessDir, jsonFiles[i].Name()))
	}
	return nil
}
