package botnet

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

// OpenRouter's web_search server tool, tested at the wire: the request must
// offer it alongside the memory function tool, and a reply carrying
// url_citation annotations must map to the shared Citation shape, persist, and
// re-serve. The search resolves server-side, so it never reaches the tool loop
// as a call to dispatch — these tests pin that it enters as annotations only.

// annotationResponse is one assistant reply carrying url_citation annotations,
// the shape OpenRouter returns when the model used web_search.
func annotationResponse(content string, anns string) string {
	return `{"choices":[{"message":{"role":"assistant","content":` +
		jsonString(content) + `,"annotations":` + anns + `}}]}`
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// TestWebSearchToolOfferedWithCorrectShape: request 0's tools array carries the
// memory FUNCTION tool AND the web_search SERVER tool, and the server tool
// marshals as exactly {"type":"openrouter:web_search"} — no bogus empty
// "function" object dragged along.
func TestWebSearchToolOfferedWithCorrectShape(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	bot := newBot(t, s)

	sc := &scriptedUpstream{responses: []string{contentResponse("ok")}}
	or := newScriptedOpenRouter(t, sc)
	if _, err := or.Complete(context.Background(), Prompt{
		Bot:      bot,
		Messages: []Message{{Role: "user", Content: "hi"}},
		Tools:    NewBotToolbox(s, bot.ID, nil),
	}); err != nil {
		t.Fatalf("complete: %v", err)
	}

	// Decode the tools array element-by-element so each tool's exact fields
	// survive — a []wireTool decode would hide a leaked empty "function".
	var payload struct {
		Tools []json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(sc.requests[0], &payload); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	if len(payload.Tools) != 2 {
		t.Fatalf("offered %d tools, want 2 (memory, web_search): %s", len(payload.Tools), sc.requests[0])
	}

	// The memory tool is a function tool.
	var memoryTool struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(payload.Tools[0], &memoryTool); err != nil {
		t.Fatalf("decode memory tool: %v", err)
	}
	if memoryTool.Type != "function" || memoryTool.Function.Name != memoryToolName {
		t.Errorf("tool 0 = %s, want the memory function tool", payload.Tools[0])
	}

	// The web_search tool is the server tool: exactly type, and no function key.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload.Tools[1], &fields); err != nil {
		t.Fatalf("decode web_search tool: %v", err)
	}
	if got := string(fields["type"]); got != jsonString(webSearchToolName) {
		t.Errorf("web_search type = %s, want %q", got, webSearchToolName)
	}
	if _, leaked := fields["function"]; leaked {
		t.Errorf("web_search tool leaked a function key: %s", payload.Tools[1])
	}
	if len(fields) != 1 {
		t.Errorf("web_search tool has fields %v, want only type", keysOf(fields))
	}
}

func keysOf(m map[string]json.RawMessage) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}

// TestWebSearchAnnotationsMapToCitations: a reply whose annotations carry
// url_citation entries comes back from Complete as ordered Citations — snippet
// and indices carried, a missing title falling back to the url host, and a
// non-url_citation annotation skipped.
func TestWebSearchAnnotationsMapToCitations(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	bot := newBot(t, s)

	anns := `[
		{"type":"url_citation","url_citation":{"url":"https://a.example/news","title":"First","content":"an excerpt","start_index":10,"end_index":20}},
		{"type":"other","url_citation":{"url":"https://ignored.example"}},
		{"type":"url_citation","url_citation":{"url":"https://b.example/path?q=1","title":"","content":"","start_index":0,"end_index":0}}
	]`
	sc := &scriptedUpstream{responses: []string{annotationResponse("here is what I found", anns)}}
	or := newScriptedOpenRouter(t, sc)

	comp, err := or.Complete(context.Background(), Prompt{
		Bot:      bot,
		Messages: []Message{{Role: "user", Content: "what happened today?"}},
		Tools:    NewBotToolbox(s, bot.ID, nil),
	})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if comp.Content != "here is what I found" {
		t.Errorf("content = %q, want the synthesized answer", comp.Content)
	}
	if len(comp.Citations) != 2 {
		t.Fatalf("citations = %+v, want 2 (the non-url_citation dropped)", comp.Citations)
	}
	first := comp.Citations[0]
	if first.URL != "https://a.example/news" || first.Title != "First" ||
		first.Snippet != "an excerpt" || first.StartIndex != 10 || first.EndIndex != 20 {
		t.Errorf("citation 0 = %+v, want the full annotation carried", first)
	}
	// Second citation had no title: it falls back to the url host.
	if second := comp.Citations[1]; second.Title != "b.example" {
		t.Errorf("citation 1 title = %q, want the host fallback %q", second.Title, "b.example")
	}
}

