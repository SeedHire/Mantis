package intel

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func TestParseCustomJSON(t *testing.T) {
	data := []byte(`[
		{"function": "Handler", "file": "api.go", "calls": 150, "duration_ms": 23.5},
		{"function": "GetUser", "file": "users.go", "calls": 80, "duration_ms": 12.0}
	]`)

	entries, err := parseCustomJSON(data)
	if err != nil {
		t.Fatalf("parseCustomJSON: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
	if entries[0].Function != "Handler" {
		t.Errorf("first function = %q, want Handler", entries[0].Function)
	}
	if entries[0].CallCount != 150 {
		t.Errorf("first calls = %d, want 150", entries[0].CallCount)
	}
	if entries[0].Source != "custom" {
		t.Errorf("source = %q, want custom", entries[0].Source)
	}
}

func TestParseCustomJSONMissingFunction(t *testing.T) {
	data := []byte(`[{"calls": 10, "duration_ms": 1.0}]`)
	_, err := parseCustomJSON(data)
	if err == nil {
		t.Error("expected error for missing function name")
	}
}

func TestParseOTLP(t *testing.T) {
	data := []byte(`{
		"resourceSpans": [{
			"scopeSpans": [{
				"spans": [
					{
						"name": "GET /api/users",
						"startTimeUnixNano": "1000000000",
						"endTimeUnixNano":   "1005000000",
						"attributes": []
					},
					{
						"name": "GET /api/users",
						"startTimeUnixNano": "2000000000",
						"endTimeUnixNano":   "2003000000",
						"attributes": []
					},
					{
						"name": "POST /api/login",
						"startTimeUnixNano": "3000000000",
						"endTimeUnixNano":   "3010000000",
						"attributes": [
							{"key": "code.filepath", "value": {"stringValue": "auth/handler.go"}}
						]
					}
				]
			}]
		}]
	}`)

	entries, err := parseOTLP(data)
	if err != nil {
		t.Fatalf("parseOTLP: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 aggregated entries, got %d", len(entries))
	}

	// Find GET /api/users entry.
	var usersEntry *TraceEntry
	for i := range entries {
		if entries[i].Function == "GET /api/users" {
			usersEntry = &entries[i]
			break
		}
	}
	if usersEntry == nil {
		t.Fatal("missing GET /api/users entry")
	}
	if usersEntry.CallCount != 2 {
		t.Errorf("users calls = %d, want 2", usersEntry.CallCount)
	}
	if usersEntry.Source != "otlp" {
		t.Errorf("source = %q, want otlp", usersEntry.Source)
	}
}

func TestParsePprofText(t *testing.T) {
	content := `
      flat  flat%   sum%        cum   cum%
     120ms  2.40%  2.40%      500ms 10.00%  main.handler
      80ms  1.60%  4.00%      200ms  4.00%  runtime.mallocgc
`
	entries, err := parsePprofText(content)
	if err != nil {
		t.Fatalf("parsePprofText: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
	if entries[0].Function != "handler" {
		t.Errorf("first function = %q, want handler", entries[0].Function)
	}
	if entries[0].Source != "pprof" {
		t.Errorf("source = %q, want pprof", entries[0].Source)
	}
}

func TestParsePprofDuration(t *testing.T) {
	tests := []struct {
		input string
		want  float64
	}{
		{"120ms", 120.0},
		{"1.5s", 1500.0},
		{"500us", 0.5},
		{"42", 42.0},
	}
	for _, tt := range tests {
		got := parsePprofDuration(tt.input)
		if got != tt.want {
			t.Errorf("parsePprofDuration(%q) = %f, want %f", tt.input, got, tt.want)
		}
	}
}

func TestIngestTraceFileCustomJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "traces.json")
	data := `[{"function":"Foo","calls":10,"duration_ms":5.0}]`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	entries, err := IngestTraceFile(path)
	if err != nil {
		t.Fatalf("IngestTraceFile: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
}

func TestIngestTraceFileEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.json")
	if err := os.WriteFile(path, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := IngestTraceFile(path)
	if err == nil {
		t.Error("expected error for empty file")
	}
}

func TestIngestTraceFileMissing(t *testing.T) {
	_, err := IngestTraceFile("/nonexistent/file.json")
	if err == nil {
		t.Error("expected error for missing file")
	}
}

func TestStoreAndQueryTraces(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	// Insert test nodes.
	_, err := db.Exec(`INSERT INTO nodes (id, type, name, file_path) VALUES
		('func:Handler:api.go', 'function', 'Handler', 'api.go'),
		('func:GetUser:users.go', 'function', 'GetUser', 'users.go')`)
	if err != nil {
		t.Fatal(err)
	}

	entries := []TraceEntry{
		{Function: "Handler", File: "api.go", CallCount: 100, DurationMs: 500, Source: "custom"},
		{Function: "GetUser", File: "users.go", CallCount: 50, DurationMs: 200, Source: "custom"},
		{Function: "Unknown", File: "", CallCount: 10, DurationMs: 5, Source: "custom"},
	}

	matched, unmatched, err := StoreTraces(db, entries)
	if err != nil {
		t.Fatalf("StoreTraces: %v", err)
	}
	if matched != 2 {
		t.Errorf("matched = %d, want 2", matched)
	}
	if unmatched != 1 {
		t.Errorf("unmatched = %d, want 1", unmatched)
	}

	// Test Hotpaths.
	stats, err := Hotpaths(db, 10)
	if err != nil {
		t.Fatalf("Hotpaths: %v", err)
	}
	if len(stats) != 2 {
		t.Fatalf("expected 2 hotpaths, got %d", len(stats))
	}
	if stats[0].CallCount != 100 {
		t.Errorf("top hotpath calls = %d, want 100", stats[0].CallCount)
	}

	// Test TraceSummary.
	totalCalls, uniqueNodes, err := TraceSummary(db)
	if err != nil {
		t.Fatalf("TraceSummary: %v", err)
	}
	if totalCalls != 150 {
		t.Errorf("totalCalls = %d, want 150", totalCalls)
	}
	if uniqueNodes != 2 {
		t.Errorf("uniqueNodes = %d, want 2", uniqueNodes)
	}

	// Test NodeTraceStats.
	ns, err := NodeTraceStats(db, "func:Handler:api.go")
	if err != nil {
		t.Fatalf("NodeTraceStats: %v", err)
	}
	if ns == nil {
		t.Fatal("expected trace stats for Handler")
	}
	if ns.CallCount != 100 {
		t.Errorf("Handler calls = %d, want 100", ns.CallCount)
	}
}

func TestColdPaths(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	// Create nodes and edges — GetUser is imported but has no trace data.
	_, err := db.Exec(`
		INSERT INTO nodes (id, type, name, file_path) VALUES
			('func:Handler:api.go', 'function', 'Handler', 'api.go'),
			('func:GetUser:users.go', 'function', 'GetUser', 'users.go');
		INSERT INTO edges (from_id, to_id, type) VALUES
			('func:Handler:api.go', 'func:GetUser:users.go', 'imports');
	`)
	if err != nil {
		t.Fatal(err)
	}

	stats, err := ColdPaths(db, 10)
	if err != nil {
		t.Fatalf("ColdPaths: %v", err)
	}
	if len(stats) != 1 {
		t.Fatalf("expected 1 cold path, got %d", len(stats))
	}
	if stats[0].Name != "GetUser" {
		t.Errorf("cold path = %q, want GetUser", stats[0].Name)
	}
}

func TestWeightedImpact(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	_, err := db.Exec(`
		INSERT INTO nodes (id, type, name, file_path) VALUES
			('func:A:a.go', 'function', 'A', 'a.go'),
			('func:B:b.go', 'function', 'B', 'b.go'),
			('func:C:c.go', 'function', 'C', 'c.go');
		INSERT INTO traces (node_id, call_count, total_duration_ms, avg_duration_ms, source)
		VALUES
			('func:A:a.go', 1000, 5000, 5.0, 'custom'),
			('func:B:b.go', 10, 100, 10.0, 'custom');
	`)
	if err != nil {
		t.Fatal(err)
	}

	bfs := map[string]int{
		"func:A:a.go": 1, // depth 1
		"func:B:b.go": 2, // depth 2
		"func:C:c.go": 3, // depth 3, no trace data
	}

	results, err := WeightedImpact(db, bfs)
	if err != nil {
		t.Fatalf("WeightedImpact: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}

	// A should be first (1000 calls / depth 2 = 500).
	if results[0].Name != "A" {
		t.Errorf("top weighted = %q, want A", results[0].Name)
	}
	if results[0].RuntimeWeight != 500.0 {
		t.Errorf("A weight = %f, want 500.0", results[0].RuntimeWeight)
	}

	// C should be last (no trace data, weight 0).
	if results[2].RuntimeWeight != 0 {
		t.Errorf("C weight = %f, want 0", results[2].RuntimeWeight)
	}
}

func TestMatchEntryToNode(t *testing.T) {
	nodes := []nodeInfo{
		{id: "func:Handler:api.go", name: "Handler", file: "api.go"},
		{id: "func:Handler:auth.go", name: "Handler", file: "auth.go"},
		{id: "func:GetUser:users.go", name: "GetUser", file: "users.go"},
	}
	nameIndex := make(map[string][]nodeInfo)
	for _, n := range nodes {
		lower := strings.ToLower(n.name)
		nameIndex[lower] = append(nameIndex[lower], n)
	}

	// Exact match with file disambiguation.
	entry := TraceEntry{Function: "Handler", File: "auth.go"}
	got := matchEntryToNode(entry, nameIndex, nodes)
	if got != "func:Handler:auth.go" {
		t.Errorf("match with file = %q, want func:Handler:auth.go", got)
	}

	// Unique name match.
	entry2 := TraceEntry{Function: "GetUser"}
	got2 := matchEntryToNode(entry2, nameIndex, nodes)
	if got2 != "func:GetUser:users.go" {
		t.Errorf("unique match = %q, want func:GetUser:users.go", got2)
	}

	// No match.
	entry3 := TraceEntry{Function: "NonExistent"}
	got3 := matchEntryToNode(entry3, nameIndex, nodes)
	if got3 != "" {
		t.Errorf("unmatched = %q, want empty", got3)
	}
}

// setupTestDB creates an in-memory SQLite database with the graph + trace schema.
func setupTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	schema := `
	CREATE TABLE nodes (
		id TEXT PRIMARY KEY, type TEXT, name TEXT, file_path TEXT,
		line_start INTEGER DEFAULT 0, line_end INTEGER DEFAULT 0,
		complexity INTEGER DEFAULT 0, exported INTEGER DEFAULT 0,
		language TEXT DEFAULT '', last_modified INTEGER DEFAULT 0
	);
	CREATE TABLE edges (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		from_id TEXT, to_id TEXT, type TEXT, metadata TEXT DEFAULT '',
		UNIQUE(from_id, to_id, type)
	);
	CREATE TABLE traces (
		node_id TEXT NOT NULL, call_count INTEGER DEFAULT 0,
		total_duration_ms REAL DEFAULT 0, avg_duration_ms REAL DEFAULT 0,
		source TEXT DEFAULT '', last_ingested INTEGER DEFAULT 0,
		UNIQUE(node_id, source)
	);
	CREATE INDEX idx_traces_node ON traces(node_id);
	`
	if _, err := db.Exec(schema); err != nil {
		t.Fatal(err)
	}
	return db
}
