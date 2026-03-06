package verify

import (
	"context"
	"regexp"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	golang "github.com/smacker/go-tree-sitter/golang"
	"github.com/smacker/go-tree-sitter/javascript"
	"github.com/smacker/go-tree-sitter/python"
	"github.com/smacker/go-tree-sitter/typescript/typescript"
)

// CodeBlockInfo holds extracted information from a parsed code block.
type CodeBlockInfo struct {
	Language     string
	FilePath     string
	FuncCalls    []string // function/method calls found
	Imports      []string // import paths found
	SyntaxErrors []SyntaxError
}

// SyntaxError represents a parse error in a code block.
type SyntaxError struct {
	Line    int
	Column  int
	Message string
}

// langFenceRe matches the language tag from a code fence: ```go or ```typescript:path/file.ts
var langFenceRe = regexp.MustCompile("^```([a-zA-Z]+)(?::([^\n`]+))?")

// ExtractCodeBlocks parses all fenced code blocks in the response using
// tree-sitter where possible, falling back to regex for unsupported languages.
func ExtractCodeBlocks(response string) []CodeBlockInfo {
	var blocks []CodeBlockInfo

	// Split on code fences
	lines := strings.Split(response, "\n")
	var inBlock bool
	var lang, filePath string
	var blockLines []string

	for _, line := range lines {
		if !inBlock {
			if m := langFenceRe.FindStringSubmatch(line); len(m) >= 2 {
				inBlock = true
				lang = strings.ToLower(m[1])
				if len(m) >= 3 {
					filePath = strings.TrimSpace(m[2])
				}
				blockLines = nil
			}
			continue
		}
		if strings.HasPrefix(line, "```") {
			// End of block
			code := strings.Join(blockLines, "\n")
			info := parseCodeBlock(lang, filePath, code)
			blocks = append(blocks, info)
			inBlock = false
			lang = ""
			filePath = ""
			blockLines = nil
			continue
		}
		blockLines = append(blockLines, line)
	}
	return blocks
}

// parseCodeBlock uses tree-sitter to parse a code block and extract symbols.
func parseCodeBlock(lang, filePath, code string) CodeBlockInfo {
	info := CodeBlockInfo{
		Language: lang,
		FilePath: filePath,
	}

	tsLang := languageForFence(lang)
	if tsLang == nil {
		// Unsupported language — fall back to regex extraction
		info.FuncCalls = regexExtractCalls(code)
		return info
	}

	parser := sitter.NewParser()
	parser.SetLanguage(tsLang)

	tree, err := parser.ParseCtx(context.Background(), nil, []byte(code))
	if err != nil {
		info.FuncCalls = regexExtractCalls(code)
		return info
	}
	defer tree.Close()

	root := tree.RootNode()

	// Collect syntax errors
	collectErrors(root, code, &info)

	// Extract function calls and imports based on language
	switch lang {
	case "go", "golang":
		extractGoSymbols(root, []byte(code), &info)
	case "typescript", "ts", "javascript", "js", "jsx", "tsx":
		extractJSSymbols(root, []byte(code), &info)
	case "python", "py":
		extractPythonSymbols(root, []byte(code), &info)
	default:
		info.FuncCalls = regexExtractCalls(code)
	}

	return info
}

// languageForFence returns the tree-sitter language for a fence tag.
func languageForFence(lang string) *sitter.Language {
	switch lang {
	case "go", "golang":
		return golang.GetLanguage()
	case "typescript", "ts", "tsx":
		return typescript.GetLanguage()
	case "javascript", "js", "jsx":
		return javascript.GetLanguage()
	case "python", "py":
		return python.GetLanguage()
	default:
		return nil
	}
}

// collectErrors walks the tree and collects ERROR/MISSING nodes.
func collectErrors(root *sitter.Node, code string, info *CodeBlockInfo) {
	walkNode(root, func(n *sitter.Node) {
		if n.IsError() || n.IsMissing() {
			line := int(n.StartPoint().Row) + 1
			col := int(n.StartPoint().Column) + 1
			snippet := nodeText(n, []byte(code))
			if len(snippet) > 40 {
				snippet = snippet[:40] + "..."
			}
			msg := "syntax error"
			if n.IsMissing() {
				msg = "missing expected token"
			}
			if snippet != "" {
				msg += " near: " + snippet
			}
			info.SyntaxErrors = append(info.SyntaxErrors, SyntaxError{
				Line: line, Column: col, Message: msg,
			})
		}
	})
}

// ── Go extraction ───────────────────────────────────────────────────────────

func extractGoSymbols(root *sitter.Node, code []byte, info *CodeBlockInfo) {
	seen := map[string]bool{}
	walkNode(root, func(n *sitter.Node) {
		switch n.Type() {
		case "call_expression":
			funcNode := n.ChildByFieldName("function")
			if funcNode != nil {
				name := extractCallName(funcNode, code)
				if name != "" && !seen[name] {
					seen[name] = true
					info.FuncCalls = append(info.FuncCalls, name)
				}
			}
		case "import_spec":
			pathNode := n.ChildByFieldName("path")
			if pathNode != nil {
				raw := strings.Trim(nodeText(pathNode, code), "\"")
				info.Imports = append(info.Imports, raw)
			}
		}
	})
}

