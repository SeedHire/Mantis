package pipeline

import (
	"strings"
	"testing"
)

func TestExtractConstraints_DontUse(t *testing.T) {
	result := extractConstraints("build a REST API but don't use express, use standard net/http")
	if result == "" {
		t.Fatal("expected constraints, got empty")
	}
	if !strings.Contains(result, "DO NOT use express") {
		t.Errorf("expected 'DO NOT use express' in: %s", result)
	}
}

func TestExtractConstraints_OnlyUse(t *testing.T) {
	result := extractConstraints("create a web server, only use stdlib packages")
	if !strings.Contains(result, "ONLY use stdlib packages") {
		t.Errorf("expected 'ONLY use stdlib packages' in: %s", result)
	}
}

func TestExtractConstraints_NoFrameworks(t *testing.T) {
	result := extractConstraints("build a todo app with no frameworks")
	if !strings.Contains(result, "NO frameworks") {
		t.Errorf("expected 'NO frameworks' in: %s", result)
	}
}

func TestExtractConstraints_StdlibOnly(t *testing.T) {
	result := extractConstraints("write a CLI tool, stdlib only")
	if !strings.Contains(result, "STANDARD LIBRARY ONLY") {
		t.Errorf("expected STANDARD LIBRARY ONLY in: %s", result)
	}
}

func TestExtractConstraints_NoConstraints(t *testing.T) {
	result := extractConstraints("build a REST API with express and prisma")
	if result != "" {
		t.Errorf("expected empty, got: %s", result)
	}
}

func TestExtractConstraints_MultipleConstraints(t *testing.T) {
	result := extractConstraints("build a server, don't use gin. must use net/http")
	if !strings.Contains(result, "DO NOT use gin") {
		t.Errorf("missing gin constraint in: %s", result)
	}
	if !strings.Contains(result, "MUST use net/http") {
		t.Errorf("missing net/http constraint in: %s", result)
	}
}
