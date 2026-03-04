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
