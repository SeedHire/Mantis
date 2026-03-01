package verify

import "testing"

func TestCheckCleanWhenNilWriter(t *testing.T) {
	result := Check("some response with Code() calls", nil)
	if !result.Clean {
		t.Error("Check with nil writer should return Clean=true")
	}
}

func TestCheckCleanNoCodeBlocks(t *testing.T) {
	result := Check("This is a text response with no code blocks", nil)
	if !result.Clean {
		t.Error("Check with no code blocks should return Clean=true")
	}
}

func TestParseConventionsEmpty(t *testing.T) {
	convs := ParseConventions("")
	if convs != nil {
		t.Errorf("expected nil for empty content, got %v", convs)
	}
}

func TestParseConventionsSections(t *testing.T) {
	content := `## Naming
- Use snake_case for DB columns
- Use camelCase for JS variables

## Architecture
- Never import from payments in auth module
`
	convs := ParseConventions(content)
	if len(convs) != 3 {
		t.Fatalf("expected 3 conventions, got %d", len(convs))
	}

	if convs[0].Section != "Naming" {
		t.Errorf("first convention section = %q, want Naming", convs[0].Section)
	}
	if convs[0].Rule != "Use snake_case for DB columns" {
		t.Errorf("first rule = %q, want 'Use snake_case for DB columns'", convs[0].Rule)
	}
	if convs[2].Section != "Architecture" {
		t.Errorf("third convention section = %q, want Architecture", convs[2].Section)
	}
}

func TestParseConventionsIgnoresHeaders(t *testing.T) {
	content := `# Title
## Section
- Rule one
(not set)
`
	convs := ParseConventions(content)
	if len(convs) != 1 {
		t.Fatalf("expected 1 convention, got %d", len(convs))
	}
}

func TestCheckConventionsCleanWhenEmpty(t *testing.T) {
	result := CheckConventions("any response", nil)
	if !result.Clean {
		t.Error("CheckConventions with nil conventions should be Clean")
	}
}

func TestCheckConventionsCleanWhenNoCode(t *testing.T) {
	convs := []Convention{{Section: "Naming", Rule: "Use snake_case"}}
	result := CheckConventions("plain text response", convs)
	if !result.Clean {
		t.Error("CheckConventions with no code blocks should be Clean")
	}
}

func TestCheckConventionsDetectsSnakeCaseViolation(t *testing.T) {
	convs := []Convention{{Section: "Naming", Rule: "Use snake_case for all variables"}}
	response := "```go\nfunc processData() {\n\tlet myVariable = 1\n}\n```"
	result := CheckConventions(response, convs)
	if result.Clean {
		t.Error("expected violation for camelCase when snake_case required")
	}
}

func TestCheckConventionsDetectsImportViolation(t *testing.T) {
	convs := []Convention{{Section: "Architecture", Rule: "Never import from payments"}}
	response := "```go\nimport \"payments/handler\"\n```"
	result := CheckConventions(response, convs)
	if result.Clean {
		t.Error("expected violation for forbidden import")
	}
}

func TestCheckConventionsCleanForCompliant(t *testing.T) {
	convs := []Convention{{Section: "Architecture", Rule: "Never import from payments"}}
	response := "```go\nimport \"auth/handler\"\n```"
	result := CheckConventions(response, convs)
	if !result.Clean {
		t.Errorf("expected clean result, got violations: %s", result.Warning)
	}
}

func TestExtractForbiddenImport(t *testing.T) {
	tests := []struct {
		rule string
		want string
	}{
		{"Never import from payments", "payments"},
		{"don't import from internal/secret", "internal/secret"},
		{"do not import lodash", "lodash"},
		{"never use moment.js", "moment.js"},
		{"normal rule", ""},
	}
	for _, tt := range tests {
		got := extractForbiddenImport(tt.rule)
		if got != tt.want {
			t.Errorf("extractForbiddenImport(%q) = %q, want %q", tt.rule, got, tt.want)
		}
	}
}

func TestSuggestCorrectionsNilWriter(t *testing.T) {
	got := SuggestCorrections([]string{"Foo"}, nil)
	if got != "" {
		t.Errorf("expected empty string for nil writer, got %q", got)
	}
}

func TestSuggestCorrectionsEmpty(t *testing.T) {
	got := SuggestCorrections(nil, nil)
	if got != "" {
		t.Errorf("expected empty string for nil symbols, got %q", got)
	}
}
