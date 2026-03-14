package repl

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// ── Project Facts ─────────────────────────────────────────────────────────────
// Pre-analysis layer: scan project dir and produce structured facts that weak
// models can't ignore. Injected BEFORE the user message so the model sees
// language, framework, entry point, and module exports up front.

// detectProjectFacts scans root for manifests and source files, returning a
// structured [PROJECT FACTS] block. Returns "" if nothing useful was found.
func detectProjectFacts(root string) string {
	lang, framework, runCmd := detectLangFramework(root)
	if lang == "" {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("[PROJECT FACTS — read these carefully]\n")
	sb.WriteString("Language: " + lang + "\n")
	if framework != "" {
		sb.WriteString("Framework: " + framework + "\n")
	}
	if entry := detectEntryPoint(root, lang); entry != "" {
		sb.WriteString("Entry point: " + entry + "\n")
	}
	if runCmd != "" {
		sb.WriteString("Run command: " + runCmd + "\n")
	}

	// Source modules with exports (Python only for now).
	if lang == "Python" || lang == "Python 3" {
		if mods := listPythonModules(root); mods != "" {
			sb.WriteString("Source modules: " + mods + "\n")
		}
	}

	sb.WriteString("[/PROJECT FACTS]")
	return sb.String()
}

// detectLangFramework returns (language, framework, runCommand) from manifests.
func detectLangFramework(root string) (string, string, string) {
	// Go
	if fileExists(filepath.Join(root, "go.mod")) {
		fw := ""
		if deps := readFileFull(filepath.Join(root, "go.mod")); deps != "" {
			for _, pair := range []struct{ dep, name string }{
				{"github.com/gin-gonic/gin", "Gin"},
				{"github.com/labstack/echo", "Echo"},
				{"github.com/gofiber/fiber", "Fiber"},
				{"github.com/gorilla/mux", "Gorilla Mux"},
			} {
				if strings.Contains(deps, pair.dep) {
					fw = pair.name
					break
				}
			}
		}
		run := "go build ./... && go run ."
		if fileExists(filepath.Join(root, "Makefile")) {
			run = "make build && make run (or: go run .)"
		}
		return "Go", fw, run
	}

	// Python
	if fileExists(filepath.Join(root, "requirements.txt")) || fileExists(filepath.Join(root, "pyproject.toml")) || fileExists(filepath.Join(root, "setup.py")) {
		fw := ""
		deps := readFileFull(filepath.Join(root, "requirements.txt")) + "\n" + readFileFull(filepath.Join(root, "pyproject.toml"))
		for _, pair := range []struct{ dep, name string }{
			{"flask", "Flask"},
			{"django", "Django"},
			{"fastapi", "FastAPI"},
			{"streamlit", "Streamlit"},
			{"tornado", "Tornado"},
		} {
			if strings.Contains(strings.ToLower(deps), pair.dep) {
				fw = pair.name
				break
			}
		}
		run := "pip install -r requirements.txt && python"
		if entry := detectEntryPoint(root, "Python"); entry != "" {
			run = "pip install -r requirements.txt && python " + entry
		}
		return "Python", fw, run
	}

	// Node.js
	if fileExists(filepath.Join(root, "package.json")) {
		fw := ""
		pkg := readFileFull(filepath.Join(root, "package.json"))
		for _, pair := range []struct{ dep, name string }{
			{"react", "React"},
			{"next", "Next.js"},
			{"express", "Express"},
			{"fastify", "Fastify"},
			{"vue", "Vue"},
			{"angular", "Angular"},
			{"svelte", "Svelte"},
			{"nestjs", "NestJS"},
		} {
			if strings.Contains(strings.ToLower(pkg), `"`+pair.dep+`"`) {
				fw = pair.name
				break
			}
		}
		run := "npm install && npm start"
		return "Node.js", fw, run
	}

	// Rust
	if fileExists(filepath.Join(root, "Cargo.toml")) {
		return "Rust", "", "cargo build && cargo run"
	}

	return "", "", ""
}

// detectEntryPoint finds the likely entry point file for the given language.
func detectEntryPoint(root, lang string) string {
	switch lang {
	case "Python", "Python 3":
		for _, name := range []string{"main.py", "app.py", "manage.py", "run.py", "server.py"} {
			if fileExists(filepath.Join(root, name)) {
				return name
			}
			// Check src/ subdirectory
			if fileExists(filepath.Join(root, "src", name)) {
				return "src/" + name
			}
		}
	case "Go":
		if fileExists(filepath.Join(root, "main.go")) {
			return "main.go"
		}
		// Check cmd/*/main.go pattern
		if matches, _ := filepath.Glob(filepath.Join(root, "cmd", "*", "main.go")); len(matches) > 0 {
			rel, _ := filepath.Rel(root, matches[0])
			return rel
		}
	case "Node.js":
		for _, name := range []string{"index.js", "index.ts", "server.js", "server.ts", "app.js", "app.ts", "src/index.js", "src/index.ts"} {
			if fileExists(filepath.Join(root, name)) {
				return name
			}
		}
	case "Rust":
		return "src/main.rs"
	}
	return ""
}

// listPythonModules lists Python source files with their exports.
func listPythonModules(root string) string {
	var parts []string
	// Scan root and src/ for .py files
	for _, dir := range []string{root, filepath.Join(root, "src")} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".py") || e.Name() == "__init__.py" {
				continue
			}
			exports := extractPythonExports(filepath.Join(dir, e.Name()))
			rel, _ := filepath.Rel(root, filepath.Join(dir, e.Name()))
			if len(exports) > 0 {
				parts = append(parts, fmt.Sprintf("%s (exports: %s)", rel, strings.Join(exports, ", ")))
			} else {
				parts = append(parts, rel)
			}
			if len(parts) >= 10 {
				break
			}
		}
	}
	return strings.Join(parts, ", ")
}

