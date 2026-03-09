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

// pendingChange holds a resolved file change not yet written to disk.
type pendingChange struct {
	path    string // relative path
	content string // final content to write
	isNew   bool   // true = new file
	diff    string // precomputed colored diff (empty for new files)
}

// codeBlockRe matches fenced code blocks tagged with a file path.
// group 1 = block type (lang or "edit"), group 2 = filepath, group 3 = body.
var codeBlockRe = regexp.MustCompile("(?m)^```([a-zA-Z0-9_+-]*)[:/ ]([^\\s`]+)\\n([\\s\\S]*?)\\n```")

type parsedBlock struct {
	blockType string
	clean     string
	body      string
}

// parseBlocks extracts and validates all code blocks from a response.
func parseBlocks(response string) []parsedBlock {
	var blocks []parsedBlock
	for _, m := range codeBlockRe.FindAllStringSubmatch(response, -1) {
		blockType := strings.ToLower(m[1])
		filePath := strings.TrimSpace(m[2])
		body := m[3]
		if filePath == "" || !looksLikeFilePath(filePath) {
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
		blocks = append(blocks, parsedBlock{blockType, clean, body})
	}
	return blocks
}

// dryRunSearchReplacePatches applies SEARCH/REPLACE patches in memory without writing.
// Returns the new content, old content, number of applied patches, and any error.
func dryRunSearchReplacePatches(filePath, body, root string) (newContent, oldContent string, applied int, err error) {
	dest := filepath.Join(root, filePath)
	data, err := os.ReadFile(dest)
	if err != nil {
		return "", "", 0, fmt.Errorf("edit: %s not found", filePath)
	}

	const searchOpen = "<<<SEARCH"
	const separator = "==="
	const searchClose = ">>>SEARCH"

	oldContent = string(data)
	content := oldContent
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

	return content, oldContent, applied, nil
}

// collectAllChanges parses the AI response and resolves all file changes in memory.
// Nothing is written to disk. Edit blocks take priority over whole-file blocks.
func collectAllChanges(response, root string) []pendingChange {
	blocks := parseBlocks(response)
	var changes []pendingChange
	editPatched := map[string]bool{}

	// Pass 1: edit blocks.
	for _, b := range blocks {
		if b.blockType != "edit" {
			continue
		}
		if editPatched[b.clean] {
			continue
		}
		editPatched[b.clean] = true

		newContent, oldContent, applied, err := dryRunSearchReplacePatches(b.clean, b.body, root)
		if err != nil {
			fmt.Printf("%s  ⚠ %v (skipped)%s\n", colorDim, err, colorReset)
			continue
		}
		if applied == 0 {
			continue
		}
		changes = append(changes, pendingChange{
			path:    b.clean,
			content: newContent,
			isNew:   false,
			diff:    diffLines(oldContent, newContent),
		})
	}

	// Pass 2: whole-file blocks.
	seen := map[string]bool{}
	for _, b := range blocks {
		if b.blockType == "edit" {
			continue
		}
		if editPatched[b.clean] || seen[b.clean] {
			continue
		}
		seen[b.clean] = true

		dest := filepath.Join(root, b.clean)
		_, statErr := os.Stat(dest)
		isNew := os.IsNotExist(statErr)
		finalContent := b.body + "\n"

		if !isNew {
			fmt.Printf("%s  ⚠ whole-file block for existing %s (prefer edit: blocks for patches)%s\n",
				colorDim, b.clean, colorReset)
		}

		var diff string
		if !isNew {
			if raw, err := os.ReadFile(dest); err == nil {
				diff = diffLines(string(raw), finalContent)
			}
		}
		changes = append(changes, pendingChange{
			path:    b.clean,
			content: finalContent,
			isNew:   isNew,
			diff:    diff,
		})
	}

	return changes
}

// writePendingChanges writes all pending changes to disk and returns WrittenFile records.
func writePendingChanges(changes []pendingChange, root string) []WrittenFile {
	var written []WrittenFile
	for _, c := range changes {
		dest := filepath.Join(root, c.path)
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			continue
		}
		if err := os.WriteFile(dest, []byte(c.content), 0o644); err != nil {
			fmt.Printf("%s  ⚠ write %s: %v%s\n", colorDim, c.path, err, colorReset)
			continue
		}
		written = append(written, WrittenFile{Path: c.path, Created: c.isNew, Diff: c.diff})
	}
	return written
}

