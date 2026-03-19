package repl

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/seedhire/mantis/internal/ollama"
	"github.com/seedhire/mantis/internal/web"
)

// webSearchPatterns are phrases that indicate the user is asking about external
// APIs, libraries, or documentation that would benefit from a web search.
var webSearchPatterns = []string{
	"how to use",
	"how do i use",
	"how do you use",
	"api for",
	"api of",
	"api docs",
	"api reference",
	"documentation for",
	"docs for",
	"library for",
	"package for",
	"module for",
	"sdk for",
	"what is the api",
	"how does the api",
	"latest version of",
	"npm install",
	"pip install",
	"go get",
	"cargo add",
}

// webSearchAPIPhrases detect questions about specific APIs/services.
var webSearchAPIPhrases = []string{
	"openai api",
	"stripe api",
	"twilio api",
	"github api",
	"rest api",
	"graphql api",
	"oauth",
	"webhook",
}

// shouldAutoWebSearch returns true if the user's input looks like a question
// about an external API, library, or technology that would benefit from web search.
func shouldAutoWebSearch(input string) bool {
	lower := strings.ToLower(input)

	// Must be a question or exploration (not a command to write code).
	hasQuestionIndicator := strings.Contains(lower, "?") ||
		strings.HasPrefix(lower, "how ") ||
		strings.HasPrefix(lower, "what ") ||
		strings.HasPrefix(lower, "where ") ||
		strings.HasPrefix(lower, "which ") ||
		strings.HasPrefix(lower, "explain ")

	if !hasQuestionIndicator {
		return false
	}

	for _, p := range webSearchPatterns {
		if strings.Contains(lower, p) {
			return true
		}
	}
	for _, p := range webSearchAPIPhrases {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
}

// autoWebSearch performs a web search and injects results into the message context.
// Returns the search results text (for display) or empty string if search was skipped/failed.
func autoWebSearch(input string, messages *[]interface{}, fetcher *web.Fetcher) string {
	if fetcher == nil || !shouldAutoWebSearch(input) {
		return ""
	}

	// Build a concise search query from the user's input.
	query := input
	if len(query) > 120 {
		query = query[:120]
	}

	fmt.Printf("%s  ● auto-searching: \"%s\"…%s\n", colorDim, truncateForDisplay(query, 60), colorReset)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Prefer Tavily (structured results) if key is available, else Jina (keyless).
	if web.HasSearchKey() {
		results, err := web.Search(ctx, query)
		if err != nil || len(results) == 0 {
			return ""
		}
		var sb strings.Builder
		for i, r := range results {
			sb.WriteString(fmt.Sprintf("%d. [%s](%s)\n   %s\n\n", i+1, r.Title, r.URL, r.Snippet))
		}
		text := sb.String()
		*messages = append(*messages, ollama.Message{
			Role:    "user",
			Content: "<search_results query=\"" + query + "\">\n" + text + "</search_results>",
		})
		return text
	}

	// Keyless Jina search.
	content, err := fetcher.Search(ctx, query)
	if err != nil || content == "" {
		return ""
	}
	*messages = append(*messages, ollama.Message{
		Role:    "user",
		Content: "<search_results query=\"" + query + "\">\n" + content + "\n</search_results>",
	})
	return content
}

// truncateForDisplay shortens a string for terminal display.
func truncateForDisplay(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-3] + "..."
}
