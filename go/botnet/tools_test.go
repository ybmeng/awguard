package botnet

import (
	"bytes"
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

// The model's memory tool, tested at the wire: a scripted fake OpenRouter
// upstream answers each chat-completions request with the next canned
// response, and the tests assert both halves of the loop — what the server
// sent (the one flat tool, the memory block, tool results) and what the
// executions did to the store.

// scriptedUpstream pops one response body per request and records every
// request body, so a test can walk the whole loop afterwards.
type scriptedUpstream struct {
	mu        sync.Mutex
	responses []string
	requests  [][]byte
}

func (sc *scriptedUpstream) pop(body []byte) string {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	sc.requests = append(sc.requests, body)
	if len(sc.responses) == 0 {
		return `{"error":{"message":"scripted upstream exhausted"}}`
	}
	next := sc.responses[0]
	sc.responses = sc.responses[1:]
	return next
}

func (sc *scriptedUpstream) requestCount() int {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	return len(sc.requests)
}

// request decodes the i-th recorded request body.
func (sc *scriptedUpstream) request(t *testing.T, i int) wireRequest {
	t.Helper()
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if i >= len(sc.requests) {
		t.Fatalf("only %d requests were made, wanted #%d", len(sc.requests), i)
	}
	var req wireRequest
	if err := json.Unmarshal(sc.requests[i], &req); err != nil {
		t.Fatalf("decode request %d: %v (body: %s)", i, err, sc.requests[i])
	}
	return req
}

// wireRequest is the slice of the chat-completions request these tests read.
type wireRequest struct {
	Messages []wireMsg  `json:"messages"`
	Tools    []wireTool `json:"tools"`
}

// newScriptedOpenRouter wires an OpenRouter client to a scripted upstream via
// the same host-rewriting seam TestOpenRouterSendsOneSummary uses.
func newScriptedOpenRouter(t *testing.T, sc *scriptedUpstream) *OpenRouter {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, sc.pop(body))
	}))
	t.Cleanup(upstream.Close)
	or := NewOpenRouter("test-key")
	or.HTTP = upstream.Client()
	or.HTTP.Transport = rewriteHost{upstream.URL, http.DefaultTransport}
	return or
}

// memoryCall is one assistant turn calling the memory tool with the given
// raw arguments object.
func memoryCall(callID, args string) string {
	return `{"choices":[{"message":{"role":"assistant","content":"",` +
		`"tool_calls":[{"id":"` + callID + `","type":"function",` +
		`"function":{"name":"memory","arguments":` + strconv.Quote(args) + `}}]}}]}`
}

func contentResponse(content string) string {
	return `{"choices":[{"message":{"role":"assistant","content":` + strconv.Quote(content) + `}}]}`
}

// findToolResult returns the tool-role message answering the given call id.
func findToolResult(t *testing.T, req wireRequest, callID string) wireMsg {
	t.Helper()
	for _, m := range req.Messages {
		if m.Role == "tool" && m.ToolCallID == callID {
			return m
		}
	}
	t.Fatalf("no tool result for call %q in %+v", callID, req.Messages)
	return wireMsg{}
}

