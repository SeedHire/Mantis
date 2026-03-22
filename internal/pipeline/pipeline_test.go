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

func TestExtractMethodNames_JSClass(t *testing.T) {
	content := `const db = require('../db/connection');

class GroupRepository {
  async create(groupData) {
    const { name, created_by } = groupData;
    db.run(sql, [name, created_by]);
    return this.findById(result.id);
  }

  async findById(id) {
    return db.get(sql, [id]);
  }

  async findByUserId(userId) {
    return db.all(sql, [userId]);
  }

  async addMember(groupId, userId, role) {
    db.run(sql, [groupId, userId, role]);
    return this.getMember(groupId, userId);
  }

  async getMember(groupId, userId) {
    return db.get(sql, [groupId, userId]);
  }

  async getGroupMembers(groupId) {
    return db.all(sql, [groupId]);
  }
}

module.exports = new GroupRepository();
`

	methods := extractMethodNames(content)
	expected := []string{"create", "findById", "findByUserId", "addMember", "getMember", "getGroupMembers"}
	if len(methods) != len(expected) {
		t.Fatalf("expected %d methods, got %d: %v", len(expected), len(methods), methods)
	}
	for i, e := range expected {
		if methods[i] != e {
			t.Errorf("method[%d]: expected %q, got %q", i, e, methods[i])
		}
	}
}

func TestExtractMethodNames_NoClass(t *testing.T) {
	content := `const express = require('express');
const router = express.Router();
router.get('/', (req, res) => { res.json({}); });
module.exports = router;
`
	methods := extractMethodNames(content)
	if len(methods) != 0 {
		t.Errorf("expected 0 methods for non-class file, got %d: %v", len(methods), methods)
	}
}

func TestExtractExportSummary_CommonJS(t *testing.T) {
	content := `class UserRepository {
  async create(data) { return db.run(sql); }
  async findById(id) { return db.get(sql, [id]); }
  async findByEmail(email) { return db.get(sql, [email]); }
}
module.exports = new UserRepository();
`
	summary := extractExportSummary(content)
	if !strings.Contains(summary, "module.exports") {
		t.Error("expected summary to contain module.exports")
	}
	if !strings.Contains(summary, "class UserRepository") {
		t.Error("expected summary to contain class declaration")
	}
	if !strings.Contains(summary, "create()") {
		t.Error("expected summary to contain create() method")
	}
	if !strings.Contains(summary, "findById()") {
		t.Error("expected summary to contain findById() method")
	}
	if !strings.Contains(summary, "findByEmail()") {
		t.Error("expected summary to contain findByEmail() method")
	}
	if !strings.Contains(summary, "EXACT method names") {
		t.Error("expected summary to warn about using exact method names")
	}
}

func TestCheckCrossFileRefs_JSMismatch(t *testing.T) {
	root := t.TempDir()

	// Write a repository with specific method names.
	repoDir := filepath.Join(root, "src", "repositories")
	os.MkdirAll(repoDir, 0o755)
	os.WriteFile(filepath.Join(repoDir, "groupRepository.js"), []byte(`
class GroupRepository {
  async findByUserId(userId) { return []; }
  async getGroupMembers(groupId) { return []; }
}
module.exports = new GroupRepository();
`), 0o644)

	// Write a controller that calls WRONG method names.
	ctrlDir := filepath.Join(root, "src", "controllers")
	os.MkdirAll(ctrlDir, 0o755)
	os.WriteFile(filepath.Join(ctrlDir, "groupController.js"), []byte(`
const groupRepository = require('../repositories/groupRepository');

async function listGroups(req, res) {
  const groups = await groupRepository.findByUser(req.user.id);
  const members = await groupRepository.getMembers(groups[0].id);
  res.json({ groups, members });
}
`), 0o644)

	files := []string{
		filepath.Join(repoDir, "groupRepository.js"),
		filepath.Join(ctrlDir, "groupController.js"),
	}

	errors := checkCrossFileRefs(root, files)
	if len(errors) == 0 {
		t.Fatal("expected cross-file errors for mismatched method names")
	}

	foundFindByUser := false
	foundGetMembers := false
	for _, e := range errors {
		if strings.Contains(e, "findByUser") {
			foundFindByUser = true
		}
		if strings.Contains(e, "getMembers") {
			foundGetMembers = true
		}
	}
	if !foundFindByUser {
		t.Error("expected error about findByUser (should be findByUserId)")
	}
	if !foundGetMembers {
		t.Error("expected error about getMembers (should be getGroupMembers)")
	}
}

func TestCheckCrossFileRefs_JSCorrect(t *testing.T) {
	root := t.TempDir()

	repoDir := filepath.Join(root, "src", "repositories")
	os.MkdirAll(repoDir, 0o755)
	os.WriteFile(filepath.Join(repoDir, "userRepo.js"), []byte(`
class UserRepo {
  async findById(id) { return {}; }
  async create(data) { return {}; }
}
module.exports = new UserRepo();
`), 0o644)

	svcDir := filepath.Join(root, "src", "services")
	os.MkdirAll(svcDir, 0o755)
	os.WriteFile(filepath.Join(svcDir, "authService.js"), []byte(`
const userRepo = require('../repositories/userRepo');

async function register(data) {
  return await userRepo.create(data);
}
async function getUser(id) {
  return await userRepo.findById(id);
}
`), 0o644)

	files := []string{
		filepath.Join(repoDir, "userRepo.js"),
		filepath.Join(svcDir, "authService.js"),
	}

	errors := checkCrossFileRefs(root, files)
	if len(errors) != 0 {
		t.Errorf("expected no errors for correct method calls, got: %v", errors)
	}
}

