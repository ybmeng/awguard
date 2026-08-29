// Package botnet is the PrivateBotNet framework: a private network of AI
// chatbot agents.
//
// This file is the SPEC. You author the structs here; storage, sync, CRUD, and
// UI are meant to be derived from them, not hand-written. Write directives in a
// "// Userspace ... // End Userspace" block and the agent applies them and
// clears the block; DECISION marks settled calls, OPEN marks yours to settle.
package botnet

import (
	"time"

	modelselector "stdtools/go/lib/modelSelector"
)

// ── IDs ─────────────────────────────────────────────────────────────────────
// Prefixed ULID strings: sortable by creation time, generatable client-side
// with no coordinator, self-describing in logs. e.g. "bot_01J9X..." .
type BotID string // "bot_" + ULID

// ── Bot ──────────────────────────────────────────────────────────────────────
// Minimal v0: a bot is a system prompt pointed at a model. That's enough to
// talk to it.
// DECISION: chatHistory is NOT a field here — it's referenced via Message,
// keyed by BotID. Embedding would make every rename rewrite the whole
// transcript and make "load a bot" pay for megabytes you rarely read.
// DECISION: Model is a universal ID resolved by go/lib/modelSelector
// (OpenRouter routing; DeepSeek V4 and GLM 5.3 Flash to start).
// DEFERRED (tools): userspace will design tools later; nothing here yet.
type Bot struct {
	ID           BotID                 `json:"id"`
	DisplayName  string                `json:"displayName"`
	CreatedAt    time.Time             `json:"createdAt"`
	SystemPrompt string                `json:"systemPrompt"`
	Model        modelselector.ModelID `json:"model"` // e.g. modelselector.DeepSeekV4.ID
}

// ── Message ───────────────────────────────────────────────────────────────────
// Chat history, referenced by bot. The message is the sync/query unit.
// OPEN (topology): Role assumes user<->bot only. If bots talk to each OTHER,
// replace Role with explicit From/To (BotID or a "user" sentinel). That is a
// foundational change, not a bolt-on — decide before scaffolding.
type Message struct {
	ID      string    `json:"id"` // "msg_" + ULID → globally ordered
	BotID   BotID     `json:"botId"`
	Role    string    `json:"role"` // "user" | "bot" | "system"
	Content string    `json:"content"`
	SentAt  time.Time `json:"sentAt"`
}

// ── PrivateBotNet ─────────────────────────────────────────────────────────────
// Top-level owner: which bots exist. Shared resources (tools, membership) land
// here when they're designed.
// OPEN (users): single-user assumed — there is no User type and no ownership on
// the Net. Multi-user adds a User type + membership here, foundational now.
type PrivateBotNet struct {
	ID   string  `json:"id"`
	Name string  `json:"name"`
	Bots []BotID `json:"bots"`
}

// Shape: Net → Bots → Messages. Everything joins by ID reference, so each type
// is an independent storage/sync/CRUD unit.
//
// DECISION (persistence): SQLite for everything. If performance degrades,
// revisit and offload then — not before.
//
// OPEN (stack): what the derivation lever generates downstream. The chat UI is
// being sketched in ChatUI.md (left nav + right panel).
//
// FINISH CONDITION ("framework booted"): these types compile; a SQLite store
// can create a PrivateBotNet, add a Bot, append Messages, and read the
// conversation back by BotID — proven by one go test doing that round-trip.
