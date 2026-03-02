// Package autofix detects the project type after files are written and runs
// the appropriate build/lint command. On failure it returns structured output
// so the caller can re-prompt the model with the error and retry.
//
// Supported project types (detected by file presence in root dir):
//   - Go          → go build ./... && go vet ./...
//   - Node/TS     → npx tsc --noEmit  (if tsconfig.json present)
//   - Python      → python -m py_compile on written .py files
//   - Rust        → cargo check
//
// Rules:
//   - Only runs if at least one file was written this turn.
//   - Node: only runs if node_modules/ exists (npm install must happen first).
//   - All commands run with a 60-second timeout.
//   - Error output is capped at 3000 chars for prompt injection.
package autofix

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Result is the outcome of a build check.
type Result struct {
	Passed  bool
	Project string // "go", "node", "python", "rust"
	Command string // the command that was run
	Output  string // combined stdout+stderr (capped)
}

// Check detects the project type and runs its build command.
// root is the project root directory. writtenFiles is the list of paths
// written this turn (used for Python per-file compilation).
// Returns nil if no recognisable project is found.
func Check(root string, writtenFiles []string) *Result {
	// Detection order — first match wins.
	switch {
	case fileExists(root, "go.mod"):
		return runCheck(root, "go", "go build ./... && go vet ./...",
			[]string{"sh", "-c", "go build ./... && go vet ./..."})

	case fileExists(root, "Cargo.toml"):
		return runCheck(root, "rust", "cargo check", []string{"cargo", "check"})

	case fileExists(root, "tsconfig.json") && dirExists(root, "node_modules"):
		return runCheck(root, "node", "npx tsc --noEmit",
			[]string{"sh", "-c", "npx tsc --noEmit"})

	case fileExists(root, "package.json") && dirExists(root, "node_modules"):
		// No tsconfig — check if there's a build script.
		if hasBuildScript(root) {
			return runCheck(root, "node", "npm run build",
				[]string{"sh", "-c", "npm run build"})
		}
		return nil // package.json but no useful build target yet

	case hasPythonFiles(writtenFiles):
		return checkPythonFiles(root, writtenFiles)
	}

	return nil
}

// ShouldRun returns true when there are written files and the project has a
// known build system. Used to decide whether to show the verify spinner.
func ShouldRun(root string, writtenFiles []string) bool {
	if len(writtenFiles) == 0 {
		return false
	}
	return fileExists(root, "go.mod") ||
		fileExists(root, "Cargo.toml") ||
		(fileExists(root, "tsconfig.json") && dirExists(root, "node_modules")) ||
		(fileExists(root, "package.json") && dirExists(root, "node_modules") && hasBuildScript(root)) ||
		hasPythonFiles(writtenFiles)
}

// FormatError returns a concise error message suitable for injecting into a
// model prompt. It strips ANSI escape codes and caps length.
func FormatError(r *Result) string {
	return fmt.Sprintf(
		"Build check failed (%s: %s).\n\nFix all errors — rewrite the affected files completely:\n\n```\n%s\n```",
		r.Project, r.Command, r.Output,
	)
}

// ── Internal helpers ──────────────────────────────────────────────────────────

func runCheck(root, project, cmdLabel string, args []string) *Result {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Dir = root
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	err := cmd.Run()

	output := stripANSI(out.String())
	if len(output) > 3000 {
		output = output[:3000] + "\n… (truncated)"
	}

	return &Result{
		Passed:  err == nil,
		Project: project,
		Command: cmdLabel,
		Output:  strings.TrimSpace(output),
	}
}

func checkPythonFiles(root string, written []string) *Result {
	var pyFiles []string
	for _, p := range written {
		if strings.HasSuffix(p, ".py") {
			pyFiles = append(pyFiles, p)
		}
	}
	if len(pyFiles) == 0 {
		return nil
	}

	// Run py_compile on all written .py files at once.
	args := append([]string{"-m", "py_compile"}, pyFiles...)
	return runCheck(root, "python", "python -m py_compile", append([]string{"python3"}, args...))
}

func hasPythonFiles(written []string) bool {
	for _, p := range written {
		if strings.HasSuffix(p, ".py") {
			return true
		}
	}
	return false
}

func hasBuildScript(root string) bool {
	data, err := os.ReadFile(filepath.Join(root, "package.json"))
	if err != nil {
		return false
	}
	// Simple check — look for "build" script key.
	return strings.Contains(string(data), `"build"`)
}

func fileExists(root, name string) bool {
	_, err := os.Stat(filepath.Join(root, name))
	return err == nil
}

func dirExists(root, name string) bool {
	info, err := os.Stat(filepath.Join(root, name))
	return err == nil && info.IsDir()
}

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)

func stripANSI(s string) string {
	return ansiRe.ReplaceAllString(s, "")
}
