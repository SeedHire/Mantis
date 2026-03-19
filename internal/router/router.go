// Package router classifies user intent and selects the right specialised model.
//
// Tiers (7):
//
//	TierTrivial  — one-liners, definitions, syntax lookups          (~1–4B model)
//	TierFast     — short code questions, small completions          (~8–24B model)
//	TierCode     — coding specialist: implement, debug, refactor    (glm-5 / devstral-2)
//	TierReason   — analysis, architecture, deep explanation         (kimi-k2-thinking / gpt-oss)
//	TierHeavy    — multi-file, large context, complex design        (glm-4.7 / mistral-large-3)
//	TierMax      — ensemble: 3 specialists in parallel + synthesis  (Opus-level output)
//	TierVision   — any image / screenshot input                     (gemini-3-flash-preview / qwen3-vl)
package router

import (
	"context"
	"os"
	"strings"
	"sync"

	"github.com/seedhire/mantis/internal/ollama"
)

// Tier represents the selected model specialisation.
type Tier int

const (
	TierTrivial Tier = iota
	TierFast
	TierCode
	TierReason
	TierHeavy
	TierMax
	TierVision
)

func (t Tier) String() string {
	names := [...]string{"trivial", "fast", "code", "reason", "heavy", "max", "vision"}
	if t < 0 || int(t) >= len(names) {
		return "unknown"
	}
	return names[t]
}

// EditFormat specifies the preferred code edit format for the resolved model.
type EditFormat string

const (
	EditFormatSearchReplace EditFormat = "search-replace" // SEARCH/REPLACE blocks (capable models ≥24B)
	EditFormatWholeFile     EditFormat = "whole-file"     // full file output (small models 7-13B)
	EditFormatUnifiedDiff   EditFormat = "unified-diff"   // unified diff (large reasoning models)
)

// Intent holds the routing decision for a user message.
type Intent struct {
	Tier        Tier
	TaskType    string     // the detected sub-type
	NeedsGraph  bool
	NeedsVision bool
	Confidence  float64
	EditFormat  EditFormat // 7.9: model-aware edit format
}

