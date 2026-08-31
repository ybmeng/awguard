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
type BotID string      // "bot_" + ULID
type SegmentID string  // "seg_" + ULID
type EventID string    // "evt_" + ULID
type CalendarID string // "cal_" + ULID

// ── Bot ──────────────────────────────────────────────────────────────────────
// Minimal v0: a bot is a system prompt pointed at a model. That's enough to
// talk to it.
// DECISION (thread shape): one bot IS one continuous conversation, permanently.
// A bot does not hold several threads, there is no thread picker, and the
// sidebar row is the bot. What keeps that single thread bounded is compaction
// (see Segment), not partitioning.
// DECISION: chatHistory is NOT a field here — it's referenced via Message,
// keyed by BotID. Embedding would make every rename rewrite the whole
// transcript and make "load a bot" pay for megabytes you rarely read.
// DECISION: Model is a universal ID resolved by go/lib/modelSelector
// (OpenRouter routing; DeepSeek V4 and GLM 5.3 Flash to start). Unknown IDs are
// rejected on write (create, patch) and never on read, so a bot persisted with
// a model that has since left the roster still lists and is repairable via
// PATCH rather than being unreadable. ModelValid reports that condition.
// DECISION: the open segment is NOT denormalized here — it is derived as the
// bot's segment with a zero SealedAt.
// DECISION (tools): the model's tool surface is ONE tool named "memory",
// Anthropic-memory-tool style, declared by the command registry in tools.go.
// The schema is FLAT — a strict "command" enum (read, replace, clear) plus an
// optional "content" string, no nested unions — because mid-tier models handle
// a flat schema with a prose description far better. The enum, description and
// dispatch all derive from the registry, so a future command (append and list
// operations are planned) is one appended entry. A malformed call gets an
// instructive tool-result error to self-correct from rather than failing the
// turn. Executions run server-side mid-turn as plain store writes, no If-Match.
// DECISION (that shape generalized, and the surface grew): the command-registry
// pattern above is now how EVERY stateful tool is declared, and "memory" is one
// of several. web_search came first (see Message.Citations), and "calendar" —
// the Event service below — is the second: same flat schema, same command enum
// derived from its own registry, same instructive tool-result errors. Whatever
// the surface, it is a table of commands, never a nested union.
type Bot struct {
	ID           BotID                 `json:"id"`
	DisplayName  string                `json:"displayName"`
	CreatedAt    time.Time             `json:"createdAt"`
	SystemPrompt string                `json:"systemPrompt"`
	Model        modelselector.ModelID `json:"model"` // e.g. modelselector.DeepSeekV4.ID

	// Memory is the bot's durable, editable memory blob — the first piece of
	// "what survives outside the transcript" (see the OPEN on Segment). Both
	// sides write it: the user via PATCH /v1/bots/{id} (a "memory" field; ""
	// is a valid value and clears it), and the model via the "memory" tool,
	// executed server-side during a turn. When non-empty it is injected into
	// every turn's context as a system-level "## Your memory" block.
	// DECISION: Memory is NOT part of the derived Version hash. The model
	// writes memory during chat, and chat traffic must never 412 a user's
	// in-flight prompt edit — the same reason list metadata is excluded.
	// DECISION: the tool's "replace" command overwrites the whole blob.
	// Sub-string patching is a possible later refinement, not built in v0.
	// OPEN (memory conflicts): user-vs-model memory writes are last-write-wins.
	// Per-field If-Match, or a separate memory version, could layer on later
	// if lost memory edits turn out to matter in practice.
	Memory string `json:"memory"`

	// List metadata, denormalized so the sidebar draws a row per bot without
	// fetching every conversation. Maintained on append; never authored.
	LastMessageAt   time.Time `json:"lastMessageAt"`   // zero until the first message
	LastMessageText string    `json:"lastMessageText"` // preview, whitespace collapsed
	// ReadAt is a watermark, not a timestamp of the act of reading: the bot is
	// unread when LastMessageAt is after it, and marking it read copies
	// LastMessageAt rather than the clock, so a message arriving mid-read cannot
	// be swallowed. Upgrading an existing database stamps every bot read, since
	// an upgrade is not new activity.
	ReadAt time.Time `json:"readAt"`

	// ModelValid is derived at read time, not stored: whether Model still
	// resolves in the modelSelector roster. False means "repair me with PATCH".
	ModelValid bool `json:"modelValid"`

	// Version is derived at read time, not stored: an opaque hash of the
	// AUTHORED fields (DisplayName, SystemPrompt, Model). A PATCH carrying it
	// in If-Match fails with 412 when the bot was edited in between, so two
	// clients editing a system prompt conflict instead of silently
	// last-write-winning. Message traffic never moves it — list metadata is
	// maintained, not authored — so chat cannot spuriously block an edit.
	Version string `json:"version"`
}

