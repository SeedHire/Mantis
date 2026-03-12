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
// ** matches zero or more directory segments (e.g. src/**/model.go matches
// src/a/b/c/model.go). Uses segment-level recursive matching.
func matchesGlob(pattern, path string) bool {
	pattern = filepath.ToSlash(pattern)
	path = filepath.ToSlash(path)

	return matchSegments(strings.Split(pattern, "/"), strings.Split(path, "/"))
}

// matchSegments recursively matches pattern segments against path segments.
// A "**" segment matches zero or more path segments.
func matchSegments(patSegs, pathSegs []string) bool {
	for len(patSegs) > 0 {
		seg := patSegs[0]
		if seg == "**" {
			patSegs = patSegs[1:]
			// ** at end matches everything
			if len(patSegs) == 0 {
				return true
			}
			// Try matching the remaining pattern at every position
			for i := 0; i <= len(pathSegs); i++ {
				if matchSegments(patSegs, pathSegs[i:]) {
					return true
				}
			}
			return false
		}
		if len(pathSegs) == 0 {
			return false
		}
		matched, err := filepath.Match(seg, pathSegs[0])
		if err != nil || !matched {
			return false
		}
		patSegs = patSegs[1:]
		pathSegs = pathSegs[1:]
	}
	return len(pathSegs) == 0
}