// ── Model preference lists ────────────────────────────────────────────────────
// Each tier lists models in priority order (cloud first, local fallback).
// Cloud models are those available on Ollama Cloud (ollama.com/search?c=cloud).
//
// Current Ollama Cloud models (March 2026):
//   glm-5, glm-4.7, glm-4.6, devstral-2, devstral-small-2, qwen3.5 (0.8b-122b),
//   qwen3-coder-next, qwen3-next, qwen3-vl, kimi-k2.5, minimax-m2.5, minimax-m2,
//   minimax-m2.1, cogito-2.1, nemotron-3-super, nemotron-3-nano, deepseek-v3.2,
//   ministral-3, rnj-1, gemini-3-flash-preview
//
// SWE-bench Verified scores:
//   glm-5 77.8% · glm-4.7 73.8% · qwen3.5:122b 72.4% · devstral-2 72.2%
//   devstral-small-2 68.0% · qwen3.5:35b ~65%
//
// Tool use (BFCL-V4): qwen3.5:122b 72.2% (best open-source for agents)
//
// Context windows:
//   gemini-3-flash-preview 1M · qwen3.5 262K (1M extrap.) · qwen3-vl 262K
//   devstral-2 256K · kimi-k2.5 256K · glm-5 198K · glm-4.7 200K
var preferredModels = map[Tier][]string{
	TierTrivial: {
		// cloud — small fast models, good for definitions/lookups
		"qwen3.5:4b", "ministral-3:3b", "rnj-1:8b", "qwen3.5:2b",
		// local
		"qwen2.5-coder:1.5b", "qwen2.5-coder:0.5b", "llama3.2:1b", "phi3:mini", "gemma2:2b",
	},
	TierFast: {
		// cloud — mid-size, good for short code questions
		// qwen3.5:27b: 72.4% SWE-bench at 27B MoE, 262K ctx, strong tool use
		// devstral-small-2: 68% SWE-bench at 24B, 256K ctx
		"qwen3.5:27b", "devstral-small-2:24b-cloud", "nemotron-3-nano:30b",
		"qwen3.5:9b", "ministral-3:8b", "qwen3-coder-next:cloud",
		// local
		"qwen2.5-coder:7b", "llama3.2:3b", "phi3:3.8b", "gemma2:9b",
	},
	TierCode: {
		// cloud — coding-specialist models ranked by SWE-bench + tool use
		// glm-5: 77.8% SWE-bench (#1 open model), 198K ctx, 744B/40B active MoE
		// qwen3.5:122b: 72.4% SWE-bench, 72.2% BFCL-V4 (best tool use), 262K ctx
		// devstral-2: 72.2% SWE-bench, 256K ctx, 123B dense, agentic multi-file
		"glm-5:cloud", "qwen3.5:122b", "devstral-2:123b-cloud",
		"glm-4.7:cloud", "kimi-k2.5:cloud", "qwen3.5:35b",
		"minimax-m2.5:cloud", "qwen3-coder-next:cloud",
		// local fallbacks (bare names)
		"glm-5", "devstral-2:123b", "deepseek-v3.1:671b",
		// local
		"qwen2.5-coder:32b", "qwen2.5-coder:14b", "deepseek-coder-v2:16b",
		"deepseek-coder:6.7b", "codellama:13b",
	},
	TierReason: {
		// cloud — reasoning/chain-of-thought models
		// cogito-2.1: 671B, strong reasoning (Llama-based + deep thinking)
		// qwen3.5:122b: 85.5% GPQA-Diamond, 86.1% MMLU-Pro
		// glm-5: 92.7% AIME 2026, 86.0% GPQA-Diamond
		"glm-5:cloud", "cogito-2.1:671b", "qwen3.5:122b",
		"kimi-k2.5:cloud", "qwen3-next:80b-cloud", "deepseek-v3.2:cloud",
		"nemotron-3-super:120b",
		// local fallbacks
		"deepseek-v3.1:671b",
		// local
		"deepseek-r1:14b", "deepseek-r1:8b", "llama3.1:70b", "mixtral:8x7b", "llama3.3:70b",
	},
	TierHeavy: {
		// cloud — largest models for hard multi-file tasks
		// glm-5: 77.8% SWE-bench, 198K ctx — strongest open coding model
		// qwen3.5:122b: 72.4% SWE-bench + best tool use (BFCL-V4 72.2%), 262K ctx
		// glm-4.7: 73.8% SWE-bench, 30B/3B active MoE (fast!), 200K ctx
		"glm-5:cloud", "qwen3.5:122b", "glm-4.7:cloud",
		"devstral-2:123b-cloud", "minimax-m2.5:cloud",
		"kimi-k2.5:cloud", "cogito-2.1:671b", "deepseek-v3.2:cloud",
		// local fallbacks
		"devstral-2:123b", "deepseek-v3.1:671b",
		// local
		"deepseek-r1:32b", "qwen2.5-coder:72b", "llama3.3:70b",
	},
	// TierMax uses ensemblePools — see EnsembleModels()
	TierMax: {
		// Single-model fallback when ensemble unavailable — use top all-round model
		"glm-5:cloud", "qwen3.5:122b", "devstral-2:123b-cloud",
		"cogito-2.1:671b", "minimax-m2.5:cloud", "kimi-k2.5:cloud",
		"qwen3-coder-next:cloud", "deepseek-v3.2:cloud",
		// local fallbacks
		"glm-5", "devstral-2:123b",
	},
	TierVision: {
		// cloud — multimodal models
		// gemini-3-flash-preview: 1M ctx, 90.4% GPQA-Diamond — best vision on Ollama Cloud
		// qwen3-vl: 262K ctx (1M extrap.), OS World SOTA, vision + tool use
		// qwen3.5: native multimodal (unified vision-language), 262K ctx
		// kimi-k2.5: native multimodal, vision-based coding
		"gemini-3-flash-preview:cloud", "qwen3-vl:235b-cloud", "qwen3.5:122b",
		"kimi-k2.5:cloud",
		// local fallbacks
		"qwen3-vl:235b",
		// local
		"llama3.2-vision:11b", "llava:13b", "llava:7b", "moondream:1.8b",
	},
}

// ensemblePools: one model per pool is selected for parallel ensemble execution.
// Pool 1 = coding specialist, Pool 2 = reasoning, Pool 3 = large general.
var ensemblePools = [][]string{
	// Pool 1: coding specialists — ranked by SWE-bench Verified
	{"glm-5:cloud", "devstral-2:123b-cloud", "qwen3.5:122b", "glm-4.7:cloud", "qwen2.5-coder:32b"},
	// Pool 2: reasoning / chain-of-thought
	{"cogito-2.1:671b", "glm-5:cloud", "deepseek-v3.2:cloud", "nemotron-3-super:120b", "llama3.3:70b"},
	// Pool 3: large general-purpose
	{"qwen3.5:122b", "minimax-m2.5:cloud", "kimi-k2.5:cloud", "glm-5", "gemma3:27b"},
}

