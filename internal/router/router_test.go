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

// ── Golden-set quality evaluation ────────────────────────────────────────────

// TestClassifyGoldenSet measures classification accuracy over all RouterExamples.
// Fails if accuracy drops below 80 % — a regression guard for the scoring logic.
func TestClassifyGoldenSet(t *testing.T) {
	total := len(RouterExamples)
	if total == 0 {
		t.Fatal("RouterExamples is empty")
	}

	correct := 0
	for _, ex := range RouterExamples {
		intent := Classify(ex.Query, false)
		if intent.Tier == ex.Tier {
			correct++
		} else {
			t.Logf("MISS query=%q  got=%s  want=%s", ex.Query, intent.Tier, ex.Tier)
		}
	}

	accuracy := float64(correct) / float64(total) * 100
	t.Logf("golden-set accuracy: %d/%d = %.1f%%", correct, total, accuracy)

	const minAccuracy = 80.0
	if accuracy < minAccuracy {
		t.Errorf("accuracy %.1f%% below threshold %.0f%%", accuracy, minAccuracy)
	}
}

// ── Dampener regressions ──────────────────────────────────────────────────────

// TestShortMessageDampener verifies that ≤4-word messages do not route to
// Heavy or Max (regression for the short-message dampener: heavy*0.35, max*0.35).
func TestShortMessageDampener(t *testing.T) {
	cases := []string{
		"build a server",
		"help me build",
		"what is goroutine",
		"fix the code",
	}
	for _, msg := range cases {
		intent := Classify(msg, false)
		if intent.Tier == TierHeavy || intent.Tier == TierMax {
			t.Errorf("short message %q routed to %s — dampener not applied", msg, intent.Tier)
		}
	}
}

// TestQuestionFormDampener verifies trailing '?' boosts TierReason over TierHeavy.
func TestQuestionFormDampener(t *testing.T) {
	cases := []struct {
		query   string
		wantNot []Tier // these tiers should NOT win
	}{
		{
			"how does the auth flow work?",
			[]Tier{TierHeavy, TierMax},
		},
		{
			"what causes a deadlock in goroutines?",
			[]Tier{TierHeavy, TierMax},
		},
	}
	for _, c := range cases {
		intent := Classify(c.query, false)
		for _, bad := range c.wantNot {
			if intent.Tier == bad {
				t.Errorf("question %q routed to %s — question dampener not applied", c.query, bad)
			}
		}
	}
}

// TestTerminalErrorCodePath verifies that a pasted panic/error message routes
// to TierCode (small models can't fix errors well — needs a capable model).
func TestTerminalErrorCodePath(t *testing.T) {
	panicMsg := "panic: runtime error: index out of range [3] with length 3"
	intent := Classify(panicMsg, false)
	if intent.Tier != TierCode {
		t.Errorf("panic paste routed to %s, want TierCode", intent.Tier)
	}
}

// ── Benchmarks ────────────────────────────────────────────────────────────────

func BenchmarkClassify(b *testing.B) {
	msgs := []string{
		"fix the null pointer in auth.go",
		"implement retry mechanism with exponential backoff",
		"explain the tradeoffs of redis vs sqlite",
		"what is a goroutine",
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		Classify(msgs[i%len(msgs)], false)
	}
}
