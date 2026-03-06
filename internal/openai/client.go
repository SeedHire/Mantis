// Package openai provides a streaming client for OpenAI-compatible inference APIs.
// Supported providers (all free tier):
//
//	Cerebras   — llama-3.3-70b          (~2000 tok/s, best for fast tiers)
//	SambaNova  — DeepSeek-R1-Distill-Llama-70B (chain-of-thought, best for reason tiers)
//	Groq       — llama-3.3-70b-versatile (free RPM tier, fallback for fast)
//	Gemini     — gemini-2.0-flash        (multimodal vision, 1M context free)
package openai

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

	"github.com/seedhire/mantis/internal/ollama"
)

// Provider base URLs.
const (
	CerebrasBaseURL  = "https://api.cerebras.ai/v1"
	SambaNovaBaseURL = "https://api.sambanova.ai/v1"
	GroqBaseURL      = "https://api.groq.com/openai/v1"
	GeminiBaseURL    = "https://generativelanguage.googleapis.com/v1beta/openai"
)

// Model names for each provider.
const (
	CerebrasModel  = "llama-3.3-70b"
	SambaNovaModel = "DeepSeek-R1-Distill-Llama-70B"
	GroqModel      = "llama-3.3-70b-versatile"
	GeminiModel    = "gemini-2.0-flash"
)

// Client talks to any OpenAI-compatible API endpoint.
type Client struct {
	baseURL      string
	apiKey       string
	providerName string
	httpClient   *http.Client
}

// New creates a Client for any OpenAI-compatible endpoint.
func New(apiKey, baseURL, providerName string) *Client {
	return &Client{
		baseURL:      baseURL,
		apiKey:       apiKey,
		providerName: providerName,
		httpClient:   &http.Client{},
	}
}

// NewCerebras returns a Cerebras client.
func NewCerebras(apiKey string) *Client { return New(apiKey, CerebrasBaseURL, "cerebras") }

// NewSambaNova returns a SambaNova client.
func NewSambaNova(apiKey string) *Client { return New(apiKey, SambaNovaBaseURL, "sambanova") }

// NewGroq returns a Groq client.
func NewGroq(apiKey string) *Client { return New(apiKey, GroqBaseURL, "groq") }

// NewGemini returns a Google Gemini (OpenAI-compat endpoint) client.
func NewGemini(apiKey string) *Client { return New(apiKey, GeminiBaseURL, "gemini") }

// NewCerebrasFromEnv returns a Cerebras client if MANTIS_CEREBRAS_KEY is set, else nil.
func NewCerebrasFromEnv() *Client {
	key := os.Getenv("MANTIS_CEREBRAS_KEY")
	if key == "" {
		return nil
	}
	return NewCerebras(key)
}

// NewSambaNovaFromEnv returns a SambaNova client if MANTIS_SAMBANOVA_KEY is set, else nil.
func NewSambaNovaFromEnv() *Client {
	key := os.Getenv("MANTIS_SAMBANOVA_KEY")
	if key == "" {
		return nil
	}
	return NewSambaNova(key)
}

// NewGroqFromEnv returns a Groq client if MANTIS_GROQ_KEY is set, else nil.
func NewGroqFromEnv() *Client {
	key := os.Getenv("MANTIS_GROQ_KEY")
	if key == "" {
		return nil
	}
	return NewGroq(key)
}

// NewGeminiFromEnv returns a Gemini client if MANTIS_GEMINI_KEY is set, else nil.
func NewGeminiFromEnv() *Client {
	key := os.Getenv("MANTIS_GEMINI_KEY")
	if key == "" {
		return nil
	}
	return NewGemini(key)
}

// Provider returns the provider name for display.
func (c *Client) Provider() string { return c.providerName }

// ── Wire format ───────────────────────────────────────────────────────────────

// chatContent is either a plain string or a multipart array (for vision).
type chatContent = interface{}

// textContent is a plain-text content part.
type textContent struct {
	Type string `json:"type"` // "text"
	Text string `json:"text"`
}

// imageURLContent is a base64-image content part (OpenAI multimodal format).
type imageURLContent struct {
	Type     string `json:"type"` // "image_url"
	ImageURL struct {
		URL string `json:"url"` // "data:image/jpeg;base64,..."
	} `json:"image_url"`
}

// chatMessage is a single message in the OpenAI wire format.
type chatMessage struct {
	Role    string      `json:"role"`
	Content chatContent `json:"content"`
}

// chatRequest is the /v1/chat/completions request body.
type chatRequest struct {
	Model         string        `json:"model"`
	Messages      []chatMessage `json:"messages"`
	Stream        bool          `json:"stream"`
	Tools         []interface{} `json:"tools,omitempty"`
	StreamOptions *streamOpts   `json:"stream_options,omitempty"`
	Temperature   *float64      `json:"temperature,omitempty"`
}

type streamOpts struct {
	IncludeUsage bool `json:"include_usage"`
}