var defaultModels = map[Tier]string{
	TierTrivial: "qwen3.5:4b",                  // fast, available on cloud
	TierFast:    "qwen3.5:27b",                  // 72.4% SWE-bench at 27B MoE, 262K ctx
	TierCode:    "glm-5:cloud",                  // 77.8% SWE-bench (#1 open model), 198K ctx
	TierReason:  "glm-5:cloud",                  // 92.7% AIME 2026, 86.0% GPQA-Diamond
	TierHeavy:   "qwen3.5:122b",                // 72.4% SWE, 72.2% BFCL-V4 (best tool use), 262K ctx
	TierMax:     "glm-5:cloud",                  // best all-round: 77.8% SWE + 92.7% AIME
	TierVision:  "gemini-3-flash-preview:cloud", // 1M ctx, 90.4% GPQA-Diamond
}

// ── Size-based fallback thresholds ────────────────────────────────────────────
const (
	gb         = int64(1_000_000_000)
	trivialMax = 5 * gb
	fastMax    = 15 * gb
	codeMax    = 40 * gb
	reasonMax  = 100 * gb

// heavy / max = anything larger
)

var (
	resolvedModels   = map[Tier]string{}
	resolvedModelsMu sync.RWMutex
)

// ResolveAll picks the best available model for every tier from a live model list.
// Phase 1: trait-based auto-discovery scores models by benchmarks per tier.
// Phase 2: legacy preference lists fill any gaps as fallback.
func ResolveAll(available []ollama.ModelInfo) {
	set := buildSet(available)
	allTiers := []Tier{TierTrivial, TierFast, TierCode, TierReason, TierHeavy, TierMax, TierVision}
	resolvedModelsMu.Lock()
	defer resolvedModelsMu.Unlock()

	// Phase 1: trait-based auto-discovery (primary).
	discovered := DiscoverBest(available)
	for tier, model := range discovered {
		resolvedModels[tier] = model
	}

	// Phase 2: fill gaps with legacy preference lists (secondary fallback).
	for _, tier := range allTiers {
		if resolvedModels[tier] != "" {
			continue
		}
		if chosen := pickFromPrefs(preferredModels[tier], set, available); chosen != "" {
			resolvedModels[tier] = chosen
		} else if chosen := pickBySize(available, tier); chosen != "" {
			resolvedModels[tier] = chosen
		}
	}
}

// EnsembleModels returns one model per speciality pool (coding/reasoning/general).
// Falls back to top-3 from TierMax prefs when pools can't be filled.
func EnsembleModels(available []ollama.ModelInfo) []string {
	set := buildSet(available)
	var picked []string
	for _, pool := range ensemblePools {
		if m := pickFromPrefs(pool, set, available); m != "" {
			picked = append(picked, m)
		}
	}
	if len(picked) < 2 {
		picked = nil
		for _, c := range preferredModels[TierMax] {
			if m := resolveOne(c, set, available); m != "" {
				picked = append(picked, m)
				if len(picked) == 3 {
					break
				}
			}
		}
	}
	return picked
}

func buildSet(available []ollama.ModelInfo) map[string]ollama.ModelInfo {
	set := make(map[string]ollama.ModelInfo, len(available)*2)
	for _, m := range available {
		set[m.Name] = m
		if idx := strings.Index(m.Name, ":"); idx != -1 {
			bare := m.Name[:idx]
			if _, exists := set[bare]; !exists {
				set[bare] = m
			}
		}
	}
	return set
}

func pickFromPrefs(prefs []string, set map[string]ollama.ModelInfo, available []ollama.ModelInfo) string {
	for _, c := range prefs {
		if m := resolveOne(c, set, available); m != "" {
			return m
		}
	}
	return ""
}

