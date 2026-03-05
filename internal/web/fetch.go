// Package web provides web fetching (via Jina Reader) and search (via Tavily) capabilities.
package web

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Fetcher handles web page fetching with Jina Reader, raw HTTP fallback, and caching.
type Fetcher struct {
	client *http.Client
	mu     sync.Mutex
	cache  map[string]*cacheEntry
}

type cacheEntry struct {
	content string
	fetched time.Time
}

const (
	cacheTTL       = 5 * time.Minute
	cacheMaxSize   = 20
	maxBodySize    = 512 * 1024 // 512KB
	maxTextLen     = 16000      // ~4K tokens
	jinaBase       = "https://r.jina.ai/"
	jinaSearchBase = "https://s.jina.ai/"
)

// NewFetcher creates a new Fetcher with a shared HTTP client.
func NewFetcher() *Fetcher {
	return &Fetcher{
		client: &http.Client{
			Timeout: 20 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
			},
		},
		cache: make(map[string]*cacheEntry),
	}
}

// Fetch retrieves a URL's content as clean markdown via Jina Reader,
// falling back to raw HTTP + HTML stripping if Jina is unavailable.
// Results are cached for 5 minutes.
func (f *Fetcher) Fetch(ctx context.Context, rawURL string) (string, error) {
	if !strings.HasPrefix(rawURL, "http://") && !strings.HasPrefix(rawURL, "https://") {
		rawURL = "https://" + rawURL
	}

	// Check cache.
	if content, ok := f.cacheGet(rawURL); ok {
		return content, nil
	}

	// Try Jina Reader first.
	content, err := f.fetchJina(ctx, rawURL)
	if err != nil {
		// Fallback to raw HTTP.
		content, err = f.fetchRaw(ctx, rawURL)
		if err != nil {
			return "", err
		}
	}

	// Truncate to ~4K tokens.
	if len(content) > maxTextLen {
		content = content[:maxTextLen] + "\n[truncated]"
	}

	f.cachePut(rawURL, content)
	return content, nil
}

// fetchJina fetches via Jina Reader which returns clean markdown.
func (f *Fetcher) fetchJina(ctx context.Context, url string) (string, error) {
	jinaURL := jinaBase + url

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, jinaURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "text/plain")
	req.Header.Set("User-Agent", "Mantis/1.0")

	resp, err := f.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("jina fetch: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("jina returned %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodySize))
	if err != nil {
		return "", fmt.Errorf("jina read: %w", err)
	}

	text := strings.TrimSpace(string(body))
	if text == "" {
		return "", fmt.Errorf("jina returned empty content")
	}
	return text, nil
}

// fetchRaw does a direct HTTP GET and strips HTML tags.
func (f *Fetcher) fetchRaw(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mantis/1.0")
	req.Header.Set("Accept", "text/html,text/plain,application/json")

	resp, err := f.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("raw fetch: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodySize))
	if err != nil {
		return "", fmt.Errorf("raw read: %w", err)
	}

	text := stripHTML(string(body))
	if text == "" {
		return "", fmt.Errorf("empty content after HTML stripping")
	}
	return text, nil
}

func (f *Fetcher) cacheGet(url string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	entry, ok := f.cache[url]
	if !ok || time.Since(entry.fetched) > cacheTTL {
		if ok {
			delete(f.cache, url)
		}
		return "", false
	}
	return entry.content, true
}

func (f *Fetcher) cachePut(url, content string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	// Evict oldest if at capacity.
	if len(f.cache) >= cacheMaxSize {
		var oldest string
		var oldestTime time.Time
		for k, v := range f.cache {
			if oldest == "" || v.fetched.Before(oldestTime) {
				oldest = k
				oldestTime = v.fetched
			}
		}
		delete(f.cache, oldest)
	}

	f.cache[url] = &cacheEntry{content: content, fetched: time.Now()}
}

// htmlTagRe matches HTML tags.
var htmlTagRe = regexp.MustCompile(`<[^>]*>`)

// spaceRe matches multiple whitespace chars.
var spaceRe = regexp.MustCompile(`\s{2,}`)

// stripHTML removes HTML tags and collapses whitespace.
func stripHTML(s string) string {
	text := htmlTagRe.ReplaceAllString(s, " ")
	text = spaceRe.ReplaceAllString(text, " ")
	r := strings.NewReplacer(
		"&amp;", "&", "&lt;", "<", "&gt;", ">",
		"&quot;", "\"", "&#39;", "'", "&nbsp;", " ",
	)
	return strings.TrimSpace(r.Replace(text))
}

// Search performs a web search using Jina's search endpoint and returns clean markdown results.
// Results are cached for 5 minutes. No API key required for basic use.
func (f *Fetcher) Search(ctx context.Context, query string) (string, error) {
	cacheKey := "search:" + query
	if content, ok := f.cacheGet(cacheKey); ok {
		return content, nil
	}

	searchURL := jinaSearchBase + url.QueryEscape(query)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, searchURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "text/plain")
	req.Header.Set("User-Agent", "Mantis/1.0")

	resp, err := f.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("search: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("search returned %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodySize))
	if err != nil {
		return "", fmt.Errorf("search read: %w", err)
	}

	text := strings.TrimSpace(string(body))
	if text == "" {
		return "", fmt.Errorf("search returned empty content")
	}
	if len(text) > maxTextLen {
		text = text[:maxTextLen] + "\n[truncated]"
	}

	f.cachePut(cacheKey, text)
	return text, nil
}

// ExtractURLs finds URLs in text (for auto-fetch from error messages).
var urlRe = regexp.MustCompile(`https?://[^\s"'<>\)]+`)

// ExtractURLs returns all URLs found in the given text.
func ExtractURLs(text string) []string {
	return urlRe.FindAllString(text, -1)
}

// ExtractGoPackage finds Go module paths like github.com/foo/bar in text.
var goPackageRe = regexp.MustCompile(`(github\.com/[\w\-]+/[\w\-]+(?:/[\w\-]+)*)`)

// ExtractGoPackages returns Go package paths found in text.
func ExtractGoPackages(text string) []string {
	matches := goPackageRe.FindAllString(text, -1)
	seen := map[string]bool{}
	var unique []string
	for _, m := range matches {
		if !seen[m] {
			seen[m] = true
			unique = append(unique, m)
		}
	}
	return unique
}
