package botnet

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	modelselector "stdtools/go/lib/modelSelector"
)

// fakeLLM echoes a canned reply and records the history it was handed, so the
// chat round-trip is verifiable with no network.
type fakeLLM struct {
	reply    string
	lastSeen []Message
}

func (f *fakeLLM) Complete(_ context.Context, _ Bot, history []Message) (string, error) {
	f.lastSeen = history
	return f.reply, nil
}

// TestServerChatRoundTrip drives the API the UI uses: create a bot, send a
// message, and get back a conversation with the model reply appended — all
// state living in the server.
func TestServerChatRoundTrip(t *testing.T) {
	store, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()

	llm := &fakeLLM{reply: "hi, I'm Ada"}
	srv, err := NewServer(store, llm)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// create a bot
	var bot Bot
	post(t, ts.URL+"/v1/bots",
		`{"displayName":"Ada","systemPrompt":"You are Ada.","model":"`+string(modelselector.DeepSeekV4.ID)+`"}`, &bot)
	if bot.ID == "" || !strings.HasPrefix(string(bot.ID), "bot_") {
		t.Fatalf("bad bot id: %q", bot.ID)
	}

	// it shows up in the list
	var bots []Bot
	get(t, ts.URL+"/v1/bots", &bots)
	if len(bots) != 1 || bots[0].ID != bot.ID {
		t.Fatalf("list bots = %+v, want the created bot", bots)
	}

	// send a message → server appends user msg, calls LLM, appends reply
	var conv []Message
	post(t, ts.URL+"/v1/bots/"+string(bot.ID)+"/messages", `{"content":"hello"}`, &conv)
	if len(conv) != 2 {
		t.Fatalf("conversation = %d messages, want 2", len(conv))
	}
	if conv[0].Role != "user" || conv[0].Content != "hello" {
		t.Errorf("msg 0 = %+v, want user/hello", conv[0])
	}
	if conv[1].Role != "bot" || conv[1].Content != "hi, I'm Ada" {
		t.Errorf("msg 1 = %+v, want bot/reply", conv[1])
	}

	// the LLM was handed the full history (the user message), not an empty one
	if len(llm.lastSeen) != 1 || llm.lastSeen[0].Content != "hello" {
		t.Errorf("llm saw %+v, want the user message", llm.lastSeen)
	}

	// the conversation persisted server-side
	var reread []Message
	get(t, ts.URL+"/v1/bots/"+string(bot.ID)+"/messages", &reread)
	if len(reread) != 2 {
		t.Errorf("reread = %d messages, want 2", len(reread))
	}
}

func post(t *testing.T, url, body string, out any) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST %s: status %d", url, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatalf("decode %s: %v", url, err)
	}
}

func get(t *testing.T, url string, out any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d", url, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatalf("decode %s: %v", url, err)
	}
}
