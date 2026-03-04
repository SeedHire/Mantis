package pipeline

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseEditBlocks_Single(t *testing.T) {
	text := "```edit:internal/router/router.go\n<<<SEARCH\nfunc old() {}\n===\nfunc new() {}\n>>>SEARCH\n```"
	edits := parseEditBlocks(text)
	if len(edits) != 1 {
		t.Fatalf("expected 1 edit, got %d", len(edits))
	}
	if edits[0].FilePath != "internal/router/router.go" {
		t.Errorf("filepath = %q", edits[0].FilePath)
	}
	if edits[0].OldText != "func old() {}" {
		t.Errorf("old = %q", edits[0].OldText)
	}
	if edits[0].NewText != "func new() {}" {
		t.Errorf("new = %q", edits[0].NewText)
	}
}

func TestParseEditBlocks_Multiple(t *testing.T) {
	text := "```edit:main.go\n<<<SEARCH\nline1\n===\nreplaced1\n>>>SEARCH\n<<<SEARCH\nline2\n===\nreplaced2\n>>>SEARCH\n```"
	edits := parseEditBlocks(text)
	if len(edits) != 2 {
		t.Fatalf("expected 2 edits, got %d", len(edits))
	}
	if edits[0].OldText != "line1" || edits[0].NewText != "replaced1" {
		t.Errorf("edit 0: old=%q new=%q", edits[0].OldText, edits[0].NewText)
	}
	if edits[1].OldText != "line2" || edits[1].NewText != "replaced2" {
		t.Errorf("edit 1: old=%q new=%q", edits[1].OldText, edits[1].NewText)
	}
}

func TestParseEditBlocks_MultilineContent(t *testing.T) {
	text := "```edit:handler.go\n<<<SEARCH\nfunc Handle() {\n\treturn nil\n}\n===\nfunc Handle() error {\n\treturn fmt.Errorf(\"not implemented\")\n}\n>>>SEARCH\n```"
	edits := parseEditBlocks(text)
	if len(edits) != 1 {
		t.Fatalf("expected 1 edit, got %d", len(edits))
	}
	if !strings.Contains(edits[0].OldText, "return nil") {
		t.Errorf("old should contain 'return nil': %q", edits[0].OldText)
	}
	if !strings.Contains(edits[0].NewText, "not implemented") {
		t.Errorf("new should contain 'not implemented': %q", edits[0].NewText)
	}
}

func TestParseEditBlocks_NoEdits(t *testing.T) {
	text := "```go:main.go\npackage main\n```"
	edits := parseEditBlocks(text)
	if len(edits) != 0 {
		t.Fatalf("expected 0 edits, got %d", len(edits))
	}
}

func TestApplyEdits_Success(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "main.go")
	os.WriteFile(file, []byte("package main\n\nfunc old() {}\n"), 0o644)

	edits := []EditBlock{{FilePath: "main.go", OldText: "func old() {}", NewText: "func new() {}"}}
	modified, warnings := applyEdits(edits, root)

	if len(warnings) != 0 {
		t.Errorf("unexpected warnings: %v", warnings)
	}
	if len(modified) != 1 {
		t.Fatalf("expected 1 modified, got %d", len(modified))
	}

	data, _ := os.ReadFile(file)
	if !strings.Contains(string(data), "func new() {}") {
		t.Errorf("file not updated: %s", data)
	}
}

func TestApplyEdits_NotFound(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "f.go"), []byte("package main\n"), 0o644)

	edits := []EditBlock{{FilePath: "f.go", OldText: "nonexistent", NewText: "replaced"}}
	modified, warnings := applyEdits(edits, root)

	if len(modified) != 0 {
		t.Errorf("expected 0 modified, got %d", len(modified))
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "not found") {
		t.Errorf("expected 'not found' warning, got %v", warnings)
	}
}

func TestApplyEdits_Ambiguous(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "f.go"), []byte("foo bar foo"), 0o644)

	edits := []EditBlock{{FilePath: "f.go", OldText: "foo", NewText: "baz"}}
	_, warnings := applyEdits(edits, root)

	if len(warnings) != 1 || !strings.Contains(warnings[0], "ambiguous") {
		t.Errorf("expected ambiguous warning, got %v", warnings)
	}
}