func resolveOne(candidate string, set map[string]ollama.ModelInfo, available []ollama.ModelInfo) string {
	if _, ok := set[candidate]; ok {
		return candidate
	}
	bare := candidate
	if idx := strings.Index(candidate, ":"); idx != -1 {
		bare = candidate[:idx]
	}
	if info, ok := set[bare]; ok {
		return info.Name
	}
	// prefix match (e.g. "devstral" matches "devstral-small-2:24b")
	for _, m := range available {
		if strings.HasPrefix(m.Name, bare) {
			return m.Name
		}
	}
	return ""
}

func pickBySize(available []ollama.ModelInfo, tier Tier) string {
	if len(available) == 0 {
		return ""
	}
	buckets := [5][]ollama.ModelInfo{} // [0]=trivial [1]=fast [2]=code [3]=reason [4]=heavy
	for _, m := range available {
		switch {
		case m.Size <= trivialMax:
			buckets[0] = append(buckets[0], m)
		case m.Size <= fastMax:
			buckets[1] = append(buckets[1], m)
		case m.Size <= codeMax:
			buckets[2] = append(buckets[2], m)
		case m.Size <= reasonMax:
			buckets[3] = append(buckets[3], m)
		default:
			buckets[4] = append(buckets[4], m)
		}
	}
	pick := func(b []ollama.ModelInfo, largest bool) string {
		if len(b) == 0 {
			return ""
		}
		best := b[0]
		for _, m := range b[1:] {
			if (largest && m.Size > best.Size) || (!largest && m.Size < best.Size) {
				best = m
			}
		}
		return best.Name
	}
	// For speed tiers, prefer quantized variants for faster inference.
	pickQuantized := func(b []ollama.ModelInfo) string {
		for _, m := range b {
			if isQuantized(m.Name) {
				return m.Name
			}
		}
		return ""
	}
	switch tier {
	case TierTrivial:
		if m := pickQuantized(buckets[0]); m != "" {
			return m
		}
		if m := pick(buckets[0], false); m != "" {
			return m
		}
		return pick(buckets[1], false)
	case TierFast:
		if m := pickQuantized(buckets[1]); m != "" {
			return m
		}
		if m := pick(buckets[1], true); m != "" {
			return m
		}
		return pick(buckets[0], true)
	case TierCode:
		if m := pick(buckets[2], true); m != "" {
			return m
		}
		return pick(buckets[1], true)
	case TierReason:
		if m := pick(buckets[3], true); m != "" {
			return m
		}
		return pick(buckets[2], true)
	case TierHeavy, TierMax:
		if m := pick(buckets[4], true); m != "" {
			return m
		}
		return pick(buckets[3], true)
	case TierVision:
		for _, m := range available {
			n := strings.ToLower(m.Name)
			if strings.Contains(n, "vl") || strings.Contains(n, "vision") || strings.Contains(n, "gemini") {
				return m.Name
			}
		}
	}
	return available[0].Name
}

func ModelFor(tier Tier) string {
	resolvedModelsMu.RLock()
	m := resolvedModels[tier]
	resolvedModelsMu.RUnlock()
	if m != "" {
		return m
	}
	return defaultModels[tier]
}

func PreferredModels(tier Tier) []string { return preferredModels[tier] }
func SetResolved(tier Tier, model string) {
	resolvedModelsMu.Lock()
	resolvedModels[tier] = model
	resolvedModelsMu.Unlock()
}

// ContextWindowFor returns the recommended NumCtx for the given model name.
// First checks the traits database for a known PracticalCtx, then falls back
// to hardcoded string matching. Defaults to 32768 for unknown/small models.
func ContextWindowFor(model string) int {
	// Check traits database first — auto-covers new model families.
	if traits := matchTraits(model); traits != nil && traits.PracticalCtx > 0 {
		return traits.PracticalCtx
	}
	// Legacy string-matching fallback.
	lower := strings.ToLower(model)
	switch {
	case strings.Contains(lower, "gemini-3-flash"):
		return 131072 // 128K — practical cap even though model supports 1M
	case strings.Contains(lower, "devstral-2"),
		strings.Contains(lower, "qwen3-coder"),
		strings.Contains(lower, "qwen3.5"),
		strings.Contains(lower, "qwen3-vl"),
		strings.Contains(lower, "kimi-k2"),
		strings.Contains(lower, "cogito-2.1"),
		strings.Contains(lower, "deepseek-v3"),
		strings.Contains(lower, "nemotron-3"):
		return 65536 // 64K — these support 256K+ but 64K balances RAM/speed
	case strings.Contains(lower, "glm-5"),
		strings.Contains(lower, "glm-4.7"),
		strings.Contains(lower, "deepseek-r1:70b"),
		strings.Contains(lower, "deepseek-r1:32b"),
		strings.Contains(lower, "llama3.3:70b"):
		return 49152 // 48K
	case strings.Contains(lower, "gemma3") && (strings.Contains(lower, ":1b") || strings.Contains(lower, ":4b")):
		return 8192 // 8K — small models have limited context on cloud free tier
	case strings.Contains(lower, "gemma3"):
		return 16384 // 16K
	default:
		return 32768 // 32K — safe default for small local models
	}
}

