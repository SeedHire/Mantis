package pipeline

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/seedhire/mantis/internal/router"
)

// ═══════════════════════════════════════════════════════════════════════════════
// Phase 14A — Offline Eval Suite
//
// Tests plan parsing, prompt construction, edit application, validation, and
// constraint extraction WITHOUT calling any LLM. Target: 95% pass rate.
// ═══════════════════════════════════════════════════════════════════════════════

// ── 1. Plan Parsing Eval ────────────────────────────────────────────────────

var samplePlans = []struct {
	name          string
	plan          string
	minTasks      int
	maxTasks      int
	expectFiles   []string // must be found by extractPlanFiles
	hasCriteria   bool
}{
	{
		name: "Go REST API",
		plan: `### Task Breakdown
1. Initialize Go module and project structure
2. Implement database models in models/user.go
3. Create HTTP handlers in handlers/user.go
4. Set up router and middleware in cmd/server/main.go
5. Write tests in handlers/user_test.go

### Verification Criteria
1.
- go.mod exists with correct module path
- Project compiles with go build
2.
- User struct has ID, Name, Email fields
3.
- CRUD endpoints return correct status codes
4.
- Server starts on port 8080
5.
- All tests pass with go test
`,
		minTasks: 4, maxTasks: 6,
		expectFiles: []string{"models/user.go", "handlers/user.go", "cmd/server/main.go", "handlers/user_test.go"},
		hasCriteria: true,
	},
	{
		name: "React Todo App",
		plan: `### Task Breakdown
1. Set up React project with TypeScript
2. Create TodoItem component in src/components/TodoItem.tsx
3. Implement TodoList container in src/components/TodoList.tsx
4. Add localStorage persistence in src/hooks/useStorage.ts
5. Style the application in src/App.css

### Verification Criteria
1.
- package.json has react and typescript deps
2.
- TodoItem renders title and checkbox
`,
		minTasks: 4, maxTasks: 6,
		expectFiles: []string{"src/components/TodoItem.tsx", "src/components/TodoList.tsx", "src/hooks/useStorage.ts", "src/App.css"},
		hasCriteria: true,
	},
	{
		name: "Python FastAPI",
		plan: `### Task Breakdown
1. Create FastAPI application in app/main.py
2. Define Pydantic models in app/models.py
3. Implement routes in app/routes/auth.py
4. Set up SQLAlchemy in app/database.py

### Verification Criteria
1.
- uvicorn starts without errors
`,
		minTasks: 3, maxTasks: 5,
		expectFiles: []string{"app/main.py", "app/models.py", "app/routes/auth.py", "app/database.py"},
		hasCriteria: true,
	},
	{
		name: "Rust CLI Tool",
		plan: `### Task Breakdown
1. Set up Cargo project with clap dependency
2. Implement argument parsing in src/main.rs
3. Add file processing logic in src/processor.rs
4. Write tests in tests/integration.rs

### Verification Criteria
1.
- Cargo.toml has clap dependency
`,
		minTasks: 3, maxTasks: 5,
		expectFiles: []string{"src/main.rs", "src/processor.rs", "tests/integration.rs"},
		hasCriteria: true,
	},
	{
		name: "Go CLI with Cobra",
		plan: `### Task Breakdown
- Initialize project with go.mod
- Create root command in cmd/root.go
- Add serve subcommand in cmd/serve.go
- Implement config loader in internal/config/config.go
`,
		minTasks: 3, maxTasks: 5,
		expectFiles: []string{"cmd/root.go", "cmd/serve.go", "internal/config/config.go"},
		hasCriteria: false,
	},
	{
		name: "Express TypeScript API",
		plan: `### Task Breakdown
1. Set up Express with TypeScript in src/index.ts
2. Define user routes in src/routes/users.ts
3. Create Prisma schema in prisma/schema.prisma
4. Implement middleware in src/middleware/auth.ts
5. Add error handler in src/middleware/error.ts
6. Write tests in src/routes/users.test.ts
`,
		minTasks: 5, maxTasks: 7,
		expectFiles: []string{"src/index.ts", "src/routes/users.ts", "prisma/schema.prisma", "src/middleware/auth.ts"},
		hasCriteria: false,
	},
	{
		name: "Checkbox format",
		plan: `### Task Breakdown
- [x] Set up project scaffolding
- [ ] Create database schema in schema.sql
- [ ] Implement API endpoints in api/handler.go
- [ ] Add authentication in api/auth.go
`,
		minTasks: 3, maxTasks: 5,
		expectFiles: []string{"schema.sql", "api/handler.go", "api/auth.go"},
		hasCriteria: false,
	},
	{
		name: "Python Flask",
		plan: `### Task Breakdown
1. Create Flask app in app/__init__.py
2. Define models in app/models.py
3. Implement views in app/views.py
4. Add templates in templates/index.html
`,
		minTasks: 3, maxTasks: 5,
		expectFiles: []string{"app/__init__.py", "app/models.py", "app/views.py"},
		hasCriteria: false,
	},
	{
		name: "Next.js Dashboard",
		plan: `### Task Breakdown
1. Create page component in app/dashboard/page.tsx
2. Build chart widget in components/Chart.tsx
3. Add API route in app/api/analytics/route.ts
4. Create layout in app/dashboard/layout.tsx

### Verification Criteria
1.
- Page renders without errors
2.
- Chart accepts data prop
`,
		minTasks: 3, maxTasks: 5,
		expectFiles: []string{"app/dashboard/page.tsx", "components/Chart.tsx", "app/api/analytics/route.ts"},
		hasCriteria: true,
	},
	{
		name: "Go Microservice",
		plan: `### Task Breakdown
1. Define protobuf in proto/service.proto
2. Generate Go code and implement server in internal/server/grpc.go
3. Create HTTP gateway in internal/server/http.go
4. Add health checks in internal/health/health.go
5. Wire up main in cmd/svc/main.go
`,
		minTasks: 4, maxTasks: 6,
		expectFiles: []string{"proto/service.proto", "internal/server/grpc.go", "internal/server/http.go", "cmd/svc/main.go"},
		hasCriteria: false,
	},
}

