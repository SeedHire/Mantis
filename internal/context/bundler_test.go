package context

import (
	"strings"
	"testing"
	"time"
)

// ── baseWithoutExt ────────────────────────────────────────────────────────────

func TestBaseWithoutExt(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"internal/router/router.go", "router"},
		{"main.go", "main"},
		{"embeddings_test.go", "embeddings_test"},
		{"noext", "noext"},
		{"dir/sub/file.ts", "file"},
		{"path/to/FILE.PY", "FILE"},
	}
	for _, tt := range tests {
		got := baseWithoutExt(tt.input)
		if got != tt.want {
			t.Errorf("baseWithoutExt(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

// ── RenderMarkdown ────────────────────────────────────────────────────────────

func TestRenderMarkdown(t *testing.T) {
	b := &Bundler{root: ""}
	bundle := &Bundle{
		Symbol:   "TestFunc",
		MaxDepth: 5,
		Files: []BundleFile{
			{Path: "main.go", Depth: 0, Content: "package main\n"},
			{Path: "util.go", Depth: 1, Content: "package util\n"},
			{Path: "helper.go", Depth: 2, Content: "package helper\n"},
		},
		Tokens: 180,
	}

	md := b.RenderMarkdown(bundle)

	if !strings.Contains(md, "# Context Bundle: TestFunc") {
		t.Error("markdown should contain bundle header")
	}
	if !strings.Contains(md, "## Entry Point") {
		t.Error("markdown should contain Entry Point section")
	}
	if !strings.Contains(md, "## Direct Dependencies") {
		t.Error("markdown should contain Direct Dependencies section")
	}
	if !strings.Contains(md, "## Indirect Dependencies") {
		t.Error("markdown should contain Indirect Dependencies section")
	}
	if !strings.Contains(md, "main.go") {
		t.Error("markdown should contain entry file main.go")
	}
	if !strings.Contains(md, "util.go") {
		t.Error("markdown should contain depth-1 file util.go")
	}
	if !strings.Contains(md, "package main") {
		t.Error("markdown should include file content")
	}
}

// ── scoreFile ─────────────────────────────────────────────────────────────────

func TestScoreFile(t *testing.T) {
	now := time.Now().Unix()
	old := time.Now().Add(-120 * 24 * time.Hour).Unix() // 120 days ago

	tests := []struct {
		name       string
		path       string
		depth      int
		content    string
		modified   int64
		entryBase  string
		churnBonus int
		minScore   int // minimum expected
		maxScore   int // maximum expected
	}{
		{
			name:      "depth0 entry point small recent",
			path:      "internal/auth/auth.go",
			depth:     0,
			content:   strings.Repeat("x", 500), // small file
			modified:  now,
			minScore:  12, // 10 (depth) + 1 (small) + 1 (recent, truncated from 1.999)
			maxScore:  13,
		},
		{
			name:      "depth1 gets 8 base",
			path:      "internal/util/util.go",
			depth:     1,
			content:   strings.Repeat("x", 5000),
			modified:  now,
			minScore:  9, // 8 + 1 (recent)
			maxScore:  10,
		},
		{
			name:      "depth2 gets 5 base",
			path:      "internal/helper/helper.go",
			depth:     2,
			content:   strings.Repeat("x", 5000),
			modified:  now,
			minScore:  6, // 5 + 1 (recent)
			maxScore:  7,
		},
		{
			name:      "default depth gets 3 base",
			path:      "internal/other/other.go",
			depth:     5,
			content:   strings.Repeat("x", 5000),
			modified:  now,
			minScore:  4, // 3 + 1 (recent)
			maxScore:  5,
		},
		{
			name:      "large file penalty",
			path:      "internal/big/big.go",
			depth:     0,
			content:   strings.Repeat("x", 60000), // >50KB → -3
			modified:  now,
			minScore:  8, // 10 - 3 + 1 (recent)
			maxScore:  9,
		},
		{
			name:      "config file demotion",
			path:      "go.sum",
			depth:     0,
			content:   "hash data",
			modified:  now,
			minScore:  6, // 10 + 1 (small) + 1 (recent) - 5 (demotion) = 7
			maxScore:  8,
		},
		{
			name:      "co-located test boost",
			path:      "internal/auth/auth_test.go",
			depth:     1,
			content:   strings.Repeat("x", 500),
			modified:  now,
			entryBase: "auth", // matches auth_test → auth
			minScore:  13,     // 8 + 1 + 2 + 3
			maxScore:  16,
		},
		{
			name:      "unrelated test demotion",
			path:      "internal/other/other_test.go",
			depth:     1,
			content:   strings.Repeat("x", 500),
			modified:  now,
			entryBase: "auth",
			minScore:  1, // 8 + 1 + 2 - 4 = 7, but floored at 1 only if negative
			maxScore:  10,
		},
		{
			name:      "churn bonus passthrough",
			path:      "internal/auth/auth.go",
			depth:     0,
			content:   strings.Repeat("x", 500),
			modified:  now,
			churnBonus: 4,
			minScore:  16, // 10 + 1 (small) + 1 (recent) + 4 (churn)
			maxScore:  17,
		},
		{
			name:      "old file gets 0 recency",
			path:      "internal/legacy/legacy.go",
			depth:     1,
			content:   strings.Repeat("x", 5000),
			modified:  old,
			minScore:  8,
			maxScore:  9,
		},
		{
			name:      "types file boost",
			path:      "internal/model/types.go",
			depth:     1,
			content:   strings.Repeat("x", 5000),
			modified:  now,
			minScore:  11, // 8 + 1 (recent) + 2 (types boost)
			maxScore:  12,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := scoreFile(tt.path, tt.depth, tt.content, tt.modified, tt.entryBase, tt.churnBonus)
			if got < tt.minScore || got > tt.maxScore {
				t.Errorf("scoreFile(%q, depth=%d) = %d, want [%d, %d]",
					tt.path, tt.depth, got, tt.minScore, tt.maxScore)
			}
			if got < 1 {
				t.Errorf("scoreFile must always return ≥1, got %d", got)
			}
		})
	}
}

func TestRenderMarkdownEmpty(t *testing.T) {
	b := &Bundler{root: ""}
	bundle := &Bundle{Symbol: "Empty", Files: nil, Tokens: 0}
	md := b.RenderMarkdown(bundle)
	if !strings.Contains(md, "# Context Bundle: Empty") {
		t.Error("empty bundle should still render header")
	}
}
