// Package verify checks AI responses for hallucinated symbols and convention violations.
// After each model response, it scans the output for function/method names
// and flags any that don't exist in the live GROUND_TRUTH index.
// It also checks code blocks against project conventions.
package verify

import (
	"regexp"
	"strings"

	"github.com/seedhire/mantis/internal/truth"
)

// Result holds the outcome of a verification pass.
type Result struct {
	Clean            bool     // true if no unknown symbols found
	UnknownSymbols   []string // symbols referenced but not in GROUND_TRUTH
	Warning          string   // human-readable warning for the REPL
}

// codeBlockRe extracts content inside fenced code blocks.
var codeBlockRe = regexp.MustCompile("```[a-z]*\n([\\s\\S]*?)```")

// funcCallRe matches function/method call patterns like foo(), Bar(), obj.Method().
var funcCallRe = regexp.MustCompile(`\b([A-Za-z_][A-Za-z0-9_]*)\s*\(`)

// stopWords are common English words and keywords that look like calls but aren't symbols.
var stopWords = map[string]bool{
	"if": true, "for": true, "switch": true, "select": true, "return": true,
	"make": true, "len": true, "cap": true, "append": true, "copy": true,
	"delete": true, "panic": true, "recover": true, "new": true, "close": true,
	"range": true, "type": true, "func": true, "var": true, "const": true,
	"import": true, "package": true, "go": true, "defer": true, "map": true,
	"chan": true, "true": true, "false": true, "nil": true, "string": true,
	"int": true, "error": true, "bool": true, "byte": true, "rune": true,
	"float64": true, "float32": true, "print": true, "println": true,
	// Python/TS common builtins
	"super": true, "list": true, "dict": true, "set": true, "str": true,
	"isinstance": true, "console": true, "require": true,
	"describe": true, "expect": true, "test": true,
}

// SuggestCorrections builds a correction string mapping unknown symbols
// to their closest matches in the ground truth index.
// Returns empty string if no close matches found.
func SuggestCorrections(unknownSymbols []string, tw *truth.Writer) string {
	if tw == nil || len(unknownSymbols) == 0 {
		return ""
	}
	var parts []string
	for _, sym := range unknownSymbols {
		closest := tw.FindClosest(sym, 3)
		if len(closest) > 0 {
			parts = append(parts, sym+" → did you mean: "+strings.Join(closest, ", ")+"?")
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "\n")
}

// against the live ground truth index. Only runs if the index has entries.
func Check(response string, tw *truth.Writer) Result {
	if tw == nil || tw.FileCount() == 0 {
		return Result{Clean: true}
	}

	// Extract code blocks for symbol scanning.
	codeBlocks := codeBlockRe.FindAllStringSubmatch(response, -1)
	if len(codeBlocks) == 0 {
		return Result{Clean: true} // no code in response, nothing to verify
	}

	var unknown []string
	seen := map[string]bool{}

	for _, block := range codeBlocks {
		if len(block) < 2 {
			continue
		}
		code := block[1]
		matches := funcCallRe.FindAllStringSubmatch(code, -1)
		for _, m := range matches {
			if len(m) < 2 {
				continue
			}
			sym := m[1]
			if seen[sym] || stopWords[strings.ToLower(sym)] {
				continue
			}
			seen[sym] = true

			// Only flag exported/capitalized symbols as potentially hallucinated.
			// Lowercase symbols are often local variables, not worth flagging.
			if len(sym) == 0 || sym[0] < 'A' || sym[0] > 'Z' {
				continue
			}
			if !tw.SymbolExists(sym) {
				unknown = append(unknown, sym)
			}
		}
	}

	if len(unknown) == 0 {
		return Result{Clean: true}
	}

	warning := "⚠ Unverified symbols in response (not found in your codebase): " +
		strings.Join(unknown, ", ") +
		"\nVerify these exist before using. Run `mantis find <name>` to check."

	return Result{
		Clean:          false,
		UnknownSymbols: unknown,
		Warning:        warning,
	}
}

// ConventionViolation represents a single convention rule violation.
type ConventionViolation struct {
	Rule    string // the convention rule that was violated
	Details string // specific violation details
}

// ConventionResult holds the outcome of convention checking.
type ConventionResult struct {
	Clean      bool
	Violations []ConventionViolation
	Warning    string
}

// Convention represents a parsed convention rule.
type Convention struct {
	Section string // e.g. "Naming", "Architecture", "Testing"
	Rule    string // the rule text
}

// ParseConventions extracts convention rules from CONVENTIONS.md content.
// Rules are lines under ## sections that start with "- " or are non-empty non-header lines.
func ParseConventions(content string) []Convention {
	if strings.TrimSpace(content) == "" {
		return nil
	}

	var conventions []Convention
	section := ""
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "## ") {
			section = strings.TrimPrefix(line, "## ")
			continue
		}
		if line == "" || line == "(not set)" || strings.HasPrefix(line, "#") {
			continue
		}
		rule := strings.TrimPrefix(line, "- ")
		rule = strings.TrimPrefix(rule, "* ")
		if rule != "" && section != "" {
			conventions = append(conventions, Convention{Section: section, Rule: rule})
		}
	}
	return conventions
}

