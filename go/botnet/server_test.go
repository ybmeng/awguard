package botnet

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	modelselector "stdtools/go/lib/modelSelector"
)

// fakeLLM answers with a canned reply and records every Prompt it was handed, so
// what actually reaches the model — one summary, the open segment's messages and
// nothing else — is checkable with no network.
//
// It is driven from the test goroutine while the server calls it from background
// turn goroutines, so every field is guarded. A gate lets a test hold a model
// call open and inspect the world mid-flight, which is the only way to observe
// the state async send exists to create.
type fakeLLM struct {
	mu         sync.Mutex
	reply      string
	citations  []Citation
	failErr    error
	gate       chan struct{}
	prompts    []Prompt
	summarized []summarizeCall
}

// summarizeCall records what compaction folded together: exactly one previous
// summary plus one segment's raw messages.
type summarizeCall struct {
	previous string
	msgs     []Message
}

func (f *fakeLLM) Complete(ctx context.Context, p Prompt) (Completion, error) {
	f.mu.Lock()
	f.prompts = append(f.prompts, p)
	gate, failErr, reply, citations := f.gate, f.failErr, f.reply, f.citations
	f.mu.Unlock()

	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return Completion{}, ctx.Err()
		}
	}
	if failErr != nil {
		return Completion{}, failErr
	}
	return Completion{Content: reply, Citations: citations}, nil
}

func (f *fakeLLM) setCitations(c []Citation) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.citations = c
}

// Summarize folds visibly, so a later assertion can see that the newest summary
// still covers the oldest segment's content.
func (f *fakeLLM) Summarize(_ context.Context, _ Bot, previous string, msgs []Message) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.summarized = append(f.summarized, summarizeCall{previous: previous, msgs: msgs})
	parts := []string{}
	if previous != "" {
		parts = append(parts, previous)
	}
	for _, m := range msgs {
		parts = append(parts, m.Content)
	}
	return "covers: " + strings.Join(parts, " | "), nil
}

// hold makes every subsequent model call block until release is called, so a
// test can observe a turn while it is genuinely in flight.
func (f *fakeLLM) hold() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.gate == nil {
		f.gate = make(chan struct{})
	}
}

func (f *fakeLLM) release() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.gate != nil {
		close(f.gate)
		f.gate = nil
	}
}

func (f *fakeLLM) setFail(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failErr = err
}

func (f *fakeLLM) setReply(s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reply = s
}

func (f *fakeLLM) lastPrompt(t *testing.T) Prompt {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.prompts) == 0 {
		t.Fatal("the model was never called")
	}
	return f.prompts[len(f.prompts)-1]
}

func (f *fakeLLM) promptCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.prompts)
}

// waitForCall blocks until the model has been called n times. A send returns
// 202 BEFORE the background turn reaches the model — that is the whole design —
// so a test holding a call open has to wait for it to start rather than assume
// it already has.
func (f *fakeLLM) waitForCall(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if f.promptCount() >= n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("the model was called %d times within the deadline, want %d", f.promptCount(), n)
}

func (f *fakeLLM) summarizeCalls() []summarizeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]summarizeCall(nil), f.summarized...)
}

// harness is a live server over a store, plus the fake it talks to.
type harness struct {
	ts    *httptest.Server
	srv   *Server
	store *Store
	llm   *fakeLLM
}

func (h *harness) bot(id BotID, suffix string) string {
	return h.ts.URL + "/v1/bots/" + string(id) + suffix
}

// settle waits for every background turn to finish. Sends are asynchronous, so a
// test that cares about the outcome rather than the asynchrony calls this before
// asserting.
func (h *harness) settle() { h.srv.Wait() }

func newHarness(t *testing.T, llm *fakeLLM) *harness {
	t.Helper()
	store, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return newHarnessOver(t, store, llm)
}

func newHarnessOver(t *testing.T, store *Store, llm *fakeLLM) *harness {
	t.Helper()
	srv, err := NewServer(store, llm)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	// Cleanups run last-registered-first, so this order gives: release any held
	// model call, wait for the background turns, stop the server, close the
	// store. Closing the store while a turn is still writing would be a race.
	t.Cleanup(func() { store.Close() })
	t.Cleanup(ts.Close)
	t.Cleanup(func() { llm.release(); srv.Wait() })
	return &harness{ts: ts, srv: srv, store: store, llm: llm}
}

func createBot(t *testing.T, h *harness, name string) Bot {
	t.Helper()
	var bot Bot
	post(t, h.ts.URL+"/v1/bots",
		`{"displayName":"`+name+`","systemPrompt":"You are `+name+`.","model":"`+
			string(modelselector.DeepSeekV4.ID)+`"}`, &bot)
	return bot
}

