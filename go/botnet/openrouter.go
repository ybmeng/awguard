package botnet

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	modelselector "stdtools/go/lib/modelSelector"
)

// LLM turns an assembled Prompt into the next assistant reply, and folds a
// sealed segment into a cumulative summary. The server depends on this
// interface, not on OpenRouter directly, so tests inject a fake and both the
// chat round-trip and compaction are verifiable offline.
type LLM interface {
	// Complete answers the next turn. The prompt carries at most ONE summary;
	// that is the invariant compaction exists to preserve. The reply carries any
	// web citations the model gathered mid-turn — see Completion.
	Complete(ctx context.Context, p Prompt) (Completion, error)
	// Summarize folds previous (the summary of everything before this segment,
	// "" the first time) together with this segment's raw messages into a single
	// summary covering everything so far.
	Summarize(ctx context.Context, bot Bot, previous string, msgs []Message) (string, error)
}

// OpenRouter is the production LLM: a stdlib-only OpenRouter chat-completions
// client. No streaming. The key can be set at runtime (via the server's config
// endpoint), so access to it is guarded.
type OpenRouter struct {
	mu     sync.RWMutex
	apiKey string
	HTTP   *http.Client
}

// NewOpenRouter returns a client with a sane request timeout.
func NewOpenRouter(apiKey string) *OpenRouter {
	return &OpenRouter{apiKey: apiKey, HTTP: &http.Client{Timeout: 120 * time.Second}}
}

// SetKey updates the API key at runtime.
func (o *OpenRouter) SetKey(k string) {
	o.mu.Lock()
	o.apiKey = k
	o.mu.Unlock()
}

// HasKey reports whether a key is configured.
func (o *OpenRouter) HasKey() bool {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.apiKey != ""
}

func (o *OpenRouter) key() string {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.apiKey
}

// wireMsg is one chat-completions message. The same shape decodes the
// assistant's reply, so a tool_calls turn appends back into the context as-is.
// Annotations decode off the reply only — the messages we build never set them.
type wireMsg struct {
	Role        string           `json:"role"`
	Content     string           `json:"content"`
	ToolCalls   []wireToolCall   `json:"tool_calls,omitempty"`
	ToolCallID  string           `json:"tool_call_id,omitempty"` // on "tool" role results
	Annotations []wireAnnotation `json:"annotations,omitempty"`
}

// wireToolCall is one function call the model requested.
type wireToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"` // JSON-encoded arguments object
	} `json:"function"`
}

// wireAnnotation is one entry of a reply's "annotations" array. OpenRouter's
// web_search server tool resolves the search itself and attaches the sources
// here (type "url_citation"), rather than handing back a tool_call we would
// dispatch — so this is where citations enter, not the tool loop.
type wireAnnotation struct {
	Type        string `json:"type"`
	URLCitation struct {
		URL        string `json:"url"`
		Title      string `json:"title"`
		Content    string `json:"content"` // an excerpt of the source
		StartIndex int    `json:"start_index"`
		EndIndex   int    `json:"end_index"`
	} `json:"url_citation"`
}

// Completion is one answered turn: the assistant's content, any web sources it
// cited, and the ordered audit trail of every tool it called. Citations is the
// aggregate of all web_search results this turn (or the fallback server tool's
// annotations); ToolCalls is the per-call record. Both are nil on the common
// turn where the model neither searched nor called a tool.
type Completion struct {
	Content   string
	Citations []Citation
	ToolCalls []ToolCall
}

// wireTool is one entry of the request's "tools" array (OpenAI function style).
type wireTool struct {
	Type     string           `json:"type"`
	Function wireToolFunction `json:"function"`
}

type wireToolFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// serverTool is a provider-resolved tool entry — OpenRouter's web_search. It
// carries only "type" (and optional parameters), no "function" object, so it
// marshals as exactly {"type":"..."} rather than dragging an empty function
// along the way a wireTool with a zero Function would.
type serverTool struct {
	Type       string         `json:"type"`
	Parameters map[string]any `json:"parameters,omitempty"`
}

// citationsFromAnnotations maps a reply's url_citation annotations to the shared
// Citation shape, preserving OpenRouter's order. A missing title falls back to
// the url host so the UI always has something to render. Non-url_citation
// annotations, if any ever appear, are skipped.
func citationsFromAnnotations(anns []wireAnnotation) []Citation {
	var out []Citation
	for _, a := range anns {
		if a.Type != "url_citation" {
			continue
		}
		c := Citation{
			URL:        a.URLCitation.URL,
			Title:      a.URLCitation.Title,
			Snippet:    a.URLCitation.Content,
			StartIndex: a.URLCitation.StartIndex,
			EndIndex:   a.URLCitation.EndIndex,
		}
		if c.Title == "" {
			c.Title = citationHost(c.URL)
		}
		out = append(out, c)
	}
	return out
}

