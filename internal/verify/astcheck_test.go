package verify

import (
	"sort"
	"strings"
	"testing"
)

func TestExtractCodeBlocks_Go(t *testing.T) {
	response := "Here's the fix:\n```go\npackage main\n\nimport \"fmt\"\n\nfunc main() {\n\tfmt.Println(\"hello\")\n\tDoSomething()\n}\n```"
	blocks := ExtractCodeBlocks(response)
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks))
	}
	b := blocks[0]
	if b.Language != "go" {
		t.Errorf("language = %q, want go", b.Language)
	}

	calls := b.FuncCalls
	sort.Strings(calls)
	if !containsStr(calls, "Println") {
		t.Errorf("expected Println in calls, got %v", calls)
	}
	if !containsStr(calls, "DoSomething") {
		t.Errorf("expected DoSomething in calls, got %v", calls)
	}

	if len(b.Imports) != 1 || b.Imports[0] != "fmt" {
		t.Errorf("imports = %v, want [fmt]", b.Imports)
	}
}

func TestExtractCodeBlocks_TypeScript(t *testing.T) {
	response := "```typescript\nimport { Router } from 'express'\n\nconst app = createApp()\napp.listen(3000)\n```"
	blocks := ExtractCodeBlocks(response)
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks))
	}
	b := blocks[0]
	if b.Language != "typescript" {
		t.Errorf("language = %q, want typescript", b.Language)
	}
	if !containsStr(b.FuncCalls, "createApp") {
		t.Errorf("expected createApp in calls, got %v", b.FuncCalls)
	}
	if !containsStr(b.FuncCalls, "listen") {
		t.Errorf("expected listen in calls, got %v", b.FuncCalls)
	}
}

func TestExtractCodeBlocks_Python(t *testing.T) {
	response := "```python\nimport os\nfrom pathlib import Path\n\nresult = process_data(input)\nprint(result)\n```"
	blocks := ExtractCodeBlocks(response)
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks))
	}
	b := blocks[0]
	if b.Language != "python" {
		t.Errorf("language = %q, want python", b.Language)
	}
	if !containsStr(b.FuncCalls, "process_data") {
		t.Errorf("expected process_data in calls, got %v", b.FuncCalls)
	}
	if !containsStr(b.FuncCalls, "print") {
		t.Errorf("expected print in calls, got %v", b.FuncCalls)
	}
}

func TestExtractCodeBlocks_WithFilePath(t *testing.T) {
	response := "```go:internal/handler.go\npackage handler\n\nfunc Handle() {}\n```"
	blocks := ExtractCodeBlocks(response)
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks))
	}
	if blocks[0].FilePath != "internal/handler.go" {
		t.Errorf("filePath = %q, want internal/handler.go", blocks[0].FilePath)
	}
}

func TestExtractCodeBlocks_MultipleBlocks(t *testing.T) {
	response := "```go\nfmt.Println(\"a\")\n```\nSome text\n```python\nprint(\"b\")\n```"
	blocks := ExtractCodeBlocks(response)
	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks, got %d", len(blocks))
	}
	if blocks[0].Language != "go" {
		t.Errorf("block 0 language = %q, want go", blocks[0].Language)
	}
	if blocks[1].Language != "python" {
		t.Errorf("block 1 language = %q, want python", blocks[1].Language)
	}
}

func TestExtractCodeBlocks_NoBlocks(t *testing.T) {
	blocks := ExtractCodeBlocks("Just some text without code blocks.")
	if len(blocks) != 0 {
		t.Errorf("expected 0 blocks, got %d", len(blocks))
	}
}

func TestExtractCodeBlocks_UnsupportedLang(t *testing.T) {
	response := "```rust\nfn main() {\n    do_thing();\n    DoThing();\n}\n```"
	blocks := ExtractCodeBlocks(response)
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks))
	}
	// Should fall back to regex
	if !containsStr(blocks[0].FuncCalls, "do_thing") && !containsStr(blocks[0].FuncCalls, "DoThing") {
		t.Errorf("expected regex fallback to find calls, got %v", blocks[0].FuncCalls)
	}
}

func TestDetectSyntaxErrors_CleanCode(t *testing.T) {
	response := "```go\npackage main\n\nfunc main() {\n\tfmt.Println(\"hello\")\n}\n```"
	errs := DetectSyntaxErrors(response)
	if len(errs) != 0 {
		t.Errorf("expected no syntax errors, got %d: %v", len(errs), errs)
	}
}

func TestDetectSyntaxErrors_BrokenGo(t *testing.T) {
	response := "```go\npackage main\n\nfunc main() {\n\tfmt.Println(\"hello\"\n}\n```"
	errs := DetectSyntaxErrors(response)
	if len(errs) == 0 {
		t.Error("expected syntax errors for broken Go code")
	}
}

