package botnet

import (
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

// toolWireDefs renders the tool surface as the chat-completions "tools" array.
func toolWireDefs() []wireTool {
	return []wireTool{memoryToolDef()}
}

// BotToolbox binds the tool surface to one bot in one store — what runTurn
// hands the LLM so the model's tool calls execute against the right bot.
type BotToolbox struct {
	store *Store
	botID BotID
}

// NewBotToolbox builds the tool surface for one bot's turn.
func NewBotToolbox(s *Store, botID BotID) *BotToolbox {
	return &BotToolbox{store: s, botID: botID}
}

// Run executes one tool call and returns the result text handed back to the
// model. A malformed call returns an instructive "error: ..." RESULT (nil
// error), so the model can self-correct on the next iteration; a returned
// error is a real store failure and fails the whole turn — the message
// strands as failed with the reason, retryable like any other failed turn.
func (tb *BotToolbox) Run(name string, args json.RawMessage) (string, error) {
	if name != memoryToolName {
		return fmt.Sprintf("error: unknown tool '%s' — the only tool is 'memory'", name), nil
	}
	var in struct {
		Command string  `json:"command"`
		Content *string `json:"content"` // pointer: absent and "" are different answers
	}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &in); err != nil {
			return `error: arguments must be a JSON object like {"command": "read"}`, nil
		}
	}
	if in.Command == "" {
		return fmt.Sprintf("error: missing 'command' — valid: %s", strings.Join(commandNames(), ", ")), nil
	}
	for _, c := range memoryCommands {
		if c.name != in.Command {
			continue
		}
		if c.needsContent && in.Content == nil {
			return fmt.Sprintf("error: '%s' requires a 'content' field", c.name), nil
		}
		if !c.needsContent && in.Content != nil {
			return fmt.Sprintf("error: '%s' takes no 'content' field — use 'replace' to overwrite your memory", c.name), nil
		}
		var content string
		if in.Content != nil {
			content = *in.Content
		}
		return c.run(tb.store, tb.botID, content)
	}
	return fmt.Sprintf("error: unknown command '%s' — valid: %s", in.Command, strings.Join(commandNames(), ", ")), nil
}
