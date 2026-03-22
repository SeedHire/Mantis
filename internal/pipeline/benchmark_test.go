//go:build benchmark

package pipeline

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/seedhire/mantis/internal/ollama"
	"github.com/seedhire/mantis/internal/router"
)

// ═══════════════════════════════════════════════════════════════════════════════
// Phase 14B — Online Benchmark Suite
//
// 20 standardized prompts across 4 languages (Go, TypeScript, Python, Rust).
// Each scored on 10 binary criteria. Run with:
//   go test -tags benchmark -run TestBenchmark -timeout 30m ./internal/pipeline/...
// ═══════════════════════════════════════════════════════════════════════════════

// BenchmarkPrompt defines a code generation benchmark task.
type BenchmarkPrompt struct {
	ID       string
	Language string
	Prompt   string
	// Expected outputs for verification.
	EntryFile     string   // expected main entry point
	ExpectFiles   []string // file patterns that should exist
	ExpectImports []string // grep patterns for core logic (handler names, routes, etc.)
}

var benchmarkPrompts = []BenchmarkPrompt{
	// ── Go (5 prompts) ─────────────────────────────────────────────────────
	{
		ID: "go-rest-api", Language: "go",
		Prompt:      "Build a REST API with user CRUD, SQLite storage, and JSON responses. Use stdlib net/http only.",
		EntryFile:   "main.go",
		ExpectFiles: []string{"*.go"},
		ExpectImports: []string{"net/http", "database/sql", "encoding/json"},
	},
	{
		ID: "go-cli-csv", Language: "go",
		Prompt:      "Create a CLI tool that reads CSV files and outputs summary statistics (row count, column count, min/max/avg for numeric columns). Use stdlib only.",
		EntryFile:   "main.go",
		ExpectFiles: []string{"*.go"},
		ExpectImports: []string{"encoding/csv", "os", "strconv"},
	},
	{
		ID: "go-middleware", Language: "go",
		Prompt:      "Build an HTTP middleware stack with logging, API key auth, and rate limiting. Use stdlib net/http only.",
		EntryFile:   "main.go",
		ExpectFiles: []string{"*.go"},
		ExpectImports: []string{"net/http", "log"},
	},
	{
		ID: "go-job-queue", Language: "go",
		Prompt:      "Implement a concurrent job queue with worker pool and graceful shutdown. Use stdlib only.",
		EntryFile:   "main.go",
		ExpectFiles: []string{"*.go"},
		ExpectImports: []string{"sync", "context"},
	},
	{
		ID: "go-url-shortener", Language: "go",
		Prompt:      "Build a URL shortener with SQLite backend and redirect endpoint. Use stdlib net/http only.",
		EntryFile:   "main.go",
		ExpectFiles: []string{"*.go"},
		ExpectImports: []string{"net/http", "database/sql"},
	},
	// ── TypeScript (5 prompts) ─────────────────────────────────────────────
	{
		ID: "ts-express-api", Language: "typescript",
		Prompt:      "Create an Express REST API with TypeScript, user auth (JWT), and a users table using better-sqlite3.",
		EntryFile:   "src/index.ts",
		ExpectFiles: []string{"*.ts", "package.json", "tsconfig.json"},
		ExpectImports: []string{"express", "jsonwebtoken"},
	},
	{
		ID: "ts-react-todo", Language: "typescript",
		Prompt:      "Build a React todo app with localStorage persistence and filter/sort capabilities.",
		EntryFile:   "src/App.tsx",
		ExpectFiles: []string{"*.tsx", "package.json"},
		ExpectImports: []string{"react", "useState"},
	},
	{
		ID: "ts-cli-tasks", Language: "typescript",
		Prompt:      "Create a CLI tool with Commander.js that manages a local JSON task database with add/list/done/delete commands.",
		EntryFile:   "src/index.ts",
		ExpectFiles: []string{"*.ts", "package.json"},
		ExpectImports: []string{"commander", "fs"},
	},
	{
		ID: "ts-websocket-chat", Language: "typescript",
		Prompt:      "Build a WebSocket chat server with rooms, nicknames, and message history using ws package.",
		EntryFile:   "src/index.ts",
		ExpectFiles: []string{"*.ts", "package.json"},
		ExpectImports: []string{"ws"},
	},
	{
		ID: "ts-nextjs-dashboard", Language: "typescript",
		Prompt:      "Create a Next.js app with a dashboard page showing mock analytics data with charts.",
		EntryFile:   "app/page.tsx",
		ExpectFiles: []string{"*.tsx", "package.json"},
		ExpectImports: []string{"react", "next"},
	},
	// ── Python (5 prompts) ─────────────────────────────────────────────────
	{
		ID: "py-fastapi", Language: "python",
		Prompt:      "Build a FastAPI app with user registration, login (JWT), and SQLite storage.",
		EntryFile:   "app/main.py",
		ExpectFiles: []string{"*.py", "requirements.txt"},
		ExpectImports: []string{"fastapi", "jwt"},
	},
	{
		ID: "py-cli-rss", Language: "python",
		Prompt:      "Create a CLI tool with Click that fetches and summarizes RSS feeds.",
		EntryFile:   "main.py",
		ExpectFiles: []string{"*.py", "requirements.txt"},
		ExpectImports: []string{"click", "feedparser"},
	},
	{
		ID: "py-flask-api", Language: "python",
		Prompt:      "Build a Flask REST API with SQLAlchemy models for users and posts.",
		EntryFile:   "app.py",
		ExpectFiles: []string{"*.py", "requirements.txt"},
		ExpectImports: []string{"flask", "sqlalchemy"},
	},
	{
		ID: "py-scheduler", Language: "python",
		Prompt:      "Create a task scheduler that runs periodic jobs with configurable intervals using schedule library.",
		EntryFile:   "main.py",
		ExpectFiles: []string{"*.py"},
		ExpectImports: []string{"schedule"},
	},
	{
		ID: "py-file-organizer", Language: "python",
		Prompt:      "Build a file organizer CLI that sorts files by type into directories.",
		EntryFile:   "main.py",
		ExpectFiles: []string{"*.py"},
		ExpectImports: []string{"pathlib", "shutil"},
	},
	// ── Rust (5 prompts) ──────────────────────────────────────────────────
	{
		ID: "rs-actix-api", Language: "rust",
		Prompt:      "Build an Actix-web REST API with user CRUD and SQLite (rusqlite).",
		EntryFile:   "src/main.rs",
		ExpectFiles: []string{"*.rs", "Cargo.toml"},
		ExpectImports: []string{"actix_web", "rusqlite"},
	},
	{
		ID: "rs-cli-wc", Language: "rust",
		Prompt:      "Create a CLI tool with clap that counts lines, words, and chars in files (like wc).",
		EntryFile:   "src/main.rs",
		ExpectFiles: []string{"*.rs", "Cargo.toml"},
		ExpectImports: []string{"clap"},
	},
	{
		ID: "rs-web-scraper", Language: "rust",
		Prompt:      "Build a concurrent web scraper that fetches URLs in parallel with tokio and reqwest.",
		EntryFile:   "src/main.rs",
		ExpectFiles: []string{"*.rs", "Cargo.toml"},
		ExpectImports: []string{"tokio", "reqwest"},
	},
	{
		ID: "rs-kv-store", Language: "rust",
		Prompt:      "Create a key-value store CLI with persistence to a binary file using bincode.",
		EntryFile:   "src/main.rs",
		ExpectFiles: []string{"*.rs", "Cargo.toml"},
		ExpectImports: []string{"bincode", "serde"},
	},
	{
		ID: "rs-http-proxy", Language: "rust",
		Prompt:      "Build a simple HTTP proxy server with request logging using hyper.",
		EntryFile:   "src/main.rs",
		ExpectFiles: []string{"*.rs", "Cargo.toml"},
		ExpectImports: []string{"hyper"},
	},
}

