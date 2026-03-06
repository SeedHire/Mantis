package repl

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// diffLines produces a compact diff between old and new content using LCS.
// Returns empty string if the content is identical.
// Shows at most 8 diff lines; truncates the rest with a count.
func diffLines(old, newContent string) string {
	if old == newContent {
		return ""
	}
	oldLines := strings.Split(old, "\n")
	newLines := strings.Split(newContent, "\n")

	removed, added := lcsEditScript(oldLines, newLines)

	const maxShow = 8
	total := len(removed) + len(added)
	if total == 0 {
		return ""
	}

	var parts []string
	for _, l := range removed {
		if len(parts) >= maxShow {
			break
		}
		if l != "" {
			parts = append(parts, colorRed+"-  "+l+colorReset)
		}
	}
	for _, l := range added {
		if len(parts) >= maxShow {
			break
		}
		if l != "" {
			parts = append(parts, colorGreen+"+  "+l+colorReset)
		}
	}

	if len(parts) == 0 {
		return ""
	}
	result := strings.Join(parts, "\n")
	shown := len(parts)
	if shown < total {
		result += fmt.Sprintf("\n%s   … %d more line(s)%s", colorDim, total-shown, colorReset)
	}
	return result
}

// lcsEditScript returns the lines removed from a and lines added in b, computed
// via the Longest Common Subsequence algorithm. Correctly handles duplicate lines.
// Caps input at 200 lines per side to bound O(n·m) time for large files.
func lcsEditScript(a, b []string) (removed, added []string) {
	const maxLines = 200
	if len(a) > maxLines {
		a = a[len(a)-maxLines:]
	}
	if len(b) > maxLines {
		b = b[len(b)-maxLines:]
	}

	m, n := len(a), len(b)
	// Build LCS length table.
	dp := make([][]int, m+1)
	for i := range dp {
		dp[i] = make([]int, n+1)
	}
	for i := 1; i <= m; i++ {
		for j := 1; j <= n; j++ {
			if a[i-1] == b[j-1] {
				dp[i][j] = dp[i-1][j-1] + 1
			} else if dp[i-1][j] >= dp[i][j-1] {
				dp[i][j] = dp[i-1][j]
			} else {
				dp[i][j] = dp[i][j-1]
			}
		}
	}

	// Backtrack to build the edit script.
	i, j := m, n
	for i > 0 || j > 0 {
		switch {
		case i > 0 && j > 0 && a[i-1] == b[j-1]:
			i--
			j--
		case j > 0 && (i == 0 || dp[i][j-1] >= dp[i-1][j]):
			added = append([]string{b[j-1]}, added...)
			j--
		default:
			removed = append([]string{a[i-1]}, removed...)
			i--
		}
	}
	return
}

// WrittenFile records a file that was written from an AI response.
type WrittenFile struct {
	Path    string
	Created bool   // true = new file, false = overwritten
	Diff    string // non-empty for modified files: compact +/- diff summary
}