// citationHost is the title fallback: the host of the source url, or the raw
// url if it does not parse.
func citationHost(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return u.Host
	}
	return raw
}

// summaryPreamble introduces the one cumulative summary in the system turn. It
// is labelled so the model treats it as recalled history rather than as
// instructions from the user.
const summaryPreamble = "Summary of the conversation so far (everything before the messages that follow):\n\n"

// memoryPreamble frames the bot's editable memory blob as a system-level
// block, so the model reads it as its own durable state rather than as part of
// the user's instructions.
const memoryPreamble = "## Your memory\n\nYour persistent memory, editable with the memory tools:\n\n"

// nowLine is the turn's ground truth for "when is now". It is ONE line and it
// is ALWAYS present, unlike the memory and summary blocks: a model that does
// not know today's date cannot resolve "tomorrow at 3", which makes the
// calendar tool useless and makes it guess rather than ask. Local zone with its
// offset, matching what the calendar tool's listings print, so the model never
// has to convert. The weekday is spelled out because "next Tuesday" is a far
// more common way to book something than a date is.
func nowLine(t time.Time) string {
	return "Current date and time: " + t.Format(time.RFC3339) + " (" + t.Format("Monday, 2 January 2006") + ")"
}

// promptContext assembles the wire messages for one turn, in the settled
// order: system prompt, the current date-time line, memory (when non-empty),
// the ONE summary, then the open segment's raw messages.
func promptContext(p Prompt) []wireMsg {
	msgs := make([]wireMsg, 0, len(p.Messages)+4)
	if p.Bot.SystemPrompt != "" {
		msgs = append(msgs, wireMsg{Role: "system", Content: p.Bot.SystemPrompt})
	}
	msgs = append(msgs, wireMsg{Role: "system", Content: nowLine(time.Now())})
	if p.Memory != "" {
		msgs = append(msgs, wireMsg{Role: "system", Content: memoryPreamble + p.Memory})
	}
	// Exactly one summary, however many times this bot has been compacted:
	// Prompt.Summary is a single string, so there is nowhere for a second to go.
	if p.Summary != "" {
		msgs = append(msgs, wireMsg{Role: "system", Content: summaryPreamble + p.Summary})
	}
	for _, m := range p.Messages {
		switch m.Role {
		case "user":
			msgs = append(msgs, wireMsg{Role: "user", Content: m.Content})
		case "bot":
			msgs = append(msgs, wireMsg{Role: "assistant", Content: m.Content})
		case "system":
			// local status/error notes are never sent to the model
		}
	}
	return msgs
}

func (o *OpenRouter) Complete(ctx context.Context, p Prompt) (Completion, error) {
	msgs := promptContext(p)
	if p.Tools == nil {
		// Tool-less turns are compaction/summarize: no tools offered, so no
		// web_search and no annotations to carry.
		content, err := o.chat(ctx, p.Bot.Model, msgs)
		return Completion{Content: content}, err
	}
	// The tool loop: offer the toolbox's surface (memory plus EITHER botnet's own
	// web_search function tool OR OpenRouter's web_search server tool, gated on
	// the search router), execute what the model calls, append the results as
	// "tool" turns, and re-call until the model answers in plain content. Every
	// dispatched call is recorded into the turn's audit trail; the fallback
	// server tool instead resolves upstream and returns url_citation annotations
	// on the final reply. Either way the reply's Citations is the aggregate of the
	// sources gathered. The cap keeps a model stuck in tool calls from holding the
	// bot's in-flight slot until the turn timeout.
	tools := p.Tools.wireDefs()
	var auditCalls []ToolCall
	var searchCitations []Citation
	for range maxToolIterations {
		reply, err := o.chatOnce(ctx, p.Bot.Model, msgs, tools)
		if err != nil {
			return Completion{}, err
		}
		if len(reply.ToolCalls) == 0 {
			// Aggregate citations: the client web_search results gathered this
			// turn, plus the fallback server tool's annotations (only one path is
			// ever active, so these never double up).
			citations := append(searchCitations, citationsFromAnnotations(reply.Annotations)...)
			return Completion{
				Content:   reply.Content,
				Citations: citations,
				ToolCalls: auditCalls,
			}, nil
		}
		msgs = append(msgs, reply)
		for _, tc := range reply.ToolCalls {
			res, err := p.Tools.Run(ctx, tc.Function.Name, json.RawMessage(tc.Function.Arguments))
			if err != nil {
				return Completion{}, fmt.Errorf("tool %s: %w", tc.Function.Name, err)
			}
			auditCalls = append(auditCalls, ToolCall{
				Name:      tc.Function.Name,
				Arguments: tc.Function.Arguments,
				Result:    truncateResult(res.text),
				Backend:   res.backend,
				RequestID: res.requestID,
				Results:   res.results,
				At:        time.Now().UTC(),
			})
			searchCitations = append(searchCitations, res.results...)
			// The model gets the FULL result text; only the stored audit record is
			// truncated.
			msgs = append(msgs, wireMsg{Role: "tool", Content: res.text, ToolCallID: tc.ID})
		}
	}
	return Completion{}, fmt.Errorf("openrouter: the model was still calling tools after %d rounds; the turn was stopped at that cap", maxToolIterations)
}

