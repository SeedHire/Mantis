package router

import (
	"testing"

	"github.com/seedhire/mantis/internal/ollama"
)

func TestTierString(t *testing.T) {
	tests := []struct {
		tier Tier
		want string
	}{
		{TierTrivial, "trivial"},
		{TierFast, "fast"},
		{TierCode, "code"},
		{TierReason, "reason"},
		{TierHeavy, "heavy"},
		{TierMax, "max"},
		{TierVision, "vision"},
	}
	for _, tt := range tests {
		if got := tt.tier.String(); got != tt.want {
			t.Errorf("Tier(%d).String() = %q, want %q", tt.tier, got, tt.want)
		}
	}
}

func TestClassifyVision(t *testing.T) {
	intent := Classify("what is this?", true)
	if intent.Tier != TierVision {
		t.Errorf("expected TierVision, got %v", intent.Tier)
	}
	if intent.Confidence != 1.0 {
		t.Errorf("expected confidence 1.0, got %f", intent.Confidence)
	}
	if !intent.NeedsVision {
		t.Error("expected NeedsVision = true")
	}
}

func TestClassifyTiers(t *testing.T) {
	tests := []struct {
		message string
		want    Tier
	}{
		{"compare approaches for caching", TierMax},
		{"evaluate the tradeoffs of using redis", TierMax},
		{"explain how the architecture of auth works", TierReason},
		{"what are the tradeoffs here", TierMax}, // "tradeoff" matches max
		{"rewrite the entire payment system", TierHeavy},
		{"refactor the login handler", TierCode},
		{"fix the null pointer in auth.go", TierCode},
		{"implement a retry mechanism", TierCode},
		{"what does fmt.Println do", TierFast},
		{"show me an example of goroutines", TierFast},
		{"define struct", TierTrivial},
		{"syntax for if else", TierTrivial},
	}
	for _, tt := range tests {
		intent := Classify(tt.message, false)
		if intent.Tier != tt.want {
			t.Errorf("Classify(%q).Tier = %v, want %v", tt.message, intent.Tier, tt.want)
		}
	}
}

func TestClassifyDefaultLowConfidence(t *testing.T) {
	intent := Classify("hello there", false)
	if intent.Confidence != 0.60 {
		t.Errorf("default confidence = %f, want 0.60", intent.Confidence)
	}
}

func TestClassifyNeedsGraph(t *testing.T) {
	intent := Classify("what depends on the auth module", false)
	if !intent.NeedsGraph {
		t.Error("expected NeedsGraph for dependency question")
	}
}

func TestModelForDefault(t *testing.T) {
	// Clear resolved models to test defaults.
	old := make(map[Tier]string)
	for k, v := range resolvedModels {
		old[k] = v
	}
	defer func() { resolvedModels = old }()
	resolvedModels = map[Tier]string{}

	model := ModelFor(TierCode)
	if model == "" {
		t.Error("ModelFor(TierCode) should return a default, got empty")
	}
}

func TestSetResolvedAndModelFor(t *testing.T) {
	old := make(map[Tier]string)
	for k, v := range resolvedModels {
		old[k] = v
	}
	defer func() { resolvedModels = old }()

	SetResolved(TierFast, "test-model:7b")
	if got := ModelFor(TierFast); got != "test-model:7b" {
		t.Errorf("ModelFor after SetResolved = %q, want %q", got, "test-model:7b")
	}
}

func TestResolvedSummary(t *testing.T) {
	summary := ResolvedSummary()
	expected := []string{"trivial", "fast", "code", "reason", "heavy", "max", "vision"}
	for _, tier := range expected {
		if _, ok := summary[tier]; !ok {
			t.Errorf("ResolvedSummary missing tier %q", tier)
		}
	}
}

func TestResolveAllPicksFromPrefs(t *testing.T) {
	old := make(map[Tier]string)
	for k, v := range resolvedModels {
		old[k] = v
	}
	defer func() { resolvedModels = old }()
	resolvedModels = map[Tier]string{}

	available := []ollama.ModelInfo{
		{Name: "gemma3:4b", Size: 4_000_000_000},
		{Name: "devstral-small-2:24b", Size: 24_000_000_000},
	}
	ResolveAll(available)

	if m := ModelFor(TierTrivial); m != "gemma3:4b" {
		t.Errorf("TierTrivial resolved to %q, want gemma3:4b", m)
	}
}

func TestIsQuantized(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"llama3:q4_0", true},
		{"llama3:q8_0", true},
		{"model-q4-gguf", true},
		{"gemma3:4b", false},
		{"devstral:24b", false},
	}
	for _, tt := range tests {
		if got := isQuantized(tt.name); got != tt.want {
			t.Errorf("isQuantized(%q) = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestPickBySizePreferQuantized(t *testing.T) {
	available := []ollama.ModelInfo{
		{Name: "gemma3:4b", Size: 4_000_000_000},
		{Name: "gemma3:q4_0", Size: 3_500_000_000},
	}
	got := pickBySize(available, TierTrivial)
	if got != "gemma3:q4_0" {
		t.Errorf("pickBySize(Trivial) = %q, want quantized model gemma3:q4_0", got)
	}
}

func TestPickBySizeEmpty(t *testing.T) {
	got := pickBySize(nil, TierCode)
	if got != "" {
		t.Errorf("pickBySize(nil) = %q, want empty", got)
	}
}
