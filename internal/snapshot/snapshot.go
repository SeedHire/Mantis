package snapshot

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Entry represents one saved snapshot.
type Entry struct {
	ID        string    `json:"id"`        // short unique ID (timestamp-based)
	Label     string    `json:"label"`     // human-readable label (e.g. "before pipeline")
	Timestamp time.Time `json:"timestamp"` // when the snapshot was taken
	StashRef  string    `json:"stash_ref"` // git stash ref at creation time (e.g. "stash@{0}")
	Auto      bool      `json:"auto"`      // true if auto-created by mantis
}

// Store manages git stash-based snapshots for a project.
// Thread-safe: all public methods are mutex-guarded.
type Store struct {
	mu       sync.Mutex
	root     string // git repo root
	logPath  string // .mantis/snapshots.json
	entries  []Entry
}

// NewStore creates or loads the snapshot store for a project.
func NewStore(projectRoot string) *Store {
	mantisDir := filepath.Join(projectRoot, ".mantis")
	s := &Store{
		root:    projectRoot,
		logPath: filepath.Join(mantisDir, "snapshots.json"),
	}
	if data, err := os.ReadFile(s.logPath); err == nil {
		_ = json.Unmarshal(data, &s.entries)
	}
	return s
}

// Take creates a new snapshot of all uncommitted changes.
// Uses `git stash push` with a mantis-prefixed message so we can identify our stashes.
// If there are no changes to stash, returns ("", nil) — not an error.
func (s *Store) Take(label string, auto bool) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Check if there are any changes to snapshot.
	if !s.hasChanges() {
		return "", nil // nothing to snapshot
	}

	id := fmt.Sprintf("snap-%d", time.Now().UnixMilli())
	stashMsg := fmt.Sprintf("mantis:%s:%s", id, label)

	// git stash push --include-untracked -m "mantis:snap-xxx:label"
	cmd := exec.Command("git", "-C", s.root, "stash", "push", "--include-untracked", "-m", stashMsg)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git stash failed: %s (%w)", strings.TrimSpace(string(out)), err)
	}

	// If git says "No local changes to save", nothing was stashed.
	if strings.Contains(string(out), "No local changes") {
		return "", nil
	}

	// Immediately pop the stash to restore the working tree — we only wanted the ref.
	// The stash entry remains in the reflog even after pop fails, so this is safe.
	popCmd := exec.Command("git", "-C", s.root, "stash", "pop", "--quiet")
	if popOut, popErr := popCmd.CombinedOutput(); popErr != nil {
		// If pop fails (conflict), try to apply instead and drop later.
		applyCmd := exec.Command("git", "-C", s.root, "stash", "apply", "--quiet")
		if _, applyErr := applyCmd.CombinedOutput(); applyErr != nil {
			return "", fmt.Errorf("could not restore working tree after snapshot: %s (%w)",
				strings.TrimSpace(string(popOut)), popErr)
		}
	}

	// Find the stash ref we just created. After pop, the stash moved —
	// but we need the reflog entry. Re-stash to keep a persistent ref.
	// Actually, simpler approach: re-push and keep it stashed this time.
	// But that changes the working tree. Instead, use a different strategy:
	// push + immediately apply (not pop) so the stash stays AND working tree is restored.

	// The above pop already happened. Let's re-push to keep a durable ref.
	cmd2 := exec.Command("git", "-C", s.root, "stash", "push", "--include-untracked", "-m", stashMsg)
	out2, err2 := cmd2.CombinedOutput()
	if err2 != nil || strings.Contains(string(out2), "No local changes") {
		// Edge case: pop restored everything, re-push sees no changes.
		// The snapshot still exists in the reflog. Find it by message.
		stashRef := s.findStashByMessage(stashMsg)
		entry := Entry{
			ID:        id,
			Label:     label,
			Timestamp: time.Now(),
			StashRef:  stashRef,
			Auto:      auto,
		}
		s.entries = append(s.entries, entry)
		s.flush()
		return id, nil
	}

	// Apply the stash to restore working tree (keep stash ref alive).
	applyCmd := exec.Command("git", "-C", s.root, "stash", "apply", "--quiet")
	applyCmd.Dir = s.root
	_, _ = applyCmd.CombinedOutput() // best-effort

	stashRef := s.findStashByMessage(stashMsg)
	entry := Entry{
		ID:        id,
		Label:     label,
		Timestamp: time.Now(),
		StashRef:  stashRef,
		Auto:      auto,
	}
	s.entries = append(s.entries, entry)
	s.flush()
	return id, nil
}

