package botnet

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	modelselector "stdtools/go/lib/modelSelector"
)

// TestRoundTrip is the "framework booted" finish condition: create a net, add a
// bot, append messages, and read the conversation back by BotID.
func TestRoundTrip(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	net, err := s.CreateNet("home")
	if err != nil {
		t.Fatalf("create net: %v", err)
	}

	bot, err := s.CreateBot(net.ID, "Ada", "You are Ada, a helpful bot.", modelselector.DeepSeekV4.ID)
	if err != nil {
		t.Fatalf("create bot: %v", err)
	}

	want := []struct{ role, content string }{
		{"user", "hello"},
		{"bot", "hi, I'm Ada"},
		{"user", "what can you do?"},
	}
	for _, m := range want {
		if _, err := s.AppendMessage(bot.ID, m.role, m.content, StatusSent); err != nil {
			t.Fatalf("append %q: %v", m.content, err)
		}
	}

	conv, err := s.Conversation(bot.ID)
	if err != nil {
		t.Fatalf("conversation: %v", err)
	}
	if len(conv) != len(want) {
		t.Fatalf("got %d messages, want %d", len(conv), len(want))
	}
	open, err := s.OpenSegment(bot.ID)
	if err != nil {
		t.Fatalf("open segment: %v", err)
	}
	if open.Index != 0 || !open.IsOpen() {
		t.Errorf("open segment = %+v, want index 0 and open", open)
	}
	for i, m := range conv {
		if m.Role != want[i].role || m.Content != want[i].content {
			t.Errorf("message %d = (%q, %q), want (%q, %q)", i, m.Role, m.Content, want[i].role, want[i].content)
		}
		if m.BotID != bot.ID {
			t.Errorf("message %d bot id = %q, want %q", i, m.BotID, bot.ID)
		}
		if m.SegmentID != open.ID {
			t.Errorf("message %d segment = %q, want the open segment %q", i, m.SegmentID, open.ID)
		}
		if m.SentAt.IsZero() {
			t.Errorf("message %d has zero SentAt", i)
		}
	}

	// Net membership is derived from the bots table.
	got, err := s.GetNet(net.ID)
	if err != nil {
		t.Fatalf("get net: %v", err)
	}
	if len(got.Bots) != 1 || got.Bots[0] != bot.ID {
		t.Errorf("net bots = %v, want [%s]", got.Bots, bot.ID)
	}

	// Bot round-trips by id with its model reference intact, and carries the
	// list metadata the sidebar needs without reading the transcript.
	gotBot, err := s.GetBot(bot.ID)
	if err != nil {
		t.Fatalf("get bot: %v", err)
	}
	if gotBot.Model != modelselector.DeepSeekV4.ID {
		t.Errorf("bot model = %q, want %q", gotBot.Model, modelselector.DeepSeekV4.ID)
	}
	if !gotBot.ModelValid {
		t.Errorf("bot model %q should be valid", gotBot.Model)
	}
	if gotBot.SystemPrompt != "You are Ada, a helpful bot." {
		t.Errorf("bot system prompt = %q", gotBot.SystemPrompt)
	}
	if gotBot.LastMessageText != "what can you do?" {
		t.Errorf("last message text = %q, want the newest message", gotBot.LastMessageText)
	}
	if gotBot.LastMessageAt.IsZero() {
		t.Error("last message at is zero after appending messages")
	}
	if !gotBot.ReadAt.IsZero() {
		t.Errorf("read at = %v, want zero until marked read", gotBot.ReadAt)
	}
}

func TestGetBotNotFound(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	if _, err := s.GetBot("bot_nope"); err != ErrNotFound {
		t.Errorf("got %v, want ErrNotFound", err)
	}
}