// send posts one message and returns the persisted user turn, asserting the 202
// that says the server did not wait on the model.
func send(t *testing.T, h *harness, botID BotID, jsonContent string) Message {
	t.Helper()
	var msg Message
	postExpect(t, http.StatusAccepted, h.bot(botID, "/messages"), `{"content":`+jsonContent+`}`, &msg)
	return msg
}

// sendAndSettle sends, waits for the reply, and returns the whole transcript.
func sendAndSettle(t *testing.T, h *harness, botID BotID, jsonContent string) []Message {
	t.Helper()
	send(t, h, botID, jsonContent)
	h.settle()
	var conv []Message
	get(t, h.bot(botID, "/messages"), &conv)
	return conv
}

// TestServerChatRoundTrip drives the API the UI uses: create a bot, send a
// message, and get back a conversation with the model reply appended — all
// state living in the server.
func TestServerChatRoundTrip(t *testing.T) {
	llm := &fakeLLM{reply: "hi, I'm Ada"}
	h := newHarness(t, llm)

	bot := createBot(t, h, "Ada")
	if bot.ID == "" || !strings.HasPrefix(string(bot.ID), "bot_") {
		t.Fatalf("bad bot id: %q", bot.ID)
	}

	// it shows up in the list
	var bots []Bot
	get(t, h.ts.URL+"/v1/bots", &bots)
	if len(bots) != 1 || bots[0].ID != bot.ID {
		t.Fatalf("list bots = %+v, want the created bot", bots)
	}

	// the send returns the user's own turn immediately, already persisted
	sent := send(t, h, bot.ID, `"hello"`)
	if sent.Role != "user" || sent.Content != "hello" {
		t.Errorf("202 body = %+v, want the user's turn", sent)
	}
	if sent.Status != StatusAwaiting {
		t.Errorf("202 body status = %q, want %q", sent.Status, StatusAwaiting)
	}
	if !strings.HasPrefix(sent.ID, "msg_") || sent.SegmentID == "" {
		t.Errorf("202 body = %+v, want an id and a segment", sent)
	}

	h.settle()
	var conv []Message
	get(t, h.bot(bot.ID, "/messages"), &conv)
	if len(conv) != 2 {
		t.Fatalf("conversation = %d messages, want 2", len(conv))
	}
	if conv[0].ID != sent.ID || conv[0].Role != "user" || conv[0].Content != "hello" {
		t.Errorf("msg 0 = %+v, want the turn the send returned", conv[0])
	}
	if conv[1].Role != "bot" || conv[1].Content != "hi, I'm Ada" {
		t.Errorf("msg 1 = %+v, want bot/reply", conv[1])
	}
	for i, m := range conv {
		if m.Status != StatusSent {
			t.Errorf("msg %d status = %q, want %q once the turn settled", i, m.Status, StatusSent)
		}
	}

	// the model was handed the user message and no summary — this bot has never
	// been compacted, so there is nothing to carry forward
	p := llm.lastPrompt(t)
	if len(p.Messages) != 1 || p.Messages[0].Content != "hello" {
		t.Errorf("llm saw %+v, want the user message", p.Messages)
	}
	if p.Summary != "" {
		t.Errorf("summary = %q, want empty before any compaction", p.Summary)
	}
}

// TestSendDoesNotWaitOnTheModel is the regression test for the reported bug:
// the user hit send and no text appeared, because the request blocked for the
// whole model call and their own turn existed nowhere renderable until it
// finished. The send must return promptly AND the turn must already be in the
// transcript while the model is still working.
func TestSendDoesNotWaitOnTheModel(t *testing.T) {
	llm := &fakeLLM{reply: "eventually"}
	llm.hold() // the model call will not return until this test says so
	h := newHarness(t, llm)
	bot := createBot(t, h, "Ada")

	start := time.Now()
	sent := send(t, h, bot.ID, `"are you there?"`)
	elapsed := time.Since(start)

	// The real call this replaces was measured at 8.3s; the fake here never
	// returns at all until released, so any wait would hang rather than be slow.
	if elapsed > 2*time.Second {
		t.Errorf("send took %v with the model still blocked; it must not wait on the model", elapsed)
	}

	// The whole point: the user's turn is readable NOW, mid-flight.
	var conv []Message
	get(t, h.bot(bot.ID, "/messages"), &conv)
	if len(conv) != 1 {
		t.Fatalf("transcript mid-flight = %d messages, want the user's turn to be renderable", len(conv))
	}
	if conv[0].ID != sent.ID || conv[0].Content != "are you there?" || conv[0].Status != StatusAwaiting {
		t.Errorf("mid-flight transcript = %+v, want the awaiting user turn", conv[0])
	}

	// And it is addressable by id, which is how the client watches it settle.
	var looked Message
	get(t, h.ts.URL+"/v1/messages/"+sent.ID, &looked)
	if looked.ID != sent.ID || looked.Status != StatusAwaiting {
		t.Errorf("GET by id mid-flight = %+v, want the awaiting turn", looked)
	}

	// Release the model; the reply lands and the turn settles.
	llm.release()
	h.settle()
	get(t, h.bot(bot.ID, "/messages"), &conv)
	if len(conv) != 2 || conv[1].Content != "eventually" {
		t.Fatalf("settled transcript = %+v, want the reply appended", conv)
	}
	if conv[0].Status != StatusSent {
		t.Errorf("user turn status = %q, want %q after the reply landed", conv[0].Status, StatusSent)
	}
}

