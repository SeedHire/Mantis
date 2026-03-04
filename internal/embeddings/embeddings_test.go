package embeddings

import (
	"math"
	"strings"
	"testing"
)

func TestCosineSimilarityIdentical(t *testing.T) {
	a := []float64{1, 2, 3}
	b := []float64{1, 2, 3}
	sim := cosineSimilarity(a, b)
	if math.Abs(sim-1.0) > 1e-9 {
		t.Errorf("identical vectors should have similarity 1.0, got %f", sim)
	}
}

func TestCosineSimilarityOrthogonal(t *testing.T) {
	a := []float64{1, 0, 0}
	b := []float64{0, 1, 0}
	sim := cosineSimilarity(a, b)
	if math.Abs(sim) > 1e-9 {
		t.Errorf("orthogonal vectors should have similarity 0, got %f", sim)
	}
}

func TestCosineSimilarityOpposite(t *testing.T) {
	a := []float64{1, 2, 3}
	b := []float64{-1, -2, -3}
	sim := cosineSimilarity(a, b)
	if math.Abs(sim-(-1.0)) > 1e-9 {
		t.Errorf("opposite vectors should have similarity -1.0, got %f", sim)
	}
}

func TestCosineSimilarityDifferentLength(t *testing.T) {
	a := []float64{1, 2}
	b := []float64{1, 2, 3}
	sim := cosineSimilarity(a, b)
	if sim != 0 {
		t.Errorf("different length vectors should return 0, got %f", sim)
	}
}

func TestCosineSimilarityEmpty(t *testing.T) {
	sim := cosineSimilarity(nil, nil)
	if sim != 0 {
		t.Errorf("nil vectors should return 0, got %f", sim)
	}
}

func TestCosineSimilarityZeroVector(t *testing.T) {
	a := []float64{0, 0, 0}
	b := []float64{1, 2, 3}
	sim := cosineSimilarity(a, b)
	if sim != 0 {
		t.Errorf("zero vector should return 0, got %f", sim)
	}
}

func TestSplitIntoChunks(t *testing.T) {
	// splitIntoChunks splits by maxChars, not lines.
	text := "line one\nline two\nline three\nline four\nline five\nline six"
	chunks := splitIntoChunks(text, 30)

	if len(chunks) < 2 {
		t.Fatalf("expected at least 2 chunks for %d chars split at 30, got %d", len(text), len(chunks))
	}

	// Verify no chunk exceeds maxChars (except possibly the last one with rounding).
	for i, c := range chunks {
		if i < len(chunks)-1 && len(c) > 30 {
			t.Errorf("chunk %d has %d chars, exceeds maxChars=30", i, len(c))
		}
	}
}

func TestSplitIntoChunksSmall(t *testing.T) {
	text := "short"
	chunks := splitIntoChunks(text, 10)
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk for small text, got %d", len(chunks))
	}
}

func TestSplitIntoChunksEmpty(t *testing.T) {
	chunks := splitIntoChunks("", 5)
	// Should return at least 1 chunk (the empty string).
	if len(chunks) == 0 {
		t.Error("expected at least 1 chunk for empty text")
	}
}

// ── ftsQuery ──────────────────────────────────────────────────────────────────

