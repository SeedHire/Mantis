package embeddings

import "testing"

func TestExtractSymbolChunk_NegativeSliceBound(t *testing.T) {
	// BUG: when maxChars < sb.Len(), maxChars-sb.Len() goes negative.
	// This happens when a line exactly fills the budget (no truncation branch),
	// then the next line triggers the branch with sb.Len() already > maxChars
	// due to the newline added by WriteByte('\n').
	// Example: maxChars=10, line1="0123456789" (len=10). After writing:
	//   sb = "0123456789\n" → sb.Len()=11 > maxChars=10
	// Next iteration: sb.Len()+len(line2) > maxChars → enters truncation
	//   maxChars-sb.Len() = 10-11 = -1 → lines[i][:-1] panics.
	lines := []string{
		"0123456789",  // exactly 10 chars — fills budget, but newline makes sb.Len()=11
		"second line", // triggers truncation with negative remaining
	}
	// Should not panic.
	result := extractSymbolChunk(lines, 1, 3, 10)
	if result == "" {
		t.Error("expected non-empty result")
	}
}

func TestExtractSymbolChunk_ZeroMaxChars(t *testing.T) {
	lines := []string{"func A() {}", "func B() {}"}
	// maxChars=0 should not panic.
	result := extractSymbolChunk(lines, 1, 2, 0)
	_ = result // just check it doesn't panic
}
