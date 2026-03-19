package pipeline

import (
	"strings"
	"testing"
)

func TestCheckSuspiciousDeps_DetectsUnrequested(t *testing.T) {
	code := `import "github.com/dgrijalva/jwt-go"
import "gorm.io/gorm"
import "go.uber.org/zap"`
	warnings := checkSuspiciousDeps(code, "build a REST API")
	if len(warnings) < 3 {
		t.Errorf("expected at least 3 warnings, got %d: %v", len(warnings), warnings)
	}
}

func TestCheckSuspiciousDeps_AllowsRequested(t *testing.T) {
	code := `import "github.com/dgrijalva/jwt-go"`
	warnings := checkSuspiciousDeps(code, "build an API with JWT authentication")
	if len(warnings) != 0 {
		t.Errorf("expected 0 warnings (user requested jwt), got: %v", warnings)
	}
}

func TestCheckSuspiciousDeps_NoFalsePositives(t *testing.T) {
	code := `import "net/http"
import "fmt"
import "encoding/json"`
	warnings := checkSuspiciousDeps(code, "build a REST API")
	if len(warnings) != 0 {
		t.Errorf("expected 0 warnings for stdlib, got: %v", warnings)
	}
}

func TestCheckGoGenerics_DetectsAnyWithEquals(t *testing.T) {
	code := `func Contains[T any](slice []T, item T) bool {
	for _, v := range slice {
		if v == item {
			return true
		}
	}
	return false
}`
	warnings := checkGoGenerics(code)
	if len(warnings) == 0 {
		t.Error("expected warning for [T any] with ==")
	}
}

func TestCheckGoGenerics_ComparableOK(t *testing.T) {
	code := `func Contains[T comparable](slice []T, item T) bool {
	for _, v := range slice {
		if v == item {
			return true
		}
	}
	return false
}`
	warnings := checkGoGenerics(code)
	if len(warnings) != 0 {
		t.Errorf("expected no warnings for [T comparable], got: %v", warnings)
	}
}

func TestCheckModulePathChange_Detects(t *testing.T) {
	code := "```go:go.mod\nmodule github.com/wrong/path\n```"
	warning := checkModulePathChange(code, "github.com/seedhire/mantis")
	if warning == "" {
		t.Error("expected warning for module path change")
	}
	if !strings.Contains(warning, "changed") {
		t.Errorf("unexpected warning: %s", warning)
	}
}

func TestCheckModulePathChange_NoChange(t *testing.T) {
	code := "```go:go.mod\nmodule github.com/seedhire/mantis\n```"
	warning := checkModulePathChange(code, "github.com/seedhire/mantis")
	if warning != "" {
		t.Errorf("expected no warning, got: %s", warning)
	}
}

func TestCheckModulePathChange_NoGoMod(t *testing.T) {
	code := "```go:main.go\npackage main\n```"
	warning := checkModulePathChange(code, "github.com/seedhire/mantis")
	if warning != "" {
		t.Errorf("expected no warning when no go.mod, got: %s", warning)
	}
}