// Restore reverts the working tree to a snapshot's state.
// Returns a diff summary of what changed.
func (s *Store) Restore(id string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entry := s.findByID(id)
	if entry == nil {
		return "", fmt.Errorf("snapshot %q not found", id)
	}

	// Find the current stash index for this entry's message.
	stashMsg := fmt.Sprintf("mantis:%s:%s", entry.ID, entry.Label)
	ref := s.findStashByMessage(stashMsg)
	if ref == "" {
		return "", fmt.Errorf("stash entry for snapshot %q has been garbage collected", id)
	}

	// Get diff preview before restoring.
	diffCmd := exec.Command("git", "-C", s.root, "stash", "show", "--stat", ref)
	diffOut, _ := diffCmd.CombinedOutput()

	// Reset working tree to clean state, then apply the snapshot.
	// First stash current changes (so we don't lose them permanently).
	backupCmd := exec.Command("git", "-C", s.root, "stash", "push", "--include-untracked",
		"-m", fmt.Sprintf("mantis:pre-revert:%s", id))
	_, _ = backupCmd.CombinedOutput()

	// Now apply the target snapshot.
	applyCmd := exec.Command("git", "-C", s.root, "stash", "apply", ref)
	applyOut, err := applyCmd.CombinedOutput()
	if err != nil {
		// Restore the backup if apply failed.
		popCmd := exec.Command("git", "-C", s.root, "stash", "pop", "--quiet")
		_, _ = popCmd.CombinedOutput()
		return "", fmt.Errorf("restore failed: %s (%w)", strings.TrimSpace(string(applyOut)), err)
	}

	return strings.TrimSpace(string(diffOut)), nil
}

// List returns all snapshots, newest first.
func (s *Store) List() []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Return newest first.
	result := make([]Entry, len(s.entries))
	for i, e := range s.entries {
		result[len(s.entries)-1-i] = e
	}
	return result
}

// Len returns the number of stored snapshots.
func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

// Prune removes snapshot entries older than maxAge and cleans up their stash refs.
func (s *Store) Prune(maxAge time.Duration) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	cutoff := time.Now().Add(-maxAge)
	var kept []Entry
	pruned := 0
	for _, e := range s.entries {
		if e.Timestamp.Before(cutoff) {
			// Try to drop the stash ref.
			stashMsg := fmt.Sprintf("mantis:%s:%s", e.ID, e.Label)
			ref := s.findStashByMessage(stashMsg)
			if ref != "" {
				dropCmd := exec.Command("git", "-C", s.root, "stash", "drop", ref)
				_, _ = dropCmd.CombinedOutput()
			}
			pruned++
		} else {
			kept = append(kept, e)
		}
	}
	s.entries = kept
	s.flush()
	return pruned
}

// hasChanges returns true if there are uncommitted changes in the working tree.
func (s *Store) hasChanges() bool {
	cmd := exec.Command("git", "-C", s.root, "status", "--porcelain")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) != ""
}

// findStashByMessage searches `git stash list` for a stash with the given message.
func (s *Store) findStashByMessage(msg string) string {
	cmd := exec.Command("git", "-C", s.root, "stash", "list")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		// Line format: "stash@{N}: On branch: message"
		// Use exact suffix match to avoid collisions (e.g. "backup" matching "backup-2").
		trimmed := strings.TrimSpace(line)
		if strings.HasSuffix(trimmed, ": "+msg) || strings.HasSuffix(trimmed, " "+msg) {
			parts := strings.SplitN(trimmed, ":", 2)
			if len(parts) >= 1 {
				return strings.TrimSpace(parts[0])
			}
		}
	}
	return ""
}

// findByID returns the entry with the given ID, or nil.
func (s *Store) findByID(id string) *Entry {
	for i := range s.entries {
		if s.entries[i].ID == id {
			return &s.entries[i]
		}
	}
	return nil
}

// flush persists the entry list to disk.
func (s *Store) flush() {
	data, err := json.MarshalIndent(s.entries, "", "  ")
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(s.logPath), 0o755)
	_ = os.WriteFile(s.logPath, data, 0o644)
}