// TestCitationsPersistAndReserve: a bot reply that cited sources stores them,
// and they come back on every read — a reload keeps the sources. The API JSON
// carries a "citations" key only when there are citations.
func TestCitationsPersistAndReserve(t *testing.T) {
	h := newHarness(t, &fakeLLM{reply: "synthesized answer"})
	cites := []Citation{
		{URL: "https://one.example", Title: "One", Snippet: "ex1", StartIndex: 1, EndIndex: 2},
		{URL: "https://two.example", Title: "Two"},
	}
	h.llm.setCitations(cites)

	bot := createBot(t, h, "Searcher")
	conv := sendAndSettle(t, h, bot.ID, `"search please"`)

	var reply Message
	found := false
	for _, m := range conv {
		if m.Role == "bot" {
			reply, found = m, true
		}
	}
	if !found {
		t.Fatalf("no bot reply in %+v", conv)
	}
	if len(reply.Citations) != 2 {
		t.Fatalf("reply carried %d citations, want 2: %+v", len(reply.Citations), reply.Citations)
	}
	if reply.Citations[0] != cites[0] || reply.Citations[1] != cites[1] {
		t.Errorf("citations = %+v, want %+v in order", reply.Citations, cites)
	}

	// A reload by id keeps them — this is the "sources survive" guarantee.
	var reloaded Message
	get(t, h.ts.URL+"/v1/messages/"+reply.ID, &reloaded)
	if len(reloaded.Citations) != 2 || reloaded.Citations[0] != cites[0] {
		t.Errorf("reloaded citations = %+v, want them kept", reloaded.Citations)
	}

	// The raw transcript JSON carries the key, so a client that decodes it sees
	// the sources.
	raw := rawGet(t, h.bot(bot.ID, "/messages"))
	if !strings.Contains(raw, `"citations"`) {
		t.Errorf("transcript JSON has no citations key: %s", raw)
	}
}

// TestReplyWithoutCitationsOmitsTheKey: the common turn cites nothing, and the
// message JSON must then omit "citations" entirely (omitempty), which is the
// shape the client decodes as absent.
func TestReplyWithoutCitationsOmitsTheKey(t *testing.T) {
	h := newHarness(t, &fakeLLM{reply: "no sources here"})
	bot := createBot(t, h, "Plain")
	sendAndSettle(t, h, bot.ID, `"just chat"`)

	raw := rawGet(t, h.bot(bot.ID, "/messages"))
	if strings.Contains(raw, "citations") {
		t.Errorf("a no-citation transcript still carries the key: %s", raw)
	}
}

// TestToolsEndpointIncludesWebSearch: the inspector reads /v1/tools, so the
// web_search server tool must actually appear there, in its server-tool shape.
// (TestToolsEndpointServesTheWireTools already pins that this body cannot drift
// from what the chat request sends.)
func TestToolsEndpointIncludesWebSearch(t *testing.T) {
	h := newHarness(t, &fakeLLM{})
	resp, err := http.Get(h.ts.URL + "/v1/tools")
	if err != nil {
		t.Fatalf("get /v1/tools: %v", err)
	}
	defer resp.Body.Close()
	var tools []json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&tools); err != nil {
		t.Fatalf("decode tools: %v", err)
	}
	if len(tools) != 2 {
		t.Fatalf("/v1/tools served %d tools, want 2", len(tools))
	}
	var last map[string]json.RawMessage
	if err := json.Unmarshal(tools[1], &last); err != nil {
		t.Fatalf("decode web_search tool: %v", err)
	}
	if string(last["type"]) != jsonString(webSearchToolName) {
		t.Errorf("/v1/tools web_search type = %s, want %q", last["type"], webSearchToolName)
	}
	if _, leaked := last["function"]; leaked {
		t.Errorf("/v1/tools web_search leaked a function key: %s", tools[1])
	}
}

// TestCitationsSurviveReopen: citations are a stored column, so they must still
// be there after the process restarts and the store is reopened.
func TestCitationsSurviveReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cites.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	bot := newBot(t, s)
	user, err := s.AppendMessage(bot.ID, "user", "q", StatusAwaiting)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	cites := []Citation{{URL: "https://kept.example", Title: "Kept", Snippet: "x", StartIndex: 3, EndIndex: 7}}
	reply, err := s.CompleteTurn(bot.ID, user.ID, "answer", cites, nil)
	if err != nil {
		t.Fatalf("complete turn: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if s, err = Open(path); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()
	got, err := s.GetMessage(reply.ID)
	if err != nil {
		t.Fatalf("get message: %v", err)
	}
	if len(got.Citations) != 1 || got.Citations[0] != cites[0] {
		t.Errorf("citations after reopen = %+v, want %+v", got.Citations, cites)
	}
}

// rawGet returns a GET body as a string, for asserting on the exact JSON keys.
func rawGet(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("get %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	return string(body)
}