// mockTruthWriter implements the interface used by CheckWithAST
type mockTruthWriter struct {
	symbols map[string]bool
}

func (m *mockTruthWriter) SymbolExists(name string) bool { return m.symbols[name] }
func (m *mockTruthWriter) FileCount() int                { return 1 }

func TestCheckWithAST_DetectsUnknown(t *testing.T) {
	tw := &mockTruthWriter{symbols: map[string]bool{
		"Println":  true,
		"NewServer": true,
	}}
	response := "```go\npackage main\n\nfunc main() {\n\tfmt.Println(\"ok\")\n\tFakeFunction()\n\tNewServer()\n}\n```"

	result := CheckWithAST(response, tw)
	if result.Clean {
		t.Error("expected unclean result (FakeFunction is unknown)")
	}
	if !containsStr(result.UnknownSymbols, "FakeFunction") {
		t.Errorf("expected FakeFunction in unknown symbols, got %v", result.UnknownSymbols)
	}
	// Println and NewServer should NOT be flagged
	if containsStr(result.UnknownSymbols, "Println") {
		t.Error("Println should not be flagged (exists in truth)")
	}
	if containsStr(result.UnknownSymbols, "NewServer") {
		t.Error("NewServer should not be flagged (exists in truth)")
	}
}

func TestCheckWithAST_AllKnown(t *testing.T) {
	tw := &mockTruthWriter{symbols: map[string]bool{
		"Handle": true,
	}}
	response := "```go\npackage main\n\nfunc main() {\n\tHandle()\n}\n```"

	result := CheckWithAST(response, tw)
	if !result.Clean {
		t.Errorf("expected clean result, got unknown: %v", result.UnknownSymbols)
	}
}

func TestCheckWithAST_LowercaseNotFlagged(t *testing.T) {
	tw := &mockTruthWriter{symbols: map[string]bool{}}
	response := "```go\npackage main\n\nfunc main() {\n\tfoo()\n\tbar()\n}\n```"

	result := CheckWithAST(response, tw)
	if !result.Clean {
		t.Errorf("lowercase functions should not be flagged, got: %v", result.UnknownSymbols)
	}
}

func TestCheckWithAST_NilWriter(t *testing.T) {
	result := CheckWithAST("```go\nFoo()\n```", nil)
	if !result.Clean {
		t.Error("nil writer should return clean")
	}
}

func TestCheckWithAST_NoCode(t *testing.T) {
	tw := &mockTruthWriter{symbols: map[string]bool{}}
	result := CheckWithAST("Just text, no code blocks.", tw)
	if !result.Clean {
		t.Error("no code blocks should return clean")
	}
}

func TestLanguageForFence(t *testing.T) {
	cases := []struct {
		tag  string
		want bool
	}{
		{"go", true},
		{"golang", true},
		{"typescript", true},
		{"ts", true},
		{"javascript", true},
		{"js", true},
		{"python", true},
		{"py", true},
		{"rust", false},
		{"java", false},
		{"", false},
	}
	for _, tc := range cases {
		got := languageForFence(tc.tag) != nil
		if got != tc.want {
			t.Errorf("languageForFence(%q) supported=%v, want %v", tc.tag, got, tc.want)
		}
	}
}

func TestRegexExtractCalls(t *testing.T) {
	code := "foo()\nBar()\nbaz(42)\nif (true) {}\nappend(s, v)"
	calls := regexExtractCalls(code)
	if !containsStr(calls, "foo") {
		t.Error("expected foo")
	}
	if !containsStr(calls, "Bar") {
		t.Error("expected Bar")
	}
	if !containsStr(calls, "baz") {
		t.Error("expected baz")
	}
	// "if" and "append" are stop words
	if containsStr(calls, "if") {
		t.Error("'if' should be filtered as stop word")
	}
	if containsStr(calls, "append") {
		t.Error("'append' should be filtered as stop word")
	}
}

func TestGoImportExtraction(t *testing.T) {
	response := "```go\npackage main\n\nimport (\n\t\"fmt\"\n\t\"os\"\n\t\"github.com/foo/bar\"\n)\n\nfunc main() {}\n```"
	blocks := ExtractCodeBlocks(response)
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks))
	}
	imports := blocks[0].Imports
	if len(imports) < 2 {
		t.Fatalf("expected at least 2 imports, got %d: %v", len(imports), imports)
	}

	hasOsOrFmt := false
	for _, imp := range imports {
		if strings.Contains(imp, "fmt") || strings.Contains(imp, "os") {
			hasOsOrFmt = true
		}
	}
	if !hasOsOrFmt {
		t.Errorf("expected fmt or os in imports, got %v", imports)
	}
}

func containsStr(slice []string, s string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
}