func TestEval_ParseTasks(t *testing.T) {
	pass, total := 0, 0

	for _, tc := range samplePlans {
		t.Run(tc.name, func(t *testing.T) {
			// Test parseTasks
			tasks := parseTasks(tc.plan)
			total++
			if len(tasks) >= tc.minTasks && len(tasks) <= tc.maxTasks {
				pass++
			} else {
				t.Errorf("parseTasks: got %d tasks, expected %d-%d", len(tasks), tc.minTasks, tc.maxTasks)
			}

			// Test tasks have titles
			total++
			allTitled := true
			for _, task := range tasks {
				if task.Title == "" {
					allTitled = false
					break
				}
			}
			if allTitled {
				pass++
			} else {
				t.Error("some tasks have empty titles")
			}

			// Test extractPlanFiles
			files := extractPlanFiles(tc.plan)
			for _, expected := range tc.expectFiles {
				total++
				found := false
				for _, f := range files {
					if f == expected {
						found = true
						break
					}
				}
				if found {
					pass++
				} else {
					t.Errorf("extractPlanFiles: missing %q in %v", expected, files)
				}
			}

			// Test verification criteria
			if tc.hasCriteria {
				criteria := parseVerificationCriteria(tc.plan)
				total++
				if len(criteria) > 0 {
					pass++
				} else {
					t.Error("expected verification criteria but got none")
				}
			}
		})
	}

	t.Logf("Plan parsing: %d/%d", pass, total)
}

// ── 2. Prompt Construction Eval ─────────────────────────────────────────────

