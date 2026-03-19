package graph

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/seedhire/mantis/internal/parser"
)

var defaultIgnore = map[string]bool{
	".git": true, "node_modules": true, "vendor": true,
	"dist": true, "build": true, ".mantis": true,
	"__pycache__": true, ".next": true, "coverage": true,
}

// Builder builds and updates the dependency graph from source files.
type Builder struct {
	db      *DB
	parsers map[string]parser.Parser // extension -> parser
	root    string
}

// NewBuilder creates a new Builder.
func NewBuilder(db *DB, root string) *Builder {
	return &Builder{
		db:      db,
		parsers: make(map[string]parser.Parser),
		root:    root,
	}
}

// RegisterParser registers a parser for all its extensions.
func (b *Builder) RegisterParser(p parser.Parser) {
	for _, ext := range p.Extensions() {
		b.parsers[ext] = p
	}
}

// BuildFull walks the directory tree and indexes all supported files.
func (b *Builder) BuildFull(ignorePatterns []string) (fileCount, symbolCount int, err error) {
	extraIgnore := map[string]bool{}
	for _, p := range ignorePatterns {
		extraIgnore[p] = true
	}

	// First pass: collect all parse results
	type pendingFile struct {
		path   string
		result *parser.ParseResult
	}
	var pending []pendingFile

	err = filepath.WalkDir(b.root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil // skip unreadable entries
		}
		if d.IsDir() {
			name := d.Name()
			if defaultIgnore[name] || extraIgnore[name] {
				return filepath.SkipDir
			}
			return nil
		}

		ext := filepath.Ext(path)
		p, ok := b.parsers[ext]
		if !ok {
			return nil
		}

		content, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}

		result, parseErr := p.ParseFile(path, content)
		if parseErr != nil || result == nil {
			return nil
		}

		pending = append(pending, pendingFile{path: path, result: result})
		return nil
	})
	if err != nil {
		return 0, 0, fmt.Errorf("walk dir: %w", err)
	}

	// Clear stale data before re-indexing to prevent orphaned edges.
	if _, err := b.db.conn.Exec(`DELETE FROM edges`); err != nil {
		return 0, 0, fmt.Errorf("clear edges: %w", err)
	}
	if _, err := b.db.conn.Exec(`DELETE FROM nodes`); err != nil {
		return 0, 0, fmt.Errorf("clear nodes: %w", err)
	}

	// Store all nodes first (so import edges can reference both sides)
	for _, pf := range pending {
		result := pf.result
		if result.FileNode != nil {
			fi, statErr := os.Stat(pf.path)
			var modTime int64
			if statErr == nil {
				modTime = fi.ModTime().Unix()
			}
			n := &Node{
				ID:           result.FileNode.ID,
				Type:         NodeTypeFile,
				Name:         result.FileNode.Name,
				FilePath:     result.FileNode.FilePath,
				LineStart:    result.FileNode.LineStart,
				LineEnd:      result.FileNode.LineEnd,
				Language:     result.FileNode.Language,
				Exported:     true,
				LastModified: modTime,
			}
			if uErr := b.db.UpsertNode(n); uErr != nil {
				continue
			}
			fileCount++
		}

		for _, sym := range result.Symbols {
			n := &Node{
				ID:        sym.ID,
				Type:      NodeType(sym.Type),
				Name:      sym.Name,
				FilePath:  sym.FilePath,
				LineStart: sym.LineStart,
				LineEnd:   sym.LineEnd,
				Language:  sym.Language,
				Exported:  sym.Exported,
			}
			if uErr := b.db.UpsertNode(n); uErr == nil {
				symbolCount++
			}
		}
	}

	// Second pass: create import edges (both nodes now exist)
	querier := NewQuerier(b.db)
	for _, pf := range pending {
		for _, imp := range pf.result.Imports {
			fromID := parser.FileNodeID(imp.FromFile)
			toID := parser.FileNodeID(imp.ToFile)
			// Only create edge if both nodes exist
			fromNode, _ := querier.GetNodeByID(fromID)
			toNode, _ := querier.GetNodeByID(toID)
			if fromNode == nil || toNode == nil {
				continue
			}
			_ = b.db.UpsertEdge(&Edge{
				FromID: fromID,
				ToID:   toID,
				Type:   EdgeTypeImport,
			})
		}
	}

	return fileCount, symbolCount, nil
}

// UpdateFile re-indexes a single file.
func (b *Builder) UpdateFile(filePath string) error {
	ext := filepath.Ext(filePath)
	p, ok := b.parsers[ext]
	if !ok {
		return nil
	}

	if err := b.db.DeleteFileEdges(filePath); err != nil {
		return err
	}
	if err := b.db.DeleteFileNodes(filePath); err != nil {
		return err
	}

	content, err := os.ReadFile(filePath)
	if err != nil {
		return nil // file may have been deleted
	}

	result, err := p.ParseFile(filePath, content)
	if err != nil || result == nil {
		return nil
	}

	fi, statErr := os.Stat(filePath)
	var modTime int64
	if statErr == nil {
		modTime = fi.ModTime().Unix()
	}

	if result.FileNode != nil {
		n := &Node{
			ID:           result.FileNode.ID,
			Type:         NodeTypeFile,
			Name:         result.FileNode.Name,
			FilePath:     result.FileNode.FilePath,
			LineStart:    result.FileNode.LineStart,
			LineEnd:      result.FileNode.LineEnd,
			Language:     result.FileNode.Language,
			Exported:     true,
			LastModified: modTime,
		}
		_ = b.db.UpsertNode(n)
	}

	for _, sym := range result.Symbols {
		n := &Node{
			ID:        sym.ID,
			Type:      NodeType(sym.Type),
			Name:      sym.Name,
			FilePath:  sym.FilePath,
			LineStart: sym.LineStart,
			LineEnd:   sym.LineEnd,
			Language:  sym.Language,
			Exported:  sym.Exported,
		}
		_ = b.db.UpsertNode(n)
	}

	querier := NewQuerier(b.db)
	for _, imp := range result.Imports {
		fromID := parser.FileNodeID(imp.FromFile)
		toID := parser.FileNodeID(imp.ToFile)
		fromNode, _ := querier.GetNodeByID(fromID)
		toNode, _ := querier.GetNodeByID(toID)
		if fromNode == nil || toNode == nil {
			continue
		}
		_ = b.db.UpsertEdge(&Edge{
			FromID: fromID,
			ToID:   toID,
			Type:   EdgeTypeImport,
		})
	}

	return nil
}

// RemoveFile removes all nodes and edges for a deleted file.
func (b *Builder) RemoveFile(filePath string) error {
	if err := b.db.DeleteFileEdges(filePath); err != nil {
		return err
	}
	return b.db.DeleteFileNodes(filePath)
}
