package botnet

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	modelselector "stdtools/go/lib/modelSelector"
)

// TestLiveWebSearch is the real-API gate: it drives the actual Complete loop
// against openrouter.ai with the configured key, forcing a search, and proves
// on the wire that (i) OpenRouter resolves web_search server-side rather than
// handing back a tool_call our loop would have to dispatch, (ii) the reply's
// annotations match the shape we decode, and (iii) Complete carries mapped
// Citations out. It is skipped unless BOTNET_LIVE is set, so the normal suite
// stays offline. Run: BOTNET_LIVE=1 go test -run TestLiveWebSearch -v.
func TestLiveWebSearch(t *testing.T) {
	if os.Getenv("BOTNET_LIVE") == "" {
		t.Skip("set BOTNET_LIVE=1 to run the live OpenRouter web_search gate")
	}
	key := os.Getenv("OPENROUTER_API_KEY")
	if key == "" {
		home, _ := os.UserHomeDir()
		data, err := os.ReadFile(filepath.Join(home, ".config", "botnet", "openrouter.txt"))
		if err != nil {
			t.Fatalf("no key: set OPENROUTER_API_KEY or seed ~/.config/botnet/openrouter.txt: %v", err)
		}
		key = strings.TrimSpace(string(data))
	}

	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	bot := newBot(t, s)
	bot.SystemPrompt = "You are a helpful research assistant. When asked about current events, use web search and cite your sources."
	bot.Model = modelselector.DeepSeekV4.ID

	tee := &teeTransport{next: http.DefaultTransport}
	or := NewOpenRouter(key)
	or.HTTP = &http.Client{Timeout: 90 * time.Second, Transport: tee}

	comp, err := or.Complete(context.Background(), Prompt{
		Bot:      bot,
		Messages: []Message{{Role: "user", Content: "What are the top technology news headlines today? Cite your sources with links."}},
		Tools:    NewBotToolbox(s, bot.ID, nil),
	})
	if err != nil {
		t.Fatalf("live Complete failed: %v", err)
	}

	// Dump every raw wire pair — this is the proof captured for the report.
	pairs := tee.snapshot()
	for i, p := range pairs {
		t.Logf("=== wire pair %d REQUEST ===\n%s", i, p.req)
		t.Logf("=== wire pair %d RESPONSE ===\n%s", i, p.resp)
	}

	// (i) Server-side resolution: no response may have carried a tool_call naming
	// web_search — if one had, our loop would have tried to dispatch it and the
	// design would change.
	for i, p := range pairs {
		var resp struct {
			Choices []struct {
				Message struct {
					ToolCalls []struct {
						Function struct {
							Name string `json:"name"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"message"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(p.resp, &resp); err != nil {
			t.Fatalf("decode response %d: %v", i, err)
		}
		for _, ch := range resp.Choices {
			for _, tc := range ch.Message.ToolCalls {
				if strings.Contains(tc.Function.Name, "web_search") || tc.Function.Name == webSearchToolName {
					t.Fatalf("OpenRouter round-tripped a web_search tool_call (%q) — the design changes; see raw pair %d", tc.Function.Name, i)
				}
			}
		}
	}

	// The first request must actually have offered the server tool.
	if len(pairs) == 0 || !strings.Contains(string(pairs[0].req), webSearchToolName) {
		t.Fatalf("first request did not offer %q: %s", webSearchToolName, firstReq(pairs))
	}

	// (ii)+(iii) Citations came out mapped. A model may occasionally decline to
	// search; log loudly rather than pass silently, but the shape check only bites
	// when it did search.
	t.Logf("answer (%d chars): %s", len(comp.Content), comp.Content)
	t.Logf("got %d citations", len(comp.Citations))
	for i, c := range comp.Citations {
		t.Logf("  [%d] url=%q title=%q snippet=%d chars idx=[%d,%d]", i, c.URL, c.Title, len(c.Snippet), c.StartIndex, c.EndIndex)
		if c.URL == "" {
			t.Errorf("citation %d has an empty url", i)
		}
		if c.Title == "" {
			t.Errorf("citation %d has an empty title (host fallback should have filled it)", i)
		}
	}
	if len(comp.Citations) == 0 {
		t.Errorf("the model returned no citations for a current-events prompt; inspect the raw dump above")
	}
}

// teeTransport records the raw request and response body of every round trip,
// so a live test can dump exactly what went over the wire.
type teeTransport struct {
	next  http.RoundTripper
	mu    sync.Mutex
	pairs []wirePair
}

type wirePair struct{ req, resp []byte }

func (tt *teeTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	var reqBody []byte
	if r.Body != nil {
		reqBody, _ = io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(reqBody))
	}
	resp, err := tt.next.RoundTrip(r)
	if err != nil {
		return resp, err
	}
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body = io.NopCloser(bytes.NewReader(respBody))
	tt.mu.Lock()
	tt.pairs = append(tt.pairs, wirePair{req: reqBody, resp: respBody})
	tt.mu.Unlock()
	return resp, nil
}

func (tt *teeTransport) snapshot() []wirePair {
	tt.mu.Lock()
	defer tt.mu.Unlock()
	return append([]wirePair(nil), tt.pairs...)
}

func firstReq(pairs []wirePair) string {
	if len(pairs) == 0 {
		return "(no requests)"
	}
	return string(pairs[0].req)
}
