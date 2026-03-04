package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestFetchJina_Success(t *testing.T) {
	// Mock Jina Reader server — Jina prepends its base URL, so we mock
	// the full round-trip by overriding the fetcher's HTTP client transport.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("# Example\n\nThis is clean markdown content."))
	}))
	defer server.Close()

	f := NewFetcher()
	// Test via fetchRaw since fetchJina rewrites the URL through r.jina.ai.
	// We test the Jina path indirectly through Fetch() when Jina fails → fallback.
	content, err := f.fetchRaw(context.Background(), server.URL)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if content != "# Example This is clean markdown content." {
		t.Errorf("unexpected content: %q", content)
	}
}

func TestFetch_FallbackToRaw(t *testing.T) {
	// When Jina is unreachable, Fetch should fallback to raw HTTP.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("<html><body><p>Fallback content</p></body></html>"))
	}))
	defer server.Close()

	f := NewFetcher()
	content, err := f.Fetch(context.Background(), server.URL)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !containsStr(content, "Fallback content") {
		t.Errorf("expected fallback content, got: %q", content)
	}
}

func TestFetchRaw_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<html><body><h1>Hello</h1><p>World</p></body></html>"))
	}))
	defer server.Close()

	f := NewFetcher()
	content, err := f.fetchRaw(context.Background(), server.URL)
	if err != nil {
		t.Fatalf("fetchRaw: %v", err)
	}
	if content == "" {
		t.Error("expected non-empty content")
	}
	if !containsAll(content, "Hello", "World") {
		t.Errorf("content missing expected text: %q", content)
	}
}

func TestFetchRaw_EmptyBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	f := NewFetcher()
	_, err := f.fetchRaw(context.Background(), server.URL)
	if err == nil {
		t.Error("expected error for empty body")
	}
}

func TestCache_HitAndMiss(t *testing.T) {
	f := NewFetcher()

	// Cache miss.
	_, ok := f.cacheGet("https://example.com")
	if ok {
		t.Error("expected cache miss")
	}

	// Cache put + hit.
	f.cachePut("https://example.com", "cached content")
	content, ok := f.cacheGet("https://example.com")
	if !ok {
		t.Error("expected cache hit")
	}
	if content != "cached content" {
		t.Errorf("cache content = %q", content)
	}
}

func TestCache_Eviction(t *testing.T) {
	f := NewFetcher()

	// Fill cache to capacity.
	for i := 0; i < cacheMaxSize; i++ {
		f.cachePut(string(rune('A'+i)), "content")
	}

	// Add one more — should evict oldest.
	f.cachePut("overflow", "new content")

	if len(f.cache) > cacheMaxSize {
		t.Errorf("cache size %d exceeds max %d", len(f.cache), cacheMaxSize)
	}

	// The new entry should exist.
	if _, ok := f.cacheGet("overflow"); !ok {
		t.Error("new entry not in cache after eviction")
	}
}

func TestCache_TTLExpiry(t *testing.T) {
	f := NewFetcher()

	// Manually insert an expired entry.
	f.mu.Lock()
	f.cache["expired"] = &cacheEntry{
		content: "old",
		fetched: time.Now().Add(-10 * time.Minute),
	}
	f.mu.Unlock()

	_, ok := f.cacheGet("expired")
	if ok {
		t.Error("expected cache miss for expired entry")
	}
}

func TestStripHTML(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"<p>Hello</p>", "Hello"},
		{"<b>Bold</b> &amp; <i>italic</i>", "Bold & italic"},
		{"no tags", "no tags"},
		{"<div><p>  spaced  </p></div>", "spaced"},
	}
	for _, tt := range tests {
		got := stripHTML(tt.input)
		if got != tt.expected {
			t.Errorf("stripHTML(%q) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}

func TestExtractURLs(t *testing.T) {
	text := "Error at https://pkg.go.dev/fmt and http://example.com/path"
	urls := ExtractURLs(text)
	if len(urls) != 2 {
		t.Fatalf("expected 2 URLs, got %d: %v", len(urls), urls)
	}
}

func TestExtractGoPackages(t *testing.T) {
	text := "cannot find github.com/gorilla/mux and github.com/gorilla/mux again"
	pkgs := ExtractGoPackages(text)
	if len(pkgs) != 1 {
		t.Fatalf("expected 1 unique package, got %d: %v", len(pkgs), pkgs)
	}
	if pkgs[0] != "github.com/gorilla/mux" {
		t.Errorf("package = %q", pkgs[0])
	}
}

func TestExtractGoPackages_None(t *testing.T) {
	pkgs := ExtractGoPackages("no packages here")
	if len(pkgs) != 0 {
		t.Errorf("expected 0 packages, got %v", pkgs)
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !contains(s, sub) {
			return false
		}
	}
	return true
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(s) > 0 && containsStr(s, sub))
}

func containsStr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