func TestEval_PromptConstruction(t *testing.T) {
	pass, total := 0, 0

	languages := []string{"go", "typescript", "python", "rust"}

	for _, lang := range languages {
		t.Run("langPlanRules_"+lang, func(t *testing.T) {
			rules := langPlanRules(lang)
			total++
			if strings.TrimSpace(rules) != "" {
				pass++
			} else {
				t.Errorf("langPlanRules(%q) returned empty", lang)
			}
		})

		t.Run("langCodeRules_"+lang, func(t *testing.T) {
			rules := langCodeRules(lang)
			total++
			if strings.TrimSpace(rules) != "" {
				pass++
			} else {
				t.Errorf("langCodeRules(%q) returned empty", lang)
			}
		})

		t.Run("codeStageSuffix_default_"+lang, func(t *testing.T) {
			suffix := codeStageSuffixFor(lang, router.EditFormatSearchReplace)
			total++
			if strings.Contains(suffix, "SEARCH") && strings.Contains(suffix, "edit:") {
				pass++
			} else {
				t.Error("default format should contain SEARCH and edit: instructions")
			}
			total++
			if strings.Contains(suffix, "IMPLEMENTER") {
				pass++
			} else {
				t.Error("should contain IMPLEMENTER role")
			}
		})

		t.Run("codeStageSuffix_wholeFile_"+lang, func(t *testing.T) {
			suffix := codeStageSuffixFor(lang, router.EditFormatWholeFile)
			total++
			if strings.Contains(suffix, "COMPLETE file") || strings.Contains(suffix, "full file") {
				pass++
			} else {
				t.Error("whole-file format should mention complete/full file")
			}
			total++
			if strings.Contains(suffix, "IMPLEMENTER") {
				pass++
			} else {
				t.Error("should contain IMPLEMENTER role")
			}
		})
	}

	// Test codeUserPrompt assembly
	t.Run("codeUserPrompt", func(t *testing.T) {
		prompt := codeUserPrompt("build a web app", "1. Create server\n2. Add routes", "## Context\nExisting files here")
		total++
		if strings.Contains(prompt, "build a web app") && strings.Contains(prompt, "Create server") && strings.Contains(prompt, "Existing files here") {
			pass++
		} else {
			t.Error("codeUserPrompt should contain request, plan, and context")
		}
	})

	t.Run("codeUserPrompt_noContext", func(t *testing.T) {
		prompt := codeUserPrompt("build a CLI", "1. Parse args", "")
		total++
		if strings.Contains(prompt, "build a CLI") && !strings.Contains(prompt, "Existing files here") {
			pass++
		} else {
			t.Error("codeUserPrompt with empty context should omit context")
		}
	})

	// Test unknown language returns empty rules
	t.Run("langPlanRules_unknown", func(t *testing.T) {
		total++
		if langPlanRules("unknown") == "" {
			pass++
		} else {
			t.Error("unknown language should return empty rules")
		}
	})

	t.Logf("Prompt construction: %d/%d", pass, total)
}

// ── 3. Edit Application Eval ────────────────────────────────────────────────

