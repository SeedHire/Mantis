package graph

import (
	"path/filepath"
	"testing"
)

// openTestDB opens a SQLite graph database in a temp dir for testing.
func openTestDB(t *testing.T) *DB {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// ---------------------------------------------------------------------------
// DB: UpsertNode / UpsertEdge / Stats
// ---------------------------------------------------------------------------

func TestUpsertNodeAndStats(t *testing.T) {
	db := openTestDB(t)

	n := &Node{
		ID:       "file:/src/main.go",
		Type:     NodeTypeFile,
		Name:     "main.go",
		FilePath: "/src/main.go",
		Language: "go",
		Exported: true,
	}
	if err := db.UpsertNode(n); err != nil {
		t.Fatalf("UpsertNode: %v", err)
	}

	nodes, edges, err := db.Stats()
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if nodes != 1 {
		t.Errorf("nodes = %d, want 1", nodes)
	}
	if edges != 0 {
		t.Errorf("edges = %d, want 0", edges)
	}
}

func TestUpsertNodeIdempotent(t *testing.T) {
	db := openTestDB(t)

	n := &Node{
		ID:       "func:Foo:/src/main.go",
		Type:     NodeTypeFunction,
		Name:     "Foo",
		FilePath: "/src/main.go",
		Language: "go",
		Exported: true,
	}
	if err := db.UpsertNode(n); err != nil {
		t.Fatal(err)
	}
	n.LineEnd = 42
	if err := db.UpsertNode(n); err != nil {
		t.Fatal(err)
	}

	count, _, _ := db.Stats()
	if count != 1 {
		t.Errorf("expected 1 node after double upsert, got %d", count)
	}
}

func TestUpsertEdgeAndStats(t *testing.T) {
	db := openTestDB(t)

	for _, n := range []*Node{
		{ID: "file:a.go", Type: NodeTypeFile, Name: "a.go", FilePath: "a.go"},
		{ID: "file:b.go", Type: NodeTypeFile, Name: "b.go", FilePath: "b.go"},
	} {
		if err := db.UpsertNode(n); err != nil {
			t.Fatal(err)
		}
	}

	e := &Edge{FromID: "file:a.go", ToID: "file:b.go", Type: EdgeTypeImport}
	if err := db.UpsertEdge(e); err != nil {
		t.Fatalf("UpsertEdge: %v", err)
	}

	_, edges, err := db.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if edges != 1 {
		t.Errorf("edges = %d, want 1", edges)
	}

	// Duplicate edge should be upserted, not fail.
	if err := db.UpsertEdge(e); err != nil {
		t.Fatalf("duplicate UpsertEdge: %v", err)
	}
	_, edges, _ = db.Stats()
	if edges != 1 {
		t.Errorf("edges after duplicate = %d, want 1", edges)
	}
}

// ---------------------------------------------------------------------------
// DB: DeleteFileNodes / DeleteFileEdges
// ---------------------------------------------------------------------------

func TestDeleteFileNodes(t *testing.T) {
	db := openTestDB(t)

	db.UpsertNode(&Node{ID: "file:/x.go", Type: NodeTypeFile, Name: "x.go", FilePath: "/x.go"})
	db.UpsertNode(&Node{ID: "func:Bar:/x.go", Type: NodeTypeFunction, Name: "Bar", FilePath: "/x.go"})
	db.UpsertNode(&Node{ID: "file:/y.go", Type: NodeTypeFile, Name: "y.go", FilePath: "/y.go"})

	if err := db.DeleteFileNodes("/x.go"); err != nil {
		t.Fatalf("DeleteFileNodes: %v", err)
	}

	count, _, _ := db.Stats()
	if count != 1 {
		t.Errorf("nodes remaining = %d, want 1 (/y.go)", count)
	}
}

func TestDeleteFileEdges(t *testing.T) {
	db := openTestDB(t)

	db.UpsertNode(&Node{ID: "file:/a.go", Type: NodeTypeFile, Name: "a.go", FilePath: "/a.go"})
	db.UpsertNode(&Node{ID: "file:/b.go", Type: NodeTypeFile, Name: "b.go", FilePath: "/b.go"})
	db.UpsertNode(&Node{ID: "file:/c.go", Type: NodeTypeFile, Name: "c.go", FilePath: "/c.go"})

	db.UpsertEdge(&Edge{FromID: "file:/a.go", ToID: "file:/b.go", Type: EdgeTypeImport})
	db.UpsertEdge(&Edge{FromID: "file:/b.go", ToID: "file:/c.go", Type: EdgeTypeImport})

	if err := db.DeleteFileEdges("/b.go"); err != nil {
		t.Fatalf("DeleteFileEdges: %v", err)
	}

	_, edges, _ := db.Stats()
	if edges != 0 {
		t.Errorf("edges remaining = %d, want 0", edges)
	}
}

// ---------------------------------------------------------------------------
// DB: Meta
// ---------------------------------------------------------------------------

func TestMetaGetSet(t *testing.T) {
	db := openTestDB(t)

	if err := db.SetMeta("version", "42"); err != nil {
		t.Fatal(err)
	}
	val, err := db.GetMeta("version")
	if err != nil {
		t.Fatal(err)
	}
	if val != "42" {
		t.Errorf("GetMeta = %q, want 42", val)
	}

	val, err = db.GetMeta("missing")
	if err != nil {
		t.Fatal(err)
	}
	if val != "" {
		t.Errorf("GetMeta(missing) = %q, want empty", val)
	}
}

// ---------------------------------------------------------------------------
// Querier: GetNodeByID / GetFileNode / FindNodeByName
// ---------------------------------------------------------------------------

func seedThreeFiles(t *testing.T, db *DB) *Querier {
	t.Helper()
	for _, n := range []*Node{
		{ID: "file:/a.go", Type: NodeTypeFile, Name: "a.go", FilePath: "/a.go", Language: "go", Exported: true},
		{ID: "file:/b.go", Type: NodeTypeFile, Name: "b.go", FilePath: "/b.go", Language: "go", Exported: true},
		{ID: "file:/c.go", Type: NodeTypeFile, Name: "c.go", FilePath: "/c.go", Language: "go", Exported: true},
	} {
		if err := db.UpsertNode(n); err != nil {
			t.Fatal(err)
		}
	}
	db.UpsertNode(&Node{ID: "func:Handler:/b.go", Type: NodeTypeFunction, Name: "Handler", FilePath: "/b.go", Language: "go", Exported: true})

	db.UpsertEdge(&Edge{FromID: "file:/a.go", ToID: "file:/b.go", Type: EdgeTypeImport})
	db.UpsertEdge(&Edge{FromID: "file:/b.go", ToID: "file:/c.go", Type: EdgeTypeImport})

	return NewQuerier(db)
}

func TestGetNodeByID(t *testing.T) {
	db := openTestDB(t)
	q := seedThreeFiles(t, db)

	n, err := q.GetNodeByID("file:/a.go")
	if err != nil {
		t.Fatal(err)
	}
	if n == nil || n.Name != "a.go" {
		t.Fatalf("expected a.go, got %v", n)
	}

	n, err = q.GetNodeByID("file:/nonexistent.go")
	if err != nil {
		t.Fatal(err)
	}
	if n != nil {
		t.Errorf("expected nil for missing node, got %v", n)
	}
}

func TestGetFileNode(t *testing.T) {
	db := openTestDB(t)
	q := seedThreeFiles(t, db)

	n, err := q.GetFileNode("/b.go")
	if err != nil {
		t.Fatal(err)
	}
	if n == nil || n.Type != NodeTypeFile {
		t.Fatalf("expected file node, got %v", n)
	}
}

func TestFindNodeByNameExact(t *testing.T) {
	db := openTestDB(t)
	q := seedThreeFiles(t, db)

	nodes, err := q.FindNodeByName("Handler")
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 {
		t.Fatalf("expected 1 match, got %d", len(nodes))
	}
	if nodes[0].Type != NodeTypeFunction {
		t.Errorf("type = %s, want function", nodes[0].Type)
	}
}

// ---------------------------------------------------------------------------
// Querier: GetImportDeps / GetImporters
// ---------------------------------------------------------------------------

func TestGetImportDeps(t *testing.T) {
	db := openTestDB(t)
	q := seedThreeFiles(t, db)

	deps, err := q.GetImportDeps("file:/a.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 1 || deps[0].ID != "file:/b.go" {
		t.Errorf("a.go deps = %v, want [file:/b.go]", nodeIDs(deps))
	}
}

func TestGetImporters(t *testing.T) {
	db := openTestDB(t)
	q := seedThreeFiles(t, db)

	importers, err := q.GetImporters("file:/b.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(importers) != 1 || importers[0].ID != "file:/a.go" {
		t.Errorf("b.go importers = %v, want [file:/a.go]", nodeIDs(importers))
	}
}

func TestGetImportDepsNone(t *testing.T) {
	db := openTestDB(t)
	q := seedThreeFiles(t, db)

	deps, err := q.GetImportDeps("file:/c.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 0 {
		t.Errorf("c.go deps = %v, want none", nodeIDs(deps))
	}
}

// ---------------------------------------------------------------------------
// Querier: BFS
// ---------------------------------------------------------------------------

func TestBFSImports(t *testing.T) {
	db := openTestDB(t)
	q := seedThreeFiles(t, db)

	visited, err := q.BFSImports("file:/a.go", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(visited) != 3 {
		t.Errorf("BFS visited %d nodes, want 3", len(visited))
	}
	if visited["file:/c.go"] != 2 {
		t.Errorf("c.go depth = %d, want 2", visited["file:/c.go"])
	}
}

func TestBFSImportsMaxDepth(t *testing.T) {
	db := openTestDB(t)
	q := seedThreeFiles(t, db)

	visited, err := q.BFSImports("file:/a.go", 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, found := visited["file:/c.go"]; found {
		t.Error("c.go should not be reachable at maxDepth=1")
	}
	if len(visited) != 2 {
		t.Errorf("visited %d nodes, want 2", len(visited))
	}
}

func TestBFSReverse(t *testing.T) {
	db := openTestDB(t)
	q := seedThreeFiles(t, db)

	visited, err := q.BFSReverse("file:/c.go", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(visited) != 3 {
		t.Errorf("BFSReverse visited %d nodes, want 3", len(visited))
	}
	if visited["file:/a.go"] != 2 {
		t.Errorf("a.go depth = %d, want 2", visited["file:/a.go"])
	}
}

// ---------------------------------------------------------------------------
// Querier: GetAllEdges / GetAllFiles / FindAllNodes / FindDeadSymbols
// ---------------------------------------------------------------------------

func TestGetAllEdges(t *testing.T) {
	db := openTestDB(t)
	seedThreeFiles(t, db)
	q := NewQuerier(db)

	edges, err := q.GetAllEdges()
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 2 {
		t.Errorf("edges = %d, want 2", len(edges))
	}
}

func TestGetAllFiles(t *testing.T) {
	db := openTestDB(t)
	seedThreeFiles(t, db)
	q := NewQuerier(db)

	files, err := q.GetAllFiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 3 {
		t.Errorf("files = %d, want 3", len(files))
	}
}

func TestFindAllNodes(t *testing.T) {
	db := openTestDB(t)
	seedThreeFiles(t, db)
	q := NewQuerier(db)

	all, err := q.FindAllNodes("")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 4 {
		t.Errorf("all nodes = %d, want 4", len(all))
	}

	funcs, err := q.FindAllNodes(NodeTypeFunction)
	if err != nil {
		t.Fatal(err)
	}
	if len(funcs) != 1 {
		t.Errorf("functions = %d, want 1", len(funcs))
	}
}

func TestFindDeadSymbols(t *testing.T) {
	db := openTestDB(t)
	seedThreeFiles(t, db)
	q := NewQuerier(db)

	dead, err := q.FindDeadSymbols()
	if err != nil {
		t.Fatal(err)
	}
	if len(dead) != 1 || dead[0].Name != "Handler" {
		t.Errorf("dead symbols = %v, want [Handler]", nodeNames(dead))
	}
}

func TestFindDeadSymbolsNoneWhenReferenced(t *testing.T) {
	db := openTestDB(t)
	seedThreeFiles(t, db)
	db.UpsertEdge(&Edge{FromID: "file:/a.go", ToID: "func:Handler:/b.go", Type: EdgeTypeCall})

	q := NewQuerier(db)
	dead, err := q.FindDeadSymbols()
	if err != nil {
		t.Fatal(err)
	}
	if len(dead) != 0 {
		t.Errorf("dead symbols = %v, want none", nodeNames(dead))
	}
}

// ---------------------------------------------------------------------------
// Querier: FindSymbolsInFile / FindFilesBySymbol
// ---------------------------------------------------------------------------

func TestFindSymbolsInFile(t *testing.T) {
	db := openTestDB(t)
	seedThreeFiles(t, db)
	q := NewQuerier(db)

	syms, err := q.FindSymbolsInFile("file:/b.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(syms) != 1 || syms[0].Name != "Handler" {
		t.Errorf("symbols in b.go = %v, want [Handler]", nodeNames(syms))
	}
}

func TestFindFilesBySymbol(t *testing.T) {
	db := openTestDB(t)
	seedThreeFiles(t, db)
	q := NewQuerier(db)

	files, err := q.FindFilesBySymbol("Handler")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].FilePath != "/b.go" {
		t.Errorf("files containing Handler = %v, want [/b.go]", nodeIDs(files))
	}
}

// ---------------------------------------------------------------------------
// Builder: NewBuilder / RemoveFile
// ---------------------------------------------------------------------------

func TestBuilderRemoveFile(t *testing.T) {
	db := openTestDB(t)

	db.UpsertNode(&Node{ID: "file:/src/x.go", Type: NodeTypeFile, Name: "x.go", FilePath: "/src/x.go"})
	db.UpsertNode(&Node{ID: "func:Run:/src/x.go", Type: NodeTypeFunction, Name: "Run", FilePath: "/src/x.go"})
	db.UpsertNode(&Node{ID: "file:/src/y.go", Type: NodeTypeFile, Name: "y.go", FilePath: "/src/y.go"})
	db.UpsertEdge(&Edge{FromID: "file:/src/x.go", ToID: "file:/src/y.go", Type: EdgeTypeImport})

	b := NewBuilder(db, "/src")
	if err := b.RemoveFile("/src/x.go"); err != nil {
		t.Fatalf("RemoveFile: %v", err)
	}

	nodes, edges, _ := db.Stats()
	if nodes != 1 {
		t.Errorf("nodes = %d, want 1 (y.go only)", nodes)
	}
	if edges != 0 {
		t.Errorf("edges = %d, want 0", edges)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func nodeIDs(nodes []*Node) []string {
	ids := make([]string, len(nodes))
	for i, n := range nodes {
		ids[i] = n.ID
	}
	return ids
}

func nodeNames(nodes []*Node) []string {
	names := make([]string, len(nodes))
	for i, n := range nodes {
		names[i] = n.Name
	}
	return names
}
