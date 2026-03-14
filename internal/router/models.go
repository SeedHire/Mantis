package router

import (
	"sort"
	"strconv"
	"strings"

	"github.com/seedhire/mantis/internal/ollama"
)

// ModelTraits describes known capabilities of a model family.
type ModelTraits struct {
	Family       string   // prefix to match against model name (e.g., "qwen3.5", "glm-5")
	SWEBench     float64  // SWE-bench Verified % (0-100), 0 = unknown
	ToolUse      float64  // BFCL-V4 tool-use % (0-100), 0 = unknown
	Reasoning    float64  // GPQA-Diamond % (0-100), 0 = unknown
	MaxCtx       int      // advertised max context window
	PracticalCtx int      // recommended practical NumCtx for Ollama
	IsMultimodal bool     // supports vision/image input
	Strengths    []string // tags: "coding", "reasoning", "tool-use", "vision", "fast"
}

// knownModels maps model family prefixes to their benchmark traits.
// Ordered by Family string length descending so longer prefixes match first.
var knownModels = []ModelTraits{
	// ── Top-tier coding models ──
	{Family: "glm-5", SWEBench: 77.8, ToolUse: 55, Reasoning: 86.0, MaxCtx: 198000, PracticalCtx: 49152, Strengths: []string{"coding", "reasoning"}},
	{Family: "glm-4.7", SWEBench: 73.8, ToolUse: 50, Reasoning: 70, MaxCtx: 200000, PracticalCtx: 49152, Strengths: []string{"coding", "fast"}},
	{Family: "glm-4.6", SWEBench: 55, ToolUse: 40, Reasoning: 55, MaxCtx: 128000, PracticalCtx: 32768, Strengths: []string{"coding"}},
	{Family: "devstral-2", SWEBench: 72.2, ToolUse: 60, Reasoning: 65, MaxCtx: 256000, PracticalCtx: 65536, Strengths: []string{"coding", "tool-use"}},
	{Family: "devstral-small-2", SWEBench: 68.0, ToolUse: 55, Reasoning: 58, MaxCtx: 256000, PracticalCtx: 65536, Strengths: []string{"coding", "fast"}},

	// ── Qwen family (multimodal + strong tool use) ──
	{Family: "qwen3.5", SWEBench: 72.4, ToolUse: 72.2, Reasoning: 85.5, MaxCtx: 262000, PracticalCtx: 65536, IsMultimodal: true, Strengths: []string{"coding", "tool-use", "reasoning", "vision"}},
	{Family: "qwen3-coder-next", SWEBench: 65, ToolUse: 60, Reasoning: 60, MaxCtx: 262000, PracticalCtx: 65536, Strengths: []string{"coding"}},
	{Family: "qwen3-next", SWEBench: 55, ToolUse: 50, Reasoning: 70, MaxCtx: 262000, PracticalCtx: 65536, Strengths: []string{"reasoning"}},
	{Family: "qwen3-vl", SWEBench: 50, ToolUse: 45, Reasoning: 70, MaxCtx: 262000, PracticalCtx: 65536, IsMultimodal: true, Strengths: []string{"vision", "reasoning"}},

	// ── Reasoning specialists ──
	{Family: "cogito-2.1", SWEBench: 60, ToolUse: 45, Reasoning: 80, MaxCtx: 128000, PracticalCtx: 65536, Strengths: []string{"reasoning"}},
	{Family: "kimi-k2.5", SWEBench: 65, ToolUse: 55, Reasoning: 75, MaxCtx: 256000, PracticalCtx: 65536, IsMultimodal: true, Strengths: []string{"coding", "reasoning", "vision"}},
	{Family: "deepseek-v3.2", SWEBench: 60, ToolUse: 50, Reasoning: 72, MaxCtx: 128000, PracticalCtx: 65536, Strengths: []string{"reasoning"}},
	{Family: "deepseek-v3", SWEBench: 55, ToolUse: 45, Reasoning: 68, MaxCtx: 128000, PracticalCtx: 65536, Strengths: []string{"reasoning"}},
	{Family: "deepseek-r1", SWEBench: 50, ToolUse: 30, Reasoning: 75, MaxCtx: 128000, PracticalCtx: 49152, Strengths: []string{"reasoning"}},

	// ── General-purpose cloud models ──
	{Family: "minimax-m2.5", SWEBench: 60, ToolUse: 50, Reasoning: 65, MaxCtx: 128000, PracticalCtx: 49152, Strengths: []string{"coding"}},
	{Family: "minimax-m2.1", SWEBench: 55, ToolUse: 45, Reasoning: 60, MaxCtx: 128000, PracticalCtx: 49152, Strengths: []string{"coding"}},
	{Family: "minimax-m2", SWEBench: 50, ToolUse: 40, Reasoning: 55, MaxCtx: 128000, PracticalCtx: 32768, Strengths: []string{"coding"}},
	{Family: "nemotron-3-super", SWEBench: 55, ToolUse: 45, Reasoning: 65, MaxCtx: 128000, PracticalCtx: 65536, Strengths: []string{"reasoning"}},
	{Family: "nemotron-3-nano", SWEBench: 40, ToolUse: 35, Reasoning: 50, MaxCtx: 128000, PracticalCtx: 32768, Strengths: []string{"fast"}},
	{Family: "gemini-3-flash-preview", SWEBench: 50, ToolUse: 55, Reasoning: 90.4, MaxCtx: 1000000, PracticalCtx: 131072, IsMultimodal: true, Strengths: []string{"vision", "reasoning"}},

	// ── Smaller / utility models ──
	{Family: "ministral-3", SWEBench: 30, ToolUse: 25, Reasoning: 35, MaxCtx: 128000, PracticalCtx: 32768, Strengths: []string{"fast"}},
	{Family: "rnj-1", SWEBench: 35, ToolUse: 30, Reasoning: 40, MaxCtx: 128000, PracticalCtx: 32768, Strengths: []string{"fast"}},

	// ── Local model families ──
	{Family: "qwen2.5-coder", SWEBench: 45, ToolUse: 40, Reasoning: 45, MaxCtx: 32768, PracticalCtx: 32768, Strengths: []string{"coding", "fast"}},
	{Family: "llama3.3", SWEBench: 40, ToolUse: 35, Reasoning: 55, MaxCtx: 128000, PracticalCtx: 32768, Strengths: []string{"reasoning"}},
	{Family: "llama3.2-vision", SWEBench: 25, ToolUse: 20, Reasoning: 40, MaxCtx: 128000, PracticalCtx: 32768, IsMultimodal: true, Strengths: []string{"vision"}},
	{Family: "llama3.2", SWEBench: 25, ToolUse: 20, Reasoning: 35, MaxCtx: 128000, PracticalCtx: 32768, Strengths: []string{"fast"}},
	{Family: "llama3.1", SWEBench: 35, ToolUse: 30, Reasoning: 50, MaxCtx: 128000, PracticalCtx: 32768, Strengths: []string{"reasoning"}},
	{Family: "gemma3", SWEBench: 30, ToolUse: 25, Reasoning: 40, MaxCtx: 32768, PracticalCtx: 16384, Strengths: []string{"fast"}},
	{Family: "gemma2", SWEBench: 25, ToolUse: 20, Reasoning: 35, MaxCtx: 8192, PracticalCtx: 8192, Strengths: []string{"fast"}},
	{Family: "llava", SWEBench: 15, ToolUse: 10, Reasoning: 25, MaxCtx: 4096, PracticalCtx: 4096, IsMultimodal: true, Strengths: []string{"vision"}},
	{Family: "moondream", SWEBench: 10, ToolUse: 5, Reasoning: 20, MaxCtx: 2048, PracticalCtx: 2048, IsMultimodal: true, Strengths: []string{"vision", "fast"}},
}

