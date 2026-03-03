package router

// Package-level embedding classifier for routing.
// Layer 3 of Classify() — fires only when accumulated scoring confidence < 0.82.
//
// Approach:
//   - Pre-index RouterExamples into the embeddings store (source="router-label",
//     section_label=tier name) at startup via IndexRouterExamples().
//   - On ambiguous queries, embed the query and fetch the 5 nearest labeled examples.
//   - Majority-vote on the returned labels; use as the tier prediction.
//   - LRU cache (128 entries) avoids re-embedding repeated queries.

import (
	"context"
	"sync"
	"time"
)

const (
	routerLabelSource = "router-label"
	cacheSize         = 128
	knnK              = 5
	classifierTimeout = 2 * time.Second
)

// EmbedStore is the minimal interface the classifier needs from embeddings.Store.
// Defined here to avoid importing the embeddings package into router (which would
// be fine architecturally, but keeps the package lighter).
type EmbedStore interface {
	Add(ctx context.Context, id, source, sectionLabel, text string) error
	SearchBySource(ctx context.Context, query, source string, limit int) ([]EmbedChunk, error)
}

// EmbedChunk is a minimal projection of embeddings.Chunk used by the classifier.
type EmbedChunk struct {
	SectionLabel string  // tier name stored here
	Score        float64 // RRF score
}

// ── LRU cache ────────────────────────────────────────────────────────────────

type cacheEntry struct {
	tier Tier
	conf float64
}

var (
	routerCache   = map[string]cacheEntry{}
	routerCacheMu sync.Mutex
)

func cacheGet(query string) (cacheEntry, bool) {
	routerCacheMu.Lock()
	e, ok := routerCache[query]
	routerCacheMu.Unlock()
	return e, ok
}

func cachePut(query string, e cacheEntry) {
	routerCacheMu.Lock()
	if len(routerCache) >= cacheSize {
		// Evict one random entry (good enough for this use case).
		for k := range routerCache {
			delete(routerCache, k)
			break
		}
	}
	routerCache[query] = e
	routerCacheMu.Unlock()
}

// ── Indexer ───────────────────────────────────────────────────────────────────

// IndexRouterExamples embeds all labeled examples into the store.
// Should be called once in a background goroutine at startup.
// Safe to call multiple times — content hashing in Add() skips unchanged entries.
func IndexRouterExamples(store EmbedStore) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	for i, ex := range RouterExamples {
		id := routerLabelSource + "-" + ex.Tier.String() + "-" + itoa(i)
		// Text = query, section_label = tier name (used as the kNN class label).
		_ = store.Add(ctx, id, routerLabelSource, ex.Tier.String(), ex.Query)
	}
}

// ── kNN classifier ────────────────────────────────────────────────────────────

// classifyByEmbedding uses nearest-neighbor lookup over labeled examples.
// Falls back to the provided fallback intent if the store is unavailable or
// returns no results within the timeout.
// The caller's ctx is respected; classifierTimeout is applied as an additional cap.
func classifyByEmbedding(callerCtx context.Context, message string, store EmbedStore, fallback Intent) Intent {
	if store == nil {
		return fallback
	}

	// Check cache first.
	if cached, ok := cacheGet(message); ok {
		return Intent{
			Tier:       cached.tier,
			TaskType:   fallback.TaskType,
			NeedsGraph: fallback.NeedsGraph,
			Confidence: cached.conf,
		}
	}

	ctx, cancel := context.WithTimeout(callerCtx, classifierTimeout)
	defer cancel()

	chunks, err := store.SearchBySource(ctx, message, routerLabelSource, knnK)
	if err != nil || len(chunks) == 0 {
		return fallback
	}

	// Majority vote with score-weighted tally.
	votes := map[Tier]float64{}
	for _, c := range chunks {
		tier := parseTierName(c.SectionLabel)
		if tier >= 0 {
			votes[tier] += c.Score
		}
	}

	// Pick tier with highest weighted vote.
	var winner Tier = fallback.Tier
	var best float64
	for tier, score := range votes {
		if score > best {
			best = score
			winner = tier
		}
	}

	// Confidence: ratio of winner votes to total.
	total := 0.0
	for _, s := range votes {
		total += s
	}
	conf := 0.60
	if total > 0 {
		conf = 0.60 + 0.35*(best/total)
	}
	if conf > 0.95 {
		conf = 0.95
	}

	entry := cacheEntry{tier: winner, conf: conf}
	cachePut(message, entry)

	return Intent{
		Tier:       winner,
		TaskType:   fallback.TaskType,
		NeedsGraph: fallback.NeedsGraph,
		Confidence: conf,
	}
}

// parseTierName converts a tier name string back to a Tier value.
func parseTierName(name string) Tier {
	for _, t := range []Tier{TierTrivial, TierFast, TierCode, TierReason, TierHeavy, TierMax, TierVision} {
		if t.String() == name {
			return t
		}
	}
	return -1 // unknown
}

// itoa is a minimal int-to-string for map key generation without importing strconv.
// BUG-15: handle negative integers (returned empty string before, causing ID collisions).
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	buf := [21]byte{}
	pos := len(buf)
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