// TestCreateBotRejectsUnknownModel: an id outside the roster never reaches the
// database, so no bot is created that can only fail.
func TestCreateBotRejectsUnknownModel(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	net, err := s.CreateNet("home")
	if err != nil {
		t.Fatalf("create net: %v", err)
	}
	if _, err := s.CreateBot(net.ID, "Broken", "", "openrouter/deepseek/deepseek-v4"); !errors.Is(err, ErrUnknownModel) {
		t.Fatalf("create with unknown model: got %v, want ErrUnknownModel", err)
	}
	bots, err := s.ListBots(net.ID)
	if err != nil {
		t.Fatalf("list bots: %v", err)
	}
	if len(bots) != 0 {
		t.Errorf("list bots = %d, want 0 — the rejected bot must not be persisted", len(bots))
	}
}

func TestPreviewCollapsesWhitespace(t *testing.T) {
	if got := preview("  hello\n\n  there\tworld  "); got != "hello there world" {
		t.Errorf("preview = %q, want collapsed to one line", got)
	}
	long := strings.Repeat("a", previewLimit+50)
	if got := preview(long); len([]rune(got)) != previewLimit+1 { // +1 for the ellipsis
		t.Errorf("preview of a long message is %d runes, want it capped at %d", len([]rune(got)), previewLimit+1)
	}
}

// ── In-flight turns ───────────────────────────────────────────────────────────

// newBot is the two-line setup most of these tests need.
func newBot(t *testing.T, s *Store) Bot {
	t.Helper()
	net, err := s.EnsureDefaultNet()
	if err != nil {
		t.Fatalf("ensure net: %v", err)
	}
	bot, err := s.CreateBot(net.ID, "Ada", "", modelselector.DeepSeekV4.ID)
	if err != nil {
		t.Fatalf("create bot: %v", err)
	}
	return bot
}

// TestOneAwaitingTurnPerBot: the limit is a storage invariant, not a convention.
// The guarded path reports ErrBusy, and a write that skips the guard entirely
// still cannot produce a second awaiting turn — which is what "structural"
// has to mean for it to be worth relying on for transcript ordering.
func TestOneAwaitingTurnPerBot(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	bot := newBot(t, s)

	first, err := s.AppendMessage(bot.ID, "user", "first", StatusAwaiting)
	if err != nil {
		t.Fatalf("first send: %v", err)
	}
	if _, err := s.AppendMessage(bot.ID, "user", "second", StatusAwaiting); !errors.Is(err, ErrBusy) {
		t.Errorf("second send: got %v, want ErrBusy", err)
	}
	// The refused send left nothing behind.
	conv, err := s.Conversation(bot.ID)
	if err != nil {
		t.Fatalf("conversation: %v", err)
	}
	if len(conv) != 1 {
		t.Fatalf("transcript = %+v, want only the first turn", conv)
	}

	// InFlight names the turn holding the bot.
	held, err := s.InFlight(bot.ID)
	if err != nil || held.ID != first.ID {
		t.Errorf("InFlight = (%+v, %v), want %s", held, err, first.ID)
	}

	// Bypassing the guard entirely still cannot break the invariant: the partial
	// unique index refuses the row.
	seg, err := s.OpenSegment(bot.ID)
	if err != nil {
		t.Fatalf("open segment: %v", err)
	}
	_, err = s.db.Exec(
		`INSERT INTO messages (id, bot_id, segment_id, role, content, sent_at, status, error)
		 VALUES (?, ?, ?, 'user', 'sneaky', ?, ?, '')`,
		newID("msg_"), bot.ID, seg.ID, fmtTime(time.Now().UTC()), StatusAwaiting)
	if err == nil {
		t.Error("a raw insert created a second awaiting turn; the invariant is advisory, not structural")
	}

	// A non-awaiting message is unaffected — only the in-flight slot is limited.
	if _, err := s.AppendMessage(bot.ID, "bot", "a reply", StatusSent); err != nil {
		t.Errorf("appending a settled message while one is in flight: %v", err)
	}

	// Settling the turn frees the bot.
	if _, err := s.CompleteTurn(bot.ID, first.ID, "the answer", nil, nil); err != nil {
		t.Fatalf("complete turn: %v", err)
	}
	if _, err := s.InFlight(bot.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("InFlight after completing = %v, want ErrNotFound", err)
	}
	if _, err := s.AppendMessage(bot.ID, "user", "second", StatusAwaiting); err != nil {
		t.Errorf("send after the turn settled: %v", err)
	}
}