func TestEval_EditApplication(t *testing.T) {
	pass, total := 0, 0

	// 3a: Basic edit block parsing
	t.Run("basic_edit_parse", func(t *testing.T) {
		text := "```edit:handler.go\n<<<SEARCH\nfunc Old() {\n===\nfunc New() {\n>>>SEARCH\n```"
		edits := parseEditBlocks(text)
		total++
		if len(edits) == 1 && edits[0].FilePath == "handler.go" {
			pass++
		} else {
			t.Errorf("expected 1 edit for handler.go, got %d", len(edits))
		}
	})

	// 3b: Multiple edits in one block
	t.Run("multiple_edits_one_block", func(t *testing.T) {
		text := "```edit:server.go\n<<<SEARCH\nfunc A() {\n===\nfunc A2() {\n>>>SEARCH\n<<<SEARCH\nfunc B() {\n===\nfunc B2() {\n>>>SEARCH\n```"
		edits := parseEditBlocks(text)
		total++
		if len(edits) == 2 {
			pass++
		} else {
			t.Errorf("expected 2 edits, got %d", len(edits))
		}
	})

	// 3c: Edit blocks across multiple files
	t.Run("multi_file_edits", func(t *testing.T) {
		text := "```edit:a.go\n<<<SEARCH\nold\n===\nnew\n>>>SEARCH\n```\n```edit:b.go\n<<<SEARCH\nfoo\n===\nbar\n>>>SEARCH\n```"
		edits := parseEditBlocks(text)
		total++
		if len(edits) == 2 {
			pass++
		} else {
			t.Errorf("expected 2 edits, got %d", len(edits))
		}
	})

	// 3d: Path traversal blocked
	t.Run("path_traversal_blocked", func(t *testing.T) {
		root := t.TempDir()
		edits := []EditBlock{{FilePath: "../../etc/passwd", OldText: "", NewText: "malicious"}}
		_, skipCount, _ := applyEdits(edits, root)
		total++
		if skipCount > 0 {
			pass++
		} else {
			t.Error("path traversal should be blocked")
		}
	})

	// 3e: Successful edit application
	t.Run("apply_edit_success", func(t *testing.T) {
		root := t.TempDir()
		os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n\nfunc old() {}\n"), 0o644)
		edits := []EditBlock{{FilePath: "main.go", OldText: "func old() {}", NewText: "func updated() {}"}}
		modified, skipCount, _ := applyEdits(edits, root)
		total++
		if len(modified) == 1 && skipCount == 0 {
			pass++
		} else {
			t.Errorf("expected 1 modified, 0 skips; got %d modified, %d skips", len(modified), skipCount)
		}
		total++
		content, _ := os.ReadFile(filepath.Join(root, "main.go"))
		if strings.Contains(string(content), "func updated()") {
			pass++
		} else {
			t.Error("file should contain updated function")
		}
	})

	// 3f: Fuzzy matching — whitespace differences
	t.Run("fuzzy_whitespace_match", func(t *testing.T) {
		root := t.TempDir()
		os.WriteFile(filepath.Join(root, "handler.go"), []byte("package main\n\nfunc Handle()  {\n\treturn nil\n}\n"), 0o644)
		edits := []EditBlock{{FilePath: "handler.go", OldText: "func Handle() {\n    return nil\n}", NewText: "func Handle() error {\n    return nil\n}"}}
		modified, _, _ := applyEdits(edits, root)
		total++
		if len(modified) > 0 {
			pass++
		} else {
			t.Error("fuzzy matching should handle whitespace differences")
		}
	})

	// 3g: extractAndApplyChanges — whole file blocks
	t.Run("whole_file_new", func(t *testing.T) {
		root := t.TempDir()
		text := "```go:newfile.go\npackage main\n\nfunc main() {}\n```"
		written, _ := extractAndApplyChanges(text, root)
		total++
		if len(written) == 1 {
			pass++
		} else {
			t.Errorf("expected 1 written file, got %d", len(written))
		}
		total++
		content, err := os.ReadFile(filepath.Join(root, "newfile.go"))
		if err == nil && strings.Contains(string(content), "func main()") {
			pass++
		} else {
			t.Error("new file should contain func main()")
		}
	})

	// 3h: extractAndApplyChanges — edit blocks take priority over whole-file
	t.Run("edit_priority_over_whole", func(t *testing.T) {
		root := t.TempDir()
		os.WriteFile(filepath.Join(root, "server.go"), []byte("package main\n\nfunc Start() {\n\t// old\n}\n"), 0o644)
		text := "```edit:server.go\n<<<SEARCH\n// old\n===\n// new\n>>>SEARCH\n```\n```go:server.go\npackage main\n\nfunc Start() {\n\t// overwritten\n}\n```"
		written, _ := extractAndApplyChanges(text, root)
		total++
		content, _ := os.ReadFile(filepath.Join(root, "server.go"))
		if strings.Contains(string(content), "// new") && !strings.Contains(string(content), "// overwritten") {
			pass++
		} else {
			t.Errorf("edit blocks should take priority; content: %s; written: %v", string(content), written)
		}
	})

	// 3i: \r\n normalization
	t.Run("crlf_normalization", func(t *testing.T) {
		root := t.TempDir()
		os.WriteFile(filepath.Join(root, "win.go"), []byte("package main\r\n\r\nfunc Win() {\r\n}\r\n"), 0o644)
		edits := []EditBlock{{FilePath: "win.go", OldText: "func Win() {\n}", NewText: "func Win() int {\n\treturn 0\n}"}}
		modified, _, _ := applyEdits(edits, root)
		total++
		if len(modified) > 0 {
			pass++
		} else {
			t.Error("should handle \\r\\n normalization")
		}
	})

	// 3j: Empty SEARCH text
	t.Run("empty_search_skipped", func(t *testing.T) {
		root := t.TempDir()
		os.WriteFile(filepath.Join(root, "test.go"), []byte("package main\n"), 0o644)
		edits := []EditBlock{{FilePath: "test.go", OldText: "", NewText: "new content"}}
		_, skipCount, _ := applyEdits(edits, root)
		total++
		if skipCount > 0 {
			pass++
		} else {
			t.Error("empty search text should be skipped")
		}
	})

	// 3k: Nonexistent file edit
	t.Run("nonexistent_file_edit", func(t *testing.T) {
		root := t.TempDir()
		edits := []EditBlock{{FilePath: "ghost.go", OldText: "old", NewText: "new"}}
		_, skipCount, _ := applyEdits(edits, root)
		total++
		if skipCount > 0 {
			pass++
		} else {
			t.Error("editing nonexistent file should skip")
		}
	})

	// 3l: allowOverwrite lets earlier pipeline files be overwritten
	t.Run("allowOverwrite", func(t *testing.T) {
		root := t.TempDir()
		// Create a file with enough lines to trigger the overwrite guard
		bigContent := "package main\n" + strings.Repeat("// line\n", 60)
		os.WriteFile(filepath.Join(root, "existing.go"), []byte(bigContent), 0o644)
		text := "```go:existing.go\npackage main\n\nfunc Replaced() {}\n```"
		allow := map[string]bool{"existing.go": true}
		written, _ := extractAndApplyChanges(text, root, allow)
		total++
		if len(written) > 0 {
			pass++
		} else {
			t.Error("allowOverwrite should permit overwriting existing large files")
		}
	})

	// 3m: Nested subdirectory creation
	t.Run("nested_dir_creation", func(t *testing.T) {
		root := t.TempDir()
		text := "```go:internal/pkg/handler.go\npackage pkg\n\nfunc Handle() {}\n```"
		written, _ := extractAndApplyChanges(text, root)
		total++
		if len(written) == 1 {
			pass++
		} else {
			t.Errorf("expected 1 written, got %d", len(written))
		}
		total++
		_, err := os.Stat(filepath.Join(root, "internal", "pkg", "handler.go"))
		if err == nil {
			pass++
		} else {
			t.Error("nested directories should be auto-created")
		}
	})

	t.Logf("Edit application: %d/%d", pass, total)
}

