package embeddings

import (
	"math"
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
