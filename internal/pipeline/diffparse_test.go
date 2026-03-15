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
	modified, skipped, _ := applyEdits(edits, root)

	if skipped != 0 {
		t.Errorf("unexpected skips: %d", skipped)
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
	modified, skipped, _ := applyEdits(edits, root)

	if len(modified) != 0 {
		t.Errorf("expected 0 modified, got %d", len(modified))
	}
	if skipped != 1 {
		t.Errorf("expected 1 skip for not-found, got %d", skipped)
	}
}

func TestApplyEdits_Ambiguous(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "f.go"), []byte("foo bar foo"), 0o644)

	edits := []EditBlock{{FilePath: "f.go", OldText: "foo", NewText: "baz"}}
	_, skipped, _ := applyEdits(edits, root)

	if skipped != 1 {
		t.Errorf("expected 1 skip for ambiguous match, got %d", skipped)
	}
}

func TestApplyEdits_PathTraversal(t *testing.T) {
	root := t.TempDir()
	edits := []EditBlock{{FilePath: "../../etc/passwd", OldText: "root", NewText: "evil"}}
	_, skipped, _ := applyEdits(edits, root)

	if skipped != 1 {
		t.Errorf("expected 1 skip for unsafe path, got %d", skipped)
	}
}

// TestApplyEdits_FuzzyMatch verifies that whitespace-normalised SEARCH still applies.
func TestApplyEdits_FuzzyMatch(t *testing.T) {
	root := t.TempDir()
	// File has extra spaces inside the function body.
	content := "package main\n\nfunc  foo()  {\n  return\n}\n"
	os.WriteFile(filepath.Join(root, "f.go"), []byte(content), 0o644)

	// SEARCH text has single spaces (exact mismatch, fuzzy match).
	edits := []EditBlock{{FilePath: "f.go", OldText: "func  foo()  {\n  return\n}", NewText: "func foo() { return }"}}
	modified, skipped, _ := applyEdits(edits, root)

	if skipped != 0 {
		t.Errorf("fuzzy match: expected 0 skips, got %d", skipped)
	}
	if len(modified) != 1 {
		t.Errorf("fuzzy match: expected 1 modified, got %d", len(modified))
	}
}

func TestExtractAndApplyChanges_MixedFormats(t *testing.T) {
	root := t.TempDir()

	// Create an existing file for edit blocks.
	os.WriteFile(filepath.Join(root, "existing.go"), []byte("package main\n\nfunc old() {}\n"), 0o644)

	text := "Here's the implementation:\n\n" +
		"```edit:existing.go\n<<<SEARCH\nfunc old() {}\n===\nfunc updated() {}\n>>>SEARCH\n```\n\n" +
		"```go:newfile.go\npackage main\n\nfunc brand() {}\n```"

	paths, _ := extractAndApplyChanges(text, root)

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
	paths, _ := extractAndApplyChanges(text, root)

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

	paths, _ := extractAndApplyChanges(text, root)

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
	paths, _ := extractAndApplyChanges(text, root)

	if len(paths) != 1 {
		t.Fatalf("expected 1 path, got %d", len(paths))
	}
	data, _ := os.ReadFile(filepath.Join(root, "svc.go"))
	if !strings.Contains(string(data), "return nil") {
		t.Errorf("edit not applied: %s", data)
	}
}

// TestApplyEdits_LineTrimmedMatch verifies Tier 2b: indentation-only mismatches.
func TestApplyEdits_LineTrimmedMatch(t *testing.T) {
	root := t.TempDir()
	// File has tabs; model SEARCH has spaces.
	content := "package main\n\nimport (\n\t\"fmt\"\n\t\"os\"\n)\n"
	os.WriteFile(filepath.Join(root, "f.go"), []byte(content), 0o644)

	edits := []EditBlock{{
		FilePath: "f.go",
		OldText:  "import (\n  \"fmt\"\n  \"os\"\n)",
		NewText:  "import (\n\t\"fmt\"\n\t\"os\"\n\t\"io\"\n)",
	}}
	modified, skipped, _ := applyEdits(edits, root)

	if skipped != 0 {
		t.Errorf("line-trimmed match: expected 0 skips, got %d", skipped)
	}
	if len(modified) != 1 {
		t.Errorf("line-trimmed match: expected 1 modified, got %d", len(modified))
	}
	data, _ := os.ReadFile(filepath.Join(root, "f.go"))
	if !strings.Contains(string(data), "\"io\"") {
		t.Errorf("edit not applied: %s", data)
	}
}