// BenchmarkScore holds per-prompt scoring results.
type BenchmarkScore struct {
	ID          string
	Language    string
	Criteria    [10]bool
	Total       int
	BuildOutput string
	Error       string
}

// Criterion names for reporting.
var criterionNames = [10]string{
	"Files created",
	"Builds",
	"No compile errors",
	"Tests exist",
	"Tests pass",
	"Entry point",
	"Core logic present",
	"No hardcoded secrets",
	"Error handling",
	"Correct imports",
}

// TestBenchmarkSuite runs all 20 benchmark prompts and prints a scorecard.
func TestBenchmarkSuite(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping benchmark suite in short mode")
	}

	client := ollama.NewFromEnv()

	// Verify models are available.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	models, err := client.ListModels(ctx)
	cancel()
	if err != nil || len(models) == 0 {
		t.Skip("no Ollama models available — skipping benchmark")
	}
	router.ResolveAll(models)

	var scores []BenchmarkScore

	for _, bp := range benchmarkPrompts {
		t.Run(bp.ID, func(t *testing.T) {
			score := runBenchmarkPrompt(t, client, bp)
			scores = append(scores, score)
		})
	}

	// Print scorecard.
	printScorecard(t, scores)
}

// Individual language benchmarks for targeted testing.
func TestBenchmark_Go(t *testing.T)         { runLanguageBenchmarks(t, "go") }
func TestBenchmark_TypeScript(t *testing.T)  { runLanguageBenchmarks(t, "typescript") }
func TestBenchmark_Python(t *testing.T)      { runLanguageBenchmarks(t, "python") }
func TestBenchmark_Rust(t *testing.T)        { runLanguageBenchmarks(t, "rust") }

