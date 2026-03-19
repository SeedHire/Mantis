// Package graph/workspace provides cross-repo graph intelligence.
// It aggregates multiple repo graphs into a unified queryable workspace.
package graph

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// WorkspaceConfig represents a mantis.workspace.yml file.
type WorkspaceConfig struct {
	Version int         `yaml:"version"`
	Repos   []RepoEntry `yaml:"repos"`
}

// RepoEntry is a single repository in the workspace.
type RepoEntry struct {
	Path  string `yaml:"path"`
	Alias string `yaml:"alias"`
}

// Workspace provides unified queries across multiple repo graphs.
type Workspace struct {
	Config WorkspaceConfig
	Root   string // directory containing mantis.workspace.yml
	repos  map[string]*repoHandle
}

type repoHandle struct {
	alias   string
	absPath string
	db      *DB
	querier *Querier
}

// LoadWorkspace loads a mantis.workspace.yml from the given directory.
func LoadWorkspace(dir string) (*WorkspaceConfig, error) {
	path := filepath.Join(dir, "mantis.workspace.yml")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read workspace config: %w", err)
	}
	var cfg WorkspaceConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse workspace config: %w", err)
	}
	if len(cfg.Repos) == 0 {
		return nil, fmt.Errorf("workspace has no repos defined")
	}
	return &cfg, nil
}

// OpenWorkspace opens all repo databases in the workspace.
func OpenWorkspace(dir string) (*Workspace, error) {
	cfg, err := LoadWorkspace(dir)
	if err != nil {
		return nil, err
	}

	ws := &Workspace{
		Config: *cfg,
		Root:   dir,
		repos:  make(map[string]*repoHandle),
	}

	for _, entry := range cfg.Repos {
		absPath := entry.Path
		if !filepath.IsAbs(absPath) {
			absPath = filepath.Join(dir, absPath)
		}
		absPath, _ = filepath.Abs(absPath)

		dbPath := filepath.Join(absPath, ".mantis", "graph.db")
		if _, err := os.Stat(dbPath); os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "⚠ repo %q not indexed — run 'mantis init' in %s\n", entry.Alias, absPath)
			continue
		}

		db, err := Open(dbPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "⚠ failed to open %q: %v\n", entry.Alias, err)
			continue
		}

		alias := entry.Alias
		if alias == "" {
			alias = filepath.Base(absPath)
		}

		ws.repos[alias] = &repoHandle{
			alias:   alias,
			absPath: absPath,
			db:      db,
			querier: NewQuerier(db),
		}
	}

	if len(ws.repos) == 0 {
		return nil, fmt.Errorf("no repos could be opened in workspace")
	}

	return ws, nil
}

// Close releases all repo database connections.
func (ws *Workspace) Close() {
	for _, r := range ws.repos {
		r.db.Close()
	}
}

// RepoNames returns the aliases of all open repos.
func (ws *Workspace) RepoNames() []string {
	var names []string
	for name := range ws.repos {
		names = append(names, name)
	}
	return names
}

// CrossRepoResult holds a search result with its repo origin.
type CrossRepoResult struct {
	Repo     string
	Node     *Node
	Depth    int    // for BFS results
	Relation string // "defines", "imports", "imported-by"
}

// FindAcrossRepos searches all repos for a symbol by name.
func (ws *Workspace) FindAcrossRepos(symbolName string) ([]CrossRepoResult, error) {
	var results []CrossRepoResult

	for alias, rh := range ws.repos {
		nodes, err := rh.querier.FindNodeByName(symbolName)
		if err != nil {
			continue
		}
		for _, n := range nodes {
			results = append(results, CrossRepoResult{
				Repo:     alias,
				Node:     n,
				Relation: "defines",
			})
		}
	}

	return results, nil
}

