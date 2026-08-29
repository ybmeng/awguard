package botnet

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// The model's tool surface: ONE tool named "memory", Anthropic-memory-tool
// style — a FLAT schema, a strict "command" enum plus an optional "content"
// string. No nested oneOf/discriminated unions: mid-tier models handle a flat
// schema with a prose description far better.
//
// memoryCommands below is THE registry: one entry declares a command's name,
// whether it requires content, the description line advertised to the model,
// and its executor. The enum, the tool description and the dispatch are all
// derived from the table, so a future command (append and list operations are
// planned) is one appended entry — no switch statements to keep in step.
//
// Executions are server-side, mid-turn, and unconditional (no If-Match): each
// is one atomic store write, captured into change_log by the schema's triggers
// like any other write. A malformed call — unknown command, missing content —
// answers the model with an instructive "error: ..." tool result it can
// self-correct from; that consumes a loop iteration but does not fail the
// turn. Only a real store failure fails the turn.

// maxToolIterations caps one turn's model↔tool loop. A model stuck calling
// tools forever would otherwise hold the bot's single in-flight slot until the
// turn timeout; at the cap the turn settles as failed, naming the cap.
const maxToolIterations = 8

// memoryToolName is the one tool the request advertises.
const memoryToolName = "memory"

// memoryCommand declares one command of the memory tool.
type memoryCommand struct {
	name         string
	needsContent bool   // "content" is required; its absence is an instructive error
	doc          string // the description line advertised to the model
	run          func(s *Store, botID BotID, content string) (string, error)
}

// memoryCommands is the registry — the single place a command is declared.
var memoryCommands = []memoryCommand{
	{
		name: "read",
		doc: `"read": returns your memory verbatim. Your memory is already shown in your ` +
			`context, so read is only needed to re-check it after an edit this turn. Takes no other fields.`,
		run: func(s *Store, botID BotID, _ string) (string, error) {
			bot, err := s.GetBot(botID)
			if err != nil {
				return "", err
			}
			if bot.Memory == "" {
				return "(your memory is empty)", nil
			}
			return bot.Memory, nil
		},
	},
	{
		name:         "replace",
		needsContent: true,
		doc: `"replace": requires "content" and overwrites your ENTIRE memory with it, ` +
			`so include everything worth keeping.`,
		run: func(s *Store, botID BotID, content string) (string, error) {
			if _, err := s.SetMemory(botID, content); err != nil {
				return "", err
			}
			return "memory saved", nil
		},
	},
	{
		name: "clear",
		doc:  `"clear": erases your memory entirely. Takes no other fields.`,
		run: func(s *Store, botID BotID, _ string) (string, error) {
			if _, err := s.SetMemory(botID, ""); err != nil {
				return "", err
			}
			return "memory cleared", nil
		},
	},
}

// commandNames lists the enum, in registry order.
func commandNames() []string {
	names := make([]string, len(memoryCommands))
	for i, c := range memoryCommands {
		names[i] = c.name
	}
	return names
}

// memoryToolDef renders the registry as the one wire tool definition: the
// description spells out each command's requirements in prose, and the enum
// is strict.
func memoryToolDef() wireTool {
	lines := []string{"Manage your persistent memory. Commands:"}
	for _, c := range memoryCommands {
		lines = append(lines, "- "+c.doc)
	}
	return wireTool{Type: "function", Function: wireToolFunction{
		Name:        memoryToolName,
		Description: strings.Join(lines, "\n"),
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{
					"type":        "string",
					"enum":        commandNames(),
					"description": "The operation to perform.",
				},
				"content": map[string]any{
					"type":        "string",
					"description": `The full new memory, for "replace" only. Omit for other commands.`,
				},
			},
			"required": []string{"command"},
		},
	}}
}

// webSearchToolName is OpenRouter's built-in web-search SERVER tool — the
// no-regression FALLBACK offered only when no client search backend is
// configured. It is resolved by OpenRouter server-side (the search never
// round-trips to us as a call to dispatch), so it stays outside the handler
// registry and Run never sees it.
const webSearchToolName = "openrouter:web_search"

// webSearchFuncName is botnet's own web-search FUNCTION tool — offered when the
// router has ≥1 available backend. Unlike the server tool it DOES round-trip:
// the model hands us the query, Run dispatches it to the active backend, and the
// results are recorded in the turn's ToolCall audit trail.
const webSearchFuncName = "web_search"

