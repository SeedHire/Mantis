// Package usage tracks token consumption for informational purposes.
// Data stored in ~/.mantis/usage.json — persists across projects.
// No limits are enforced — usage is unbounded.
package usage

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// DayUsage tracks usage for a single calendar day.
type DayUsage struct {
	Date        string `json:"date"`
	Tokens      int    `json:"tokens"`
	HeavyCalls  int    `json:"heavy_calls"`
	VisionCalls int    `json:"vision_calls"`
}

// Tracker reads and writes ~/.mantis/usage.json.
type Tracker struct {
	path  string
	today *DayUsage
}

// New returns a Tracker backed by ~/.mantis/usage.json.
func New() *Tracker {
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".mantis")
	_ = os.MkdirAll(dir, 0o755)
	t := &Tracker{path: filepath.Join(dir, "usage.json")}
	t.today = t.loadToday()
	return t
}

// Add records token usage. Always returns "" — no limits are enforced.
func (t *Tracker) Add(tokens int, isHeavy, isVision bool) string {
	t.today.Tokens += tokens
	if isHeavy {
		t.today.HeavyCalls++
	}
	if isVision {
		t.today.VisionCalls++
	}
	_ = t.flush()
	return ""
}

// Summary returns a one-line usage status string.
func (t *Tracker) Summary() string {
	return fmt.Sprintf("today: %s tokens · %d heavy · %d vision",
		formatTokens(t.today.Tokens), t.today.HeavyCalls, t.today.VisionCalls)
}

func (t *Tracker) loadToday() *DayUsage {
	today := time.Now().Format("2006-01-02")
	data, err := os.ReadFile(t.path)
	if err != nil {
		return &DayUsage{Date: today}
	}
	var u DayUsage
	if err := json.Unmarshal(data, &u); err != nil || u.Date != today {
		return &DayUsage{Date: today}
	}
	return &u
}

func (t *Tracker) flush() error {
	data, err := json.MarshalIndent(t.today, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(t.path, data, 0o644)
}

func formatTokens(n int) string {
	if n >= 1000 {
		return fmt.Sprintf("%dk", n/1000)
	}
	return fmt.Sprintf("%d", n)
}
