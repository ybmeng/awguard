package botnet

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// The client-side search router, tested at three levels: the router's
// availability/selection logic against a fake backend; each real backend's wire
// request and response→SearchResult mapping against a recorded sample body; and
// the whole tool loop, where a scripted model calls web_search, the router runs
// a fake backend, and the results are fed back, aggregated into citations, and
// recorded in the turn's ToolCall audit trail alongside a memory call.

// fakeBackend is a SearchBackend that returns canned results (or an error) and
// records every query and options it was asked for.
type fakeBackend struct {
	name      string
	results   []SearchResult
	requestID string
	err       error

	mu      sync.Mutex
	queries []string
	opts    []SearchOpts
}

func (f *fakeBackend) Name() string { return f.name }

func (f *fakeBackend) Search(_ context.Context, query string, opts SearchOpts) (SearchResponse, error) {
	f.mu.Lock()
	f.queries = append(f.queries, query)
	f.opts = append(f.opts, opts)
	f.mu.Unlock()
	return SearchResponse{Results: f.results, RequestID: f.requestID}, f.err
}

func (f *fakeBackend) lastQuery(t *testing.T) (string, SearchOpts) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.queries) == 0 {
		t.Fatal("the backend was never queried")
	}
	return f.queries[len(f.queries)-1], f.opts[len(f.opts)-1]
}

// TestMockBackendDeterministic: the dev/verification backend returns a stable,
// varied result set — same results on repeat calls — including a titleless
// source mapped to its host, and honors the result-count clamp.
func TestMockBackendDeterministic(t *testing.T) {
	var b SearchBackend = MockBackend{}
	if b.Name() != "mock" {
		t.Fatalf("name = %q, want mock", b.Name())
	}
	first, err := b.Search(context.Background(), "anything", SearchOpts{})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	second, _ := b.Search(context.Background(), "anything", SearchOpts{})
	if len(first.Results) != len(second.Results) || len(first.Results) == 0 {
		t.Fatalf("mock is not deterministic: %d vs %d", len(first.Results), len(second.Results))
	}
	for i := range first.Results {
		if first.Results[i] != second.Results[i] {
			t.Errorf("result %d differs across calls: %+v vs %+v", i, first.Results[i], second.Results[i])
		}
	}
	// The synthetic request id is present and stable across calls — the provenance
	// field is exercised end-to-end with no real provider.
	if first.RequestID == "" || first.RequestID != second.RequestID {
		t.Errorf("mock request id = %q / %q, want a stable non-empty synthetic id", first.RequestID, second.RequestID)
	}
	// Every result has a non-empty title and url (the contract), and one title is
	// a bare host — the titleless→host-fallback the UI renders.
	sawHost := false
	for _, r := range first.Results {
		if r.Title == "" || r.URL == "" {
			t.Errorf("result missing title/url: %+v", r)
		}
		if r.Title == "example.org" {
			sawHost = true
		}
	}
	if !sawHost {
		t.Error("no host-fallback title present; the UI's titleless path is not exercised")
	}
	// num_results clamps the set.
	if got, _ := b.Search(context.Background(), "q", SearchOpts{NumResults: 2}); len(got.Results) != 2 {
		t.Errorf("num_results=2 returned %d results, want 2", len(got.Results))
	}
}

// (SEARCH_BACKEND=mock selection through NewRouterFromEnv is covered by
// TestMockBackendSelectedByEnv in mock_search_test.go.)

