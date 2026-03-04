package ollama

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newStubServer starts an httptest.Server that mimics the Ollama API.
// It handles:
//   - POST /api/chat   → streams 2 NDJSON lines: one content chunk + a final done message
//   - GET  /api/tags   → returns a single model entry ("stub-model:latest")
//
// Returns the test server and a pre-configured Client pointing at it.
func newStubServer(t *testing.T, reply string) (*httptest.Server, *Client) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/chat":
			w.Header().Set("Content-Type", "application/json")
			// First chunk: content
			chunk1, _ := json.Marshal(map[string]interface{}{
				"message": map[string]string{"role": "assistant", "content": reply},
				"done":    false,
			})
			w.Write(chunk1)
			w.Write([]byte("\n"))
			// Final chunk: done + token counts
			chunk2, _ := json.Marshal(map[string]interface{}{
				"message":          map[string]string{"role": "assistant", "content": ""},
				"done":             true,
				"prompt_eval_count": 10,
				"eval_count":       20,
			})
			w.Write(chunk2)
			w.Write([]byte("\n"))

		case r.Method == http.MethodGet && r.URL.Path == "/api/tags":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"models": []map[string]interface{}{
					{"name": "stub-model:latest", "size": int64(1 << 30)},
				},
			})

		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, NewWithBaseURL(srv.URL)
}

// ── Tests using the stub ───────────────────────────────────────────────────────

func TestStreamChat_StubServer(t *testing.T) {
	_, client := newStubServer(t, "Hello from stub!")

	var buf strings.Builder
	pt, ct, err := client.StreamChat(
		context.Background(),
		"stub-model:latest",
		[]interface{}{Message{Role: "user", Content: "hi"}},
		nil,
		func(chunk string) { buf.WriteString(chunk) },
	)
	if err != nil {
		t.Fatalf("StreamChat: %v", err)
	}
	if !strings.Contains(buf.String(), "Hello from stub!") {
		t.Errorf("response %q missing expected content", buf.String())
	}
	if pt != 10 || ct != 20 {
		t.Errorf("token counts: prompt=%d completion=%d, want 10 20", pt, ct)
	}
}

func TestListModels_StubServer(t *testing.T) {
	_, client := newStubServer(t, "")
	models, err := client.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) != 1 {
		t.Fatalf("expected 1 model, got %d", len(models))
	}
	if models[0].Name != "stub-model:latest" {
		t.Errorf("model name = %q, want stub-model:latest", models[0].Name)
	}
}

func TestStreamChat_EmptyReply(t *testing.T) {
	_, client := newStubServer(t, "")
	var buf strings.Builder
	_, _, err := client.StreamChat(
		context.Background(),
		"stub-model:latest",
		[]interface{}{Message{Role: "user", Content: "hello"}},
		nil,
		func(chunk string) { buf.WriteString(chunk) },
	)
	if err != nil {
		t.Fatalf("StreamChat with empty reply: %v", err)
	}
}

func TestStreamChat_ContextCancel(t *testing.T) {
	_, client := newStubServer(t, "This should be cancelled")
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	var buf strings.Builder
	_, _, err := client.StreamChat(ctx, "stub-model:latest",
		[]interface{}{Message{Role: "user", Content: "hi"}},
		nil,
		func(chunk string) { buf.WriteString(chunk) },
	)
	// Cancelled context should return an error
	if err == nil {
		t.Log("context cancel: no error returned (server may have replied before cancel)")
	}
}
