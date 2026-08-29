package botnet

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// The three concrete SearchBackends: Exa, Brave, Tavily. Each holds its own key
// and an http.Client with a request timeout (the OpenRouter client pattern), and
// hits the provider's REST search endpoint with its auth header, mapping the
// response to []SearchResult. Request/response field names come from each
// provider's current API doc, recorded in sr-server-report.md.

// searchTimeout bounds one provider call — a backstop so a slow search cannot
// hold the turn's tool loop open near the turn timeout.
const searchTimeout = 20 * time.Second

// defaultSearchResults is the count requested when the model names none.
const defaultSearchResults = 5

// snippetLimit caps a normalized snippet so a provider that returns full page
// text (Exa) cannot bloat the rendered tool result and the stored audit record.
const snippetLimit = 500

// clampResults keeps a model-supplied count in a sane range (providers cap
// around 10-20); 0 or negative means "use the default".
func clampResults(n int) int {
	if n <= 0 {
		return defaultSearchResults
	}
	if n > 10 {
		return 10
	}
	return n
}

// firstNonEmpty returns the first non-empty string, or "".
func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

// clip trims s to at most n runes, appending an ellipsis when it cut.
func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimSpace(string(r[:n])) + "…"
}

// ── Exa ───────────────────────────────────────────────────────────────────────
// POST https://api.exa.ai/search, auth header `x-api-key`. We request result
// contents (highlights + a bounded text excerpt) so a snippet comes back:
// highlights[0] is the best short excerpt, with the text field as the fallback.

// ExaBackend searches via exa.ai.
type ExaBackend struct {
	key  string
	http *http.Client
}

// NewExaBackend builds an Exa backend with a bounded-timeout client.
func NewExaBackend(key string) *ExaBackend {
	return &ExaBackend{key: key, http: &http.Client{Timeout: searchTimeout}}
}

func (b *ExaBackend) Name() string { return "exa" }

