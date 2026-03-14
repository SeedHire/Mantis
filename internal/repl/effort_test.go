package repl

import (
	"testing"

	"github.com/seedhire/mantis/internal/router"
)

func TestParseEffortLevel(t *testing.T) {
	cases := []struct {
		input string
		want  EffortLevel
		ok    bool
	}{
		{"low", EffortLow, true},
		{"l", EffortLow, true},
		{"fast", EffortLow, true},
		{"medium", EffortMedium, true},
		{"m", EffortMedium, true},
		{"high", EffortHigh, true},
		{"h", EffortHigh, true},
		{"think", EffortHigh, true},
		{"invalid", EffortMedium, false},
	}
	for _, tc := range cases {
		got, ok := ParseEffortLevel(tc.input)
		if ok != tc.ok || got != tc.want {
			t.Errorf("ParseEffortLevel(%q) = (%v, %v), want (%v, %v)", tc.input, got, ok, tc.want, tc.ok)
		}
	}
}

func TestEffortLevel_MinMaxTier(t *testing.T) {
	if EffortLow.MaxTier() != router.TierFast {
		t.Error("low effort max tier should be Fast")
	}
	if EffortHigh.MinTier() != router.TierReason {
		t.Error("high effort min tier should be Reason")
	}
	if EffortMedium.MinTier() != router.TierTrivial {
		t.Error("medium effort min tier should be Trivial")
	}
}

func TestEffortLevel_ThinkingInstruction(t *testing.T) {
	if EffortLow.ThinkingInstruction() != "" {
		t.Error("low effort should have no thinking instruction")
	}
	if EffortMedium.ThinkingInstruction() != "" {
		t.Error("medium effort should have no thinking instruction")
	}
	if EffortHigh.ThinkingInstruction() == "" {
		t.Error("high effort should have thinking instruction")
	}
}

func TestIsThinkingModel(t *testing.T) {
	if !IsThinkingModel("kimi-k2-thinking:cloud") {
		t.Error("kimi-k2-thinking should be a thinking model")
	}
	if !IsThinkingModel("deepseek-r1:14b") {
		t.Error("deepseek-r1 should be a thinking model")
	}
	if IsThinkingModel("gemma3:4b") {
		t.Error("gemma3 should not be a thinking model")
	}
}