// promptWriteApproval shows a diff preview and asks the user to confirm writing.
// Returns true if the user approves, false on decline or error.
func promptWriteApproval(changes []pendingChange, rl readliner) bool {
	if len(changes) == 0 {
		return false
	}

	fmt.Printf("\n%s┌─ Preview (%d file(s)) ──%s\n", colorDim, len(changes), colorReset)
	for _, c := range changes {
		icon := "✚ new"
		if !c.isNew {
			icon = "✎ mod"
		}
		fmt.Printf("%s│ %s %s%s\n", colorDim, icon, c.path, colorReset)
		if c.diff != "" {
			for _, line := range strings.Split(c.diff, "\n") {
				fmt.Printf("%s│%s   %s\n", colorDim, colorReset, line)
			}
		}
	}
	fmt.Printf("%s└──%s\n", colorDim, colorReset)

	fmt.Printf("Write %d file(s)? [Y/n]: ", len(changes))
	line, err := rl.Readline()
	if err != nil {
		return false // Ctrl+C or error
	}
	ans := strings.TrimSpace(strings.ToLower(line))
	return ans == "" || ans == "y" || ans == "yes" || ans == "a"
}

// readliner is the subset of readline.Instance used by promptWriteApproval.
// Extracted as interface for testability.
type readliner interface {
	Readline() (string, error)
}

// extractAndWriteFilesWithApproval collects changes, shows a diff preview,
// and writes only if the user approves. Returns nil if declined.
func extractAndWriteFilesWithApproval(response, root string, rl readliner) []WrittenFile {
	changes := collectAllChanges(response, root)
	if len(changes) == 0 {
		return nil
	}
	if !promptWriteApproval(changes, rl) {
		fmt.Printf("%s  (write declined)%s\n", colorDim, colorReset)
		return nil
	}
	return writePendingChanges(changes, root)
}

// extractAndWriteFiles scans the AI response for fenced code blocks tagged
// with a file path. Handles two formats:
//   - ```lang:path/to/file — whole-file write (new files or full replacements)
//   - ```edit:path/to/file — SEARCH/REPLACE patch for existing files
//
// Edit blocks always take priority: two passes are made so that an edit block
// wins even when the model emits a whole-file block for the same path first.
// This variant writes immediately with no approval gate (for automated paths).
func extractAndWriteFiles(response, root string) []WrittenFile {
	changes := collectAllChanges(response, root)
	if len(changes) == 0 {
		return nil
	}
	return writePendingChanges(changes, root)
}

// applySearchReplacePatches reads the file at root/filePath, applies every
// <<<SEARCH/===/>>>SEARCH section in body, writes the result, and returns the
// WrittenFile record. Skips individual sections that are not found or ambiguous.
func applySearchReplacePatches(filePath, body, root string) []WrittenFile {
	newContent, oldContent, applied, err := dryRunSearchReplacePatches(filePath, body, root)
	if err != nil {
		fmt.Printf("%s  ⚠ %v (skipped)%s\n", colorDim, err, colorReset)
		return nil
	}
	if applied == 0 {
		return nil
	}
	dest := filepath.Join(root, filePath)
	if err := os.WriteFile(dest, []byte(newContent), 0o644); err != nil {
		fmt.Printf("%s  ⚠ edit %s: write error: %v%s\n", colorDim, filePath, err, colorReset)
		return nil
	}
	return []WrittenFile{{Path: filePath, Created: false, Diff: diffLines(oldContent, newContent)}}
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
