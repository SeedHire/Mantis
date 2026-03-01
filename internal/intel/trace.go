// Package intel provides runtime trace ingestion and analysis.
// Supports OTLP JSON, Go pprof text, and a simple custom JSON format.
// Trace data is stored in the graph database and used to weight
// impact analysis by actual runtime behavior.
package intel

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ── Types ────────────────────────────────────────────────────────────────────

// TraceEntry represents a single parsed trace record mapped to code.
type TraceEntry struct {
	Function    string  `json:"function"`
	File        string  `json:"file,omitempty"`
	CallCount   int     `json:"calls"`
	DurationMs  float64 `json:"duration_ms"`
	Source      string  `json:"source,omitempty"` // "otlp", "pprof", "custom"
}

// TraceStats holds aggregated runtime stats for a code node.
type TraceStats struct {
	NodeID        string
	Name          string
	FilePath      string
	CallCount     int
	TotalDuration float64
	AvgDuration   float64
	Source        string
}

// WeightedNode combines structural graph depth with runtime frequency.
type WeightedNode struct {
	NodeID        string
	Name          string
	FilePath      string
	StructDepth   int     // BFS depth from target
	CallCount     int     // runtime call frequency
	AvgDurationMs float64 // average call duration
	RuntimeWeight float64 // computed: calls * (1 / depth)
}

// ── Format Detection & Parsing ───────────────────────────────────────────────

// IngestTraceFile detects format and parses trace entries from a file.
func IngestTraceFile(path string) ([]TraceEntry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read trace file: %w", err)
	}

	content := strings.TrimSpace(string(data))
	if len(content) == 0 {
		return nil, fmt.Errorf("empty trace file")
	}

	// Try JSON formats first.
	if content[0] == '[' || content[0] == '{' {
		// Try custom JSON array first (simplest).
		if entries, err := parseCustomJSON(data); err == nil && len(entries) > 0 {
			return entries, nil
		}
		// Try OTLP JSON.
		if entries, err := parseOTLP(data); err == nil && len(entries) > 0 {
			return entries, nil
		}
		return nil, fmt.Errorf("unrecognized JSON trace format")
	}

	// Try pprof text format.
	if entries, err := parsePprofText(content); err == nil && len(entries) > 0 {
		return entries, nil
	}

	return nil, fmt.Errorf("unrecognized trace format (supported: OTLP JSON, pprof text, custom JSON)")
}

// ── Custom JSON Format ───────────────────────────────────────────────────────
// Simple array: [{"function":"Handler","file":"api.go","calls":150,"duration_ms":23.5}]

func parseCustomJSON(data []byte) ([]TraceEntry, error) {
	var entries []TraceEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, err
	}
	for i := range entries {
		entries[i].Source = "custom"
		if entries[i].Function == "" {
			return nil, fmt.Errorf("entry %d missing function name", i)
		}
	}
	return entries, nil
}

// ── OTLP JSON Format ────────────────────────────────────────────────────────
// Standard OpenTelemetry trace export format.

type otlpExport struct {
	ResourceSpans []struct {
		ScopeSpans []struct {
			Spans []otlpSpan `json:"spans"`
		} `json:"scopeSpans"`
	} `json:"resourceSpans"`
}

type otlpSpan struct {
	Name         string `json:"name"`
	StartTimeNs string `json:"startTimeUnixNano"`
	EndTimeNs    string `json:"endTimeUnixNano"`
	Attributes   []struct {
		Key   string          `json:"key"`
		Value json.RawMessage `json:"value"`
	} `json:"attributes"`
}

