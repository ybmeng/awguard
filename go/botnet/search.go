package botnet

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// Web search, owned by botnet rather than fused into OpenRouter — the shift the
// ToolCalls audit trail exists for (see the DECISION on Message). A backend runs
// one query against a provider's REST search API and normalizes the raw response
// to []SearchResult; a Router holds the available backends and the active
// selection. The web_search tool handler (tools.go) calls the active backend.
//
// DECISION (interface + router, not if-provider branches): each provider is a
// SearchBackend; the Router picks one. Adding the OpenWebSearch aggregator or a
// per-bot backend choice later is a new backend and a selection rule, not a new
// branch threaded through the tool loop.
//
// DECISION (stdlib only): providers are called with net/http and encoding/json,
// no provider SDKs — the same discipline as the OpenRouter client.

// SearchResult is one backend-normalized web result. Title and URL are always
// set (Title falls back to the URL host when the provider gives none); Snippet
// and PublishedAt are best-effort and may be empty.
type SearchResult struct {
	Title       string
	URL         string
	Snippet     string // short description/excerpt
	PublishedAt string // provider date if any, RFC3339 or raw; optional
}

// SearchOpts carries the knobs the model may set on a search. NumResults is a
// hint; 0 means "use the backend's default".
type SearchOpts struct {
	NumResults int
}

// SearchResponse is one backend's normalized answer: the results plus the
// provider's request/response id when it exposes one (""); the tool loop records
// RequestID on the ToolCall audit for debugging. Returning it as a struct (not a
// second string) keeps the interface open to more per-search provenance later
// without touching every backend again.
type SearchResponse struct {
	Results   []SearchResult
	RequestID string
}

// SearchBackend is one web-search provider. Search makes a network call, so it
// takes a context — the tool loop threads the turn's context through to here.
type SearchBackend interface {
	Name() string
	Search(ctx context.Context, query string, opts SearchOpts) (SearchResponse, error)
}

// Router holds the available backends in preference order and the active
// selection. A nil *Router, or one with no backends, means client search is off
// — the caller then keeps offering OpenRouter's fused server tool as fallback.
type Router struct {
	backends []SearchBackend // available, in fixed preference order
	active   SearchBackend   // the selected backend; nil when none is available
}

// NewRouter builds a router over the already-available backends (each one's key
// is assumed resolved by the caller), selecting the active one. prefer names a
// backend to activate by Name(); an unknown or empty prefer falls back to the
// first available in the order given, which is the fixed preference order
// NewRouterFromEnv supplies (Exa, Brave, Tavily).
func NewRouter(available []SearchBackend, prefer string) *Router {
	r := &Router{backends: available}
	if len(available) == 0 {
		return r
	}
	if prefer != "" {
		for _, b := range available {
			if b.Name() == prefer {
				r.active = b
				return r
			}
		}
	}
	r.active = available[0]
	return r
}

// Available reports whether the router has a backend to run a search — the gate
// on offering the client web_search function tool at all.
func (r *Router) Available() bool { return r != nil && r.active != nil }

// Active returns the selected backend; nil when none is available. Only call it
// after Available reports true.
func (r *Router) Active() SearchBackend {
	if r == nil {
		return nil
	}
	return r.active
}

// Names lists the available backends in preference order — for diagnostics.
func (r *Router) Names() []string {
	if r == nil {
		return nil
	}
	names := make([]string, len(r.backends))
	for i, b := range r.backends {
		names[i] = b.Name()
	}
	return names
}

// NewRouterFromEnv resolves each provider's key (env var, else the per-provider
// config file) and builds a router over exactly the backends whose key resolves,
// in the fixed preference order Exa, Brave, Tavily. SEARCH_BACKEND optionally
// names the active one; otherwise the first available wins. Called by botnetd —
// tests build a Router directly so ambient env cannot make them non-deterministic.
func NewRouterFromEnv() *Router {
	prefer := os.Getenv("SEARCH_BACKEND")
	var available []SearchBackend
	// The mock backend is keyless and dev-only: it exists only when explicitly
	// selected, so the whole search pipe can be exercised without a provider key
	// and can never activate by accident.
	if prefer == "mock" {
		available = append(available, NewMockBackend())
	}
	if key := searchKey("EXA_API_KEY", "exa"); key != "" {
		available = append(available, NewExaBackend(key))
	}
	if key := searchKey("BRAVE_API_KEY", "brave"); key != "" {
		available = append(available, NewBraveBackend(key))
	}
	if key := searchKey("TAVILY_API_KEY", "tavily"); key != "" {
		available = append(available, NewTavilyBackend(key))
	}
	return NewRouter(available, prefer)
}

// searchKey resolves one provider's key: the env var, else the trimmed contents
// of ~/.config/botnet/<provider>.txt. This mirrors botnetd's apiKey() for the
// OpenRouter key, so all keys are configured the same way.
func searchKey(envVar, provider string) string {
	if k := strings.TrimSpace(os.Getenv(envVar)); k != "" {
		return k
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(home, ".config", "botnet", provider+".txt"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// resultHost is the Title fallback: the host of the result url, or the raw url
// if it does not parse — the same rule citationHost applies to annotations.
func resultHost(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return u.Host
	}
	return raw
}
