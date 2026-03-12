package parser

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/typescript/typescript"
)

// TypeScriptParser parses TypeScript/JavaScript files.
type TypeScriptParser struct{}

func (p *TypeScriptParser) Language() string { return "typescript" }

func (p *TypeScriptParser) Extensions() []string {
	return []string{".ts", ".tsx", ".js", ".mjs"}
}

// ParseFile parses a TypeScript/JavaScript file and extracts symbols and imports.
func (p *TypeScriptParser) ParseFile(path string, content []byte) (*ParseResult, error) {
	parser := sitter.NewParser()
	defer parser.Close()
	parser.SetLanguage(typescript.GetLanguage())

	tree, err := parser.ParseCtx(context.Background(), nil, content)
	if err != nil {
		// Return partial result with just the file node
		return &ParseResult{FileNode: fileSymbolNode(path, "typescript", len(strings.Split(string(content), "\n")))}, nil
	}
	defer tree.Close()

	root := tree.RootNode()
	lineCount := countLines(content)

	result := &ParseResult{
		FileNode: fileSymbolNode(path, "typescript", lineCount),
	}

	walkAST(root, func(node *sitter.Node) {
		switch node.Type() {
		case "import_statement":
			src := node.ChildByFieldName("source")
			if src == nil {
				return
			}
			raw := StripQuotes(string(content[src.StartByte():src.EndByte()]))
			if IsExternalImport(raw) {
				return
			}
			resolved := resolveImportPath(path, raw)
			result.Imports = append(result.Imports, &ImportEdge{
				FromFile: path,
				ToFile:   resolved,
				RawPath:  raw,
			})

		case "function_declaration":
			nameNode := node.ChildByFieldName("name")
			if nameNode == nil {
				return
			}
			name := string(content[nameNode.StartByte():nameNode.EndByte()])
			sym := &SymbolNode{
				ID:        SymbolNodeID(path, name),
				Type:      "function",
				Name:      name,
				FilePath:  path,
				LineStart: int(node.StartPoint().Row) + 1,
				LineEnd:   int(node.EndPoint().Row) + 1,
				Exported:  isNodeExported(node),
				Language:  "typescript",
			}
			result.Symbols = append(result.Symbols, sym)

		case "method_definition":
			nameNode := node.ChildByFieldName("name")
			if nameNode == nil {
				return
			}
			name := string(content[nameNode.StartByte():nameNode.EndByte()])
			sym := &SymbolNode{
				ID:        SymbolNodeID(path, name),
				Type:      "method",
				Name:      name,
				FilePath:  path,
				LineStart: int(node.StartPoint().Row) + 1,
				LineEnd:   int(node.EndPoint().Row) + 1,
				Exported:  isNodeExported(node),
				Language:  "typescript",
			}
			result.Symbols = append(result.Symbols, sym)

		case "class_declaration":
			nameNode := node.ChildByFieldName("name")
			if nameNode == nil {
				return
			}
			name := string(content[nameNode.StartByte():nameNode.EndByte()])
			sym := &SymbolNode{
				ID:        SymbolNodeID(path, name),
				Type:      "class",
				Name:      name,
				FilePath:  path,
				LineStart: int(node.StartPoint().Row) + 1,
				LineEnd:   int(node.EndPoint().Row) + 1,
				Exported:  isNodeExported(node),
				Language:  "typescript",
			}
			result.Symbols = append(result.Symbols, sym)

		case "interface_declaration":
			nameNode := node.ChildByFieldName("name")
			if nameNode == nil {
				return
			}
			name := string(content[nameNode.StartByte():nameNode.EndByte()])
			sym := &SymbolNode{
				ID:        SymbolNodeID(path, name),
				Type:      "interface",
				Name:      name,
				FilePath:  path,
				LineStart: int(node.StartPoint().Row) + 1,
				LineEnd:   int(node.EndPoint().Row) + 1,
				Exported:  isNodeExported(node),
				Language:  "typescript",
			}
			result.Symbols = append(result.Symbols, sym)

		case "type_alias_declaration":
			nameNode := node.ChildByFieldName("name")
			if nameNode == nil {
				return
			}
			name := string(content[nameNode.StartByte():nameNode.EndByte()])
			sym := &SymbolNode{
				ID:        SymbolNodeID(path, name),
				Type:      "type_alias",
				Name:      name,
				FilePath:  path,
				LineStart: int(node.StartPoint().Row) + 1,
				LineEnd:   int(node.EndPoint().Row) + 1,
				Exported:  isNodeExported(node),
				Language:  "typescript",
			}
			result.Symbols = append(result.Symbols, sym)
		}
	})

	return result, nil
}

// isNodeExported checks if a node is directly under an export_statement.
func isNodeExported(node *sitter.Node) bool {
	parent := node.Parent()
	if parent == nil {
		return false
	}
	if parent.Type() == "export_statement" {
		return true
	}
	// Check for export keyword as a child
	for i := 0; i < int(node.ChildCount()); i++ {
		if node.Child(i).Type() == "export" {
			return true
		}
	}
	return false
}

// walkAST performs a depth-first traversal of the AST.
func walkAST(node *sitter.Node, fn func(*sitter.Node)) {
	if node == nil {
		return
	}
	fn(node)
	for i := 0; i < int(node.ChildCount()); i++ {
		walkAST(node.Child(i), fn)
	}
}

// resolveImportPath resolves a relative import path to an absolute file path.
func resolveImportPath(fromFile, importPath string) string {
	if !strings.HasPrefix(importPath, ".") {
		return importPath
	}
	dir := filepath.Dir(fromFile)
	base := filepath.Join(dir, importPath)

	// If already has extension and file exists
	if ext := filepath.Ext(base); ext != "" {
		if _, err := os.Stat(base); err == nil {
			return base
		}
	}

	// Try common extensions
	for _, ext := range []string{".ts", ".tsx", ".js", "/index.ts", "/index.js"} {
		candidate := base + ext
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}

	// Return with .ts as default if nothing found
	return base + ".ts"
}

func fileSymbolNode(path, lang string, lineCount int) *SymbolNode {
	return &SymbolNode{
		ID:        FileNodeID(path),
		Type:      "file",
		Name:      filepath.Base(path),
		FilePath:  path,
		LineStart: 1,
		LineEnd:   lineCount,
		Exported:  true,
		Language:  lang,
	}
}

func countLines(content []byte) int {
	count := 1
	for _, b := range content {
		if b == '\n' {
			count++
		}
	}
	return count
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