// TestCompleteTurnSettlesAtomically: the reply and the settle land together, so
// there is no instant at which the bot is free with its reply still missing —
// the window a second send could have slipped its turn into.
func TestCompleteTurnSettlesAtomically(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	bot := newBot(t, s)

	user, err := s.AppendMessage(bot.ID, "user", "question", StatusAwaiting)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	reply, err := s.CompleteTurn(bot.ID, user.ID, "answer", nil, nil)
	if err != nil {
		t.Fatalf("complete turn: %v", err)
	}
	settled, err := s.GetMessage(user.ID)
	if err != nil {
		t.Fatalf("get message: %v", err)
	}
	if settled.Status != StatusSent {
		t.Errorf("user turn status = %q, want %q", settled.Status, StatusSent)
	}
	conv, err := s.Conversation(bot.ID)
	if err != nil {
		t.Fatalf("conversation: %v", err)
	}
	if len(conv) != 2 || conv[0].ID != user.ID || conv[1].ID != reply.ID {
		t.Errorf("transcript = %+v, want the question then its answer", conv)
	}
}

// TestClaimRetryRefusesWhileBusy: a retry claims the bot the same way a send
// does, so it cannot start a second concurrent turn.
func TestClaimRetryRefusesWhileBusy(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	bot := newBot(t, s)

	stranded, err := s.AppendMessage(bot.ID, "user", "stranded", StatusAwaiting)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if err := s.SetMessageStatus(stranded.ID, StatusFailed, "boom"); err != nil {
		t.Fatalf("strand it: %v", err)
	}
	// Something else is now in flight for this bot.
	if _, err := s.AppendMessage(bot.ID, "user", "in flight", StatusAwaiting); err != nil {
		t.Fatalf("second send: %v", err)
	}
	if err := s.ClaimRetry(stranded); !errors.Is(err, ErrBusy) {
		t.Errorf("retry while busy: got %v, want ErrBusy", err)
	}
	got, err := s.GetMessage(stranded.ID)
	if err != nil {
		t.Fatalf("get message: %v", err)
	}
	if got.Status != StatusFailed {
		t.Errorf("refused retry changed status to %q, want it left %q", got.Status, StatusFailed)
	}
}

// TestInterruptedSendsRecoverOnStartup: a turn left awaiting by a process that
// died has nothing coming to complete it, so a restart must settle it as failed
// rather than leave it spinning forever.
func TestInterruptedSendsRecoverOnStartup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "crash.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	bot := newBot(t, s)
	orphan, err := s.AppendMessage(bot.ID, "user", "answer me", StatusAwaiting)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	// Closing without settling it is what a process death looks like on disk.
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if s, err = Open(path); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()
	got, err := s.GetMessage(orphan.ID)
	if err != nil {
		t.Fatalf("get message: %v", err)
	}
	if got.Status != StatusFailed {
		t.Fatalf("interrupted turn status = %q, want %q after a restart", got.Status, StatusFailed)
	}
	if !strings.Contains(got.Error, "restarted") {
		t.Errorf("error = %q, want it to say the server restarted", got.Error)
	}
	// The message survives intact and the bot is free again.
	if got.Content != "answer me" {
		t.Errorf("content = %q, want it preserved", got.Content)
	}
	if _, err := s.InFlight(bot.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("InFlight after recovery = %v, want ErrNotFound", err)
	}
	// And it is retryable, which is the whole point of settling it as failed.
	if err := s.ClaimRetry(got); err != nil {
		t.Fatalf("retry a recovered turn: %v", err)
	}
	if again, _ := s.GetMessage(orphan.ID); again.Status != StatusAwaiting || again.Error != "" {
		t.Errorf("after retry = %+v, want awaiting with the error cleared", again)
	}
}

