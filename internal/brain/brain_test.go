package brain

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newTestBrain creates a Brain backed by a temp directory with .mantis/ pre-created.
func newTestBrain(t *testing.T) *Brain {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, dirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return &Brain{root: root, dir: dir}
}

// ── UpdateBrain append behaviour ──────────────────────────────────────────────

func TestUpdateBrain_CreatesFileOnFirstCall(t *testing.T) {
	b := newTestBrain(t)
	if err := b.UpdateBrain("First summary"); err != nil {
		t.Fatalf("UpdateBrain failed: %v", err)
	}
	content := b.ReadBrain()
	if content == "" {
		t.Fatal("expected BRAIN.md to be non-empty after first update")
	}
	if !strings.Contains(content, "First summary") {
		t.Error("expected BRAIN.md to contain the summary text")
	}
}

func TestUpdateBrain_AppendsOnSecondCall(t *testing.T) {
	b := newTestBrain(t)
	if err := b.UpdateBrain("Session one"); err != nil {
		t.Fatalf("first UpdateBrain failed: %v", err)
	}
	if err := b.UpdateBrain("Session two"); err != nil {
		t.Fatalf("second UpdateBrain failed: %v", err)
	}
	content := b.ReadBrain()
	if !strings.Contains(content, "Session one") {
		t.Error("expected BRAIN.md to still contain 'Session one' after second update")
	}
	if !strings.Contains(content, "Session two") {
		t.Error("expected BRAIN.md to contain 'Session two'")
	}
}

func TestUpdateBrain_PreservesAllSections(t *testing.T) {
	b := newTestBrain(t)
	summaries := []string{"alpha", "beta", "gamma", "delta", "epsilon"}
	for _, s := range summaries {
		if err := b.UpdateBrain(s); err != nil {
			t.Fatalf("UpdateBrain(%q) failed: %v", s, err)
		}
	}
	content := b.ReadBrain()
	for _, s := range summaries {
		if !strings.Contains(content, s) {
			t.Errorf("BRAIN.md missing session summary %q after %d updates", s, len(summaries))
		}
	}
}

func TestUpdateBrain_AddsConsolidationMarker(t *testing.T) {
	b := newTestBrain(t)
	// Write 10 sessions so the 11th triggers the consolidation marker.
	for i := 0; i < 10; i++ {
		if err := b.UpdateBrain("session content"); err != nil {
			t.Fatalf("UpdateBrain iteration %d failed: %v", i, err)
		}
	}
	if err := b.UpdateBrain("trigger consolidation"); err != nil {
		t.Fatalf("UpdateBrain (consolidation trigger) failed: %v", err)
	}
	content := b.ReadBrain()
	if !strings.Contains(content, "Consolidated checkpoint") {
		t.Error("expected 'Consolidated checkpoint' marker after 10 sessions, but it is missing")
	}
}

func TestUpdateBrain_HeaderOnFirstCall(t *testing.T) {
	b := newTestBrain(t)
	if err := b.UpdateBrain("init summary"); err != nil {
		t.Fatal(err)
	}
	content := b.ReadBrain()
	if !strings.Contains(content, "# BRAIN.md") {
		t.Error("expected '# BRAIN.md' header on first call")
	}
}

func TestUpdateBrain_SessionSectionPresent(t *testing.T) {
	b := newTestBrain(t)
	if err := b.UpdateBrain("timestamped session"); err != nil {
		t.Fatal(err)
	}
	content := b.ReadBrain()
	if !strings.Contains(content, "## Session ") {
		t.Error("expected a '## Session YYYY-MM-DD' heading in BRAIN.md")
	}
}

func TestLoadSkills_UTF8RuneBoundary(t *testing.T) {
	b := newTestBrain(t)
	skillsDir := filepath.Join(b.dir, "skills")
	if err := os.MkdirAll(skillsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Write a skill file containing multi-byte UTF-8 characters (each é is 2 bytes).
	// The content is crafted so a naive byte-slice truncation would split a multi-byte rune.
	content := strings.Repeat("é", 200) // 400 bytes, 200 runes
	if err := os.WriteFile(filepath.Join(skillsDir, "test-skill.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	// Request fewer chars than available — triggers the truncation path.
	// 350 bytes falls in the middle of a 2-byte é character.
	result := b.LoadSkills(350)
	if result == "" {
		t.Fatal("expected non-empty result")
	}

	// The result must be valid UTF-8 — no broken runes.
	for i, r := range result {
		if r == '\uFFFD' {
			t.Errorf("found replacement character (U+FFFD) at byte offset %d — truncation broke a UTF-8 rune", i)
		}
	}
}