// extractAndWriteFiles scans the AI response for fenced code blocks tagged
// with a file path. Handles two formats:
//   - ```lang:path/to/file — whole-file write (new files or full replacements)
//   - ```edit:path/to/file — SEARCH/REPLACE patch for existing files
//
// Edit blocks are processed first; whole-file blocks skip files already patched.
func extractAndWriteFiles(response, root string) []WrittenFile {
	var written []WrittenFile
	seen := map[string]bool{}

	// Capture group 1 = block type (lang or "edit"), group 2 = filepath, group 3 = body.
	re := regexp.MustCompile("(?m)^```([a-zA-Z0-9_+-]*)[:/ ]([^\\s`]+)\\n([\\s\\S]*?)\\n```")

	for _, m := range re.FindAllStringSubmatch(response, -1) {
		blockType := strings.ToLower(m[1]) // "go", "ts", "edit", etc.
		filePath := strings.TrimSpace(m[2])
		body := m[3]

		if filePath == "" {
			continue
		}
		if !looksLikeFilePath(filePath) {
			continue
		}
		if strings.ContainsAny(filePath, " \t") || len(filePath) > 200 {
			continue
		}
		if filepath.IsAbs(filePath) {
			continue
		}
		clean := filepath.Clean(filePath)
		if strings.HasPrefix(clean, "..") {
			continue
		}
		if seen[clean] {
			continue
		}
		seen[clean] = true

		if blockType == "edit" {
			// Apply SEARCH/REPLACE patches — does not overwrite; only patches existing files.
			wfs := applySearchReplacePatches(clean, body, root)
			written = append(written, wfs...)
		} else {
			// Whole-file write.
			dest := filepath.Join(root, clean)
			_, statErr := os.Stat(dest)
			isNew := os.IsNotExist(statErr)

			var oldContent string
			if !isNew {
				if b, err := os.ReadFile(dest); err == nil {
					oldContent = string(b)
				}
			}
			if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
				continue
			}
			if err := os.WriteFile(dest, []byte(body+"\n"), 0o644); err != nil {
				continue
			}
			diff := ""
			if !isNew {
				diff = diffLines(oldContent, body+"\n")
			}
			written = append(written, WrittenFile{Path: clean, Created: isNew, Diff: diff})
		}
	}

	return written
}

// applySearchReplacePatches reads the file at root/filePath, applies every
// <<<SEARCH/===/>>>SEARCH section in body, writes the result, and returns the
// WrittenFile record. Skips individual sections that are not found or ambiguous.
func applySearchReplacePatches(filePath, body, root string) []WrittenFile {
	dest := filepath.Join(root, filePath)
	data, err := os.ReadFile(dest)
	if err != nil {
		fmt.Printf("%s  ⚠ edit: %s not found (skipped)%s\n", colorDim, filePath, colorReset)
		return nil
	}

	const searchOpen = "<<<SEARCH"
	const separator = "==="
	const searchClose = ">>>SEARCH"

	oldContent := string(data)
	content := oldContent
	applied := 0
	remaining := body

	for {
		openIdx := strings.Index(remaining, searchOpen)
		if openIdx < 0 {
			break
		}
		remaining = remaining[openIdx+len(searchOpen):]
		if len(remaining) > 0 && remaining[0] == '\n' {
			remaining = remaining[1:]
		}

		sepIdx := strings.Index(remaining, "\n"+separator+"\n")
		if sepIdx < 0 {
			break
		}
		oldText := remaining[:sepIdx]
		remaining = remaining[sepIdx+len("\n"+separator+"\n"):]

		var newText string
		closeIdx := strings.Index(remaining, "\n"+searchClose)
		if closeIdx < 0 {
			closeIdx = strings.Index(remaining, searchClose)
			if closeIdx < 0 {
				break
			}
			newText = strings.TrimRight(remaining[:closeIdx], "\n")
			remaining = remaining[closeIdx+len(searchClose):]
		} else {
			newText = remaining[:closeIdx]
			remaining = remaining[closeIdx+1+len(searchClose):]
		}

		count := strings.Count(content, oldText)
		if count == 0 {
			fmt.Printf("%s  ⚠ edit %s: SEARCH text not found (skipped)%s\n", colorDim, filePath, colorReset)
			continue
		}
		if count > 1 {
			fmt.Printf("%s  ⚠ edit %s: SEARCH text ambiguous (%d matches, skipped)%s\n", colorDim, filePath, count, colorReset)
			continue
		}
		content = strings.Replace(content, oldText, newText, 1)
		applied++
	}

	if applied == 0 {
		return nil
	}
	if err := os.WriteFile(dest, []byte(content), 0o644); err != nil {
		fmt.Printf("%s  ⚠ edit %s: write error: %v%s\n", colorDim, filePath, err, colorReset)
		return nil
	}
	return []WrittenFile{{Path: filePath, Created: false, Diff: diffLines(oldContent, content)}}
}

