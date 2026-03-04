package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

const tavilyEndpoint = "https://api.tavily.com/search"

// SearchResult represents a single search result.
type SearchResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"content"`
}

// tavilyResponse is the API response shape.
type tavilyResponse struct {
	Results []SearchResult `json:"results"`
}

// Search performs a web search using Tavily API.
// Returns up to 5 results. Requires MANTIS_TAVILY_KEY env var.
func Search(ctx context.Context, query string) ([]SearchResult, error) {
	apiKey := os.Getenv("MANTIS_TAVILY_KEY")
	if apiKey == "" {
		return nil, fmt.Errorf("MANTIS_TAVILY_KEY not set — get a free key at https://tavily.com")
	}

	payload := fmt.Sprintf(`{"api_key":"%s","query":"%s","max_results":5,"search_depth":"basic"}`,
		apiKey, escapeJSON(query))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tavilyEndpoint, strings.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("search request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("tavily returned %d: %s", resp.StatusCode, string(body))
	}

	var result tavilyResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}

	return result.Results, nil
}

// HasSearchKey returns true if MANTIS_TAVILY_KEY is set.
func HasSearchKey() bool {
	return os.Getenv("MANTIS_TAVILY_KEY") != ""
}

// escapeJSON escapes a string for safe inclusion in a JSON string literal.
func escapeJSON(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	s = strings.ReplaceAll(s, "\t", `\t`)
	return s
}