// ImpactAcrossRepos traces the impact of changing a symbol across all repos.
func (ws *Workspace) ImpactAcrossRepos(symbolName string, maxDepth int) ([]CrossRepoResult, error) {
	var results []CrossRepoResult

	for alias, rh := range ws.repos {
		nodes, err := rh.querier.FindNodeByName(symbolName)
		if err != nil || len(nodes) == 0 {
			continue
		}

		// Get the file node for the symbol.
		for _, sym := range nodes {
			fileNode, err := rh.querier.GetFileNode(sym.FilePath)
			if err != nil || fileNode == nil {
				continue
			}

			// BFS reverse traversal — who imports this file?
			depthMap, err := rh.querier.BFSReverse(fileNode.ID, maxDepth)
			if err != nil {
				continue
			}

			for nodeID, depth := range depthMap {
				n, err := rh.querier.GetNodeByID(nodeID)
				if err != nil || n == nil {
					continue
				}
				results = append(results, CrossRepoResult{
					Repo:     alias,
					Node:     n,
					Depth:    depth,
					Relation: "imported-by",
				})
			}
		}
	}

	return results, nil
}

// CrossRepoEdge represents an import relationship between repos.
type CrossRepoEdge struct {
	FromRepo string
	FromFile string
	ToRepo   string
	ToFile   string
	Type     string // "import"
}

// DetectCrossRepoEdges finds import statements that reference files in other repos.
// This works by checking if any import path contains a known repo alias or path prefix.
func (ws *Workspace) DetectCrossRepoEdges() ([]CrossRepoEdge, error) {
	var edges []CrossRepoEdge

	for alias, rh := range ws.repos {
		// Get all edges of type "imports" in this repo.
		allEdges, err := rh.querier.GetAllEdges()
		if err != nil {
			continue
		}

		for _, e := range allEdges {
			if e.Type != EdgeTypeImport {
				continue
			}

			// Check if the target references another repo.
			for otherAlias, otherRh := range ws.repos {
				if otherAlias == alias {
					continue
				}

				// Check if edge metadata or to_id contains the other repo's path.
				if containsRepoRef(e.ToID, e.Metadata, otherRh.absPath, otherAlias) {
					fromNode, _ := rh.querier.GetNodeByID(e.FromID)
					fromFile := e.FromID
					if fromNode != nil {
						fromFile = fromNode.FilePath
					}

					edges = append(edges, CrossRepoEdge{
						FromRepo: alias,
						FromFile: fromFile,
						ToRepo:   otherAlias,
						ToFile:   e.ToID,
						Type:     "import",
					})
				}
			}
		}
	}

	return edges, nil
}

// Stats returns per-repo statistics for the workspace.
type WorkspaceStats struct {
	Repo    string
	Files   int
	Symbols int
	Edges   int
}

// GetStats returns statistics for each repo in the workspace.
func (ws *Workspace) GetStats() []WorkspaceStats {
	var stats []WorkspaceStats

	for alias, rh := range ws.repos {
		var files, symbols, edgeCount int

		rh.db.conn.QueryRow(`SELECT COUNT(*) FROM nodes WHERE type = 'file'`).Scan(&files)
		rh.db.conn.QueryRow(`SELECT COUNT(*) FROM nodes WHERE type != 'file'`).Scan(&symbols)
		rh.db.conn.QueryRow(`SELECT COUNT(*) FROM edges`).Scan(&edgeCount)

		stats = append(stats, WorkspaceStats{
			Repo:    alias,
			Files:   files,
			Symbols: symbols,
			Edges:   edgeCount,
		})
	}

	return stats
}

// containsRepoRef checks if an edge target references another repo.
func containsRepoRef(toID, metadata, repoPath, repoAlias string) bool {
	if strings.Contains(toID, repoPath) || strings.Contains(metadata, repoPath) {
		return true
	}
	lower := strings.ToLower(toID + metadata)
	aliasLower := strings.ToLower(repoAlias)
	if strings.Contains(lower, aliasLower+"/") || strings.Contains(lower, aliasLower+".") {
		return true
	}
	return false
}

// InitWorkspaceConfig creates a template mantis.workspace.yml.
func InitWorkspaceConfig(dir string, repos []RepoEntry) error {
	cfg := WorkspaceConfig{
		Version: 1,
		Repos:   repos,
	}
	data, err := yaml.Marshal(&cfg)
	if err != nil {
		return err
	}

	header := "# Mantis Workspace — multi-repo graph configuration\n" +
		"# Each repo must have been initialized with 'mantis init'\n\n"

	path := filepath.Join(dir, "mantis.workspace.yml")
	return os.WriteFile(path, []byte(header+string(data)), 0o644)
}
