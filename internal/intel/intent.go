package intel

import (
	"fmt"
	"os/exec"
	"regexp"
	"sort"
	"strings"
)

// CommitIntent represents the parsed intent from a git commit message.
type CommitIntent struct {
	Hash         string
	Author       string
	Type         string // feat, fix, refactor, docs, chore, test, etc.
	Scope        string // optional scope from conventional commit
	Summary      string
	IssueRefs    []string // #42, GH-123, etc.
	FilesChanged []string
}

// IntentSummary aggregates intent data for a file or symbol.
type IntentSummary struct {
	Path          string
	Intents       []CommitIntent
	FeatureCount  int
	FixCount      int
	RefactorCount int
	TestCount     int
	TodoCount     int // from code TODO/FIXME/HACK comments
}

// TodoItem represents a TODO/FIXME/HACK found in source code.
type TodoItem struct {
	File    string
	Line    int
	Type    string // TODO, FIXME, HACK, XXX
	Comment string
}

var conventionalRe = regexp.MustCompile(`^(\w+)(?:\(([^)]+)\))?[!]?:\s*(.+)`)
var issueRefRe = regexp.MustCompile(`(?:#|GH-)(\d+)`)

// ParseCommitIntent extracts structured intent from git log output.
func ParseCommitIntent(root string, path string, limit int) ([]CommitIntent, error) {
	if limit <= 0 {
		limit = 50
	}

	args := []string{"-C", root, "log",
		fmt.Sprintf("-%d", limit),
		"--format=COMMIT|%h|%an|%s",
		"--name-only",
	}
	if path != "" {
		args = append(args, "--", path)
	}

	out, err := exec.Command("git", args...).Output()
	if err != nil {
		return nil, err
	}

	var intents []CommitIntent
	var current *CommitIntent

	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		if strings.HasPrefix(line, "COMMIT|") {
			if current != nil {
				intents = append(intents, *current)
			}
			parts := strings.SplitN(line, "|", 4)
			if len(parts) < 4 {
				current = nil
				continue
			}

			ci := CommitIntent{
				Hash:    parts[1],
				Author:  parts[2],
				Summary: parts[3],
			}

			// Parse conventional commit format.
			if m := conventionalRe.FindStringSubmatch(parts[3]); m != nil {
				ci.Type = strings.ToLower(m[1])
				ci.Scope = m[2]
				ci.Summary = m[3]
			} else {
				ci.Type = inferType(parts[3])
			}

			// Extract issue references.
			if refs := issueRefRe.FindAllString(parts[3], -1); len(refs) > 0 {
				ci.IssueRefs = refs
			}

			current = &ci
		} else if current != nil {
			current.FilesChanged = append(current.FilesChanged, line)
		}
	}
	if current != nil {
		intents = append(intents, *current)
	}

	return intents, nil
}

// IntentFor builds an intent summary for a specific file path.
func IntentFor(root, path string) (*IntentSummary, error) {
	intents, err := ParseCommitIntent(root, path, 100)
	if err != nil {
		return nil, err
	}

	summary := &IntentSummary{
		Path:    path,
		Intents: intents,
	}

	for _, ci := range intents {
		switch ci.Type {
		case "feat":
			summary.FeatureCount++
		case "fix":
			summary.FixCount++
		case "refactor":
			summary.RefactorCount++
		case "test":
			summary.TestCount++
		}
	}

	return summary, nil
}

// FindTodos scans source files for TODO/FIXME/HACK/XXX comments.
func FindTodos(root string) ([]TodoItem, error) {
	out, err := exec.Command("grep", "-rn",
		"-E", `(TODO|FIXME|HACK|XXX)\b`,
		"--include=*.go", "--include=*.ts", "--include=*.tsx",
		"--include=*.js", "--include=*.jsx", "--include=*.py",
		"--include=*.rs", "--include=*.java",
		root,
	).Output()
	if err != nil {
		// grep returns exit 1 for no matches.
		if len(out) == 0 {
			return nil, nil
		}
	}

	todoRe := regexp.MustCompile(`(TODO|FIXME|HACK|XXX)\b[:\s]*(.*)`)
	var items []TodoItem

	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// Format: file:line:content
		colonIdx := strings.Index(line, ":")
		if colonIdx < 0 {
			continue
		}
		rest := line[colonIdx+1:]
		secondColon := strings.Index(rest, ":")
		if secondColon < 0 {
			continue
		}

		file := line[:colonIdx]
		lineNumStr := rest[:secondColon]
		content := rest[secondColon+1:]

		lineNum := 0
		fmt.Sscanf(lineNumStr, "%d", &lineNum)

		// Strip root prefix for cleaner display.
		if strings.HasPrefix(file, root) {
			file = strings.TrimPrefix(file, root)
			file = strings.TrimPrefix(file, "/")
		}

		if m := todoRe.FindStringSubmatch(content); m != nil {
			items = append(items, TodoItem{
				File:    file,
				Line:    lineNum,
				Type:    m[1],
				Comment: strings.TrimSpace(m[2]),
			})
		}
	}

	return items, nil
}

// SpecGaps finds intent declared in commits (feat: X) but potentially
// incomplete — files with many feat commits but also recent fix commits.
func SpecGaps(root string, limit int) ([]IntentSummary, error) {
	intents, err := ParseCommitIntent(root, "", 200)
	if err != nil {
		return nil, err
	}

	// Group by file.
	byFile := make(map[string]*IntentSummary)
	for _, ci := range intents {
		for _, f := range ci.FilesChanged {
			s, ok := byFile[f]
			if !ok {
				s = &IntentSummary{Path: f}
				byFile[f] = s
			}
			s.Intents = append(s.Intents, ci)
			switch ci.Type {
			case "feat":
				s.FeatureCount++
			case "fix":
				s.FixCount++
			case "refactor":
				s.RefactorCount++
			case "test":
				s.TestCount++
			}
		}
	}

	// Find files with feature intent but many fixes (instability signal).
	var gaps []IntentSummary
	for _, s := range byFile {
		if s.FeatureCount > 0 && s.FixCount >= s.FeatureCount {
			gaps = append(gaps, *s)
		}
	}

	sort.Slice(gaps, func(i, j int) bool {
		return gaps[i].FixCount > gaps[j].FixCount
	})

	if limit > 0 && limit < len(gaps) {
		gaps = gaps[:limit]
	}

	return gaps, nil
}

// inferType guesses commit type from non-conventional commit messages.
func inferType(msg string) string {
	lower := strings.ToLower(msg)
	switch {
	case strings.HasPrefix(lower, "fix") || strings.Contains(lower, "bug"):
		return "fix"
	case strings.HasPrefix(lower, "add") || strings.Contains(lower, "implement") || strings.Contains(lower, "feature"):
		return "feat"
	case strings.Contains(lower, "refactor") || strings.Contains(lower, "clean"):
		return "refactor"
	case strings.Contains(lower, "test"):
		return "test"
	case strings.Contains(lower, "doc") || strings.Contains(lower, "readme"):
		return "docs"
	default:
		return "chore"
	}
}
