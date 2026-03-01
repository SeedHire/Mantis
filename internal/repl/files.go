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
// with a file path (format: ```lang:path/to/file or ```lang path/to/file),
// writes each one to disk relative to root, and returns the list of files written.
func extractAndWriteFiles(response, root string) []WrittenFile {
	var written []WrittenFile

	// Match fenced code blocks: ```lang:filepath or ```lang filepath
	// The filepath must look like a file (contain a dot or a slash, no spaces).
	re := regexp.MustCompile("(?m)^```[a-zA-Z0-9_+-]*[:/ ]([^\\s`]+\\.[^\\s`]+)\\n([\\s\\S]*?)\\n```")
	matches := re.FindAllStringSubmatchIndex(response, -1)

	seen := map[string]bool{}
	for _, loc := range matches {
		// loc[2]:loc[3] = filepath capture, loc[4]:loc[5] = content capture
		filePath := strings.TrimSpace(response[loc[2]:loc[3]])
		content := response[loc[4]:loc[5]]

		if filePath == "" || seen[filePath] {
			continue
		}
		seen[filePath] = true

		// Safety: reject absolute paths outside root and path traversal.
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

// printWrittenFiles prints a compact summary of files Mantis wrote to disk.
func printWrittenFiles(files []WrittenFile) {
	if len(files) == 0 {
		return
	}
	fmt.Printf("%s● files written:%s\n", colorGold, colorReset)
	for _, f := range files {
		verb := "updated"
		if f.Created {
			verb = "created"
		}
		fmt.Printf("%s  %s %s%s\n", colorGreen, verb, f.Path, colorReset)
	}
	fmt.Println()
}