// ── Segment ──────────────────────────────────────────────────────────────────
// The one conversation is stored as a chain of segments. Exactly one segment is
// open at a time (zero SealedAt) and new messages append to it. Compaction seals
// the open segment and opens a fresh empty one.
//
// The summary is CUMULATIVE, and that is the load-bearing detail. Sealing
// segment N folds the previous segment's Summary together with segment N's raw
// messages into one new summary covering everything so far. The model context is
// therefore SystemPrompt + the newest sealed Summary + the open segment's raw
// messages — exactly one summary, never a growing pile of them, so context stays
// constant-size however many times a bot is compacted. Older summaries are
// retained as history for the UI and never sent to the model.
//
// Compaction never deletes messages: sealed segments keep their rows and stay
// fully readable in the transcript, they just stop being sent to the model.
// Compacting an open segment with no messages is a no-op — no empty sealed
// segment is created.
//
// DECISION (compaction trigger): manual only. A Compact button in the details
// panel, no automatic threshold, so nothing fires behind the user's back. An
// automatic trigger can be layered on later without changing this shape, since
// it would just call the same endpoint.
//
// OPEN (editable summaries): a cumulative summary is the bot's whole memory of
// everything before the open segment, so a bad one poisons it permanently with
// no way back. Decide whether Summary is user-editable and whether a seal can be
// undone before building the panel.
//
// OPEN (what survives): this keeps only a text Summary. The intent is that
// durable state lives outside the transcript entirely — memory, skills, files in
// std_artifacts — with the transcript as scratchpad. Bot.Memory is the first
// piece of that layer to exist; skills and files are still undesigned, and
// Summary remains the interim for everything else.
type Segment struct {
	ID       SegmentID `json:"id"`
	BotID    BotID     `json:"botId"`
	Index    int       `json:"index"` // 0-based position in the chain
	OpenedAt time.Time `json:"openedAt"`
	SealedAt time.Time `json:"sealedAt"` // zero while open
	Summary  string    `json:"summary"`  // set when sealed; "" while open
}

// IsOpen reports whether this is the segment new messages append to.
func (s Segment) IsOpen() bool { return s.SealedAt.IsZero() }

