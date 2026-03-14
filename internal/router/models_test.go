package router

import (
	"testing"

	"github.com/seedhire/mantis/internal/ollama"
)

func TestDiscoverBest_BasicAssignment(t *testing.T) {
	available := []ollama.ModelInfo{
		{Name: "glm-5:cloud", Size: 100_000_000_000},
		{Name: "qwen3.5:122b", Size: 70_000_000_000},
		{Name: "qwen3.5:27b", Size: 16_000_000_000},
		{Name: "qwen3.5:4b", Size: 2_500_000_000},
		{Name: "gemini-3-flash-preview:cloud", Size: 50_000_000_000},
		{Name: "devstral-2:123b-cloud", Size: 70_000_000_000},
	}
	result := DiscoverBest(available)

	// Should have assignments for all tiers
	for _, tier := range []Tier{TierTrivial, TierFast, TierCode, TierReason, TierHeavy, TierMax, TierVision} {
		if result[tier] == "" {
			t.Errorf("tier %s has no model assigned", tier)
		}
	}
}

func TestDiscoverBest_TrivialPrefersSmall(t *testing.T) {
	available := []ollama.ModelInfo{
		{Name: "qwen3.5:4b", Size: 2_500_000_000},
		{Name: "qwen3.5:122b", Size: 70_000_000_000},
		{Name: "qwen3.5:27b", Size: 16_000_000_000},
	}
	result := DiscoverBest(available)
	if result[TierTrivial] != "qwen3.5:4b" {
		t.Errorf("TierTrivial = %q, want qwen3.5:4b", result[TierTrivial])
	}
}

func TestDiscoverBest_MaxPrefersLargest(t *testing.T) {
	available := []ollama.ModelInfo{
		{Name: "qwen3.5:4b", Size: 2_500_000_000},
		{Name: "qwen3.5:122b", Size: 70_000_000_000},
		{Name: "glm-5:cloud", Size: 100_000_000_000},
	}
	result := DiscoverBest(available)
	// qwen3.5:122b wins Max: high SWE + best tool-use + large size bonus
	if result[TierMax] != "qwen3.5:122b" {
		t.Errorf("TierMax = %q, want qwen3.5:122b", result[TierMax])
	}
}

func TestDiscoverBest_VisionExcludesNonMultimodal(t *testing.T) {
	available := []ollama.ModelInfo{
		{Name: "glm-5:cloud", Size: 100_000_000_000},  // not multimodal
		{Name: "devstral-2:123b-cloud", Size: 70_000_000_000}, // not multimodal
	}
	result := DiscoverBest(available)
	if result[TierVision] != "" {
		t.Errorf("TierVision = %q, want empty (no multimodal models)", result[TierVision])
	}
}

func TestDiscoverBest_VisionPicksMultimodal(t *testing.T) {
	available := []ollama.ModelInfo{
		{Name: "glm-5:cloud", Size: 100_000_000_000},
		{Name: "gemini-3-flash-preview:cloud", Size: 50_000_000_000},
		{Name: "qwen3-vl:235b-cloud", Size: 120_000_000_000},
	}
	result := DiscoverBest(available)
	// gemini-3 has 90.4% reasoning + vision strength → should win
	if result[TierVision] != "gemini-3-flash-preview:cloud" {
		t.Errorf("TierVision = %q, want gemini-3-flash-preview:cloud", result[TierVision])
	}
}

func TestDiscoverBest_UnknownFamilyGraceful(t *testing.T) {
	available := []ollama.ModelInfo{
		{Name: "totally-unknown-model:7b", Size: 4_000_000_000},
	}
	result := DiscoverBest(available)
	// Unknown family should return empty — no panics
	for _, tier := range []Tier{TierTrivial, TierFast, TierCode, TierReason, TierHeavy, TierMax, TierVision} {
		if result[tier] != "" {
			t.Errorf("tier %s = %q for unknown model, want empty", tier, result[tier])
		}
	}
}

func TestMatchTraits(t *testing.T) {
	// Longer prefix should win
	traits := matchTraits("devstral-small-2:24b-cloud")
	if traits == nil || traits.Family != "devstral-small-2" {
		t.Errorf("matchTraits(devstral-small-2:24b-cloud) = %v, want family=devstral-small-2", traits)
	}

	// Short prefix
	traits = matchTraits("glm-5:cloud")
	if traits == nil || traits.Family != "glm-5" {
		t.Errorf("matchTraits(glm-5:cloud) = %v, want family=glm-5", traits)
	}

	// Unknown
	traits = matchTraits("unknown-model:7b")
	if traits != nil {
		t.Errorf("matchTraits(unknown-model:7b) = %v, want nil", traits)
	}
}

func TestParseModelSize(t *testing.T) {
	tests := []struct {
		name string
		want float64
	}{
		{"qwen3.5:122b", 122},
		{"qwen3.5:4b", 4},
		{"devstral-2:123b-cloud", 123},
		{"glm-5:cloud", 0},
		{"qwen3.5:0.8b", 0.8},
		{"llama3.2:1b", 1},
	}
	for _, tt := range tests {
		got := parseModelSize(tt.name)
		if got != tt.want {
			t.Errorf("parseModelSize(%q) = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestDiscoverBest_CodeWeightsSWEAndToolUse(t *testing.T) {
	available := []ollama.ModelInfo{
		{Name: "glm-5:cloud", Size: 100_000_000_000},         // SWE 77.8, ToolUse 55
		{Name: "qwen3.5:122b", Size: 70_000_000_000},         // SWE 72.4, ToolUse 72.2, size 122b
		{Name: "devstral-2:123b-cloud", Size: 70_000_000_000}, // SWE 72.2, ToolUse 60
	}
	result := DiscoverBest(available)
	// qwen3.5:122b wins Code: SWE*0.5 + ToolUse*0.3 (72.2!) + size bonus
	if result[TierCode] != "qwen3.5:122b" {
		t.Errorf("TierCode = %q, want qwen3.5:122b (best combined SWE+ToolUse+size)", result[TierCode])
	}
}