func TestFtsQuery(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"hello world", `"hello" AND "world"`},
		{"", ""},
		// ≤2 char words are skipped from parts; fallback wraps first word as-is.
		{"go", `"go"`},
		{"is an", `"is"`}, // both ≤2 chars → no parts → fallback to first word
		{"router classify", `"router" AND "classify"`},
		// "in" is ≤2 chars and skipped; remaining words >2 chars are included.
		{"fix bug in auth handler", `"fix" AND "bug" AND "auth" AND "handler"`},
	}
	for _, tt := range tests {
		got := ftsQuery(tt.input)
		if got != tt.want {
			t.Errorf("ftsQuery(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestFtsQuerySpecialChars(t *testing.T) {
	// Quotes, parens, asterisks, dashes must be stripped.
	got := ftsQuery(`"hello" (world)* fix-bug`)
	// After stripping: hello, world, fixbug → all >2 chars
	if !strings.Contains(got, `"hello"`) {
		t.Errorf("ftsQuery should include hello, got %q", got)
	}
	if strings.Contains(got, "(") || strings.Contains(got, ")") || strings.Contains(got, "*") {
		t.Errorf("ftsQuery should strip special chars, got %q", got)
	}
}

// ── splitOnHeaders / splitBrainMD / split* ────────────────────────────────────

func TestSplitOnHeaders(t *testing.T) {
	text := "## Section One\nline A\nline B\nline C\nmore content here\n## Section Two\nline D\nline E\nline F\nmore stuff"
	chunks := splitBrainMD(text)
	if len(chunks) < 2 {
		t.Fatalf("expected ≥2 chunks, got %d", len(chunks))
	}
	labels := make([]string, len(chunks))
	for i, c := range chunks {
		labels[i] = c.label
	}
	found := false
	for _, l := range labels {
		if l == "Section One" || l == "Section Two" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected section labels in chunks, got %v", labels)
	}
}

func TestSplitOnHeadersShort(t *testing.T) {
	// Content <20 chars per section → no section chunks; falls back to splitIntoChunks.
	text := "## S1\nhello\n## S2\nworld\n"
	chunks := splitBrainMD(text)
	// Fallback gives at least 1 chunk
	if len(chunks) == 0 {
		t.Error("expected at least 1 chunk from fallback path")
	}
}

func TestSplitBrainMD(t *testing.T) {
	text := "## Architecture\nThis describes the system\nmore details here yes\n## Decisions\nDecision log entries go here and have details"
	chunks := splitBrainMD(text)
	if len(chunks) == 0 {
		t.Fatal("splitBrainMD returned 0 chunks")
	}
	// Should split on ## headings
	for _, c := range chunks {
		if c.text == "" {
			t.Error("unexpected empty chunk text")
		}
	}
}

func TestSplitDecisionsLog(t *testing.T) {
	text := "[2026-01-01] Chose SQLite for embeddings storage because it is embedded\n[2026-01-02] Switched to BM25+cosine hybrid for better recall\n"
	chunks := splitDecisionsLog(text)
	if len(chunks) == 0 {
		t.Fatal("splitDecisionsLog returned 0 chunks")
	}
	// Each chunk label should start with the timestamp prefix
	for _, c := range chunks {
		if c.label == "" {
			t.Errorf("empty label in decision chunk: text=%q", c.text)
		}
	}
}

func TestSplitRejectedMD(t *testing.T) {
	text := "- **Approach A**: tried using redis but hit connection issues and latency\n- **Approach B**: tried embedding every file but memory usage was too high\n"
	chunks := splitRejectedMD(text)
	if len(chunks) == 0 {
		t.Fatal("splitRejectedMD returned 0 chunks")
	}
}

func TestSplitConventionsMD(t *testing.T) {
	// splitConventionsMD delegates to splitOnHeaders with ## pattern, same as splitBrainMD.
	text := "## Naming\nUse snake_case for variables and file names throughout the codebase\n## Imports\nNever import from internal packages across service boundaries ever"
	chunks := splitConventionsMD(text)
	if len(chunks) < 2 {
		t.Fatalf("expected ≥2 chunks from ## sections, got %d", len(chunks))
	}
}

// ── Benchmarks ────────────────────────────────────────────────────────────────

// BenchmarkCosineSimilarity768 benchmarks cosine similarity on typical 768-dim
// embedding vectors (nomic-embed-text output dimension).
func BenchmarkCosineSimilarity768(b *testing.B) {
	dim := 768
	a := make([]float64, dim)
	bv := make([]float64, dim)
	for i := range a {
		a[i] = float64(i%13+1) / 13.0
		bv[i] = float64((i+7)%17+1) / 17.0
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = cosineSimilarity(a, bv)
	}
}

// BenchmarkSplitIntoChunks10KB benchmarks chunking a 10 KB text into 512-char pieces.
func BenchmarkSplitIntoChunks10KB(b *testing.B) {
	text := strings.Repeat("The quick brown fox jumps over the lazy dog.\n", 230) // ~10 KB
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = splitIntoChunks(text, 512)
	}
}

// BenchmarkEncodeVec768 benchmarks binary encoding of a 768-element vector.
func BenchmarkEncodeVec768(b *testing.B) {
	v := make([]float64, 768)
	for i := range v {
		v[i] = float64(i) / 768.0
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = encodeVec(v)
	}
}

// BenchmarkDecodeVec768 benchmarks binary decoding back to a 768-element vector.
func BenchmarkDecodeVec768(b *testing.B) {
	v := make([]float64, 768)
	for i := range v {
		v[i] = float64(i) / 768.0
	}
	encoded := encodeVec(v)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = decodeVec(encoded)
	}
}