// streamChunk is one SSE data line from the API.
type streamChunk struct {
	Choices []struct {
		Delta struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				Function struct {
					Name      string          `json:"name"`
					Arguments json.RawMessage `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

// convertMessages maps Ollama message types to OpenAI wire format.
// ImageMessages are converted to multipart content arrays for vision providers.
func convertMessages(messages []interface{}) []chatMessage {
	out := make([]chatMessage, 0, len(messages))
	for _, m := range messages {
		switch v := m.(type) {
		case ollama.Message:
			out = append(out, chatMessage{Role: v.Role, Content: v.Content})
		case ollama.ImageMessage:
			if len(v.Images) == 0 {
				out = append(out, chatMessage{Role: v.Role, Content: v.Content})
				continue
			}
			parts := []interface{}{textContent{Type: "text", Text: v.Content}}
			for _, b64 := range v.Images {
				var ic imageURLContent
				ic.Type = "image_url"
				// Detect JPEG vs PNG by magic bytes (simplified: always use jpeg).
				ic.ImageURL.URL = "data:image/jpeg;base64," + b64
				parts = append(parts, ic)
			}
			out = append(out, chatMessage{Role: v.Role, Content: parts})
		case ollama.ToolMessage:
			out = append(out, chatMessage{Role: "tool", Content: v.Content})
		}
	}
	return out
}

// ── StreamChat ────────────────────────────────────────────────────────────────

// StreamChat sends messages and streams the response token by token via SSE.
// Signature matches ollama.Client.StreamChat exactly for interface compatibility.
func (c *Client) StreamChat(
	ctx context.Context,
	model string,
	messages []interface{},
	opts *ollama.ModelOptions,
	onChunk func(string),
) (promptTokens int, completionTokens int, err error) {
	incUsage := true
	req := chatRequest{
		Model:         model,
		Messages:      convertMessages(messages),
		Stream:        true,
		StreamOptions: &streamOpts{IncludeUsage: incUsage},
	}
	if opts != nil && opts.Temperature != 0 {
		t := opts.Temperature
		req.Temperature = &t
	}

	body, err := json.Marshal(req)
	if err != nil {
		return 0, 0, fmt.Errorf("marshal: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return 0, 0, fmt.Errorf("request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return 0, 0, fmt.Errorf("%s: %w", c.providerName, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return 0, 0, fmt.Errorf("%s %s: %s", c.providerName, resp.Status, strings.TrimSpace(string(b)))
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 10*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			break
		}
		var chunk streamChunk
		if json.Unmarshal([]byte(payload), &chunk) != nil {
			continue
		}
		for _, choice := range chunk.Choices {
			if choice.Delta.Content != "" {
				onChunk(choice.Delta.Content)
			}
		}
		if chunk.Usage != nil {
			promptTokens = chunk.Usage.PromptTokens
			completionTokens = chunk.Usage.CompletionTokens
		}
	}
	if scanErr := scanner.Err(); scanErr != nil {
		return promptTokens, completionTokens, fmt.Errorf("stream read: %w", scanErr)
	}
	return promptTokens, completionTokens, nil
}

// Chat is a non-streaming call that returns the full response.
func (c *Client) Chat(ctx context.Context, model string, messages []interface{}, opts *ollama.ModelOptions) (string, int, int, error) {
	var sb strings.Builder
	pt, ct, err := c.StreamChat(ctx, model, messages, opts, func(chunk string) { sb.WriteString(chunk) })
	return sb.String(), pt, ct, err
}

// QuickChat sends a simple system+user message and returns the text response.
// Satisfies the brain.Querier interface used by some internal packages.
func (c *Client) QuickChat(ctx context.Context, model, systemPrompt, userMsg string) (string, error) {
	msgs := []interface{}{
		ollama.Message{Role: "system", Content: systemPrompt},
		ollama.Message{Role: "user", Content: userMsg},
	}
	text, _, _, err := c.Chat(ctx, model, msgs, nil)
	return text, err
}

// ChatWithTools sends a non-streaming request with tool definitions.
// Tool calling support is provider-dependent; uses OpenAI tool format.
func (c *Client) ChatWithTools(
	ctx context.Context,
	model string,
	messages []interface{},
	tools []ollama.Tool,
	opts *ollama.ModelOptions,
) (*ollama.ToolResult, error) {
	// Convert ollama.Tool to OpenAI tool format (identical schema, same JSON).
	openAITools := make([]interface{}, len(tools))
	for i, t := range tools {
		openAITools[i] = map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        t.Function.Name,
				"description": t.Function.Description,
				"parameters":  t.Function.Parameters,
			},
		}
	}

	req := chatRequest{
		Model:    model,
		Messages: convertMessages(messages),
		Stream:   false,
		Tools:    openAITools,
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", c.providerName, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("%s %s: %s", c.providerName, resp.Status, strings.TrimSpace(string(b)))
	}

	var result struct {
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					Function struct {
						Name      string          `json:"name"`
						Arguments json.RawMessage `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	if len(result.Choices) == 0 {
		return nil, fmt.Errorf("%s: empty response", c.providerName)
	}

	msg := result.Choices[0].Message
	toolCalls := make([]ollama.ToolCall, len(msg.ToolCalls))
	for i, tc := range msg.ToolCalls {
		toolCalls[i] = ollama.ToolCall{
			Function: struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}{
				Name:      tc.Function.Name,
				Arguments: tc.Function.Arguments,
			},
		}
	}
	return &ollama.ToolResult{
		Content:   msg.Content,
		ToolCalls: toolCalls,
		PromptTok: result.Usage.PromptTokens,
		ComplTok:  result.Usage.CompletionTokens,
	}, nil
}