// ── Message ───────────────────────────────────────────────────────────────────
// Chat history, referenced by bot. The message is the sync/query unit.
//
// DECISION (identity): ids stay "msg_" + ULID rather than becoming UUIDv4. They
// are already unique, so what a swap would actually buy is nothing, and ULID
// additionally sorts by creation time. Note that the transcript's order comes
// from the rowid, not the id — insertion order is the stronger guarantee, since
// two ULIDs minted in the same millisecond are ordered by their random half
// rather than by which was written first. What the sortable id buys is that a
// client can order and diff messages locally, without a round trip, which
// optimistic rendering needs. Lookup by id was the part genuinely missing, and
// that is what got added.
//
// DECISION (ordering under async): a reply can never be appended after a later
// user turn, so no explicit reply-to reference is needed and Message keeps the
// shape below. Three things make it impossible rather than merely unlikely:
// at most one awaiting message per bot is a storage invariant (a partial unique
// index, not a convention); a send arriving while one is in flight is refused
// rather than queued, so no second user turn can be interleaved; and the reply
// is appended in the same transaction that settles the user turn, so the bot is
// never observably free with its reply still missing. Queueing instead of
// refusing would have broken this AND fed the model a prompt in which the second
// user turn preceded the reply to the first.
//
// OPEN (topology): Role assumes user<->bot only. If bots talk to each OTHER,
// replace Role with explicit From/To (BotID or a "user" sentinel). That is a
// foundational change, not a bolt-on — decide before scaffolding.
type Message struct {
	ID        string        `json:"id"` // "msg_" + ULID → sortable by creation time
	BotID     BotID         `json:"botId"`
	SegmentID SegmentID     `json:"segmentId"`
	Role      string        `json:"role"` // "user" | "bot" | "system"
	Content   string        `json:"content"`
	SentAt    time.Time     `json:"sentAt"`
	Status    MessageStatus `json:"status"`
	Error     string        `json:"error"` // set only when Status is StatusFailed

	// Citations are the web sources a bot reply drew on. They exist only on bot
	// replies that searched — the common turn has none.
	// DECISION (search is now a CLIENT tool, with the server tool as fallback):
	// we first shipped search as OpenRouter's fused `openrouter:web_search` server
	// tool — OpenRouter ran the search itself and returned only url_citation
	// annotations, never the query or a tool-call record. To get a full audit
	// trail (the model's actual queries and results as real, recorded tool calls)
	// and swappable backends, botnet now OWNS the search: it offers the model its
	// own `web_search` FUNCTION tool, runs the query through a backend router
	// (Exa/Brave/Tavily — see search.go), feeds the results back, and records
	// every step (see ToolCalls below). The old server tool remains the
	// no-regression FALLBACK: when no client backend key is configured the request
	// keeps offering `openrouter:web_search` and citations still enter via
	// annotations. Whichever path ran, Citations holds the AGGREGATE of the
	// sources so the shipped "Sources (N)" UI is unchanged.
	// DECISION (persist as a JSON column, omit when empty): Citations ride in the
	// same INSERT as the message — no new write path, so the change_log triggers
	// capture the reply exactly as before. The JSON key is omitted entirely when
	// there are no citations (the common case), which is the shape the client
	// decodes as absent/nil.
	Citations []Citation `json:"citations,omitempty"`

	// ToolCalls is the ordered audit trail of every tool the model invoked to
	// produce this reply — web_search and memory alike — the thing the fused
	// server tool could never give us. It exists only on bot replies that called
	// a tool; the common turn has none.
	// DECISION (aggregate vs. per-call): Citations above stays the flat aggregate
	// of all web_search sources this turn (the Sources UI decodes it unchanged);
	// ToolCalls is the additive, per-call record the new tool-call surface decodes.
	// A web_search entry carries its Backend and structured Results; a memory
	// entry carries only its Result text. Arguments is the raw JSON the model
	// sent, so the query is recoverable verbatim.
	// DECISION (persist as a JSON column, omit when empty): like Citations it
	// rides the message's own INSERT — no new write path, so change_log capture is
	// unchanged — and the key is omitted entirely on the common no-tool turn.
	// DECISION (truncate the stored Result): the Result fed back to the model is
	// stored capped (maxToolResultBytes) so a large search dump cannot bloat the
	// row; the structured Results carry the real sources anyway.
	ToolCalls []ToolCall `json:"toolCalls,omitempty"`
}

// ToolCall is one tool invocation recorded on a bot reply — the audit record the
// UI decodes. Name and Arguments are what the model sent; Result is the text fed
// back to it (truncated for storage). Backend, RequestID and Results are set for
// web_search only: which backend ran the query, the provider's request/response
// id for debugging, and the normalized sources. At is when it ran.
//
// DECISION (requestId rides the tool_calls JSON column): it is one more field on
// this struct, so it persists and re-serves through the EXISTING tool_calls
// column with no new DB column and no new write path — the change_log oracle is
// untouched. omitempty because memory calls and providers that expose no id
// leave it "".
type ToolCall struct {
	Name      string     `json:"name"`                // "web_search" | "memory"
	Arguments string     `json:"arguments"`           // raw JSON args the model sent
	Result    string     `json:"result"`              // string result fed back (may be truncated)
	Backend   string     `json:"backend,omitempty"`   // web_search only: the backend that ran it
	RequestID string     `json:"requestId,omitempty"` // web_search only: provider request/response id, "" when none
	Results   []Citation `json:"results,omitempty"`   // web_search only: its structured sources
	At        time.Time  `json:"at"`                  // when it ran
}

// Citation is one web source behind a bot's reply — the shared shape the UI
// decodes. url and title are always set (title falls back to the url host when
// the source has none); snippet and the index pair are optional. The indices
// point into the reply Content, for a later refinement that anchors inline
// superscripts; v1 carries them but renders only the sources row.
type Citation struct {
	URL        string `json:"url"`
	Title      string `json:"title"`
	Snippet    string `json:"snippet,omitempty"`
	StartIndex int    `json:"startIndex,omitempty"`
	EndIndex   int    `json:"endIndex,omitempty"`
}