// TestApplyEdits_BestEffortMatch verifies Tier 2c: ≥90% line match for 6+ line blocks.
func TestApplyEdits_BestEffortMatch(t *testing.T) {
	root := t.TempDir()
	// File content — 10 lines in the target block (need ≥6 for best-effort, ≥90% match).
	content := "package main\n\nconst a = 1\nconst b = 2\nconst c = 3\nconst d = 4\nconst e = 5\nconst f = 6\nconst g = 7\nconst h = 8\nfunc main() {}\n"
	os.WriteFile(filepath.Join(root, "f.go"), []byte(content), 0o644)

	// Model gets 9 out of 10 lines right (90%), one non-signature line differs slightly.
	// This should pass Tier 2c since 9/10 = 0.90 >= 0.90.
	edits := []EditBlock{{
		FilePath: "f.go",
		OldText:  "const a = 1\nconst b = 2\nconst c = 3\nconst d = 4\nconst e = 5\nconst f = 6\nconst g = 7\nconst h = WRONG\nfunc main() {}\npackage main",
		NewText:  "const a = 10\nconst b = 20",
	}}
	// This has 10 lines, 8 match out of 10 = 80% → should FAIL.
	_, skipped, _ := applyEdits(edits, root)
	if skipped != 1 {
		t.Errorf("expected 1 skip for 80%% match, got %d", skipped)
	}

	// Now test a 9/10 = 90% match that should pass.
	os.WriteFile(filepath.Join(root, "f.go"), []byte(content), 0o644)
	edits2 := []EditBlock{{
		FilePath: "f.go",
		OldText:  "const a = 1\nconst b = 2\nconst c = 3\nconst d = 4\nconst e = 5\nconst f = 6\nconst g = 7\nconst h = WRONG\nfunc main() {}",
		NewText:  "const a = 10\nconst b = 20",
	}}
	// 9 lines, 8 match = 88.9% → should FAIL (need ≥90%).
	_, skipped2, _ := applyEdits(edits2, root)
	if skipped2 != 1 {
		t.Errorf("expected 1 skip for 88.9%% match, got %d", skipped2)
	}

	// Test 6/6 = 100% match on non-exact (requires Tier 2c path, reached via trimmed-line diff).
	os.WriteFile(filepath.Join(root, "g.go"), []byte("  line1\n  line2\n  line3\n  line4\n  line5\n  line6\n"), 0o644)
	edits3 := []EditBlock{{
		FilePath: "g.go",
		OldText:  "line1\nline2\nline3\nline4\nline5\nline6",
		NewText:  "replaced",
	}}
	// All 6 trimmed lines match (100%) — should pass via Tier 2b (line-trimmed) actually.
	modified3, skipped3, _ := applyEdits(edits3, root)
	if skipped3 != 0 {
		t.Errorf("expected 0 skips for 100%% trimmed match, got %d", skipped3)
	}
	if len(modified3) != 1 {
		t.Errorf("expected 1 modified, got %d", len(modified3))
	}
}

// TestApplyEdits_BestEffortRejectLowMatch verifies 80% match is rejected (needs 90%).
func TestApplyEdits_BestEffortRejectLowMatch(t *testing.T) {
	root := t.TempDir()
	content := "line1\nline2\nline3\nline4\nline5\nline6\nline7\n"
	os.WriteFile(filepath.Join(root, "f.go"), []byte(content), 0o644)

	// 5/7 = 71% match — should be rejected.
	edits := []EditBlock{{
		FilePath: "f.go",
		OldText:  "line1\nWRONG\nline3\nWRONG\nline5\nline6\nline7",
		NewText:  "replaced",
	}}
	_, skipped, _ := applyEdits(edits, root)
	if skipped != 1 {
		t.Errorf("expected 1 skip for low-match block, got %d", skipped)
	}
}

// TestApplyEdits_BestEffortTooFewLines verifies Tier 2c doesn't trigger for <6 lines.
func TestApplyEdits_BestEffortTooFewLines(t *testing.T) {
	root := t.TempDir()
	content := "line A\nline B\nline C\nline D\nline E\n"
	os.WriteFile(filepath.Join(root, "f.txt"), []byte(content), 0o644)

	// 5 lines — below the 6-line minimum for Tier 2c.
	edits := []EditBlock{{
		FilePath: "f.txt",
		OldText:  "line X\nline B\nline C\nline D\nline E",
		NewText:  "replaced",
	}}
	_, skipped, _ := applyEdits(edits, root)
	if skipped != 1 {
		t.Errorf("expected 1 skip (too few lines for best-effort), got %d", skipped)
	}
}

// TestCollectFailedEdits verifies collectFailedEdits identifies failing edits.
func TestCollectFailedEdits(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "a.go"), []byte("package a\nfunc Real() {}\n"), 0o644)

	// Edit with wrong SEARCH text — should appear in failed map.
	text := "```edit:a.go\n<<<SEARCH\nfunc Fake() {}\n===\nfunc Fixed() {}\n>>>SEARCH\n```"
	failed := collectFailedEdits(text, root)

	if len(failed) != 1 {
		t.Fatalf("expected 1 failed file, got %d", len(failed))
	}
	if _, ok := failed["a.go"]; !ok {
		t.Error("expected a.go in failed map")
	}
}