// ── Migration ─────────────────────────────────────────────────────────────────

// seedLegacyDB writes a database in the pre-segment shape — exactly the schema
// and row shapes found in a live ~/.botnet/net.db, including a bot holding a
// model id that has since left the roster.
func seedLegacyDB(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("seed open: %v", err)
	}
	defer db.Close()
	const legacy = `
CREATE TABLE nets (id TEXT PRIMARY KEY, name TEXT NOT NULL);
CREATE TABLE bots (
    id TEXT PRIMARY KEY, net_id TEXT NOT NULL REFERENCES nets(id), display_name TEXT NOT NULL,
    created_at TEXT NOT NULL, system_prompt TEXT NOT NULL, model TEXT NOT NULL);
CREATE INDEX idx_bots_net ON bots(net_id);
CREATE TABLE messages (
    id TEXT PRIMARY KEY, bot_id TEXT NOT NULL REFERENCES bots(id), role TEXT NOT NULL,
    content TEXT NOT NULL, sent_at TEXT NOT NULL);
CREATE INDEX idx_messages_bot ON messages(bot_id, id);

INSERT INTO nets VALUES ('net_LEGACY', 'default');
INSERT INTO bots VALUES ('bot_STALE', 'net_LEGACY', 'First bot',
    '2020-01-01T10:00:00Z', '', 'openrouter/deepseek/deepseek-v4');
INSERT INTO bots VALUES ('bot_OK', 'net_LEGACY', 'hihi',
    '2020-01-01T11:00:00Z', '', 'openrouter/deepseek/deepseek-v4-flash-0731');
-- A bot nobody ever messaged: it must come through with a zero read_at rather
-- than a fabricated one.
INSERT INTO bots VALUES ('bot_QUIET', 'net_LEGACY', 'never used',
    '2020-01-01T12:00:00Z', '', 'openrouter/z-ai/glm-5.3-flash');
INSERT INTO messages VALUES ('msg_1', 'bot_STALE', 'user', 'hi there', '2020-01-01T10:01:00Z');
INSERT INTO messages VALUES ('msg_2', 'bot_OK', 'user', 'hello   world', '2020-01-01T11:01:00Z');
INSERT INTO messages VALUES ('msg_3', 'bot_OK', 'bot', ' Hello to you too!', '2020-01-01T11:02:00Z');
`
	if _, err := db.Exec(legacy); err != nil {
		t.Fatalf("seed legacy schema: %v", err)
	}
}

// unread is the sidebar's rule, spelled out once so the tests below assert the
// thing the UI actually renders.
func unread(b Bot) bool { return b.LastMessageAt.After(b.ReadAt) }

