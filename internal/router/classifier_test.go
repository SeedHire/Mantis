package router

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// ── mockEmbedStore ─────────────────────────────────────────────────────────────

type mockEmbedStore struct {
	results []EmbedChunk
	err     error
}

func (m *mockEmbedStore) Add(_ context.Context, _, _, _, _ string) error { return nil }
func (m *mockEmbedStore) SearchBySource(_ context.Context, _, _ string, _ int) ([]EmbedChunk, error) {
	return m.results, m.err
}

// ── TestClassifyByEmbedding ───────────────────────────────────────────────────

func TestClassifyByEmbedding_Majority(t *testing.T) {
	// 3 code chunks + 1 reason chunk → TierCode should win
	store := &mockEmbedStore{
		results: []EmbedChunk{
			{SectionLabel: TierCode.String(), Score: 0.9},
			{SectionLabel: TierCode.String(), Score: 0.85},
			{SectionLabel: TierCode.String(), Score: 0.80},
			{SectionLabel: TierReason.String(), Score: 0.75},
		},
	}
	fallback := Intent{Tier: TierFast, TaskType: "general"}
	got := classifyByEmbedding(context.Background(), "write a function to parse JSON", store, fallback)
	if got.Tier != TierCode {
		t.Errorf("majority code chunks: expected TierCode, got %v", got.Tier)
	}
	if got.Confidence <= 0.60 {
		t.Errorf("expected confidence > 0.60, got %.2f", got.Confidence)
	}
	// TaskType preserved from fallback
	if got.TaskType != fallback.TaskType {
		t.Errorf("TaskType should be preserved from fallback, got %v", got.TaskType)
	}
}

func TestClassifyByEmbedding_Empty(t *testing.T) {
	// No results → fall back unchanged
	store := &mockEmbedStore{results: nil}
	fallback := Intent{Tier: TierReason, TaskType: "explain", Confidence: 0.75}
	got := classifyByEmbedding(context.Background(), "explain generics in Go", store, fallback)
	if got.Tier != TierReason {
		t.Errorf("empty results: expected fallback TierReason, got %v", got.Tier)
	}
}

func TestClassifyByEmbedding_StoreError(t *testing.T) {
	// Store returns an error → fall back unchanged
	store := &mockEmbedStore{err: errors.New("embedding store unavailable")}
	fallback := Intent{Tier: TierHeavy, TaskType: "implement"}
	got := classifyByEmbedding(context.Background(), "build a full auth system", store, fallback)
	if got.Tier != TierHeavy {
		t.Errorf("store error: expected fallback TierHeavy, got %v", got.Tier)
	}
}

func TestClassifyByEmbedding_NilStore(t *testing.T) {
	fallback := Intent{Tier: TierCode, TaskType: "fix"}
	got := classifyByEmbedding(context.Background(), "fix the null pointer", nil, fallback)
	if got.Tier != TierCode {
		t.Errorf("nil store: expected fallback TierCode, got %v", got.Tier)
	}
}

// ── A10: itoa negative integers ───────────────────────────────────────────────

// TestItoaValues is a regression for BUG-15 (A10): the original itoa returned
// an empty string for negative integers, causing two different negative indices
// to produce the same (empty) cache key and silently collide.
func TestItoaValues(t *testing.T) {
	tests := []struct {
		n    int
		want string
	}{
		{0, "0"},
		{1, "1"},
		{9, "9"},
		{10, "10"},
		{42, "42"},
		{-1, "-1"},
		{-42, "-42"},
		{1_000_000_000, "1000000000"},
		{-2_147_483_648, "-2147483648"},
	}
	for _, tt := range tests {
		got := itoa(tt.n)
		if got != tt.want {
			t.Errorf("itoa(%d) = %q, want %q", tt.n, got, tt.want)
		}
	}
}

// TestItoaNegativesDistinct verifies that two distinct negative integers
// produce distinct string IDs — the pre-fix bug returned "" for all negatives,
// causing every negative index to collide into the same empty-string key.
func TestItoaNegativesDistinct(t *testing.T) {
	for _, pair := range [][2]int{{-1, -2}, {-42, -43}, {-100, -200}} {
		a, b := itoa(pair[0]), itoa(pair[1])
		if a == b {
			t.Errorf("BUG-15 regression: itoa(%d)==itoa(%d)==%q (should be distinct)",
				pair[0], pair[1], a)
		}
	}
}