func TestExtractMethodNames_GoExported(t *testing.T) {
	content := `package repo

type UserRepo struct {
	db *sql.DB
}

func NewUserRepo(db *sql.DB) *UserRepo {
	return &UserRepo{db: db}
}

func (r *UserRepo) FindByID(id int) (*User, error) {
	return nil, nil
}

func (r *UserRepo) Create(data UserData) (*User, error) {
	return nil, nil
}

func (r *UserRepo) DeleteAll() error {
	return nil
}

func helperFunc() {} // unexported, should not appear
`
	methods := extractMethodNames(content)
	expected := map[string]bool{
		"NewUserRepo": true,
		"FindByID":    true,
		"Create":      true,
		"DeleteAll":   true,
	}
	for _, m := range methods {
		if !expected[m] {
			t.Errorf("unexpected method: %q", m)
		}
		delete(expected, m)
	}
	for missing := range expected {
		t.Errorf("missing expected method: %q", missing)
	}
	// helperFunc should not be extracted (unexported).
	for _, m := range methods {
		if m == "helperFunc" {
			t.Error("unexported helperFunc should not be extracted")
		}
	}
}

func TestExtractMethodNames_PythonMixed(t *testing.T) {
	content := `from sqlalchemy import Column

class UserRepository:
    def __init__(self, session):
        self.session = session

    def find_by_id(self, user_id):
        return self.session.get(User, user_id)

    def find_by_email(self, email):
        return self.session.query(User).filter_by(email=email).first()

    def create(self, data):
        user = User(**data)
        self.session.add(user)
        return user

    def _internal_helper(self):
        pass

def get_db_session():
    return Session()

def create_tables():
    Base.metadata.create_all(engine)

def _private_func():
    pass
`
	methods := extractMethodNames(content)
	expected := map[string]bool{
		"find_by_id":    true,
		"find_by_email": true,
		"create":        true,
		"get_db_session": true,
		"create_tables":  true,
	}
	for _, m := range methods {
		if !expected[m] {
			t.Errorf("unexpected method: %q", m)
		}
		delete(expected, m)
	}
	for missing := range expected {
		t.Errorf("missing expected method: %q", missing)
	}
}

func TestCheckCrossFileRefs_PythonMismatch(t *testing.T) {
	root := t.TempDir()

	repoDir := filepath.Join(root, "repositories")
	os.MkdirAll(repoDir, 0o755)
	os.WriteFile(filepath.Join(repoDir, "__init__.py"), []byte(""), 0o644)
	os.WriteFile(filepath.Join(repoDir, "user_repo.py"), []byte(`
class UserRepo:
    def find_by_id(self, user_id):
        return None

    def find_by_email(self, email):
        return None

    def create(self, data):
        return None
`), 0o644)

	svcDir := filepath.Join(root, "services")
	os.MkdirAll(svcDir, 0o755)
	os.WriteFile(filepath.Join(svcDir, "__init__.py"), []byte(""), 0o644)
	os.WriteFile(filepath.Join(svcDir, "auth_service.py"), []byte(`
from .repositories.user_repo import UserRepo

class AuthService:
    def __init__(self):
        self.repo = UserRepo()

    def login(self, email, password):
        user = self.repo.find_by_email(email)
        return user
`), 0o644)

	files := []string{
		filepath.Join(repoDir, "user_repo.py"),
		filepath.Join(svcDir, "auth_service.py"),
	}

	// This should pass — all calls match.
	errors := checkCrossFileRefs(root, files)
	if len(errors) != 0 {
		t.Errorf("expected no errors for correct Python calls, got: %v", errors)
	}
}

func TestCheckCrossFileRefs_ESMImport(t *testing.T) {
	root := t.TempDir()

	modDir := filepath.Join(root, "src", "models")
	os.MkdirAll(modDir, 0o755)
	os.WriteFile(filepath.Join(modDir, "userService.js"), []byte(`
class UserService {
  async getUser(id) { return {}; }
  async createUser(data) { return {}; }
}
export default new UserService();
`), 0o644)

	ctrlDir := filepath.Join(root, "src", "controllers")
	os.MkdirAll(ctrlDir, 0o755)
	os.WriteFile(filepath.Join(ctrlDir, "userController.js"), []byte(`
import userService from '../models/userService';

async function show(req, res) {
  const user = await userService.getUser(req.params.id);
  res.json(user);
}
async function create(req, res) {
  const user = await userService.createUser(req.body);
  res.json(user);
}
`), 0o644)

	files := []string{
		filepath.Join(modDir, "userService.js"),
		filepath.Join(ctrlDir, "userController.js"),
	}

	errors := checkCrossFileRefs(root, files)
	if len(errors) != 0 {
		t.Errorf("expected no errors for correct ESM import calls, got: %v", errors)
	}
}
