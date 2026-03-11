package pipeline

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/term"
)

// taskRenderer owns all stdout output for the task region.
// Every state change erases and redraws the entire block atomically,
// preventing the text-overlap bugs caused by per-line ANSI cursor math.
type taskRenderer struct {
	mu        sync.Mutex
	tasks     []Task     // shared reference — caller must not resize
	warnings  []string   // edit block warnings, shown in summary
	lineCount int        // lines currently on screen (for erase)
	spinFrame int
	isTTY     bool
	// lastStatus tracks per-task status for non-TTY append-only mode.
	lastStatus []string
}

// newTaskRenderer creates a renderer bound to the given task slice.
func newTaskRenderer(tasks []Task) *taskRenderer {
	r := &taskRenderer{
		tasks:      tasks,
		isTTY:      term.IsTerminal(int(os.Stdout.Fd())),
		lastStatus: make([]string, len(tasks)),
	}
	for i := range tasks {
		r.lastStatus[i] = tasks[i].Status
	}
	return r
}

// render erases the previous output and redraws the full task region.
// Must be called with r.mu held.
func (r *taskRenderer) render() {
	if !r.isTTY {
		r.renderNonTTY()
		return
	}

	var buf bytes.Buffer

	// Erase previous output: move cursor up lineCount lines and clear to end of screen.
	if r.lineCount > 0 {
		fmt.Fprintf(&buf, "\033[%dA", r.lineCount)
	}
	buf.WriteString("\033[J") // clear from cursor to end of screen

	w := termWidth()

	// Header
	fmt.Fprintf(&buf, "  %s── tasks ──%s\n", pColorDim, pColorReset)
	lines := 1

	// Task lines
	for i := range r.tasks {
		icon, iconColor, titleColor := taskIcon(r.tasks[i].Status, r.spinFrame)
		title := truncateTitle(r.tasks[i].Title, w)
		suffix := taskSuffix(&r.tasks[i])
		fmt.Fprintf(&buf, "  %s%s %s%s%s%s\n", iconColor, icon, titleColor, title, pColorReset, suffix)
		lines++

		// Sub-message (retry info, stuck message, skip reason)
		if r.tasks[i].SubMessage != "" {
			fmt.Fprintf(&buf, "     %s↳ %s%s\n", pColorDim, r.tasks[i].SubMessage, pColorReset)
			lines++
		}
	}

	// Footer: progress count
	done := 0
	totalFiles := 0
	totalTokens := 0
	for j := range r.tasks {
		if r.tasks[j].Status == "done" {
			done++
		}
		totalFiles += r.tasks[j].FileCount
		totalTokens += r.tasks[j].Tokens
		if r.tasks[j].Status == "running" {
			totalTokens += int(atomic.LoadInt64(&r.tasks[j].streamTok))
		}
	}
	buf.WriteString("\n")
	lines++
	fmt.Fprintf(&buf, "  %s%d/%d done", pColorDim, done, len(r.tasks))
	if totalFiles > 0 {
		fmt.Fprintf(&buf, " · %d files", totalFiles)
	}
	if totalTokens > 0 {
		fmt.Fprintf(&buf, " · %d tokens", totalTokens)
	}
	fmt.Fprintf(&buf, "%s\n", pColorReset)
	lines++

	os.Stdout.Write(buf.Bytes())
	r.lineCount = lines
}

// renderNonTTY prints append-only log lines when stdout is not a terminal (CI/pipes).
// Only prints when a task status actually changes.
func (r *taskRenderer) renderNonTTY() {
	for i := range r.tasks {
		status := r.tasks[i].Status
		if status != r.lastStatus[i] {
			r.lastStatus[i] = status
			icon, _, _ := taskIcon(status, 0)
			suffix := ""
			if r.tasks[i].SubMessage != "" {
				suffix = " (" + r.tasks[i].SubMessage + ")"
			}
			fmt.Fprintf(os.Stdout, "  %s %s%s%s\n", icon, r.tasks[i].Title, suffix, taskSuffix(&r.tasks[i]))
		}
	}
}

// setStatus updates a task's status and re-renders.
func (r *taskRenderer) setStatus(idx int, status string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if idx < 0 || idx >= len(r.tasks) {
		return
	}
	r.tasks[idx].Status = status
	r.render()
}

// setSubMessage sets an indented sub-line for a task (e.g., retry info) and re-renders.
func (r *taskRenderer) setSubMessage(idx int, msg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if idx < 0 || idx >= len(r.tasks) {
		return
	}
	r.tasks[idx].SubMessage = msg
	r.render()
}

// clearSubMessage removes the sub-line for a task and re-renders.
func (r *taskRenderer) clearSubMessage(idx int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if idx < 0 || idx >= len(r.tasks) {
		return
	}
	r.tasks[idx].SubMessage = ""
	r.render()
}

// addWarning collects a warning to display in the post-completion summary.
// Does not trigger a re-render.
func (r *taskRenderer) addWarning(msg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.warnings = append(r.warnings, msg)
}

// startSpinner launches a goroutine that increments the spin frame every 120ms
// and re-renders. Returns a stop function.
func (r *taskRenderer) startSpinner() func() {
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(120 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				r.mu.Lock()
				hasRunning := false
				for i := range r.tasks {
					if r.tasks[i].Status == "running" {
						hasRunning = true
						break
					}
				}
				if hasRunning {
					r.spinFrame++
					r.render()
				}
				r.mu.Unlock()
			}
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { close(done) }) }
}

// printSummary prints collected warnings after the spinner has stopped.
func (r *taskRenderer) printSummary() {
	r.mu.Lock()
	warns := make([]string, len(r.warnings))
	copy(warns, r.warnings)
	r.mu.Unlock()

	if len(warns) == 0 {
		return
	}
	for _, w := range warns {
		fmt.Printf("  %s⚠ %s%s\n", pColorDim, w, pColorReset)
	}
}

// initialRender draws the task list for the first time. Must be called once
// before startSpinner.
func (r *taskRenderer) initialRender() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.render()
}

// ── Helpers (moved from pipeline.go) ─────────────────────────────────────────

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
func truncateTitle(title string, maxWidth int) string {
	available := maxWidth - 40
	if available < 20 {
		available = 20
	}
	if len(title) <= available {
		return title
	}
	return title[:available-1] + "…"
}