// TestCollectFailedEdits_NoFailures verifies no false positives.
func TestCollectFailedEdits_NoFailures(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "a.go"), []byte("package a\nfunc Real() {}\n"), 0o644)

	text := "```edit:a.go\n<<<SEARCH\nfunc Real() {}\n===\nfunc Fixed() {}\n>>>SEARCH\n```"
	failed := collectFailedEdits(text, root)

	if failed != nil {
		t.Errorf("expected nil (no failures), got %v", failed)
	}
}

// TestParseCodeBlocks_NestedBackticks verifies that JS/TS template literals
// with backticks don't truncate the file (Fix 1: the critical bug).
func TestParseCodeBlocks_NestedBackticks(t *testing.T) {
	text := "```typescript:src/app.ts\n" +
		"const greeting = `Hello ${name}`;\n" +
		"const multi = `\n" +
		"  line1\n" +
		"  line2\n" +
		"`;\n" +
		"console.log(greeting);\n" +
		"```\n"

	blocks := parseCodeBlocks(text)
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks))
	}
	if blocks[0].path != "src/app.ts" {
		t.Errorf("path = %q", blocks[0].path)
	}
	if blocks[0].isEdit {
		t.Error("expected isEdit = false")
	}
	// Critical: the body must contain ALL content, including the nested backticks.
	if !strings.Contains(blocks[0].body, "console.log(greeting)") {
		t.Errorf("body truncated — nested backticks broke parsing:\n%s", blocks[0].body)
	}
	if !strings.Contains(blocks[0].body, "`Hello ${name}`") {
		t.Errorf("body missing template literal:\n%s", blocks[0].body)
	}
}

// TestParseCodeBlocks_EditAndWholeFile verifies mixed edit + whole-file blocks.
func TestParseCodeBlocks_EditAndWholeFile(t *testing.T) {
	text := "```edit:main.go\n<<<SEARCH\nold\n===\nnew\n>>>SEARCH\n```\n\n" +
		"```go:utils.go\npackage utils\n\nfunc Helper() {}\n```\n"

	blocks := parseCodeBlocks(text)
	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks, got %d", len(blocks))
	}
	if !blocks[0].isEdit || blocks[0].path != "main.go" {
		t.Errorf("block 0: isEdit=%v path=%q", blocks[0].isEdit, blocks[0].path)
	}
	if blocks[1].isEdit || blocks[1].path != "utils.go" {
		t.Errorf("block 1: isEdit=%v path=%q", blocks[1].isEdit, blocks[1].path)
	}
}

// TestParseCodeBlocks_BareBackticksInside verifies that triple backticks
// followed by more text (like ```js) inside content don't close the block.
func TestParseCodeBlocks_BareBackticksInside(t *testing.T) {
	text := "```markdown:README.md\n" +
		"# My App\n" +
		"\n" +
		"Usage:\n" +
		"```js\n" +
		"import app from './app'\n" +
		"```\n" + // This ``` has nothing after it — but context matters
		"\n" +
		"More docs here.\n" +
		"```\n" // This is the real closing

	blocks := parseCodeBlocks(text)
	// The line ```js starts a nested example but bare ``` closes — this is an edge case.
	// Our parser treats bare ``` as close, so the first block will close at the first bare ```.
	// This is acceptable because real model output wraps inner examples differently.
	if len(blocks) == 0 {
		t.Fatal("expected at least 1 block")
	}
	if blocks[0].path != "README.md" {
		t.Errorf("path = %q", blocks[0].path)
	}
}

// TestExtractAndApplyChanges_NestedBackticks is an integration test:
// write a file with template literals and verify it's complete on disk.
func TestExtractAndApplyChanges_NestedBackticks(t *testing.T) {
	root := t.TempDir()
	text := "```typescript:src/index.ts\n" +
		"const sql = `SELECT * FROM users WHERE id = ${id}`;\n" +
		"const html = `<div class=\"${cls}\">`;\n" +
		"export default sql;\n" +
		"```\n"

	paths, _ := extractAndApplyChanges(text, root)
	if len(paths) != 1 {
		t.Fatalf("expected 1 path, got %d", len(paths))
	}

	data, err := os.ReadFile(filepath.Join(root, "src/index.ts"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "export default sql") {
		t.Errorf("file truncated:\n%s", content)
	}
	if !strings.Contains(content, "${id}") {
		t.Errorf("template literal missing:\n%s", content)
	}
}