// webSearchServerToolDef is the fallback server-tool entry. It carries no
// parameters, so the model searches at its own discretion with OpenRouter's
// defaults.
func webSearchServerToolDef() serverTool {
	return serverTool{Type: webSearchToolName}
}

// webSearchFuncDef is the client function tool: {query, num_results?}. The
// model hands us the query; we run it and feed the results back.
func webSearchFuncDef() wireTool {
	return wireTool{Type: "function", Function: wireToolFunction{
		Name: webSearchFuncName,
		Description: "Search the web for current or external information and get back a ranked " +
			"list of results, each with a title, URL and short excerpt. Use it whenever the " +
			"answer depends on recent events or facts outside your knowledge, then cite the sources.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{
					"type":        "string",
					"description": "The search query.",
				},
				"num_results": map[string]any{
					"type":        "integer",
					"description": "How many results to return (optional; defaults to a handful).",
				},
			},
			"required": []string{"query"},
		},
	}}
}

// toolWireDefs renders the tool surface as the chat-completions "tools" array,
// gated on the search router. It always offers the memory FUNCTION tool; for
// search it offers EITHER botnet's own web_search function tool (when the router
// has a backend) OR OpenRouter's web_search SERVER tool (the fallback) — never
// both, so the model has exactly one way to search. Returning []any lets each
// entry marshal with exactly its own fields, so the server tool never leaks a
// bogus empty "function" object into the request or into /v1/tools.
func toolWireDefs(search *Router) []any {
	defs := []any{memoryToolDef()}
	if search.Available() {
		defs = append(defs, webSearchFuncDef())
	} else {
		defs = append(defs, webSearchServerToolDef())
	}
	return defs
}

// toolResult is what a tool handler produces: the text handed back to the model,
// plus — for web_search only — the backend that ran and the structured sources,
// which the loop folds into the turn's ToolCall audit record and the reply's
// aggregate citations. Memory handlers leave backend, requestID and results zero.
type toolResult struct {
	text      string
	backend   string
	requestID string // web_search only: provider request/response id, "" when none
	results   []Citation
}

// BotToolbox binds the tool surface to one bot in one store — what runTurn hands
// the LLM so the model's tool calls execute against the right bot. search is the
// backend router for web_search; a nil router means no client search backend is
// configured, in which case web_search is never offered and never dispatched
// (the OpenRouter server tool is offered instead — see toolWireDefs).
type BotToolbox struct {
	store  *Store
	botID  BotID
	search *Router
}

// NewBotToolbox builds the tool surface for one bot's turn. A nil search router
// disables the client web_search tool; the server falls back to OpenRouter's
// server tool. Tests that exercise only memory pass nil.
func NewBotToolbox(s *Store, botID BotID, search *Router) *BotToolbox {
	return &BotToolbox{store: s, botID: botID, search: search}
}

// wireDefs is the tool surface this toolbox will actually run, gated on its own
// router — so what the request advertises can never drift from what Run can
// dispatch.
func (tb *BotToolbox) wireDefs() []any { return toolWireDefs(tb.search) }

// toolHandlers is THE dispatch registry: one entry per tool the model can call
// and we resolve ourselves (memory, web_search). Replacing the old single-name
// gate with this table means a new tool is one entry, and an unknown name is a
// clean instructive error rather than a missed branch. The OpenRouter server
// tool is deliberately absent — it resolves upstream and never reaches Run.
var toolHandlers = map[string]func(*BotToolbox, context.Context, json.RawMessage) (toolResult, error){
	memoryToolName:    (*BotToolbox).runMemory,
	webSearchFuncName: (*BotToolbox).runWebSearch,
}

// Run executes one tool call and returns its result. A malformed call returns an
// instructive "error: ..." RESULT (nil error) so the model can self-correct on
// the next iteration; a returned error is a real failure and fails the whole
// turn — the message strands as failed with the reason, retryable like any other
// failed turn. ctx is threaded through because web_search makes a network call;
// the memory handler ignores it.
func (tb *BotToolbox) Run(ctx context.Context, name string, args json.RawMessage) (toolResult, error) {
	handler, ok := toolHandlers[name]
	if !ok {
		return toolResult{text: fmt.Sprintf("error: unknown tool '%s' — valid: %s", name, strings.Join(toolNames(), ", "))}, nil
	}
	return handler(tb, ctx, args)
}

