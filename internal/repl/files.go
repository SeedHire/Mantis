package repl

import (
"fmt"
"os"
"path/filepath"
"regexp"
"strings"
)

// diffLines produces a compact unified diff between old and new content.
// Returns empty string if the content is identical.
// Shows at most 8 diff lines; truncates the rest with a count.
func diffLines(old, newContent string) string {
	if old == newContent {
		return ""
	}
	oldLines := strings.Split(old, "\n")
	newLines := strings.Split(newContent, "\n")

	// Simple line diff: collect added/removed lines (no context).
	oldSet := make(map[string]bool, len(oldLines))
	for _, l := range oldLines {
		oldSet[l] = true
	}
	newSet := make(map[string]bool, len(newLines))
	for _, l := range newLines {
		newSet[l] = true
	}

	const maxShow = 8
	var parts []string
	added, removed := 0, 0

	// Removed lines (in old but not in new).
	for _, l := range oldLines {
		if !newSet[l] && l != "" {
			removed++
			if len(parts) < maxShow {
				parts = append(parts, colorRed+"-  "+l+colorReset)
			}
		}
	}
	// Added lines (in new but not in old).
	for _, l := range newLines {
		if !oldSet[l] && l != "" {
			added++
			if len(parts) < maxShow {
				parts = append(parts, colorGreen+"+  "+l+colorReset)
			}
		}
	}

	if len(parts) == 0 {
		return ""
	}

	total := added + removed
	shown := len(parts)
	result := strings.Join(parts, "\n")
	if shown < total {
		result += fmt.Sprintf("\n%s   … %d more line(s)%s", colorDim, total-shown, colorReset)
	}
	return result
}

// WrittenFile records a file that was written from an AI response.
type WrittenFile struct {
Path    string
Created bool   // true = new file, false = overwritten
Diff    string // non-empty for modified files: compact +/- diff summary
}

// extractAndWriteFiles scans the AI response for fenced code blocks tagged
// with a file path (format: ```lang:path/to/file or ```lang filepath),
// writes each one to disk relative to root, and returns the list of files written.
func extractAndWriteFiles(response, root string) []WrittenFile {
var written []WrittenFile

// No dot requirement — we validate with looksLikeFilePath below so that
// extensionless files like Makefile and Dockerfile are captured too.
re := regexp.MustCompile("(?m)^```[a-zA-Z0-9_+-]*[:/ ]([^\\s`]+)\\n([\\s\\S]*?)\\n```")
matches := re.FindAllStringSubmatchIndex(response, -1)

seen := map[string]bool{}
for _, loc := range matches {
filePath := strings.TrimSpace(response[loc[2]:loc[3]])
content := response[loc[4]:loc[5]]

if filePath == "" || seen[filePath] {
continue
}
if !looksLikeFilePath(filePath) {
continue
}
// Reject filenames that bleed into content (e.g., "README.mdbash npm install").
if strings.ContainsAny(filePath, " \t") || len(filePath) > 200 {
continue
}
seen[filePath] = true

// Safety: reject absolute paths and path traversal.
if filepath.IsAbs(filePath) {
continue
}
clean := filepath.Clean(filePath)
if strings.HasPrefix(clean, "..") {
continue
}

dest := filepath.Join(root, clean)

_, statErr := os.Stat(dest)
isNew := os.IsNotExist(statErr)

// Capture old content for diff display before overwriting.
var oldContent string
if !isNew {
	if b, err := os.ReadFile(dest); err == nil {
		oldContent = string(b)
	}
}

if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
continue
}
if err := os.WriteFile(dest, []byte(content+"\n"), 0o644); err != nil {
continue
}

diff := ""
if !isNew {
	diff = diffLines(oldContent, content+"\n")
}
written = append(written, WrittenFile{Path: clean, Created: isNew, Diff: diff})
}

return written
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

// stripInternalBlocks removes any [Internal analysis] ... sections the model
// leaks into its final response. These are reasoning artifacts, not output.
var internalBlockRe = regexp.MustCompile(`(?s)\[Internal analysis\].*?(\n\n|\z)`)

func stripInternalBlocks(s string) string {
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