// matchTraits finds the best-matching ModelTraits for a model name.
// Returns nil if no known family matches.
func matchTraits(modelName string) *ModelTraits {
	lower := strings.ToLower(modelName)
	var best *ModelTraits
	bestLen := 0
	for i := range knownModels {
		fam := knownModels[i].Family
		if strings.HasPrefix(lower, fam) && len(fam) > bestLen {
			best = &knownModels[i]
			bestLen = len(fam)
		}
	}
	return best
}

// parseModelSize extracts the parameter count in billions from a model name.
// E.g., "qwen3.5:122b" → 122, "glm-5:cloud" → 0, "devstral-2:123b-cloud" → 123.
func parseModelSize(name string) float64 {
	lower := strings.ToLower(name)
	// Check for :Nb or :N.Mb patterns
	if idx := strings.Index(lower, ":"); idx >= 0 {
		tag := lower[idx+1:]
		// Remove trailing suffixes like "-cloud"
		for _, suf := range []string{"-cloud", "-preview", "-instruct", "-chat"} {
			tag = strings.TrimSuffix(tag, suf)
		}
		tag = strings.TrimSuffix(tag, "b")
		if v, err := strconv.ParseFloat(tag, 64); err == nil {
			return v
		}
	}
	return 0
}

// scoreForTier computes a suitability score for a model+traits combo for a given tier.
func scoreForTier(modelName string, traits *ModelTraits, tier Tier) float64 {
	size := parseModelSize(modelName)

	switch tier {
	case TierTrivial:
		// Prefer smallest models for speed
		score := 50.0
		if size > 0 && size <= 4 {
			score += 30
		} else if size > 4 && size <= 8 {
			score += 15
		} else if size > 8 {
			score -= size // penalize large models
		}
		if hasStrength(traits, "fast") {
			score += 10
		}
		return score

	case TierFast:
		// Prefer 9b-35b range with good SWE + tool use
		score := traits.SWEBench*0.4 + traits.ToolUse*0.3
		if size >= 9 && size <= 35 {
			score += 20
		} else if size > 0 && size < 9 {
			score += 10
		} else if size > 35 {
			score -= 5 // slightly penalize very large for "fast" tier
		}
		if hasStrength(traits, "fast") {
			score += 10
		}
		return score

	case TierCode:
		// Coding specialist: SWE-bench dominant
		score := traits.SWEBench*0.5 + traits.ToolUse*0.3 + traits.Reasoning*0.1
		if size >= 30 {
			score += 10
		}
		if hasStrength(traits, "coding") {
			score += 5
		}
		return score

	case TierReason:
		// Reasoning specialist
		score := traits.Reasoning*0.5 + traits.SWEBench*0.3 + traits.ToolUse*0.1
		if hasStrength(traits, "reasoning") {
			score += 10
		}
		if size >= 70 {
			score += 5
		}
		return score

	case TierHeavy:
		// All-round large: SWE + tool use + reasoning + size
		score := traits.SWEBench*0.4 + traits.ToolUse*0.3 + traits.Reasoning*0.2
		if size >= 70 {
			score += 10
		} else if size >= 30 {
			score += 5
		}
		return score

	case TierMax:
		// Same as heavy but prefer largest
		score := traits.SWEBench*0.4 + traits.ToolUse*0.3 + traits.Reasoning*0.2
		if size >= 100 {
			score += 15
		} else if size >= 70 {
			score += 10
		}
		return score

	case TierVision:
		// Must be multimodal, then weight reasoning
		if !traits.IsMultimodal {
			return -1 // exclude non-multimodal
		}
		score := traits.Reasoning*0.5 + traits.SWEBench*0.2 + traits.ToolUse*0.2
		if hasStrength(traits, "vision") {
			score += 15
		}
		return score
	}

	return 0
}

