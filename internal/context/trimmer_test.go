package context

import (
	"strings"
	"testing"
)

func TestEstimateTokens(t *testing.T) {
	tests := []struct {
		text string
		want int
	}{
		{"", 0},
		{"hello", 1},                   // 5 / 3.5 = 1
		{strings.Repeat("a", 35), 10},  // 35 / 3.5 = 10
		{strings.Repeat("x", 100), 28}, // 100 / 3.5 ≈ 28
	}
	for _, tt := range tests {
		got := EstimateTokens(tt.text)
		if got != tt.want {
			t.Errorf("EstimateTokens(%d chars) = %d, want %d", len(tt.text), got, tt.want)
		}
	}
}

func TestTrimToTokenBudgetPriority(t *testing.T) {
	sections := []Section{
		{Content: strings.Repeat("a", 35), Priority: 1, Label: "low"},   // 10 tokens
		{Content: strings.Repeat("b", 35), Priority: 10, Label: "high"}, // 10 tokens
		{Content: strings.Repeat("c", 35), Priority: 5, Label: "mid"},   // 10 tokens
	}

	result := TrimToTokenBudget(sections, 20)

	if len(result) != 2 {
		t.Fatalf("expected 2 sections, got %d", len(result))
	}
	if result[0].Label != "high" {
		t.Errorf("first section should be 'high', got %q", result[0].Label)
	}
	if result[1].Label != "mid" {
		t.Errorf("second section should be 'mid', got %q", result[1].Label)
	}
}

func TestTrimToTokenBudgetFitsAll(t *testing.T) {
	sections := []Section{
		{Content: "short", Priority: 1},
		{Content: "also short", Priority: 2},
	}

	result := TrimToTokenBudget(sections, 1000)
	if len(result) != 2 {
		t.Errorf("expected all 2 sections to fit, got %d", len(result))
	}
}

func TestTrimToTokenBudgetEmpty(t *testing.T) {
	result := TrimToTokenBudget(nil, 100)
	if len(result) != 0 {
		t.Errorf("expected 0 sections for nil input, got %d", len(result))
	}
}

func TestTrimToTokenBudgetTruncatesLast(t *testing.T) {
	// Section with 100 chars = ~28 tokens. Budget = 15 tokens, so it must truncate.
	bigContent := "func main() {\n" + strings.Repeat("  x := 1\n", 10) + "}\n"
	sections := []Section{
		{Content: bigContent, Priority: 1, Label: "big"},
	}

	result := TrimToTokenBudget(sections, 5)
	if len(result) != 1 {
		t.Fatalf("expected 1 truncated section, got %d", len(result))
	}
	if len(result[0].Content) >= len(bigContent) {
		t.Error("section should have been truncated")
	}
}

func TestTruncateAtBoundary(t *testing.T) {
	content := "package main\n\nfunc foo() {\n  return 1\n}\n\nfunc bar() {\n  return 2\n}\n"

	// Truncate at about 50 chars — should cut at a func boundary.
	result := truncateAtBoundary(content, 50)
	if !strings.Contains(result, "func foo") {
		t.Error("truncated content should include first function")
	}
	if strings.Contains(result, "func bar") {
		t.Error("truncated content should NOT include second function")
	}
	if !strings.HasSuffix(result, "// … truncated") {
		t.Error("truncated content should end with truncation marker")
	}
}

func TestTruncateAtBoundaryNoTruncationNeeded(t *testing.T) {
	content := "short content"
	result := truncateAtBoundary(content, 100)
	if result != content {
		t.Error("should return original content when no truncation needed")
	}
}

func TestTruncateAtBoundaryFallsBackToLine(t *testing.T) {
	content := "line one\nline two\nline three\nline four\n"
	// Truncate at 25 chars. No func boundaries, should fall back to last newline.
	result := truncateAtBoundary(content, 25)
	if !strings.HasSuffix(result, "// … truncated") {
		t.Error("should end with truncation marker")
	}
}

// ── Benchmarks ────────────────────────────────────────────────────────────────

// BenchmarkEstimateTokens benchmarks token estimation on a 1000-char code string.
func BenchmarkEstimateTokens(b *testing.B) {
	code := strings.Repeat("func foo() { return 42 }\n", 40) // ~1000 chars
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = EstimateTokens(code)
	}
}

// BenchmarkTrimToTokenBudget benchmarks priority-based trimming of 10 sections.
func BenchmarkTrimToTokenBudget(b *testing.B) {
	sections := make([]Section, 10)
	for i := range sections {
		sections[i] = Section{
			Content:  strings.Repeat("x", 140), // ~40 tokens each
			Priority: i + 1,
			Label:    "section",
		}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = TrimToTokenBudget(sections, 4000)
	}
}

// BenchmarkTruncateAtBoundary benchmarks boundary-aware truncation of a 5000-char Go file.
func BenchmarkTruncateAtBoundary(b *testing.B) {
	// Build a realistic-looking Go file (~5000 chars).
	var sb strings.Builder
	sb.WriteString("package main\n\n")
	for i := 0; i < 50; i++ {
		sb.WriteString("func doWork() {\n\tx := computeValue()\n\t_ = x\n}\n\n")
	}
	content := sb.String()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = truncateAtBoundary(content, 2000)
	}
}
