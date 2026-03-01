// Package verify checks AI responses for hallucinated symbols.
// After each model response, it scans the output for function/method names
// and flags any that don't exist in the live GROUND_TRUTH index.
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

// Check scans the AI response for symbol references and validates them
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