func (b *ExaBackend) Search(ctx context.Context, query string, opts SearchOpts) ([]SearchResult, error) {
	reqBody, err := json.Marshal(map[string]any{
		"query":      query,
		"numResults": clampResults(opts.NumResults),
		"type":       "auto",
		"contents": map[string]any{
			"text":       map[string]any{"maxCharacters": 1000},
			"highlights": true,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("exa: encode: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.exa.ai/search", bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("exa: request: %w", err)
	}
	req.Header.Set("x-api-key", b.key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	body, err := doSearch(b.http, req, "exa")
	if err != nil {
		return nil, err
	}
	return parseExa(body)
}

// parseExa maps an Exa /search response body to []SearchResult.
func parseExa(body []byte) ([]SearchResult, error) {
	var out struct {
		Results []struct {
			Title         string   `json:"title"`
			URL           string   `json:"url"`
			PublishedDate string   `json:"publishedDate"`
			Text          string   `json:"text"`
			Highlights    []string `json:"highlights"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("exa: decode: %w", err)
	}
	results := make([]SearchResult, 0, len(out.Results))
	for _, r := range out.Results {
		snippet := ""
		if len(r.Highlights) > 0 {
			snippet = r.Highlights[0]
		}
		snippet = firstNonEmpty(snippet, r.Text)
		results = append(results, SearchResult{
			Title:       firstNonEmpty(r.Title, resultHost(r.URL)),
			URL:         r.URL,
			Snippet:     clip(snippet, snippetLimit),
			PublishedAt: r.PublishedDate,
		})
	}
	return results, nil
}

// ── Brave ─────────────────────────────────────────────────────────────────────
// GET https://api.search.brave.com/res/v1/web/search, auth header
// `X-Subscription-Token`. text_decorations=false keeps the description free of
// <strong> highlight tags. Web results live under web.results[].

// BraveBackend searches via the Brave Search API.
type BraveBackend struct {
	key  string
	http *http.Client
}

// NewBraveBackend builds a Brave backend with a bounded-timeout client.
func NewBraveBackend(key string) *BraveBackend {
	return &BraveBackend{key: key, http: &http.Client{Timeout: searchTimeout}}
}

func (b *BraveBackend) Name() string { return "brave" }

func (b *BraveBackend) Search(ctx context.Context, query string, opts SearchOpts) ([]SearchResult, error) {
	q := url.Values{}
	q.Set("q", query)
	q.Set("count", strconv.Itoa(clampResults(opts.NumResults)))
	q.Set("text_decorations", "false")
	endpoint := "https://api.search.brave.com/res/v1/web/search?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("brave: request: %w", err)
	}
	req.Header.Set("X-Subscription-Token", b.key)
	req.Header.Set("Accept", "application/json")

	body, err := doSearch(b.http, req, "brave")
	if err != nil {
		return nil, err
	}
	return parseBrave(body)
}

// parseBrave maps a Brave web-search response body to []SearchResult.
func parseBrave(body []byte) ([]SearchResult, error) {
	var out struct {
		Web struct {
			Results []struct {
				Title       string `json:"title"`
				URL         string `json:"url"`
				Description string `json:"description"`
				Age         string `json:"age"`
				PageAge     string `json:"page_age"`
			} `json:"results"`
		} `json:"web"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("brave: decode: %w", err)
	}
	results := make([]SearchResult, 0, len(out.Web.Results))
	for _, r := range out.Web.Results {
		results = append(results, SearchResult{
			Title:       firstNonEmpty(r.Title, resultHost(r.URL)),
			URL:         r.URL,
			Snippet:     clip(r.Description, snippetLimit),
			PublishedAt: firstNonEmpty(r.PageAge, r.Age),
		})
	}
	return results, nil
}

// ── Tavily ────────────────────────────────────────────────────────────────────
// POST https://api.tavily.com/search, auth header `Authorization: Bearer`. The
// snippet is each result's content; published_date is present only for the news
// topic, so it is usually empty on a general search.

// TavilyBackend searches via tavily.com.
type TavilyBackend struct {
	key  string
	http *http.Client
}

// NewTavilyBackend builds a Tavily backend with a bounded-timeout client.
func NewTavilyBackend(key string) *TavilyBackend {
	return &TavilyBackend{key: key, http: &http.Client{Timeout: searchTimeout}}
}

func (b *TavilyBackend) Name() string { return "tavily" }

func (b *TavilyBackend) Search(ctx context.Context, query string, opts SearchOpts) ([]SearchResult, error) {
	reqBody, err := json.Marshal(map[string]any{
		"query":       query,
		"max_results": clampResults(opts.NumResults),
	})
	if err != nil {
		return nil, fmt.Errorf("tavily: encode: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.tavily.com/search", bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("tavily: request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+b.key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	body, err := doSearch(b.http, req, "tavily")
	if err != nil {
		return nil, err
	}
	return parseTavily(body)
}

// parseTavily maps a Tavily /search response body to []SearchResult.
func parseTavily(body []byte) ([]SearchResult, error) {
	var out struct {
		Results []struct {
			Title         string `json:"title"`
			URL           string `json:"url"`
			Content       string `json:"content"`
			PublishedDate string `json:"published_date"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("tavily: decode: %w", err)
	}
	results := make([]SearchResult, 0, len(out.Results))
	for _, r := range out.Results {
		results = append(results, SearchResult{
			Title:       firstNonEmpty(r.Title, resultHost(r.URL)),
			URL:         r.URL,
			Snippet:     clip(r.Content, snippetLimit),
			PublishedAt: r.PublishedDate,
		})
	}
	return results, nil
}

// ── Mock ──────────────────────────────────────────────────────────────────────
// A deterministic, keyless dev/verification backend — NOT a real provider. It
// exists so the whole feature (web_search → results → audit trail) is
// demonstrable and acceptance-testable without any paid key: run botnetd with
// SEARCH_BACKEND=mock and the model's searches resolve to these canned results.
// It is a first-class backend in the same router (same interface, no
// special-casing), added to the available set ONLY when explicitly selected, so
// it can never activate by accident. Swapping to a real provider is then just
// SEARCH_BACKEND=exa plus a key — nothing else changes.

// MockBackend returns a fixed set of varied results, echoing the query so a live
// turn visibly drives the search.
type MockBackend struct{}

// NewMockBackend builds the mock backend. It needs no key. Value receivers, so a
// bare MockBackend{} literal also satisfies SearchBackend.
func NewMockBackend() MockBackend { return MockBackend{} }

func (MockBackend) Name() string { return "mock" }

func (MockBackend) Search(_ context.Context, query string, opts SearchOpts) ([]SearchResult, error) {
	// Deterministic given the query. The third entry has no provider title, so
	// its Title is the URL host — exercising the same fallback the real backends
	// apply, which is what the UI's host-fallback path renders.
	all := []SearchResult{
		{
			Title:       "Go 1.24 is released - The Go Blog",
			URL:         "https://go.dev/blog/go1.24",
			Snippet:     fmt.Sprintf("Top result for %q: Go 1.24 adds generic type aliases, a faster runtime, and improved tooling.", query),
			PublishedAt: "2025-02-11T00:00:00Z",
		},
		{
			Title:       "SQLite Release 3.46.0 Notes",
			URL:         "https://sqlite.org/releaselog/3_46_0.html",
			Snippet:     "This release focuses on query-planner improvements and new JSON functions.",
			PublishedAt: "2024-05-23T00:00:00Z",
		},
		{
			Title:       resultHost("https://example.org/untitled-note"), // titleless source → host fallback
			URL:         "https://example.org/untitled-note",
			Snippet:     "A source that arrived without a title; the UI shows its host instead.",
			PublishedAt: "",
		},
		{
			Title:       "Rust 1.85 and the 2024 Edition",
			URL:         "https://blog.rust-lang.org/2025/02/20/Rust-1.85.0.html",
			Snippet:     "The 2024 edition stabilizes async closures and refines the borrow checker.",
			PublishedAt: "2025-02-20T00:00:00Z",
		},
	}
	n := clampResults(opts.NumResults)
	if n > len(all) {
		n = len(all)
	}
	return all[:n], nil
}

// doSearch runs one search request and returns the response body, turning a
// non-2xx status into an error that carries a bounded slice of the body — the
// provider's error JSON is the useful diagnostic. Shared by all three backends.
func doSearch(client *http.Client, req *http.Request, provider string) ([]byte, error) {
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: send: %w", provider, err)
	}
	defer resp.Body.Close()
	body, err := readAllLimited(resp)
	if err != nil {
		return nil, fmt.Errorf("%s: read: %w", provider, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s: search failed (status %d): %s", provider, resp.StatusCode, clip(string(body), 300))
	}
	return body, nil
}

// maxSearchBody caps how much of a provider response we read, so a runaway body
// cannot exhaust memory.
const maxSearchBody = 4 << 20 // 4 MiB

// readAllLimited reads at most maxSearchBody bytes of a response body.
func readAllLimited(resp *http.Response) ([]byte, error) {
	return io.ReadAll(io.LimitReader(resp.Body, maxSearchBody))
}