// TestSecondSendWhileInFlightIsRefused pins the concurrency rule: one reply in
// flight per bot, and a second send is refused rather than queued. Refusing is
// what makes an interleaved transcript impossible — see the ordering DECISION on
// Message.
func TestSecondSendWhileInFlightIsRefused(t *testing.T) {
	llm := &fakeLLM{reply: "reply"}
	llm.hold()
	h := newHarness(t, llm)
	bot := createBot(t, h, "Ada")

	first := send(t, h, bot.ID, `"first"`)

	code, body := postRaw(t, h.bot(bot.ID, "/messages"), `{"content":"second"}`)
	if code != http.StatusConflict {
		t.Fatalf("second send = %d, want 409; body: %s", code, body)
	}
	// The refusal names the turn holding the bot, so the client polls that
	// rather than guessing what it collided with.
	var busy struct {
		Error   string  `json:"error"`
		Message Message `json:"message"`
	}
	if err := json.Unmarshal([]byte(body), &busy); err != nil {
		t.Fatalf("decode 409 body %q: %v", body, err)
	}
	if busy.Message.ID != first.ID {
		t.Errorf("409 names message %q, want the in-flight %q", busy.Message.ID, first.ID)
	}
	if busy.Error == "" {
		t.Error("409 carried no error text")
	}

	// The refused send was not persisted: no second user turn exists.
	var conv []Message
	get(t, h.bot(bot.ID, "/messages"), &conv)
	if len(conv) != 1 {
		t.Fatalf("transcript = %+v, want only the first turn — the refused send must not append", conv)
	}
	llm.waitForCall(t, 1)
	if got := llm.promptCount(); got != 1 {
		t.Errorf("the model was called %d times, want 1 — the refused send started a turn", got)
	}

	// Once the first turn settles the bot is free again, and the transcript is
	// strictly ordered: user, reply, user, reply.
	llm.release()
	h.settle()
	conv = sendAndSettle(t, h, bot.ID, `"second"`)
	want := []struct{ role, content string }{
		{"user", "first"}, {"bot", "reply"}, {"user", "second"}, {"bot", "reply"},
	}
	if len(conv) != len(want) {
		t.Fatalf("transcript = %d messages, want %d: %+v", len(conv), len(want), conv)
	}
	for i, m := range conv {
		if m.Role != want[i].role || m.Content != want[i].content {
			t.Errorf("transcript[%d] = (%q, %q), want (%q, %q)", i, m.Role, m.Content, want[i].role, want[i].content)
		}
	}
}

// TestConcurrentSendsRaceToOneAcceptance fires many sends at one bot at the same
// instant. Exactly one may be accepted — the guard has to be race-free, not just
// correct when calls happen to arrive spaced apart. A check-then-insert that was
// not inside the transaction doing the insert would let several through here.
func TestConcurrentSendsRaceToOneAcceptance(t *testing.T) {
	llm := &fakeLLM{reply: "ok"}
	llm.hold()
	h := newHarness(t, llm)
	bot := createBot(t, h, "Ada")

	const n = 12
	codes := make([]int, n)
	errs := make([]error, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // line them all up on the same instant
			// No t.Fatalf here: this is not the test goroutine.
			resp, err := http.Post(h.bot(bot.ID, "/messages"), "application/json",
				strings.NewReader(`{"content":"race"}`))
			if err != nil {
				errs[i] = err
				return
			}
			defer resp.Body.Close()
			io.Copy(io.Discard, resp.Body)
			codes[i] = resp.StatusCode
		}(i)
	}
	close(start)
	wg.Wait()

	accepted := 0
	for i, code := range codes {
		if errs[i] != nil {
			t.Fatalf("send %d: %v", i, errs[i])
		}
		switch code {
		case http.StatusAccepted:
			accepted++
		case http.StatusConflict: // refused, as it must be
		default:
			t.Errorf("send %d = %d, want 202 or 409", i, code)
		}
	}
	if accepted != 1 {
		t.Errorf("%d of %d concurrent sends were accepted, want exactly 1", accepted, n)
	}

	var conv []Message
	get(t, h.bot(bot.ID, "/messages"), &conv)
	if len(conv) != 1 {
		t.Errorf("transcript = %d messages, want 1 — a refused send appended anyway", len(conv))
	}
	llm.waitForCall(t, 1)
	if got := llm.promptCount(); got != 1 {
		t.Errorf("the model was called %d times, want 1", got)
	}
}

