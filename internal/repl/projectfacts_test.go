package repl

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDetectProjectFacts_Python(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "requirements.txt"), []byte("flask==2.3\nrequests\n"), 0644)
	os.WriteFile(filepath.Join(dir, "app.py"), []byte("from flask import Flask\napp = Flask(__name__)\n"), 0644)
	os.WriteFile(filepath.Join(dir, "models.py"), []byte("class User:\n    pass\nclass Group:\n    pass\ndef get_all():\n    pass\n"), 0644)

	facts := detectProjectFacts(dir)
	if facts == "" {
		t.Fatal("expected project facts, got empty")
	}
	if !strings.Contains(facts, "Language: Python") {
		t.Errorf("expected Python language, got:\n%s", facts)
	}
	if !strings.Contains(facts, "Framework: Flask") {
		t.Errorf("expected Flask framework, got:\n%s", facts)
	}
	if !strings.Contains(facts, "Entry point: app.py") {
		t.Errorf("expected entry point app.py, got:\n%s", facts)
	}
	if !strings.Contains(facts, "User") || !strings.Contains(facts, "Group") {
		t.Errorf("expected exports User, Group, got:\n%s", facts)
	}
}

func TestDetectProjectFacts_Go(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/test\ngo 1.21\n"), 0644)
	os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\nfunc main() {}\n"), 0644)

	facts := detectProjectFacts(dir)
	if !strings.Contains(facts, "Language: Go") {
		t.Errorf("expected Go, got:\n%s", facts)
	}
	if !strings.Contains(facts, "Entry point: main.go") {
		t.Errorf("expected entry point main.go, got:\n%s", facts)
	}
}

func TestDetectProjectFacts_Node(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"dependencies":{"express":"^4.0"}}`), 0644)
	os.WriteFile(filepath.Join(dir, "index.js"), []byte("const express = require('express');\n"), 0644)

	facts := detectProjectFacts(dir)
	if !strings.Contains(facts, "Language: Node.js") {
		t.Errorf("expected Node.js, got:\n%s", facts)
	}
	if !strings.Contains(facts, "Framework: Express") {
		t.Errorf("expected Express, got:\n%s", facts)
	}
}

func TestDetectProjectFacts_Empty(t *testing.T) {
	dir := t.TempDir()
	facts := detectProjectFacts(dir)
	if facts != "" {
		t.Errorf("expected empty for empty dir, got: %s", facts)
	}
}

func TestAnalyzeError_PythonImport(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "models.py"), []byte("class User:\n    pass\nclass Group:\n    pass\n"), 0644)

	input := `Traceback (most recent call last):
  File "src/api.py", line 1, in <module>
    from models import User, SplitType
ImportError: cannot import name 'SplitType' from 'models'`

	analysis := analyzeError(input, dir)
	if analysis == "" {
		t.Fatal("expected error analysis, got empty")
	}
	if !strings.Contains(analysis, "ImportError") {
		t.Error("expected ImportError type")
	}
	if !strings.Contains(analysis, "SplitType") {
		t.Error("expected SplitType mentioned")
	}
	if !strings.Contains(analysis, "User") {
		t.Error("expected available exports to include User")
	}
}

func TestAnalyzeError_GoUndefined(t *testing.T) {
	input := `./main.go:15:2: undefined: FooBar`
	analysis := analyzeError(input, t.TempDir())
	if !strings.Contains(analysis, "undefined symbol") {
		t.Errorf("expected undefined symbol analysis, got:\n%s", analysis)
	}
	if !strings.Contains(analysis, "FooBar") {
		t.Errorf("expected FooBar mentioned, got:\n%s", analysis)
	}
}

func TestAnalyzeError_NoError(t *testing.T) {
	analysis := analyzeError("how do I use flask?", t.TempDir())
	if analysis != "" {
		t.Errorf("expected empty for non-error input, got: %s", analysis)
	}
}

func TestSanityCheckResponse_WrongTooling(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "requirements.txt"), []byte("flask\n"), 0644)

	correction := sanityCheckResponse("First run npm install to install dependencies", dir)
	if correction == "" {
		t.Fatal("expected correction for npm in Python project")
	}
	if !strings.Contains(correction, "pip") {
		t.Error("expected pip suggestion in correction")
	}
}

func TestSanityCheckResponse_CantReadFiles(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "requirements.txt"), []byte("flask\n"), 0644)

	correction := sanityCheckResponse("I can't read files on your system, please paste the contents", dir)
	if correction == "" {
		t.Fatal("expected correction for 'can't read files'")
	}
}

func TestSanityCheckResponse_OK(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "requirements.txt"), []byte("flask\n"), 0644)

	correction := sanityCheckResponse("Run pip install -r requirements.txt", dir)
	if correction != "" {
		t.Errorf("expected no correction for correct response, got: %s", correction)
	}
}

func TestSanityCheckResponse_Polyglot(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "requirements.txt"), []byte("flask\n"), 0644)
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{}`), 0644)

	// Polyglot project — should not trigger correction.
	correction := sanityCheckResponse("npm install && pip install", dir)
	if correction != "" {
		t.Errorf("expected no correction for polyglot project, got: %s", correction)
	}
}

func TestExtractPythonExports(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "models.py")
	os.WriteFile(path, []byte(`
class User:
    pass

class Group(Base):
    pass

def get_all():
    return []

def _private():
    pass

MAX_RETRIES = 3
`), 0644)

	exports := extractPythonExports(path)
	if len(exports) != 4 {
		t.Fatalf("expected 4 exports, got %d: %v", len(exports), exports)
	}
	expected := []string{"User", "Group", "get_all", "MAX_RETRIES"}
	for i, e := range expected {
		if exports[i] != e {
			t.Errorf("export[%d] = %q, want %q", i, exports[i], e)
		}
	}
}
