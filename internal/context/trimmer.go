package context

import "sort"

// Section is a chunk of content with a priority for token budget trimming.
type Section struct {
	Content  string
	Priority int // higher = keep longer (1=lowest, 10=highest)
	Label    string
}

// EstimateTokens approximates the token count for a text string.
func EstimateTokens(text string) int {
	return len(text) / 4
}

// TrimToTokenBudget selects sections that fit within the given token budget.
// Sections are sorted by priority (highest first). The last included section
// may be truncated to fit.
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
			// Truncate to fit
			maxChars := remaining * 4
			truncated := s
			if maxChars < len(s.Content) {
				truncated.Content = s.Content[:maxChars]
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
