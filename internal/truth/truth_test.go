package truth

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNew_CreatesIndex(t *testing.T) {
	dir := t.TempDir()
	w := New(dir)
	if w == nil {
		t.Fatal("expected non-nil Writer")
	}
	if w.FileCount() != 0 {
		t.Errorf("expected 0 files, got %d", w.FileCount())
	}
}

func TestFindClosest_EmptyName(t *testing.T) {
	dir := t.TempDir()
	w := New(dir)
	result := w.FindClosest("", 5)
	if len(result) != 0 {
		t.Errorf("expected empty result for empty name, got %v", result)
	}
}

func TestFindClosest_ZeroLimit(t *testing.T) {
	dir := t.TempDir()
	w := New(dir)
	result := w.FindClosest("foo", 0)
	if len(result) != 0 {
		t.Errorf("expected empty result for zero limit, got %v", result)
	}
}

func TestSymbolExists_NotFound(t *testing.T) {
	dir := t.TempDir()
	w := New(dir)
	if w.SymbolExists("NonExistent") {
		t.Error("expected false for non-existent symbol")
	}
}

func TestUpdateFile_IndexesGoFile(t *testing.T) {
	dir := t.TempDir()

	// Create a Go file in the project.
	goFile := filepath.Join(dir, "main.go")
	code := `package main

func Hello() string {
	return "hello"
}
`
	if err := os.WriteFile(goFile, []byte(code), 0o644); err != nil {
		t.Fatal(err)
	}

	// Also need go.mod for the Go parser.
	goMod := filepath.Join(dir, "go.mod")
	if err := os.WriteFile(goMod, []byte("module example.com/test\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	w := New(dir)
	w.UpdateFile(goFile)

	if w.FileCount() != 1 {
		t.Errorf("expected 1 file indexed, got %d", w.FileCount())
	}

	if !w.SymbolExists("Hello") {
		t.Error("expected Hello symbol to exist after indexing")
	}
}

func TestContextSnippet_EmptyIndex(t *testing.T) {
	dir := t.TempDir()
	w := New(dir)
	snippet := w.ContextSnippet()
	if snippet != "" {
		t.Errorf("expected empty snippet for empty index, got %q", snippet)
	}
}

func TestContextSnippetN_Limits(t *testing.T) {
	dir := t.TempDir()
	w := New(dir)

	// Manually add entries to the index.
	w.mu.Lock()
	for i := 0; i < 20; i++ {
		w.index[filepath.Join(dir, "file"+string(rune('a'+i))+".go")] = FileEntry{
			Hash: "abc123",
			Functions: []FuncSig{
				{Name: "Func" + string(rune('A'+i))},
			},
		}
	}
	w.mu.Unlock()

	snippet := w.ContextSnippetN(5, 10000)
	if snippet == "" {
		t.Error("expected non-empty snippet")
	}
	// Should not contain all 20 files — capped at 5.
}

func TestContextSnippetForTier(t *testing.T) {
	dir := t.TempDir()
	w := New(dir)

	// Empty index should return "" for all tiers.
	tiers := []string{"trivial", "fast", "code", "reason", "heavy", "max", "unknown"}
	for _, tier := range tiers {
		if snippet := w.ContextSnippetForTier(tier); snippet != "" {
			t.Errorf("expected empty snippet for tier %s with empty index", tier)
		}
	}
}

func TestRemoveFile(t *testing.T) {
	dir := t.TempDir()
	w := New(dir)

	// Manually add an entry.
	w.mu.Lock()
	w.index["test.go"] = FileEntry{Hash: "abc"}
	w.mu.Unlock()

	if w.FileCount() != 1 {
		t.Fatal("expected 1 file")
	}

	w.RemoveFile("test.go")

	if w.FileCount() != 0 {
		t.Errorf("expected 0 files after remove, got %d", w.FileCount())
	}
}

func TestFindClosest_PrefixMatch(t *testing.T) {
	dir := t.TempDir()
	w := New(dir)

	w.mu.Lock()
	w.index["test.go"] = FileEntry{
		Functions: []FuncSig{
			{Name: "HandleRequest"},
			{Name: "HandleResponse"},
			{Name: "ParseInput"},
		},
	}
	w.mu.Unlock()

	results := w.FindClosest("Hand", 5)
	if len(results) == 0 {
		t.Fatal("expected matches for 'Hand'")
	}
	// Both HandleRequest and HandleResponse should match.
	found := 0
	for _, r := range results {
		if r == "HandleRequest" || r == "HandleResponse" {
			found++
		}
	}
	if found < 2 {
		t.Errorf("expected both Handle* functions, found %d in %v", found, results)
	}
}

func TestFindClosest_LimitEnforced(t *testing.T) {
	dir := t.TempDir()
	w := New(dir)

	w.mu.Lock()
	w.index["test.go"] = FileEntry{
		Functions: []FuncSig{
			{Name: "FooA"}, {Name: "FooB"}, {Name: "FooC"},
			{Name: "FooD"}, {Name: "FooE"},
		},
	}
	w.mu.Unlock()

	results := w.FindClosest("Foo", 2)
	if len(results) > 2 {
		t.Errorf("expected at most 2 results, got %d", len(results))
	}
}

func TestBuildFull_SkipsDirs(t *testing.T) {
	dir := t.TempDir()

	// Create directories that should be skipped.
	for _, skip := range []string{".git", "node_modules", "vendor"} {
		os.MkdirAll(filepath.Join(dir, skip), 0o755)
		os.WriteFile(filepath.Join(dir, skip, "test.go"), []byte("package test\n"), 0o644)
	}

	w := New(dir)
	err := w.BuildFull(dir)
	if err != nil {
		t.Fatal(err)
	}

	// Files in skipped dirs should not be indexed.
	if w.FileCount() != 0 {
		t.Errorf("expected 0 indexed files (all in skipped dirs), got %d", w.FileCount())
	}
}
