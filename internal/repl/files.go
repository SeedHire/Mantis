package repl

import (
"fmt"
"os"
"path/filepath"
"regexp"
"strings"
)

// WrittenFile records a file that was written from an AI response.
type WrittenFile struct {
Path    string
Created bool // true = new file, false = overwritten
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

if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
continue
}
if err := os.WriteFile(dest, []byte(content+"\n"), 0o644); err != nil {
continue
}
written = append(written, WrittenFile{Path: clean, Created: isNew})
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
if len(sub) < 2 || !looksLikeFilePath(sub[1]) {
return match
}
return fmt.Sprintf("> ✎ `%s`", sub[1])
})
}
