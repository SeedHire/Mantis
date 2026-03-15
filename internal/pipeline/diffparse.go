package pipeline

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// EditBlock represents a single SEARCH/REPLACE edit within an ```edit:filepath block.
type EditBlock struct {
	FilePath string
	OldText  string
	NewText  string
}

// codeBlock represents a single fenced code block extracted from model output.
type codeBlock struct {
	lang   string // language tag (e.g. "go", "typescript", "edit")
	path   string // file path from the fence header
	body   string // content between the fences
	isEdit bool   // true if this is an ```edit:filepath block
}

// parseCodeBlocks extracts fenced code blocks using line-by-line scanning.
// This avoids regex [\s\S]*? which truncates content containing nested backticks
// (e.g. JS/TS template literals). A block ends ONLY on a bare "```" line
// (triple backtick with nothing after it), which never appears inside real code.
func parseCodeBlocks(text string) []codeBlock {
	lines := strings.Split(text, "\n")
	var blocks []codeBlock
	var current *codeBlock
	var bodyLines []string

	for _, line := range lines {
		if current == nil {
			// Look for opening fence: ```lang:path or ```lang/path or ```lang path
			if !strings.HasPrefix(line, "```") {
				continue
			}
			header := line[3:]
			if header == "" || header == "`" {
				continue // bare ``` or ```` — not an opener with a path
			}
			// Extract lang and path from header.
			// Formats: "edit:filepath", "go:filepath", "typescript/filepath", "go filepath"
			lang, path := parseFenceHeader(header)
			if path == "" {
				continue // no file path — skip (plain ```go code blocks)
			}
			current = &codeBlock{
				lang:   lang,
				path:   strings.TrimSpace(path),
				isEdit: lang == "edit",
			}
			bodyLines = nil
		} else {
			// Inside a block — check for closing fence.
			// A bare "```" (with optional trailing whitespace) closes the block.
			trimmed := strings.TrimRight(line, " \t")
			if trimmed == "```" {
				current.body = strings.Join(bodyLines, "\n")
				blocks = append(blocks, *current)
				current = nil
				bodyLines = nil
			} else {
				bodyLines = append(bodyLines, line)
			}
		}
	}
	return blocks
}

