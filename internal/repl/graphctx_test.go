package repl

import (
	"testing"
)

func TestExtractMentionedFiles_ExplicitPaths(t *testing.T) {
	tests := []struct {
		input    string
		expected []string
	}{
		{"fix the bug in router.go", []string{"router.go"}},
		{"look at internal/auth/handler.go", []string{"internal/auth/handler.go"}},
		{"check src/utils.ts and src/app.tsx", []string{"src/utils.ts", "src/app.tsx"}},
		{"file `main.go` has issues", []string{"main.go"}},
	}

	for _, tt := range tests {
		files := extractMentionedFiles(tt.input, nil)
		if len(files) != len(tt.expected) {
			t.Errorf("input %q: got %d files %v, want %d %v", tt.input, len(files), files, len(tt.expected), tt.expected)
			continue
		}
		for i, f := range files {
			if f != tt.expected[i] {
				t.Errorf("input %q: file[%d] = %q, want %q", tt.input, i, f, tt.expected[i])
			}
		}
	}
}

func TestExtractMentionedFiles_ErrorPaths(t *testing.T) {
	input := "internal/router/router.go:142: undefined: Foo"
	files := extractMentionedFiles(input, nil)
	found := false
	for _, f := range files {
		if f == "internal/router/router.go" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected to find internal/router/router.go, got %v", files)
	}
}

func TestExtractMentionedFiles_NoFiles(t *testing.T) {
	files := extractMentionedFiles("what is this project about?", nil)
	if len(files) != 0 {
		t.Errorf("expected 0 files, got %v", files)
	}
}

func TestExtractMentionedFiles_Dedup(t *testing.T) {
	input := "look at main.go and then check main.go again"
	files := extractMentionedFiles(input, nil)
	count := 0
	for _, f := range files {
		if f == "main.go" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected 1 main.go, got %d in %v", count, files)
	}
}

func TestExtractMentionedFiles_SymbolPattern(t *testing.T) {
	tests := []struct {
		input    string
		hasMatch bool
	}{
		{"the Classify function", true},
		{"look at Handle()", true},
		{"function RunTests", true},
		{"just some lowercase text", false},
	}

	for _, tt := range tests {
		matches := symbolRe.FindAllStringSubmatch(tt.input, -1)
		hasMatch := len(matches) > 0
		if hasMatch != tt.hasMatch {
			t.Errorf("input %q: hasMatch=%v, want %v (matches: %v)", tt.input, hasMatch, tt.hasMatch, matches)
		}
	}
}

func TestReadFileHead(t *testing.T) {
	// readFileHead on a nonexistent file returns empty string.
	result := readFileHead("/nonexistent/path/file.go", 10)
	if result != "" {
		t.Errorf("expected empty for nonexistent file, got %q", result)
	}
}
