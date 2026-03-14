package repl

import (
	"encoding/json"
	"testing"
)

func TestAssessRisk_SafeTools(t *testing.T) {
	safe := []string{"read_file", "search_codebase", "search_files", "find_symbol", "list_files", "run_tests", "finish"}
	for _, tool := range safe {
		if r := assessRisk(tool, nil); r != RiskSafe {
			t.Errorf("assessRisk(%q) = %v, want safe", tool, r)
		}
	}
}

func TestAssessRisk_ReversibleTools(t *testing.T) {
	reversible := []string{"edit_file", "multi_edit_file", "write_file", "git_stage", "git_commit"}
	for _, tool := range reversible {
		if r := assessRisk(tool, nil); r != RiskReversible {
			t.Errorf("assessRisk(%q) = %v, want reversible", tool, r)
		}
	}
}

func TestAssessRisk_DestructiveBash(t *testing.T) {
	destructive := []struct{ cmd string }{
		{"rm -rf /tmp/foo"},
		{"git push --force origin main"},
		{"git reset --hard HEAD~1"},
		{"git clean -fdx"},
		{"DROP TABLE users"},
		{"kubectl delete pod foo"},
	}
	for _, tc := range destructive {
		args, _ := json.Marshal(map[string]string{"command": tc.cmd})
		if r := assessRisk("run_bash", args); r != RiskDestructive {
			t.Errorf("assessBashRisk(%q) = %v, want destructive", tc.cmd, r)
		}
	}
}

func TestAssessRisk_SafeBash(t *testing.T) {
	safe := []struct{ cmd string }{
		{"go test ./..."},
		{"git status"},
		{"npm run build"},
	}
	for _, tc := range safe {
		args, _ := json.Marshal(map[string]string{"command": tc.cmd})
		if r := assessRisk("run_bash", args); r == RiskDestructive {
			t.Errorf("assessBashRisk(%q) = destructive, want safe or reversible", tc.cmd)
		}
	}
}

func TestScopedApprovalCache(t *testing.T) {
	c := NewScopedApprovalCache()
	if c.IsApproved("git_push", "main") {
		t.Error("should not be approved before approval")
	}
	c.Approve("git_push", "feature-branch")
	if c.IsApproved("git_push", "main") {
		t.Error("approval for feature-branch should not grant access to main")
	}
	if !c.IsApproved("git_push", "feature-branch") {
		t.Error("should be approved after approval")
	}
}

func TestExtractScope(t *testing.T) {
	args, _ := json.Marshal(map[string]string{"path": "src/main.go"})
	scope := extractScope("edit_file", args)
	if scope != "src/main.go" {
		t.Errorf("extractScope(edit_file) = %q, want src/main.go", scope)
	}

	args, _ = json.Marshal(map[string]string{"command": "git push origin feature"})
	scope = extractScope("run_bash", args)
	if scope != "feature" {
		t.Errorf("extractScope(git push) = %q, want feature", scope)
	}
}

func TestPermissionMode_Plan(t *testing.T) {
	mode := ModePlan
	// Safe tool should be allowed.
	if ok, _ := mode.IsToolAllowed("read_file", RiskSafe); !ok {
		t.Error("plan mode should allow read_file")
	}
	// Reversible tool should be blocked.
	if ok, _ := mode.IsToolAllowed("edit_file", RiskReversible); ok {
		t.Error("plan mode should block edit_file")
	}
}

func TestPermissionMode_AutoEdit(t *testing.T) {
	mode := ModeAutoAcceptEdits
	// Reversible should be allowed.
	if ok, _ := mode.IsToolAllowed("edit_file", RiskReversible); !ok {
		t.Error("auto-edit mode should allow edit_file")
	}
	// Destructive should be blocked.
	if ok, _ := mode.IsToolAllowed("run_bash", RiskDestructive); ok {
		t.Error("auto-edit mode should block destructive bash")
	}
}

func TestParsePermissionMode(t *testing.T) {
	cases := []struct {
		input string
		want  PermissionMode
		ok    bool
	}{
		{"default", ModeDefault, true},
		{"auto-edit", ModeAutoAcceptEdits, true},
		{"auto", ModeAutoAcceptEdits, true},
		{"plan", ModePlan, true},
		{"p", ModePlan, true},
		{"invalid", ModeDefault, false},
	}
	for _, tc := range cases {
		got, ok := ParsePermissionMode(tc.input)
		if ok != tc.ok || got != tc.want {
			t.Errorf("ParsePermissionMode(%q) = (%v, %v), want (%v, %v)", tc.input, got, ok, tc.want, tc.ok)
		}
	}
}

func TestRiskBadge(t *testing.T) {
	if b := riskBadge(RiskSafe); b == "" {
		t.Error("expected non-empty badge for safe")
	}
	if b := riskBadge(RiskDestructive); b == "" {
		t.Error("expected non-empty badge for destructive")
	}
}