// TestItoaMatchesFmt verifies itoa agrees with fmt.Sprintf for a range of values.
func TestItoaMatchesFmt(t *testing.T) {
	cases := []int{0, 1, -1, 127, -128, 32767, -32768, 99999, -99999}
	for _, n := range cases {
		want := fmt.Sprintf("%d", n)
		got := itoa(n)
		if got != want {
			t.Errorf("itoa(%d) = %q, want %q", n, got, want)
		}
	}
}

// ── LRU correctness ───────────────────────────────────────────────────────────

// TestLRU_BasicEviction fills a cap-2 LRU, then adds a third entry and verifies
// the least-recently-used entry is evicted.
func TestLRU_BasicEviction(t *testing.T) {
	c := newLRU(2)
	c.put("a", cacheEntry{tier: TierFast, conf: 0.5})
	c.put("b", cacheEntry{tier: TierCode, conf: 0.6})

	// Access "a" to make it the most-recently-used.
	if _, ok := c.get("a"); !ok {
		t.Fatal("expected 'a' to be present")
	}

	// Add "c" — should evict "b" (LRU).
	c.put("c", cacheEntry{tier: TierReason, conf: 0.7})

	if _, ok := c.get("b"); ok {
		t.Error("expected 'b' to be evicted (LRU), but it is still present")
	}
	if _, ok := c.get("a"); !ok {
		t.Error("expected 'a' to remain (it was recently accessed)")
	}
	if _, ok := c.get("c"); !ok {
		t.Error("expected 'c' to be present (just added)")
	}
}

// TestLRU_UpdateExisting verifies that updating an existing key moves it to front
// and does not grow the cache.
func TestLRU_UpdateExisting(t *testing.T) {
	c := newLRU(2)
	c.put("x", cacheEntry{tier: TierFast, conf: 0.5})
	c.put("y", cacheEntry{tier: TierCode, conf: 0.6})

	// Re-insert "x" with a new value (should move it to front).
	c.put("x", cacheEntry{tier: TierReason, conf: 0.9})

	if len(c.items) != 2 {
		t.Errorf("expected 2 items after update, got %d", len(c.items))
	}
	got, ok := c.get("x")
	if !ok {
		t.Fatal("expected 'x' after update")
	}
	if got.tier != TierReason {
		t.Errorf("expected updated tier TierReason, got %v", got.tier)
	}
}

// TestLRU_MissonEmpty confirms an empty LRU returns miss for any key.
func TestLRU_MissOnEmpty(t *testing.T) {
	c := newLRU(5)
	if _, ok := c.get("nonexistent"); ok {
		t.Error("empty LRU should return miss for any key")
	}
}

// ── Cache round-trip ──────────────────────────────────────────────────────────

// TestCacheRoundTrip verifies cacheGet/cachePut store and retrieve entries.
func TestCacheRoundTrip(t *testing.T) {
	// Use a unique key to avoid cross-test contamination.
	key := "test-query-for-cache-round-trip-unique"

	// Should not exist initially.
	if _, ok := cacheGet(key); ok {
		t.Skip("key already in cache — test environment may be shared")
	}

	entry := cacheEntry{tier: TierCode, conf: 0.85}
	cachePut(key, entry)

	got, ok := cacheGet(key)
	if !ok {
		t.Fatal("cacheGet returned miss after cachePut")
	}
	if got.tier != TierCode {
		t.Errorf("cached tier = %v, want TierCode", got.tier)
	}
	if got.conf != 0.85 {
		t.Errorf("cached conf = %f, want 0.85", got.conf)
	}
}

// ── Benchmarks ────────────────────────────────────────────────────────────────

func BenchmarkItoa(b *testing.B) {
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = itoa(i)
	}
}

func BenchmarkItoaNegative(b *testing.B) {
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = itoa(-i)
	}
}