// TestMigrationLeavesNoBotUnread: upgrading is not new activity, so a migrated
// database must not light up every row in the sidebar. The pair that pins the
// behavior is that migration reports nothing unread, and that a message
// arriving afterwards does.
func TestMigrationLeavesNoBotUnread(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	seedLegacyDB(t, path)
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	bots, err := s.ListBots("net_LEGACY")
	if err != nil {
		t.Fatalf("list bots: %v", err)
	}
	if len(bots) != 3 {
		t.Fatalf("listed %d bots, want the 3 seeded", len(bots))
	}
	for _, b := range bots {
		if unread(b) {
			t.Errorf("bot %s reads as unread after migration (lastMessageAt %v > readAt %v)",
				b.ID, b.LastMessageAt, b.ReadAt)
		}
	}
	// A migrated bot is read exactly up to its last message, not to some later
	// wall-clock instant.
	ok, err := s.GetBot("bot_OK")
	if err != nil {
		t.Fatalf("get bot: %v", err)
	}
	if !ok.ReadAt.Equal(ok.LastMessageAt) {
		t.Errorf("readAt = %v, want it equal to lastMessageAt %v", ok.ReadAt, ok.LastMessageAt)
	}
	// The never-messaged bot keeps a zero readAt rather than a fabricated one.
	quiet, err := s.GetBot("bot_QUIET")
	if err != nil {
		t.Fatalf("get quiet bot: %v", err)
	}
	if !quiet.ReadAt.IsZero() || !quiet.LastMessageAt.IsZero() {
		t.Errorf("never-messaged bot = readAt %v / lastMessageAt %v, want both zero",
			quiet.ReadAt, quiet.LastMessageAt)
	}

	// A message arriving AFTER migration does make its bot unread.
	if _, err := s.AppendMessage("bot_OK", "user", "something new", StatusSent); err != nil {
		t.Fatalf("append: %v", err)
	}
	if ok, err = s.GetBot("bot_OK"); err != nil {
		t.Fatalf("get bot: %v", err)
	}
	if !unread(ok) {
		t.Fatalf("bot is not unread after a new message (lastMessageAt %v, readAt %v)",
			ok.LastMessageAt, ok.ReadAt)
	}

	// And a restart must not swallow it. This is the trap the one-shot exists
	// for: a guarded `WHERE read_at = ''` backfill would leave this bot alone
	// (its read_at is set), but a bot created after the upgrade and left unread
	// has an EMPTY read_at and is indistinguishable from a legacy row — so the
	// same guard would silently mark it read. Both cases are checked below.
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if s, err = Open(path); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()
	if ok, err = s.GetBot("bot_OK"); err != nil {
		t.Fatalf("get bot: %v", err)
	}
	if !unread(ok) {
		t.Error("a restart marked an unread bot read")
	}

	// A bot created after the upgrade, messaged and never read, stays unread
	// across a restart.
	fresh, err := s.CreateBot("net_LEGACY", "Newcomer", "", modelselector.DeepSeekV4.ID)
	if err != nil {
		t.Fatalf("create bot: %v", err)
	}
	if !fresh.ReadAt.IsZero() {
		t.Errorf("a new bot starts with readAt %v, want zero", fresh.ReadAt)
	}
	if _, err := s.AppendMessage(fresh.ID, "user", "hello", StatusSent); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if s, err = Open(path); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got, err := s.GetBot(fresh.ID)
	if err != nil {
		t.Fatalf("get bot: %v", err)
	}
	if !unread(got) {
		t.Errorf("a bot created after the upgrade and never read was marked read by a restart "+
			"(readAt %v, lastMessageAt %v)", got.ReadAt, got.LastMessageAt)
	}

	// MarkRead clears it, to the last message rather than to now.
	read, err := s.MarkRead(fresh.ID)
	if err != nil {
		t.Fatalf("mark read: %v", err)
	}
	if !read.ReadAt.Equal(read.LastMessageAt) {
		t.Errorf("after MarkRead readAt = %v, want lastMessageAt %v", read.ReadAt, read.LastMessageAt)
	}
	if unread(read) {
		t.Error("bot still reads as unread after MarkRead")
	}
}