// ── 4. Validate Code Output Eval ────────────────────────────────────────────

func TestEval_ValidateCodeOutput(t *testing.T) {
	pass, total := 0, 0

	cases := []struct {
		name   string
		input  string
		wantOK bool
	}{
		{"empty", "", false},
		{"whitespace_only", "   \n\t  ", false},
		{"refusal_cant_help", "I can't help with that request.", false},
		{"refusal_sorry", "I'm sorry, but I cannot generate code for this.", false},
		{"refusal_ai_model", "As an AI language model, I don't have access to files.", false},
		{"refusal_unable", "I'm unable to complete this task.", false},
		{"no_code_fences", "Here is how you would do it:\nCreate a file called main.go and add the handler.", false},
		{"valid_code", "```go\npackage main\n\nfunc main() {\n\tfmt.Println(\"hello\")\n}\n```", true},
		{"valid_edit", "```edit:main.go\n<<<SEARCH\nold\n===\nnew\n>>>SEARCH\n```", true},
		{"refusal_in_comment", "```go\npackage main\n// This is great code\nfunc main() {}\n```", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, reason := validateCodeOutput(tc.input)
			total++
			if ok == tc.wantOK {
				pass++
			} else {
				t.Errorf("validateCodeOutput(%q): got ok=%v reason=%q, want ok=%v", tc.name, ok, reason, tc.wantOK)
			}
		})
	}

	t.Logf("Code validation: %d/%d", pass, total)
}

