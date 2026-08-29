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

// LLM turns a bot plus its conversation into the next assistant reply. The
// server depends on this interface, not on OpenRouter directly, so tests inject
// a fake and the chat round-trip is verifiable offline.
type LLM interface {
	Complete(ctx context.Context, bot Bot, history []Message) (string, error)
}

// OpenRouter is the production LLM: a stdlib-only OpenRouter chat-completions
// client. No streaming, no compaction — the full history goes up each turn.
// The key can be set at runtime (via the server's config endpoint), so access
// to it is guarded.
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

func (o *OpenRouter) Complete(ctx context.Context, bot Bot, history []Message) (string, error) {
	apiKey := o.key()
	if apiKey == "" {
		return "", fmt.Errorf("openrouter: no API key configured — set it in the app's Settings")
	}
	type wireMsg struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	msgs := make([]wireMsg, 0, len(history)+1)
	if bot.SystemPrompt != "" {
		msgs = append(msgs, wireMsg{Role: "system", Content: bot.SystemPrompt})
	}
	for _, m := range history {
		switch m.Role {
		case "user":
			msgs = append(msgs, wireMsg{Role: "user", Content: m.Content})
		case "bot":
			msgs = append(msgs, wireMsg{Role: "assistant", Content: m.Content})
		case "system":
			// local status/error notes are never sent to the model
		}
	}

	body, err := json.Marshal(map[string]any{
		"model":    openRouterSlug(bot.Model),
		"messages": msgs,
	})
	if err != nil {
		return "", fmt.Errorf("openrouter: encode: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://openrouter.ai/api/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("openrouter: request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := o.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("openrouter: send: %w", err)
	}
	defer resp.Body.Close()

	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("openrouter: decode (status %d): %w", resp.StatusCode, err)
	}
	if out.Error.Message != "" {
		return "", fmt.Errorf("openrouter: %s", out.Error.Message)
	}
	if resp.StatusCode != http.StatusOK || len(out.Choices) == 0 {
		return "", fmt.Errorf("openrouter: unexpected response (status %d)", resp.StatusCode)
	}
	return out.Choices[0].Message.Content, nil
}

// openRouterSlug turns a universal ModelID ("openrouter/deepseek/deepseek-v4")
// into the slug the OpenRouter API expects ("deepseek/deepseek-v4").
func openRouterSlug(id modelselector.ModelID) string {
	return strings.TrimPrefix(string(id), "openrouter/")
}
