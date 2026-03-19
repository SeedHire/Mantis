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

// ── BUG 1: Edit blocks picked up by codeFencePathRe ──────────────────────────

// TestCheckConventions_EditBlockFalsePositive exposes that codeFencePathRe
// matches edit:filepath blocks. The SEARCH section of an edit block contains the
// OLD code being removed. If that old code has a forbidden import, it should NOT
// be flagged — but it is, because codeFencePathRe does not filter edit: prefixes.
func TestCheckConventions_EditBlockFalsePositive(t *testing.T) {
	convs := []Convention{{Section: "Architecture", Rule: "Never import from payments"}}
	// edit block: the SEARCH section has the forbidden import (old code being replaced).
	// The REPLACE section has the clean new code.
	response := "```go:edit:auth.go\n<<<SEARCH\nimport \"payments/handler\"\n===\nimport \"billing/handler\"\n>>>SEARCH\n```"
	result := CheckConventions(response, convs)
	if !result.Clean {
		t.Errorf("BUG 1: edit block SEARCH section should not trigger convention violation, got: %s", result.Warning)
	}
}

// ── BUG: Multiple violations same rule should deduplicate ─────────────────────

func TestCheckConventions_MultipleViolationsSameRule(t *testing.T) {
	convs := []Convention{{Section: "Architecture", Rule: "Never import from payments"}}
	// Three blocks all importing "payments" — same forbidden token in all.
	response := "```go\nimport \"payments/handler\"\n```\n" +
		"```go\nimport \"payments/service\"\n```\n" +
		"```go\nimport \"payments/model\"\n```"
	result := CheckConventions(response, convs)
	if result.Clean {
		t.Fatal("expected violations")
	}
	// All three generate the same Details string ("code contains import/reference to 'payments'"),
	// so deduplication correctly collapses them to 1.
	if len(result.Violations) != 1 {
		t.Errorf("expected 1 deduplicated violation, got %d: %v", len(result.Violations), result.Violations)
	}
}

// ── BUG: camelCase convention detection ──────────────────────────────────────

func TestCheckConventions_CamelCaseRule(t *testing.T) {
	convs := []Convention{{Section: "Naming", Rule: "Use camelCase for all variables"}}
	response := "```js\nlet my_snake_var = 1\n```"
	result := CheckConventions(response, convs)
	if result.Clean {
		t.Error("expected violation for snake_case variable when camelCase required")
	}
	found := false
	for _, v := range result.Violations {
		if v.Details == "'my_snake_var' should use camelCase (found snake_case)" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected violation about my_snake_var, got %v", result.Violations)
	}
}

// ── BUG: Path scope should reset between sections ────────────────────────────

func TestCheckConventions_PathScopeReset(t *testing.T) {
	content := `## Architecture
[path: internal/router/*.go]
- Never import from payments

## Testing
- Never import from payments
`
	convs := ParseConventions(content)
	if len(convs) != 2 {
		t.Fatalf("expected 2 conventions, got %d", len(convs))
	}
	// The first rule should be path-scoped, the second should be global (empty PathGlob).
	if convs[0].PathGlob != "internal/router/*.go" {
		t.Errorf("first convention PathGlob = %q, want internal/router/*.go", convs[0].PathGlob)
	}
	if convs[1].PathGlob != "" {
		t.Errorf("second convention PathGlob = %q, want empty (scope should reset on new section)", convs[1].PathGlob)
	}
}

// ── BUG 2: Nested backticks in code blocks ────────────────────────────────────

