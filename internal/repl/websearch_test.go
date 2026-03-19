package repl

import "testing"

func TestShouldAutoWebSearch_APIQuestions(t *testing.T) {
	positives := []string{
		"how to use the Stripe API?",
		"What is the api for sending emails with SendGrid?",
		"how do i use jwt in Go?",
		"where are the docs for the github api?",
		"which library for HTTP requests in Python?",
		"how does the openai api work?",
		"explain how to use oauth with Google?",
	}
	for _, q := range positives {
		if !shouldAutoWebSearch(q) {
			t.Errorf("expected true for %q", q)
		}
	}
}

func TestShouldAutoWebSearch_NonAPIQuestions(t *testing.T) {
	negatives := []string{
		"fix the login bug",
		"refactor the auth module",
		"build a REST API with auth",
		"add error handling to the parser",
		"run the tests",
		"explain this function",
	}
	for _, q := range negatives {
		if shouldAutoWebSearch(q) {
			t.Errorf("expected false for %q", q)
		}
	}
}

func TestShouldAutoWebSearch_RequiresQuestionForm(t *testing.T) {
	// "how to use X" has "how" prefix so should match.
	if !shouldAutoWebSearch("how to use redis for caching?") {
		t.Error("expected true for 'how to use redis for caching?'")
	}
	// Same content but as imperative command — should NOT match.
	if shouldAutoWebSearch("use redis for caching") {
		t.Error("expected false for imperative 'use redis for caching'")
	}
}

func TestTruncateForDisplay(t *testing.T) {
	if r := truncateForDisplay("short", 20); r != "short" {
		t.Errorf("expected 'short', got %q", r)
	}
	if r := truncateForDisplay("this is a very long string", 10); r != "this is..." {
		t.Errorf("expected 'this is...', got %q", r)
	}
}