// TestRouterAvailabilityAndSelection pins the router's gating and active pick:
// no backends → unavailable; first available wins by default; SEARCH_BACKEND-
// style prefer selects by name; an unknown prefer falls back to the first; and a
// nil *Router is safely unavailable.
func TestRouterAvailabilityAndSelection(t *testing.T) {
	exa := &fakeBackend{name: "exa"}
	brave := &fakeBackend{name: "brave"}

	// No backends: unavailable, no active.
	empty := NewRouter(nil, "")
	if empty.Available() {
		t.Error("a router with no backends reports available")
	}
	if empty.Active() != nil {
		t.Error("an empty router has an active backend")
	}

	// Default: first available in the order given.
	def := NewRouter([]SearchBackend{exa, brave}, "")
	if !def.Available() || def.Active().Name() != "exa" {
		t.Errorf("default active = %v, want exa", names(def))
	}

	// Prefer selects by name.
	pref := NewRouter([]SearchBackend{exa, brave}, "brave")
	if pref.Active().Name() != "brave" {
		t.Errorf("preferred active = %q, want brave", pref.Active().Name())
	}

	// Unknown prefer falls back to the first available.
	unknown := NewRouter([]SearchBackend{exa, brave}, "duckduckgo")
	if unknown.Active().Name() != "exa" {
		t.Errorf("unknown prefer active = %q, want the first (exa)", unknown.Active().Name())
	}

	// A nil router is safely unavailable — the "no client backend" case.
	var nilRouter *Router
	if nilRouter.Available() {
		t.Error("a nil router reports available")
	}
}

func names(r *Router) []string { return r.Names() }

// backendUpstream wires a backend's http.Client at a recording test server that
// answers with the given body, and returns the server so the test can read what
// the backend sent. The rewriteHost transport keeps the backend's real path,
// headers and body while redirecting the host.
func backendUpstream(t *testing.T, respBody string) (*httptest.Server, *recorder) {
	t.Helper()
	rec := &recorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rec.record(r, body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, respBody)
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

type recorder struct {
	mu     sync.Mutex
	method string
	path   string
	query  string
	header http.Header
	body   []byte
}

func (rc *recorder) record(r *http.Request, body []byte) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	rc.method, rc.path, rc.query, rc.header, rc.body = r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Clone(), body
}

// aimAt points a backend's client at the test upstream.
func aimAt(c *http.Client, url string) {
	c.Transport = rewriteHost{url, http.DefaultTransport}
}

// TestExaBackendWire: the request is a POST to /search with the x-api-key header
// and a numResults body, and a recorded response maps to SearchResult with the
// highlight as the snippet, the published date carried, and a missing title
// falling back to the URL host.
func TestExaBackendWire(t *testing.T) {
	const resp = `{
		"requestId":"r1",
		"results":[
			{"title":"Go 1.24 released","url":"https://go.dev/blog/go1.24","publishedDate":"2025-02-11T00:00:00.000Z",
			 "text":"The Go team is delighted to announce the release of Go 1.24. Full body here.",
			 "highlights":["Go 1.24 adds generic type aliases and a faster runtime."]},
			{"title":"","url":"https://example.org/untitled","text":"Some text excerpt with no highlight and no title."}
		]
	}`
	srv, rec := backendUpstream(t, resp)
	b := NewExaBackend("exa-key")
	aimAt(b.http, srv.URL)

	sr, err := b.Search(context.Background(), "go 1.24", SearchOpts{NumResults: 3})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if rec.method != http.MethodPost || rec.path != "/search" {
		t.Errorf("request = %s %s, want POST /search", rec.method, rec.path)
	}
	if got := rec.header.Get("x-api-key"); got != "exa-key" {
		t.Errorf("x-api-key = %q, want the key", got)
	}
	var sent map[string]any
	if err := json.Unmarshal(rec.body, &sent); err != nil {
		t.Fatalf("decode sent body: %v", err)
	}
	if sent["query"] != "go 1.24" || sent["numResults"].(float64) != 3 {
		t.Errorf("sent body = %v, want query+numResults", sent)
	}
	// The top-level requestId is captured as the call's provenance id.
	if sr.RequestID != "r1" {
		t.Errorf("request id = %q, want r1 from the response body", sr.RequestID)
	}
	results := sr.Results
	if len(results) != 2 {
		t.Fatalf("results = %d, want 2", len(results))
	}
	if r := results[0]; r.Title != "Go 1.24 released" || r.URL != "https://go.dev/blog/go1.24" ||
		r.Snippet != "Go 1.24 adds generic type aliases and a faster runtime." || r.PublishedAt == "" {
		t.Errorf("result 0 = %+v, want the mapped fields with the highlight as snippet", r)
	}
	// No title → host fallback; no highlight → text as snippet.
	if r := results[1]; r.Title != "example.org" || !strings.HasPrefix(r.Snippet, "Some text excerpt") {
		t.Errorf("result 1 = %+v, want host-fallback title and text snippet", r)
	}
}

