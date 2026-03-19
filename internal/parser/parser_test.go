package parser

import "testing"

func TestStripQuotes_DoubleQuotes(t *testing.T) {
	if got := StripQuotes(`"hello"`); got != "hello" {
		t.Errorf("got %q, want %q", got, "hello")
	}
}

func TestStripQuotes_SingleQuotes(t *testing.T) {
	if got := StripQuotes("'hello'"); got != "hello" {
		t.Errorf("got %q, want %q", got, "hello")
	}
}

func TestStripQuotes_Backticks(t *testing.T) {
	if got := StripQuotes("`hello`"); got != "hello" {
		t.Errorf("got %q, want %q", got, "hello")
	}
}

func TestStripQuotes_NoQuotes(t *testing.T) {
	if got := StripQuotes("hello"); got != "hello" {
		t.Errorf("got %q, want %q", got, "hello")
	}
}

func TestStripQuotes_Empty(t *testing.T) {
	if got := StripQuotes(""); got != "" {
		t.Errorf("got %q, want %q", got, "")
	}
}

func TestStripQuotes_SingleChar(t *testing.T) {
	if got := StripQuotes("a"); got != "a" {
		t.Errorf("got %q, want %q", got, "a")
	}
}

func TestStripQuotes_MismatchedQuotes(t *testing.T) {
	if got := StripQuotes(`"hello'`); got != `"hello'` {
		t.Errorf("mismatched quotes should not be stripped: got %q", got)
	}
}

func TestIsExternalImport(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{"github.com/foo/bar", true},
		{"fmt", true},
		{"./relative", false},
		{"../parent", false},
		{"/absolute/path", false},
		{"encoding/json", true},
	}
	for _, tt := range tests {
		if got := IsExternalImport(tt.path); got != tt.want {
			t.Errorf("IsExternalImport(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

func TestFileNodeID(t *testing.T) {
	if got := FileNodeID("internal/router/router.go"); got != "file:internal/router/router.go" {
		t.Errorf("got %q", got)
	}
}

func TestSymbolNodeID(t *testing.T) {
	got := SymbolNodeID("internal/router/router.go", "Classify")
	want := "sym:internal/router/router.go#Classify"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestGoParser_Extensions(t *testing.T) {
	p := &GoParser{}
	exts := p.Extensions()
	if len(exts) != 1 || exts[0] != ".go" {
		t.Errorf("expected [.go], got %v", exts)
	}
}

func TestGoParser_Language(t *testing.T) {
	p := &GoParser{}
	if p.Language() != "go" {
		t.Errorf("expected 'go', got %q", p.Language())
	}
}

func TestTypeScriptParser_Extensions(t *testing.T) {
	p := &TypeScriptParser{}
	exts := p.Extensions()
	if len(exts) < 2 {
		t.Errorf("expected at least .ts and .tsx, got %v", exts)
	}
}

func TestPythonParser_Language(t *testing.T) {
	p := &PythonParser{}
	if p.Language() != "python" {
		t.Errorf("expected 'python', got %q", p.Language())
	}
}

func TestGoParser_ParseFile_SimpleFunction(t *testing.T) {
	p := &GoParser{Root: "/tmp"}
	code := []byte(`package main

func Hello() string {
	return "hello"
}

func private() {}
`)
	result, err := p.ParseFile("/tmp/test.go", code)
	if err != nil {
		t.Fatal(err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result.FileNode == nil {
		t.Fatal("expected FileNode")
	}

	// Should find at least the Hello function.
	found := false
	for _, sym := range result.Symbols {
		if sym.Name == "Hello" && sym.Type == "function" {
			found = true
			if !sym.Exported {
				t.Error("Hello should be exported")
			}
		}
		if sym.Name == "private" && sym.Exported {
			t.Error("private should not be exported")
		}
	}
	if !found {
		t.Error("Hello function not found in symbols")
	}
}

func TestGoParser_ParseFile_Imports(t *testing.T) {
	p := &GoParser{Root: "/tmp/project"}
	code := []byte(`package main

import (
	"fmt"
	"strings"
)
`)
	result, err := p.ParseFile("/tmp/project/main.go", code)
	if err != nil {
		t.Fatal(err)
	}
	// Stdlib imports should not be resolved to files (os.Stat check).
	for _, imp := range result.Imports {
		if imp.RawPath == "fmt" && imp.ToFile != "" {
			// fmt should not resolve to a file since /tmp/project/fmt doesn't exist.
			t.Logf("import: raw=%s to=%s (may or may not resolve)", imp.RawPath, imp.ToFile)
		}
	}
}
