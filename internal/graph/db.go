package graph

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS nodes (
    id TEXT PRIMARY KEY,
    type TEXT NOT NULL,
    name TEXT NOT NULL,
    file_path TEXT NOT NULL,
    line_start INTEGER DEFAULT 0,
    line_end INTEGER DEFAULT 0,
    complexity INTEGER DEFAULT 0,
    exported INTEGER DEFAULT 0,
    language TEXT DEFAULT '',
    last_modified INTEGER DEFAULT 0
);
CREATE TABLE IF NOT EXISTS edges (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    from_id TEXT NOT NULL,
    to_id TEXT NOT NULL,
    type TEXT NOT NULL,
    metadata TEXT DEFAULT '',
    UNIQUE(from_id, to_id, type)
);
CREATE INDEX IF NOT EXISTS idx_edges_from ON edges(from_id);
CREATE INDEX IF NOT EXISTS idx_edges_to ON edges(to_id);
CREATE INDEX IF NOT EXISTS idx_nodes_name ON nodes(name);
CREATE INDEX IF NOT EXISTS idx_nodes_file ON nodes(file_path);
CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL DEFAULT '');
CREATE TABLE IF NOT EXISTS traces (
    node_id TEXT NOT NULL,
    call_count INTEGER DEFAULT 0,
    total_duration_ms REAL DEFAULT 0,
    avg_duration_ms REAL DEFAULT 0,
    source TEXT DEFAULT '',
    last_ingested INTEGER DEFAULT 0,
    UNIQUE(node_id, source)
);
CREATE INDEX IF NOT EXISTS idx_traces_node ON traces(node_id);
`

// DB wraps a SQLite database for the dependency graph.
type DB struct {
	conn *sql.DB
}

// Open opens (or creates) the SQLite database at dbPath.
func Open(dbPath string) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return nil, fmt.Errorf("create db dir: %w", err)
	}
	conn, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	conn.SetMaxOpenConns(1)
	if _, err := conn.Exec(schema); err != nil {
		conn.Close()
		return nil, fmt.Errorf("migrate schema: %w", err)
	}
	return &DB{conn: conn}, nil
}

// Close closes the underlying database connection.
func (db *DB) Close() error {
	return db.conn.Close()
}

// Conn returns the underlying *sql.DB.
func (db *DB) Conn() *sql.DB {
	return db.conn
}

// UpsertNode inserts or replaces a node.
func (db *DB) UpsertNode(n *Node) error {
	exported := 0
	if n.Exported {
		exported = 1
	}
	_, err := db.conn.Exec(`
		INSERT INTO nodes (id, type, name, file_path, line_start, line_end, complexity, exported, language, last_modified)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			type=excluded.type, name=excluded.name, file_path=excluded.file_path,
			line_start=excluded.line_start, line_end=excluded.line_end,
			complexity=excluded.complexity, exported=excluded.exported,
			language=excluded.language, last_modified=excluded.last_modified`,
		n.ID, string(n.Type), n.Name, n.FilePath,
		n.LineStart, n.LineEnd, n.Complexity,
		exported, n.Language, n.LastModified,
	)
	return err
}

// UpsertEdge inserts or ignores an edge (unique on from_id, to_id, type).
func (db *DB) UpsertEdge(e *Edge) error {
	_, err := db.conn.Exec(`
		INSERT INTO edges (from_id, to_id, type, metadata)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(from_id, to_id, type) DO UPDATE SET metadata=excluded.metadata`,
		e.FromID, e.ToID, string(e.Type), e.Metadata,
	)
	return err
}

// DeleteFileNodes removes all nodes whose file_path matches.
func (db *DB) DeleteFileNodes(filePath string) error {
	_, err := db.conn.Exec(`DELETE FROM nodes WHERE file_path = ?`, filePath)
	return err
}

// DeleteFileEdges removes edges originating from the file's node ID.
func (db *DB) DeleteFileEdges(filePath string) error {
	fileID := "file:" + filePath
	_, err := db.conn.Exec(`DELETE FROM edges WHERE from_id = ? OR to_id = ?`, fileID, fileID)
	return err
}

// SetMeta stores a key-value pair in the meta table.
func (db *DB) SetMeta(key, value string) error {
	_, err := db.conn.Exec(`
		INSERT INTO meta (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

// GetMeta retrieves a value from the meta table.
func (db *DB) GetMeta(key string) (string, error) {
	var value string
	err := db.conn.QueryRow(`SELECT value FROM meta WHERE key = ?`, key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return value, err
}

// Stats returns the total count of nodes and edges.
func (db *DB) Stats() (nodeCount, edgeCount int, err error) {
	if err = db.conn.QueryRow(`SELECT COUNT(*) FROM nodes`).Scan(&nodeCount); err != nil {
		return
	}
	err = db.conn.QueryRow(`SELECT COUNT(*) FROM edges`).Scan(&edgeCount)
	return
}
