package context

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/seedhire/mantis/internal/graph"
	"github.com/seedhire/mantis/internal/parser"
)

// BundleFile represents a single file in a context bundle.
type BundleFile struct {
	Path    string
	Depth   int
	Content string
}

// Bundle is the result of bundling a symbol's context.
type Bundle struct {
	Symbol   string
	Files    []BundleFile
	Tokens   int
	MaxDepth int
}

// Bundler assembles context bundles for symbols.
type Bundler struct {
	db      *graph.DB
	querier *graph.Querier
	root    string
}

// NewBundler creates a new Bundler.
func NewBundler(db *graph.DB, root string) *Bundler {
	return &Bundler{
		db:      db,
		querier: graph.NewQuerier(db),
		root:    root,
	}
}

// Bundle assembles a context bundle for the given symbol name.
func (b *Bundler) Bundle(symbolName string, maxDepth, tokenBudget int) (*Bundle, error) {
	nodes, err := b.querier.FindNodeByName(symbolName)
	if err != nil {
		return nil, err
	}
	if len(nodes) == 0 {
		return nil, fmt.Errorf("symbol %q not found", symbolName)
	}

	// Use the first match
	sym := nodes[0]

	// Get file node for the symbol
	fileNode, err := b.querier.GetFileNode(sym.FilePath)
	if err != nil || fileNode == nil {
		// Try using the file path directly
		fileNode = &graph.Node{
			ID:       parser.FileNodeID(sym.FilePath),
			FilePath: sym.FilePath,
		}
	}

	// BFS import traversal
	depthMap, err := b.querier.BFSImports(fileNode.ID, maxDepth)
	if err != nil {
		return nil, err
	}

	// Get importers (referenced-by files)
	importers, err := b.querier.GetImporters(fileNode.ID)
	if err != nil {
		return nil, err
	}

	// Build sections for token trimming
	var sections []Section
	filesByID := map[string]BundleFile{}

	// Add BFS files
	for nodeID, depth := range depthMap {
		n, nErr := b.querier.GetNodeByID(nodeID)
		if nErr != nil || n == nil {
			continue
		}
		content, _ := os.ReadFile(n.FilePath)
		priority := depthPriority(depth)
		bf := BundleFile{
			Path:    n.FilePath,
			Depth:   depth,
			Content: string(content),
		}
		filesByID[nodeID] = bf
		sections = append(sections, Section{
			Content:  string(content),
			Priority: priority,
			Label:    n.FilePath,
		})
	}

	// Add referenced-by files at depth maxDepth+1
	for _, imp := range importers {
		if _, already := depthMap[imp.ID]; already {
			continue
		}
		content, _ := os.ReadFile(imp.FilePath)
		bf := BundleFile{
			Path:    imp.FilePath,
			Depth:   maxDepth + 1,
			Content: string(content),
		}
		filesByID[imp.ID] = bf
		sections = append(sections, Section{
			Content:  string(content),
			Priority: 2,
			Label:    imp.FilePath,
		})
	}

	// Apply token budget
	trimmed := TrimToTokenBudget(sections, tokenBudget)
	trimmedSet := map[string]string{}
	for _, s := range trimmed {
		trimmedSet[s.Label] = s.Content
	}

	var files []BundleFile
	totalTokens := 0
	for _, bf := range filesByID {
		if content, ok := trimmedSet[bf.Path]; ok {
			bf.Content = content
			files = append(files, bf)
			totalTokens += EstimateTokens(content)
		}
	}
	return &Bundle{
		Symbol:   symbolName,
		Files:    files,
		Tokens:   totalTokens,
		MaxDepth: maxDepth,
	}, nil
}

func depthPriority(depth int) int {
	switch depth {
	case 0:
		return 10
	case 1:
		return 8
	case 2:
		return 5
	default:
		return 3
	}
}

// RenderMarkdown renders a context bundle as a Markdown document.
func (b *Bundler) RenderMarkdown(bundle *Bundle) string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("# Context Bundle: %s\n", bundle.Symbol))
	sb.WriteString(fmt.Sprintf("Generated: %s  |  Files: %d  |  Tokens: ~%d\n\n",
		time.Now().Format("2006-01-02 15:04:05"), len(bundle.Files), bundle.Tokens))

	type group struct {
		label string
		files []BundleFile
	}

	entry := group{label: "## Entry Point"}
	depth1 := group{label: "## Direct Dependencies (depth 1)"}
	depth2plus := group{label: "## Indirect Dependencies (depth 2+)"}
	refBy := group{label: "## Referenced By"}

	for _, f := range bundle.Files {
		switch f.Depth {
		case 0:
			entry.files = append(entry.files, f)
		case 1:
			depth1.files = append(depth1.files, f)
		default:
			if f.Depth > bundle.MaxDepth {
				refBy.files = append(refBy.files, f)
			} else {
				depth2plus.files = append(depth2plus.files, f)
			}
		}
	}

	for _, g := range []group{entry, depth1, depth2plus, refBy} {
		if len(g.files) == 0 {
			continue
		}
		sb.WriteString(g.label + "\n\n")
		for _, f := range g.files {
			rel, err := filepath.Rel(b.root, f.Path)
			if err != nil {
				rel = f.Path
			}
			ext := strings.TrimPrefix(filepath.Ext(f.Path), ".")
			sb.WriteString(fmt.Sprintf("`%s`\n\n```%s\n%s\n```\n\n", rel, ext, f.Content))
		}
	}

	return sb.String()
}
