package pipeline

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ClarifyQuestion represents a single clarifying question the model wants to ask.
type ClarifyQuestion struct {
	ID       int      `json:"id"`
	Question string   `json:"question"`
	Options  []string `json:"options"`
	Default  int      `json:"default"`
}

// ClarifyResult holds the user's answers to clarifying questions.
type ClarifyResult struct {
	Questions []ClarifyQuestion
	Answers   []int // index into Options for each question
}

// parseClarifyBlock extracts a ```clarify fenced JSON block from model output.
// Returns false if no block found, JSON is malformed, or a real plan (### Overview) is present.
func parseClarifyBlock(text string) ([]ClarifyQuestion, bool) {
	// If the model produced a real plan, don't treat clarify blocks as questions.
	if strings.Contains(text, "### Overview") {
		return nil, false
	}

	const startMarker = "```clarify"
	idx := strings.Index(text, startMarker)
	if idx < 0 {
		return nil, false
	}

	body := text[idx+len(startMarker):]
	endIdx := strings.Index(body, "```")
	if endIdx < 0 {
		return nil, false
	}
	jsonStr := strings.TrimSpace(body[:endIdx])

	var questions []ClarifyQuestion
	if err := json.Unmarshal([]byte(jsonStr), &questions); err != nil {
		return nil, false
	}

	// Validate each question.
	var valid []ClarifyQuestion
	for _, q := range questions {
		if q.Question == "" || len(q.Options) < 2 || len(q.Options) > 5 {
			continue
		}
		if q.Default < 0 || q.Default >= len(q.Options) {
			q.Default = 0
		}
		valid = append(valid, q)
	}

	if len(valid) == 0 {
		return nil, false
	}

	// Cap at 5 questions.
	if len(valid) > 5 {
		valid = valid[:5]
	}

	return valid, true
}

// formatAnswers produces a human-readable summary of the user's choices
// suitable for injecting back into the conversation.
func formatAnswers(result *ClarifyResult) string {
	if result == nil || len(result.Questions) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("Here are my choices:\n")
	for i, q := range result.Questions {
		ans := result.Answers[i]
		if ans < 0 || ans >= len(q.Options) {
			ans = q.Default
		}
		sb.WriteString(fmt.Sprintf("%d. %s → %s\n", q.ID, q.Question, q.Options[ans]))
	}
	sb.WriteString("\nPlease proceed with the plan using these choices.")
	return sb.String()
}