// TestPollForReplyAfterCursor covers how the client actually watches for a
// reply: hold the id it last saw and ask for what came after it.
func TestPollForReplyAfterCursor(t *testing.T) {
	llm := &fakeLLM{reply: "the answer"}
	llm.hold()
	h := newHarness(t, llm)
	bot := createBot(t, h, "Ada")

	sent := send(t, h, bot.ID, `"the question"`)

	// Nothing new yet while the model is still working.
	var fresh []Message
	get(t, h.bot(bot.ID, "/messages?after="+sent.ID), &fresh)
	if len(fresh) != 0 {
		t.Errorf("after the newest message = %+v, want nothing yet", fresh)
	}

	llm.release()
	h.settle()

	// Now exactly the reply, without refetching the transcript.
	get(t, h.bot(bot.ID, "/messages?after="+sent.ID), &fresh)
	if len(fresh) != 1 || fresh[0].Role != "bot" || fresh[0].Content != "the answer" {
		t.Fatalf("after the user turn = %+v, want just the reply", fresh)
	}

	// Omitting the cursor still returns everything.
	var all []Message
	get(t, h.bot(bot.ID, "/messages"), &all)
	if len(all) != 2 {
		t.Errorf("full transcript = %d messages, want 2", len(all))
	}

	// A cursor the server has never seen is a 404, so a client holding a stale
	// id learns to resync instead of polling forever for nothing.
	if code, _ := getRaw(t, h.bot(bot.ID, "/messages?after=msg_NOPE")); code != http.StatusNotFound {
		t.Errorf("unknown cursor = %d, want 404", code)
	}
	// So is a cursor belonging to a different bot.
	other := createBot(t, h, "Other")
	if code, _ := getRaw(t, h.bot(other.ID, "/messages?after="+sent.ID)); code != http.StatusNotFound {
		t.Errorf("cursor from another bot = %d, want 404", code)
	}
}

// TestGetMessageByID: lookup with no bot in the path.
func TestGetMessageByID(t *testing.T) {
	llm := &fakeLLM{reply: "ok"}
	h := newHarness(t, llm)
	bot := createBot(t, h, "Ada")
	conv := sendAndSettle(t, h, bot.ID, `"hello"`)

	for _, want := range conv {
		var got Message
		get(t, h.ts.URL+"/v1/messages/"+want.ID, &got)
		if got.ID != want.ID || got.Content != want.Content || got.BotID != bot.ID {
			t.Errorf("GET /v1/messages/%s = %+v, want %+v", want.ID, got, want)
		}
	}
	if code, _ := getRaw(t, h.ts.URL+"/v1/messages/msg_NOPE"); code != http.StatusNotFound {
		t.Errorf("unknown message = %d, want 404", code)
	}
}

