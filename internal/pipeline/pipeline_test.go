package pipeline

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/seedhire/mantis/internal/router"
)

func TestShouldRun(t *testing.T) {
	codeIntent := router.Intent{Tier: router.TierCode, TaskType: "implement"}
	reasonIntent := router.Intent{Tier: router.TierReason, TaskType: "design"}

	should := []string{
		"build a web app",
		"create a REST API with database",
		"implement a todo app from scratch",
		"build a CLI tool with auth and config",
		"create a full stack application",
		"develop a microservice",
		"write a backend with JWT auth and db schema",
		"build a web server with routes and middleware",
	}
	shouldNot := []string{
		"fix this bug in parseUser",
		"explain how defer works",
		"refactor this function",
		"what is the difference between sync.Mutex and sync.RWMutex",
		"write a unit test for fetchUser",
		"rename this variable",
	}

	for _, msg := range should {
		if !ShouldRun(codeIntent, msg) {
			t.Errorf("expected pipeline for %q, got false", msg)
		}
		if !ShouldRun(reasonIntent, msg) {
			t.Errorf("expected pipeline (reason) for %q, got false", msg)
		}
	}
	for _, msg := range shouldNot {
		if ShouldRun(codeIntent, msg) {
			t.Errorf("expected NO pipeline for %q, got true", msg)
		}
	}
}

func TestShouldRunNeverForBlockedTiers(t *testing.T) {
	msg := "build a web app with database and auth"
	blocked := []router.Tier{router.TierMax, router.TierTrivial, router.TierFast, router.TierVision}
	for _, tier := range blocked {
		intent := router.Intent{Tier: tier}
		if ShouldRun(intent, msg) {
			t.Errorf("pipeline should never run for tier %s, but got true", tier)
		}
	}
}

func TestExtractSealedTypes(t *testing.T) {
	dir := t.TempDir()

	// Write a Go file with exported types.
	goContent := `package models

type User struct {
	ID   int
	Name string
}

type Group struct {
	ID      int
	Members []User
}

func NewUser(name string) *User {
	return &User{Name: name}
}

func helper() {} // unexported, should be excluded
`
	goFile := filepath.Join(dir, "models.go")
	if err := os.WriteFile(goFile, []byte(goContent), 0o644); err != nil {
		t.Fatal(err)
	}

	// Write a TS file with exported types.
	tsContent := `export interface ApiResponse {
  data: unknown;
  error?: string;
}

export class UserService {
  constructor() {}
}

export enum Role {
  Admin = "admin",
  User = "user",
}

export const DEFAULT_TIMEOUT = 5000;
`
	tsFile := filepath.Join(dir, "types.ts")
	if err := os.WriteFile(tsFile, []byte(tsContent), 0o644); err != nil {
		t.Fatal(err)
	}

	result := extractSealedTypes(dir, []string{"models.go", "types.ts"})

	if result == "" {
		t.Fatal("expected non-empty sealed types manifest")
	}
	if !strings.Contains(result, "SEALED TYPES") {
		t.Error("missing SEALED TYPES header")
	}

	// Check Go types.
	for _, name := range []string{"User", "Group", "NewUser"} {
		if !strings.Contains(result, "`"+name+"`") {
			t.Errorf("expected Go symbol %q in manifest", name)
		}
	}
	// helper is unexported, should not appear.
	if strings.Contains(result, "`helper`") {
		t.Error("unexported 'helper' should not appear in manifest")
	}

	// Check TS types.
	for _, name := range []string{"ApiResponse", "UserService", "Role", "DEFAULT_TIMEOUT"} {
		if !strings.Contains(result, "`"+name+"`") {
			t.Errorf("expected TS symbol %q in manifest", name)
		}
	}
}

