package pipeline

import (
	"testing"
)

func TestParseClarifyBlock_Valid(t *testing.T) {
	input := "Some preamble\n```clarify\n" +
		`[{"id":1,"question":"Which API style?","options":["REST","GraphQL","gRPC"],"default":0},` +
		`{"id":2,"question":"Which database?","options":["PostgreSQL","MongoDB"],"default":1}]` +
		"\n```\nSome trailing text"

	qs, ok := parseClarifyBlock(input)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if len(qs) != 2 {
		t.Fatalf("expected 2 questions, got %d", len(qs))
	}
	if qs[0].Question != "Which API style?" {
		t.Errorf("unexpected question: %s", qs[0].Question)
	}
	if len(qs[0].Options) != 3 {
		t.Errorf("expected 3 options, got %d", len(qs[0].Options))
	}
	if qs[1].Default != 1 {
		t.Errorf("expected default=1, got %d", qs[1].Default)
	}
}

func TestParseClarifyBlock_NoBlock(t *testing.T) {
	_, ok := parseClarifyBlock("Just a regular response with no clarify block.")
	if ok {
		t.Fatal("expected ok=false for no block")
	}
}

func TestParseClarifyBlock_MalformedJSON(t *testing.T) {
	input := "```clarify\n{not valid json}\n```"
	_, ok := parseClarifyBlock(input)
	if ok {
		t.Fatal("expected ok=false for malformed JSON")
	}
}

func TestParseClarifyBlock_PlanWins(t *testing.T) {
	input := "### Overview\nSome plan\n```clarify\n" +
		`[{"id":1,"question":"Q?","options":["A","B"],"default":0}]` +
		"\n```"
	_, ok := parseClarifyBlock(input)
	if ok {
		t.Fatal("expected ok=false when ### Overview present")
	}
}

func TestParseClarifyBlock_TruncateOver5(t *testing.T) {
	input := "```clarify\n["
	for i := 1; i <= 7; i++ {
		if i > 1 {
			input += ","
		}
		input += `{"id":` + itoa(i) + `,"question":"Q` + itoa(i) + `?","options":["A","B"],"default":0}`
	}
	input += "]\n```"

	qs, ok := parseClarifyBlock(input)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if len(qs) != 5 {
		t.Fatalf("expected 5 questions (truncated), got %d", len(qs))
	}
}

func TestParseClarifyBlock_TooFewOptions(t *testing.T) {
	input := "```clarify\n" +
		`[{"id":1,"question":"Q?","options":["Only one"],"default":0}]` +
		"\n```"
	_, ok := parseClarifyBlock(input)
	if ok {
		t.Fatal("expected ok=false for <2 options")
	}
}

func TestParseClarifyBlock_TooManyOptions(t *testing.T) {
	input := "```clarify\n" +
		`[{"id":1,"question":"Q?","options":["A","B","C","D","E","F"],"default":0}]` +
		"\n```"
	_, ok := parseClarifyBlock(input)
	if ok {
		t.Fatal("expected ok=false for >5 options")
	}
}

func TestFormatAnswers(t *testing.T) {
	result := &ClarifyResult{
		Questions: []ClarifyQuestion{
			{ID: 1, Question: "Which API style?", Options: []string{"REST", "GraphQL", "gRPC"}, Default: 0},
			{ID: 2, Question: "Which database?", Options: []string{"PostgreSQL", "MongoDB"}, Default: 0},
		},
		Answers: []int{1, 0},
	}
	out := formatAnswers(result)
	if out == "" {
		t.Fatal("expected non-empty output")
	}
	if !contains(out, "GraphQL") {
		t.Error("expected GraphQL in output")
	}
	if !contains(out, "PostgreSQL") {
		t.Error("expected PostgreSQL in output")
	}
}

func TestFormatAnswers_FewerAnswersThanQuestions(t *testing.T) {
	// BUG: formatAnswers panics if len(Answers) < len(Questions)
	// because it accesses result.Answers[i] without bounds check.
	result := &ClarifyResult{
		Questions: []ClarifyQuestion{
			{ID: 1, Question: "Q1?", Options: []string{"A", "B"}, Default: 0},
			{ID: 2, Question: "Q2?", Options: []string{"X", "Y"}, Default: 1},
			{ID: 3, Question: "Q3?", Options: []string{"M", "N"}, Default: 0},
		},
		Answers: []int{0}, // only 1 answer for 3 questions
	}
	// Should not panic — should use default for missing answers.
	out := formatAnswers(result)
	if out == "" {
		t.Error("expected non-empty output")
	}
}

func TestFormatAnswers_Nil(t *testing.T) {
	if formatAnswers(nil) != "" {
		t.Error("expected empty string for nil result")
	}
}

func itoa(i int) string {
	return string(rune('0' + i))
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(s) > 0 && containsHelper(s, sub))
}

func containsHelper(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