// toolNames lists the dispatchable tool names, for the unknown-tool error.
func toolNames() []string {
	return []string{memoryToolName, webSearchFuncName}
}

// runMemory dispatches the memory tool through the memoryCommands registry. It
// ignores ctx — memory writes are local store operations.
func (tb *BotToolbox) runMemory(_ context.Context, args json.RawMessage) (toolResult, error) {
	var in struct {
		Command string  `json:"command"`
		Content *string `json:"content"` // pointer: absent and "" are different answers
	}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &in); err != nil {
			return toolResult{text: `error: arguments must be a JSON object like {"command": "read"}`}, nil
		}
	}
	if in.Command == "" {
		return toolResult{text: fmt.Sprintf("error: missing 'command' — valid: %s", strings.Join(commandNames(), ", "))}, nil
	}
	for _, c := range memoryCommands {
		if c.name != in.Command {
			continue
		}
		if c.needsContent && in.Content == nil {
			return toolResult{text: fmt.Sprintf("error: '%s' requires a 'content' field", c.name)}, nil
		}
		if !c.needsContent && in.Content != nil {
			return toolResult{text: fmt.Sprintf("error: '%s' takes no 'content' field — use 'replace' to overwrite your memory", c.name)}, nil
		}
		var content string
		if in.Content != nil {
			content = *in.Content
		}
		text, err := c.run(tb.store, tb.botID, content)
		return toolResult{text: text}, err
	}
	return toolResult{text: fmt.Sprintf("error: unknown command '%s' — valid: %s", in.Command, strings.Join(commandNames(), ", "))}, nil
}

// runWebSearch dispatches the web_search tool: it runs the model's query through
// the router's active backend, renders a compact text list back to the model,
// and carries the structured sources out for the audit record and the reply's
// citations. A malformed call or a backend failure is an instructive error
// RESULT — the model can answer without search rather than failing the turn —
// and the backend name is recorded either way. Only offered when a backend is
// available, so tb.search.Active() is non-nil here.
func (tb *BotToolbox) runWebSearch(ctx context.Context, args json.RawMessage) (toolResult, error) {
	var in struct {
		Query      string `json:"query"`
		NumResults int    `json:"num_results"`
	}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &in); err != nil {
			return toolResult{text: `error: arguments must be a JSON object like {"query": "..."}`}, nil
		}
	}
	if strings.TrimSpace(in.Query) == "" {
		return toolResult{text: "error: missing 'query' — provide a search query string"}, nil
	}
	backend := tb.search.Active()
	resp, err := backend.Search(ctx, in.Query, SearchOpts{NumResults: in.NumResults})
	if err != nil {
		// Fail soft: a transient search failure answers the model with an
		// instructive error it can proceed past, and the call is still audited
		// (backend named, no results) rather than failing the turn.
		return toolResult{text: fmt.Sprintf("error: web search failed: %v", err), backend: backend.Name()}, nil
	}
	cites := make([]Citation, 0, len(resp.Results))
	for _, r := range resp.Results {
		cites = append(cites, Citation{URL: r.URL, Title: r.Title, Snippet: r.Snippet})
	}
	return toolResult{
		text:      renderSearchResults(in.Query, backend.Name(), resp.Results),
		backend:   backend.Name(),
		requestID: resp.RequestID,
		results:   cites,
	}, nil
}

// renderSearchResults formats the backend's results as the compact numbered
// list fed back to the model — enough to read and cite, not a wall of text.
func renderSearchResults(query, backend string, results []SearchResult) string {
	if len(results) == 0 {
		return fmt.Sprintf("No web results found for %q (via %s).", query, backend)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Web results for %q (via %s):\n", query, backend)
	for i, r := range results {
		fmt.Fprintf(&b, "%d. %s — %s\n", i+1, r.Title, r.URL)
		if r.PublishedAt != "" {
			fmt.Fprintf(&b, "   (%s)\n", r.PublishedAt)
		}
		if r.Snippet != "" {
			fmt.Fprintf(&b, "   %s\n", r.Snippet)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}
