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

// normalizeWS collapses all runs of whitespace (including newlines) into a
// single space, used for fuzzy SEARCH matching when exact match fails.
func normalizeWS(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// applyEdits applies a slice of EditBlocks to files on disk under root.
// Returns the list of files modified and a count of edits that were skipped.
// Individual skipped-edit messages are NOT printed here; callers receive the
// skip count and decide how to surface it (to keep terminal output clean).
func applyEdits(edits []EditBlock, root string) (modified []string, skipCount int) {
	seen := map[string]bool{}

	for _, edit := range edits {
		relPath := edit.FilePath
		if filepath.IsAbs(relPath) {
			skipCount++
			continue
		}
		abs := filepath.Join(root, filepath.Clean(relPath))
		if rel, err := filepath.Rel(root, abs); err != nil || strings.HasPrefix(rel, "..") {
			skipCount++
			continue
		}
		data, err := os.ReadFile(abs)
		if err != nil {
			skipCount++
			continue
		}

		content := string(data)

		// Exact match first.
		count := strings.Count(content, edit.OldText)
		if count == 1 {
			content = strings.Replace(content, edit.OldText, edit.NewText, 1)
		} else if count == 0 {
			// Fuzzy fallback: normalise whitespace on both sides and try to find
			// the search text within the file line-by-line.  We rebuild a best-
			// effort replacement by locating the stretch of original lines whose
			// whitespace-normalised form matches the normalised OldText.
			normOld := normalizeWS(edit.OldText)
			if normOld == "" {
				skipCount++
				continue
			}
			lines := strings.Split(content, "\n")
			oldLines := strings.Split(edit.OldText, "\n")
			n := len(oldLines)
			found := false
			for start := 0; start+n <= len(lines); start++ {
				candidate := strings.Join(lines[start:start+n], "\n")
				if normalizeWS(candidate) == normOld {
					// Replace the matched block with NewText.
					var sb strings.Builder
					sb.WriteString(strings.Join(lines[:start], "\n"))
					if start > 0 {
						sb.WriteByte('\n')
					}
					sb.WriteString(edit.NewText)
					if start+n < len(lines) {
						sb.WriteByte('\n')
						sb.WriteString(strings.Join(lines[start+n:], "\n"))
					}
					content = sb.String()
					found = true
					break
				}
			}
			if !found {
				skipCount++
				continue
			}
		} else {
			// Ambiguous — more than one exact match.
			skipCount++
			continue
		}

		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			skipCount++
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
//
// Terminal output is kept minimal: a single batched summary line is printed when
// edit blocks are skipped, instead of one ⚠ line per skipped edit.
func extractAndApplyChanges(text, root string) []string {
	var allPaths []string
	seen := map[string]bool{}

	// 1. Apply edit blocks first (surgical changes to existing files).
	edits := parseEditBlocks(text)
	if len(edits) > 0 {
		modified, skipped := applyEdits(edits, root)
		if skipped > 0 {
			// Single batched warning — not one per skipped edit.
			fmt.Printf("%s  ⚠ %d edit block(s) skipped (SEARCH mismatch — see .mantis/last-pipeline.md)%s\n",
				pColorDim, skipped, pColorReset)
		}
		for _, p := range modified {
			if !seen[p] {
				allPaths = append(allPaths, p)
				seen[p] = true
			}
		}
	}

	// 2. Apply whole-file blocks (new files, or fallback when model ignores edit format).
	// For existing files: silently overwrite — the model fell back to whole-file because
	// edit blocks failed; skipping the whole-file block would leave broken code in place.
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
			continue // already modified by an edit block — don't overwrite
		}
		seen[abs] = true

		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			continue
		}
		// Write unconditionally — new file or silent overwrite for existing files.
		if err := os.WriteFile(abs, []byte(content+"\n"), 0o644); err != nil {
			continue
		}
		allPaths = append(allPaths, abs)
	}

	return allPaths
}
