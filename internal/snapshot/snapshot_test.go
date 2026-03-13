package snapshot

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// initGitRepo creates a temp directory with a git repo and one committed file.
func initGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test",
			"GIT_AUTHOR_EMAIL=test@test.com",
			"GIT_COMMITTER_NAME=test",
			"GIT_COMMITTER_EMAIL=test@test.com",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v failed: %s (%v)", args, out, err)
		}
	}

	run("init", "-b", "main")
	run("config", "user.email", "test@test.com")
	run("config", "user.name", "test")

	// Create initial file and commit.
	if err := os.WriteFile(filepath.Join(dir, "hello.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-m", "initial")

	// Create .mantis dir.
	if err := os.MkdirAll(filepath.Join(dir, ".mantis"), 0o755); err != nil {
		t.Fatal(err)
	}

	return dir
}

func TestNewStore(t *testing.T) {
	dir := initGitRepo(t)
	s := NewStore(dir)
	if s.Len() != 0 {
		t.Errorf("new store should have 0 entries, got %d", s.Len())
	}
}

func TestTakeNoChanges(t *testing.T) {
	dir := initGitRepo(t)
	s := NewStore(dir)

	id, err := s.Take("test", false)
	if err != nil {
		t.Fatalf("Take with no changes should not error: %v", err)
	}
	if id != "" {
		t.Errorf("Take with no changes should return empty id, got %q", id)
	}
	if s.Len() != 0 {
		t.Errorf("no snapshot should be stored when there are no changes")
	}
}

func TestTakeAndList(t *testing.T) {
	dir := initGitRepo(t)
	s := NewStore(dir)

	// Make a change.
	if err := os.WriteFile(filepath.Join(dir, "hello.go"), []byte("package main\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	id, err := s.Take("before pipeline", true)
	if err != nil {
		t.Fatalf("Take failed: %v", err)
	}
	if id == "" {
		t.Fatal("Take should return a non-empty ID")
	}

	// List should show 1 entry.
	entries := s.List()
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].ID != id {
		t.Errorf("entry ID mismatch: %q != %q", entries[0].ID, id)
	}
	if entries[0].Label != "before pipeline" {
		t.Errorf("entry label mismatch: %q", entries[0].Label)
	}
	if !entries[0].Auto {
		t.Error("entry should be auto=true")
	}

	// Working tree should still have the change (snapshot preserves it).
	content, err := os.ReadFile(filepath.Join(dir, "hello.go"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "package main\nfunc main() {}\n" {
		t.Errorf("working tree should be preserved after snapshot, got: %q", string(content))
	}
}

func TestTakePersistence(t *testing.T) {
	dir := initGitRepo(t)
	s := NewStore(dir)

	// Make a change and take a snapshot.
	if err := os.WriteFile(filepath.Join(dir, "hello.go"), []byte("modified\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := s.Take("persist test", false)
	if err != nil {
		t.Fatalf("Take failed: %v", err)
	}

	// Create a new store instance — should load persisted entries.
	s2 := NewStore(dir)
	if s2.Len() != 1 {
		t.Errorf("persisted store should have 1 entry, got %d", s2.Len())
	}
}

func TestPrune(t *testing.T) {
	dir := initGitRepo(t)
	s := NewStore(dir)

	// Make a change and take a snapshot.
	if err := os.WriteFile(filepath.Join(dir, "hello.go"), []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _ = s.Take("old snap", true)

	// Backdate the entry.
	s.mu.Lock()
	if len(s.entries) > 0 {
		s.entries[0].Timestamp = time.Now().Add(-48 * time.Hour)
	}
	s.flush()
	s.mu.Unlock()

	// Prune entries older than 24h.
	pruned := s.Prune(24 * time.Hour)
	if pruned != 1 {
		t.Errorf("expected 1 pruned, got %d", pruned)
	}
	if s.Len() != 0 {
		t.Errorf("expected 0 entries after prune, got %d", s.Len())
	}
}