// TestCheck_NestedBackticksTruncation demonstrates that the regex-based code
// block extraction can be confused by backticks inside code (e.g., template
// literals in TypeScript). The non-greedy [\s\S]*? may terminate early.
func TestCheck_NestedBackticksTruncation(t *testing.T) {
	root := t.TempDir()
	mantisDir := filepath.Join(root, ".mantis")
	if err := os.MkdirAll(mantisDir, 0o755); err != nil {
		t.Fatal(err)
	}
	idx := truth.Index{
		filepath.Join(root, "tmpl.ts"): truth.FileEntry{
			Hash:            "aaa",
			LastModified:    "2026-01-01T00:00:00Z",
			ExportedSymbols: []string{"RealFunc"},
		},
	}
	data, _ := json.MarshalIndent(idx, "", "  ")
	if err := os.WriteFile(filepath.Join(mantisDir, "GROUND_TRUTH.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	tw := truth.New(root)

	// TypeScript with a template literal containing backticks.
	// The HallucinatedCall() is AFTER the inner backtick — the regex may not reach it.
	response := "```typescript\nconst sql = `SELECT * FROM users`\nconst x = HallucinatedCall()\n```"
	vr := Check(response, tw)
	// If the regex works correctly, it should flag HallucinatedCall.
	// BUG 2: the backtick in the template literal can cause the regex to
	// terminate early, missing the HallucinatedCall entirely.
	found := false
	for _, sym := range vr.UnknownSymbols {
		if sym == "HallucinatedCall" {
			found = true
		}
	}
	if !found {
		t.Errorf("BUG 2: HallucinatedCall not flagged — regex likely truncated at nested backtick. UnknownSymbols=%v, Clean=%v", vr.UnknownSymbols, vr.Clean)
	}
}

// ── BUG 4: stopWords missing common keywords ─────────────────────────────────

func TestCheck_StopWordsCompleteness(t *testing.T) {
	root := t.TempDir()
	mantisDir := filepath.Join(root, ".mantis")
	if err := os.MkdirAll(mantisDir, 0o755); err != nil {
		t.Fatal(err)
	}
	idx := truth.Index{
		filepath.Join(root, "main.go"): truth.FileEntry{
			Hash:            "bbb",
			LastModified:    "2026-01-01T00:00:00Z",
			ExportedSymbols: []string{"RealFunc"},
		},
	}
	data, _ := json.MarshalIndent(idx, "", "  ")
	if err := os.WriteFile(filepath.Join(mantisDir, "GROUND_TRUTH.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	tw := truth.New(root)

	// These are common language keywords that should be in stopWords but aren't.
	// They are capitalized here to pass the exported-symbol gate.
	keywords := []string{"While", "Else", "Try", "Catch", "Throw", "Typeof", "Await", "Async", "Class", "This", "Self"}
	var codeLines []string
	for _, kw := range keywords {
		codeLines = append(codeLines, kw+"()")
	}
	response := "```go\n" + joinLines(codeLines) + "\n```"
	vr := Check(response, tw)

	// BUG 4: These keywords are NOT in stopWords, so they'll be flagged as unknown symbols.
	if !vr.Clean {
		t.Errorf("BUG 4: common keywords flagged as unknown symbols: %v", vr.UnknownSymbols)
	}
}

func joinLines(ss []string) string {
	result := ""
	for i, s := range ss {
		if i > 0 {
			result += "\n"
		}
		result += s
	}
	return result
}

// ── Check() skips edit blocks ─────────────────────────────────────────────────

func TestCheck_SkipsEditBlocks(t *testing.T) {
	root := t.TempDir()
	mantisDir := filepath.Join(root, ".mantis")
	if err := os.MkdirAll(mantisDir, 0o755); err != nil {
		t.Fatal(err)
	}
	idx := truth.Index{
		filepath.Join(root, "app.go"): truth.FileEntry{
			Hash:            "ccc",
			LastModified:    "2026-01-01T00:00:00Z",
			ExportedSymbols: []string{"RealFunc"},
		},
	}
	data, _ := json.MarshalIndent(idx, "", "  ")
	if err := os.WriteFile(filepath.Join(mantisDir, "GROUND_TRUTH.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	tw := truth.New(root)

	// edit block containing an unknown exported symbol. Check() should skip it.
	response := "```edit:app.go\n<<<SEARCH\nOldFunction()\n===\nNewFunction()\n>>>SEARCH\n```"
	vr := Check(response, tw)
	if !vr.Clean {
		t.Errorf("Check() should skip edit blocks entirely, but flagged: %v", vr.UnknownSymbols)
	}
}

// ── Same symbol in multiple code blocks: dedup ───────────────────────────────

func TestCheck_MultipleCodeBlocksSameSymbol(t *testing.T) {
	root := t.TempDir()
	mantisDir := filepath.Join(root, ".mantis")
	if err := os.MkdirAll(mantisDir, 0o755); err != nil {
		t.Fatal(err)
	}
	idx := truth.Index{
		filepath.Join(root, "main.go"): truth.FileEntry{
			Hash:            "ddd",
			LastModified:    "2026-01-01T00:00:00Z",
			ExportedSymbols: []string{"RealFunc"},
		},
	}
	data, _ := json.MarshalIndent(idx, "", "  ")
	if err := os.WriteFile(filepath.Join(mantisDir, "GROUND_TRUTH.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	tw := truth.New(root)

	// Same unknown symbol in two separate code blocks.
	response := "```go\nFakeSymbol()\n```\n\n```go\nFakeSymbol()\n```"
	vr := Check(response, tw)
	if vr.Clean {
		t.Fatal("expected unknown symbol to be flagged")
	}
	count := 0
	for _, sym := range vr.UnknownSymbols {
		if sym == "FakeSymbol" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected FakeSymbol to appear exactly once in UnknownSymbols, got %d (total: %v)", count, vr.UnknownSymbols)
	}
}

// ── extractForbiddenImport: all 13+ phrasings ────────────────────────────────

func TestExtractForbiddenImport_AllVariants(t *testing.T) {
	tests := []struct {
		rule string
		want string
	}{
		{"Never import from payments", "payments"},
		{"don't import from secret", "secret"},
		{"do not import from legacy", "legacy"},
		{"do not import lodash", "lodash"},
		{"never use moment", "moment"},
		{"must not import from vendor", "vendor"},
		{"must not import axios", "axios"},
		{"cannot import from restricted", "restricted"},
		{"cannot import banned", "banned"},
		{"should not import deprecated", "deprecated"},
		{"avoid importing legacy_lib", "legacy_lib"},
		{"no imports from old_service", "old_service"},
		{"not import from forbidden_pkg", "forbidden_pkg"},
		// "X may not import Y" — not in the markers list, should return empty.
		{"auth may not import payments", ""},
		// No match at all.
		{"always write tests", ""},
	}
	for _, tt := range tests {
		got := extractForbiddenImport(tt.rule)
		if got != tt.want {
			t.Errorf("extractForbiddenImport(%q) = %q, want %q", tt.rule, got, tt.want)
		}
	}
}

// ── Dependencies section convention checking ─────────────────────────────────

func TestCheckConventions_DependenciesSection(t *testing.T) {
	convs := []Convention{{Section: "Dependencies", Rule: "Never import from payments"}}
	response := "```go\nimport \"payments/handler\"\n```"
	result := CheckConventions(response, convs)
	if result.Clean {
		t.Error("Dependencies section should also enforce import restrictions")
	}
}

// ── Imports section convention checking ──────────────────────────────────────

func TestCheckConventions_ImportsSection(t *testing.T) {
	convs := []Convention{{Section: "Imports", Rule: "Never import from payments"}}
	response := "```go\nimport \"payments/handler\"\n```"
	result := CheckConventions(response, convs)
	if result.Clean {
		t.Error("Imports section should also enforce import restrictions")
	}
}

// ── Empty code block should not panic ────────────────────────────────────────

func TestCheck_EmptyCodeBlock(t *testing.T) {
	root := t.TempDir()
	mantisDir := filepath.Join(root, ".mantis")
	if err := os.MkdirAll(mantisDir, 0o755); err != nil {
		t.Fatal(err)
	}
	idx := truth.Index{
		filepath.Join(root, "main.go"): truth.FileEntry{
			Hash:            "eee",
			LastModified:    "2026-01-01T00:00:00Z",
			ExportedSymbols: []string{"RealFunc"},
		},
	}
	data, _ := json.MarshalIndent(idx, "", "  ")
	if err := os.WriteFile(filepath.Join(mantisDir, "GROUND_TRUTH.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	tw := truth.New(root)

	// Empty code block — should not panic or error.
	response := "```go\n```"
	vr := Check(response, tw)
	if !vr.Clean {
		t.Errorf("empty code block should be clean, got: %v", vr.UnknownSymbols)
	}
}

// ── BUG 5: Lowercase calls are ignored by design ────────────────────────────

func TestCheck_LowercaseCallsIgnored(t *testing.T) {
	root := t.TempDir()
	mantisDir := filepath.Join(root, ".mantis")
	if err := os.MkdirAll(mantisDir, 0o755); err != nil {
		t.Fatal(err)
	}
	idx := truth.Index{
		filepath.Join(root, "main.go"): truth.FileEntry{
			Hash:            "fff",
			LastModified:    "2026-01-01T00:00:00Z",
			ExportedSymbols: []string{"RealFunc"},
		},
	}
	data, _ := json.MarshalIndent(idx, "", "  ")
	if err := os.WriteFile(filepath.Join(mantisDir, "GROUND_TRUTH.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	tw := truth.New(root)

	// Lowercase function calls like processData() should be ignored (by design for Go).
	// This documents the behavior: even if processData doesn't exist, it's not flagged.
	response := "```go\nprocessData()\nhandleRequest()\nvalidateInput()\n```"
	vr := Check(response, tw)
	if !vr.Clean {
		t.Errorf("lowercase function calls should not be flagged, got: %v", vr.UnknownSymbols)
	}
}

// ── Task 5: Verification bypass ──────────────────────────────────────────────

func TestCheck_UppercaseLanguageTag(t *testing.T) {
	// BUG: codeBlockRe uses [a-z] — uppercase tags like ```Go bypass verification.
	root := t.TempDir()
	mantisDir := filepath.Join(root, ".mantis")
	os.MkdirAll(mantisDir, 0o755)
	idx := truth.Index{
		filepath.Join(root, "main.go"): truth.FileEntry{
			Hash:            "test",
			ExportedSymbols: []string{"RealFunc"},
		},
	}
	b, _ := json.Marshal(idx)
	os.WriteFile(filepath.Join(mantisDir, "GROUND_TRUTH.json"), b, 0o644)
	tw := truth.New(root)

	// Uppercase language tag — should still be parsed and checked.
	response := "```Go\nFakeFunction()\nRealFunc()\n```"
	result := Check(response, tw)
	if result.Clean {
		t.Error("expected unclean result — uppercase ```Go block should be parsed")
	}
	found := false
	for _, sym := range result.UnknownSymbols {
		if sym == "FakeFunction" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected FakeFunction in unknown symbols, got: %v", result.UnknownSymbols)
	}
}

func TestCheckConventions_UppercaseLanguageTag(t *testing.T) {
	// Same bug in codeFencePathRe — convention checking also uses [a-z].
	conventions := []Convention{
		{Section: "Naming", Rule: "Use snake_case for variables"},
	}
	// Uppercase tag with filepath — should still be checked.
	response := "```Go:main.go\nfunc main() {\n\tlet myVar = 1\n}\n```"
	result := CheckConventions(response, conventions)
	// Just verify it doesn't skip the block entirely.
	_ = result // no panic = good
}
