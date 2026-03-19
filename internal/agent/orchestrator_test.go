package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/seedhire/mantis/internal/graph"
	"github.com/seedhire/mantis/internal/intel"
)

// ── collectPackages ────────────────────────────────────────────────────────────

func TestCollectPackages_DedupsDirectories(t *testing.T) {
	// Two nodes in the same directory should yield only one package entry.
	impact := &intel.ImpactResult{
		ByDepth: map[int][]*graph.Node{
			0: {
				{FilePath: "internal/auth/auth.go"},
				{FilePath: "internal/auth/jwt.go"}, // same dir — should deduplicate
			},
			1: {
				{FilePath: "internal/session/session.go"},
			},
		},
	}

	pkgs := collectPackages(impact)
	if len(pkgs) != 2 {
		t.Errorf("expected 2 unique packages, got %d: %v", len(pkgs), pkgs)
	}

	seen := map[string]int{}
	for _, p := range pkgs {
		seen[p]++
	}
	for p, count := range seen {
		if count > 1 {
			t.Errorf("directory %q appears %d times (should be 1)", p, count)
		}
	}
}

func TestCollectPackages_Nil(t *testing.T) {
	// nil ImpactResult should not panic; collectPackages guards on ByDepth iteration.
	// We pass an empty result instead of nil since collectPackages dereferences impact.
	impact := &intel.ImpactResult{ByDepth: nil}
	pkgs := collectPackages(impact)
	if pkgs != nil && len(pkgs) != 0 {
		t.Errorf("expected empty result for nil ByDepth, got %v", pkgs)
	}
}

func TestCollectPackages_MultiDepth(t *testing.T) {
	impact := &intel.ImpactResult{
		ByDepth: map[int][]*graph.Node{
			0: {{FilePath: "internal/router/router.go"}},
			1: {{FilePath: "internal/embeddings/embeddings.go"}},
			2: {{FilePath: "internal/ollama/client.go"}},
		},
	}
	pkgs := collectPackages(impact)
	if len(pkgs) != 3 {
		t.Errorf("expected 3 packages from 3 depths, got %d: %v", len(pkgs), pkgs)
	}
}

// ── writeScratch ───────────────────────────────────────────────────────────────

func TestWriteScratch_EmptyDir(t *testing.T) {
	// dir="" must be a no-op — no panic, no file creation
	scratch := &agentScratch{Task: "test task", Workers: map[string]WorkerResult{}}
	writeScratch("", scratch) // must not panic
}

func TestWriteScratch_CreatesFile(t *testing.T) {
	dir := t.TempDir()
	scratch := &agentScratch{
		Task: "add login feature",
		Workers: map[string]WorkerResult{
			"internal/auth": {
				Package: "internal/auth",
				Summary: "Implemented JWT auth",
				Code:    "func Login() {}",
			},
		},
	}

	writeScratch(dir, scratch)

	path := filepath.Join(dir, scratchFile)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Fatalf("expected %s to be created, but it doesn't exist", path)
	}
}

func TestWriteScratch_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	scratch := &agentScratch{
		Task: "refactor payment module",
		Workers: map[string]WorkerResult{
			"internal/payment": {
				Package: "internal/payment",
				Summary: "Refactored payment handler",
				Code:    "func ProcessPayment() {}",
			},
		},
	}

	writeScratch(dir, scratch)

	// Read back and verify contents.
	data, err := os.ReadFile(filepath.Join(dir, scratchFile))
	if err != nil {
		t.Fatalf("failed to read scratch file: %v", err)
	}

	var loaded agentScratch
	if err := json.Unmarshal(data, &loaded); err != nil {
		t.Fatalf("failed to parse scratch JSON: %v", err)
	}

	if loaded.Task != scratch.Task {
		t.Errorf("Task = %q, want %q", loaded.Task, scratch.Task)
	}
	wr, ok := loaded.Workers["internal/payment"]
	if !ok {
		t.Fatal("expected worker entry for internal/payment")
	}
	if wr.Summary != "Refactored payment handler" {
		t.Errorf("Summary = %q, want %q", wr.Summary, "Refactored payment handler")
	}
}

func TestWriteScratch_NilScratch(t *testing.T) {
	// nil scratch should fail gracefully (json.MarshalIndent returns null, writes "null")
	dir := t.TempDir()
	writeScratch(dir, nil) // must not panic
}

// ── topoSort ───────────────────────────────────────────────────────────────────

func TestTopoSort_NoDependencies(t *testing.T) {
	decomp := map[string]string{
		"pkg/a": "task a",
		"pkg/b": "task b",
		"pkg/c": "task c",
	}
	dag := map[string][]string{} // no edges

	levels := topoSort(decomp, dag)
	if len(levels) != 1 {
		t.Errorf("no dependencies: expected 1 level (all parallel), got %d levels", len(levels))
	}
	if len(levels[0]) != 3 {
		t.Errorf("expected 3 packages in level 0, got %d", len(levels[0]))
	}
}

