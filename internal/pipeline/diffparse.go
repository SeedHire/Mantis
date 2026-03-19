package pipeline

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
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

// EditFailure records a single edit that could not be applied.
type EditFailure struct {
	FilePath      string // relative path
	Reason        string // "not found", "ambiguous", "path traversal", "file unreadable"
	SearchPreview string // first 3 lines of SEARCH block
}

// applyEdits applies a slice of EditBlocks to files on disk under root.
// Returns the list of files modified, a count of edits that were skipped,
// and structured failure info for diagnostics.
func applyEdits(edits []EditBlock, root string) (modified []string, skipCount int, failures []EditFailure) {
	seen := map[string]bool{}

	searchPreview := func(old string) string {
		lines := strings.SplitN(old, "\n", 4)
		if len(lines) > 3 {
			lines = lines[:3]
		}
		return strings.Join(lines, "\n")
	}

	for _, edit := range edits {
		relPath := edit.FilePath
		// Strip absolute root prefix if model outputs full paths.
		if filepath.IsAbs(relPath) {
			if rel, err := filepath.Rel(root, relPath); err == nil && !strings.HasPrefix(rel, "..") {
				relPath = rel
			} else {
				skipCount++
				failures = append(failures, EditFailure{relPath, "path traversal", searchPreview(edit.OldText)})
				continue
			}
		}
		abs := filepath.Join(root, filepath.Clean(relPath))
		if rel, err := filepath.Rel(root, abs); err != nil || strings.HasPrefix(rel, "..") {
			skipCount++
			failures = append(failures, EditFailure{relPath, "path traversal", searchPreview(edit.OldText)})
			continue
		}
		data, err := os.ReadFile(abs)
		if err != nil {
			skipCount++
			failures = append(failures, EditFailure{relPath, "file unreadable", searchPreview(edit.OldText)})
			continue
		}

		content := string(data)

		// Normalise \r\n → \n in both content and search text (models sometimes
		// output Windows line endings even on UNIX, or file has mixed endings).
		content = strings.ReplaceAll(content, "\r\n", "\n")
		oldText := strings.ReplaceAll(edit.OldText, "\r\n", "\n")
		newText := strings.ReplaceAll(edit.NewText, "\r\n", "\n")

		// Guard: empty SEARCH text is invalid — skip.
		if strings.TrimSpace(oldText) == "" {
			skipCount++
			failures = append(failures, EditFailure{relPath, "empty search text", ""})
			continue
		}

		// Exact match first.
		count := strings.Count(content, oldText)
		if count == 1 {
			content = strings.Replace(content, oldText, newText, 1)
		} else if count == 0 {
			// Fuzzy fallback: normalise whitespace on both sides and try to find
			// the search text within the file line-by-line.  We rebuild a best-
			// effort replacement by locating the stretch of original lines whose
			// whitespace-normalised form matches the normalised OldText.
			normOld := normalizeWS(oldText)
			if normOld == "" {
				skipCount++
				failures = append(failures, EditFailure{relPath, "empty search text", ""})
				continue
			}
			lines := strings.Split(content, "\n")
			oldLines := strings.Split(oldText, "\n")
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
					sb.WriteString(newText)
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
				trimOld := trimEachLine(oldText)
				for start := 0; start+n <= len(lines); start++ {
					candidate := trimEachLine(strings.Join(lines[start:start+n], "\n"))
					if candidate == trimOld {
						var sb2 strings.Builder
						sb2.WriteString(strings.Join(lines[:start], "\n"))
						if start > 0 {
							sb2.WriteByte('\n')
						}
						sb2.WriteString(newText)
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

			// Tier 2c: best-effort — ≥90% of trimmed lines match consecutively.
			// Only for blocks of 6+ lines to avoid false positives on short snippets.
			// Signature lines (func/class/def/type/interface) must match exactly.
			if !found && n >= 6 {
				trimmedOldLines := strings.Split(oldText, "\n")
				bestStart, bestCount := -1, 0
				for start := 0; start+n <= len(lines); start++ {
					matchCount := 0
					sigFail := false
					for j := 0; j < n; j++ {
						trimmedActual := strings.TrimSpace(lines[start+j])
						trimmedExpected := strings.TrimSpace(trimmedOldLines[j])
						if trimmedActual == trimmedExpected {
							matchCount++
						} else if isSignatureLine(trimmedExpected) {
							// Signature lines must match exactly — abort this window.
							sigFail = true
							break
						}
					}
					if sigFail {
						continue
					}
					if matchCount > bestCount {
						bestStart, bestCount = start, matchCount
					}
				}
				if bestStart >= 0 && float64(bestCount)/float64(n) >= 0.90 {
					var sb3 strings.Builder
					sb3.WriteString(strings.Join(lines[:bestStart], "\n"))
					if bestStart > 0 {
						sb3.WriteByte('\n')
					}
					sb3.WriteString(newText)
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
				failures = append(failures, EditFailure{relPath, "not found", searchPreview(oldText)})
				continue
			}
		} else {
			// Ambiguous — more than one exact match.
			skipCount++
			failures = append(failures, EditFailure{relPath, "ambiguous (multiple matches)", searchPreview(oldText)})
			continue
		}

		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			skipCount++
			failures = append(failures, EditFailure{relPath, "write error", searchPreview(edit.OldText)})
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
// allowOverwrite is an optional set of relative paths that may be overwritten by whole-file blocks
// even if they already exist. Pass nil to use the default 9B guard (block overwrites).
// This is used to allow later pipeline stages to overwrite files written by earlier stages.
func extractAndApplyChanges(text, root string, allowOverwrite ...map[string]bool) ([]string, []string) {
	pipelineWritten := map[string]bool{}
	if len(allowOverwrite) > 0 && allowOverwrite[0] != nil {
		pipelineWritten = allowOverwrite[0]
	}
	var allPaths []string
	var warnings []string
	seen := map[string]bool{}

	// 1. Apply edit blocks first (surgical changes to existing files).
	edits := parseEditBlocks(text)
	if len(edits) > 0 {
		modified, skipped, editFailures := applyEdits(edits, root)
		if skipped > 0 {
			warnings = append(warnings, fmt.Sprintf("%d edit block(s) skipped (SEARCH mismatch)", skipped))
			// 9F: Per-file failure diagnostics — inline warnings for each failure.
			for _, f := range editFailures {
				preview := strings.SplitN(f.SearchPreview, "\n", 2)[0]
				if len(preview) > 60 {
					preview = preview[:60] + "…"
				}
				warnings = append(warnings, fmt.Sprintf("  edit failed: %s — %s (%s)", f.FilePath, f.Reason, preview))
			}
			// Write detailed failures to .mantis/last-failures.log.
			writeFailureLog(root, editFailures)
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

		// 9B: Protect existing files from whole-file overwrite.
		// If the file already exists with content, require edit blocks instead.
		// Exceptions: (1) files written by earlier pipeline stages (in pipelineWritten),
		// (2) small files under 50 lines (model is told to use whole-file for these).
		if info, err := os.Stat(abs); err == nil && info.Size() > 0 && !pipelineWritten[relPath] {
			existing, readErr := os.ReadFile(abs)
			if readErr == nil && strings.TrimSpace(string(existing)) != strings.TrimSpace(content) {
				// Allow whole-file overwrite for small files (<50 lines).
				lineCount := strings.Count(string(existing), "\n") + 1
				if lineCount >= 50 {
					warnings = append(warnings, fmt.Sprintf("skipped whole-file overwrite of existing %s (%d lines) — use edit blocks", relPath, lineCount))
					continue
				}
			}
			// Content is identical (or only whitespace diff) — allow as no-op.
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

// isSignatureLine returns true if the line looks like a function/class/type declaration.
// These lines must match exactly in fuzzy matching to prevent wrong-location replacements.
func isSignatureLine(trimmed string) bool {
	for _, prefix := range []string{"func ", "class ", "def ", "interface ", "type ", "struct ", "enum ", "trait ", "impl ", "pub fn ", "pub struct ", "export function ", "export class ", "export interface ", "export type "} {
		if strings.HasPrefix(trimmed, prefix) {
			return true
		}
	}
	return false
}

// writeFailureLog writes structured failure details to .mantis/last-failures.log
// for post-mortem debugging.
func writeFailureLog(root string, failures []EditFailure) {
	if root == "" || len(failures) == 0 {
		return
	}
	dir := filepath.Join(root, ".mantis")
	_ = os.MkdirAll(dir, 0o755)
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("# Edit Failures — %s\n\n", time.Now().Format("2006-01-02 15:04:05")))
	for i, f := range failures {
		sb.WriteString(fmt.Sprintf("## %d. %s — %s\n", i+1, f.FilePath, f.Reason))
		if f.SearchPreview != "" {
			sb.WriteString("```\n")
			sb.WriteString(f.SearchPreview)
			sb.WriteString("\n```\n\n")
		}
	}
	_ = os.WriteFile(filepath.Join(dir, "last-failures.log"), []byte(sb.String()), 0o644)
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
		// Tier 2c: best-effort — must match applyEdits thresholds (n≥6, 90%, sig check).
		if n >= 6 {
			trimmedOldLines := strings.Split(edit.OldText, "\n")
			for start := 0; start+n <= len(lines); start++ {
				matchCount := 0
				sigFail := false
				for j := 0; j < n; j++ {
					trimmedActual := strings.TrimSpace(lines[start+j])
					trimmedExpected := strings.TrimSpace(trimmedOldLines[j])
					if trimmedActual == trimmedExpected {
						matchCount++
					} else if isSignatureLine(trimmedExpected) {
						sigFail = true
						break
					}
				}
				if sigFail {
					continue
				}
				if float64(matchCount)/float64(n) >= 0.90 {
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