// ResolvedSummary returns a human-readable mapping of tier → resolved model.
func ResolvedSummary() map[string]string {
	summary := make(map[string]string)
	for _, tier := range []Tier{TierTrivial, TierFast, TierCode, TierReason, TierHeavy, TierMax, TierVision} {
		summary[tier.String()] = ModelFor(tier)
	}
	return summary
}

// EditFormatForTier returns the best edit format for a given tier.
// TierFast (7-13B models): whole-file (simpler, more reliable).
// TierMax (large reasoning): unified diff (most expressive).
// Everything else: SEARCH/REPLACE blocks.
func EditFormatForTier(t Tier) EditFormat {
	switch t {
	case TierTrivial, TierFast:
		return EditFormatWholeFile
	case TierMax:
		return EditFormatUnifiedDiff
	default:
		return EditFormatSearchReplace
	}
}

// DraftModel returns the speculative decoding draft model if configured via
// MANTIS_DRAFT_MODEL env var. Returns empty string when not set (disables feature).
// Usage: set MANTIS_DRAFT_MODEL=qwen2.5-coder:7b to pair a small local draft model
// with larger cloud models for up to 4x speedup on structured outputs.
func DraftModel() string {
	return strings.TrimSpace(os.Getenv("MANTIS_DRAFT_MODEL"))
}

// isQuantized returns true if the model name suggests a quantized variant (q4, q5, q8, etc).
func isQuantized(name string) bool {
	lower := strings.ToLower(name)
	for _, q := range []string{":q4", ":q5", ":q6", ":q8", "-q4", "-q5", "-q8", "gguf"} {
		if strings.Contains(lower, q) {
			return true
		}
	}
	return false
}

// ── Classifier ────────────────────────────────────────────────────────────────

// Classify analyses a user message and returns the routing intent.
//
// Three-layer pipeline:
//
//	Layer 1 (0 ms)  — structural rules: vision, terminal errors
//	Layer 2 (0 ms)  — accumulated keyword scoring with context modifiers
//	Layer 3 (20 ms) — embedding kNN on labeled examples (only when conf < 0.82)
//
// Pass a non-nil EmbedStore to enable Layer 3; nil disables it (safe default).
// Classify classifies message intent using keyword scoring + optional embedding kNN.
// Uses context.Background() as the base context. For a tighter deadline, use ClassifyCtx.
func Classify(message string, hasImage bool, store ...EmbedStore) Intent {
	return ClassifyCtx(context.Background(), message, hasImage, store...)
}

// ClassifyCtx is Classify with an explicit context (respects caller cancellation and deadline).
func ClassifyCtx(ctx context.Context, message string, hasImage bool, store ...EmbedStore) Intent {
	var es EmbedStore
	if len(store) > 0 {
		es = store[0]
	}
	return classify(ctx, message, hasImage, es)
}

