package graph

import (
	"database/sql"
	"fmt"
)

// Querier provides read operations on the dependency graph.
type Querier struct {
	db *DB
}

// NewQuerier creates a new Querier for the given DB.
func NewQuerier(db *DB) *Querier {
	return &Querier{db: db}
}

// rowScanner is satisfied by both *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...interface{}) error
}

func scanNodeRow(row rowScanner) (*Node, error) {
	n := &Node{}
	var exported int
	var nodeType string
	err := row.Scan(
		&n.ID, &nodeType, &n.Name, &n.FilePath,
		&n.LineStart, &n.LineEnd, &n.Complexity,
		&exported, &n.Language, &n.LastModified,
	)
	if err != nil {
		return nil, err
	}
	n.Type = NodeType(nodeType)
	n.Exported = exported == 1
	return n, nil
}

func scanNodes(rows *sql.Rows) ([]*Node, error) {
	var nodes []*Node
	for rows.Next() {
		n, err := scanNodeRow(rows)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, n)
	}
	return nodes, rows.Err()
}

const nodeSelectCols = `id, type, name, file_path, line_start, line_end, complexity, exported, language, last_modified`

// FindNodeByName searches by exact name first, then LIKE.
func (q *Querier) FindNodeByName(name string) ([]*Node, error) {
	rows, err := q.db.conn.Query(
		`SELECT `+nodeSelectCols+` FROM nodes WHERE name = ?`, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	nodes, err := scanNodes(rows)
	if err != nil {
		return nil, err
	}
	if len(nodes) > 0 {
		return nodes, nil
	}
	// fallback: LIKE search
	rows2, err := q.db.conn.Query(
		`SELECT `+nodeSelectCols+` FROM nodes WHERE name LIKE ?`, "%"+name+"%")
	if err != nil {
		return nil, err
	}
	defer rows2.Close()
	return scanNodes(rows2)
}

// GetNodeByID retrieves a node by its ID.
func (q *Querier) GetNodeByID(id string) (*Node, error) {
	row := q.db.conn.QueryRow(
		`SELECT `+nodeSelectCols+` FROM nodes WHERE id = ?`, id)
	n, err := scanNodeRow(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return n, err
}

// GetFileNode retrieves the file node for a given file path.
func (q *Querier) GetFileNode(filePath string) (*Node, error) {
	return q.GetNodeByID("file:" + filePath)
}

// GetImportDeps returns files imported by the given file node ID (outbound).
func (q *Querier) GetImportDeps(fileNodeID string) ([]*Node, error) {
	rows, err := q.db.conn.Query(`
		SELECT `+nodeSelectCols+` FROM nodes
		WHERE id IN (SELECT to_id FROM edges WHERE from_id = ? AND type = ?)`,
		fileNodeID, string(EdgeTypeImport))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNodes(rows)
}

// GetImporters returns files that import the given file node ID (inbound).
func (q *Querier) GetImporters(fileNodeID string) ([]*Node, error) {
	rows, err := q.db.conn.Query(`
		SELECT `+nodeSelectCols+` FROM nodes
		WHERE id IN (SELECT from_id FROM edges WHERE to_id = ? AND type = ?)`,
		fileNodeID, string(EdgeTypeImport))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNodes(rows)
}

// BFSImports performs a breadth-first search over import edges from startID.
// Returns a map of nodeID → depth. maxDepth=0 means unlimited.
func (q *Querier) BFSImports(startID string, maxDepth int) (map[string]int, error) {
	visited := map[string]int{startID: 0}
	queue := []string{startID}

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		depth := visited[current]
		if maxDepth > 0 && depth >= maxDepth {
			continue
		}
		deps, err := q.GetImportDeps(current)
		if err != nil {
			return nil, err
		}
		for _, dep := range deps {
			if _, seen := visited[dep.ID]; !seen {
				visited[dep.ID] = depth + 1
				queue = append(queue, dep.ID)
			}
		}
	}
	return visited, nil
}

// FindAllNodes returns all nodes of the given type. Pass empty string for all.
func (q *Querier) FindAllNodes(nodeType NodeType) ([]*Node, error) {
	var rows *sql.Rows
	var err error
	if nodeType == "" {
		rows, err = q.db.conn.Query(`SELECT ` + nodeSelectCols + ` FROM nodes`)
	} else {
		rows, err = q.db.conn.Query(
			`SELECT `+nodeSelectCols+` FROM nodes WHERE type = ?`, string(nodeType))
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNodes(rows)
}

// FindDeadSymbols returns exported symbols with no inbound edges.
func (q *Querier) FindDeadSymbols() ([]*Node, error) {
	rows, err := q.db.conn.Query(`
		SELECT `+nodeSelectCols+` FROM nodes
		WHERE exported = 1
		  AND type != ?
		  AND id NOT IN (SELECT to_id FROM edges)`,
		string(NodeTypeFile))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNodes(rows)
}

// GetAllFiles returns all file-type nodes.
func (q *Querier) GetAllFiles() ([]*Node, error) {
	rows, err := q.db.conn.Query(
		`SELECT `+nodeSelectCols+` FROM nodes WHERE type = ?`, string(NodeTypeFile))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNodes(rows)
}

// BFSReverse performs a breadth-first search following INBOUND import edges from startID.
// Returns a map of nodeID → depth. maxDepth=0 means unlimited.
func (q *Querier) BFSReverse(startID string, maxDepth int) (map[string]int, error) {
	visited := map[string]int{startID: 0}
	queue := []string{startID}

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		depth := visited[current]
		if maxDepth > 0 && depth >= maxDepth {
			continue
		}
		importers, err := q.GetImporters(current)
		if err != nil {
			return nil, err
		}
		for _, imp := range importers {
			if _, seen := visited[imp.ID]; !seen {
				visited[imp.ID] = depth + 1
				queue = append(queue, imp.ID)
			}
		}
	}
	return visited, nil
}

// GetAllEdges returns all edges in the graph.
func (q *Querier) GetAllEdges() ([]Edge, error) {
	rows, err := q.db.conn.Query(`SELECT from_id, to_id, type, COALESCE(metadata, '') FROM edges`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var edges []Edge
	for rows.Next() {
		var e Edge
		var edgeType string
		if err := rows.Scan(&e.FromID, &e.ToID, &edgeType, &e.Metadata); err != nil {
			return nil, err
		}
		e.Type = EdgeType(edgeType)
		edges = append(edges, e)
	}
	return edges, rows.Err()
}

// FindSymbolsInFile returns all non-file nodes in the same file as the given node.
func (q *Querier) FindSymbolsInFile(fileID string) ([]*Node, error) {
	rows, err := q.db.conn.Query(`
		SELECT `+nodeSelectCols+` FROM nodes
		WHERE file_path = (SELECT file_path FROM nodes WHERE id = ?)
		  AND type != ?`, fileID, string(NodeTypeFile))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNodes(rows)
}

// FindFilesBySymbol returns FILE nodes whose file contains a node named symbolName.
func (q *Querier) FindFilesBySymbol(symbolName string) ([]*Node, error) {
	rows, err := q.db.conn.Query(`
		SELECT DISTINCT n.id, n.type, n.name, n.file_path, n.line_start, n.line_end,
		       n.complexity, n.exported, n.language, n.last_modified
		FROM nodes n
		JOIN nodes sym ON sym.file_path = n.file_path
		WHERE sym.name LIKE ? AND n.type = ?`, "%"+symbolName+"%", string(NodeTypeFile))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNodes(rows)
}

// nodeNotFoundError is returned when a node cannot be found.
type nodeNotFoundError struct{ id string }

func (e *nodeNotFoundError) Error() string {
	return fmt.Sprintf("node not found: %s", e.id)
}