// Naming pattern regexes for common conventions.
var (
	snakeCaseRe = regexp.MustCompile(`^[a-z][a-z0-9]*(_[a-z0-9]+)*$`)
	camelCaseRe = regexp.MustCompile(`^[a-z][a-zA-Z0-9]*$`)
	pascalCaseRe = regexp.MustCompile(`^[A-Z][a-zA-Z0-9]*$`)
	// Matches variable/function declarations in common languages.
	varDeclRe = regexp.MustCompile(`(?:let|const|var|func|def|function)\s+([a-zA-Z_][a-zA-Z0-9_]*)`)
)

// CheckConventions checks extracted code blocks against parsed convention rules.
// Currently checks naming conventions (snake_case, camelCase, PascalCase patterns).
func CheckConventions(response string, conventions []Convention) ConventionResult {
	if len(conventions) == 0 {
		return ConventionResult{Clean: true}
	}

	codeBlocks := codeBlockRe.FindAllStringSubmatch(response, -1)
	if len(codeBlocks) == 0 {
		return ConventionResult{Clean: true}
	}

	var violations []ConventionViolation

	for _, conv := range conventions {
		lower := strings.ToLower(conv.Rule)

		// Check naming conventions.
		if conv.Section == "Naming" || strings.Contains(lower, "case") || strings.Contains(lower, "naming") {
			for _, block := range codeBlocks {
				if len(block) < 2 {
					continue
				}
				code := block[1]
				decls := varDeclRe.FindAllStringSubmatch(code, -1)
				for _, d := range decls {
					if len(d) < 2 {
						continue
					}
					name := d[1]
					if len(name) <= 1 || stopWords[strings.ToLower(name)] {
						continue
					}

					if strings.Contains(lower, "snake_case") && !snakeCaseRe.MatchString(name) && !pascalCaseRe.MatchString(name) {
						// snake_case expected but found mixed case
						if strings.Contains(name, "_") {
							continue // has underscores, likely attempting snake_case
						}
						violations = append(violations, ConventionViolation{
							Rule:    conv.Rule,
							Details: "'" + name + "' should use snake_case",
						})
					}
					if strings.Contains(lower, "camelcase") && !camelCaseRe.MatchString(name) && !pascalCaseRe.MatchString(name) {
						if strings.Contains(name, "_") {
							violations = append(violations, ConventionViolation{
								Rule:    conv.Rule,
								Details: "'" + name + "' should use camelCase (found snake_case)",
							})
						}
					}
				}
			}
		}

		// Check import restrictions.
		isImportRule := strings.Contains(lower, "import") ||
			strings.Contains(lower, "never") ||
			strings.Contains(lower, "don't") ||
			strings.Contains(lower, "must not") ||
			strings.Contains(lower, "cannot") ||
			strings.Contains(lower, "avoid") ||
			strings.Contains(lower, "no imports")
		if (conv.Section == "Architecture" || conv.Section == "Dependencies" || conv.Section == "Imports") && isImportRule {
			for _, block := range codeBlocks {
				if len(block) < 2 {
					continue
				}
				code := block[1]
				// Extract forbidden patterns from rule like "never import from X"
				forbidden := extractForbiddenImport(conv.Rule)
				if forbidden != "" && strings.Contains(code, forbidden) {
					violations = append(violations, ConventionViolation{
						Rule:    conv.Rule,
						Details: "code contains import/reference to '" + forbidden + "'",
					})
				}
			}
		}
	}

	// Deduplicate violations.
	seen := map[string]bool{}
	var unique []ConventionViolation
	for _, v := range violations {
		key := v.Rule + "|" + v.Details
		if !seen[key] {
			seen[key] = true
			unique = append(unique, v)
		}
	}

	if len(unique) == 0 {
		return ConventionResult{Clean: true}
	}

	var warning strings.Builder
	warning.WriteString("⚠ Convention violations in response:\n")
	for _, v := range unique {
		warning.WriteString("  • " + v.Details + " (rule: " + v.Rule + ")\n")
	}

	return ConventionResult{
		Clean:      false,
		Violations: unique,
		Warning:    warning.String(),
	}
}

// extractForbiddenImport pulls a module/package name from convention rules.
// Handles patterns like:
//
//	"never import from payments"   "don't use lodash"
//	"must not import from X"       "X may not import Y"
//	"avoid importing X"            "no imports from X"
func extractForbiddenImport(rule string) string {
	lower := strings.ToLower(rule)
	markers := []string{
		"never import from ",
		"don't import from ",
		"do not import from ",
		"do not import ",
		"never use ",
		"must not import from ",
		"must not import ",
		"cannot import from ",
		"cannot import ",
		"should not import ",
		"avoid importing ",
		"no imports from ",
		"not import from ",
	}
	for _, marker := range markers {
		if idx := strings.Index(lower, marker); idx != -1 {
			rest := strings.TrimSpace(rule[idx+len(marker):])
			parts := strings.Fields(rest)
			if len(parts) > 0 {
				return strings.Trim(parts[0], "'\"`,.")
			}
		}
	}
	return ""
}