// ── JS/TS extraction ────────────────────────────────────────────────────────

func extractJSSymbols(root *sitter.Node, code []byte, info *CodeBlockInfo) {
	seen := map[string]bool{}
	walkNode(root, func(n *sitter.Node) {
		switch n.Type() {
		case "call_expression":
			funcNode := n.ChildByFieldName("function")
			if funcNode != nil {
				name := extractCallName(funcNode, code)
				if name != "" && !seen[name] {
					seen[name] = true
					info.FuncCalls = append(info.FuncCalls, name)
				}
			}
		case "import_statement":
			src := n.ChildByFieldName("source")
			if src != nil {
				raw := strings.Trim(nodeText(src, code), "\"'")
				info.Imports = append(info.Imports, raw)
			}
		}
	})
}

// ── Python extraction ───────────────────────────────────────────────────────

func extractPythonSymbols(root *sitter.Node, code []byte, info *CodeBlockInfo) {
	seen := map[string]bool{}
	walkNode(root, func(n *sitter.Node) {
		switch n.Type() {
		case "call":
			funcNode := n.ChildByFieldName("function")
			if funcNode != nil {
				name := extractCallName(funcNode, code)
				if name != "" && !seen[name] {
					seen[name] = true
					info.FuncCalls = append(info.FuncCalls, name)
				}
			}
		case "import_statement", "import_from_statement":
			text := nodeText(n, code)
			info.Imports = append(info.Imports, text)
		}
	})
}

// ── Shared helpers ──────────────────────────────────────────────────────────

// extractCallName gets the function name from a call expression's function node.
// Handles: foo, Foo, pkg.Func, obj.Method, a.b.c
func extractCallName(funcNode *sitter.Node, code []byte) string {
	switch funcNode.Type() {
	case "identifier":
		return nodeText(funcNode, code)
	case "selector_expression", "member_expression":
		// Go: selector_expression, JS/TS: member_expression
		field := funcNode.ChildByFieldName("field")
		if field == nil {
			// Try "property" for JS
			field = funcNode.ChildByFieldName("property")
		}
		if field != nil {
			return nodeText(field, code)
		}
		return nodeText(funcNode, code)
	case "attribute":
		// Python: attribute access
		attr := funcNode.ChildByFieldName("attribute")
		if attr != nil {
			return nodeText(attr, code)
		}
	}
	return ""
}

func nodeText(n *sitter.Node, code []byte) string {
	start := n.StartByte()
	end := n.EndByte()
	if int(end) > len(code) {
		end = uint32(len(code))
	}
	return string(code[start:end])
}

func walkNode(node *sitter.Node, fn func(*sitter.Node)) {
	fn(node)
	for i := 0; i < int(node.ChildCount()); i++ {
		walkNode(node.Child(i), fn)
	}
}

// regexExtractCalls is the fallback for unsupported languages.
func regexExtractCalls(code string) []string {
	matches := funcCallRe.FindAllStringSubmatch(code, -1)
	seen := map[string]bool{}
	var calls []string
	for _, m := range matches {
		if len(m) < 2 {
			continue
		}
		name := m[1]
		if !seen[name] && !stopWords[strings.ToLower(name)] {
			seen[name] = true
			calls = append(calls, name)
		}
	}
	return calls
}

// CheckWithAST performs the same check as Check but uses tree-sitter AST parsing
// for more accurate symbol extraction. Falls back to regex for unsupported languages.
func CheckWithAST(response string, tw interface {
	SymbolExists(string) bool
	FileCount() int
}) Result {
	if tw == nil || tw.FileCount() == 0 {
		return Result{Clean: true}
	}

	blocks := ExtractCodeBlocks(response)
	if len(blocks) == 0 {
		return Result{Clean: true}
	}

	var unknown []string
	seen := map[string]bool{}

	for _, block := range blocks {
		for _, call := range block.FuncCalls {
			if seen[call] || stopWords[strings.ToLower(call)] {
				continue
			}
			seen[call] = true

			// Only flag exported/capitalized symbols
			if len(call) == 0 || call[0] < 'A' || call[0] > 'Z' {
				continue
			}
			if !tw.SymbolExists(call) {
				unknown = append(unknown, call)
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

// DetectSyntaxErrors checks all code blocks for syntax errors via tree-sitter.
func DetectSyntaxErrors(response string) []SyntaxError {
	blocks := ExtractCodeBlocks(response)
	var allErrors []SyntaxError
	for _, block := range blocks {
		for i := range block.SyntaxErrors {
			e := block.SyntaxErrors[i]
			if block.FilePath != "" {
				e.Message = block.FilePath + ":" + e.Message
			}
			allErrors = append(allErrors, e)
		}
	}
	return allErrors
}