func parseOTLP(data []byte) ([]TraceEntry, error) {
	var export otlpExport
	if err := json.Unmarshal(data, &export); err != nil {
		return nil, err
	}

	// Aggregate by operation name.
	type agg struct {
		calls    int
		totalNs  int64
		file     string
	}
	byOp := make(map[string]*agg)

	for _, rs := range export.ResourceSpans {
		for _, ss := range rs.ScopeSpans {
			for _, span := range ss.Spans {
				name := span.Name
				if name == "" {
					continue
				}

				a, ok := byOp[name]
				if !ok {
					a = &agg{}
					byOp[name] = a
				}
				a.calls++

				startNs, _ := strconv.ParseInt(span.StartTimeNs, 10, 64)
				endNs, _ := strconv.ParseInt(span.EndTimeNs, 10, 64)
				if endNs > startNs {
					a.totalNs += endNs - startNs
				}

				// Extract code.filepath or code.function from attributes.
				for _, attr := range span.Attributes {
					if attr.Key == "code.filepath" {
						var sv struct{ StringValue string `json:"stringValue"` }
						if json.Unmarshal(attr.Value, &sv) == nil && sv.StringValue != "" {
							a.file = sv.StringValue
						}
					}
				}
			}
		}
	}

	var entries []TraceEntry
	for name, a := range byOp {
		entries = append(entries, TraceEntry{
			Function:   name,
			File:       a.file,
			CallCount:  a.calls,
			DurationMs: float64(a.totalNs) / 1e6,
			Source:     "otlp",
		})
	}
	return entries, nil
}

// ── Go pprof Text Format ─────────────────────────────────────────────────────
// Output of: go tool pprof -text -cum cpu.prof
// Lines like: 120ms  2.40%  85.00%   500ms  10.00%  main.handler

var pprofLineRe = regexp.MustCompile(
	`^\s*([\d.]+\w+)\s+[\d.]+%\s+[\d.]+%\s+([\d.]+\w+)\s+[\d.]+%\s+(.+)$`,
)

func parsePprofText(content string) ([]TraceEntry, error) {
	var entries []TraceEntry
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		m := pprofLineRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}

		funcName := strings.TrimSpace(m[3])
		cumDuration := parsePprofDuration(m[2])

		// Extract file path if present (func looks like "pkg/file.go:123 funcName").
		file := ""
		if idx := strings.LastIndex(funcName, " "); idx > 0 {
			file = funcName[:idx]
			funcName = funcName[idx+1:]
		}
		// Strip package prefix for matching (e.g., "main.handler" → "handler").
		if dot := strings.LastIndex(funcName, "."); dot >= 0 {
			funcName = funcName[dot+1:]
		}

		entries = append(entries, TraceEntry{
			Function:   funcName,
			File:       file,
			CallCount:  1, // pprof doesn't give call counts directly
			DurationMs: cumDuration,
			Source:     "pprof",
		})
	}
	return entries, nil
}

func parsePprofDuration(s string) float64 {
	s = strings.TrimSpace(s)
	if strings.HasSuffix(s, "ms") {
		v, _ := strconv.ParseFloat(strings.TrimSuffix(s, "ms"), 64)
		return v
	}
	if strings.HasSuffix(s, "us") || strings.HasSuffix(s, "µs") {
		v, _ := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSuffix(s, "us"), "µs"), 64)
		return v / 1000
	}
	if strings.HasSuffix(s, "s") {
		v, _ := strconv.ParseFloat(strings.TrimSuffix(s, "s"), 64)
		return v * 1000
	}
	v, _ := strconv.ParseFloat(s, 64)
	return v
}

// ── Database Storage ─────────────────────────────────────────────────────────

// nodeInfo holds basic node data for trace matching.
type nodeInfo struct {
	id, name, file string
}

// StoreTraces upserts trace entries into the graph database traces table.
// It maps function names to graph node IDs using fuzzy matching.
func StoreTraces(db *sql.DB, entries []TraceEntry) (matched, unmatched int, err error) {
	// Build a lookup: function_name → node_id.
	rows, err := db.Query(`SELECT id, name, file_path FROM nodes WHERE type IN ('function', 'method', 'class')`)
	if err != nil {
		return 0, 0, fmt.Errorf("query nodes: %w", err)
	}
	defer rows.Close()

	var nodes []nodeInfo
	nameIndex := make(map[string][]nodeInfo) // name → []nodeInfo

	for rows.Next() {
		var n nodeInfo
		if err := rows.Scan(&n.id, &n.name, &n.file); err != nil {
			continue
		}
		nodes = append(nodes, n)
		lower := strings.ToLower(n.name)
		nameIndex[lower] = append(nameIndex[lower], n)
	}

	now := time.Now().Unix()
	tx, err := db.Begin()
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`
		INSERT INTO traces (node_id, call_count, total_duration_ms, avg_duration_ms, source, last_ingested)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(node_id, source) DO UPDATE SET
			call_count = call_count + excluded.call_count,
			total_duration_ms = total_duration_ms + excluded.total_duration_ms,
			avg_duration_ms = (total_duration_ms + excluded.total_duration_ms) /
				CASE WHEN (call_count + excluded.call_count) > 0
					THEN (call_count + excluded.call_count) ELSE 1 END,
			last_ingested = excluded.last_ingested
	`)
	if err != nil {
		return 0, 0, fmt.Errorf("prepare stmt: %w", err)
	}
	defer stmt.Close()

	for _, entry := range entries {
		nodeID := matchEntryToNode(entry, nameIndex, nodes)
		if nodeID == "" {
			unmatched++
			continue
		}
		matched++

		avgMs := entry.DurationMs
		if entry.CallCount > 0 {
			avgMs = entry.DurationMs / float64(entry.CallCount)
		}

		if _, err := stmt.Exec(nodeID, entry.CallCount, entry.DurationMs, avgMs, entry.Source, now); err != nil {
			return matched, unmatched, fmt.Errorf("insert trace: %w", err)
		}
	}

	return matched, unmatched, tx.Commit()
}

