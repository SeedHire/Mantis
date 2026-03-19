package intel

import "testing"

func makeStats() *TemporalStats {
	return &TemporalStats{
		Files: []FileChurn{
			{Path: "cmd/main.go", Commits: 10, Authors: 3, AuthorNames: []string{"alice", "bob", "carol"}, LinesAdded: 200, LinesDeleted: 100, ChurnScore: 30.0, DaysSinceChange: 1, LastAuthor: "alice"},
			{Path: "internal/handler.go", Commits: 5, Authors: 1, AuthorNames: []string{"alice"}, LinesAdded: 80, LinesDeleted: 20, ChurnScore: 20.0, DaysSinceChange: 3, LastAuthor: "alice"},
			{Path: "internal/db.go", Commits: 8, Authors: 2, AuthorNames: []string{"bob", "carol"}, LinesAdded: 150, LinesDeleted: 50, ChurnScore: 25.0, DaysSinceChange: 2, LastAuthor: "bob"},
			{Path: "pkg/util.go", Commits: 2, Authors: 1, AuthorNames: []string{"bob"}, LinesAdded: 10, LinesDeleted: 5, ChurnScore: 7.5, DaysSinceChange: 10, LastAuthor: "bob"},
			{Path: "internal/router.go", Commits: 4, Authors: 1, AuthorNames: []string{"carol"}, LinesAdded: 60, LinesDeleted: 30, ChurnScore: 22.5, DaysSinceChange: 5, LastAuthor: "carol"},
		},
		Coupling: []CoupledFile{
			{FileA: "cmd/main.go", FileB: "internal/handler.go", CoChanges: 4, Coupling: 0.8},
			{FileA: "internal/db.go", FileB: "internal/handler.go", CoChanges: 3, Coupling: 0.6},
			{FileA: "cmd/main.go", FileB: "internal/db.go", CoChanges: 2, Coupling: 0.25},
		},
	}
}

func TestHotspots(t *testing.T) {
	stats := makeStats()
	top := Hotspots(stats, 3)
	if len(top) != 3 {
		t.Fatalf("expected 3 hotspots, got %d", len(top))
	}
	// cmd/main.go has highest churn*authors (30*3=90)
	if top[0].Path != "cmd/main.go" {
		t.Errorf("top hotspot = %q, want cmd/main.go", top[0].Path)
	}
}

func TestHotspotsNoLimit(t *testing.T) {
	stats := makeStats()
	all := Hotspots(stats, 0)
	if len(all) != 5 {
		t.Errorf("expected all 5 files, got %d", len(all))
	}
}

func TestHotspotsLargeLimit(t *testing.T) {
	stats := makeStats()
	all := Hotspots(stats, 100)
	if len(all) != 5 {
		t.Errorf("expected 5 files with limit=100, got %d", len(all))
	}
}

func TestRisky(t *testing.T) {
	stats := makeStats()
	risky := Risky(stats, 10)

	// Risky = ≥3 commits, ≤1 author
	// handler.go: 5 commits, 1 author ✓
	// router.go: 4 commits, 1 author ✓
	// util.go: 2 commits — excluded (< 3)
	if len(risky) != 2 {
		t.Fatalf("expected 2 risky files, got %d", len(risky))
	}

	// handler.go has higher churn (22.5 vs 20.0... wait actually router=22.5, handler=20)
	if risky[0].Path != "internal/router.go" {
		t.Errorf("top risky = %q, want internal/router.go", risky[0].Path)
	}
}

func TestRiskyEmpty(t *testing.T) {
	stats := &TemporalStats{
		Files: []FileChurn{
			{Path: "a.go", Commits: 10, Authors: 3},
			{Path: "b.go", Commits: 1, Authors: 1},
		},
	}
	risky := Risky(stats, 10)
	if len(risky) != 0 {
		t.Errorf("expected 0 risky files, got %d", len(risky))
	}
}

func TestCouplingFor(t *testing.T) {
	stats := makeStats()

	coupled := CouplingFor(stats, "cmd/main.go", 10)
	if len(coupled) != 2 {
		t.Fatalf("expected 2 coupled files for cmd/main.go, got %d", len(coupled))
	}
	// Highest coupling first (0.8 with handler.go)
	if coupled[0].Coupling < coupled[1].Coupling {
		t.Error("coupled files should be sorted by coupling descending")
	}
}

func TestCouplingForLimit(t *testing.T) {
	stats := makeStats()
	coupled := CouplingFor(stats, "cmd/main.go", 1)
	if len(coupled) != 1 {
		t.Fatalf("expected 1 coupled file with limit=1, got %d", len(coupled))
	}
	if coupled[0].FileB != "internal/handler.go" {
		t.Errorf("top coupled = %q, want internal/handler.go", coupled[0].FileB)
	}
}

func TestCouplingForUnknownPath(t *testing.T) {
	stats := makeStats()
	coupled := CouplingFor(stats, "nonexistent.go", 10)
	if len(coupled) != 0 {
		t.Errorf("expected 0 coupled files for unknown path, got %d", len(coupled))
	}
}

func TestHotspotsEmptyStats(t *testing.T) {
	stats := &TemporalStats{}
	top := Hotspots(stats, 5)
	if len(top) != 0 {
		t.Errorf("expected 0 hotspots for empty stats, got %d", len(top))
	}
}

func TestCouplingForBothDirections(t *testing.T) {
	stats := makeStats()
	// internal/handler.go appears as both FileA and FileB in different entries
	coupled := CouplingFor(stats, "internal/handler.go", 10)
	if len(coupled) != 2 {
		t.Fatalf("expected 2 coupled files for internal/handler.go, got %d", len(coupled))
	}
}