// TestCumulativeSummary is the load-bearing test for compaction: after two
// compactions the model receives exactly ONE summary, and that summary still
// covers the first segment. If summaries were accumulated into a list instead
// of folded, the newest summary would not mention the oldest segment.
func TestCumulativeSummary(t *testing.T) {
	llm := &fakeLLM{reply: "ok"}
	h := newHarness(t, llm)
	bot := createBot(t, h, "Ada")
	var chain []Segment

	sendAndSettle(t, h, bot.ID, `"one"`)
	post(t, h.bot(bot.ID, "/compact"), `{}`, &chain)
	if len(chain) != 2 {
		t.Fatalf("chain after first compaction = %d segments, want 2", len(chain))
	}
	firstSummary := chain[0].Summary

	sendAndSettle(t, h, bot.ID, `"two"`)
	// The turn in segment 1 is handed the first summary — one string, not a list.
	if got := llm.lastPrompt(t).Summary; got != firstSummary {
		t.Errorf("summary sent in segment 1 = %q, want the sealed summary %q", got, firstSummary)
	}

	post(t, h.bot(bot.ID, "/compact"), `{}`, &chain)
	if len(chain) != 3 {
		t.Fatalf("chain after second compaction = %d segments, want 3", len(chain))
	}
	secondSummary := chain[1].Summary

	// The second compaction folded the FIRST summary in, rather than starting
	// fresh or being handed a pile of summaries.
	calls := llm.summarizeCalls()
	if len(calls) != 2 {
		t.Fatalf("Summarize called %d times, want 2", len(calls))
	}
	if calls[0].previous != "" {
		t.Errorf("first compaction previous = %q, want empty", calls[0].previous)
	}
	if calls[1].previous != firstSummary {
		t.Errorf("second compaction previous = %q, want the first summary %q", calls[1].previous, firstSummary)
	}
	// It was handed only segment 1's raw messages, not the whole transcript.
	for _, m := range calls[1].msgs {
		if m.Content == "one" {
			t.Error("second compaction was handed segment 0's raw messages; it should get only the summary")
		}
	}

	sendAndSettle(t, h, bot.ID, `"three"`)
	p := llm.lastPrompt(t)

	// EXACTLY ONE summary reaches the model: Prompt.Summary is a single string,
	// and it is the newest sealed one.
	if p.Summary != secondSummary {
		t.Errorf("summary sent = %q, want the newest sealed summary %q", p.Summary, secondSummary)
	}
	// And it still covers the earliest segment.
	if !strings.Contains(p.Summary, "one") {
		t.Errorf("summary %q does not cover segment 0's content — it is not cumulative", p.Summary)
	}
	// The raw messages are the OPEN segment's only: context stays constant-size.
	if len(p.Messages) != 1 || p.Messages[0].Content != "three" {
		t.Errorf("messages sent = %+v, want only the open segment's turn", p.Messages)
	}
	if chain[2].Summary != "" || !chain[2].SealedAt.IsZero() {
		t.Errorf("segment 2 = %+v, want open with no summary", chain[2])
	}
	for i, seg := range chain[:2] {
		if seg.SealedAt.IsZero() {
			t.Errorf("segment %d has no seal time", i)
		}
	}
}

// TestTranscriptSurvivesCompaction: compaction changes what the model is sent,
// never what the user can read. Every message stays, in order, across segments.
func TestTranscriptSurvivesCompaction(t *testing.T) {
	llm := &fakeLLM{reply: "ok"}
	h := newHarness(t, llm)
	bot := createBot(t, h, "Ada")
	var chain []Segment

	sendAndSettle(t, h, bot.ID, `"one"`)
	post(t, h.bot(bot.ID, "/compact"), `{}`, &chain)
	sendAndSettle(t, h, bot.ID, `"two"`)
	post(t, h.bot(bot.ID, "/compact"), `{}`, &chain)
	full := sendAndSettle(t, h, bot.ID, `"three"`)

	want := []string{"one", "ok", "two", "ok", "three", "ok"}
	if len(full) != len(want) {
		t.Fatalf("transcript = %d messages, want %d — compaction deleted messages", len(full), len(want))
	}
	for i, m := range full {
		if m.Content != want[i] {
			t.Errorf("transcript[%d] = %q, want %q", i, m.Content, want[i])
		}
	}
	// The transcript spans all three segments, oldest first.
	seen := map[SegmentID]bool{}
	for _, m := range full {
		seen[m.SegmentID] = true
	}
	if len(seen) != 3 {
		t.Errorf("transcript spans %d segments, want 3", len(seen))
	}
}

// TestCompactEmptySegmentIsNoOp: compacting with nothing to compact must not
// create an empty sealed segment, and must not call the model.
func TestCompactEmptySegmentIsNoOp(t *testing.T) {
	llm := &fakeLLM{reply: "ok"}
	h := newHarness(t, llm)
	bot := createBot(t, h, "Ada")

	// A brand-new bot: one open segment, nothing in it.
	var chain []Segment
	post(t, h.bot(bot.ID, "/compact"), `{}`, &chain)
	if len(chain) != 1 || !chain[0].IsOpen() {
		t.Fatalf("chain after compacting an empty bot = %+v, want the single open segment", chain)
	}

	// And after a real compaction, compacting again immediately is also a no-op.
	sendAndSettle(t, h, bot.ID, `"one"`)
	post(t, h.bot(bot.ID, "/compact"), `{}`, &chain)
	if len(chain) != 2 {
		t.Fatalf("chain after a real compaction = %d, want 2", len(chain))
	}
	post(t, h.bot(bot.ID, "/compact"), `{}`, &chain)
	if len(chain) != 2 {
		t.Errorf("chain after compacting the empty new segment = %d, want 2 — an empty sealed segment was created", len(chain))
	}
	if len(llm.summarizeCalls()) != 1 {
		t.Errorf("Summarize called %d times, want 1 — an empty segment must not reach the model", len(llm.summarizeCalls()))
	}
}

