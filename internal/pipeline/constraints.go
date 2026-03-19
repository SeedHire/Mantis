package pipeline

import (
	"strings"
)

// extractConstraints scans the user's request for explicit constraints like
// "don't use X", "only use Y", "no frameworks", "must use Z", etc.
// Returns a formatted block suitable for injection into prompts.
// Empty string if no constraints found.
func extractConstraints(request string) string {
	lower := strings.ToLower(request)
	var constraints []string

	// Patterns: "don't use X", "do not use X", "no X", "only use X",
	// "must use X", "without X", "avoid X", "never use X", "prefer X"
	patterns := []struct {
		marker string
		prefix string // what to prepend in the constraint
	}{
		{"don't use ", "DO NOT use "},
		{"do not use ", "DO NOT use "},
		{"dont use ", "DO NOT use "},
		{"never use ", "NEVER use "},
		{"avoid ", "AVOID "},
		{"no frameworks", "NO frameworks — "},
		{"no external", "NO external dependencies — "},
		{"no third-party", "NO third-party packages — "},
		{"no third party", "NO third-party packages — "},
		{"only use ", "ONLY use "},
		{"must use ", "MUST use "},
		{"use only ", "ONLY use "},
		{"without ", "WITHOUT "},
		{"prefer ", "PREFER "},
		{"stdlib only", "Use STANDARD LIBRARY ONLY — "},
		{"standard library only", "Use STANDARD LIBRARY ONLY — "},
	}

	for _, p := range patterns {
		idx := strings.Index(lower, p.marker)
		if idx < 0 {
			continue
		}
		// Extract the rest of the phrase up to punctuation or end.
		rest := request[idx+len(p.marker):]
		end := findConstraintEnd(rest)
		value := strings.TrimSpace(rest[:end])
		if value == "" {
			// Whole-phrase markers (e.g. "no frameworks", "stdlib only")
			// don't have a value after — use the prefix as-is.
			if strings.HasSuffix(p.prefix, " — ") || strings.HasSuffix(p.prefix, "ONLY — ") {
				constraints = append(constraints, strings.TrimSuffix(p.prefix, " — "))
				continue
			}
			continue
		}
		constraints = append(constraints, p.prefix+value)
	}

	if len(constraints) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("\n### User Constraints (MUST follow):\n")
	for _, c := range constraints {
		b.WriteString("- ")
		b.WriteString(c)
		b.WriteByte('\n')
	}
	return b.String()
}

// findConstraintEnd finds the end of a constraint phrase.
// Stops at sentence-ending punctuation or newline.
func findConstraintEnd(s string) int {
	for i, c := range s {
		if c == '.' || c == ',' || c == '\n' || c == ';' || c == '!' {
			return i
		}
	}
	return len(s)
}
