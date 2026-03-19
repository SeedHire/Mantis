package pipeline

import "strings"

// codeTemperature returns a task-adaptive temperature for code generation.
// Fix/debug tasks get lower temperature for deterministic output,
// refactoring gets slightly higher, and creative implementation gets the most.
func codeTemperature(taskText string) float64 {
	lower := strings.ToLower(taskText)

	// Fix/debug: most deterministic.
	fixKeywords := []string{"fix", "bug", "error", "broken", "failing", "crash", "panic", "undefined", "nil pointer", "segfault"}
	for _, kw := range fixKeywords {
		if strings.Contains(lower, kw) {
			return 0.05
		}
	}

	// Refactor/restructure: low creativity needed.
	refactorKeywords := []string{"refactor", "rename", "move", "restructure", "reorganize", "extract", "inline", "cleanup", "clean up"}
	for _, kw := range refactorKeywords {
		if strings.Contains(lower, kw) {
			return 0.08
		}
	}

	// Default: implement/build/create — moderate creativity.
	return 0.12
}
