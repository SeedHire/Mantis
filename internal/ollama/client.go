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

// Tool describes a function the model may call.
type Tool struct {
	Type     string       `json:"type"` // always "function"
	Function ToolFunction `json:"function"`
}

// ToolFunction holds the name, description and JSON-Schema parameters for a tool.
type ToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// ToolCall is a model-generated request to invoke a tool.
type ToolCall struct {
	Function struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"function"`
}

// ToolMessage carries the result of a tool invocation back to the model.
type ToolMessage struct {
	Role    string `json:"role"` // "tool"
	Content string `json:"content"`
}

// ChatRequest is the payload sent to /api/chat.
type ChatRequest struct {
	Model    string        `json:"model"`
	Messages []interface{} `json:"messages"` // Message, ImageMessage or ToolMessage
	Stream   bool          `json:"stream"`
	Tools    []Tool        `json:"tools,omitempty"`
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
		Role      string     `json:"role"`
		Content   string     `json:"content"`
		ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	} `json:"message"`
	Done            bool       `json:"done"`
	PromptEvalCount int        `json:"prompt_eval_count"`
	EvalCount       int        `json:"eval_count"`
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

// QuickChat sends a simple system+user message and returns the text response.
// Satisfies the brain.Querier interface.
func (c *Client) QuickChat(ctx context.Context, model, systemPrompt, userMsg string) (string, error) {
	msgs := []interface{}{
		Message{Role: "system", Content: systemPrompt},
		Message{Role: "user", Content: userMsg},
	}
	text, _, _, err := c.Chat(ctx, model, msgs, nil)
	return text, err
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

// ToolResult holds the outcome of a single non-streaming chat call with tools.
type ToolResult struct {
	Content   string
	ToolCalls []ToolCall
	PromptTok int
	ComplTok  int
}

// ChatWithTools sends a non-streaming request with tools and returns all tool
// calls (if any) and the full assistant message.
func (c *Client) ChatWithTools(
	ctx context.Context,
	model string,
	messages []interface{},
	tools []Tool,
	opts *ModelOptions,
) (*ToolResult, error) {
	req := ChatRequest{
		Model:    model,
		Messages: messages,
		Stream:   false,
		Tools:    tools,
		Options:  opts,
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("ollama %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}

	var chunk ChatChunk
	if err := json.NewDecoder(resp.Body).Decode(&chunk); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return &ToolResult{
		Content:   chunk.Message.Content,
		ToolCalls: chunk.Message.ToolCalls,
		PromptTok: chunk.PromptEvalCount,
		ComplTok:  chunk.EvalCount,
	}, nil
}

// EmbedRequest is the payload for /api/embed.
type EmbedRequest struct {
	Model string `json:"model"`
	Input string `json:"input"`
}

// EmbedResponse holds the embedding vector returned by Ollama.
type EmbedResponse struct {
	Embeddings [][]float64 `json:"embeddings"`
}

// Embed generates an embedding vector for the given text using the specified model.
func (c *Client) Embed(ctx context.Context, model, text string) ([]float64, error) {
	body, _ := json.Marshal(EmbedRequest{Model: model, Input: text})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/embed", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("embed: %s — %s", resp.Status, string(b))
	}

	var result EmbedResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	if len(result.Embeddings) == 0 || len(result.Embeddings[0]) == 0 {
		return nil, fmt.Errorf("embed: empty embedding returned")
	}
	return result.Embeddings[0], nil
}
