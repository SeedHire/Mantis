package router

import (
	"fmt"
	"math"
	"strings"
	"testing"
)

// ── AutoResearch-style eval harness ──────────────────────────────────────────
// Inspired by Karpathy's autoresearch: enumerate weight/modifier mutations,
// evaluate each against the golden set, keep improvements.

// weightSet holds one configuration of scoring weights + modifiers.
type weightSet struct {
	Max     float64
	Reason  float64
	Heavy   float64
	Code    float64
	Fast    float64
	Trivial float64

	// Context modifiers
	QuestionHeavyDamp  float64 // multiply heavy score when trailing "?"
	QuestionCodeDamp   float64 // multiply code score when trailing "?"
	QuestionReasonBoost float64 // multiply reason score when trailing "?"
	ShortHeavyDamp     float64 // multiply heavy when ≤4 words
	ShortMaxDamp       float64 // multiply max when ≤4 words
}

// baseline returns the current production weights.
func baseline() weightSet {
	return weightSet{
		Max: 1.5, Reason: 1.2, Heavy: 1.3, Code: 0.8, Fast: 0.9, Trivial: 1.0,
		QuestionHeavyDamp: 0.4, QuestionCodeDamp: 0.7, QuestionReasonBoost: 1.3,
		ShortHeavyDamp: 0.35, ShortMaxDamp: 0.35,
	}
}

// classifyWith runs the classification logic with custom weights (no embedding layer).
// Also applies the fix-floor bypass for trivial typo/rename tasks.
func classifyWith(message string, ws weightSet, fixFloorBypass bool) Tier {
	lower := strings.ToLower(message)

	// Terminal error → TierCode (unchanged).
	for _, sig := range terminalErrorSignatures {
		if strings.Contains(lower, sig) {
			return TierCode
		}
	}

	scores := map[Tier]float64{}

	for _, kw := range maxKeywords {
		if strings.Contains(lower, kw) {
			scores[TierMax] += ws.Max
		}
	}
	for _, kw := range reasonKeywords {
		if strings.Contains(lower, kw) {
			scores[TierReason] += ws.Reason
		}
	}
	for _, kw := range heavyKeywords {
		if strings.Contains(lower, kw) {
			scores[TierHeavy] += ws.Heavy
		}
	}
	for _, kw := range codeKeywords {
		if strings.Contains(lower, kw) {
			scores[TierCode] += ws.Code
		}
	}
	for _, kw := range fastKeywords {
		if strings.Contains(lower, kw) {
			scores[TierFast] += ws.Fast
		}
	}
	for _, kw := range trivialKeywords {
		if strings.Contains(lower, kw) {
			scores[TierTrivial] += ws.Trivial
		}
	}

	// Context modifiers
	if strings.Contains(lower, "how to run") || strings.Contains(lower, "how to start") {
		scores[TierTrivial] = 0
	}

	isQuestion := strings.HasSuffix(strings.TrimSpace(lower), "?")
	if isQuestion {
		scores[TierHeavy] *= ws.QuestionHeavyDamp
		scores[TierCode] *= ws.QuestionCodeDamp
		scores[TierReason] *= ws.QuestionReasonBoost
	}

	if wc := len(strings.Fields(message)); wc <= 4 {
		scores[TierHeavy] *= ws.ShortHeavyDamp
		scores[TierMax] *= ws.ShortMaxDamp
	}

	if strings.Contains(lower, "tradeoff") && strings.Contains(lower, "between") {
		scores[TierReason] += 1.0
		if scores[TierMax] > 0 {
			scores[TierMax] -= 0.5
		}
	}

	best := TierCode
	bestScore := 0.0
	for tier, score := range scores {
		if score > bestScore {
			bestScore = score
			best = tier
		}
	}

	if bestScore == 0 {
		return TierCode
	}

	// Fix-floor: bypass for trivial tasks (typo, rename, spelling).
	if fixFloorBypass {
		taskType := detectTaskType(lower)
		isTrivialFix := taskType == "fix" && (strings.Contains(lower, "typo") ||
			strings.Contains(lower, "spelling") || strings.Contains(lower, "rename"))
		if taskType == "fix" && best < TierCode && !isTrivialFix {
			best = TierCode
		}
	} else {
		// Original fix floor.
		taskType := detectTaskType(lower)
		if taskType == "fix" && best < TierCode {
			best = TierCode
		}
	}

	return best
}

// evalAccuracy measures accuracy of a weight set against golden examples.
func evalAccuracy(ws weightSet, fixFloorBypass bool) (correct int, total int, misses []string) {
	total = len(RouterExamples)
	for _, ex := range RouterExamples {
		got := classifyWith(ex.Query, ws, fixFloorBypass)
		if got == ex.Tier {
			correct++
		} else {
			misses = append(misses, fmt.Sprintf("  %q → got=%s want=%s", ex.Query, got, ex.Tier))
		}
	}
	return
}

// TestAutoResearchBaseline reports baseline accuracy (sanity check).
func TestAutoResearchBaseline(t *testing.T) {
	ws := baseline()
	correct, total, misses := evalAccuracy(ws, false)
	pct := float64(correct) / float64(total) * 100
	t.Logf("BASELINE: %d/%d = %.1f%%", correct, total, pct)
	for _, m := range misses {
		t.Log(m)
	}
}

