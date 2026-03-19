package pipeline

import (
	"fmt"
	"strings"
)

// suspiciousDeps are packages that indicate over-engineering when not requested.
// Key: package name fragment. Value: category for the warning message.
var suspiciousDeps = map[string]string{
	"jwt":          "authentication",
	"oauth":        "authentication",
	"passport":     "authentication",
	"bcrypt":       "authentication",
	"jsonwebtoken": "authentication",
	"gorm":         "ORM",
	"prisma":       "ORM",
	"sequelize":    "ORM",
	"typeorm":      "ORM",
	"mongoose":     "ORM",
	"sqlalchemy":   "ORM",
	"zap":          "logging framework",
	"logrus":       "logging framework",
	"winston":      "logging framework",
	"pino":         "logging framework",
	"redis":        "caching layer",
	"kafka":        "message queue",
	"rabbitmq":     "message queue",
	"elasticsearch": "search engine",
	"prometheus":    "metrics",
	"datadog":      "metrics",
	"sentry":       "error tracking",
}

// checkSuspiciousDeps scans code output for packages that weren't requested.
// Returns warning strings for each suspicious dependency found.
func checkSuspiciousDeps(code, userRequest string) []string {
	lower := strings.ToLower(userRequest)
	codeLower := strings.ToLower(code)
	var warnings []string

	for dep, category := range suspiciousDeps {
		// Skip if the user explicitly asked for this dep.
		if strings.Contains(lower, dep) {
			continue
		}
		// Check if the code imports/requires this dep.
		if strings.Contains(codeLower, dep) {
			warnings = append(warnings, fmt.Sprintf("⚠ unnecessary %s dependency (%s) — not requested by user", category, dep))
		}
	}
	return warnings
}

// checkGoGenerics scans Go code for [T any] type params used with == or !=.
// These should use [T comparable] instead.
func checkGoGenerics(code string) []string {
	var warnings []string
	lines := strings.Split(code, "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		// Look for function/type declarations with [T any] or [T, U any]
		if (strings.Contains(trimmed, "[T any]") || strings.Contains(trimmed, " any]")) &&
			(strings.HasPrefix(trimmed, "func ") || strings.HasPrefix(trimmed, "type ")) {
			// Check next ~20 lines for == or != usage with the type param
			for j := i + 1; j < len(lines) && j < i+20; j++ {
				if strings.Contains(lines[j], "==") || strings.Contains(lines[j], "!=") {
					warnings = append(warnings, fmt.Sprintf("⚠ line %d: [T any] with == — use [T comparable] instead", i+1))
					break
				}
			}
		}
	}
	return warnings
}

// checkModulePathChange detects if the model changed the Go module path.
// Returns a warning if go.mod was modified with a different module path.
func checkModulePathChange(code, modulePath string) string {
	if modulePath == "" {
		return ""
	}
	// Look for "module " declarations in code blocks that target go.mod.
	if !strings.Contains(code, "go.mod") {
		return ""
	}
	// Check if there's a module declaration with a different path.
	for _, line := range strings.Split(code, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "module ") {
			declaredModule := strings.TrimSpace(strings.TrimPrefix(trimmed, "module "))
			if declaredModule != modulePath && declaredModule != "" {
				return fmt.Sprintf("⚠ module path changed from %s to %s — reverting", modulePath, declaredModule)
			}
		}
	}
	return ""
}
