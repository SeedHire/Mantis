// Package notify provides cross-platform desktop notifications for long-running
// tasks so users don't have to watch the terminal.
//
// Supported platforms:
//   - macOS: osascript (AppleScript)
//   - Linux: notify-send
//   - Fallback: terminal bell (\a)
package notify

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sync/atomic"
	"time"
)

// Notifier sends desktop notifications.
type Notifier struct {
	enabled  atomic.Bool
	minDelay time.Duration // minimum task duration before notifying
}

// New creates a new notifier. Enabled by default.
func New() *Notifier {
	n := &Notifier{minDelay: 30 * time.Second}
	n.enabled.Store(true)
	return n
}

// SetEnabled turns notifications on or off.
func (n *Notifier) SetEnabled(enabled bool) {
	n.enabled.Store(enabled)
}

// IsEnabled returns whether notifications are on.
func (n *Notifier) IsEnabled() bool {
	return n.enabled.Load()
}

// SetMinDelay sets the minimum task duration before sending a notification.
func (n *Notifier) SetMinDelay(d time.Duration) {
	n.minDelay = d
}

// Notify sends a desktop notification with the given title and body.
// Does nothing if notifications are disabled.
func (n *Notifier) Notify(title, body string) {
	if !n.enabled.Load() {
		return
	}
	if err := send(title, body); err != nil {
		// Fallback to terminal bell.
		fmt.Fprint(os.Stderr, "\a")
	}
}

// NotifyIfSlow sends a notification only if the elapsed duration exceeds minDelay.
// Use this for task completion notifications.
func (n *Notifier) NotifyIfSlow(title, body string, elapsed time.Duration) {
	if elapsed < n.minDelay {
		return
	}
	n.Notify(title, body)
}

// send dispatches a notification to the platform-specific notification system.
func send(title, body string) error {
	switch runtime.GOOS {
	case "darwin":
		script := fmt.Sprintf(`display notification %q with title %q`, body, title)
		return exec.Command("osascript", "-e", script).Run()
	case "linux":
		return exec.Command("notify-send", title, body).Run()
	default:
		// Unsupported — use terminal bell.
		fmt.Fprint(os.Stderr, "\a")
		return nil
	}
}

// Bell sends a terminal bell character. Always works, no dependencies.
func Bell() {
	fmt.Fprint(os.Stderr, "\a")
}