var (
	pyClassRe    = regexp.MustCompile(`^class\s+([A-Z]\w*)\s*[:(]`)
	pyFuncRe     = regexp.MustCompile(`^def\s+([a-zA-Z]\w*)\s*\(`)
	pyAssignRe   = regexp.MustCompile(`^([A-Z][A-Z_0-9]+)\s*=`)
)

// extractPythonExports returns public class/function/constant names from a .py file.
func extractPythonExports(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	var exports []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if m := pyClassRe.FindStringSubmatch(line); m != nil {
			exports = append(exports, m[1])
		} else if m := pyFuncRe.FindStringSubmatch(line); m != nil {
			if !strings.HasPrefix(m[1], "_") {
				exports = append(exports, m[1])
			}
		} else if m := pyAssignRe.FindStringSubmatch(line); m != nil {
			exports = append(exports, m[1])
		}
	}
	return exports
}

// ── Error Analysis ────────────────────────────────────────────────────────────

var (
	pyImportErrRe    = regexp.MustCompile(`(?i)ImportError:\s*cannot import name '(\w+)' from '(\w+)'`)
	pyModuleErrRe    = regexp.MustCompile(`(?i)ModuleNotFoundError:\s*No module named '(\w+)'`)
	pyAttrErrRe      = regexp.MustCompile(`(?i)AttributeError:\s*(?:module|type) '(\w+)' has no attribute '(\w+)'`)
	pyTypeErrRe      = regexp.MustCompile(`(?i)TypeError:.*`)
	pyFileLineRe     = regexp.MustCompile(`File "([^"]+)", line (\d+)`)
	goUndefinedRe    = regexp.MustCompile(`(\S+\.go):(\d+):\d+: undefined: (\w+)`)
	goUnusedRe       = regexp.MustCompile(`(\S+\.go):(\d+):\d+: (\w+) declared (and|but) not used`)
	nodeRefErrRe     = regexp.MustCompile(`(?i)ReferenceError:\s*(\w+) is not defined`)
	nodeModuleErrRe  = regexp.MustCompile(`(?i)Cannot find module '([^']+)'`)
)

