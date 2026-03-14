package repl

import (
	"strings"

	"github.com/seedhire/mantis/internal/router"
)

// EffortLevel controls how much thinking the model should do.
type EffortLevel int

const (
	EffortLow    EffortLevel = iota // skip thinking, prefer fast models
	EffortMedium                    // default — use router's tier selection
	EffortHigh                      // force thinking instruction, prefer reason+ models
)

func (e EffortLevel) String() string {
	switch e {
	case EffortLow:
		return "low"
	case EffortMedium:
		return "medium"
	case EffortHigh:
		return "high"
	}
	return "unknown"
}

// ParseEffortLevel parses user input into an effort level.
func ParseEffortLevel(s string) (EffortLevel, bool) {
	switch strings.TrimSpace(strings.ToLower(s)) {
	case "low", "l", "fast":
		return EffortLow, true
	case "medium", "m", "med", "default":
		return EffortMedium, true
	case "high", "h", "max", "think":
		return EffortHigh, true
	}
	return EffortMedium, false
}

// MinTier returns the minimum routing tier for this effort level.
func (e EffortLevel) MinTier() router.Tier {
	switch e {
	case EffortLow:
		return router.TierTrivial
	case EffortHigh:
		return router.TierReason
	default:
		return router.TierTrivial
	}
}

// MaxTier returns the maximum routing tier for this effort level.
func (e EffortLevel) MaxTier() router.Tier {
	switch e {
	case EffortLow:
		return router.TierFast
	default:
		return router.TierMax
	}
}

// ThinkingInstruction returns a thinking instruction to prepend to the system
// prompt for models that support extended thinking (kimi-k2-thinking, deepseek-r1).
func (e EffortLevel) ThinkingInstruction() string {
	if e != EffortHigh {
		return ""
	}
	return `<thinking_instruction>
Think step-by-step before responding. Break down the problem, consider edge cases,
and verify your reasoning before giving the final answer. Use <think>...</think>
tags to show your reasoning process.
</thinking_instruction>

`
}

// IsThinkingModel returns true for models that natively support extended thinking.
func IsThinkingModel(model string) bool {
	lower := strings.ToLower(model)
	return strings.Contains(lower, "thinking") ||
		strings.Contains(lower, "deepseek-r1") ||
		strings.Contains(lower, "qwq")
}
