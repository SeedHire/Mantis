package router

import "testing"

func TestTaskTemplate(t *testing.T) {
	tests := []struct {
		taskType string
		wantEmpty bool
	}{
		{"explain", false},
		{"fix", false},
		{"refactor", false},
		{"implement", false},
		{"test", false},
		{"impact-query", false},
		{"unknown", true},
		{"", true},
	}
	for _, tt := range tests {
		got := TaskTemplate(tt.taskType)
		if tt.wantEmpty && got != "" {
			t.Errorf("TaskTemplate(%q) should be empty, got %q", tt.taskType, got)
		}
		if !tt.wantEmpty && got == "" {
			t.Errorf("TaskTemplate(%q) should not be empty", tt.taskType)
		}
	}
}

func TestTaskTemplateContainsMeaningfulContent(t *testing.T) {
	tmpl := TaskTemplate("fix")
	if len(tmpl) < 50 {
		t.Errorf("fix template too short (%d chars), expected meaningful content", len(tmpl))
	}
}