// MessageStatus distinguishes a user turn still awaiting a reply from one whose
// model call failed, so a send in flight is visible and a stranded one is
// retryable instead of the two looking identical.
//
// Sending is asynchronous: the request persists the user turn as StatusAwaiting
// and returns immediately, and the model call runs in the background. Awaiting
// is therefore a real, observable state that outlives a request, which is what
// makes both of the invariants above necessary — and what makes a process death
// mid-reply something the store has to recover from, since the goroutine that
// would have settled the message dies with it.
type MessageStatus string

const (
	StatusSent     MessageStatus = "sent"     // settled: a reply followed, or this is one
	StatusAwaiting MessageStatus = "awaiting" // user turn, model call in flight
	StatusFailed   MessageStatus = "failed"   // user turn, model call failed; Error is set
)

// ── Calendar ──────────────────────────────────────────────────────────────────
// A named calendar: events are partitioned into calendars ("Personal", "Company
// Earnings", "Financial Updates") so the user and the bots can keep booked
// lunches apart from fed announcements. A Calendar is a service entity like
// Event — owned by the net, written by both the REST path and the calendar
// tool, and a fifth trigger-captured sync citizen.
//
// DECISION (no If-Match): calendars are last-write-wins, the same call already
// made for Event and Bot.Memory. A calendar has two authored fields — a name
// and a color — edited by one user and the odd bot; version-checking that would
// be ceremony with no contention to protect against. There is no derived
// Version field, so there is nothing for a client to send.
//
// DECISION (the default calendar is an ENSURE, not a flag): there is no
// isDefault column and no reserved id. When a write needs a calendar and none
// was named — a REST create without calendarId, a tool create without a
// calendar arg, the migration backfill — the server uses the calendar NAMED
// "Personal" (case-insensitive), creating it (color "blue", createdBy "user")
// if missing. A flag would be one more piece of state to sync, to migrate and
// to fight over; a name-keyed ensure is idempotent and self-healing — deleting
// "Personal" is legal, and it simply comes back on the next unqualified write.
// GET /v1/calendars deliberately does NOT ensure: an empty list is a valid
// answer, not a prompt to create state on a read.
//
// DECISION (delete is asymmetric by writer): REST DELETE /v1/calendars/{id}
// CASCADES — it deletes the calendar's events in the same transaction, as
// explicit per-row event DELETEs so the chg_event_* triggers hand sync clients
// real tombstones — because the UI confirms with the user first ("Delete
// calendar and its N events"). The tool's delete_calendar REFUSES a non-empty
// calendar with an instructive error naming the event count: a cheap model must
// not be able to wipe a calendar in one call, so the destructive form is
// UI-only.
type Calendar struct {
	ID    CalendarID `json:"id"`
	Name  string     `json:"name"`  // trimmed, non-empty, <= 64 chars, unique case-insensitively
	Color string     `json:"color"` // one of calendarColors; omitted on create → assigned by cycling

	// CreatedBy follows Event.CreatedBy exactly: a BotID for a tool write, the
	// "user" sentinel for a REST one, stamped by the write path and never by
	// the caller.
	CreatedBy string `json:"createdBy"`

	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// ── Event ─────────────────────────────────────────────────────────────────────
// The calendar: the first SERVICE, as opposed to a property of a bot. An Event
// is owned by the net rather than by any one bot, because the point of it is
// that the user and every bot look at the SAME calendar — a bot books a lunch,
// the user sees it in the Calendar panel, another bot lists it next turn.
//
// DECISION (native, not an integration): events live in the server's own SQLite
// beside bots and messages. That makes the calendar a fourth trigger-captured
// table and therefore a first-class sync citizen for free; an external provider
// (CalDAV, Google) would be a syncing peer of this table later, not a
// replacement for it.
//
// DECISION (no If-Match): events are NOT version-checked the way a bot's
// authored fields are. Calendar edits are low-contention — one user and a
// handful of bots, rarely on the same event in the same second — so a PATCH is
// last-write-wins, matching the call already made for Bot.Memory. There is no
// derived Version field here at all, so there is nothing for a client to send.
//
// DECISION (CreatedBy is a BotID or the "user" sentinel): a plain string, not a
// BotID, because the user is not a bot and inventing a bot row for them would
// leak into the sidebar. The UI reads it as "a bot if it resolves in the bot
// list, otherwise you". This is the same shape Message.Role already uses to
// mean user-vs-bot, and it inherits the same OPEN (topology): if bots ever
// address each other, both places want one explicit actor type.
//
// DECISION (storage format): times are stored as FIXED-WIDTH RFC3339 UTC to the
// second — not the RFC3339Nano the other tables use. ListEvents' overlap filter
// is a string comparison in SQL, and RFC3339Nano drops trailing zeros from the
// fractional part, so "12:00:00Z" would sort after "12:00:00.5Z". Fixed width
// makes lexicographic order agree with chronological order by construction, and
// second precision is all a calendar means.
//
// OPEN (recurrence, all-day, invitees, reminders): v0 is a flat interval with a
// title. Recurrence in particular is NOT a bolt-on — an RRULE turns one row into
// an expansion at read time, which changes what ListEvents even is — so decide
// it before the calendar has data worth migrating.
type Event struct {
	ID EventID `json:"id"`

	// CalendarID names the calendar this event belongs to. Always present and
	// always resolvable from a current server: a write that names no calendar
	// gets the Personal ensure (see Calendar), and one that names an unknown
	// calendar is rejected, so no event row can dangle.
	CalendarID CalendarID `json:"calendarId"`

	Title    string    `json:"title"`
	StartsAt time.Time `json:"startsAt"`
	EndsAt   time.Time `json:"endsAt"` // must not precede StartsAt
	Location string    `json:"location,omitempty"`
	Notes    string    `json:"notes,omitempty"`

	// CreatedBy is the BotID that created the event, or "user" for one created
	// in the UI. It is set by the write path, never by the caller: a tool write
	// stamps the calling bot, the REST write stamps "user", so an event can
	// never claim an author it did not have.
	CreatedBy string `json:"createdBy"`

	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// userAuthor is the CreatedBy sentinel for an event the user created in the UI,
// as opposed to one a bot booked with the calendar tool.
const userAuthor = "user"

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

// ── Prompt ────────────────────────────────────────────────────────────────────
// The model context for one turn, assembled by the server and handed to the LLM.
// Making it a type rather than a message slice is what keeps the cumulative-
// summary invariant checkable: Summary is one string, so there is nowhere to put
// a second one.
// Context order on the wire: SystemPrompt, Memory (when non-empty), Summary,
// then Messages.
type Prompt struct {
	Bot      Bot       // SystemPrompt and Model come from here
	Memory   string    // the bot's editable memory blob; "" injects nothing
	Summary  string    // newest sealed segment's cumulative summary; "" if never compacted
	Messages []Message // the OPEN segment's raw messages only

	// Tools is the turn's tool surface, bound to this bot — nil offers the
	// model no tools (compaction, tests). The registry behind it is in
	// tools.go; tool calls execute mid-turn as plain store writes.
	Tools *BotToolbox
}

// DECISION (multi-client sync): built bespoke, borrowing JMAP's model and
// vocabulary rather than adopting any protocol — reasoning, rule-outs and the
// call-site oracle live in DESIGN-sync.md. What shipped, in order:
//   - Single-writer lock: flock(2) sidecar taken in Open, released on Close
//     (lock.go); cmd/botnetd binds its port before Open, matching botnetsvc.
//     The startup sweep is unreachable while another process holds the DB.
//   - Change feed: change_log table (AUTOINCREMENT seq, never bare rowid)
//     populated by AFTER INSERT/UPDATE/DELETE triggers on bots/messages/
//     segments, so capture is a schema property, not a Go convention. Read via
//     GET /v1/changes?since={opaque token} → ids-only buckets + tombstones,
//     coalesced (rules on ChangesSince); unknown/pruned cursor → 410
//     cannotCalculateChanges; X-BotNet-State on collection GETs;
//     GET /v1/messages?ids=… batch fetch. One global cursor with per-type
//     buckets, not three tokens — per-type can layer on later.
//   - Write-path hardening: optional client-supplied "msg_" ULID on POST makes
//     sends idempotent (replay returns the stored row, no second turn; wrong
//     bot → 409); If-Match with the derived Bot.Version on PATCH → 412.
//   - Long-poll: ?wait=30s on /v1/changes, same endpoint, cursor and payload
//     as the plain poll.
//
// DEFERRED (token streaming): sequenced BEHIND the feed, per DESIGN-sync.md
// §7 — the feed says message M is awaiting, a token channel streams M, the
// feed's settle is authoritative. Not started.

// Shape: Net → Bots → Segments → Messages. Everything joins by ID reference, so
// each type is an independent storage/sync/CRUD unit.
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