// TestBraveBackendWire: a GET to /res/v1/web/search with the X-Subscription-Token
// header and count/text_decorations query params, mapping web.results[] with the
// description as snippet and page_age as the date.
func TestBraveBackendWire(t *testing.T) {
	const resp = `{
		"web":{"results":[
			{"title":"Machine learning","url":"https://ml.example/guide","description":"An intro to ML.","page_age":"2023-06-01T12:00:00","age":"June 1, 2023"},
			{"url":"https://notitle.example/x","description":"No title here."}
		]}
	}`
	srv, rec := backendUpstream(t, resp)
	b := NewBraveBackend("brave-key")
	aimAt(b.http, srv.URL)

	sr, err := b.Search(context.Background(), "machine learning", SearchOpts{NumResults: 4})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if rec.method != http.MethodGet || rec.path != "/res/v1/web/search" {
		t.Errorf("request = %s %s, want GET /res/v1/web/search", rec.method, rec.path)
	}
	if got := rec.header.Get("X-Subscription-Token"); got != "brave-key" {
		t.Errorf("X-Subscription-Token = %q, want the key", got)
	}
	if !strings.Contains(rec.query, "count=4") || !strings.Contains(rec.query, "text_decorations=false") {
		t.Errorf("query = %q, want count and text_decorations", rec.query)
	}
	// Brave exposes no request id in the body, so it stays empty.
	if sr.RequestID != "" {
		t.Errorf("request id = %q, want empty for Brave", sr.RequestID)
	}
	results := sr.Results
	if len(results) != 2 {
		t.Fatalf("results = %d, want 2", len(results))
	}
	if r := results[0]; r.Title != "Machine learning" || r.Snippet != "An intro to ML." || r.PublishedAt != "2023-06-01T12:00:00" {
		t.Errorf("result 0 = %+v, want mapped fields with page_age date", r)
	}
	if r := results[1]; r.Title != "notitle.example" {
		t.Errorf("result 1 title = %q, want host fallback", r.Title)
	}
}

// TestTavilyBackendWire: a POST to /search with the Bearer auth header and a
// max_results body, mapping results[] with content as snippet and published_date
// carried when present.
func TestTavilyBackendWire(t *testing.T) {
	const resp = `{
		"query":"leo messi",
		"response_id":"tav-abc123",
		"results":[
			{"title":"Lionel Messi","url":"https://britannica.example/messi","content":"Argentine footballer.","score":0.8},
			{"title":"Messi news","url":"https://news.example/messi","content":"Latest.","published_date":"Wed, 27 Aug 2025 12:34:56 GMT"}
		],
		"response_time":1.2
	}`
	srv, rec := backendUpstream(t, resp)
	b := NewTavilyBackend("tavily-key")
	aimAt(b.http, srv.URL)

	result, err := b.Search(context.Background(), "leo messi", SearchOpts{})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if rec.method != http.MethodPost || rec.path != "/search" {
		t.Errorf("request = %s %s, want POST /search", rec.method, rec.path)
	}
	if got := rec.header.Get("Authorization"); got != "Bearer tavily-key" {
		t.Errorf("Authorization = %q, want Bearer <key>", got)
	}
	var sent map[string]any
	if err := json.Unmarshal(rec.body, &sent); err != nil {
		t.Fatalf("decode sent body: %v", err)
	}
	// Zero NumResults maps to the default, not 0.
	if sent["max_results"].(float64) != float64(defaultSearchResults) {
		t.Errorf("max_results = %v, want the default %d", sent["max_results"], defaultSearchResults)
	}
	// The top-level response_id is captured as the call's provenance id.
	if result.RequestID != "tav-abc123" {
		t.Errorf("request id = %q, want tav-abc123 from response_id", result.RequestID)
	}
	results := result.Results
	if len(results) != 2 {
		t.Fatalf("results = %d, want 2", len(results))
	}
	if r := results[0]; r.Title != "Lionel Messi" || r.Snippet != "Argentine footballer." || r.PublishedAt != "" {
		t.Errorf("result 0 = %+v, want content as snippet and empty date", r)
	}
	if r := results[1]; r.PublishedAt != "Wed, 27 Aug 2025 12:34:56 GMT" {
		t.Errorf("result 1 date = %q, want the news published_date", r.PublishedAt)
	}
}

