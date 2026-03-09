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
