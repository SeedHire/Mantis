package parser

import "strings"

// SymbolNode represents a code symbol (function, class, file, etc.).
type SymbolNode struct {
	ID        string
	Type      string
	Name      string
	FilePath  string
	LineStart int
	LineEnd   int
	Exported  bool
	Language  string
}

// ImportEdge represents a dependency between two files.
type ImportEdge struct {
	FromFile string
	ToFile   string
	RawPath  string
}

// ParseResult contains everything extracted from parsing a single file.
type ParseResult struct {
	FileNode *SymbolNode
	Symbols  []*SymbolNode
	Imports  []*ImportEdge
}

// Parser is the interface all language parsers must implement.
type Parser interface {
	ParseFile(path string, content []byte) (*ParseResult, error)
	Extensions() []string
	Language() string
}

// StripQuotes removes surrounding single, double, or backtick quotes.
func StripQuotes(s string) string {
	if len(s) < 2 {
		return s
	}
	if (s[0] == '"' && s[len(s)-1] == '"') ||
		(s[0] == '\'' && s[len(s)-1] == '\'') ||
		(s[0] == '`' && s[len(s)-1] == '`') {
		return s[1 : len(s)-1]
	}
	return s
}

// IsExternalImport returns true if the import path is an external package
// (i.e., not relative or absolute).
func IsExternalImport(path string) bool {
	return !strings.HasPrefix(path, ".") && !strings.HasPrefix(path, "/")
}

// FileNodeID returns the canonical node ID for a file.
func FileNodeID(filePath string) string {
	return "file:" + filePath
}

// SymbolNodeID returns the canonical node ID for a symbol within a file.
func SymbolNodeID(filePath, symbolName string) string {
	return "sym:" + filePath + "#" + symbolName
}