// TestBackendNon2xxIsError: a provider error status becomes an error carrying a
// slice of the body, so the tool handler can report it and audit the failure.
func TestBackendNon2xxIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":"invalid api key"}`)
	}))
	t.Cleanup(srv.Close)
	b := NewTavilyBackend("bad")
	aimAt(b.http, srv.URL)
	_, err := b.Search(context.Background(), "q", SearchOpts{})
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("err = %v, want a 401 error carrying the body", err)
	}
}

// toolCallResp is one scripted assistant turn calling a named function tool.
func toolCallResp(callID, name, args string) string {
	return `{"choices":[{"message":{"role":"assistant","content":"",` +
		`"tool_calls":[{"id":"` + callID + `","type":"function",` +
		`"function":{"name":"` + name + `","arguments":` + strconv.Quote(args) + `}}]}}]}`
}

// TestClientWebSearchToolOffered: with a router that has a backend, the request
// offers botnet's own web_search FUNCTION tool — not OpenRouter's server tool.
func TestClientWebSearchToolOffered(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	bot := newBot(t, s)

	router := NewRouter([]SearchBackend{&fakeBackend{name: "fake"}}, "")
	sc := &scriptedUpstream{responses: []string{contentResponse("ok")}}
	or := newScriptedOpenRouter(t, sc)
	if _, err := or.Complete(context.Background(), Prompt{
		Bot:      bot,
		Messages: []Message{{Role: "user", Content: "hi"}},
		Tools:    NewBotToolbox(s, bot.ID, router),
	}); err != nil {
		t.Fatalf("complete: %v", err)
	}

	var payload struct {
		Tools []json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(sc.requests[0], &payload); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	if len(payload.Tools) != 2 {
		t.Fatalf("offered %d tools, want 2 (memory, web_search)", len(payload.Tools))
	}
	var searchTool struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(payload.Tools[1], &searchTool); err != nil {
		t.Fatalf("decode web_search tool: %v", err)
	}
	if searchTool.Type != "function" || searchTool.Function.Name != webSearchFuncName {
		t.Errorf("tool 1 = %s, want the web_search function tool", payload.Tools[1])
	}
}

// TestWebSearchToolLoopRecordsAudit walks the whole feature: the model calls
// web_search, the router runs the fake backend, the results are fed back and
// then a memory call runs, and the turn's Completion carries the aggregate
// citations plus an ordered ToolCall audit trail with the query, backend and
// structured results — which persists on the reply and survives a reload.
func TestWebSearchToolLoopRecordsAudit(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	bot := newBot(t, s)
	user, err := s.AppendMessage(bot.ID, "user", "what's new in Go?", StatusAwaiting)
	if err != nil {
		t.Fatalf("seed user turn: %v", err)
	}

	backend := &fakeBackend{name: "fake", requestID: "req-xyz789", results: []SearchResult{
		{Title: "Go 1.24", URL: "https://go.dev/1", Snippet: "generics", PublishedAt: "2025-02-11"},
		{Title: "Release notes", URL: "https://go.dev/2", Snippet: "notes"},
	}}
	router := NewRouter([]SearchBackend{backend}, "")

	sc := &scriptedUpstream{responses: []string{
		toolCallResp("call_1", "web_search", `{"query":"go release news","num_results":2}`),
		toolCallResp("call_2", "memory", `{"command":"replace","content":"user follows Go releases"}`),
		contentResponse("Go 1.24 is out."),
	}}
	or := newScriptedOpenRouter(t, sc)

	comp, err := or.Complete(context.Background(), Prompt{
		Bot:      bot,
		Messages: []Message{{Role: "user", Content: "what's new in Go?"}},
		Tools:    NewBotToolbox(s, bot.ID, router),
	})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if comp.Content != "Go 1.24 is out." {
		t.Errorf("content = %q, want the final answer", comp.Content)
	}

	// The backend saw the model's query and result count.
	if q, opts := backend.lastQuery(t); q != "go release news" || opts.NumResults != 2 {
		t.Errorf("backend saw (%q, %d), want the model's query and count", q, opts.NumResults)
	}

	// Aggregate citations = the web_search results.
	if len(comp.Citations) != 2 || comp.Citations[0].URL != "https://go.dev/1" {
		t.Errorf("citations = %+v, want the 2 search results aggregated", comp.Citations)
	}

	// The audit trail: web_search first (with backend + structured results), then
	// memory.
	if len(comp.ToolCalls) != 2 {
		t.Fatalf("tool calls = %+v, want 2 (web_search, memory)", comp.ToolCalls)
	}
	ws := comp.ToolCalls[0]
	if ws.Name != webSearchFuncName || ws.Backend != "fake" || len(ws.Results) != 2 {
		t.Errorf("web_search audit = %+v, want name, backend and 2 results", ws)
	}
	if ws.RequestID != "req-xyz789" {
		t.Errorf("web_search request id = %q, want the backend's provider id captured", ws.RequestID)
	}
	if !strings.Contains(ws.Arguments, "go release news") {
		t.Errorf("web_search arguments = %q, want the query recoverable", ws.Arguments)
	}
	if !strings.Contains(ws.Result, "go.dev/1") {
		t.Errorf("web_search result text = %q, want the rendered list", ws.Result)
	}
	mem := comp.ToolCalls[1]
	if mem.Name != memoryToolName || mem.Result != "memory saved" || mem.Backend != "" || mem.RequestID != "" || mem.Results != nil {
		t.Errorf("memory audit = %+v, want a bare memory record", mem)
	}

	// Persist on the reply and reload: both citations and tool_calls survive.
	reply, err := s.CompleteTurn(bot.ID, user.ID, comp.Content, comp.Citations, comp.ToolCalls)
	if err != nil {
		t.Fatalf("complete turn: %v", err)
	}
	got, err := s.GetMessage(reply.ID)
	if err != nil {
		t.Fatalf("get message: %v", err)
	}
	if len(got.Citations) != 2 {
		t.Errorf("reloaded citations = %d, want 2", len(got.Citations))
	}
	if len(got.ToolCalls) != 2 || got.ToolCalls[0].Backend != "fake" ||
		got.ToolCalls[0].RequestID != "req-xyz789" || len(got.ToolCalls[0].Results) != 2 {
		t.Errorf("reloaded tool_calls = %+v, want the audit trail (with request id) kept", got.ToolCalls)
	}
}

// TestWebSearchBackendFailureIsAudited: a backend error becomes an instructive
// tool result the model can answer past, and the failed call is still recorded
// with its backend named and no results — the turn does not fail.
func TestWebSearchBackendFailureIsAudited(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	bot := newBot(t, s)

	backend := &fakeBackend{name: "fake", err: io.ErrUnexpectedEOF}
	router := NewRouter([]SearchBackend{backend}, "")

	sc := &scriptedUpstream{responses: []string{
		toolCallResp("call_1", "web_search", `{"query":"anything"}`),
		contentResponse("I could not search, but here is what I know."),
	}}
	or := newScriptedOpenRouter(t, sc)

	comp, err := or.Complete(context.Background(), Prompt{
		Bot:   bot,
		Tools: NewBotToolbox(s, bot.ID, router),
	})
	if err != nil {
		t.Fatalf("a backend error failed the turn: %v", err)
	}
	if len(comp.ToolCalls) != 1 {
		t.Fatalf("tool calls = %+v, want the failed call recorded", comp.ToolCalls)
	}
	ws := comp.ToolCalls[0]
	if ws.Backend != "fake" || len(ws.Results) != 0 || !strings.Contains(ws.Result, "error: web search failed") {
		t.Errorf("failed web_search audit = %+v, want an error result with the backend named", ws)
	}
	if len(comp.Citations) != 0 {
		t.Errorf("citations = %+v, want none on a failed search", comp.Citations)
	}
	// The tool result fed back to the model was the instructive error.
	if got := findToolResult(t, sc.request(t, 1), "call_1"); !strings.Contains(got.Content, "web search failed") {
		t.Errorf("tool result to model = %q, want the instructive error", got.Content)
	}
}

// TestToolResultTruncatedInAudit: a huge search result is stored truncated in the
// audit record (with a marker), while the model still receives the full text.
func TestToolResultTruncatedInAudit(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	bot := newBot(t, s)

	// A backend result whose rendered text far exceeds the storage cap.
	huge := strings.Repeat("x", maxToolResultBytes*2)
	backend := &fakeBackend{name: "fake", results: []SearchResult{{Title: "Big", URL: "https://big.example", Snippet: huge}}}
	router := NewRouter([]SearchBackend{backend}, "")

	sc := &scriptedUpstream{responses: []string{
		toolCallResp("call_1", "web_search", `{"query":"big"}`),
		contentResponse("done"),
	}}
	or := newScriptedOpenRouter(t, sc)

	comp, err := or.Complete(context.Background(), Prompt{Bot: bot, Tools: NewBotToolbox(s, bot.ID, router)})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	stored := comp.ToolCalls[0].Result
	if len(stored) > maxToolResultBytes+len("\n…[truncated]") {
		t.Errorf("stored result is %d bytes, want it capped near %d", len(stored), maxToolResultBytes)
	}
	if !strings.HasSuffix(stored, "[truncated]") {
		t.Errorf("stored result = ...%q, want the truncation marker", stored[len(stored)-20:])
	}
	// The model got the full text: the tool-role message the loop appended is the
	// full render, not the truncated one.
	full := findToolResult(t, sc.request(t, 1), "call_1")
	if len(full.Content) < maxToolResultBytes {
		t.Errorf("model received %d bytes, want the full untruncated result", len(full.Content))
	}
}

// TestToolsEndpointReflectsConfiguredSearch: /v1/tools reflects the server's
// actual search config — the web_search function tool when a backend is
// configured, the OpenRouter server tool when none is.
func TestToolsEndpointReflectsConfiguredSearch(t *testing.T) {
	// No backend configured: the fallback server tool.
	h := newHarness(t, &fakeLLM{})
	if got := toolNamesFromEndpoint(t, h.ts.URL+"/v1/tools"); got != memoryToolName+","+webSearchToolName {
		t.Errorf("no-backend /v1/tools = %q, want the OpenRouter server tool fallback", got)
	}

	// A backend configured: the client function tool.
	h.srv.ConfigureSearch(NewRouter([]SearchBackend{&fakeBackend{name: "fake"}}, ""))
	if got := toolNamesFromEndpoint(t, h.ts.URL+"/v1/tools"); got != memoryToolName+","+webSearchFuncName {
		t.Errorf("with-backend /v1/tools = %q, want the client web_search function tool", got)
	}
}

// toolNamesFromEndpoint fetches /v1/tools and returns a comma-joined list of the
// tools' identities — the function name for a function tool, the type for a
// server tool.
func toolNamesFromEndpoint(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("get tools: %v", err)
	}
	defer resp.Body.Close()
	var tools []json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&tools); err != nil {
		t.Fatalf("decode tools: %v", err)
	}
	var out []string
	for _, raw := range tools {
		var tool struct {
			Type     string `json:"type"`
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		}
		if err := json.Unmarshal(raw, &tool); err != nil {
			t.Fatalf("decode tool: %v", err)
		}
		if tool.Type == "function" {
			out = append(out, tool.Function.Name)
		} else {
			out = append(out, tool.Type)
		}
	}
	return strings.Join(out, ",")
}