// TestCompactRefusedWhileReplyInFlight: sealing mid-reply would strand the
// question in the sealed segment and land its answer in the next one.
func TestCompactRefusedWhileReplyInFlight(t *testing.T) {
	llm := &fakeLLM{reply: "ok"}
	h := newHarness(t, llm)
	bot := createBot(t, h, "Ada")
	sendAndSettle(t, h, bot.ID, `"settled"`)

	llm.hold()
	send(t, h, bot.ID, `"in flight"`)
	if code, body := postRaw(t, h.bot(bot.ID, "/compact"), `{}`); code != http.StatusConflict {
		t.Errorf("compact while a reply is in flight = %d, want 409; body: %s", code, body)
	}

	llm.release()
	h.settle()
	var chain []Segment
	post(t, h.bot(bot.ID, "/compact"), `{}`, &chain)
	if len(chain) != 2 {
		t.Errorf("compact once settled = %d segments, want 2", len(chain))
	}
}

// TestStrandedSendIsRetryable: when the model call fails, the user turn stays
// persisted and marked failed with the reason, and the send is retried against
// that message rather than the user retyping it.
func TestStrandedSendIsRetryable(t *testing.T) {
	llm := &fakeLLM{reply: "ok", failErr: fmt.Errorf("openrouter: upstream exploded")}
	h := newHarness(t, llm)
	bot := createBot(t, h, "Ada")

	sent := send(t, h, bot.ID, `"hello"`)
	h.settle()

	// The failure is recorded on the message, which is where the client finds
	// it — the send itself was accepted and long since returned.
	var stranded Message
	get(t, h.ts.URL+"/v1/messages/"+sent.ID, &stranded)
	if stranded.Status != StatusFailed {
		t.Fatalf("stranded message status = %q, want %q", stranded.Status, StatusFailed)
	}
	if !strings.Contains(stranded.Error, "upstream exploded") {
		t.Errorf("stranded message error = %q, want the model's reason", stranded.Error)
	}
	var conv []Message
	get(t, h.bot(bot.ID, "/messages"), &conv)
	if len(conv) != 1 || conv[0].ID != sent.ID {
		t.Fatalf("transcript = %+v, want the stranded user turn still there", conv)
	}

	// Retry against that message once the model recovers — no retyping.
	llm.setFail(nil)
	var retried Message
	postExpect(t, http.StatusAccepted, h.bot(bot.ID, "/messages/"+sent.ID+"/retry"), `{}`, &retried)
	if retried.ID != sent.ID {
		t.Errorf("retry returned message %q, want the stranded %q", retried.ID, sent.ID)
	}
	if retried.Status != StatusAwaiting || retried.Error != "" {
		t.Errorf("retry returned %+v, want it awaiting with the old error cleared", retried)
	}
	h.settle()

	get(t, h.bot(bot.ID, "/messages"), &conv)
	if len(conv) != 2 {
		t.Fatalf("after retry = %d messages, want the user turn plus its reply", len(conv))
	}
	if conv[0].ID != sent.ID || conv[0].Status != StatusSent || conv[0].Error != "" {
		t.Errorf("retried message = %+v, want settled with no error", conv[0])
	}
	if conv[1].Role != "bot" || conv[1].Content != "ok" {
		t.Errorf("reply = %+v, want the model's answer", conv[1])
	}

	// Retrying an already-answered message is a conflict, not a duplicate turn.
	if code, _ := postRaw(t, h.bot(bot.ID, "/messages/"+sent.ID+"/retry"), `{}`); code != http.StatusConflict {
		t.Errorf("retry of an answered message = %d, want 409", code)
	}
}

