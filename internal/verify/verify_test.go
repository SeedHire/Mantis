package verify

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/seedhire/mantis/internal/truth"
)

func TestCheckCleanWhenNilWriter(t *testing.T) {
	result := Check("some response with Code() calls", nil)
	if !result.Clean {
		t.Error("Check with nil writer should return Clean=true")
	}
}

func TestCheckCleanNoCodeBlocks(t *testing.T) {
	result := Check("This is a text response with no code blocks", nil)
	if !result.Clean {
		t.Error("Check with no code blocks should return Clean=true")
	}
}

func TestParseConventionsEmpty(t *testing.T) {
	convs := ParseConventions("")
	if convs != nil {
		t.Errorf("expected nil for empty content, got %v", convs)
	}
}

func TestParseConventionsSections(t *testing.T) {
	content := `## Naming
- Use snake_case for DB columns
- Use camelCase for JS variables

## Architecture
- Never import from payments in auth module
`
	convs := ParseConventions(content)
	if len(convs) != 3 {
		t.Fatalf("expected 3 conventions, got %d", len(convs))
	}

	if convs[0].Section != "Naming" {
		t.Errorf("first convention section = %q, want Naming", convs[0].Section)
	}
	if convs[0].Rule != "Use snake_case for DB columns" {
		t.Errorf("first rule = %q, want 'Use snake_case for DB columns'", convs[0].Rule)
	}
	if convs[2].Section != "Architecture" {
		t.Errorf("third convention section = %q, want Architecture", convs[2].Section)
	}
}

func TestParseConventionsIgnoresHeaders(t *testing.T) {
	content := `# Title
## Section
- Rule one
(not set)
`
	convs := ParseConventions(content)
	if len(convs) != 1 {
		t.Fatalf("expected 1 convention, got %d", len(convs))
	}
}

func TestCheckConventionsCleanWhenEmpty(t *testing.T) {
	result := CheckConventions("any response", nil)
	if !result.Clean {
		t.Error("CheckConventions with nil conventions should be Clean")
	}
}

func TestCheckConventionsCleanWhenNoCode(t *testing.T) {
	convs := []Convention{{Section: "Naming", Rule: "Use snake_case"}}
	result := CheckConventions("plain text response", convs)
	if !result.Clean {
		t.Error("CheckConventions with no code blocks should be Clean")
	}
}

func TestCheckConventionsDetectsSnakeCaseViolation(t *testing.T) {
	convs := []Convention{{Section: "Naming", Rule: "Use snake_case for all variables"}}
	response := "```go\nfunc processData() {\n\tlet myVariable = 1\n}\n```"
	result := CheckConventions(response, convs)
	if result.Clean {
		t.Error("expected violation for camelCase when snake_case required")
	}
}

func TestCheckConventionsDetectsImportViolation(t *testing.T) {
	convs := []Convention{{Section: "Architecture", Rule: "Never import from payments"}}
	response := "```go\nimport \"payments/handler\"\n```"
	result := CheckConventions(response, convs)
	if result.Clean {
		t.Error("expected violation for forbidden import")
	}
}

func TestCheckConventionsCleanForCompliant(t *testing.T) {
	convs := []Convention{{Section: "Architecture", Rule: "Never import from payments"}}
	response := "```go\nimport \"auth/handler\"\n```"
	result := CheckConventions(response, convs)
	if !result.Clean {
		t.Errorf("expected clean result, got violations: %s", result.Warning)
	}
}

func TestExtractForbiddenImport(t *testing.T) {
	tests := []struct {
		rule string
		want string
	}{
		{"Never import from payments", "payments"},
		{"don't import from internal/secret", "internal/secret"},
		{"do not import lodash", "lodash"},
		{"never use moment.js", "moment.js"},
		{"normal rule", ""},
	}
	for _, tt := range tests {
		got := extractForbiddenImport(tt.rule)
		if got != tt.want {
			t.Errorf("extractForbiddenImport(%q) = %q, want %q", tt.rule, got, tt.want)
		}
	}
}

// ── MatchesPath ───────────────────────────────────────────────────────────────

func TestMatchesPathNoGlob(t *testing.T) {
	c := Convention{Rule: "Use snake_case"}
	// No PathGlob → matches everything.
	if !c.MatchesPath("internal/router/router.go") {
		t.Error("convention with no PathGlob should match any file")
	}
}

func TestMatchesPathGlobMatch(t *testing.T) {
	c := Convention{Rule: "Use snake_case", PathGlob: "internal/router/*.go"}
	if !c.MatchesPath("internal/router/router.go") {
		t.Error("expected glob match for internal/router/router.go")
	}
}

func TestMatchesPathGlobNoMatch(t *testing.T) {
	c := Convention{Rule: "Use snake_case", PathGlob: "internal/router/*.go"}
	if c.MatchesPath("internal/embeddings/embeddings.go") {
		t.Error("glob should not match unrelated path")
	}
}

func TestMatchesPathDoubleStarPrefix(t *testing.T) {
	c := Convention{Rule: "No vendor imports", PathGlob: "internal/**"}
	if !c.MatchesPath("internal/agent/toolkit.go") {
		t.Error("** prefix pattern should match nested file")
	}
}