func hasStrength(t *ModelTraits, s string) bool {
	for _, str := range t.Strengths {
		if str == s {
			return true
		}
	}
	return false
}

// DiscoverBest scores all available models against known traits and returns
// the highest-scoring model per tier. Returns only tiers where a match was found.
func DiscoverBest(available []ollama.ModelInfo) map[Tier]string {
	type scored struct {
		name  string
		score float64
	}

	allTiers := []Tier{TierTrivial, TierFast, TierCode, TierReason, TierHeavy, TierMax, TierVision}
	result := make(map[Tier]string, len(allTiers))

	// Build per-tier candidate lists.
	tierCandidates := make(map[Tier][]scored)
	for _, m := range available {
		traits := matchTraits(m.Name)
		if traits == nil {
			continue // unknown family — skip, let legacy fallback handle it
		}
		for _, tier := range allTiers {
			s := scoreForTier(m.Name, traits, tier)
			if s > 0 {
				tierCandidates[tier] = append(tierCandidates[tier], scored{m.Name, s})
			}
		}
	}

	// Pick highest score per tier.
	for _, tier := range allTiers {
		candidates := tierCandidates[tier]
		if len(candidates) == 0 {
			continue
		}
		sort.Slice(candidates, func(i, j int) bool {
			return candidates[i].score > candidates[j].score
		})
		result[tier] = candidates[0].name
	}

	return result
}
