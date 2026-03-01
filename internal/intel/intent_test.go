package intel

import (
	"testing"
)

func TestInferTypeConventional(t *testing.T) {
	tests := []struct {
		msg      string
		wantType string
	}{
		{"Add login page", "feat"},
		{"fix null pointer in auth", "fix"},
		{"Refactor router logic", "refactor"},
		{"Update documentation", "docs"},
		{"add unit tests for auth", "feat"}, // "add" prefix matches before "test"
		{"Random commit message", "chore"},
		{"implement retry mechanism", "feat"},
		{"Fix broken login flow", "fix"},
		{"clean up dead code", "refactor"},
	}
	for _, tt := range tests {
		got := inferType(tt.msg)
		if got != tt.wantType {
			t.Errorf("inferType(%q) = %q, want %q", tt.msg, got, tt.wantType)
		}
	}
}

func TestConventionalCommitRegex(t *testing.T) {
	tests := []struct {
		subject   string
		wantMatch bool
		wantType  string
		wantScope string
	}{
		{"feat: add login page", true, "feat", ""},
		{"fix(auth): null pointer", true, "fix", "auth"},
		{"refactor!: breaking change", true, "refactor", ""},
		{"docs(api): update endpoints", true, "docs", "api"},
		{"random message", false, "", ""},
	}
	for _, tt := range tests {
		m := conventionalRe.FindStringSubmatch(tt.subject)
		if tt.wantMatch {
			if m == nil {
				t.Errorf("expected match for %q, got nil", tt.subject)
				continue
			}
			if m[1] != tt.wantType {
				t.Errorf("type for %q = %q, want %q", tt.subject, m[1], tt.wantType)
			}
			if m[2] != tt.wantScope {
				t.Errorf("scope for %q = %q, want %q", tt.subject, m[2], tt.wantScope)
			}
		} else if m != nil {
			t.Errorf("expected no match for %q, got %v", tt.subject, m)
		}
	}
}

func TestIssueRefRegex(t *testing.T) {
	tests := []struct {
		subject string
		want    int
	}{
		{"fix: resolve #42", 1},
		{"closes #10, fixes #20", 2},
		{"no refs here", 0},
		{"feat: GH-123 and #456", 2},
	}
	for _, tt := range tests {
		refs := issueRefRe.FindAllString(tt.subject, -1)
		if len(refs) != tt.want {
			t.Errorf("issue refs in %q: got %d, want %d", tt.subject, len(refs), tt.want)
		}
	}
}
