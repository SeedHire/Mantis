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
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	CloudBaseURL = "https://api.ollama.com"
	LocalBaseURL = "http://localhost:11434"
	Timeout      = 0 // no client-level timeout; callers pass context deadlines
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
	NumDraft    int     `json:"num_draft,omitempty"` // speculative decoding draft tokens (Ollama ≥0.3)
}

// ChatChunk is one streamed line from the API.
type ChatChunk struct {
	Message struct {
		Role      string     `json:"role"`
		Content   string     `json:"content"`
		ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	} `json:"message"`
	Done            bool `json:"done"`
	PromptEvalCount int  `json:"prompt_eval_count"`
	EvalCount       int  `json:"eval_count"`
}

// ChatProvider defines the core chat methods needed for fallback providers.
// Both openai.Client and ollama.Client satisfy this interface.
type ChatProvider interface {
	StreamChat(ctx context.Context, model string, messages []interface{}, opts *ModelOptions, onChunk func(string)) (int, int, error)
	ChatWithTools(ctx context.Context, model string, messages []interface{}, tools []Tool, opts *ModelOptions) (*ToolResult, error)
}

// FallbackProvider holds an alternative chat provider and its default model.
type FallbackProvider struct {
	Provider ChatProvider
	Model    string // default model for this provider
	Name     string // display name (e.g. "groq", "cerebras")
}

// Client talks to Ollama Cloud or local Ollama.
type Client struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
	isCloud    bool

	// Fallback is an optional OpenAI-compatible provider used when Ollama is unavailable.
	// Set by the repl at startup if MANTIS_*_KEY env vars are detected.
	Fallback      *FallbackProvider
	useFallback   bool // true after Ollama fails and fallback succeeds
}

// newHTTPClient returns an http.Client with sensible connection timeouts.
// No overall request timeout — callers control that via context deadlines.
func newHTTPClient() *http.Client {
	return &http.Client{
		Timeout: Timeout, // 0 = no overall timeout; context handles it
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout:   10 * time.Second, // TCP connect timeout
				KeepAlive: 30 * time.Second,
			}).DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second, // time to first response byte
			MaxIdleConns:          10,
			IdleConnTimeout:       90 * time.Second,
		},
	}
}

// New returns a Client pointed at Ollama Cloud if apiKey is set,
// otherwise falls back to local Ollama at localhost:11434.
func New(apiKey string) *Client {
	if apiKey != "" {
		return &Client{
			baseURL:    CloudBaseURL,
			apiKey:     apiKey,
			httpClient: newHTTPClient(),
			isCloud:    true,
		}
	}
	return &Client{
		baseURL:    LocalBaseURL,
		httpClient: newHTTPClient(),
		isCloud:    false,
	}
}

// NewFromEnv reads OLLAMA_API_KEY from environment, falls back to local.
func NewFromEnv() *Client {
	return New(os.Getenv("OLLAMA_API_KEY"))
}

// NewWithBaseURL creates a Client targeting a custom base URL (e.g. an httptest.Server).
// Useful for testing and self-hosted Ollama deployments.
func NewWithBaseURL(baseURL string) *Client {
	return &Client{
		baseURL:    baseURL,
		httpClient: newHTTPClient(),
	}
}

// SetAPIKey swaps the API key on an existing client.
// If the new key is non-empty, the client switches to Cloud mode.
func (c *Client) SetAPIKey(key string) {
	c.apiKey = key
	if key != "" {
		c.baseURL = CloudBaseURL
		c.isCloud = true
	} else {
		c.baseURL = LocalBaseURL
		c.isCloud = false
	}
}

// APIKey returns the current API key (for debugging/display).
func (c *Client) APIKey() string { return c.apiKey }

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
		// Connection failed — try fallback if available.
		if pt, ct, fbErr, ok := c.tryFallbackStream(ctx, model, messages, opts, onChunk); ok {
			return pt, ct, fbErr
		}
		return 0, 0, fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return 0, 0, fmt.Errorf("ollama %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}

	scanner := bufio.NewScanner(resp.Body)
	// Default 64 KB buffer is too small for large model responses; raise to 10 MB.
	scanner.Buffer(make([]byte, 64*1024), 10*1024*1024)
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

// tryFallbackStream attempts to use the fallback provider for streaming.
// Returns (pt, ct, err, ok) where ok=true if fallback was attempted.
func (c *Client) tryFallbackStream(ctx context.Context, model string, messages []interface{}, opts *ModelOptions, onChunk func(string)) (int, int, error, bool) {
	if c.Fallback == nil {
		return 0, 0, nil, false
	}
	fbModel := model
	if c.Fallback.Model != "" {
		fbModel = c.Fallback.Model
	}
	pt, ct, err := c.Fallback.Provider.StreamChat(ctx, fbModel, messages, opts, onChunk)
	if err == nil {
		c.useFallback = true
	}
	return pt, ct, err, true
}

// UseFallback reports whether the client has switched to a fallback provider.
func (c *Client) UseFallback() bool { return c.useFallback }

// FallbackName returns the display name of the active fallback provider, or "".
func (c *Client) FallbackName() string {
	if c.Fallback != nil {
		return c.Fallback.Name
	}
	return ""
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

	// BUG-16: check HTTP status before decoding; 401/429 bodies are not JSON.
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("list models %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}

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
	// If no Ollama models are available but a fallback is configured,
	// inject a synthetic model entry so the router has something to resolve.
	if len(infos) == 0 && c.Fallback != nil && c.Fallback.Model != "" {
		infos = append(infos, ModelInfo{
			Name: c.Fallback.Model,
			Size: 70_000_000_000, // assume 70B-class for proper tier routing
		})
		c.useFallback = true
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
		// Connection failed — try fallback if available.
		if c.Fallback != nil {
			fbModel := model
			if c.Fallback.Model != "" {
				fbModel = c.Fallback.Model
			}
			result, fbErr := c.Fallback.Provider.ChatWithTools(ctx, fbModel, messages, tools, opts)
			if fbErr == nil {
				c.useFallback = true
			}
			return result, fbErr
		}
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
	// BUG-10: handle marshal error instead of silently ignoring it.
	body, err := json.Marshal(EmbedRequest{Model: model, Input: text})
	if err != nil {
		return nil, fmt.Errorf("marshal embed request: %w", err)
	}
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
