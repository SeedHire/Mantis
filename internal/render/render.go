package render

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"sync"

	"golang.org/x/term"
)

// AtomicRenderer provides thread-safe, flicker-free terminal rendering.
// Each call to Render atomically erases the previous output and writes new content.
// Non-TTY environments (CI, pipes) get append-only output with no ANSI escapes.
type AtomicRenderer struct {
	mu        sync.Mutex
	lineCount int
	isTTY     bool
}

// New creates an AtomicRenderer that auto-detects TTY on stdout.
func New() *AtomicRenderer {
	return &AtomicRenderer{
		isTTY: term.IsTerminal(int(os.Stdout.Fd())),
	}
}

// IsTTY returns whether the renderer is in TTY mode.
func (r *AtomicRenderer) IsTTY() bool {
	return r.isTTY
}

// Render atomically erases the previous output and writes the content produced
// by fn. The callback writes to a buffer; the renderer handles erase + flush.
// Thread-safe — multiple goroutines may call Render concurrently.
func (r *AtomicRenderer) Render(fn func(w io.Writer)) {
	var buf bytes.Buffer
	fn(&buf)

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.isTTY {
		r.eraseLocked()
	}

	os.Stdout.Write(buf.Bytes())
	r.lineCount = countLines(buf.Bytes())
}

// Clear erases the current rendered content from the screen.
func (r *AtomicRenderer) Clear() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.eraseLocked()
	r.lineCount = 0
}

// Lines returns the number of lines currently on screen.
func (r *AtomicRenderer) Lines() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lineCount
}

// eraseLocked moves the cursor up and clears to end of screen.
// Must be called with r.mu held.
func (r *AtomicRenderer) eraseLocked() {
	if r.lineCount > 0 {
		fmt.Fprintf(os.Stdout, "\033[%dA", r.lineCount)
	}
	fmt.Fprint(os.Stdout, "\033[J")
}

// countLines counts the number of newline characters in b.
func countLines(b []byte) int {
	n := 0
	for _, c := range b {
		if c == '\n' {
			n++
		}
	}
	return n
}

// TermWidth returns the current terminal width, defaulting to 80.
func TermWidth() int {
	w, _, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || w <= 0 {
		return 80
	}
	return w
}