// TestAutoResearchFixFloorBypass tests accuracy with the typo/rename fix-floor bypass.
func TestAutoResearchFixFloorBypass(t *testing.T) {
	ws := baseline()
	correct, total, misses := evalAccuracy(ws, true)
	pct := float64(correct) / float64(total) * 100
	t.Logf("FIX-FLOOR BYPASS: %d/%d = %.1f%%", correct, total, pct)
	for _, m := range misses {
		t.Log(m)
	}
}

// TestAutoResearchWeightSweep runs a grid search over weight space.
func TestAutoResearchWeightSweep(t *testing.T) {
	type result struct {
		ws       weightSet
		correct  int
		total    int
		accuracy float64
		bypass   bool
	}

	var best result
	best.accuracy = 0

	// Grid: sweep each weight ±0.3 from baseline in 0.1 steps.
	base := baseline()
	steps := []float64{-0.3, -0.2, -0.1, 0, 0.1, 0.2, 0.3}

	experiments := 0

	for _, dMax := range steps {
		for _, dReason := range steps {
			for _, dHeavy := range steps {
				for _, dCode := range steps {
					for _, dFast := range steps {
						for _, dTrivial := range steps {
							ws := weightSet{
								Max:     math.Max(0.1, base.Max+dMax),
								Reason:  math.Max(0.1, base.Reason+dReason),
								Heavy:   math.Max(0.1, base.Heavy+dHeavy),
								Code:    math.Max(0.1, base.Code+dCode),
								Fast:    math.Max(0.1, base.Fast+dFast),
								Trivial: math.Max(0.1, base.Trivial+dTrivial),
								// Keep modifiers at baseline for this sweep.
								QuestionHeavyDamp:   base.QuestionHeavyDamp,
								QuestionCodeDamp:    base.QuestionCodeDamp,
								QuestionReasonBoost: base.QuestionReasonBoost,
								ShortHeavyDamp:      base.ShortHeavyDamp,
								ShortMaxDamp:        base.ShortMaxDamp,
							}
							for _, bypass := range []bool{false, true} {
								experiments++
								correct, total, _ := evalAccuracy(ws, bypass)
								acc := float64(correct) / float64(total) * 100
								if acc > best.accuracy {
									best = result{ws: ws, correct: correct, total: total, accuracy: acc, bypass: bypass}
								}
							}
						}
					}
				}
			}
		}
	}

	t.Logf("SWEEP: %d experiments evaluated", experiments)
	t.Logf("BEST: %d/%d = %.1f%% (bypass=%v)", best.correct, best.total, best.accuracy, best.bypass)
	t.Logf("  Weights: max=%.1f reason=%.1f heavy=%.1f code=%.1f fast=%.1f trivial=%.1f",
		best.ws.Max, best.ws.Reason, best.ws.Heavy, best.ws.Code, best.ws.Fast, best.ws.Trivial)

	// Also show baseline for comparison.
	baseCorrect, baseTotal, _ := evalAccuracy(base, false)
	basePct := float64(baseCorrect) / float64(baseTotal) * 100
	t.Logf("BASELINE: %d/%d = %.1f%%", baseCorrect, baseTotal, basePct)
	t.Logf("IMPROVEMENT: +%.1f%% (%d more correct)", best.accuracy-basePct, best.correct-baseCorrect)
}

// TestAutoResearchModifierSweep sweeps context modifiers with the best weights.
func TestAutoResearchModifierSweep(t *testing.T) {
	type result struct {
		ws       weightSet
		correct  int
		accuracy float64
		bypass   bool
	}

	var best result

	// Use baseline weights, sweep modifiers.
	base := baseline()
	dampSteps := []float64{0.2, 0.3, 0.4, 0.5, 0.6, 0.7, 0.8}
	boostSteps := []float64{1.0, 1.1, 1.2, 1.3, 1.4, 1.5}

	experiments := 0
	for _, qhd := range dampSteps {
		for _, qcd := range dampSteps {
			for _, qrb := range boostSteps {
				for _, shd := range dampSteps {
					for _, smd := range dampSteps {
						ws := base
						ws.QuestionHeavyDamp = qhd
						ws.QuestionCodeDamp = qcd
						ws.QuestionReasonBoost = qrb
						ws.ShortHeavyDamp = shd
						ws.ShortMaxDamp = smd
						for _, bypass := range []bool{false, true} {
							experiments++
							correct, total, _ := evalAccuracy(ws, bypass)
							acc := float64(correct) / float64(total) * 100
							if acc > best.accuracy || (acc == best.accuracy && bypass) {
								best = result{ws: ws, correct: correct, accuracy: acc, bypass: bypass}
								_ = total
							}
						}
					}
				}
			}
		}
	}

	t.Logf("MODIFIER SWEEP: %d experiments evaluated", experiments)
	t.Logf("BEST: %d/%d = %.1f%% (bypass=%v)", best.correct, len(RouterExamples), best.accuracy, best.bypass)
	t.Logf("  QHeavyDamp=%.1f QCodeDamp=%.1f QReasonBoost=%.1f ShortHeavy=%.1f ShortMax=%.1f",
		best.ws.QuestionHeavyDamp, best.ws.QuestionCodeDamp, best.ws.QuestionReasonBoost,
		best.ws.ShortHeavyDamp, best.ws.ShortMaxDamp)
}