// TestReadAtBackfillReachesAnAlreadyMigratedDatabase covers the databases
// migrated by the build that added read_at WITHOUT backfilling it: the column is
// already present and empty, so any scheme keyed on the moment the column is
// created would skip them and leave every bot badged unread forever. The
// recorded marker is what reaches them on the next open.
func TestReadAtBackfillReachesAnAlreadyMigratedDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	seedLegacyDB(t, path)
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Reconstruct the interim state: every column present, read_at blank, and no
	// record that the backfill ever ran.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("reopen raw: %v", err)
	}
	if _, err := db.Exec(`UPDATE bots SET read_at = ''; DELETE FROM meta;`); err != nil {
		t.Fatalf("reconstruct interim state: %v", err)
	}
	db.Close()

	if s, err = Open(path); err != nil {
		t.Fatalf("open after interim state: %v", err)
	}
	defer s.Close()
	bots, err := s.ListBots("net_LEGACY")
	if err != nil {
		t.Fatalf("list bots: %v", err)
	}
	for _, b := range bots {
		if unread(b) {
			t.Errorf("bot %s still reads as unread; the backfill did not reach an already-migrated database "+
				"(lastMessageAt %v, readAt %v)", b.ID, b.LastMessageAt, b.ReadAt)
		}
	}

	// Having run, it does not run again: a bot messaged and left unread after
	// this point stays unread across a restart.
	if _, err := s.AppendMessage("bot_OK", "user", "brand new", StatusSent); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if s, err = Open(path); err != nil {
		t.Fatalf("final open: %v", err)
	}
	got, err := s.GetBot("bot_OK")
	if err != nil {
		t.Fatalf("get bot: %v", err)
	}
	if !unread(got) {
		t.Error("the one-shot ran a second time and swallowed a genuinely unread message")
	}
}

// TestMarkReadDoesNotSwallowAConcurrentMessage: read_at is a watermark over the
// messages that exist, not a wall-clock stamp. A message that lands after the
// read must still show as unread, which a `read_at = now()` implementation would
// get wrong.
func TestMarkReadDoesNotSwallowAConcurrentMessage(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	net, err := s.CreateNet("home")
	if err != nil {
		t.Fatalf("create net: %v", err)
	}
	bot, err := s.CreateBot(net.ID, "Ada", "", modelselector.DeepSeekV4.ID)
	if err != nil {
		t.Fatalf("create bot: %v", err)
	}
	if _, err := s.AppendMessage(bot.ID, "user", "first", StatusSent); err != nil {
		t.Fatalf("append: %v", err)
	}
	read, err := s.MarkRead(bot.ID)
	if err != nil {
		t.Fatalf("mark read: %v", err)
	}
	if unread(read) {
		t.Fatal("bot is unread immediately after being marked read")
	}

	// A message arriving after the read is unread, even though it arrived before
	// the wall clock moved much.
	if _, err := s.AppendMessage(bot.ID, "bot", "second", StatusSent); err != nil {
		t.Fatalf("append: %v", err)
	}
	after, err := s.GetBot(bot.ID)
	if err != nil {
		t.Fatalf("get bot: %v", err)
	}
	if !unread(after) {
		t.Errorf("a message arriving after the read was swallowed (readAt %v, lastMessageAt %v)",
			after.ReadAt, after.LastMessageAt)
	}

	// Marking a bot with no messages read leaves the watermark at zero.
	quiet, err := s.CreateBot(net.ID, "Quiet", "", modelselector.DeepSeekV4.ID)
	if err != nil {
		t.Fatalf("create bot: %v", err)
	}
	if quiet, err = s.MarkRead(quiet.ID); err != nil {
		t.Fatalf("mark read: %v", err)
	}
	if !quiet.ReadAt.IsZero() {
		t.Errorf("readAt = %v on a bot with no messages, want zero", quiet.ReadAt)
	}
}