// TestUnknownModelRejectedButPersistedBotRepairable covers the live situation:
// a bot in the database holds a model id that has since left the roster. It must
// not vanish from the listing, a NEW bot must not be creatable with that id, and
// PATCH must repair the existing one.
func TestUnknownModelRejectedButPersistedBotRepairable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	seedLegacyDB(t, path)
	store, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	llm := &fakeLLM{reply: "ok"}
	h := newHarnessOver(t, store, llm)
	const stale = "bot_STALE"
	base := h.ts.URL + "/v1/bots/" + stale

	// A NEW bot cannot be created with that id.
	code, body := postRaw(t, h.ts.URL+"/v1/bots",
		`{"displayName":"Nope","model":"openrouter/deepseek/deepseek-v4"}`)
	if code != http.StatusBadRequest {
		t.Errorf("create with unknown model = %d, want 400; body: %s", code, body)
	}

	// The already-persisted one still lists, flagged for repair.
	var bots []Bot
	get(t, h.ts.URL+"/v1/bots", &bots)
	var found *Bot
	for i := range bots {
		if string(bots[i].ID) == stale {
			found = &bots[i]
		}
	}
	if found == nil {
		t.Fatalf("bot %s vanished from the listing: %+v", stale, bots)
	}
	if found.ModelValid {
		t.Errorf("bot %s modelValid = true, want false so the UI can offer a repair", stale)
	}
	if found.DisplayName != "First bot" {
		t.Errorf("display name = %q, want it intact", found.DisplayName)
	}
	// Its transcript is readable too.
	var conv []Message
	get(t, base+"/messages", &conv)
	if len(conv) != 1 || conv[0].Content != "hi there" {
		t.Errorf("transcript = %+v, want the existing message", conv)
	}

	// The send is accepted — the user's turn is persisted and renders — and the
	// background turn strands it with a message naming the repair. An unusable
	// model is bot state, not a bad request, and the user's text is never lost.
	sent := send(t, h, stale, `"hello"`)
	h.settle()
	var stranded Message
	get(t, h.ts.URL+"/v1/messages/"+sent.ID, &stranded)
	if stranded.Status != StatusFailed {
		t.Fatalf("send to a stale-model bot left status %q, want %q", stranded.Status, StatusFailed)
	}
	if !strings.Contains(stranded.Error, "PATCH /v1/bots/"+stale) {
		t.Errorf("error %q should name the repair endpoint", stranded.Error)
	}
	if llm.promptCount() != 0 {
		t.Error("a bot with an unresolvable model should not reach the model at all")
	}

	// PATCH repairs it.
	var fixed Bot
	patch(t, base, `{"model":"`+string(modelselector.GLM53Flash.ID)+`","displayName":"Repaired"}`, &fixed)
	if fixed.Model != modelselector.GLM53Flash.ID || !fixed.ModelValid {
		t.Fatalf("patched bot = %+v, want the new model and modelValid true", fixed)
	}
	if fixed.DisplayName != "Repaired" {
		t.Errorf("display name = %q, want %q", fixed.DisplayName, "Repaired")
	}
	// An omitted field is left alone.
	if fixed.SystemPrompt != "" {
		t.Errorf("system prompt = %q, want it untouched", fixed.SystemPrompt)
	}
	// PATCH cannot introduce an unknown model either.
	if code, _ := patchRaw(t, base, `{"model":"openrouter/nope/nope"}`); code != http.StatusBadRequest {
		t.Errorf("patch to an unknown model = %d, want 400", code)
	}

	// And the stranded message is now answerable without retyping.
	postExpect(t, http.StatusAccepted, base+"/messages/"+sent.ID+"/retry", `{}`, &stranded)
	h.settle()
	get(t, base+"/messages", &conv)
	if len(conv) != 3 || conv[2].Content != "ok" {
		t.Errorf("after repair and retry = %+v, want the reply appended", conv)
	}
}

// TestBotListMetadata: the sidebar draws itself from GET /v1/bots alone — no
// per-bot transcript fetch — so the row's preview, recency ordering and unread
// state all have to come back on the bot.
func TestBotListMetadata(t *testing.T) {
	llm := &fakeLLM{reply: "ok"}
	h := newHarness(t, llm)
	first := createBot(t, h, "First")
	second := createBot(t, h, "Second")

	// A never-messaged bot sorts by creation, newest first.
	var bots []Bot
	get(t, h.ts.URL+"/v1/bots", &bots)
	if len(bots) != 2 || bots[0].ID != second.ID {
		t.Fatalf("listing = %+v, want the newest bot first", bots)
	}

	// Messaging the older bot moves it to the top, with a collapsed preview.
	sendAndSettle(t, h, first.ID, `"hello   there\nfriend"`)
	get(t, h.ts.URL+"/v1/bots", &bots)
	if bots[0].ID != first.ID {
		t.Errorf("listing = %+v, want the just-messaged bot first", bots)
	}
	if bots[0].LastMessageText != "ok" {
		t.Errorf("preview = %q, want the newest message (the reply)", bots[0].LastMessageText)
	}
	if bots[0].LastMessageAt.IsZero() {
		t.Error("lastMessageAt is zero after a message")
	}
	// Unread: a message arrived and the bot has never been read.
	if !bots[0].ReadAt.Before(bots[0].LastMessageAt) {
		t.Error("bot should read as unread before POST /read")
	}

	// POST /read stamps the watermark at the newest message, not at the clock,
	// so a message landing between the client's read and this write is not
	// silently swallowed.
	var read Bot
	post(t, h.bot(first.ID, "/read"), `{}`, &read)
	if !read.ReadAt.Equal(read.LastMessageAt) {
		t.Errorf("after POST /read, readAt %v should equal lastMessageAt %v", read.ReadAt, read.LastMessageAt)
	}
	get(t, h.ts.URL+"/v1/bots", &bots)
	if bots[0].ReadAt.IsZero() {
		t.Error("readAt did not persist")
	}

	// The preview collapses whitespace rather than carrying newlines into the row.
	sendAndSettle(t, h, second.ID, `"a\n\nb"`)
	llm.setReply("multi\nline reply")
	sendAndSettle(t, h, second.ID, `"again"`)
	get(t, h.ts.URL+"/v1/bots", &bots)
	if bots[0].LastMessageText != "multi line reply" {
		t.Errorf("preview = %q, want whitespace collapsed", bots[0].LastMessageText)
	}
}