// ── 5. Constraint Extraction Eval ───────────────────────────────────────────

func TestEval_ConstraintExtraction(t *testing.T) {
	pass, total := 0, 0

	cases := []struct {
		name    string
		request string
		must    []string // substrings that must appear
		empty   bool
	}{
		{"dont_use_gin", "Build a REST API. Don't use gin or any framework.", []string{"DO NOT use gin"}, false},
		{"only_stdlib", "Create a web server. Only use stdlib.", []string{"ONLY use stdlib"}, false},
		{"no_frameworks", "Build an API with no frameworks.", []string{"NO frameworks"}, false},
		{"stdlib_only", "Build a CLI tool, stdlib only.", []string{"STANDARD LIBRARY ONLY"}, false},
		{"avoid_pattern", "Create a server. Avoid using global state.", []string{"AVOID"}, false},
		{"must_use", "Build an app. Must use PostgreSQL.", []string{"MUST use PostgreSQL"}, false},
		{"multiple_constraints", "Don't use gin. Only use net/http. Avoid ORMs.", []string{"DO NOT use gin", "ONLY use net/http", "AVOID"}, false},
		{"no_constraints", "Build a simple web app with user authentication.", nil, true},
		{"never_use", "Never use eval() in the codebase.", []string{"NEVER use eval()"}, false},
		{"prefer_pattern", "Create a CLI tool. Prefer cobra for argument parsing.", []string{"PREFER cobra"}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := extractConstraints(tc.request)
			if tc.empty {
				total++
				if result == "" {
					pass++
				} else {
					t.Errorf("expected empty constraints, got %q", result)
				}
			} else {
				for _, sub := range tc.must {
					total++
					if strings.Contains(result, sub) {
						pass++
					} else {
						t.Errorf("extractConstraints(%q): expected %q in result %q", tc.request, sub, result)
					}
				}
			}
		})
	}

	t.Logf("Constraints: %d/%d", pass, total)
}

// ── 6. Overall Scorecard ────────────────────────────────────────────────────

type evalResult struct {
	name  string
	pass  int
	total int
}

func TestEval_OverallScorecard(t *testing.T) {
	results := []evalResult{
		runSubEval(t, "Plan parsing", evalPlanParsing),
		runSubEval(t, "Prompt construction", evalPromptConstruction),
		runSubEval(t, "Edit application", evalEditApplication),
		runSubEval(t, "Code validation", evalCodeValidation),
		runSubEval(t, "Constraints", evalConstraints),
	}

	totalPass, totalAll := 0, 0
	t.Log("\n=== Offline Eval Scorecard ===")
	for _, r := range results {
		totalPass += r.pass
		totalAll += r.total
		t.Logf("%-24s %d/%d", r.name+":", r.pass, r.total)
	}
	t.Log("─────────────────────────────")
	pct := 0.0
	if totalAll > 0 {
		pct = float64(totalPass) / float64(totalAll) * 100
	}
	t.Logf("Overall:                 %d/%d = %.1f%%", totalPass, totalAll, pct)

	if pct < 95.0 {
		t.Errorf("Overall eval score %.1f%% is below 95%% threshold", pct)
	}
}

// runSubEval runs an eval function and returns its result.
func runSubEval(t *testing.T, name string, fn func() (int, int)) evalResult {
	t.Helper()
	p, tot := fn()
	return evalResult{name: name, pass: p, total: tot}
}

func evalPlanParsing() (int, int) {
	pass, total := 0, 0
	for _, tc := range samplePlans {
		tasks := parseTasks(tc.plan)
		total++
		if len(tasks) >= tc.minTasks && len(tasks) <= tc.maxTasks {
			pass++
		}
		total++
		allTitled := true
		for _, task := range tasks {
			if task.Title == "" {
				allTitled = false
			}
		}
		if allTitled {
			pass++
		}
		files := extractPlanFiles(tc.plan)
		for _, expected := range tc.expectFiles {
			total++
			for _, f := range files {
				if f == expected {
					pass++
					break
				}
			}
		}
		if tc.hasCriteria {
			total++
			if len(parseVerificationCriteria(tc.plan)) > 0 {
				pass++
			}
		}
	}
	return pass, total
}