func classify(ctx context.Context, message string, hasImage bool, store EmbedStore) Intent {
	if hasImage {
		return Intent{Tier: TierVision, TaskType: "vision", NeedsVision: true, Confidence: 1.0}
	}
	lower := strings.ToLower(message)

	// Terminal error paste — route to TierCode so a capable model handles the fix.
	for _, sig := range terminalErrorSignatures {
		if strings.Contains(lower, sig) {
			return Intent{Tier: TierCode, TaskType: "fix", NeedsGraph: false, Confidence: 0.90}
		}
	}

	// Accumulated scores per tier.
	scores := map[Tier]float64{}

	for _, kw := range maxKeywords {
		if strings.Contains(lower, kw) {
			scores[TierMax] += 1.5
		}
	}
	for _, kw := range reasonKeywords {
		if strings.Contains(lower, kw) {
			scores[TierReason] += 1.2
		}
	}
	for _, kw := range heavyKeywords {
		if strings.Contains(lower, kw) {
			scores[TierHeavy] += 1.3
		}
	}
	for _, kw := range codeKeywords {
		if strings.Contains(lower, kw) {
			scores[TierCode] += 0.8
		}
	}
	for _, kw := range fastKeywords {
		if strings.Contains(lower, kw) {
			scores[TierFast] += 0.9
		}
	}
	for _, kw := range trivialKeywords {
		if strings.Contains(lower, kw) {
			scores[TierTrivial] += 1.0
		}
	}

	// Context modifiers:
	// 0. "how to run/start" should never be trivial — needs project awareness.
	if strings.Contains(lower, "how to run") || strings.Contains(lower, "how to start") {
		scores[TierTrivial] = 0
	}

	// 1. Question-form dampener — a trailing "?" strongly suggests explanation/reason.
	isQuestion := strings.HasSuffix(strings.TrimSpace(lower), "?")
	if isQuestion {
		scores[TierHeavy] *= 0.4
		scores[TierCode] *= 0.7
		scores[TierReason] *= 1.3
	}
	// 2. Very short messages (≤ 4 words) are unlikely to be heavy-tier build requests.
	if wc := len(strings.Fields(message)); wc <= 4 {
		scores[TierHeavy] *= 0.35
		scores[TierMax] *= 0.35
	}
	// 3. "tradeoffs between X and Y" is a targeted comparison → reason, not a full review.
	if strings.Contains(lower, "tradeoff") && strings.Contains(lower, "between") {
		scores[TierReason] += 1.0
		if scores[TierMax] > 0 {
			scores[TierMax] -= 0.5
		}
	}
	// 4. "what does X mean" is a definition lookup → trivial, not fast.
	// Exclude error-related queries — "what does this error mean" is a fix question.
	if strings.Contains(lower, "what does") && strings.Contains(lower, "mean") &&
		!strings.Contains(lower, "error") && !strings.Contains(lower, "bug") &&
		!strings.Contains(lower, "crash") && !strings.Contains(lower, "fail") {
		scores[TierTrivial] += 1.5
	}

	// Pick tier with highest score.
	best := TierCode
	bestScore := 0.0
	for tier, score := range scores {
		if score > bestScore {
			bestScore = score
			best = tier
		}
	}

	if bestScore == 0 {
		// No keywords matched — default to code tier.
		return Intent{Tier: TierCode, TaskType: detectTaskType(lower), NeedsGraph: needsGraph(lower), Confidence: 0.60}
	}

	// Confidence = fraction of total score held by winner, mapped to [0.60, 0.95].
	totalScore := 0.0
	for _, s := range scores {
		totalScore += s
	}
	confidence := 0.60 + 0.35*(bestScore/totalScore)
	if confidence > 0.95 {
		confidence = 0.95
	}

	taskType := detectTaskType(lower)
	switch best {
	case TierMax:
		taskType = "ensemble"
	case TierReason:
		taskType = "reason"
	case TierHeavy:
		taskType = "design"
	}

	intent := Intent{
		Tier:       best,
		TaskType:   taskType,
		NeedsGraph: needsGraph(lower),
		Confidence: confidence,
	}

	// Layer 3: embedding kNN — only for ambiguous queries (conf < 0.82).
	if confidence < 0.82 && store != nil {
		intent = classifyByEmbedding(ctx, message, store, intent)
		intent.TaskType = taskType            // preserve detected task type
		intent.NeedsGraph = needsGraph(lower) // preserve graph flag
	}

	// Fix tasks need at least TierCode — small models can't fix errors well.
	// Exception 1: trivial fixes (typo, spelling, rename) stay trivial.
	// Exception 2: when another tier dominates (score ≥ 2× the code score),
	// the user clearly wants that tier, not a fix — don't override.
	if intent.TaskType == "fix" && intent.Tier < TierCode {
		isTrivialFix := strings.Contains(lower, "typo") ||
			strings.Contains(lower, "spelling") ||
			strings.Contains(lower, "rename")
		dominantScore := scores[best]
		codeScore := scores[TierCode]
		isDominant := dominantScore >= 2*codeScore && codeScore > 0
		if !isTrivialFix && !isDominant {
			intent.Tier = TierCode
		}
	}

	return intent
}