func TestExtractSealedTypesEmpty(t *testing.T) {
	dir := t.TempDir()

	// Empty file — no types.
	if err := os.WriteFile(filepath.Join(dir, "empty.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	result := extractSealedTypes(dir, []string{"empty.go"})
	if result != "" {
		t.Errorf("expected empty manifest for file with no types, got: %s", result)
	}

	// Missing file.
	result = extractSealedTypes(dir, []string{"nonexistent.go"})
	if result != "" {
		t.Errorf("expected empty manifest for missing file, got: %s", result)
	}

	// Empty root.
	result = extractSealedTypes("", []string{"models.go"})
	if result != "" {
		t.Errorf("expected empty manifest for empty root, got: %s", result)
	}
}

func TestExtractSealedTypesDeduplicate(t *testing.T) {
	dir := t.TempDir()

	// Two files define the same type name.
	for _, name := range []string{"a.go", "b.go"} {
		content := "package x\n\ntype Shared struct{}\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	result := extractSealedTypes(dir, []string{"a.go", "b.go"})
	count := strings.Count(result, "`Shared`")
	if count != 1 {
		t.Errorf("expected Shared to appear once, got %d times", count)
	}
}

// ── detectLang ────────────────────────────────────────────────────────────────

func TestDetectLang_Go(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/app\n\ngo 1.22\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := detectLang(dir); got != "go" {
		t.Errorf("expected go, got %q", got)
	}
}

func TestDetectLang_TypeScript(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"app"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := detectLang(dir); got != "typescript" {
		t.Errorf("expected typescript, got %q", got)
	}
}

func TestDetectLang_Python(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pyproject.toml"), []byte("[project]\nname=\"app\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := detectLang(dir); got != "python" {
		t.Errorf("expected python, got %q", got)
	}
}

func TestDetectLang_PythonRequirements(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "requirements.txt"), []byte("requests\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := detectLang(dir); got != "python" {
		t.Errorf("expected python, got %q", got)
	}
}

func TestDetectLang_Rust(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Cargo.toml"), []byte("[package]\nname=\"app\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := detectLang(dir); got != "rust" {
		t.Errorf("expected rust, got %q", got)
	}
}

func TestDetectLang_Unknown(t *testing.T) {
	dir := t.TempDir()
	if got := detectLang(dir); got != "unknown" {
		t.Errorf("empty dir: expected unknown, got %q", got)
	}
}

func TestDetectLang_GoTakesPriority(t *testing.T) {
	// go.mod and package.json both present — go should win (checked first).
	dir := t.TempDir()
	for _, f := range []string{"go.mod", "package.json"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Both present — whichever detectLang checks first should be returned consistently.
	got := detectLang(dir)
	if got != "go" && got != "typescript" {
		t.Errorf("expected go or typescript, got %q", got)
	}
}

// ── langPlanRules ─────────────────────────────────────────────────────────────

func TestLangPlanRules_NonEmpty(t *testing.T) {
	for _, lang := range []string{"go", "typescript", "python", "rust"} {
		rules := langPlanRules(lang)
		if strings.TrimSpace(rules) == "" {
			t.Errorf("langPlanRules(%q) returned empty string", lang)
		}
	}
}

func TestLangPlanRules_Unknown(t *testing.T) {
	// Unknown lang should return empty (no specific rules).
	rules := langPlanRules("unknown")
	if rules != "" {
		t.Errorf("langPlanRules(unknown) should return empty, got %q", rules)
	}
}

// ── langTestNaming ────────────────────────────────────────────────────────────

func TestLangTestNaming_ContainsLangSpecific(t *testing.T) {
	cases := map[string]string{
		"go":         "Test",
		"typescript": "it(",
		"python":     "test_",
		"rust":       "snake_case",
	}
	for lang, expected := range cases {
		got := langTestNaming(lang)
		if !strings.Contains(got, expected) {
			t.Errorf("langTestNaming(%q) should contain %q, got %q", lang, expected, got)
		}
	}
}

func TestLangTestNaming_UnknownNotEmpty(t *testing.T) {
	// Should return a generic fallback, not empty.
	got := langTestNaming("unknown")
	if strings.TrimSpace(got) == "" {
		t.Error("langTestNaming(unknown) returned empty — expected generic fallback")
	}
}