func evalPromptConstruction() (int, int) {
	pass, total := 0, 0
	for _, lang := range []string{"go", "typescript", "python", "rust"} {
		total++
		if strings.TrimSpace(langPlanRules(lang)) != "" {
			pass++
		}
		total++
		if strings.TrimSpace(langCodeRules(lang)) != "" {
			pass++
		}
		s := codeStageSuffixFor(lang, router.EditFormatSearchReplace)
		total++
		if strings.Contains(s, "SEARCH") {
			pass++
		}
		total++
		if strings.Contains(s, "IMPLEMENTER") {
			pass++
		}
		w := codeStageSuffixFor(lang, router.EditFormatWholeFile)
		total++
		if strings.Contains(w, "COMPLETE file") || strings.Contains(w, "full file") {
			pass++
		}
	}
	total++
	p := codeUserPrompt("req", "plan", "ctx")
	if strings.Contains(p, "req") && strings.Contains(p, "plan") {
		pass++
	}
	total++
	if langPlanRules("unknown") == "" {
		pass++
	}
	return pass, total
}

func evalEditApplication() (int, int) {
	pass, total := 0, 0

	// Basic parse
	total++
	edits := parseEditBlocks("```edit:h.go\n<<<SEARCH\nold\n===\nnew\n>>>SEARCH\n```")
	if len(edits) == 1 {
		pass++
	}

	// Multi edit
	total++
	edits = parseEditBlocks("```edit:s.go\n<<<SEARCH\na\n===\nb\n>>>SEARCH\n<<<SEARCH\nc\n===\nd\n>>>SEARCH\n```")
	if len(edits) == 2 {
		pass++
	}

	// Apply success
	root := os.TempDir() + fmt.Sprintf("/eval_test_%d", os.Getpid())
	os.MkdirAll(root, 0o755)
	defer os.RemoveAll(root)
	os.WriteFile(filepath.Join(root, "t.go"), []byte("package main\nfunc old() {}\n"), 0o644)
	total++
	mod, sk, _ := applyEdits([]EditBlock{{FilePath: "t.go", OldText: "func old() {}", NewText: "func new2() {}"}}, root)
	if len(mod) == 1 && sk == 0 {
		pass++
	}

	// Path traversal
	total++
	_, sk, _ = applyEdits([]EditBlock{{FilePath: "../../etc/passwd", OldText: "", NewText: "x"}}, root)
	if sk > 0 {
		pass++
	}

	// Whole file new
	total++
	wr, _ := extractAndApplyChanges("```go:brand_new.go\npackage main\n```", root)
	if len(wr) > 0 {
		pass++
	}

	return pass, total
}

func evalCodeValidation() (int, int) {
	pass, total := 0, 0
	cases := []struct {
		input  string
		wantOK bool
	}{
		{"", false},
		{"I can't help with that.", false},
		{"No code here, just text.", false},
		{"```go\nfunc main() {}\n```", true},
	}
	for _, c := range cases {
		total++
		ok, _ := validateCodeOutput(c.input)
		if ok == c.wantOK {
			pass++
		}
	}
	return pass, total
}

func evalConstraints() (int, int) {
	pass, total := 0, 0

	total++
	if strings.Contains(extractConstraints("Don't use gin"), "DO NOT use gin") {
		pass++
	}
	total++
	if strings.Contains(extractConstraints("Only use stdlib"), "ONLY use stdlib") {
		pass++
	}
	total++
	if strings.Contains(extractConstraints("no frameworks"), "NO frameworks") {
		pass++
	}
	total++
	if extractConstraints("Build a simple web app") == "" {
		pass++
	}
	return pass, total
}
