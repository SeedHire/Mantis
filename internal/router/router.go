// Package router classifies user intent and selects the right specialised model.
//
// Tiers (7):
//   TierTrivial  — one-liners, definitions, syntax lookups          (~1–4B model)
//   TierFast     — short code questions, small completions          (~8–14B model)
//   TierCode     — coding specialist: implement, debug, refactor    (devstral / qwen3-coder)
//   TierReason   — analysis, architecture, deep explanation         (kimi-thinking / cogito)
//   TierHeavy    — multi-file, large context, complex design        (devstral-2 / deepseek-v3)
//   TierMax      — ensemble: 3 specialists in parallel + synthesis  (Opus-level output)
//   TierVision   — any image / screenshot input                     (qwen3-vl / gemini)
package router

import (
"strings"

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
return [...]string{"trivial", "fast", "code", "reason", "heavy", "max", "vision"}[t]
}

// Intent holds the routing decision for a user message.
type Intent struct {
Tier        Tier
TaskType    string // the detected sub-type
NeedsGraph  bool
NeedsVision bool
Confidence  float64
}

// ── Model preference lists ────────────────────────────────────────────────────
// Each tier lists models in priority order (cloud first, local fallback).
var preferredModels = map[Tier][]string{
TierTrivial: {
"gemma3:4b", "ministral-3:3b", "gemma3:1b", "rnj-1:8b",
// local
"qwen2.5-coder:1.5b", "qwen2.5-coder:0.5b", "llama3.2:1b", "phi3:mini", "gemma2:2b",
},
TierFast: {
"gemma3:12b", "ministral-3:8b", "gpt-oss:20b", "nemotron-3-nano:30b",
// local
"qwen2.5-coder:7b", "llama3.2:3b", "phi3:3.8b", "gemma2:9b",
},
TierCode: {
// Coding-specialist models — best for implement/debug/refactor
"devstral-small-2:24b", "qwen3-coder-next", "ministral-3:14b",
"gpt-oss:120b",
// local
"qwen2.5-coder:14b", "qwen2.5-coder:32b", "deepseek-coder:6.7b",
"codellama:13b", "deepseek-coder-v2:16b",
},
TierReason: {
// Reasoning/analysis models — best for architecture, explanation, tradeoffs
"kimi-k2-thinking", "cogito-2.1:671b", "deepseek-v3.2",
"glm-5", "minimax-m2.1", "qwen3-next:80b",
// local
"llama3.1:70b", "mixtral:8x7b", "llama3.3:70b",
},
TierHeavy: {
// Largest general + coding models for hard multi-file tasks
"devstral-2:123b", "qwen3-coder:480b", "deepseek-v3.1:671b",
"kimi-k2.5", "mistral-large-3:675b", "minimax-m2.5",
"gemma3:27b", "glm-4.7", "qwen3.5:397b",
// local
"qwen2.5-coder:72b", "llama3.3:70b",
},
// TierMax uses ensemblePools — see EnsembleModels()
TierMax: {
"devstral-2:123b", "deepseek-v3.2", "kimi-k2-thinking",
"qwen3-coder:480b", "cogito-2.1:671b", "mistral-large-3:675b",
"devstral-small-2:24b", "qwen3-coder-next", "glm-5",
},
TierVision: {
"qwen3-vl:235b-instruct", "qwen3-vl:235b", "gemini-3-flash-preview",
// local
"llama3.2-vision:11b", "llava:13b", "llava:7b", "moondream:1.8b",
},
}

// ensemblePools: one model per pool is selected for parallel ensemble execution.
// Pool 1 = coding specialist, Pool 2 = reasoning, Pool 3 = large general.
var ensemblePools = [][]string{
{"devstral-small-2:24b", "qwen3-coder-next", "devstral-2:123b", "qwen3-coder:480b", "qwen2.5-coder:32b"},
{"kimi-k2-thinking", "cogito-2.1:671b", "deepseek-v3.2", "deepseek-v3.1:671b", "llama3.3:70b"},
{"mistral-large-3:675b", "minimax-m2.5", "kimi-k2.5", "glm-5", "gemma3:27b", "llama3.1:70b"},
}

var defaultModels = map[Tier]string{
TierTrivial: "gemma3:4b",
TierFast:    "gemma3:12b",
TierCode:    "devstral-small-2:24b",
TierReason:  "kimi-k2-thinking",
TierHeavy:   "devstral-2:123b",
TierMax:     "devstral-2:123b", // single-model fallback when ensemble unavailable
TierVision:  "qwen3-vl:235b-instruct",
}

// ── Size-based fallback thresholds ────────────────────────────────────────────
const (
gb           = int64(1_000_000_000)
trivialMax   = 5 * gb
fastMax      = 15 * gb
codeMax      = 40 * gb
reasonMax    = 100 * gb
// heavy / max = anything larger
)

var resolvedModels = map[Tier]string{}

