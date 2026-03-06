package pipeline

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// EditBlock represents a single SEARCH/REPLACE edit within an ```edit:filepath block.
type EditBlock struct {
	FilePath string
	OldText  string
	NewText  string
}

// parseEditBlocks extracts SEARCH/REPLACE pairs from ```edit:filepath fenced blocks.
// Format:
//
//	```edit:filepath
//	<<<SEARCH
//	exact old text
//	===
//	exact new text
//	>>>SEARCH
//	```
//
// Multiple <<<SEARCH ... >>>SEARCH sections per block are supported.
func parseEditBlocks(text string) []EditBlock {
	// Match ```edit:filepath ... ``` blocks.
	blockRe := regexp.MustCompile("(?m)^```edit[:/ ]([^\\s`]+)\\n([\\s\\S]*?)\\n```")
	var edits []EditBlock

	for _, m := range blockRe.FindAllStringSubmatch(text, -1) {
		if len(m) < 3 {
			continue
		}
		relPath := strings.TrimSpace(m[1])
		body := m[2]

		// Parse individual SEARCH/REPLACE sections within the block.
		edits = append(edits, parseSearchReplacePairs(relPath, body)...)
	}
	return edits
}

// parseSearchReplacePairs extracts <<<SEARCH / === / >>>SEARCH sections from a block body.
func parseSearchReplacePairs(filePath, body string) []EditBlock {
	var edits []EditBlock
	const searchOpen = "<<<SEARCH"
	const separator = "==="
	const searchClose = ">>>SEARCH"

	remaining := body
	for {
		openIdx := strings.Index(remaining, searchOpen)
		if openIdx < 0 {
			break
		}
		remaining = remaining[openIdx+len(searchOpen):]

		// Skip the newline after <<<SEARCH
		if len(remaining) > 0 && remaining[0] == '\n' {
			remaining = remaining[1:]
		}

		sepIdx := strings.Index(remaining, "\n"+separator+"\n")
		if sepIdx < 0 {
			break
		}
		oldText := remaining[:sepIdx]

		remaining = remaining[sepIdx+len("\n"+separator+"\n"):]

		closeIdx := strings.Index(remaining, "\n"+searchClose)
		if closeIdx < 0 {
			// Try without leading newline (empty new text).
			closeIdx = strings.Index(remaining, searchClose)
			if closeIdx < 0 {
				break
			}
			newText := remaining[:closeIdx]
			edits = append(edits, EditBlock{
				FilePath: filePath,
				OldText:  oldText,
				NewText:  strings.TrimRight(newText, "\n"),
			})
			remaining = remaining[closeIdx+len(searchClose):]
			continue
		}

		newText := remaining[:closeIdx]
		edits = append(edits, EditBlock{
			FilePath: filePath,
			OldText:  oldText,
			NewText:  newText,
		})
		remaining = remaining[closeIdx+1+len(searchClose):]
	}
	return edits
}

// applyEdits applies a slice of EditBlocks to files on disk under root.
// Returns the list of files modified and any warnings for edits that couldn't be applied.
func applyEdits(edits []EditBlock, root string) (modified []string, warnings []string) {
	seen := map[string]bool{}

	for _, edit := range edits {
		relPath := edit.FilePath
		if filepath.IsAbs(relPath) || strings.HasPrefix(filepath.Clean(relPath), "..") {
			warnings = append(warnings, fmt.Sprintf("skipped unsafe path: %s", relPath))
			continue
		}

		abs := filepath.Join(root, filepath.Clean(relPath))
		data, err := os.ReadFile(abs)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("%s: cannot read file: %v", relPath, err))
			continue
		}

		content := string(data)
		count := strings.Count(content, edit.OldText)
		if count == 0 {
			warnings = append(warnings, fmt.Sprintf("%s: SEARCH text not found (skipped)", relPath))
			continue
		}
		if count > 1 {
			warnings = append(warnings, fmt.Sprintf("%s: SEARCH text matches %d times (ambiguous, skipped)", relPath, count))
			continue
		}

		content = strings.Replace(content, edit.OldText, edit.NewText, 1)
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			warnings = append(warnings, fmt.Sprintf("%s: write error: %v", relPath, err))
			continue
		}

		if !seen[abs] {
			modified = append(modified, abs)
			seen[abs] = true
		}
	}
	return
}

// extractAndApplyChanges handles both ```edit:filepath (diff) and ```lang:filepath (whole file)
// blocks from model output. Edit blocks are applied as SEARCH/REPLACE patches; whole-file blocks
// overwrite the file entirely (used for new files or when the model ignores edit format).
// Returns all file paths written/modified.
func extractAndApplyChanges(text, root string) []string {
	var allPaths []string
	seen := map[string]bool{}

	// 1. Apply edit blocks first (surgical changes to existing files).
	edits := parseEditBlocks(text)
	if len(edits) > 0 {
		modified, warnings := applyEdits(edits, root)
		for _, w := range warnings {
			fmt.Printf("%s  ⚠ %s%s\n", pColorDim, w, pColorReset)
		}
		for _, p := range modified {
			if !seen[p] {
				allPaths = append(allPaths, p)
				seen[p] = true
			}
		}
	}

	// 2. Apply whole-file blocks (new files, or fallback when model ignores edit format).
	// Skip files that were already modified by edit blocks above.
	wholeFileRe := regexp.MustCompile("(?m)^```[a-zA-Z0-9_+-]*[:/ ]([^\\s`]+)\\n([\\s\\S]*?)\\n```")
	editPathRe := regexp.MustCompile("(?m)^```edit[:/ ]")
	for _, m := range wholeFileRe.FindAllStringSubmatch(text, -1) {
		if len(m) < 3 {
			continue
		}
		// Skip if this is an edit block (already handled above).
		fullMatch := m[0]
		if editPathRe.MatchString(fullMatch) {
			continue
		}

		relPath := strings.TrimSpace(m[1])
		content := m[2]
		if relPath == "" {
			continue
		}
		if filepath.IsAbs(relPath) || strings.HasPrefix(filepath.Clean(relPath), "..") {
			continue
		}

		abs := filepath.Join(root, filepath.Clean(relPath))
		if seen[abs] {
			continue // already modified by an edit block
		}
		seen[abs] = true

		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			continue
		}
		if err := os.WriteFile(abs, []byte(content+"\n"), 0o644); err != nil {
			continue
		}
		allPaths = append(allPaths, abs)
	}

	return allPaths
}
