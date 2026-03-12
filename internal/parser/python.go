package parser

import (
	"context"
	"path/filepath"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/python"
)

// PythonParser parses Python source files.
// Root should be set to the project root directory for accurate import resolution.
type PythonParser struct {
	Root string
}

func (p *PythonParser) Language() string { return "python" }

func (p *PythonParser) Extensions() []string { return []string{".py"} }

// ParseFile parses a Python file and extracts symbols and imports.
func (p *PythonParser) ParseFile(path string, content []byte) (*ParseResult, error) {
	parser := sitter.NewParser()
	defer parser.Close()
	parser.SetLanguage(python.GetLanguage())

	tree, err := parser.ParseCtx(context.Background(), nil, content)
	if err != nil {
		return &ParseResult{FileNode: fileSymbolNode(path, "python", countLines(content))}, nil
	}
	defer tree.Close()

	root := tree.RootNode()
	lineCount := countLines(content)

	result := &ParseResult{
		FileNode: fileSymbolNode(path, "python", lineCount),
	}

	walkAST(root, func(node *sitter.Node) {
		switch node.Type() {
		case "import_statement":
			// e.g. import os, import foo.bar
			for i := 0; i < int(node.ChildCount()); i++ {
				child := node.Child(i)
				if child.Type() == "dotted_name" || child.Type() == "aliased_import" {
					var modulePath string
					if child.Type() == "aliased_import" {
						// get the name before 'as'
						inner := child.ChildByFieldName("name")
						if inner != nil {
							modulePath = string(content[inner.StartByte():inner.EndByte()])
						}
					} else {
						modulePath = string(content[child.StartByte():child.EndByte()])
					}
					if modulePath == "" {
						continue
					}
					pyPath := strings.ReplaceAll(modulePath, ".", "/") + ".py"
					resolved := p.resolvePyImport(path, pyPath)
					if resolved == "" {
						continue
					}
					result.Imports = append(result.Imports, &ImportEdge{
						FromFile: path,
						ToFile:   resolved,
						RawPath:  modulePath,
					})
				}
			}

		case "import_from_statement":
			// e.g. from foo.bar import baz
			modNode := node.ChildByFieldName("module_name")
			if modNode == nil {
				return
			}
			modulePath := string(content[modNode.StartByte():modNode.EndByte()])
			pyPath := strings.ReplaceAll(modulePath, ".", "/") + ".py"
			resolved := p.resolvePyImport(path, pyPath)
			if resolved == "" {
				return
			}
			result.Imports = append(result.Imports, &ImportEdge{
				FromFile: path,
				ToFile:   resolved,
				RawPath:  modulePath,
			})

		case "function_definition":
			nameNode := node.ChildByFieldName("name")
			if nameNode == nil {
				return
			}
			name := string(content[nameNode.StartByte():nameNode.EndByte()])
			exported := !strings.HasPrefix(name, "_")
			sym := &SymbolNode{
				ID:        SymbolNodeID(path, name),
				Type:      "function",
				Name:      name,
				FilePath:  path,
				LineStart: int(node.StartPoint().Row) + 1,
				LineEnd:   int(node.EndPoint().Row) + 1,
				Exported:  exported,
				Language:  "python",
			}
			result.Symbols = append(result.Symbols, sym)

		case "class_definition":
			nameNode := node.ChildByFieldName("name")
			if nameNode == nil {
				return
			}
			name := string(content[nameNode.StartByte():nameNode.EndByte()])
			exported := !strings.HasPrefix(name, "_")
			sym := &SymbolNode{
				ID:        SymbolNodeID(path, name),
				Type:      "class",
				Name:      name,
				FilePath:  path,
				LineStart: int(node.StartPoint().Row) + 1,
				LineEnd:   int(node.EndPoint().Row) + 1,
				Exported:  exported,
				Language:  "python",
			}
			result.Symbols = append(result.Symbols, sym)
		}
	})

	return result, nil
}

// resolvePyImport resolves a Python module path (e.g. "src/token.py") to an absolute file path.
// It tries: project root, then file's directory, then stdlib skip.
func (p *PythonParser) resolvePyImport(fromFile, pyPath string) string {
	candidates := []string{}

	// Try from project root first (most common for absolute imports)
	if p.Root != "" {
		candidates = append(candidates,
			filepath.Join(p.Root, pyPath),
			filepath.Join(p.Root, strings.TrimSuffix(pyPath, ".py"), "__init__.py"),
		)
	}
	// Try relative to the file's directory (for relative imports)
	candidates = append(candidates,
		filepath.Join(filepath.Dir(fromFile), pyPath),
	)

	for _, c := range candidates {
		if fileExists(c) {
			return c
		}
	}
	return ""
}
