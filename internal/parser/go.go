package parser

import (
	"context"
	"path/filepath"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	golang "github.com/smacker/go-tree-sitter/golang"
)

// GoParser parses Go source files using tree-sitter.
type GoParser struct {
	Root string
}

func (p *GoParser) Language() string     { return "go" }
func (p *GoParser) Extensions() []string { return []string{".go"} }

func (p *GoParser) ParseFile(path string, content []byte) (*ParseResult, error) {
	parser := sitter.NewParser()
	parser.SetLanguage(golang.GetLanguage())

	tree, err := parser.ParseCtx(context.Background(), nil, content)
	if err != nil {
		return &ParseResult{FileNode: fileSymbolNode(path, "go", countLines(content))}, nil
	}
	defer tree.Close()

	root := tree.RootNode()
	result := &ParseResult{
		FileNode: fileSymbolNode(path, "go", countLines(content)),
	}

	walkAST(root, func(node *sitter.Node) {
		switch node.Type() {

		case "import_declaration":
			// import "pkg" or import ( "pkg1" \n "pkg2" )
			for i := 0; i < int(node.ChildCount()); i++ {
				child := node.Child(i)
				if child.Type() == "import_spec_list" {
					for j := 0; j < int(child.ChildCount()); j++ {
						spec := child.Child(j)
						if spec.Type() == "import_spec" {
							pathNode := spec.ChildByFieldName("path")
							if pathNode != nil {
								raw := StripQuotes(string(content[pathNode.StartByte():pathNode.EndByte()]))
								resolved := resolveGoImport(path, raw, p.Root)
								if resolved != "" {
									result.Imports = append(result.Imports, &ImportEdge{
										FromFile: path,
										ToFile:   resolved,
										RawPath:  raw,
									})
								}
							}
						}
					}
				} else if child.Type() == "import_spec" {
					pathNode := child.ChildByFieldName("path")
					if pathNode != nil {
						raw := StripQuotes(string(content[pathNode.StartByte():pathNode.EndByte()]))
						resolved := resolveGoImport(path, raw, p.Root)
						if resolved != "" {
							result.Imports = append(result.Imports, &ImportEdge{
								FromFile: path,
								ToFile:   resolved,
								RawPath:  raw,
							})
						}
					}
				}
			}

		case "function_declaration":
			nameNode := node.ChildByFieldName("name")
			if nameNode == nil {
				return
			}
			name := string(content[nameNode.StartByte():nameNode.EndByte()])
			exported := len(name) > 0 && name[0] >= 'A' && name[0] <= 'Z'

			// Extract parameter + return signature text for ground truth.
			params := extractGoParams(node, content)
			returns := extractGoReturns(node, content)

			sym := &SymbolNode{
				ID:        SymbolNodeID(path, name),
				Type:      "function",
				Name:      name,
				FilePath:  path,
				LineStart: int(node.StartPoint().Row) + 1,
				LineEnd:   int(node.EndPoint().Row) + 1,
				Exported:  exported,
				Language:  "go",
			}
			// Stash sig in name field temporarily as "Name|params|returns" for truth engine.
			sym.ID = SymbolNodeID(path, name) + "|" + params + "|" + returns
			result.Symbols = append(result.Symbols, sym)

		case "method_declaration":
			nameNode := node.ChildByFieldName("name")
			if nameNode == nil {
				return
			}
			name := string(content[nameNode.StartByte():nameNode.EndByte()])
			exported := len(name) > 0 && name[0] >= 'A' && name[0] <= 'Z'

			params := extractGoParams(node, content)
			returns := extractGoReturns(node, content)

			sym := &SymbolNode{
				ID:        SymbolNodeID(path, name) + "|" + params + "|" + returns,
				Type:      "method",
				Name:      name,
				FilePath:  path,
				LineStart: int(node.StartPoint().Row) + 1,
				LineEnd:   int(node.EndPoint().Row) + 1,
				Exported:  exported,
				Language:  "go",
			}
			result.Symbols = append(result.Symbols, sym)

		case "type_declaration":
			for i := 0; i < int(node.ChildCount()); i++ {
				spec := node.Child(i)
				if spec.Type() != "type_spec" {
					continue
				}
				nameNode := spec.ChildByFieldName("name")
				if nameNode == nil {
					continue
				}
				name := string(content[nameNode.StartByte():nameNode.EndByte()])
				exported := len(name) > 0 && name[0] >= 'A' && name[0] <= 'Z'
				sym := &SymbolNode{
					ID:        SymbolNodeID(path, name),
					Type:      "type",
					Name:      name,
					FilePath:  path,
					LineStart: int(spec.StartPoint().Row) + 1,
					LineEnd:   int(spec.EndPoint().Row) + 1,
					Exported:  exported,
					Language:  "go",
				}
				result.Symbols = append(result.Symbols, sym)
			}
		}
	})

	return result, nil
}

func extractGoParams(node *sitter.Node, content []byte) string {
	params := node.ChildByFieldName("parameters")
	if params == nil {
		return ""
	}
	return strings.TrimSpace(string(content[params.StartByte():params.EndByte()]))
}

func extractGoReturns(node *sitter.Node, content []byte) string {
	result := node.ChildByFieldName("result")
	if result == nil {
		return ""
	}
	return strings.TrimSpace(string(content[result.StartByte():result.EndByte()]))
}

// resolveGoImport maps a Go import path to a relative file path within the project.
// Only resolves project-internal imports (same module), ignores stdlib + external.
func resolveGoImport(fromFile, importPath, root string) string {
	if root == "" {
		return ""
	}
	if strings.HasPrefix(importPath, ".") {
		dir := filepath.Dir(fromFile)
		return filepath.Join(dir, importPath)
	}

	// Try to find a matching subdirectory in root by stripping module prefix segments.
	parts := strings.Split(importPath, "/")
	if len(parts) < 2 {
		return "" // stdlib single-word package
	}
	for i := 1; i <= len(parts); i++ {
		candidate := filepath.Join(parts[i:]...)
		fullPath := filepath.Join(root, candidate)
		if fullPath != root {
			return fullPath
		}
	}
	return ""
}