func TestSuggestCorrectionsNilWriter(t *testing.T) {
	got := SuggestCorrections([]string{"Foo"}, nil)
	if got != "" {
		t.Errorf("expected empty string for nil writer, got %q", got)
	}
}

func TestSuggestCorrectionsEmpty(t *testing.T) {
	got := SuggestCorrections(nil, nil)
	if got != "" {
		t.Errorf("expected empty string for nil symbols, got %q", got)
	}
}

// ── Path-scoped convention regression ────────────────────────────────────────

// TestPathScopedConventionDetectsViolation verifies that a [path: glob] scoped
// convention flags violations in matching files and ignores non-matching ones.
// This is a regression guard for the MatchesPath integration in CheckConventions.
func TestPathScopedConventionDetectsViolation(t *testing.T) {
	// Convention scoped to internal/router only.
	content := `## Architecture
[path: internal/router/*.go]
- Never import from payments
`
	convs := ParseConventions(content)
	if len(convs) == 0 {
		t.Fatal("ParseConventions returned no conventions")
	}
	if convs[0].PathGlob != "internal/router/*.go" {
		t.Fatalf("expected PathGlob=internal/router/*.go, got %q", convs[0].PathGlob)
	}

	// Code block in a router file — should trigger a violation.
	routerResponse := "```go:internal/router/router.go\nimport \"payments/handler\"\n```"
	result := CheckConventions(routerResponse, convs)
	if result.Clean {
		t.Error("expected violation for payments import in internal/router/router.go")
	}
}

// TestPathScopedConventionIgnoresOtherPaths verifies that a path-scoped
// convention does NOT flag violations in files outside the glob.
func TestPathScopedConventionIgnoresOtherPaths(t *testing.T) {
	content := `## Architecture
[path: internal/router/*.go]
- Never import from payments
`
	convs := ParseConventions(content)
	if len(convs) == 0 {
		t.Fatal("ParseConventions returned no conventions")
	}

	// Same import in a file outside the glob — should be clean.
	otherResponse := "```go:internal/embeddings/embeddings.go\nimport \"payments/handler\"\n```"
	result := CheckConventions(otherResponse, convs)
	if !result.Clean {
		t.Errorf("path-scoped rule should not fire for internal/embeddings/embeddings.go, got: %s", result.Warning)
	}
}

// ── Check with active truth.Writer ────────────────────────────────────────────

// TestCheckWithTruthWriter verifies that Check flags unknown exported symbols
// when a truth.Writer with a populated index is provided.
func TestCheckWithTruthWriter(t *testing.T) {
	root := t.TempDir()
	mantisDir := filepath.Join(root, ".mantis")
	if err := os.MkdirAll(mantisDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Seed GROUND_TRUTH.json with one real exported symbol.
	idx := truth.Index{
		filepath.Join(root, "real.go"): truth.FileEntry{
			Hash:            "abc123",
			LastModified:    "2026-01-01T00:00:00Z",
			ExportedSymbols: []string{"RealFunction"},
		},
	}
	data, _ := json.MarshalIndent(idx, "", "  ")
	if err := os.WriteFile(filepath.Join(mantisDir, "GROUND_TRUTH.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	tw := truth.New(root)
	if tw.FileCount() == 0 {
		t.Fatal("truth.Writer loaded 0 files — check GROUND_TRUTH.json seed")
	}

	// Response contains a hallucinated exported symbol.
	response := "```go\nresult := FakeHallucinatedSymbol()\n```"
	vr := Check(response, tw)
	if vr.Clean {
		t.Error("expected Clean=false when response references unknown exported symbol")
	}
	found := false
	for _, sym := range vr.UnknownSymbols {
		if sym == "FakeHallucinatedSymbol" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected FakeHallucinatedSymbol in UnknownSymbols, got %v", vr.UnknownSymbols)
	}
}

// TestCheckCleanForKnownSymbol verifies Check returns Clean=true when the
// response only references exported symbols that exist in the truth index.
func TestCheckCleanForKnownSymbol(t *testing.T) {
	root := t.TempDir()
	mantisDir := filepath.Join(root, ".mantis")
	if err := os.MkdirAll(mantisDir, 0o755); err != nil {
		t.Fatal(err)
	}

	idx := truth.Index{
		filepath.Join(root, "auth.go"): truth.FileEntry{
			Hash:            "def456",
			LastModified:    "2026-01-01T00:00:00Z",
			ExportedSymbols: []string{"Authenticate", "CreateToken"},
		},
	}
	data, _ := json.MarshalIndent(idx, "", "  ")
	if err := os.WriteFile(filepath.Join(mantisDir, "GROUND_TRUTH.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	tw := truth.New(root)
	response := "```go\ntoken, err := CreateToken(user)\n```"
	vr := Check(response, tw)
	if !vr.Clean {
		t.Errorf("expected Clean=true for known symbol, got UnknownSymbols=%v", vr.UnknownSymbols)
	}
}
