package linter

import (
	"path/filepath"
	"strings"

	"github.com/seedhire/mantis/internal/config"
)

// Violation represents a single lint rule breach.
type Violation struct {
	Rule     string // rule name
	Severity string // "error" | "warning"
	File     string // file that violated
	Line     int    // line number (0 if unknown)
	Message  string // human readable
}

// DisallowPatterns extracts disallow_import as a slice, handling string or []interface{}.
func DisallowPatterns(rule config.LintRule) []string {
	if rule.DisallowImport == nil {
		return nil
	}
	switch v := rule.DisallowImport.(type) {
	case string:
		return []string{v}
	case []interface{}:
		patterns := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				patterns = append(patterns, s)
			}
		}
		return patterns
	}
	return nil
}

// matchesGlob matches path against a glob pattern, supporting ** wildcards.
func matchesGlob(pattern, path string) bool {
	pattern = filepath.ToSlash(pattern)
	path = filepath.ToSlash(path)

	// Handle pattern ending with /**
	if strings.HasSuffix(pattern, "/**") {
		prefix := strings.TrimSuffix(pattern, "/**")
		return strings.HasPrefix(path, prefix+"/")
	}

	// Handle pattern ending with /*
	if strings.HasSuffix(pattern, "/*") {
		dir := strings.TrimSuffix(pattern, "/*")
		return filepath.ToSlash(filepath.Dir(path)) == dir
	}

	// Standard glob
	matched, err := filepath.Match(pattern, path)
	if err == nil && matched {
		return true
	}

	// If pattern contains **, try matching suffixes
	if strings.Contains(pattern, "**") {
		normalized := strings.ReplaceAll(pattern, "**", "*")
		matched, err = filepath.Match(normalized, path)
		if err == nil && matched {
			return true
		}
	}

	return false
}