func TestTopoSort_LinearChain(t *testing.T) {
	// a → b → c  (c depends on b, b depends on a)
	decomp := map[string]string{
		"pkg/a": "task a",
		"pkg/b": "task b",
		"pkg/c": "task c",
	}
	dag := map[string][]string{
		"pkg/b": {"pkg/a"},
		"pkg/c": {"pkg/b"},
	}

	levels := topoSort(decomp, dag)
	if len(levels) != 3 {
		t.Errorf("linear chain: expected 3 levels, got %d: %v", len(levels), levels)
	}
	// Each level should have exactly one package.
	for i, level := range levels {
		if len(level) != 1 {
			t.Errorf("level %d: expected 1 package, got %d: %v", i, len(level), level)
		}
	}
	// Level 0 must be "pkg/a" (no deps).
	if levels[0][0] != "pkg/a" {
		t.Errorf("level 0 should be pkg/a, got %v", levels[0])
	}
}

func TestTopoSort_Diamond(t *testing.T) {
	// a → b, a → c, b → d, c → d
	decomp := map[string]string{
		"pkg/a": "task a",
		"pkg/b": "task b",
		"pkg/c": "task c",
		"pkg/d": "task d",
	}
	dag := map[string][]string{
		"pkg/b": {"pkg/a"},
		"pkg/c": {"pkg/a"},
		"pkg/d": {"pkg/b", "pkg/c"},
	}

	levels := topoSort(decomp, dag)
	// Expect 3 levels: [a], [b,c], [d]
	if len(levels) != 3 {
		t.Errorf("diamond: expected 3 levels, got %d: %v", len(levels), levels)
	}
	if len(levels[0]) != 1 || levels[0][0] != "pkg/a" {
		t.Errorf("level 0 should be [pkg/a], got %v", levels[0])
	}
	if len(levels[1]) != 2 {
		t.Errorf("level 1 should have 2 packages (b and c), got %v", levels[1])
	}
	if len(levels[2]) != 1 || levels[2][0] != "pkg/d" {
		t.Errorf("level 2 should be [pkg/d], got %v", levels[2])
	}
}

func TestTopoSort_CycleDoesNotDeadlock(t *testing.T) {
	// a → b → a (cycle) — should not hang; must return all packages in one level.
	decomp := map[string]string{
		"pkg/a": "task a",
		"pkg/b": "task b",
	}
	dag := map[string][]string{
		"pkg/a": {"pkg/b"},
		"pkg/b": {"pkg/a"},
	}

	// Must complete without hanging.
	done := make(chan [][]string, 1)
	go func() { done <- topoSort(decomp, dag) }()

	select {
	case levels := <-done:
		// Cycle detected: all remaining packages in one level.
		total := 0
		for _, l := range levels {
			total += len(l)
		}
		if total != 2 {
			t.Errorf("cycle: expected all 2 packages returned (across levels), got %d", total)
		}
	}
}

func TestTopoSort_EmptyDecomposition(t *testing.T) {
	levels := topoSort(map[string]string{}, map[string][]string{})
	if len(levels) != 0 {
		t.Errorf("empty decomposition: expected 0 levels, got %d", len(levels))
	}
}

// ── getWorkerMaxIter ────────────────────────────────────────────────────────

func TestGetWorkerMaxIter_Default(t *testing.T) {
	os.Unsetenv("MANTIS_AGENT_MAX_ITER")
	n := getWorkerMaxIter()
	if n != defaultWorkerMaxIter {
		t.Errorf("expected default %d, got %d", defaultWorkerMaxIter, n)
	}
}

func TestGetWorkerMaxIter_EnvOverride(t *testing.T) {
	t.Setenv("MANTIS_AGENT_MAX_ITER", "15")
	n := getWorkerMaxIter()
	if n != 15 {
		t.Errorf("expected 15 from env, got %d", n)
	}
}

func TestGetWorkerMaxIter_InvalidEnv(t *testing.T) {
	t.Setenv("MANTIS_AGENT_MAX_ITER", "abc")
	n := getWorkerMaxIter()
	if n != defaultWorkerMaxIter {
		t.Errorf("invalid env should fall back to default %d, got %d", defaultWorkerMaxIter, n)
	}
}

func TestGetWorkerMaxIter_ZeroEnv(t *testing.T) {
	t.Setenv("MANTIS_AGENT_MAX_ITER", "0")
	n := getWorkerMaxIter()
	if n != defaultWorkerMaxIter {
		t.Errorf("zero env should fall back to default %d, got %d", defaultWorkerMaxIter, n)
	}
}
