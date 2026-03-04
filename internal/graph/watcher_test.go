package graph

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/seedhire/mantis/internal/parser"
)

// stubParser is a minimal Parser that accepts .txt files without touching a DB.
// Used to let isCodeFile return true so the watcher debounce is triggered
// while keeping db nil (it would only be accessed after Stop() returns
// early in the A9 fix).
type stubParser struct{}

func (stubParser) ParseFile(_ string, _ []byte) (*parser.ParseResult, error) {
	return &parser.ParseResult{}, nil
}
func (stubParser) Extensions() []string { return []string{".txt"} }
func (stubParser) Language() string     { return "stub" }

// TestWatcherStopBeforeDebounce is a regression test for A9 (BUG-13).
// Before the fix the 200ms AfterFunc callback could fire after Stop() had
// closed w.done and attempt to call w.builder.UpdateFile on a nil *DB,
// causing a panic.  The fix adds a select-on-done guard at the top of the
// callback so it returns immediately when the watcher has been stopped.
func TestWatcherStopBeforeDebounce(t *testing.T) {
	tmpDir := t.TempDir()

	// Builder with a stub parser so isCodeFile returns true for .txt files.
	// db is intentionally nil — UpdateFile would panic on nil *DB, but the
	// A9 fix ensures the callback returns early before reaching that code.
	b := &Builder{
		parsers: map[string]parser.Parser{".txt": stubParser{}},
		root:    tmpDir,
	}
	w := NewWatcher(b, tmpDir)

	if err := w.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Write a .txt file to trigger an fsnotify event → debounce timer set.
	if err := os.WriteFile(filepath.Join(tmpDir, "change.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Stop immediately — well within the 200ms debounce window.
	w.Stop()

	// Sleep past the debounce window. If the callback fires and tries to use
	// the nil *DB without the A9 fix, the test process will crash/panic here.
	time.Sleep(300 * time.Millisecond)

	// If we reach this point without a panic, the regression is covered.
}

// TestWatcherStopClean verifies that Stop() on a started watcher exits without error.
func TestWatcherStopClean(t *testing.T) {
	tmpDir := t.TempDir()
	b := &Builder{
		parsers: map[string]parser.Parser{},
		root:    tmpDir,
	}
	w := NewWatcher(b, tmpDir)
	if err := w.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	w.Stop() // must not panic
}
