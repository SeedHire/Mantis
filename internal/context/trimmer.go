package context

import (
	"sort"
	"strings"
)

// Section is a chunk of content with a priority for token budget trimming.
type Section struct {
	Content  string
	Priority int // higher = keep longer (1=lowest, 10=highest)
	Label    string
}

// EstimateTokens approximates the token count for a text string.
// LLaMA-based tokenizers average ~3.5 chars/token for code.
func EstimateTokens(text string) int {
	return int(float64(len(text)) / 3.5)
}

// TrimToTokenBudget selects sections that fit within the given token budget.
// Sections are sorted by priority (highest first). The last included section
// is truncated at function/block boundaries to avoid mid-statement cuts.
func TrimToTokenBudget(sections []Section, budget int) []Section {
	sorted := make([]Section, len(sections))
	copy(sorted, sections)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].Priority > sorted[j].Priority
	})

	var result []Section
	remaining := budget

	for _, s := range sorted {
		tokens := EstimateTokens(s.Content)
		if tokens <= remaining {
			result = append(result, s)
			remaining -= tokens
		} else if remaining > 0 {
			maxChars := int(float64(remaining) * 3.5)
			truncated := s
			if maxChars < len(s.Content) {
				truncated.Content = truncateAtBoundary(s.Content, maxChars)
			}
			result = append(result, truncated)
			remaining = 0
			break
		} else {
			break
		}
	}

	return result
}

// truncateAtBoundary cuts content at a function or block boundary
// rather than mid-statement. Looks for common boundary markers
// (func, class, def, function) before the maxChars limit.
func truncateAtBoundary(content string, maxChars int) string {
	if maxChars >= len(content) {
		return content
	}

	// Search backwards from maxChars for a clean boundary.
	chunk := content[:maxChars]

	// Try to find function/class boundaries (blank line before func/class/def).
	markers := []string{"\nfunc ", "\nclass ", "\ndef ", "\nfunction ", "\nexport ", "\ntype "}
	bestCut := -1
	for _, m := range markers {
		if idx := strings.LastIndex(chunk, m); idx > bestCut {
			bestCut = idx
		}
	}

	// If we found a good boundary and it's not too far back (>50% of content), use it.
	if bestCut > maxChars/2 {
		return content[:bestCut] + "\n// … truncated"
	}

	// Fall back to last complete line.
	if idx := strings.LastIndex(chunk, "\n"); idx > 0 {
		return content[:idx] + "\n// … truncated"
	}

	return chunk
}