func runLanguageBenchmarks(t *testing.T, lang string) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	client := ollama.NewFromEnv()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	models, err := client.ListModels(ctx)
	cancel()
	if err != nil || len(models) == 0 {
		t.Skip("no models available")
	}
	router.ResolveAll(models)

	var scores []BenchmarkScore
	for _, bp := range benchmarkPrompts {
		if bp.Language != lang {
			continue
		}
		t.Run(bp.ID, func(t *testing.T) {
			score := runBenchmarkPrompt(t, client, bp)
			scores = append(scores, score)
		})
	}
	printScorecard(t, scores)
}

func runBenchmarkPrompt(t *testing.T, client *ollama.Client, bp BenchmarkPrompt) BenchmarkScore {
	t.Helper()
	score := BenchmarkScore{ID: bp.ID, Language: bp.Language}

	// Create temp project directory.
	dir := t.TempDir()

	// Init project based on language.
	switch bp.Language {
	case "go":
		os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module benchmark\n\ngo 1.22\n"), 0o644)
	case "typescript":
		os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"benchmark","type":"module","scripts":{"build":"tsc","test":"echo ok"}}`), 0o644)
		os.WriteFile(filepath.Join(dir, "tsconfig.json"), []byte(`{"compilerOptions":{"target":"ES2022","module":"ES2022","moduleResolution":"node","outDir":"dist","strict":true}}`), 0o644)
	case "python":
		os.WriteFile(filepath.Join(dir, "requirements.txt"), []byte(""), 0o644)
	case "rust":
		os.WriteFile(filepath.Join(dir, "Cargo.toml"), []byte("[package]\nname = \"benchmark\"\nversion = \"0.1.0\"\nedition = \"2021\"\n"), 0o644)
		os.MkdirAll(filepath.Join(dir, "src"), 0o755)
		os.WriteFile(filepath.Join(dir, "src", "main.rs"), []byte("fn main() {}\n"), 0o644)
	}

	// Run pipeline.
	systemPrompt := "You are a senior software engineer. Write clean, production-quality code."
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	result, err := Run(ctx, client, bp.Prompt, systemPrompt, Options{
		Root:       dir,
		MaxRetries: 2,
		SkipTests:  false,
	})
	if err != nil {
		score.Error = err.Error()
		t.Logf("pipeline error: %v", err)
		return score
	}

	// ── Score each criterion ──────────────────────────────────────────────

	// 1. Files created
	sourceFiles := findSourceFiles(dir, bp.Language)
	score.Criteria[0] = len(sourceFiles) > 0

	// 2. Builds
	buildOK, buildOut := checkBuild(dir, bp.Language)
	score.Criteria[1] = buildOK
	score.BuildOutput = buildOut

	// 3. No compile errors
	score.Criteria[2] = buildOK && !strings.Contains(buildOut, "error")

	// 4. Tests exist
	testFiles := findTestFiles(dir, bp.Language)
	score.Criteria[3] = len(testFiles) > 0

	// 5. Tests pass
	if len(testFiles) > 0 {
		score.Criteria[4] = checkTests(dir, bp.Language)
	}

	// 6. Entry point exists
	score.Criteria[5] = fileExists(dir, bp.EntryFile)

	// 7. Core logic present
	coreLogicFound := 0
	for _, pattern := range bp.ExpectImports {
		if grepDir(dir, pattern) {
			coreLogicFound++
		}
	}
	score.Criteria[6] = coreLogicFound > 0

	// 8. No hardcoded secrets
	score.Criteria[7] = !hasHardcodedSecrets(dir)

	// 9. Error handling
	score.Criteria[8] = hasErrorHandling(dir, bp.Language)

	// 10. Correct imports
	score.Criteria[9] = !hasNonexistentImports(dir, bp.Language)

	// Count total.
	for _, c := range score.Criteria {
		if c {
			score.Total++
		}
	}

	_ = result // use result for logging if needed
	t.Logf("%s: %d/10", bp.ID, score.Total)
	return score
}

// ── Scoring helpers ─────────────────────────────────────────────────────────

func findSourceFiles(dir, lang string) []string {
	exts := map[string][]string{
		"go":         {"*.go"},
		"typescript": {"*.ts", "*.tsx"},
		"python":     {"*.py"},
		"rust":       {"*.rs"},
	}
	var found []string
	for _, ext := range exts[lang] {
		matches, _ := filepath.Glob(filepath.Join(dir, "**", ext))
		found = append(found, matches...)
		// Also check root.
		rootMatches, _ := filepath.Glob(filepath.Join(dir, ext))
		found = append(found, rootMatches...)
	}
	return found
}

func findTestFiles(dir, lang string) []string {
	patterns := map[string][]string{
		"go":         {"*_test.go"},
		"typescript": {"*.test.ts", "*.spec.ts", "*.test.tsx", "*.spec.tsx"},
		"python":     {"test_*.py", "*_test.py"},
		"rust":       {}, // Rust tests are inline
	}
	var found []string
	for _, pat := range patterns[lang] {
		matches, _ := filepath.Glob(filepath.Join(dir, "**", pat))
		found = append(found, matches...)
		rootMatches, _ := filepath.Glob(filepath.Join(dir, pat))
		found = append(found, rootMatches...)
	}
	// Rust: check for #[test] in source files.
	if lang == "rust" {
		if grepDir(dir, "#\\[test\\]") {
			found = append(found, "inline-tests")
		}
	}
	return found
}

func checkBuild(dir, lang string) (bool, string) {
	var cmd *exec.Cmd
	switch lang {
	case "go":
		cmd = exec.Command("go", "build", "./...")
	case "typescript":
		cmd = exec.Command("npx", "tsc", "--noEmit")
	case "python":
		cmd = exec.Command("python3", "-m", "py_compile", "main.py")
	case "rust":
		cmd = exec.Command("cargo", "check")
	default:
		return false, "unknown language"
	}
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return err == nil, string(out)
}

func checkTests(dir, lang string) bool {
	var cmd *exec.Cmd
	switch lang {
	case "go":
		cmd = exec.Command("go", "test", "./...")
	case "typescript":
		cmd = exec.Command("npm", "test")
	case "python":
		cmd = exec.Command("python3", "-m", "pytest", "-x")
	case "rust":
		cmd = exec.Command("cargo", "test")
	default:
		return false
	}
	cmd.Dir = dir
	err := cmd.Run()
	return err == nil
}

func fileExists(dir, relPath string) bool {
	// Check exact path first.
	if _, err := os.Stat(filepath.Join(dir, relPath)); err == nil {
		return true
	}
	// Check common alternatives.
	alts := []string{
		relPath,
		"src/" + filepath.Base(relPath),
		"cmd/" + filepath.Base(relPath),
		filepath.Base(relPath),
	}
	for _, alt := range alts {
		if _, err := os.Stat(filepath.Join(dir, alt)); err == nil {
			return true
		}
	}
	return false
}

func grepDir(dir, pattern string) bool {
	cmd := exec.Command("grep", "-r", "-l", pattern, ".")
	cmd.Dir = dir
	err := cmd.Run()
	return err == nil
}

func hasHardcodedSecrets(dir string) bool {
	secrets := []string{"password123", "secret123", "sk-", "AKIA", "ghp_"}
	for _, s := range secrets {
		if grepDir(dir, s) {
			return true
		}
	}
	return false
}

func hasErrorHandling(dir, lang string) bool {
	switch lang {
	case "go":
		return grepDir(dir, "if err != nil")
	case "typescript":
		return grepDir(dir, "catch") || grepDir(dir, "try")
	case "python":
		return grepDir(dir, "except") || grepDir(dir, "try:")
	case "rust":
		return grepDir(dir, "\\?") || grepDir(dir, "unwrap_or") || grepDir(dir, "match")
	}
	return false
}

func hasNonexistentImports(dir, lang string) bool {
	// Heuristic: if the build passes, imports are correct.
	ok, _ := checkBuild(dir, lang)
	return !ok
}

// ── Scorecard ───────────────────────────────────────────────────────────────

func printScorecard(t *testing.T, scores []BenchmarkScore) {
	t.Helper()
	if len(scores) == 0 {
		return
	}

	totalChecks := len(scores) * 10
	totalPassed := 0

	t.Log("")
	t.Log("═══════════════════════════════════════════════════════════")
	t.Log("  Mantis Code Generation Benchmark")
	t.Logf("  Model: %s | Date: %s", router.ModelFor(router.TierCode), time.Now().Format("2006-01-02"))
	t.Log("═══════════════════════════════════════════════════════════")

	currentLang := ""
	for _, s := range scores {
		if s.Language != currentLang {
			currentLang = s.Language
			t.Logf("\n  ── %s ──", strings.ToUpper(currentLang))
		}
		bar := makeBar(s.Total, 10)
		t.Logf("  %-25s %s %2d/10", s.ID, bar, s.Total)
		totalPassed += s.Total

		if s.Error != "" {
			t.Logf("    error: %s", truncateBench(s.Error, 80))
		}
	}

	pct := float64(totalPassed) / float64(totalChecks) * 100
	t.Log("")
	t.Log("───────────────────────────────────────────────────────────")
	t.Logf("  Overall: %d/%d = %.1f%%", totalPassed, totalChecks, pct)
	t.Log("═══════════════════════════════════════════════════════════")

	if pct < 70.0 {
		t.Errorf("benchmark score %.1f%% is below 70%% target", pct)
	}
}

func makeBar(score, max int) string {
	filled := score
	empty := max - score
	return "[" + strings.Repeat("█", filled) + strings.Repeat("░", empty) + "]"
}

func truncateBench(s string, max int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > max {
		return s[:max-3] + "..."
	}
	return s
}

// TestBenchmarkQuick runs 5 prompts (1 per language + 1 mixed) for fast feedback.
func TestBenchmarkQuick(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	client := ollama.NewFromEnv()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	models, err := client.ListModels(ctx)
	cancel()
	if err != nil || len(models) == 0 {
		t.Skip("no models available")
	}
	router.ResolveAll(models)

	// Pick first prompt from each language.
	quickIDs := map[string]bool{
		"go-rest-api":      true,
		"ts-express-api":   true,
		"py-fastapi":       true,
		"rs-actix-api":     true,
		"go-url-shortener": true, // extra Go prompt
	}

	var scores []BenchmarkScore
	for _, bp := range benchmarkPrompts {
		if !quickIDs[bp.ID] {
			continue
		}
		t.Run(bp.ID, func(t *testing.T) {
			score := runBenchmarkPrompt(t, client, bp)
			scores = append(scores, score)
		})
	}
	printScorecard(t, scores)
}

// Ensure unused imports are consumed.
var _ = fmt.Sprintf