// TestToolLoopReadReplaceAnswer walks the whole loop the feature exists for:
// the model reads its memory, replaces it, then answers — and the replace is a
// real store write, visible in the database and in change_log.
func TestToolLoopReadReplaceAnswer(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	bot := newBot(t, s)
	if _, err := s.SetMemory(bot.ID, "old notes"); err != nil {
		t.Fatalf("seed memory: %v", err)
	}
	mark := topSeq(t, s)

	sc := &scriptedUpstream{responses: []string{
		memoryCall("call_1", `{"command":"read"}`),
		memoryCall("call_2", `{"command":"replace","content":"the user likes Go"}`),
		contentResponse("noted!"),
	}}
	or := newScriptedOpenRouter(t, sc)

	reply, err := or.Complete(context.Background(), Prompt{
		Bot:      bot,
		Memory:   "old notes",
		Messages: []Message{{Role: "user", Content: "remember that I like Go"}},
		Tools:    NewBotToolbox(s, bot.ID, nil),
	})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if reply.Content != "noted!" {
		t.Errorf("reply = %q, want the final answer", reply.Content)
	}
	if got := sc.requestCount(); got != 3 {
		t.Fatalf("the loop made %d requests, want 3 (read, replace, answer)", got)
	}

	// Request 0 advertised the memory FUNCTION tool — flat schema with the strict
	// command enum and a description spelling out the commands — alongside the
	// web_search SERVER tool (its own shape is pinned in websearch_test.go).
	first := sc.request(t, 0)
	if len(first.Tools) != 2 || first.Tools[0].Function.Name != "memory" {
		t.Fatalf("tools advertised = %+v, want memory then web_search", first.Tools)
	}
	if first.Tools[1].Type != webSearchToolName {
		t.Errorf("second tool type = %q, want %q", first.Tools[1].Type, webSearchToolName)
	}
	fn := first.Tools[0].Function
	props, _ := fn.Parameters["properties"].(map[string]any)
	command, _ := props["command"].(map[string]any)
	var enum []string
	if raw, ok := command["enum"].([]any); ok {
		for _, v := range raw {
			enum = append(enum, v.(string))
		}
	}
	if strings.Join(enum, ",") != "read,replace,clear" {
		t.Errorf("command enum = %v, want read,replace,clear", enum)
	}
	if req, ok := fn.Parameters["required"].([]any); !ok || len(req) != 1 || req[0] != "command" {
		t.Errorf("required = %v, want just command", fn.Parameters["required"])
	}
	for _, must := range []string{`"replace"`, `"content"`, "overwrites", `"read"`, `"clear"`} {
		if !strings.Contains(fn.Description, must) {
			t.Errorf("tool description misses %s: %q", must, fn.Description)
		}
	}

	// The memory block was injected once, framed, carrying the blob.
	memoryBlocks := 0
	for _, m := range first.Messages {
		if m.Role == "system" && strings.Contains(m.Content, memoryPreamble) {
			memoryBlocks++
			if !strings.Contains(m.Content, "old notes") {
				t.Errorf("memory block %q does not carry the blob", m.Content)
			}
		}
	}
	if memoryBlocks != 1 {
		t.Errorf("request carried %d memory blocks, want exactly 1", memoryBlocks)
	}

	// Request 1 carried the read result: the CURRENT blob, as a tool turn.
	if got := findToolResult(t, sc.request(t, 1), "call_1"); got.Content != "old notes" {
		t.Errorf("read result = %q, want the stored blob", got.Content)
	}
	// Request 2 carried the replace's acknowledgement.
	if got := findToolResult(t, sc.request(t, 2), "call_2"); got.Content != "memory saved" {
		t.Errorf("replace result = %q", got.Content)
	}

	// The replace is durable: it landed in the database…
	after, err := s.GetBot(bot.ID)
	if err != nil {
		t.Fatalf("get bot: %v", err)
	}
	if after.Memory != "the user likes Go" {
		t.Errorf("memory after the turn = %q, want the model's edit", after.Memory)
	}
	// …and in the change feed, so a second client sees the model take notes.
	expectRows(t, logAfter(t, s, mark),
		[]changeRow{{"bot", string(bot.ID), "updated"}}, "memory replace tool")
}

// TestToolLoopMalformedCallRecovers: a replace with no content gets an
// INSTRUCTIVE tool-result error — consuming an iteration, not failing the
// turn — and the model self-corrects on the next call.
func TestToolLoopMalformedCallRecovers(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	bot := newBot(t, s)

	sc := &scriptedUpstream{responses: []string{
		memoryCall("call_1", `{"command":"replace"}`), // malformed: no content
		memoryCall("call_2", `{"command":"replace","content":"second try"}`),
		contentResponse("fixed it"),
	}}
	or := newScriptedOpenRouter(t, sc)

	reply, err := or.Complete(context.Background(), Prompt{Bot: bot, Tools: NewBotToolbox(s, bot.ID, nil)})
	if err != nil {
		t.Fatalf("a malformed tool call failed the turn: %v", err)
	}
	if reply.Content != "fixed it" {
		t.Errorf("reply = %q, want the recovered answer", reply.Content)
	}
	if got := findToolResult(t, sc.request(t, 1), "call_1"); got.Content != "error: 'replace' requires a 'content' field" {
		t.Errorf("malformed call result = %q, want the instructive error", got.Content)
	}
	if after, _ := s.GetBot(bot.ID); after.Memory != "second try" {
		t.Errorf("memory after recovery = %q, want the corrected replace", after.Memory)
	}
}