// matchEntryToNode maps a trace entry to a graph node ID.
func matchEntryToNode(entry TraceEntry, nameIndex map[string][]nodeInfo, allNodes []nodeInfo) string {
	lower := strings.ToLower(entry.Function)

	// Exact name match.
	if candidates, ok := nameIndex[lower]; ok {
		if len(candidates) == 1 {
			return candidates[0].id
		}
		// Multiple matches — prefer file path match.
		if entry.File != "" {
			for _, c := range candidates {
				if strings.Contains(c.file, entry.File) || strings.Contains(entry.File, c.file) {
					return c.id
				}
			}
		}
		return candidates[0].id
	}

	// Strip common prefixes/suffixes for partial matching.
	// e.g., "HandleGetUser" → try "GetUser", "handlegetuser"
	for _, n := range allNodes {
		if strings.EqualFold(n.name, entry.Function) {
			return n.id
		}
		if entry.File != "" && strings.Contains(n.file, filepath.Base(entry.File)) {
			if strings.Contains(strings.ToLower(n.name), lower) || strings.Contains(lower, strings.ToLower(n.name)) {
				return n.id
			}
		}
	}

	return ""
}

// ── Query Functions ──────────────────────────────────────────────────────────

// Hotpaths returns the most frequently called nodes, sorted by call count.
func Hotpaths(db *sql.DB, limit int) ([]TraceStats, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := db.Query(`
		SELECT t.node_id, n.name, n.file_path,
			SUM(t.call_count), SUM(t.total_duration_ms),
			CASE WHEN SUM(t.call_count) > 0
				THEN SUM(t.total_duration_ms) / SUM(t.call_count) ELSE 0 END,
			t.source
		FROM traces t
		JOIN nodes n ON t.node_id = n.id
		GROUP BY t.node_id
		ORDER BY SUM(t.call_count) DESC
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTraceStats(rows)
}

// ColdPaths returns structurally important nodes (high reverse-dep count)
// that have zero or very low runtime call counts.
func ColdPaths(db *sql.DB, limit int) ([]TraceStats, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := db.Query(`
		SELECT n.id, n.name, n.file_path,
			COALESCE(SUM(t.call_count), 0) as calls,
			COALESCE(SUM(t.total_duration_ms), 0),
			0, ''
		FROM nodes n
		LEFT JOIN traces t ON n.id = t.node_id
		WHERE n.type IN ('function', 'method')
		AND n.id IN (
			SELECT DISTINCT to_id FROM edges WHERE type = 'imports'
		)
		GROUP BY n.id
		HAVING calls = 0
		ORDER BY (
			SELECT COUNT(*) FROM edges e WHERE e.to_id = n.id
		) DESC
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTraceStats(rows)
}

// NodeTraceStats returns runtime stats for a specific node.
func NodeTraceStats(db *sql.DB, nodeID string) (*TraceStats, error) {
	row := db.QueryRow(`
		SELECT t.node_id, n.name, n.file_path,
			SUM(t.call_count), SUM(t.total_duration_ms),
			CASE WHEN SUM(t.call_count) > 0
				THEN SUM(t.total_duration_ms) / SUM(t.call_count) ELSE 0 END,
			GROUP_CONCAT(DISTINCT t.source)
		FROM traces t
		JOIN nodes n ON t.node_id = n.id
		WHERE t.node_id = ?
		GROUP BY t.node_id
	`, nodeID)

	var s TraceStats
	if err := row.Scan(&s.NodeID, &s.Name, &s.FilePath, &s.CallCount,
		&s.TotalDuration, &s.AvgDuration, &s.Source); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return &s, nil
}

// TraceSummary returns total trace count and unique nodes traced.
func TraceSummary(db *sql.DB) (totalCalls int, uniqueNodes int, err error) {
	err = db.QueryRow(`
		SELECT COALESCE(SUM(call_count), 0), COUNT(DISTINCT node_id)
		FROM traces
	`).Scan(&totalCalls, &uniqueNodes)
	return
}

func scanTraceStats(rows *sql.Rows) ([]TraceStats, error) {
	var stats []TraceStats
	for rows.Next() {
		var s TraceStats
		if err := rows.Scan(&s.NodeID, &s.Name, &s.FilePath,
			&s.CallCount, &s.TotalDuration, &s.AvgDuration, &s.Source); err != nil {
			return nil, err
		}
		stats = append(stats, s)
	}
	return stats, nil
}

// ── Weighted Impact ──────────────────────────────────────────────────────────

// WeightedImpact combines structural BFS depth with runtime call frequency
// to produce a runtime-aware impact score for each affected node.
// Nodes with high call counts at shallow depths get the highest weight.
func WeightedImpact(db *sql.DB, bfsResults map[string]int) ([]WeightedNode, error) {
	if len(bfsResults) == 0 {
		return nil, nil
	}

	// Build node ID list for batch query.
	ids := make([]string, 0, len(bfsResults))
	for id := range bfsResults {
		ids = append(ids, id)
	}

	// Query trace data for all affected nodes.
	traceMap := make(map[string]*TraceStats)
	placeholders := strings.Repeat("?,", len(ids))
	placeholders = placeholders[:len(placeholders)-1]

	args := make([]interface{}, len(ids))
	for i, id := range ids {
		args[i] = id
	}

	rows, err := db.Query(fmt.Sprintf(`
		SELECT t.node_id, n.name, n.file_path,
			SUM(t.call_count), SUM(t.total_duration_ms),
			CASE WHEN SUM(t.call_count) > 0
				THEN SUM(t.total_duration_ms) / SUM(t.call_count) ELSE 0 END
		FROM traces t
		JOIN nodes n ON t.node_id = n.id
		WHERE t.node_id IN (%s)
		GROUP BY t.node_id
	`, placeholders), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var s TraceStats
		if err := rows.Scan(&s.NodeID, &s.Name, &s.FilePath,
			&s.CallCount, &s.TotalDuration, &s.AvgDuration); err != nil {
			continue
		}
		traceMap[s.NodeID] = &s
	}

	// Also get node names for nodes without trace data.
	nodeNames := make(map[string]struct{ name, file string })
	rows2, err := db.Query(fmt.Sprintf(`
		SELECT id, name, file_path FROM nodes WHERE id IN (%s)
	`, placeholders), args...)
	if err != nil {
		return nil, err
	}
	defer rows2.Close()
	for rows2.Next() {
		var id, name, file string
		if rows2.Scan(&id, &name, &file) == nil {
			nodeNames[id] = struct{ name, file string }{name, file}
		}
	}

	// Build weighted results.
	var results []WeightedNode
	for nodeID, depth := range bfsResults {
		wn := WeightedNode{
			NodeID:      nodeID,
			StructDepth: depth,
		}

		if info, ok := nodeNames[nodeID]; ok {
			wn.Name = info.name
			wn.FilePath = info.file
		}

		if ts, ok := traceMap[nodeID]; ok {
			wn.CallCount = ts.CallCount
			wn.AvgDurationMs = ts.AvgDuration
			// Weight formula: calls / (depth + 1)
			// Hot + close = highest weight. Cold + far = lowest.
			wn.RuntimeWeight = float64(ts.CallCount) / float64(depth+1)
		}
		// Nodes with no trace data get weight 0 (unknown runtime behavior).

		results = append(results, wn)
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].RuntimeWeight > results[j].RuntimeWeight
	})

	return results, nil
}