// TestOpenRouterSendsOneSummary checks the invariant on the wire, not just at
// the LLM interface: however many segments a bot has, the request body carries
// exactly one summary turn.
func TestOpenRouterSendsOneSummary(t *testing.T) {
	var got struct {
		Messages []wireMsg `json:"messages"`
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &got); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer upstream.Close()

	or := NewOpenRouter("test-key")
	or.HTTP = upstream.Client()
	// Point the client at the test upstream by rewriting the request host.
	or.HTTP.Transport = rewriteHost{upstream.URL, http.DefaultTransport}

	bot := Bot{DisplayName: "Ada", SystemPrompt: "You are Ada.", Model: modelselector.DeepSeekV4.ID}
	_, err := or.Complete(context.Background(), Prompt{
		Bot:     bot,
		Summary: "everything that happened before",
		Messages: []Message{
			{Role: "user", Content: "hello"},
			{Role: "bot", Content: "hi"},
			{Role: "system", Content: "a local note that must not be sent"},
		},
	})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}

	summaries := 0
	for _, m := range got.Messages {
		if strings.Contains(m.Content, summaryPreamble) {
			summaries++
		}
		if strings.Contains(m.Content, "must not be sent") {
			t.Error("a local system note reached the model")
		}
	}
	if summaries != 1 {
		t.Errorf("request carried %d summary turns, want exactly 1: %+v", summaries, got.Messages)
	}
	if len(got.Messages) != 4 { // system prompt, summary, user, assistant
		t.Errorf("request carried %d turns, want 4: %+v", len(got.Messages), got.Messages)
	}
}

// rewriteHost sends every request to a test upstream instead of openrouter.ai.
type rewriteHost struct {
	base string
	next http.RoundTripper
}

func (rw rewriteHost) RoundTrip(r *http.Request) (*http.Response, error) {
	u := *r.URL
	base := strings.TrimPrefix(rw.base, "http://")
	u.Host = base
	u.Scheme = "http"
	clone := r.Clone(r.Context())
	clone.URL = &u
	clone.Host = base
	return rw.next.RoundTrip(clone)
}

// ── HTTP helpers ──────────────────────────────────────────────────────────────

func post(t *testing.T, url, body string, out any) {
	t.Helper()
	postExpect(t, http.StatusOK, url, body, out)
}

func postExpect(t *testing.T, want int, url, body string, out any) {
	t.Helper()
	code, raw := postRaw(t, url, body)
	if code != want {
		t.Fatalf("POST %s: status %d, want %d: %s", url, code, want, raw)
	}
	if err := json.Unmarshal([]byte(raw), out); err != nil {
		t.Fatalf("decode %s: %v (body: %s)", url, err, raw)
	}
}

func postRaw(t *testing.T, url, body string) (int, string) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	return resp.StatusCode, string(raw)
}

func patch(t *testing.T, url, body string, out any) {
	t.Helper()
	code, raw := patchRaw(t, url, body)
	if code != http.StatusOK {
		t.Fatalf("PATCH %s: status %d: %s", url, code, raw)
	}
	if err := json.Unmarshal([]byte(raw), out); err != nil {
		t.Fatalf("decode %s: %v (body: %s)", url, err, raw)
	}
}

func patchRaw(t *testing.T, url, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPatch, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("PATCH %s: %v", url, err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PATCH %s: %v", url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

func get(t *testing.T, url string, out any) {
	t.Helper()
	code, raw := getRaw(t, url)
	if code != http.StatusOK {
		t.Fatalf("GET %s: status %d: %s", url, code, raw)
	}
	if err := json.Unmarshal([]byte(raw), out); err != nil {
		t.Fatalf("decode %s: %v (body: %s)", url, err, raw)
	}
}

func getRaw(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}