// TestToolLoopClearAndEmptyRead: clear erases the blob, and reading an empty
// memory answers with an explicit placeholder rather than "".
func TestToolLoopClearAndEmptyRead(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	bot := newBot(t, s)
	if _, err := s.SetMemory(bot.ID, "to be forgotten"); err != nil {
		t.Fatalf("seed memory: %v", err)
	}

	sc := &scriptedUpstream{responses: []string{
		memoryCall("call_1", `{"command":"clear"}`),
		memoryCall("call_2", `{"command":"read"}`),
		contentResponse("forgotten"),
	}}
	or := newScriptedOpenRouter(t, sc)

	if _, err := or.Complete(context.Background(), Prompt{
		Bot:   bot,
		Tools: NewBotToolbox(s, bot.ID, nil),
	}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if after, _ := s.GetBot(bot.ID); after.Memory != "" {
		t.Errorf("memory after clear = %q, want empty", after.Memory)
	}
	if got := findToolResult(t, sc.request(t, 2), "call_2"); got.Content != "(your memory is empty)" {
		t.Errorf("read of empty memory = %q, want the placeholder", got.Content)
	}
}

// TestMemoryToolValidation pins every instructive error at the Run seam: each
// malformed call answers the model with a correctable "error: ..." RESULT and
// a nil error, so no validation mistake can fail a turn.
func TestMemoryToolValidation(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	bot := newBot(t, s)
	tb := NewBotToolbox(s, bot.ID, nil)

	cases := []struct{ name, tool, args, want string }{
		{"unknown tool", "launch_missiles", `{}`,
			"error: unknown tool 'launch_missiles' — valid: memory, web_search"},
		{"bad json", "memory", `not json`,
			`error: arguments must be a JSON object like {"command": "read"}`},
		{"missing command", "memory", `{}`,
			"error: missing 'command' — valid: read, replace, clear"},
		{"unknown command", "memory", `{"command":"append"}`,
			"error: unknown command 'append' — valid: read, replace, clear"},
		{"replace without content", "memory", `{"command":"replace"}`,
			"error: 'replace' requires a 'content' field"},
		{"clear with content", "memory", `{"command":"clear","content":"x"}`,
			"error: 'clear' takes no 'content' field — use 'replace' to overwrite your memory"},
	}
	for _, c := range cases {
		got, err := tb.Run(context.Background(), c.tool, json.RawMessage(c.args))
		if err != nil {
			t.Errorf("%s: Run returned a turn-failing error %v, want an instructive result", c.name, err)
		}
		if got.text != c.want {
			t.Errorf("%s: result %q, want %q", c.name, got.text, c.want)
		}
	}

	// Nothing above wrote anything.
	if after, _ := s.GetBot(bot.ID); after.Memory != "" {
		t.Errorf("a rejected call wrote memory: %q", after.Memory)
	}
	// And an explicit empty content on replace is legal — it is a provided value.
	if got, err := tb.Run(context.Background(), "memory", json.RawMessage(`{"command":"replace","content":""}`)); err != nil || got.text != "memory saved" {
		t.Errorf("replace with empty content = (%q, %v), want it accepted", got.text, err)
	}
}

// TestToolLoopCap: a model that never stops calling tools is cut off at the
// cap, with the turn failing on an error that names it — the message then
// strands as failed like any other model error, retryable.
func TestToolLoopCap(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	bot := newBot(t, s)

	responses := make([]string, 0, maxToolIterations+2)
	for range maxToolIterations + 2 {
		responses = append(responses, memoryCall("call_x", `{"command":"read"}`))
	}
	sc := &scriptedUpstream{responses: responses}
	or := newScriptedOpenRouter(t, sc)

	_, err = or.Complete(context.Background(), Prompt{Bot: bot, Tools: NewBotToolbox(s, bot.ID, nil)})
	if err == nil {
		t.Fatal("an endless tool loop completed, want the cap to stop it")
	}
	if !strings.Contains(err.Error(), strconv.Itoa(maxToolIterations)) {
		t.Errorf("cap error %q does not name the cap %d", err, maxToolIterations)
	}
	if got := sc.requestCount(); got != maxToolIterations {
		t.Errorf("the capped loop made %d model calls, want exactly %d", got, maxToolIterations)
	}
}

// TestPromptContextOrder pins the settled context order: system prompt, then
// the memory block, then the ONE summary, then the open segment's messages.
func TestPromptContextOrder(t *testing.T) {
	msgs := promptContext(Prompt{
		Bot:      Bot{SystemPrompt: "sys"},
		Memory:   "mem",
		Summary:  "sum",
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if len(msgs) != 4 {
		t.Fatalf("context = %d turns, want 4: %+v", len(msgs), msgs)
	}
	if msgs[0].Content != "sys" {
		t.Errorf("turn 0 = %q, want the system prompt first", msgs[0].Content)
	}
	if !strings.HasPrefix(msgs[1].Content, memoryPreamble) || !strings.HasSuffix(msgs[1].Content, "mem") {
		t.Errorf("turn 1 = %q, want the framed memory block", msgs[1].Content)
	}
	if !strings.HasPrefix(msgs[2].Content, summaryPreamble) {
		t.Errorf("turn 2 = %q, want the summary block", msgs[2].Content)
	}
	if msgs[3].Role != "user" || msgs[3].Content != "hi" {
		t.Errorf("turn 3 = %+v, want the user message", msgs[3])
	}
	for i, m := range msgs[:3] {
		if m.Role != "system" {
			t.Errorf("turn %d role = %q, want system", i, m.Role)
		}
	}
}

// TestToollessRequestOmitsTools: a Prompt with no toolbox (compaction,
// summarize) sends no tools array at all, and an empty memory injects nothing.
func TestToollessRequestOmitsTools(t *testing.T) {
	sc := &scriptedUpstream{responses: []string{contentResponse("ok")}}
	or := newScriptedOpenRouter(t, sc)

	bot := Bot{DisplayName: "Ada", SystemPrompt: "You are Ada.", Model: "openrouter/deepseek/deepseek-v4"}
	if _, err := or.Complete(context.Background(), Prompt{
		Bot:      bot,
		Messages: []Message{{Role: "user", Content: "hi"}},
	}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	req := sc.request(t, 0)
	if len(req.Tools) != 0 {
		t.Errorf("tool-less prompt sent %d tools, want none", len(req.Tools))
	}
	for _, m := range req.Messages {
		if strings.Contains(m.Content, memoryPreamble) {
			t.Errorf("empty memory injected a block: %q", m.Content)
		}
	}
	if len(req.Messages) != 2 { // system prompt + user turn
		t.Errorf("request carried %d turns, want 2: %+v", len(req.Messages), req.Messages)
	}
}

// TestToolsEndpointServesTheWireTools pins GET /v1/tools' guarantee: its body
// is byte-for-byte the "tools" array a real chat request carries, because both
// are toolWireDefs() through encoding/json. If they ever diverged, the UI
// would show the user something the model is not being told.
func TestToolsEndpointServesTheWireTools(t *testing.T) {
	h := newHarness(t, &fakeLLM{reply: "ok"})
	resp, err := http.Get(h.ts.URL + "/v1/tools")
	if err != nil {
		t.Fatalf("get /v1/tools: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type = %q, want application/json", ct)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	body := bytes.TrimSuffix(raw, []byte("\n")) // writeJSON's Encoder appends one

	// The bytes the shared source marshals to (the harness server has no search
	// router, so it offers the OpenRouter fallback — the same nil the endpoint uses)...
	want, err := json.Marshal(toolWireDefs(nil))
	if err != nil {
		t.Fatalf("marshal defs: %v", err)
	}
	if !bytes.Equal(body, want) {
		t.Errorf("endpoint body = %s, want %s", body, want)
	}

	// ...and, end to end, the bytes an actual chat request put on the wire.
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
	var payload struct {
		Tools json.RawMessage `json:"tools"` // raw, so the wire bytes survive verbatim
	}
	if err := json.Unmarshal(sc.requests[0], &payload); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	if !bytes.Equal(body, []byte(payload.Tools)) {
		t.Errorf("endpoint body = %s, but the chat request sent tools = %s", body, payload.Tools)
	}
}

// TestToolsEndpointRejectsNonGET: /v1/tools is read-only derived data.
func TestToolsEndpointRejectsNonGET(t *testing.T) {
	h := newHarness(t, &fakeLLM{})
	resp, err := http.Post(h.ts.URL+"/v1/tools", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("post /v1/tools: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /v1/tools status = %d, want 405", resp.StatusCode)
	}
}
