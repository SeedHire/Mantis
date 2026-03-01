// Package ollama provides a streaming client for Ollama Cloud and local Ollama.
// Primary endpoint: https://api.ollama.com  (free tier, requires OLLAMA_API_KEY or ~/.mantis/config)
// Fallback:         http://localhost:11434   (local Ollama, unlimited, offline)
package ollama

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	CloudBaseURL = "https://api.ollama.com"
	LocalBaseURL = "http://localhost:11434"
	Timeout      = 120 * time.Second
)

// Message is a single chat turn.
type Message struct {
	Role    string `json:"role"` // system | user | assistant
	Content string `json:"content"`
}

// ImageMessage is a chat turn with image data (multimodal).
type ImageMessage struct {
	Role    string   `json:"role"`
	Content string   `json:"content"`
	Images  []string `json:"images"` // base64-encoded
}

// ChatRequest is the payload sent to /api/chat.
type ChatRequest struct {
	Model    string        `json:"model"`
	Messages []interface{} `json:"messages"` // Message or ImageMessage
	Stream   bool          `json:"stream"`
	Options  *ModelOptions `json:"options,omitempty"`
}

// ModelOptions allows per-request tuning.
type ModelOptions struct {
	Temperature float64 `json:"temperature,omitempty"`
	NumCtx      int     `json:"num_ctx,omitempty"`
	NumPredict  int     `json:"num_predict,omitempty"`
}

// ChatChunk is one streamed line from the API.
type ChatChunk struct {
	Message struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"message"`
	Done               bool  `json:"done"`
	PromptEvalCount    int   `json:"prompt_eval_count"`
	EvalCount          int   `json:"eval_count"`
}

// Client talks to Ollama Cloud or local Ollama.
type Client struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
	isCloud    bool
}

// New returns a Client pointed at Ollama Cloud if apiKey is set,
// otherwise falls back to local Ollama at localhost:11434.
func New(apiKey string) *Client {
	if apiKey != "" {
		return &Client{
			baseURL:    CloudBaseURL,
			apiKey:     apiKey,
			httpClient: &http.Client{Timeout: Timeout},
			isCloud:    true,
		}
	}
	return &Client{
		baseURL:    LocalBaseURL,
		httpClient: &http.Client{Timeout: Timeout},
		isCloud:    false,
	}
}

// NewFromEnv reads OLLAMA_API_KEY from environment, falls back to local.
func NewFromEnv() *Client {
	return New(os.Getenv("OLLAMA_API_KEY"))
}

// IsCloud reports whether this client is using Ollama Cloud.
func (c *Client) IsCloud() bool { return c.isCloud }

// BaseURL returns the endpoint being used.
func (c *Client) BaseURL() string { return c.baseURL }

// StreamChat sends messages and streams the response token by token.
// onChunk is called for each streamed content fragment.
// Returns total (promptTokens, completionTokens, error).
func (c *Client) StreamChat(
	ctx context.Context,
	model string,
	messages []interface{},
	opts *ModelOptions,
	onChunk func(string),
) (promptTokens int, completionTokens int, err error) {
	req := ChatRequest{
		Model:    model,
		Messages: messages,
		Stream:   true,
		Options:  opts,
	}

	body, err := json.Marshal(req)
	if err != nil {
		return 0, 0, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return 0, 0, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return 0, 0, fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return 0, 0, fmt.Errorf("ollama %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}

	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		var chunk ChatChunk
		if err := json.Unmarshal([]byte(line), &chunk); err != nil {
			continue
		}
		if chunk.Message.Content != "" {
			onChunk(chunk.Message.Content)
		}
		if chunk.Done {
			promptTokens = chunk.PromptEvalCount
			completionTokens = chunk.EvalCount
		}
	}
	if err := scanner.Err(); err != nil {
		return promptTokens, completionTokens, fmt.Errorf("stream read: %w", err)
	}
	return promptTokens, completionTokens, nil
}

// Chat is a non-streaming version that returns the full response.
func (c *Client) Chat(ctx context.Context, model string, messages []interface{}, opts *ModelOptions) (string, int, int, error) {
	var sb strings.Builder
	pt, ct, err := c.StreamChat(ctx, model, messages, opts, func(chunk string) {
		sb.WriteString(chunk)
	})
	return sb.String(), pt, ct, err
}

// ModelInfo holds a model name and its size in bytes.
type ModelInfo struct {
	Name string
	Size int64
}

// ListModels returns the models available on the current endpoint with sizes.
func (c *Client) ListModels(ctx context.Context) ([]ModelInfo, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/tags", nil)
	if err != nil {
		return nil, err
	}
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("list models: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		Models []struct {
			Name string `json:"name"`
			Size int64  `json:"size"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	infos := make([]ModelInfo, len(result.Models))
	for i, m := range result.Models {
		infos[i] = ModelInfo{Name: m.Name, Size: m.Size}
	}
	return infos, nil
}

// Ping checks connectivity to the endpoint. Returns nil if reachable.
func (c *Client) Ping(ctx context.Context) error {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL, nil)
	if err != nil {
		return err
	}
	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}