// looksLikeFilePath returns true if s looks like a file path:
//   - contains a dot  (app.py, docker-compose.yml)
//   - contains a slash (src/app, scripts/run)
//   - starts with a dot (.env, .gitignore)
//   - is a known extensionless filename (Makefile, Dockerfile, etc.)
func looksLikeFilePath(s string) bool {
	if strings.Contains(s, ".") || strings.Contains(s, "/") || strings.HasPrefix(s, ".") {
		return true
	}
	return knownExtensionless[strings.ToLower(s)]
}

var knownExtensionless = map[string]bool{
	"makefile": true, "dockerfile": true, "procfile": true,
	"gemfile": true, "rakefile": true, "guardfile": true,
	"vagrantfile": true, "jenkinsfile": true, "brewfile": true,
	"cmakelists": true, "license": true, "readme": true,
}

// printWrittenFiles prints a compact summary of files Mantis wrote to disk.
// Modified files include an inline +/- diff of up to 8 changed lines.
func printWrittenFiles(files []WrittenFile) {
	if len(files) == 0 {
		return
	}
	for _, f := range files {
		icon := "✚"
		if !f.Created {
			icon = "✎"
		}
		fmt.Printf("%s%s %s%s\n", colorGreen, icon, f.Path, colorReset)
		if f.Diff != "" {
			// Indent each diff line for visual grouping under the file path.
			for _, line := range strings.Split(f.Diff, "\n") {
				fmt.Printf("   %s\n", line)
			}
		}
	}
	fmt.Println()
}

// stripFileBlocks removes fenced code blocks that are tagged with a file path
// (i.e., blocks that extractAndWriteFiles will write to disk) from a response,
// replacing them with a compact single-line notice so the terminal stays clean.
func stripFileBlocks(response string) string {
	re := regexp.MustCompile("(?m)^```[a-zA-Z0-9_+-]*[:/ ]([^\\s`]+)\\n[\\s\\S]*?\\n```")
	return re.ReplaceAllStringFunc(response, func(match string) string {
		pathRe := regexp.MustCompile("^```[a-zA-Z0-9_+-]*[:/ ]([^\\s`]+)")
		sub := pathRe.FindStringSubmatch(match)
		if len(sub) < 2 || !looksLikeFilePath(sub[1]) || strings.ContainsAny(sub[1], " \t") {
			return match
		}
		return fmt.Sprintf("> ✎ `%s`", sub[1])
	})
}

// stripInternalBlocks removes reasoning artifacts from model responses:
//   - [Internal analysis] sections (Ollama custom)
//   - <think>...</think> blocks (DeepSeek-R1 / SambaNova chain-of-thought)
//   - <thinking>...</thinking> blocks (Anthropic-style)
var internalBlockRe = regexp.MustCompile(`(?s)\[Internal analysis\].*?(\n\n|\z)`)
var thinkBlockRe = regexp.MustCompile(`(?s)<think(?:ing)?>.*?</think(?:ing)?>`)

func stripInternalBlocks(s string) string {
	s = thinkBlockRe.ReplaceAllString(s, "")
	s = internalBlockRe.ReplaceAllString(s, "")
	// Strip "Would you like me to" trailing menus.
	if idx := strings.Index(s, "\nWould you like me to"); idx != -1 {
		s = strings.TrimRight(s[:idx], "\n ")
	}
	if idx := strings.Index(s, "\nWould you like to"); idx != -1 {
		s = strings.TrimRight(s[:idx], "\n ")
	}
	// Warn user if model left stubs — this shouldn't happen with new system prompt
	// but we surface it so they know to re-ask.
	stubPatterns := []string{"// TODO:", "# TODO:", "// FIXME:", "// ... rest", "# ... rest", "pass  # implement", "raise NotImplementedError"}
	for _, p := range stubPatterns {
		if strings.Contains(s, p) {
			s = s + "\n\n> ⚠️  Response contains incomplete stubs. Ask Mantis to \"complete the implementation\" for full code.\n"
			break
		}
	}
	return strings.TrimSpace(s) + "\n"
}
