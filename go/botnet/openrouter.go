package botnet

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	modelselector "stdtools/go/lib/modelSelector"
)

// LLM turns an assembled Prompt into the next assistant reply, and folds a
// sealed segment into a cumulative summary. The server depends on this
// interface, not on OpenRouter directly, so tests inject a fake and both the
// chat round-trip and compaction are verifiable offline.
type LLM interface {
	// Complete answers the next turn. The prompt carries at most ONE summary;
	// that is the invariant compaction exists to preserve.
	Complete(ctx context.Context, p Prompt) (string, error)
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
type wireMsg struct {
	Role       string         `json:"role"`
	Content    string         `json:"content"`
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"` // on "tool" role results
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

// summaryPreamble introduces the one cumulative summary in the system turn. It
// is labelled so the model treats it as recalled history rather than as
// instructions from the user.
const summaryPreamble = "Summary of the conversation so far (everything before the messages that follow):\n\n"

// memoryPreamble frames the bot's editable memory blob as a system-level
// block, so the model reads it as its own durable state rather than as part of
// the user's instructions.
const memoryPreamble = "## Your memory\n\nYour persistent memory, editable with the memory tools:\n\n"

// promptContext assembles the wire messages for one turn, in the settled
// order: system prompt, memory (when non-empty), the ONE summary, then the
// open segment's raw messages.
func promptContext(p Prompt) []wireMsg {
	msgs := make([]wireMsg, 0, len(p.Messages)+3)
	if p.Bot.SystemPrompt != "" {
		msgs = append(msgs, wireMsg{Role: "system", Content: p.Bot.SystemPrompt})
	}
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

func (o *OpenRouter) Complete(ctx context.Context, p Prompt) (string, error) {
	msgs := promptContext(p)
	if p.Tools == nil {
		return o.chat(ctx, p.Bot.Model, msgs)
	}
	// The tool loop: offer the registry's tools, execute what the model calls,
	// append the results as "tool" turns, and re-call until the model answers
	// in plain content. The cap keeps a model stuck in tool calls from holding
	// the bot's in-flight slot until the turn timeout.
	tools := toolWireDefs()
	for range maxToolIterations {
		reply, err := o.chatOnce(ctx, p.Bot.Model, msgs, tools)
		if err != nil {
			return "", err
		}
		if len(reply.ToolCalls) == 0 {
			return reply.Content, nil
		}
		msgs = append(msgs, reply)
		for _, tc := range reply.ToolCalls {
			result, err := p.Tools.Run(tc.Function.Name, json.RawMessage(tc.Function.Arguments))
			if err != nil {
				return "", fmt.Errorf("tool %s: %w", tc.Function.Name, err)
			}
			msgs = append(msgs, wireMsg{Role: "tool", Content: result, ToolCallID: tc.ID})
		}
	}
	return "", fmt.Errorf("openrouter: the model was still calling tools after %d rounds; the turn was stopped at that cap", maxToolIterations)
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
func (o *OpenRouter) chatOnce(ctx context.Context, model modelselector.ModelID, msgs []wireMsg, tools []wireTool) (wireMsg, error) {
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