var maxKeywords = []string{
	"find all bug", "find bugs", "audit", "code review", "review all",
	"compare approach", "compare approaches", "compare all", "which is better",
	"comprehensive", "thorough", "deep dive", "full analysis",
	"all issues", "all problem", "security", "vulnerability",
	"production ready", "production-ready", "before deploy", "before release",
	"tradeoff", "evaluate all", "to scale", "use case",
}

var reasonKeywords = []string{
	"why does", "explain how", "how does", "how do i decide",
	"architecture", "design pattern", "trade-off", "tradeoff", "when to use",
	"difference between", "what is the best way", "what approach", "best approach",
	"reasoning", "analyse", "analyze", "pros and cons", "pros", "cons",
	"should i use", "when should i use", "how should i",
	// Moved from heavyKeywords — "explain the whole X" is a reasoning question.
	"explain the whole", "explain the entire",
}

var heavyKeywords = []string{
	"refactor entire", "refactor all", "refactor all files", "refactor all the", "refactor the whole", "rewrite",
	"migrate", "explain all", "codebase",
	"multi-file", "across all", "every file", "full project", "from scratch",
	"plan the", "strategy for",
	// Specific multi-file project builds (not generic "build a X").
	"create a full", "create an app", "build a full", "build a backend",
	"build a rest api", "build a graphql", "build a microservice",
	"build a complete", "build the entire", "the entire", "implement the entire",
	"clone", "scaffold", "full stack", "full-stack", "entire app",
	"rest api", "graphql api", "a microservice", "backend for",
	"setup project", "initialize project",
	"implement the whole", "build the whole", "refactor the whole",
	"the whole service", "the whole system", "the whole module", "the whole package",
	"whole notification", "whole user", "whole payment", "whole auth",
}

var codeKeywords = []string{
	"implement", "write", "create", "add", "build",
	"fix", "bug", "error", "broken", "crash", "exception",
	"refactor", "improve", "optimise", "optimize", "clean up",
	"test", "spec", "mock", "function", "method", "class",
	"api", "endpoint", "query", "type", "interface",
}

var fastKeywords = []string{
	"what does", "what is this", "explain this line", "what type",
	"how to use", "how to", "how do i", "how do you", "example of", "show me", "snippet",
}

var trivialKeywords = []string{
	"what is", "what's", "define", "meaning of", "syntax for",
	"typo", "spelling", "rename", "one line", "single line",
	"autocomplete", "complete this",
	"how to declare", "how to define", "how to name",
}

// terminalErrorSignatures — raw error output pasted from the shell.
// normalizeTerminalInput() rewrites these as "fix this X error:" first,
// then Classify() routes them to fast/fix regardless of phrasing.
var terminalErrorSignatures = []string{
	"command not found",
	"npm err!",
	"npm warn",
	"error ts", // TypeScript compiler
	"error[e",  // Rust compiler
	"syntaxerror:",
	"typeerror:",
	"referenceerror:",
	"cannot find module",
	"module not found",
	"enoent:",
	"eaddrinuse",
	"traceback (most recent call last)",
	"exit status 1",
	"panic:",
	"fix this shell error",
	"fix this npm error",
	"fix this runtime error",
	"fix this typescript error",
	"fix this python traceback",
}

var graphKeywords = []string{
	"break", "affect", "impact", "depend", "import", "uses", "calls",
	"fix", "bug", "error", "why does", "where is", "find", "dead code",
	"circular", "refactor", "change", "modify", "delete", "remove",
	"what calls", "who imports",
}

func needsGraph(lower string) bool {
	for _, kw := range graphKeywords {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}

func detectTaskType(lower string) string {
	switch {
	case strings.Contains(lower, "explain") || strings.Contains(lower, "how does") || strings.Contains(lower, "what does"):
		return "explain"
	case strings.Contains(lower, "fix") || strings.Contains(lower, "bug") || strings.Contains(lower, "error") || strings.Contains(lower, "broken"):
		return "fix"
	case strings.Contains(lower, "refactor") || strings.Contains(lower, "improve") || strings.Contains(lower, "clean"):
		return "refactor"
	case strings.Contains(lower, "impact") || strings.Contains(lower, "affect") || strings.Contains(lower, "break"):
		return "impact-query"
	case strings.Contains(lower, "test") || strings.Contains(lower, "spec"):
		return "test"
	case strings.Contains(lower, "write") || strings.Contains(lower, "create") || strings.Contains(lower, "add") || strings.Contains(lower, "implement"):
		return "implement"
	default:
		return "general"
	}
}