func TestApplyEdits_PathTraversal(t *testing.T) {
	root := t.TempDir()
	edits := []EditBlock{{FilePath: "../../etc/passwd", OldText: "root", NewText: "evil"}}
	_, warnings := applyEdits(edits, root)

	if len(warnings) != 1 || !strings.Contains(warnings[0], "unsafe") {
		t.Errorf("expected unsafe path warning, got %v", warnings)
	}
}

func TestExtractAndApplyChanges_MixedFormats(t *testing.T) {
	root := t.TempDir()

	// Create an existing file for edit blocks.
	os.WriteFile(filepath.Join(root, "existing.go"), []byte("package main\n\nfunc old() {}\n"), 0o644)

	text := "Here's the implementation:\n\n" +
		"```edit:existing.go\n<<<SEARCH\nfunc old() {}\n===\nfunc updated() {}\n>>>SEARCH\n```\n\n" +
		"```go:newfile.go\npackage main\n\nfunc brand() {}\n```"

	paths := extractAndApplyChanges(text, root)

	if len(paths) != 2 {
		t.Fatalf("expected 2 paths, got %d: %v", len(paths), paths)
	}

	// Verify edit was applied.
	data, _ := os.ReadFile(filepath.Join(root, "existing.go"))
	if !strings.Contains(string(data), "func updated() {}") {
		t.Errorf("edit not applied: %s", data)
	}

	// Verify new file was written.
	data, _ = os.ReadFile(filepath.Join(root, "newfile.go"))
	if !strings.Contains(string(data), "func brand() {}") {
		t.Errorf("new file not written: %s", data)
	}
}

func TestExtractAndApplyChanges_WholeFileOnly(t *testing.T) {
	root := t.TempDir()
	text := "```go:app.go\npackage main\n\nfunc main() {}\n```"
	paths := extractAndApplyChanges(text, root)

	if len(paths) != 1 {
		t.Fatalf("expected 1 path, got %d", len(paths))
	}
	data, _ := os.ReadFile(filepath.Join(root, "app.go"))
	if !strings.Contains(string(data), "func main() {}") {
		t.Errorf("file not written: %s", data)
	}
}

func TestExtractAndApplyChanges_EditBlockSkipsDuplicate(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "f.go"), []byte("package main\nvar x = 1\n"), 0o644)

	// Both an edit block and a whole-file block for the same file.
	// Edit block should take priority; whole-file block should be skipped.
	text := "```edit:f.go\n<<<SEARCH\nvar x = 1\n===\nvar x = 42\n>>>SEARCH\n```\n\n" +
		"```go:f.go\npackage main\nvar x = 999\n```"

	paths := extractAndApplyChanges(text, root)

	if len(paths) != 1 {
		t.Fatalf("expected 1 path (deduped), got %d", len(paths))
	}

	data, _ := os.ReadFile(filepath.Join(root, "f.go"))
	if !strings.Contains(string(data), "var x = 42") {
		t.Errorf("expected edit block result, got: %s", data)
	}
	if strings.Contains(string(data), "999") {
		t.Error("whole-file block should NOT have been applied over edit block")
	}
}

func TestExtractAndApplyChanges_EditOnly(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "svc.go"), []byte("package svc\n\nfunc Run() { panic(\"todo\") }\n"), 0o644)

	text := "```edit:svc.go\n<<<SEARCH\nfunc Run() { panic(\"todo\") }\n===\nfunc Run() error { return nil }\n>>>SEARCH\n```"
	paths := extractAndApplyChanges(text, root)

	if len(paths) != 1 {
		t.Fatalf("expected 1 path, got %d", len(paths))
	}
	data, _ := os.ReadFile(filepath.Join(root, "svc.go"))
	if !strings.Contains(string(data), "return nil") {
		t.Errorf("edit not applied: %s", data)
	}
}