// analyzeError parses error messages in the input and produces a structured
// [ERROR ANALYSIS] block with the diagnosis and available fixes. Returns "" if
// no recognizable error pattern was found.
func analyzeError(input, root string) string {
	lower := strings.ToLower(input)
	// Quick check: does this look like an error at all?
	hasError := false
	for _, sig := range []string{"error", "traceback", "panic:", "cannot", "undefined", "failed"} {
		if strings.Contains(lower, sig) {
			hasError = true
			break
		}
	}
	if !hasError {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("[ERROR ANALYSIS — use this to fix]\n")
	wrote := false

	// Python ImportError
	if m := pyImportErrRe.FindStringSubmatch(input); m != nil {
		symbol, module := m[1], m[2]
		sb.WriteString("Type: ImportError\n")
		if fl := pyFileLineRe.FindStringSubmatch(input); fl != nil {
			sb.WriteString(fmt.Sprintf("File: %s line %s\n", fl[1], fl[2]))
		}
		sb.WriteString(fmt.Sprintf("Problem: '%s' imported from '%s' but does not exist\n", symbol, module))
		// Look up actual exports
		if exports := findModuleExports(root, module); exports != "" {
			sb.WriteString(fmt.Sprintf("Available exports in %s.py: %s\n", module, exports))
		}
		sb.WriteString(fmt.Sprintf("Fix: Remove '%s' from the import or use a name that exists\n", symbol))
		wrote = true
	}

	// Python ModuleNotFoundError
	if m := pyModuleErrRe.FindStringSubmatch(input); m != nil && !wrote {
		sb.WriteString("Type: ModuleNotFoundError\n")
		sb.WriteString(fmt.Sprintf("Problem: Module '%s' is not installed\n", m[1]))
		sb.WriteString(fmt.Sprintf("Fix: pip install %s\n", m[1]))
		wrote = true
	}

	// Python AttributeError
	if m := pyAttrErrRe.FindStringSubmatch(input); m != nil && !wrote {
		module, attr := m[1], m[2]
		sb.WriteString("Type: AttributeError\n")
		sb.WriteString(fmt.Sprintf("Problem: '%s' has no attribute '%s'\n", module, attr))
		if exports := findModuleExports(root, module); exports != "" {
			sb.WriteString(fmt.Sprintf("Available in %s: %s\n", module, exports))
		}
		wrote = true
	}

	// Python TypeError
	if pyTypeErrRe.MatchString(input) && !wrote {
		sb.WriteString("Type: TypeError\n")
		if fl := pyFileLineRe.FindStringSubmatch(input); fl != nil {
			sb.WriteString(fmt.Sprintf("File: %s line %s\n", fl[1], fl[2]))
		}
		sb.WriteString("Problem: Wrong argument types or count in function call\n")
		sb.WriteString("Fix: Check the function signature and match argument types\n")
		wrote = true
	}

	// Go undefined
	if matches := goUndefinedRe.FindAllStringSubmatch(input, -1); len(matches) > 0 && !wrote {
		sb.WriteString("Type: undefined symbol (Go)\n")
		for _, m := range matches {
			sb.WriteString(fmt.Sprintf("File: %s line %s — undefined: %s\n", m[1], m[2], m[3]))
		}
		sb.WriteString("Fix: Check spelling, add missing import, or define the symbol\n")
		wrote = true
	}

	// Go unused
	if matches := goUnusedRe.FindAllStringSubmatch(input, -1); len(matches) > 0 && !wrote {
		sb.WriteString("Type: unused variable (Go)\n")
		for _, m := range matches {
			sb.WriteString(fmt.Sprintf("File: %s line %s — '%s' unused\n", m[1], m[2], m[3]))
		}
		sb.WriteString("Fix: Use the variable or remove it\n")
		wrote = true
	}

	// Node ReferenceError
	if m := nodeRefErrRe.FindStringSubmatch(input); m != nil && !wrote {
		sb.WriteString("Type: ReferenceError (Node.js)\n")
		sb.WriteString(fmt.Sprintf("Problem: '%s' is not defined\n", m[1]))
		sb.WriteString("Fix: Import or declare the variable before use\n")
		wrote = true
	}

	// Node module not found
	if m := nodeModuleErrRe.FindStringSubmatch(input); m != nil && !wrote {
		sb.WriteString("Type: Module not found (Node.js)\n")
		sb.WriteString(fmt.Sprintf("Problem: Cannot find module '%s'\n", m[1]))
		sb.WriteString(fmt.Sprintf("Fix: npm install %s (or check the import path)\n", m[1]))
		wrote = true
	}

	if !wrote {
		return ""
	}

	sb.WriteString("[/ERROR ANALYSIS]")
	return sb.String()
}

// findModuleExports looks for <module>.py in root and src/ and returns exports.
func findModuleExports(root, module string) string {
	for _, dir := range []string{root, filepath.Join(root, "src")} {
		path := filepath.Join(dir, module+".py")
		if fileExists(path) {
			exports := extractPythonExports(path)
			if len(exports) > 0 {
				return strings.Join(exports, ", ")
			}
		}
	}
	return ""
}

// ── Sanity Check ──────────────────────────────────────────────────────────────

// sanityCheckResponse validates model output for obvious mistakes in
// single-language projects. Returns a correction prompt if something is wrong,
// or "" if the response looks fine.
func sanityCheckResponse(response, root string) string {
	lower := strings.ToLower(response)
	lang, _, _ := detectLangFramework(root)
	if lang == "" {
		return ""
	}

	// Only enforce for single-language projects (skip polyglot).
	if isPolyglot(root) {
		return ""
	}

	switch lang {
	case "Python", "Python 3":
		if strings.Contains(lower, "npm install") || strings.Contains(lower, "yarn add") {
			return "[CORRECTION: This is a Python project — use pip/pip3 for package installation, not npm/yarn. Redo your answer using Python tooling only.]"
		}
	case "Node.js":
		if strings.Contains(lower, "pip install") || strings.Contains(lower, "pip3 install") {
			return "[CORRECTION: This is a Node.js project — use npm/yarn for package installation, not pip. Redo your answer using Node.js tooling only.]"
		}
	case "Go":
		if strings.Contains(lower, "npm install") || strings.Contains(lower, "pip install") {
			return "[CORRECTION: This is a Go project — use 'go get' for dependencies. Redo your answer using Go tooling only.]"
		}
	case "Rust":
		if strings.Contains(lower, "npm install") || strings.Contains(lower, "pip install") {
			return "[CORRECTION: This is a Rust project — use 'cargo add' for dependencies. Redo your answer using Rust tooling only.]"
		}
	}

	// Model says "I can't read files" / "paste the output" when files are injected.
	for _, phrase := range []string{
		"i can't read files",
		"i cannot read files",
		"i can't access files",
		"i cannot access files",
		"paste the contents",
		"paste the output",
		"share the file",
		"provide the file",
		"i don't have access to",
	} {
		if strings.Contains(lower, phrase) {
			return "[CORRECTION: You DO have access to the files — they are injected above in the context. Read the [PROJECT FACTS] and file contents provided, then answer based on them.]"
		}
	}

	return ""
}

// isPolyglot returns true if multiple language manifests exist.
func isPolyglot(root string) bool {
	count := 0
	for _, f := range []string{"go.mod", "package.json", "requirements.txt", "pyproject.toml", "Cargo.toml", "setup.py"} {
		if fileExists(filepath.Join(root, f)) {
			count++
		}
	}
	return count > 1
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func readFileFull(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
}