// ResolveAll picks the best available model for every tier from a live model list.
func ResolveAll(available []ollama.ModelInfo) {
set := buildSet(available)
allTiers := []Tier{TierTrivial, TierFast, TierCode, TierReason, TierHeavy, TierMax, TierVision}
for _, tier := range allTiers {
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
		if m := pickQuantized(buckets[0]); m != "" { return m }
		if m := pick(buckets[0], false); m != "" { return m }
		return pick(buckets[1], false)
	case TierFast:
		if m := pickQuantized(buckets[1]); m != "" { return m }
		if m := pick(buckets[1], true); m != "" { return m }
		return pick(buckets[0], true)
	case TierCode:
		if m := pick(buckets[2], true); m != "" { return m }
		return pick(buckets[1], true)
	case TierReason:
		if m := pick(buckets[3], true); m != "" { return m }
		return pick(buckets[2], true)
	case TierHeavy, TierMax:
		if m := pick(buckets[4], true); m != "" { return m }
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
if m, ok := resolvedModels[tier]; ok && m != "" {
return m
}
return defaultModels[tier]
}

func PreferredModels(tier Tier) []string { return preferredModels[tier] }
func SetResolved(tier Tier, model string) { resolvedModels[tier] = model }

// ResolvedSummary returns a human-readable mapping of tier → resolved model.
func ResolvedSummary() map[string]string {
	summary := make(map[string]string)
	for _, tier := range []Tier{TierTrivial, TierFast, TierCode, TierReason, TierHeavy, TierMax, TierVision} {
		summary[tier.String()] = ModelFor(tier)
	}
	return summary
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
func Classify(message string, hasImage bool) Intent {
if hasImage {
return Intent{Tier: TierVision, TaskType: "vision", NeedsVision: true, Confidence: 1.0}
}
lower := strings.ToLower(message)

// Terminal error paste — always a fast fix regardless of phrasing.
for _, sig := range terminalErrorSignatures {
if strings.Contains(lower, sig) {
return Intent{Tier: TierFast, TaskType: "fix", NeedsGraph: false, Confidence: 0.90}
}
}

// Max/ensemble — runs before all others.
for _, kw := range maxKeywords {
if strings.Contains(lower, kw) {
return Intent{Tier: TierMax, TaskType: "ensemble", NeedsGraph: needsGraph(lower), Confidence: 0.92}
}
}
// Reasoning tier — architecture, tradeoffs, deep explanation.
for _, kw := range reasonKeywords {
if strings.Contains(lower, kw) {
return Intent{Tier: TierReason, TaskType: "reason", NeedsGraph: needsGraph(lower), Confidence: 0.88}
}
}
// Heavy tier — multi-file, system-wide, large rewrite.
for _, kw := range heavyKeywords {
if strings.Contains(lower, kw) {
return Intent{Tier: TierHeavy, TaskType: "design", NeedsGraph: needsGraph(lower), Confidence: 0.85}
}
}
// Code tier — implement, debug, refactor specific code.
for _, kw := range codeKeywords {
if strings.Contains(lower, kw) {
return Intent{Tier: TierCode, TaskType: detectTaskType(lower), NeedsGraph: needsGraph(lower), Confidence: 0.82}
}
}
// Fast tier — short focused questions.
for _, kw := range fastKeywords {
if strings.Contains(lower, kw) {
return Intent{Tier: TierFast, TaskType: detectTaskType(lower), NeedsGraph: needsGraph(lower), Confidence: 0.78}
}
}
// Trivial tier — one-word lookups, syntax, spelling.
for _, kw := range trivialKeywords {
if strings.Contains(lower, kw) {
return Intent{Tier: TierTrivial, TaskType: "trivial", NeedsGraph: false, Confidence: 0.80}
}
}
// Default: code tier with low confidence — task template will still guide quality.
return Intent{Tier: TierCode, TaskType: detectTaskType(lower), NeedsGraph: needsGraph(lower), Confidence: 0.60}
}

var maxKeywords = []string{
"find all bug", "find bugs", "audit", "code review", "review all",
"best approach", "compare approach", "which is better", "pros and cons",
"comprehensive", "thorough", "deep dive", "full analysis",
"all issues", "all problem", "security", "vulnerability",
"production ready", "production-ready", "before deploy", "before release",
"should i use", "tradeoff",
}

var reasonKeywords = []string{
"why does", "explain how", "how does", "how do i decide",
"architecture", "design pattern", "trade-off", "when to use",
"difference between", "what is the best way", "what approach",
"reasoning", "analyse", "analyze", "pros", "cons",
}

var heavyKeywords = []string{
"refactor entire", "refactor all", "refactor the whole", "rewrite",
"migrate", "explain the whole", "explain all", "codebase",
"multi-file", "across all", "every file", "full project", "from scratch",
"plan the", "strategy for",
// Multi-file project builds
"build a", "build an", "build me", "create a full", "create an app",
"clone", "scaffold", "full stack", "full-stack", "entire app",
"rest api", "graphql api", "microservice", "backend for",
"set up a", "setup project", "initialize project",
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
"how to use", "example of", "show me", "snippet",
}

var trivialKeywords = []string{
"what is", "what's", "define", "meaning of", "syntax for",
"typo", "spelling", "rename", "one line", "single line",
"autocomplete", "complete this",
}

// terminalErrorSignatures — raw error output pasted from the shell.
// normalizeTerminalInput() rewrites these as "fix this X error:" first,
// then Classify() routes them to fast/fix regardless of phrasing.
var terminalErrorSignatures = []string{
"command not found",
"npm err!",
"npm warn",
"error ts",        // TypeScript compiler
"error[e",         // Rust compiler
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
