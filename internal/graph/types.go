package graph

// NodeType represents the kind of a graph node.
type NodeType string

const (
	NodeTypeFile      NodeType = "file"
	NodeTypeFunction  NodeType = "function"
	NodeTypeClass     NodeType = "class"
	NodeTypeInterface NodeType = "interface"
	NodeTypeTypeAlias NodeType = "type_alias"
	NodeTypeMethod    NodeType = "method"
)

// EdgeType represents the kind of relationship between two nodes.
type EdgeType string

const (
	EdgeTypeImport    EdgeType = "import"
	EdgeTypeCall      EdgeType = "call"
	EdgeTypeReference EdgeType = "reference"
	EdgeTypeInherits  EdgeType = "inherits"
)

// Node is a vertex in the dependency graph.
type Node struct {
	ID           string
	Name         string
	FilePath     string
	Language     string
	Type         NodeType
	LineStart    int
	LineEnd      int
	Complexity   int
	Exported     bool
	LastModified int64
}

// Edge is a directed relationship between two nodes.
type Edge struct {
	FromID   string
	ToID     string
	Metadata string
	Type     EdgeType
}