// TestMigrationIsIdempotent proves migrate is safe on every Open, which is what
// the botnetd process relies on: it runs on every start. Opening a legacy
// database repeatedly must not create a second segment 0, reassign a message,
// or clobber list metadata — and that must stay true after the bot has been
// compacted, when segment 0 is no longer the open one.
func TestMigrationIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	seedLegacyDB(t, path)

	// ── First open: the backfill runs.
	s, err := Open(path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	segs, err := s.Segments("bot_OK")
	if err != nil {
		t.Fatalf("segments: %v", err)
	}
	if len(segs) != 1 || segs[0].Index != 0 || !segs[0].IsOpen() {
		t.Fatalf("segments after migration = %+v, want one open segment 0", segs)
	}
	seg0 := segs[0].ID
	conv, err := s.Conversation("bot_OK")
	if err != nil {
		t.Fatalf("conversation: %v", err)
	}
	if len(conv) != 2 {
		t.Fatalf("conversation = %d messages, want 2", len(conv))
	}
	for _, m := range conv {
		if m.SegmentID != seg0 {
			t.Errorf("message %s segment = %q, want segment 0 %q", m.ID, m.SegmentID, seg0)
		}
		if m.Status != StatusSent {
			t.Errorf("migrated message %s status = %q, want %q", m.ID, m.Status, StatusSent)
		}
	}
	bot, err := s.GetBot("bot_OK")
	if err != nil {
		t.Fatalf("get bot: %v", err)
	}
	if bot.LastMessageText != "Hello to you too!" {
		t.Errorf("backfilled preview = %q, want the last message collapsed", bot.LastMessageText)
	}
	if bot.LastMessageAt.IsZero() {
		t.Error("backfilled last message at is zero")
	}
	// The stale-model bot migrated like any other and stays readable.
	stale, err := s.GetBot("bot_STALE")
	if err != nil {
		t.Fatalf("get stale bot: %v", err)
	}
	if stale.ModelValid {
		t.Errorf("bot_STALE model %q should not resolve", stale.Model)
	}
	staleSegs, err := s.Segments("bot_STALE")
	if err != nil || len(staleSegs) != 1 {
		t.Fatalf("stale bot segments = %+v (%v), want one", staleSegs, err)
	}

	// ── Second open: nothing changes.
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	assertUnchanged := func(when string) {
		t.Helper()
		got, err := s.Segments("bot_OK")
		if err != nil {
			t.Fatalf("%s: segments: %v", when, err)
		}
		if len(got) != len(segs) {
			t.Fatalf("%s: segments = %d, want %d — the backfill ran again", when, len(got), len(segs))
		}
		if got[0].ID != seg0 {
			t.Errorf("%s: segment 0 id = %q, want the original %q", when, got[0].ID, seg0)
		}
		conv, err := s.Conversation("bot_OK")
		if err != nil {
			t.Fatalf("%s: conversation: %v", when, err)
		}
		for _, m := range conv {
			if m.SegmentID != seg0 {
				t.Errorf("%s: message %s reassigned to %q", when, m.ID, m.SegmentID)
			}
		}
		b, err := s.GetBot("bot_OK")
		if err != nil {
			t.Fatalf("%s: get bot: %v", when, err)
		}
		if b.LastMessageText != bot.LastMessageText || !b.LastMessageAt.Equal(bot.LastMessageAt) {
			t.Errorf("%s: list metadata changed: %q/%v", when, b.LastMessageText, b.LastMessageAt)
		}
	}
	assertUnchanged("second open")

	// ── Third open, after compaction: segment 0 is sealed and no longer the
	// open one. A re-run must still not resurrect a segment 0 or move messages
	// off it onto the new open segment.
	open, err := s.OpenSegment("bot_OK")
	if err != nil {
		t.Fatalf("open segment: %v", err)
	}
	if err := s.Seal(open, "summary of segment 0"); err != nil {
		t.Fatalf("seal: %v", err)
	}
	segs, err = s.Segments("bot_OK")
	if err != nil {
		t.Fatalf("segments after seal: %v", err)
	}
	if len(segs) != 2 {
		t.Fatalf("segments after seal = %d, want 2", len(segs))
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatalf("third open: %v", err)
	}
	defer s.Close()
	assertUnchanged("third open, post-compaction")
	if got, _ := s.Segments("bot_OK"); len(got) != 2 || got[0].IsOpen() || !got[1].IsOpen() {
		t.Errorf("chain after reopen = %+v, want segment 0 sealed and segment 1 open", got)
	}
}
