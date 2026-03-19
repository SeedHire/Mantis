package pipeline

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// buildError represents a single structured error extracted from build output.
type buildError struct {
	File    string
	Line    int
	Column  int
	Message string
}

// parseBuildErrors extracts structured errors from raw build output.
// Supports Go, TypeScript, Rust, Python, and a generic fallback.
func parseBuildErrors(output, lang string) []buildError {
	var errors []buildError
	lines := strings.Split(output, "\n")

	switch lang {
	case "go":
		errors = parseGoBuildErrors(lines)
	case "typescript":
		errors = parseTSBuildErrors(lines)
	case "rust":
		errors = parseRustBuildErrors(lines)
	case "python":
		errors = parsePythonBuildErrors(lines)
	}

	// If language-specific parsing found nothing, try generic fallback.
	if len(errors) == 0 {
		errors = parseGenericBuildErrors(lines)
	}
	return errors
}

// Go: path/to/file.go:42:15: error message
var goErrRe = regexp.MustCompile(`^(.+\.go):(\d+):(\d+):\s*(.+)$`)

func parseGoBuildErrors(lines []string) []buildError {
	var errs []buildError
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if m := goErrRe.FindStringSubmatch(line); m != nil {
			ln, _ := strconv.Atoi(m[2])
			col, _ := strconv.Atoi(m[3])
			errs = append(errs, buildError{File: m[1], Line: ln, Column: col, Message: m[4]})
		}
	}
	return errs
}

// TypeScript: path/to/file.ts(42,15): error TS2345: message
var tsErrRe = regexp.MustCompile(`^(.+\.(?:ts|tsx|js|jsx))\((\d+),(\d+)\):\s*(.+)$`)

func parseTSBuildErrors(lines []string) []buildError {
	var errs []buildError
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if m := tsErrRe.FindStringSubmatch(line); m != nil {
			ln, _ := strconv.Atoi(m[2])
			col, _ := strconv.Atoi(m[3])
			errs = append(errs, buildError{File: m[1], Line: ln, Column: col, Message: m[4]})
		}
	}
	return errs
}

// Rust: error[E0308]: message \n  --> path/to/file.rs:42:15
var rustLocRe = regexp.MustCompile(`^\s*-->\s*(.+\.rs):(\d+):(\d+)`)

func parseRustBuildErrors(lines []string) []buildError {
	var errs []buildError
	var lastMsg string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "error") {
			lastMsg = trimmed
		}
		if m := rustLocRe.FindStringSubmatch(line); m != nil {
			ln, _ := strconv.Atoi(m[2])
			col, _ := strconv.Atoi(m[3])
			msg := lastMsg
			if msg == "" {
				msg = "compile error"
			}
			errs = append(errs, buildError{File: m[1], Line: ln, Column: col, Message: msg})
			lastMsg = ""
		}
	}
	return errs
}

// Python: File "path/to/file.py", line 42
var pyErrRe = regexp.MustCompile(`File "(.+\.py)", line (\d+)`)

func parsePythonBuildErrors(lines []string) []buildError {
	var errs []buildError
	for i, line := range lines {
		if m := pyErrRe.FindStringSubmatch(line); m != nil {
			ln, _ := strconv.Atoi(m[2])
			msg := "error"
			// Look ahead for the actual error message.
			for j := i + 1; j < len(lines) && j <= i+3; j++ {
				t := strings.TrimSpace(lines[j])
				if t != "" && !strings.HasPrefix(t, "File ") && !strings.HasPrefix(t, "^") {
					msg = t
					break
				}
			}
			errs = append(errs, buildError{File: m[1], Line: ln, Message: msg})
		}
	}
	return errs
}

// Generic fallback: any line containing "error" or "Error".
var genericErrRe = regexp.MustCompile(`(?i)\berror\b`)

func parseGenericBuildErrors(lines []string) []buildError {
	var errs []buildError
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if genericErrRe.MatchString(trimmed) {
			errs = append(errs, buildError{Message: trimmed})
		}
	}
	// Cap generic errors to avoid noise.
	if len(errs) > 10 {
		errs = errs[:10]
	}
	return errs
}

// buildRetryContext creates a focused retry message with source context around each error.
// It deduplicates by file and takes at most maxErrors unique file errors.
func buildRetryContext(root string, errors []buildError, maxErrors int) string {
	if len(errors) == 0 {
		return ""
	}

	// Deduplicate: keep first error per file.
	seen := map[string]bool{}
	var unique []buildError
	for _, e := range errors {
		if e.File == "" {
			// Generic errors without file — include up to maxErrors.
			unique = append(unique, e)
			if len(unique) >= maxErrors {
				break
			}
			continue
		}
		if !seen[e.File] {
			seen[e.File] = true
			unique = append(unique, e)
			if len(unique) >= maxErrors {
				break
			}
		}
	}

	var sb strings.Builder
	sb.WriteString("Build failed with the following errors:\n\n")

	for _, e := range unique {
		if e.File == "" {
			sb.WriteString(fmt.Sprintf("- %s\n", e.Message))
			continue
		}

		sb.WriteString(fmt.Sprintf("Error in `%s` line %d: `%s`\n", e.File, e.Line, e.Message))

		// Read surrounding lines from disk.
		if root != "" && e.Line > 0 {
			content := readLinesAround(root, e.File, e.Line, 15)
			if content != "" {
				ext := langExtension(e.File)
				sb.WriteString(fmt.Sprintf("```%s\n%s\n```\n\n", ext, content))
			}
		}
	}

	sb.WriteString("Fix only the affected function(s). Use ```edit:filepath blocks with <<<SEARCH/===/>>>SEARCH markers " +
		"to patch existing files. Only use ```lang:filepath for brand new files.")

	return sb.String()
}

// readLinesAround reads approximately `radius` lines above and below `targetLine`
// from the given file (resolved relative to root). Returns empty string on error.
func readLinesAround(root, filePath string, targetLine, radius int) string {
	// Resolve path: try as-is first, then relative to root.
	fullPath := filePath
	if !strings.HasPrefix(filePath, "/") && root != "" {
		fullPath = root + "/" + filePath
	}

	f, err := os.Open(fullPath)
	if err != nil {
		return ""
	}
	defer f.Close()

	startLine := targetLine - radius
	if startLine < 1 {
		startLine = 1
	}
	endLine := targetLine + radius

	var lines []string
	scanner := bufio.NewScanner(f)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		if lineNum < startLine {
			continue
		}
		if lineNum > endLine {
			break
		}
		prefix := "  "
		if lineNum == targetLine {
			prefix = "» "
		}
		lines = append(lines, fmt.Sprintf("%s%4d │ %s", prefix, lineNum, scanner.Text()))
	}
	return strings.Join(lines, "\n")
}

// langExtension returns a short language identifier for syntax highlighting based on file extension.
func langExtension(filePath string) string {
	if strings.HasSuffix(filePath, ".go") {
		return "go"
	}
	if strings.HasSuffix(filePath, ".ts") || strings.HasSuffix(filePath, ".tsx") {
		return "typescript"
	}
	if strings.HasSuffix(filePath, ".js") || strings.HasSuffix(filePath, ".jsx") {
		return "javascript"
	}
	if strings.HasSuffix(filePath, ".rs") {
		return "rust"
	}
	if strings.HasSuffix(filePath, ".py") {
		return "python"
	}
	return ""
}