// parseFenceHeader splits a fence header like "go:filepath" or "edit/filepath" into (lang, path).
func parseFenceHeader(header string) (string, string) {
	// Try colon separator first (most common): ```go:src/main.go
	if idx := strings.IndexByte(header, ':'); idx > 0 {
		return header[:idx], header[idx+1:]
	}
	// Try slash separator: ```go/src/main.go
	if idx := strings.IndexByte(header, '/'); idx > 0 {
		lang := header[:idx]
		// Only treat as lang/path if the lang part looks like a language tag (no dots, short).
		if len(lang) <= 12 && !strings.ContainsAny(lang, ". \t") {
			return lang, header[idx+1:]
		}
	}
	// Try space separator: ```go src/main.go
	if idx := strings.IndexByte(header, ' '); idx > 0 {
		return header[:idx], strings.TrimSpace(header[idx+1:])
	}
	return header, ""
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
	var edits []EditBlock
	for _, block := range parseCodeBlocks(text) {
		if !block.isEdit {
			continue
		}
		edits = append(edits, parseSearchReplacePairs(block.path, block.body)...)
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

// trimEachLine trims leading/trailing whitespace from each line independently
// and rejoins. Used for Tier 2b matching (indentation-only mismatches).
func trimEachLine(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSpace(l)
	}
	return strings.Join(lines, "\n")
}

// applyEdits applies a slice of EditBlocks to files on disk under root.
// Returns the list of files modified and a count of edits that were skipped.
// Individual skipped-edit messages are NOT printed here; callers receive the
// skip count and decide how to surface it (to keep terminal output clean).
func applyEdits(edits []EditBlock, root string) (modified []string, skipCount int) {
	seen := map[string]bool{}

	for _, edit := range edits {
		relPath := edit.FilePath
		// Strip absolute root prefix if model outputs full paths.
		if filepath.IsAbs(relPath) {
			if rel, err := filepath.Rel(root, relPath); err == nil && !strings.HasPrefix(rel, "..") {
				relPath = rel
			} else {
				skipCount++
				continue
			}
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
			// Tier 2b: per-line trim — catches indentation-only mismatches.
			if !found {
				trimOld := trimEachLine(edit.OldText)
				for start := 0; start+n <= len(lines); start++ {
					candidate := trimEachLine(strings.Join(lines[start:start+n], "\n"))
					if candidate == trimOld {
						var sb2 strings.Builder
						sb2.WriteString(strings.Join(lines[:start], "\n"))
						if start > 0 {
							sb2.WriteByte('\n')
						}
						sb2.WriteString(edit.NewText)
						if start+n < len(lines) {
							sb2.WriteByte('\n')
							sb2.WriteString(strings.Join(lines[start+n:], "\n"))
						}
						content = sb2.String()
						found = true
						break
					}
				}
			}

			// Tier 2c: best-effort — ≥70% of trimmed lines match consecutively.
			// Only for blocks of 4+ lines to avoid false positives on short snippets.
			if !found && n >= 4 {
				trimmedOldLines := strings.Split(edit.OldText, "\n")
				bestStart, bestCount := -1, 0
				for start := 0; start+n <= len(lines); start++ {
					matchCount := 0
					for j := 0; j < n; j++ {
						if strings.TrimSpace(lines[start+j]) == strings.TrimSpace(trimmedOldLines[j]) {
							matchCount++
						}
					}
					if matchCount > bestCount {
						bestStart, bestCount = start, matchCount
					}
				}
				if bestStart >= 0 && float64(bestCount)/float64(n) >= 0.70 {
					var sb3 strings.Builder
					sb3.WriteString(strings.Join(lines[:bestStart], "\n"))
					if bestStart > 0 {
						sb3.WriteByte('\n')
					}
					sb3.WriteString(edit.NewText)
					if bestStart+n < len(lines) {
						sb3.WriteByte('\n')
						sb3.WriteString(strings.Join(lines[bestStart+n:], "\n"))
					}
					content = sb3.String()
					found = true
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
// Returns (writtenPaths, warnings). Warnings are collected instead of printed directly,
// so the caller can route them through a coordinated renderer.
func extractAndApplyChanges(text, root string) ([]string, []string) {
	var allPaths []string
	var warnings []string
	seen := map[string]bool{}

	// 1. Apply edit blocks first (surgical changes to existing files).
	edits := parseEditBlocks(text)
	if len(edits) > 0 {
		modified, skipped := applyEdits(edits, root)
		if skipped > 0 {
			warnings = append(warnings, fmt.Sprintf("%d edit block(s) skipped (SEARCH mismatch — see .mantis/last-pipeline.md)", skipped))
		}
		for _, p := range modified {
			if !seen[p] {
				allPaths = append(allPaths, p)
				seen[p] = true
			}
		}
	}

	// 2. Apply whole-file blocks (new files, or fallback when model ignores edit format).
	// Uses line-by-line parseCodeBlocks to avoid nested-backtick truncation.
	for _, block := range parseCodeBlocks(text) {
		if block.isEdit {
			continue // already handled above
		}

		relPath := strings.TrimSpace(block.path)
		content := block.body
		if relPath == "" {
			continue
		}
		// Strip absolute root prefix if model outputs full paths like /Users/.../project/src/file.py
		if filepath.IsAbs(relPath) {
			if rel, err := filepath.Rel(root, relPath); err == nil && !strings.HasPrefix(rel, "..") {
				relPath = rel
			} else {
				continue // truly external path — skip
			}
		}
		if strings.HasPrefix(filepath.Clean(relPath), "..") {
			continue
		}
		// Strip shell command fragments that models sometimes output as paths.
		if strings.Contains(relPath, " ") && !strings.Contains(relPath, string(filepath.Separator)) {
			continue // "install -r requirements.txt" is not a file path
		}

		abs := filepath.Join(root, filepath.Clean(relPath))
		if seen[abs] {
			continue // already modified by an edit block — don't overwrite
		}
		seen[abs] = true

		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			continue
		}
		// Fix 3: conditional newline — don't double-append if content already ends with \n.
		if !strings.HasSuffix(content, "\n") {
			content += "\n"
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			continue
		}
		allPaths = append(allPaths, abs)
	}

	// UX-7: diagnostic when no files could be extracted.
	if len(allPaths) == 0 {
		fences := countCodeFences(text)
		if fences > 0 {
			warnings = append(warnings, fmt.Sprintf("0 files written (%d code fence(s) found but none had valid file paths)", fences))
		}
	}

	return allPaths, warnings
}

// collectFailedEdits parses edit blocks from model output, tries matching each
// against the actual file on disk, and returns a map of relPath → actual file content
// for every file that has at least one failing edit. Used by retryFailedEdits to
// re-prompt the model with real file content.
func collectFailedEdits(text, root string) map[string]string {
	edits := parseEditBlocks(text)
	if len(edits) == 0 {
		return nil
	}

	failed := map[string]string{}
	for _, edit := range edits {
		relPath := edit.FilePath
		if filepath.IsAbs(relPath) {
			if rel, err := filepath.Rel(root, relPath); err == nil && !strings.HasPrefix(rel, "..") {
				relPath = rel
			} else {
				continue
			}
		}
		abs := filepath.Join(root, filepath.Clean(relPath))
		if rel, err := filepath.Rel(root, abs); err != nil || strings.HasPrefix(rel, "..") {
			continue
		}

		data, err := os.ReadFile(abs)
		if err != nil {
			continue
		}
		content := string(data)

		// Check if this edit would match (exact or any fuzzy tier).
		if strings.Count(content, edit.OldText) == 1 {
			continue // exact match — not a failure
		}
		// Fuzzy: normalizeWS
		normOld := normalizeWS(edit.OldText)
		lines := strings.Split(content, "\n")
		oldLines := strings.Split(edit.OldText, "\n")
		n := len(oldLines)
		matched := false
		for start := 0; start+n <= len(lines); start++ {
			candidate := strings.Join(lines[start:start+n], "\n")
			if normalizeWS(candidate) == normOld {
				matched = true
				break
			}
		}
		if matched {
			continue
		}
		// Tier 2b: line-trimmed
		trimOld := trimEachLine(edit.OldText)
		for start := 0; start+n <= len(lines); start++ {
			candidate := trimEachLine(strings.Join(lines[start:start+n], "\n"))
			if candidate == trimOld {
				matched = true
				break
			}
		}
		if matched {
			continue
		}
		// Tier 2c: best-effort
		if n >= 4 {
			trimmedOldLines := strings.Split(edit.OldText, "\n")
			for start := 0; start+n <= len(lines); start++ {
				matchCount := 0
				for j := 0; j < n; j++ {
					if strings.TrimSpace(lines[start+j]) == strings.TrimSpace(trimmedOldLines[j]) {
						matchCount++
					}
				}
				if float64(matchCount)/float64(n) >= 0.70 {
					matched = true
					break
				}
			}
		}
		if matched {
			continue
		}

		// This edit would fail — record the file's actual content.
		failed[relPath] = content
	}

	if len(failed) == 0 {
		return nil
	}
	return failed
}
