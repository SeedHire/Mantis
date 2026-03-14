package repl

import (
	"encoding/json"
	"fmt"
	"strings"
)

// RiskLevel classifies a tool action by its reversibility.
type RiskLevel int

const (
	RiskSafe        RiskLevel = iota // read-only, always auto-approve
	RiskReversible                   // edit/write — approve once per session
	RiskDestructive                  // rm, force push — always confirm
)

func (r RiskLevel) String() string {
	switch r {
	case RiskSafe:
		return "safe"
	case RiskReversible:
		return "reversible"
	case RiskDestructive:
		return "destructive"
	}
	return "unknown"
}

// riskBadge returns a colored badge string for the risk level.
func riskBadge(r RiskLevel) string {
	switch r {
	case RiskSafe:
		return "\033[32m● safe\033[0m"
	case RiskReversible:
		return "\033[33m● reversible\033[0m"
	case RiskDestructive:
		return "\033[31m⚠ destructive\033[0m"
	}
	return ""
}

// assessRisk classifies a tool call by its risk level.
func assessRisk(toolName string, argsRaw json.RawMessage) RiskLevel {
	switch toolName {
	// Always safe — read-only operations.
	case "read_file", "search_codebase", "search_files", "find_symbol",
		"list_files", "run_tests", "finish":
		return RiskSafe

	// Reversible — file edits (tracked by oplog checkpoint).
	case "edit_file", "multi_edit_file":
		return RiskReversible

	case "write_file":
		// New files are reversible; overwrites need checking but are still reversible
		// since oplog tracks previous content.
		return RiskReversible

	case "git_stage":
		return RiskReversible

	case "git_commit":
		return RiskReversible

	case "git_branch":
		return RiskSafe

	case "run_bash", "run_command":
		return assessBashRisk(argsRaw)

	default:
		return RiskReversible // unknown tools default to reversible
	}
}

// assessBashRisk examines a bash command for destructive patterns.
func assessBashRisk(argsRaw json.RawMessage) RiskLevel {
	var args struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(argsRaw, &args); err != nil {
		return RiskDestructive // can't parse = treat as dangerous
	}
	cmd := strings.TrimSpace(args.Command)
	lower := strings.ToLower(cmd)

	// Destructive patterns — always confirm.
	destructive := []string{
		"rm -rf", "rm -r ", "rmdir",
		"git push --force", "git push -f ",
		"git reset --hard",
		"git clean -f", "git clean -fd", "git clean -fdx",
		"git checkout .",
		"git branch -D ", "git branch -d ",
		"drop table", "drop database", "truncate ",
		"docker system prune", "docker volume rm",
		"kubectl delete",
		"> /dev/null", ">/dev/null", // output redirection can mask errors
	}
	for _, pat := range destructive {
		if strings.Contains(lower, pat) {
			return RiskDestructive
		}
	}

	// File deletion patterns.
	if strings.HasPrefix(lower, "rm ") {
		return RiskDestructive
	}

	// Everything else is reversible (the allowlist already gates what can run).
	return RiskReversible
}

// PermissionMode controls which tool calls are auto-approved vs prompted.
type PermissionMode int

const (
	ModeDefault         PermissionMode = iota // prompt for file writes + bash
	ModeAutoAcceptEdits                       // auto-approve edits, prompt for bash
	ModePlan                                  // read-only tools only
)

func (m PermissionMode) String() string {
	switch m {
	case ModeDefault:
		return "default"
	case ModeAutoAcceptEdits:
		return "auto-edit"
	case ModePlan:
		return "plan"
	}
	return "unknown"
}

// PromptSuffix returns the mode indicator for the REPL prompt.
func (m PermissionMode) PromptSuffix() string {
	switch m {
	case ModeAutoAcceptEdits:
		return " \033[33m[auto-edit]\033[0m"
	case ModePlan:
		return " \033[36m[plan]\033[0m"
	}
	return ""
}

// IsToolAllowed checks whether a tool call should proceed given the current mode.
// Returns (allowed, reason).
func (m PermissionMode) IsToolAllowed(toolName string, risk RiskLevel) (bool, string) {
	switch m {
	case ModePlan:
		// Plan mode: only safe (read-only) tools allowed.
		if risk != RiskSafe {
			return false, fmt.Sprintf("blocked in plan mode — %s is not a read-only tool", toolName)
		}
		return true, ""
	case ModeAutoAcceptEdits:
		// Auto-edit: auto-approve safe + reversible, prompt for destructive.
		if risk == RiskDestructive {
			return false, fmt.Sprintf("%s is destructive — requires confirmation", toolName)
		}
		return true, ""
	default:
		// Default: safe is auto-approved, everything else needs confirmation.
		return risk == RiskSafe, ""
	}
}

// ParsePermissionMode parses a mode string from user input.
func ParsePermissionMode(s string) (PermissionMode, bool) {
	switch strings.TrimSpace(strings.ToLower(s)) {
	case "default", "d", "":
		return ModeDefault, true
	case "auto-edit", "auto", "a", "autoedit":
		return ModeAutoAcceptEdits, true
	case "plan", "p", "readonly", "read-only":
		return ModePlan, true
	}
	return ModeDefault, false
}

// ScopedApprovalCache tracks user approvals with scope awareness.
// "user approved git push to feature-branch" ≠ approval to push to main.
type ScopedApprovalCache struct {
	approvals map[string]bool // key = "tool:scope"
}

// NewScopedApprovalCache creates a new scoped approval cache.
func NewScopedApprovalCache() *ScopedApprovalCache {
	return &ScopedApprovalCache{approvals: make(map[string]bool)}
}

// Approve records that a tool+scope combination was approved.
func (c *ScopedApprovalCache) Approve(toolName, scope string) {
	c.approvals[approvalKey(toolName, scope)] = true
}

// IsApproved checks if a tool+scope was previously approved.
func (c *ScopedApprovalCache) IsApproved(toolName, scope string) bool {
	return c.approvals[approvalKey(toolName, scope)]
}

func approvalKey(toolName, scope string) string {
	if scope == "" {
		return toolName
	}
	return fmt.Sprintf("%s:%s", toolName, scope)
}

// extractScope determines the scope of a tool call for approval tracking.
func extractScope(toolName string, argsRaw json.RawMessage) string {
	var generic map[string]interface{}
	_ = json.Unmarshal(argsRaw, &generic)

	switch toolName {
	case "edit_file", "multi_edit_file", "write_file", "read_file":
		if p, ok := generic["path"].(string); ok {
			return p
		}
	case "run_bash", "run_command":
		if cmd, ok := generic["command"].(string); ok {
			// For git push, scope is the branch
			if strings.Contains(cmd, "git push") {
				parts := strings.Fields(cmd)
				for i, p := range parts {
					if p == "push" && i+2 < len(parts) {
						return parts[i+2] // branch name
					}
				}
			}
			// For rm, scope is the target path
			if strings.HasPrefix(strings.TrimSpace(cmd), "rm ") {
				return cmd
			}
			return cmd
		}
	case "git_stage":
		if paths, ok := generic["paths"].([]interface{}); ok && len(paths) > 0 {
			if p, ok := paths[0].(string); ok {
				return p
			}
		}
	}
	return ""
}