// maxToolResultBytes caps the tool result stored in the audit record, so a large
// search dump cannot bloat a message row. The model still receives the full
// result on the wire; only Message.ToolCalls[].Result is capped.
const maxToolResultBytes = 8 << 10 // 8 KiB

// truncateResult caps s for storage, marking where it was cut. It cuts on a byte
// boundary rounded back off any partial UTF-8 rune.
func truncateResult(s string) string {
	if len(s) <= maxToolResultBytes {
		return s
	}
	cut := maxToolResultBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "\n…[truncated]"
}

// compactInstruction asks for a replacement summary rather than an addendum —
// the output must stand alone, because it is the only memory the bot keeps of
// everything before the next segment.
const compactInstruction = `You maintain a running summary of a long conversation.

Rewrite the summary so it covers EVERYTHING so far: the previous summary and the new messages, folded into one. Do not append a section and do not refer to "the previous summary"; produce a single self-contained summary that replaces it.

Keep what the conversation would be worse off forgetting: decisions and their reasons, facts about the user, open questions, commitments, names and identifiers. Drop pleasantries and anything already superseded. Write prose or terse bullets, no preamble, no sign-off.`

func (o *OpenRouter) Summarize(ctx context.Context, bot Bot, previous string, msgs []Message) (string, error) {
	var b strings.Builder
	if previous != "" {
		b.WriteString("PREVIOUS SUMMARY:\n")
		b.WriteString(previous)
		b.WriteString("\n\n")
	}
	b.WriteString("NEW MESSAGES:\n")
	for _, m := range msgs {
		switch m.Role {
		case "user":
			b.WriteString("User: ")
		case "bot":
			b.WriteString("Assistant: ")
		default:
			continue
		}
		b.WriteString(m.Content)
		b.WriteString("\n\n")
	}
	return o.chat(ctx, bot.Model, []wireMsg{
		{Role: "system", Content: compactInstruction},
		{Role: "user", Content: b.String()},
	})
}

// chat is the tool-less call: one request, plain content back. Summarize and
// tool-less turns use it.
func (o *OpenRouter) chat(ctx context.Context, model modelselector.ModelID, msgs []wireMsg) (string, error) {
	reply, err := o.chatOnce(ctx, model, msgs, nil)
	if err != nil {
		return "", err
	}
	return reply.Content, nil
}

// chatOnce makes one chat-completions request and returns the assistant's
// message, tool calls included.
func (o *OpenRouter) chatOnce(ctx context.Context, model modelselector.ModelID, msgs []wireMsg, tools []any) (wireMsg, error) {
	apiKey := o.key()
	if apiKey == "" {
		return wireMsg{}, fmt.Errorf("openrouter: no API key configured — set it in the app's Settings")
	}
	payload := map[string]any{
		"model":    openRouterSlug(model),
		"messages": msgs,
	}
	if len(tools) > 0 {
		payload["tools"] = tools
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return wireMsg{}, fmt.Errorf("openrouter: encode: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://openrouter.ai/api/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return wireMsg{}, fmt.Errorf("openrouter: request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := o.HTTP.Do(req)
	if err != nil {
		return wireMsg{}, fmt.Errorf("openrouter: send: %w", err)
	}
	defer resp.Body.Close()

	var out struct {
		Choices []struct {
			Message wireMsg `json:"message"`
		} `json:"choices"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return wireMsg{}, fmt.Errorf("openrouter: decode (status %d): %w", resp.StatusCode, err)
	}
	if out.Error.Message != "" {
		return wireMsg{}, fmt.Errorf("openrouter: %s", out.Error.Message)
	}
	if resp.StatusCode != http.StatusOK || len(out.Choices) == 0 {
		return wireMsg{}, fmt.Errorf("openrouter: unexpected response (status %d)", resp.StatusCode)
	}
	return out.Choices[0].Message, nil
}

// openRouterSlug turns a universal ModelID ("openrouter/deepseek/deepseek-v4")
// into the slug the OpenRouter API expects ("deepseek/deepseek-v4").
func openRouterSlug(id modelselector.ModelID) string {
	return strings.TrimPrefix(string(id), "openrouter/")
}
