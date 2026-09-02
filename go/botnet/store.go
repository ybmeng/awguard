package botnet

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	modelselector "stdtools/go/lib/modelSelector"

	_ "modernc.org/sqlite" // pure-Go SQLite driver (no cgo)
)

// Store is the bot library backed by SQLite. Everything lives in one disk file;
// the whole domain (nets, bots, segments, messages) is derived from the
// schema.go structs.
//
// The backend is deliberately behind this one type — if the SQLite dependency
// is ever reversed, only this file changes.
type Store struct {
	db   *sql.DB
	lock *os.File // single-writer flock sidecar; nil for :memory: — see lock.go
}

// Open opens (creating if needed) the SQLite database at path and ensures the
// schema exists. Pass ":memory:" for an ephemeral store (used by tests).
// WAL mode is enabled for concurrent readers; sidecar -wal/-shm files appear
// next to a file-backed path.
//
// A file-backed database is single-writer: Open takes an exclusive lock and
// returns ErrLocked while another process holds it. The lock must precede
// everything else, because migrate's startup sweep would fail another
// process's in-flight turns.
func Open(path string) (*Store, error) {
	lock, err := acquireLock(path)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		releaseLock(lock)
		return nil, fmt.Errorf("open sqlite %q: %w", path, err)
	}
	// database/sql pools connections; a file DB with WAL tolerates that, but a
	// single conn keeps :memory: coherent and avoids cross-conn lock churn.
	db.SetMaxOpenConns(1)
	s := &Store{db: db, lock: lock}
	if _, err := db.Exec(`PRAGMA journal_mode = WAL; PRAGMA foreign_keys = ON;`); err != nil {
		s.Close()
		return nil, fmt.Errorf("set pragmas: %w", err)
	}
	if err := s.migrate(); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the underlying database and releases the single-writer lock,
// in that order — the lock outlives the last write.
func (s *Store) Close() error {
	err := s.db.Close()
	releaseLock(s.lock)
	return err
}

// dbtx is the query surface shared by *sql.DB and *sql.Tx, so a helper can run
// standalone or as one step of a larger transaction without knowing which.
type dbtx interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

// tx runs fn in a transaction, rolling back on error. Writes that must not be
// observed half-done — claiming a bot then appending its turn, appending a reply
// then settling the turn it answers — go through here.
func (s *Store) tx(fn func(dbtx) error) error {
	t, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer t.Rollback()
	if err := fn(t); err != nil {
		return err
	}
	return t.Commit()
}

// ── Times ─────────────────────────────────────────────────────────────────────
// Timestamps store as RFC3339Nano text, and the zero time stores as the empty
// string so "never" is distinguishable from a real instant. Over the wire a zero
// time marshals as Go's "0001-01-01T00:00:00Z" and means the same thing.

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func parseTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339Nano, s)
}

// Event times are the one exception to the format above: fixed-width RFC3339
// UTC to the second, because ListEvents filters a range with a TEXT comparison
// and RFC3339Nano's variable-length fraction does not sort chronologically —
// see the storage DECISION on Event. Everything an Event holds goes through
// here, timestamps included, so one table never mixes two encodings.

func fmtEventTime(t time.Time) string {
	return t.UTC().Truncate(time.Second).Format(time.RFC3339)
}

func parseEventTime(s string) (time.Time, error) {
	return time.Parse(time.RFC3339, s)
}

// migrate creates the tables if absent and brings older databases up to the
// current shape. Idempotent — safe on every Open. Every step is guarded by the
// state it produces, so a second run changes nothing: no second segment 0, no
// message reassigned, no list metadata clobbered.
func (s *Store) migrate() error {
	const schema = `
CREATE TABLE IF NOT EXISTS nets (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS bots (
    id            TEXT PRIMARY KEY,
    net_id        TEXT NOT NULL REFERENCES nets(id),
    display_name  TEXT NOT NULL,
    created_at    TEXT NOT NULL,
    system_prompt TEXT NOT NULL,
    model         TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_bots_net ON bots(net_id);
CREATE TABLE IF NOT EXISTS messages (
    id       TEXT PRIMARY KEY,
    bot_id   TEXT NOT NULL REFERENCES bots(id),
    role     TEXT NOT NULL,
    content  TEXT NOT NULL,
    sent_at  TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_messages_bot ON messages(bot_id, id);
CREATE TABLE IF NOT EXISTS segments (
    id        TEXT PRIMARY KEY,
    bot_id    TEXT NOT NULL REFERENCES bots(id),
    idx       INTEGER NOT NULL,
    opened_at TEXT NOT NULL,
    sealed_at TEXT NOT NULL DEFAULT '',
    summary   TEXT NOT NULL DEFAULT ''
);
-- One segment per position per bot: the structural guard that a re-run of the
-- backfill cannot produce a second segment 0.
CREATE UNIQUE INDEX IF NOT EXISTS idx_segments_bot_idx ON segments(bot_id, idx);
-- The calendar service. Owned by the net, not by a bot: the user and every bot
-- read and write the SAME calendar, which is the whole point of it being a
-- service rather than a per-bot field. Times are fixed-width RFC3339 UTC to the
-- second so the range filter below can compare them as text — see the storage
-- DECISION on Event.
CREATE TABLE IF NOT EXISTS events (
    id         TEXT PRIMARY KEY,
    title      TEXT NOT NULL,
    starts_at  TEXT NOT NULL,
    ends_at    TEXT NOT NULL,
    location   TEXT NOT NULL DEFAULT '',
    notes      TEXT NOT NULL DEFAULT '',
    created_by TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
-- The calendar is read as a time range far more than by id.
CREATE INDEX IF NOT EXISTS idx_events_starts ON events(starts_at);
-- Named calendars partition the events table. No default flag and no reserved
-- id: the default is the calendar NAMED "Personal", ensured on demand — see the
-- Calendar DECISIONs in schema.go. Name uniqueness is case-insensitive, and the
-- index makes the database enforce it rather than a Go check remembering to.
CREATE TABLE IF NOT EXISTS calendars (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL,
    color      TEXT NOT NULL,
    created_by TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_calendars_name ON calendars(name COLLATE NOCASE);
-- The projects service: what work is ABOUT, as against the automations that say
-- how it happens. A project is a name, a goal and its facts; its health is
-- DERIVED from the facts on every read and deliberately has no column here —
-- see the Project DECISIONs in schema.go. Name uniqueness is case-insensitive
-- and the index enforces it, exactly as calendars do — globally, NOT per
-- parent, so the name a user says out loud addresses one project whatever the
-- hierarchy looks like. parent_id, default_lead_days, owner_bot_id and
-- last_health ride the guarded column list below.
CREATE TABLE IF NOT EXISTS projects (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL,
    goal       TEXT NOT NULL DEFAULT '',
    created_by TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_projects_name ON projects(name COLLATE NOCASE);
-- Facts are the project's only AUTHORED state. due is fixed-width RFC3339 UTC
-- to the second like every other date the store range-compares, and '' when the
-- kind carries no date, so "undated" is distinguishable from the year 1.
-- event_id points at the projected calendar event, '' when the fact needs none.
CREATE TABLE IF NOT EXISTS facts (
    id         TEXT PRIMARY KEY,
    project_id TEXT NOT NULL REFERENCES projects(id),
    kind       TEXT NOT NULL,
    title      TEXT NOT NULL,
    due        TEXT NOT NULL DEFAULT '',
    lead_days  INTEGER NOT NULL DEFAULT 0,
    rrule      TEXT NOT NULL DEFAULT '',
    tz         TEXT NOT NULL DEFAULT '',
    done       INTEGER NOT NULL DEFAULT 0,
    blocker    TEXT NOT NULL DEFAULT '',
    body       TEXT NOT NULL DEFAULT '',
    event_id   TEXT NOT NULL DEFAULT '',
    created_by TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
-- Facts are always read a whole project at a time.
CREATE INDEX IF NOT EXISTS idx_facts_project ON facts(project_id);
-- Bookkeeping for one-shot migration steps; see once().
CREATE TABLE IF NOT EXISTS meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
-- The change feed's log: one pointer row (never a payload) per mutation of a
-- synced entity, appended by the AFTER triggers below rather than by Go code,
-- so a write that bypasses the store helpers — a migration step, a manual
-- sqlite3 session — is captured all the same. seq must be AUTOINCREMENT: bare
-- rowids are reused after a delete, and a reused position would let a stale
-- cursor silently resolve to the wrong place instead of failing.
CREATE TABLE IF NOT EXISTS change_log (
    seq       INTEGER PRIMARY KEY AUTOINCREMENT,
    entity    TEXT NOT NULL, -- 'bot' | 'message' | 'segment' | 'event' | 'calendar' | 'project' | 'fact'
    entity_id TEXT NOT NULL,
    op        TEXT NOT NULL  -- 'created' | 'updated' | 'destroyed'
);
CREATE TRIGGER IF NOT EXISTS chg_bot_created AFTER INSERT ON bots BEGIN
    INSERT INTO change_log (entity, entity_id, op) VALUES ('bot', NEW.id, 'created');
END;
CREATE TRIGGER IF NOT EXISTS chg_bot_updated AFTER UPDATE ON bots BEGIN
    INSERT INTO change_log (entity, entity_id, op) VALUES ('bot', NEW.id, 'updated');
END;
CREATE TRIGGER IF NOT EXISTS chg_bot_destroyed AFTER DELETE ON bots BEGIN
    INSERT INTO change_log (entity, entity_id, op) VALUES ('bot', OLD.id, 'destroyed');
END;
CREATE TRIGGER IF NOT EXISTS chg_message_created AFTER INSERT ON messages BEGIN
    INSERT INTO change_log (entity, entity_id, op) VALUES ('message', NEW.id, 'created');
END;
CREATE TRIGGER IF NOT EXISTS chg_message_updated AFTER UPDATE ON messages BEGIN
    INSERT INTO change_log (entity, entity_id, op) VALUES ('message', NEW.id, 'updated');
END;
CREATE TRIGGER IF NOT EXISTS chg_message_destroyed AFTER DELETE ON messages BEGIN
    INSERT INTO change_log (entity, entity_id, op) VALUES ('message', OLD.id, 'destroyed');
END;
CREATE TRIGGER IF NOT EXISTS chg_segment_created AFTER INSERT ON segments BEGIN
    INSERT INTO change_log (entity, entity_id, op) VALUES ('segment', NEW.id, 'created');
END;
CREATE TRIGGER IF NOT EXISTS chg_segment_updated AFTER UPDATE ON segments BEGIN
    INSERT INTO change_log (entity, entity_id, op) VALUES ('segment', NEW.id, 'updated');
END;
CREATE TRIGGER IF NOT EXISTS chg_segment_destroyed AFTER DELETE ON segments BEGIN
    INSERT INTO change_log (entity, entity_id, op) VALUES ('segment', OLD.id, 'destroyed');
END;
CREATE TRIGGER IF NOT EXISTS chg_event_created AFTER INSERT ON events BEGIN
    INSERT INTO change_log (entity, entity_id, op) VALUES ('event', NEW.id, 'created');
END;
CREATE TRIGGER IF NOT EXISTS chg_event_updated AFTER UPDATE ON events BEGIN
    INSERT INTO change_log (entity, entity_id, op) VALUES ('event', NEW.id, 'updated');
END;
CREATE TRIGGER IF NOT EXISTS chg_event_destroyed AFTER DELETE ON events BEGIN
    INSERT INTO change_log (entity, entity_id, op) VALUES ('event', OLD.id, 'destroyed');
END;
CREATE TRIGGER IF NOT EXISTS chg_calendar_created AFTER INSERT ON calendars BEGIN
    INSERT INTO change_log (entity, entity_id, op) VALUES ('calendar', NEW.id, 'created');
END;
CREATE TRIGGER IF NOT EXISTS chg_calendar_updated AFTER UPDATE ON calendars BEGIN
    INSERT INTO change_log (entity, entity_id, op) VALUES ('calendar', NEW.id, 'updated');
END;
CREATE TRIGGER IF NOT EXISTS chg_calendar_destroyed AFTER DELETE ON calendars BEGIN
    INSERT INTO change_log (entity, entity_id, op) VALUES ('calendar', OLD.id, 'destroyed');
END;
CREATE TRIGGER IF NOT EXISTS chg_project_created AFTER INSERT ON projects BEGIN
    INSERT INTO change_log (entity, entity_id, op) VALUES ('project', NEW.id, 'created');
END;
CREATE TRIGGER IF NOT EXISTS chg_project_updated AFTER UPDATE ON projects BEGIN
    INSERT INTO change_log (entity, entity_id, op) VALUES ('project', NEW.id, 'updated');
END;
CREATE TRIGGER IF NOT EXISTS chg_project_destroyed AFTER DELETE ON projects BEGIN
    INSERT INTO change_log (entity, entity_id, op) VALUES ('project', OLD.id, 'destroyed');
END;
CREATE TRIGGER IF NOT EXISTS chg_fact_created AFTER INSERT ON facts BEGIN
    INSERT INTO change_log (entity, entity_id, op) VALUES ('fact', NEW.id, 'created');
END;
CREATE TRIGGER IF NOT EXISTS chg_fact_updated AFTER UPDATE ON facts BEGIN
    INSERT INTO change_log (entity, entity_id, op) VALUES ('fact', NEW.id, 'updated');
END;
CREATE TRIGGER IF NOT EXISTS chg_fact_destroyed AFTER DELETE ON facts BEGIN
    INSERT INTO change_log (entity, entity_id, op) VALUES ('fact', OLD.id, 'destroyed');
END;
`
	if _, err := s.db.Exec(schema); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}

	// Columns added after the first release. Existing rows take the default,
	// which is also what the backfill below looks for.
	added := []struct{ table, column, decl string }{
		{"bots", "last_message_at", `TEXT NOT NULL DEFAULT ''`},
		{"bots", "last_message_text", `TEXT NOT NULL DEFAULT ''`},
		{"bots", "read_at", `TEXT NOT NULL DEFAULT ''`},
		{"bots", "memory", `TEXT NOT NULL DEFAULT ''`},
		{"messages", "segment_id", `TEXT NOT NULL DEFAULT ''`},
		{"messages", "status", `TEXT NOT NULL DEFAULT 'sent'`},
		{"messages", "error", `TEXT NOT NULL DEFAULT ''`},
		// citations is a JSON array of Citation, '' when the reply cited nothing
		// (the common case). Riding a column keeps it in the message's own INSERT,
		// so the change_log triggers capture it with no new write path.
		{"messages", "citations", `TEXT NOT NULL DEFAULT ''`},
		// tool_calls is a JSON array of ToolCall, '' when the reply called no tool
		// (the common case). Same rationale as citations: it rides the message
		// INSERT, so change_log capture needs no new write path.
		{"messages", "tool_calls", `TEXT NOT NULL DEFAULT ''`},
		// calendar_id is logically NOT NULL without a real default: the DEFAULT ''
		// exists only so the column can be added to a live table, and the backfill
		// below immediately points every '' row at the ensured Personal calendar.
		// Current write paths always resolve a real calendar before inserting.
		{"events", "calendar_id", `TEXT NOT NULL DEFAULT ''`},
		// Recurrence + firing (the Event DECISIONs in schema.go). '' / 0 are the
		// real defaults — a pre-recurrence row decodes as a single, non-firing
		// event with no backfill to run. The fields ride the existing rows, so
		// the chg_event_*/chg_calendar_* triggers capture their edits with no
		// new write path and NO new synced entity.
		{"events", "rrule", `TEXT NOT NULL DEFAULT ''`},
		{"events", "tz", `TEXT NOT NULL DEFAULT ''`},
		{"events", "automation", `TEXT NOT NULL DEFAULT ''`},
		{"calendars", "executable", `INTEGER NOT NULL DEFAULT 0`},
		// parent_id is the project hierarchy's only authored state (the Project
		// DECISIONs in schema.go). '' is the real default and needs no backfill:
		// every project written before hierarchy existed IS a top-level project,
		// which is exactly what '' means.
		{"projects", "parent_id", `TEXT NOT NULL DEFAULT ''`},
		// The project's inherited lead threshold. 0 is the real default and
		// means "unset — take it from above", so a project written before the
		// column existed reads the global default with no backfill to run.
		{"projects", "default_lead_days", `INTEGER NOT NULL DEFAULT 0`},
		// The owner bot and the tick's bookkeeping. '' is the real default for
		// both — "nobody owns this" and "never ticked" — so a pre-nudge row
		// needs no backfill and, having no recorded health, nudges on the first
		// tick that finds it already unwell.
		{"projects", "owner_bot_id", `TEXT NOT NULL DEFAULT ''`},
		{"projects", "last_health", `TEXT NOT NULL DEFAULT ''`},
	}
	for _, c := range added {
		if err := s.addColumn(c.table, c.column, c.decl); err != nil {
			return err
		}
	}
	return s.backfill()
}

// addColumn adds a column only if it is absent, so migrate stays re-runnable.
func (s *Store) addColumn(table, column, decl string) error {
	var one int
	err := s.db.QueryRow(`SELECT 1 FROM pragma_table_info(?) WHERE name = ?`, table, column).Scan(&one)
	if err == nil {
		return nil // already there
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("inspect %s.%s: %w", table, column, err)
	}
	if _, err := s.db.Exec(fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s %s`, table, column, decl)); err != nil {
		return fmt.Errorf("add %s.%s: %w", table, column, err)
	}
	return nil
}

// once runs a migration step at most once in the life of a database, recording
// it in the meta table. Reserve it for steps that genuinely CANNOT be expressed
// as a guard over the data itself — every other backfill here is guarded by the
// state it writes, which is cheaper and self-healing. See markExistingBotsRead
// for the one case that needs this.
func (s *Store) once(key string, step func() error) error {
	var seen string
	err := s.db.QueryRow(`SELECT value FROM meta WHERE key = ?`, key).Scan(&seen)
	if err == nil {
		return nil // already run
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("check one-shot %q: %w", key, err)
	}
	if err := step(); err != nil {
		return err
	}
	if _, err := s.db.Exec(`INSERT INTO meta (key, value) VALUES (?, ?)`,
		key, fmtTime(time.Now().UTC())); err != nil {
		return fmt.Errorf("record one-shot %q: %w", key, err)
	}
	return nil
}

// markExistingBotsRead marks every bot that predates the unread feature as read
// up to its newest message. An upgrade is not new activity, and a sidebar that
// comes back with a badge on every row conveys nothing.
//
// This has to be a ONE-SHOT rather than a guarded, re-runnable step. Every other
// backfill can be guarded by the state it writes, but this one cannot: a bot
// created AFTER the upgrade, messaged and genuinely left unread, has exactly the
// shape a pre-upgrade bot has — an empty read_at beside a real last_message_at —
// so a guard on the empty read_at would silently mark it read on the next
// restart, which is the very bug this backfill exists to avoid. Only a recorded
// marker can tell the two apart, so it runs under once().
//
// It still fills blanks only, so a bot the user has actually read keeps its own
// watermark. A bot with no messages has an empty last_message_at and keeps a
// zero read_at rather than a fabricated one — with no last message it cannot be
// unread either way.
func (s *Store) markExistingBotsRead() error {
	if _, err := s.db.Exec(`UPDATE bots SET read_at = last_message_at WHERE read_at = ''`); err != nil {
		return fmt.Errorf("backfill read_at: %w", err)
	}
	return nil
}

// backfill gives pre-segment databases the state the current code assumes:
// every bot has an open segment 0, every message belongs to it, and every bot
// carries list metadata. Each step's guard is the state it writes, so running it
// twice is a no-op.
func (s *Store) backfill() error {
	// 1. Every bot with no segments at all gets one open segment 0. Bots that
	//    already have a chain (including a compacted one) are skipped entirely.
	type newSeg struct{ botID, openedAt string }
	var pending []newSeg
	rows, err := s.db.Query(`SELECT id, created_at FROM bots WHERE id NOT IN (SELECT bot_id FROM segments)`)
	if err != nil {
		return fmt.Errorf("backfill segments: %w", err)
	}
	for rows.Next() {
		var n newSeg
		if err := rows.Scan(&n.botID, &n.openedAt); err != nil {
			rows.Close()
			return fmt.Errorf("scan bot for backfill: %w", err)
		}
		pending = append(pending, n)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("backfill segments: %w", err)
	}
	for _, n := range pending {
		if _, err := s.db.Exec(
			`INSERT INTO segments (id, bot_id, idx, opened_at, sealed_at, summary) VALUES (?, ?, 0, ?, '', '')`,
			newID("seg_"), n.botID, n.openedAt); err != nil {
			return fmt.Errorf("backfill segment for %s: %w", n.botID, err)
		}
	}

	// 2. Every message with no segment joins its bot's segment 0. Messages
	//    written by the current code always carry one, so this only ever moves
	//    pre-migration rows.
	if _, err := s.db.Exec(`
UPDATE messages SET segment_id = (SELECT id FROM segments WHERE bot_id = messages.bot_id AND idx = 0)
 WHERE segment_id = ''
   AND EXISTS (SELECT 1 FROM segments WHERE bot_id = messages.bot_id AND idx = 0)`); err != nil {
		return fmt.Errorf("backfill message segments: %w", err)
	}

	// 3. List metadata from each bot's last message. A bot with a recorded
	//    last_message_at is left alone, so live values are never clobbered.
	type meta struct{ botID, sentAt, content string }
	var metas []meta
	rows, err = s.db.Query(`
SELECT b.id, m.sent_at, m.content FROM bots b
  JOIN messages m ON m.rowid = (SELECT rowid FROM messages WHERE bot_id = b.id ORDER BY rowid DESC LIMIT 1)
 WHERE b.last_message_at = ''`)
	if err != nil {
		return fmt.Errorf("backfill list metadata: %w", err)
	}
	for rows.Next() {
		var m meta
		if err := rows.Scan(&m.botID, &m.sentAt, &m.content); err != nil {
			rows.Close()
			return fmt.Errorf("scan list metadata: %w", err)
		}
		metas = append(metas, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("backfill list metadata: %w", err)
	}
	for _, m := range metas {
		if _, err := s.db.Exec(`UPDATE bots SET last_message_at = ?, last_message_text = ? WHERE id = ?`,
			m.sentAt, preview(m.content), m.botID); err != nil {
			return fmt.Errorf("backfill list metadata for %s: %w", m.botID, err)
		}
	}

	// 4. Mark pre-existing bots read, so upgrading does not badge every row.
	//    This runs after step 3 because it reads last_message_at, and it is the
	//    one step here that needs a recorded marker rather than a data guard.
	if err := s.once("read_at_backfill", s.markExistingBotsRead); err != nil {
		return err
	}

	// 5. Settle sends interrupted by the last process death. This MUST precede
	//    step 6: the index it creates cannot exist while two awaiting turns are
	//    left over for one bot, and a crashed process could have left exactly
	//    that.
	if err := s.failInterruptedSends(); err != nil {
		return err
	}

	// 6. At most one awaiting turn per bot, enforced by the database rather than
	//    by remembering to check. This is what makes "one reply in flight per
	//    bot" an invariant instead of a convention, and with it the transcript
	//    cannot interleave — see the ordering DECISION on Message.
	if _, err := s.db.Exec(
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_messages_one_awaiting
		   ON messages(bot_id) WHERE status = 'awaiting'`); err != nil {
		return fmt.Errorf("create one-awaiting index: %w", err)
	}

	// 7. Every pre-calendar event joins the Personal calendar, through the SAME
	//    ensure every unqualified write uses. The guard is the '' the column was
	//    added with, so a database with no stragglers — a fresh one included —
	//    never conjures a Personal calendar just by being opened.
	var one int
	err = s.db.QueryRow(`SELECT 1 FROM events WHERE calendar_id = '' LIMIT 1`).Scan(&one)
	if err == nil {
		personal, err := s.EnsurePersonalCalendar()
		if err != nil {
			return fmt.Errorf("backfill event calendars: %w", err)
		}
		if _, err := s.db.Exec(`UPDATE events SET calendar_id = ? WHERE calendar_id = ''`, personal.ID); err != nil {
			return fmt.Errorf("backfill event calendars: %w", err)
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("backfill event calendars: %w", err)
	}
	return nil
}

// failInterruptedSends settles every turn left awaiting by a process that died
// mid-reply. Nothing is coming to complete them: the goroutine that would have
// appended the reply went with the process, so without this they wait forever.
// Retry works on them like any other stranded turn.
//
// SAFE ONLY AT STARTUP, which is why it is called from migrate and migrate is
// called only from Open. A process that has begun serving has live turns in
// flight and every one of them is awaiting — running this then would mark live
// work failed and then race the goroutine still about to write its reply. There
// is no way to tell a dead process's awaiting turn from a live one by looking at
// the row; the only thing that distinguishes them is that at startup there are
// no live ones yet.
func (s *Store) failInterruptedSends() error {
	const reason = "the server restarted while this message was being answered — retry to send it again"
	if _, err := s.db.Exec(`UPDATE messages SET status = ?, error = ? WHERE status = ?`,
		StatusFailed, reason, StatusAwaiting); err != nil {
		return fmt.Errorf("recover interrupted sends: %w", err)
	}
	return nil
}

// ErrNotFound is returned when a lookup by id has no row.
var ErrNotFound = errors.New("botnet: not found")

// ErrUnknownModel is returned when a write would persist a model id that the
// modelSelector roster does not resolve. Reads never raise it: a bot already
// holding a stale id stays listable and repairable via UpdateBot.
var ErrUnknownModel = errors.New("botnet: unknown model id")

// ErrInvalid is returned when a write is rejected on its own merits rather than
// for a missing row — an empty display name, say. The server maps it to 400.
var ErrInvalid = errors.New("botnet: invalid")

// ErrDuplicateName is returned when a write would take a name another row
// already holds case-insensitively. It is deliberately NOT an ErrInvalid: the
// value is well-formed and the caller fixes it by choosing another name, which
// is a 409 rather than a 400 about a malformed field.
var ErrDuplicateName = errors.New("botnet: that name is already taken")

// ErrBusy is returned when a bot already has a reply in flight. At most one
// awaiting turn per bot is a storage invariant rather than a convention, so a
// second send is refused rather than queued — see the ordering DECISION on
// Message for why refusing is what keeps the transcript in order.
var ErrBusy = errors.New("botnet: a reply is already in flight for this bot")

// ErrIDConflict is returned when a client-supplied message id already names a
// message of a DIFFERENT bot. Same bot is not an error — it is the idempotent
// replay AppendMessageAs exists for.
var ErrIDConflict = errors.New("botnet: message id already belongs to another bot")

// ErrVersionMismatch is returned when an update conditioned on a bot version
// (If-Match) finds the bot has been edited in between. The client refetches,
// re-decides, retries; the server maps it to 412.
var ErrVersionMismatch = errors.New("botnet: bot changed since the version the client holds")

// previewLimit caps the denormalized sidebar preview so the bot list stays small
// however long a message was.
const previewLimit = 200

// preview collapses a message to one line for the sidebar.
func preview(s string) string {
	r := []rune(strings.Join(strings.Fields(s), " "))
	if len(r) > previewLimit {
		return strings.TrimSpace(string(r[:previewLimit])) + "…"
	}
	return string(r)
}

// CreateNet inserts a new PrivateBotNet and returns it.
func (s *Store) CreateNet(name string) (PrivateBotNet, error) {
	net := PrivateBotNet{ID: newID("net_"), Name: name}
	if _, err := s.db.Exec(`INSERT INTO nets (id, name) VALUES (?, ?)`, net.ID, net.Name); err != nil {
		return PrivateBotNet{}, fmt.Errorf("create net: %w", err)
	}
	return net, nil
}

// CreateBot adds a bot to a net and returns it. CreatedAt is set here, an open
// segment 0 is opened for it, and the model is checked against the roster —
// this is the gate that keeps an unusable model id out of the database.
func (s *Store) CreateBot(netID, displayName, systemPrompt string, model modelselector.ModelID) (Bot, error) {
	if _, ok := modelselector.ByID(model); !ok {
		return Bot{}, fmt.Errorf("%w: %q", ErrUnknownModel, model)
	}
	bot := Bot{
		ID:           BotID(newID("bot_")),
		DisplayName:  displayName,
		CreatedAt:    time.Now().UTC(),
		SystemPrompt: systemPrompt,
		Model:        model,
		ModelValid:   true,
	}
	_, err := s.db.Exec(
		`INSERT INTO bots (id, net_id, display_name, created_at, system_prompt, model, last_message_at, last_message_text, read_at)
		 VALUES (?, ?, ?, ?, ?, ?, '', '', '')`,
		bot.ID, netID, bot.DisplayName, fmtTime(bot.CreatedAt), bot.SystemPrompt, bot.Model,
	)
	if err != nil {
		return Bot{}, fmt.Errorf("create bot: %w", err)
	}
	if _, err := ensureOpenSegment(s.db, bot.ID); err != nil {
		return Bot{}, err
	}
	bot.Version = botVersion(bot)
	return bot, nil
}

// BotPatch is the set of fields an update may change; a nil field is left alone.
// It is what makes a bot persisted with a since-removed model repairable.
type BotPatch struct {
	DisplayName  *string                `json:"displayName"`
	SystemPrompt *string                `json:"systemPrompt"`
	Model        *modelselector.ModelID `json:"model"`
	Memory       *string                `json:"memory"` // "" is a real value: it clears the memory
}

// UpdateBot applies a patch and returns the bot as stored. A model in the patch
// is validated the same way creation validates one; a model already in the row
// is not, so repairing a bot with a stale model never trips over the stale
// model itself.
//
// A non-empty ifVersion makes the update conditional: it must equal the bot's
// current Version or the patch fails with ErrVersionMismatch. The check and
// the write share a transaction, so two conditional edits cannot interleave.
// Empty means unconditional — the pre-If-Match behavior, unchanged.
func (s *Store) UpdateBot(id BotID, p BotPatch, ifVersion string) (Bot, error) {
	var bot Bot
	err := s.tx(func(q dbtx) error {
		var err error
		bot, err = getBot(q, id)
		if err != nil {
			return err
		}
		if ifVersion != "" && ifVersion != bot.Version {
			return fmt.Errorf("%w: it is now version %s", ErrVersionMismatch, bot.Version)
		}
		if p.DisplayName != nil {
			if strings.TrimSpace(*p.DisplayName) == "" {
				return fmt.Errorf("%w: displayName must not be empty", ErrInvalid)
			}
			bot.DisplayName = *p.DisplayName
		}
		if p.SystemPrompt != nil {
			bot.SystemPrompt = *p.SystemPrompt
		}
		if p.Model != nil {
			if _, ok := modelselector.ByID(*p.Model); !ok {
				return fmt.Errorf("%w: %q", ErrUnknownModel, *p.Model)
			}
			bot.Model = *p.Model
		}
		if p.Memory != nil {
			bot.Memory = *p.Memory
		}
		if _, err := q.Exec(`UPDATE bots SET display_name = ?, system_prompt = ?, model = ?, memory = ? WHERE id = ?`,
			bot.DisplayName, bot.SystemPrompt, bot.Model, bot.Memory, id); err != nil {
			return fmt.Errorf("update bot: %w", err)
		}
		return nil
	})
	if err != nil {
		return Bot{}, err
	}
	_, bot.ModelValid = modelselector.ByID(bot.Model)
	bot.Version = botVersion(bot)
	return bot, nil
}

// SetMemory replaces the bot's whole memory blob — the model's write path,
// used by the memory tool's replace and clear commands mid-turn. It is deliberately
// unconditional (no If-Match): a tool execution is one atomic store write, and
// user-vs-model memory writes are last-write-wins for now (OPEN in schema.go).
// The bots AFTER UPDATE trigger captures it into change_log like any other
// write, and it never moves the derived Version — memory is not an authored
// field, so it cannot 412 an edit in flight.
func (s *Store) SetMemory(id BotID, memory string) (Bot, error) {
	res, err := s.db.Exec(`UPDATE bots SET memory = ? WHERE id = ?`, memory, id)
	if err != nil {
		return Bot{}, fmt.Errorf("set memory: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return Bot{}, ErrNotFound
	}
	return s.GetBot(id)
}

// MarkRead marks the bot read up to its newest message — NOT as of now.
// Stamping the wall clock would swallow a message that lands between the client
// reading the transcript and this write, marking it read though nobody saw it.
// Copying last_message_at in one statement means the watermark can never move
// past a message that exists.
//
// A bot with no messages keeps a zero read_at; there is nothing to have read.
func (s *Store) MarkRead(id BotID) (Bot, error) {
	res, err := s.db.Exec(`UPDATE bots SET read_at = last_message_at WHERE id = ?`, id)
	if err != nil {
		return Bot{}, fmt.Errorf("mark read: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return Bot{}, ErrNotFound
	}
	return s.GetBot(id)
}

// AppendMessage records one message in a bot's conversation and returns it. The
// message joins the bot's open segment, and the bot's list metadata is refreshed
// so the sidebar never has to read a transcript to draw a row.
//
// Appending an awaiting turn claims the bot: it returns ErrBusy if a reply is
// already in flight. The check and the insert share a transaction so two
// concurrent sends cannot both pass it.
func (s *Store) AppendMessage(botID BotID, role, content string, status MessageStatus) (Message, error) {
	msg, _, err := s.append("", botID, role, content, status)
	return msg, err
}

// AppendMessageAs is AppendMessage under a client-supplied id — the idempotent
// send. If a message with that id already exists and belongs to botID, the
// stored row comes back unchanged with replayed true: a retry after a lost
// response is a no-op, per the transactional-idempotency discipline the design
// borrows from Replicache. The same id on a different bot is ErrIDConflict.
//
// The caller validates the id's shape first — this is client input reaching a
// primary key — and must not start a model turn for a replay.
func (s *Store) AppendMessageAs(id string, botID BotID, role, content string, status MessageStatus) (Message, bool, error) {
	return s.append(id, botID, role, content, status)
}

// append is the one write path for new messages. The replay lookup, the claim
// and the insert share a transaction, so two concurrent replays of one id
// cannot both insert. The replay check comes FIRST: replaying a send whose
// original is still awaiting must return that awaiting row, not trip over the
// bot being busy — with its own turn.
func (s *Store) append(id string, botID BotID, role, content string, status MessageStatus) (Message, bool, error) {
	var msg Message
	var replayed bool
	err := s.tx(func(q dbtx) error {
		if id != "" {
			existing, err := getMessage(q, id)
			if err == nil {
				if existing.BotID != botID {
					return fmt.Errorf("%w: %s", ErrIDConflict, id)
				}
				msg, replayed = existing, true
				return nil
			}
			if !errors.Is(err, ErrNotFound) {
				return err
			}
		}
		if status == StatusAwaiting {
			if err := claimBot(q, botID); err != nil {
				return err
			}
		}
		var err error
		msg, err = appendMessage(q, id, botID, role, content, status, nil, nil)
		return err
	})
	return msg, replayed, err
}

// CompleteTurn records a successful turn atomically: the reply is appended and
// the user turn settles in ONE transaction.
//
// Doing both at once is what keeps the transcript ordered. Settling the user
// turn first would free the bot a moment before the reply landed, letting a
// second send slip its user turn in ahead of the reply; appending the reply
// first would leave a window in which the reply exists beside a user turn still
// marked awaiting. One transaction has neither window.
func (s *Store) CompleteTurn(botID BotID, userMessageID, reply string, citations []Citation, toolCalls []ToolCall) (Message, error) {
	var msg Message
	err := s.tx(func(q dbtx) error {
		var err error
		if msg, err = appendMessage(q, "", botID, "bot", reply, StatusSent, citations, toolCalls); err != nil {
			return err
		}
		return setStatus(q, userMessageID, StatusSent, "")
	})
	return msg, err
}

// ClaimRetry flips a stranded user turn back to awaiting so it can be sent
// again, refusing with ErrBusy if the bot already has a reply in flight. Like
// AppendMessage it claims the bot inside the transaction that does the write.
func (s *Store) ClaimRetry(msg Message) error {
	return s.tx(func(q dbtx) error {
		if err := claimBot(q, msg.BotID); err != nil {
			return err
		}
		return setStatus(q, msg.ID, StatusAwaiting, "")
	})
}

// claimBot fails unless the bot has no reply in flight. The partial unique index
// on messages makes a second awaiting turn impossible even if a future caller
// forgets this check; the check exists so the common path reports ErrBusy rather
// than a raw constraint violation.
func claimBot(q dbtx, botID BotID) error {
	var one int
	err := q.QueryRow(`SELECT 1 FROM messages WHERE bot_id = ? AND status = ? LIMIT 1`,
		botID, StatusAwaiting).Scan(&one)
	if err == nil {
		return ErrBusy
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("check in-flight reply: %w", err)
	}
	return nil
}

// appendMessage inserts one message row; an empty id mints one. Citations and
// tool_calls are stored as JSON arrays, ” when there are none — only bot
// replies that searched or called a tool carry any. Both ride this one INSERT,
// so the change_log triggers capture them with no separate write path.
func appendMessage(q dbtx, id string, botID BotID, role, content string, status MessageStatus, citations []Citation, toolCalls []ToolCall) (Message, error) {
	seg, err := ensureOpenSegment(q, botID)
	if err != nil {
		return Message{}, err
	}
	if id == "" {
		id = newID("msg_")
	}
	msg := Message{
		ID:        id,
		BotID:     botID,
		SegmentID: seg.ID,
		Role:      role,
		Content:   content,
		SentAt:    time.Now().UTC(),
		Status:    status,
		Citations: citations,
		ToolCalls: toolCalls,
	}
	citeJSON := ""
	if len(citations) > 0 {
		encoded, err := json.Marshal(citations)
		if err != nil {
			return Message{}, fmt.Errorf("encode citations: %w", err)
		}
		citeJSON = string(encoded)
	}
	toolJSON := ""
	if len(toolCalls) > 0 {
		encoded, err := json.Marshal(toolCalls)
		if err != nil {
			return Message{}, fmt.Errorf("encode tool_calls: %w", err)
		}
		toolJSON = string(encoded)
	}
	if _, err := q.Exec(
		`INSERT INTO messages (id, bot_id, segment_id, role, content, sent_at, status, error, citations, tool_calls)
		 VALUES (?, ?, ?, ?, ?, ?, ?, '', ?, ?)`,
		msg.ID, msg.BotID, msg.SegmentID, msg.Role, msg.Content, fmtTime(msg.SentAt), msg.Status, citeJSON, toolJSON,
	); err != nil {
		return Message{}, fmt.Errorf("append message: %w", err)
	}
	if _, err := q.Exec(`UPDATE bots SET last_message_at = ?, last_message_text = ? WHERE id = ?`,
		fmtTime(msg.SentAt), preview(msg.Content), botID); err != nil {
		return Message{}, fmt.Errorf("touch bot: %w", err)
	}
	return msg, nil
}

// SetMessageStatus settles a message: "sent" once its reply landed, "failed"
// with the reason when the model call did not. Use ClaimRetry to move one back
// to awaiting — that direction has to claim the bot first.
func (s *Store) SetMessageStatus(id string, status MessageStatus, errText string) error {
	return setStatus(s.db, id, status, errText)
}

func setStatus(q dbtx, id string, status MessageStatus, errText string) error {
	if status != StatusFailed {
		errText = ""
	}
	res, err := q.Exec(`UPDATE messages SET status = ?, error = ? WHERE id = ?`, status, errText, id)
	if err != nil {
		return fmt.Errorf("set message status: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ErrNotFound
	}
	return nil
}

const messageColumns = `id, bot_id, segment_id, role, content, sent_at, status, error, citations, tool_calls`

func scanMessage(sc interface{ Scan(...any) error }) (Message, error) {
	var m Message
	var sentAt, citations, toolCalls string
	if err := sc.Scan(&m.ID, &m.BotID, &m.SegmentID, &m.Role, &m.Content, &sentAt, &m.Status, &m.Error, &citations, &toolCalls); err != nil {
		return Message{}, err
	}
	var err error
	if m.SentAt, err = parseTime(sentAt); err != nil {
		return Message{}, fmt.Errorf("parse sent_at: %w", err)
	}
	if citations != "" {
		if err := json.Unmarshal([]byte(citations), &m.Citations); err != nil {
			return Message{}, fmt.Errorf("parse citations: %w", err)
		}
	}
	if toolCalls != "" {
		if err := json.Unmarshal([]byte(toolCalls), &m.ToolCalls); err != nil {
			return Message{}, fmt.Errorf("parse tool_calls: %w", err)
		}
	}
	return m, nil
}

// GetMessage loads one message by id.
func (s *Store) GetMessage(id string) (Message, error) { return getMessage(s.db, id) }

func getMessage(q dbtx, id string) (Message, error) {
	m, err := scanMessage(q.QueryRow(`SELECT `+messageColumns+` FROM messages WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Message{}, ErrNotFound
	}
	if err != nil {
		return Message{}, fmt.Errorf("get message: %w", err)
	}
	return m, nil
}

// InFlight returns the bot's awaiting turn — the one holding its single reply
// slot — or ErrNotFound if the bot is free. The one-awaiting-per-bot index is
// what makes "the" the right article here.
func (s *Store) InFlight(botID BotID) (Message, error) {
	m, err := scanMessage(s.db.QueryRow(
		`SELECT `+messageColumns+` FROM messages WHERE bot_id = ? AND status = ? LIMIT 1`,
		botID, StatusAwaiting))
	if errors.Is(err, sql.ErrNoRows) {
		return Message{}, ErrNotFound
	}
	if err != nil {
		return Message{}, fmt.Errorf("in-flight message: %w", err)
	}
	return m, nil
}

// Conversation returns a bot's FULL transcript in insertion order (by rowid,
// reliable even when several messages land in the same millisecond), across
// every segment. Compaction changes what is sent to the model, never what is
// readable here.
func (s *Store) Conversation(botID BotID) ([]Message, error) {
	return s.messages(`WHERE bot_id = ? ORDER BY rowid`, botID)
}

// MessagesAfter returns only the messages that follow the given one — the poll
// cursor a client uses to pick up a reply without refetching the transcript.
//
// "After" is defined by rowid, exactly as Conversation orders, so it means the
// same thing as the transcript's own order rather than an id comparison that
// could disagree for two ids minted in the same millisecond. The cursor is a
// message id, which is also what an event stream would carry, so replacing
// polling with server-sent events later needs no change to this shape.
func (s *Store) MessagesAfter(botID BotID, afterID string) ([]Message, error) {
	var rowid int64
	err := s.db.QueryRow(`SELECT rowid FROM messages WHERE id = ? AND bot_id = ?`, afterID, botID).Scan(&rowid)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("locate cursor %q: %w", afterID, err)
	}
	return s.messages(`WHERE bot_id = ? AND rowid > ? ORDER BY rowid`, botID, rowid)
}

// SegmentMessages returns one segment's raw messages in order.
func (s *Store) SegmentMessages(segID SegmentID) ([]Message, error) {
	return s.messages(`WHERE segment_id = ? ORDER BY rowid`, segID)
}

func (s *Store) messages(where string, args ...any) ([]Message, error) {
	rows, err := s.db.Query(`SELECT `+messageColumns+` FROM messages `+where, args...)
	if err != nil {
		return nil, fmt.Errorf("messages: %w", err)
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, fmt.Errorf("scan message: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ── Segments ──────────────────────────────────────────────────────────────────

const segmentColumns = `id, bot_id, idx, opened_at, sealed_at, summary`

func scanSegment(sc interface{ Scan(...any) error }) (Segment, error) {
	var seg Segment
	var openedAt, sealedAt string
	if err := sc.Scan(&seg.ID, &seg.BotID, &seg.Index, &openedAt, &sealedAt, &seg.Summary); err != nil {
		return Segment{}, err
	}
	var err error
	if seg.OpenedAt, err = parseTime(openedAt); err != nil {
		return Segment{}, fmt.Errorf("parse opened_at: %w", err)
	}
	if seg.SealedAt, err = parseTime(sealedAt); err != nil {
		return Segment{}, fmt.Errorf("parse sealed_at: %w", err)
	}
	return seg, nil
}

// Segments returns a bot's whole chain, oldest first.
func (s *Store) Segments(botID BotID) ([]Segment, error) {
	rows, err := s.db.Query(`SELECT `+segmentColumns+` FROM segments WHERE bot_id = ? ORDER BY idx`, botID)
	if err != nil {
		return nil, fmt.Errorf("segments: %w", err)
	}
	defer rows.Close()
	var out []Segment
	for rows.Next() {
		seg, err := scanSegment(rows)
		if err != nil {
			return nil, fmt.Errorf("scan segment: %w", err)
		}
		out = append(out, seg)
	}
	return out, rows.Err()
}

// OpenSegment returns the segment new messages append to — the one with a zero
// SealedAt — opening one if the bot somehow has none.
func (s *Store) OpenSegment(botID BotID) (Segment, error) { return ensureOpenSegment(s.db, botID) }

func ensureOpenSegment(q dbtx, botID BotID) (Segment, error) {
	seg, err := scanSegment(q.QueryRow(
		`SELECT `+segmentColumns+` FROM segments WHERE bot_id = ? AND sealed_at = '' ORDER BY idx DESC LIMIT 1`, botID))
	if err == nil {
		return seg, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Segment{}, fmt.Errorf("open segment: %w", err)
	}
	var next int
	if err := q.QueryRow(`SELECT COALESCE(MAX(idx) + 1, 0) FROM segments WHERE bot_id = ?`, botID).Scan(&next); err != nil {
		return Segment{}, fmt.Errorf("next segment index: %w", err)
	}
	return openSegment(q, botID, next)
}

func openSegment(q dbtx, botID BotID, index int) (Segment, error) {
	seg := Segment{ID: SegmentID(newID("seg_")), BotID: botID, Index: index, OpenedAt: time.Now().UTC()}
	if _, err := q.Exec(
		`INSERT INTO segments (id, bot_id, idx, opened_at, sealed_at, summary) VALUES (?, ?, ?, ?, '', '')`,
		seg.ID, seg.BotID, seg.Index, fmtTime(seg.OpenedAt)); err != nil {
		return Segment{}, fmt.Errorf("open segment %d: %w", index, err)
	}
	return seg, nil
}

// LatestSummary returns the newest sealed segment's cumulative summary — the
// ONE summary that goes to the model — or "" if the bot has never been
// compacted. Older summaries stay in the chain for the UI and are never read
// here, which is what keeps the prompt constant-size.
func (s *Store) LatestSummary(botID BotID) (string, error) {
	var summary string
	err := s.db.QueryRow(
		`SELECT summary FROM segments WHERE bot_id = ? AND sealed_at != '' ORDER BY idx DESC LIMIT 1`, botID).
		Scan(&summary)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("latest summary: %w", err)
	}
	return summary, nil
}

// Seal closes a segment with its cumulative summary and opens the next one in
// the same transaction, so a bot is never left with no open segment. Messages
// are untouched: the sealed segment keeps every row it had.
func (s *Store) Seal(seg Segment, summary string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("seal: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE segments SET sealed_at = ?, summary = ? WHERE id = ? AND sealed_at = ''`,
		fmtTime(time.Now().UTC()), summary, seg.ID); err != nil {
		return fmt.Errorf("seal segment: %w", err)
	}
	if _, err := tx.Exec(
		`INSERT INTO segments (id, bot_id, idx, opened_at, sealed_at, summary) VALUES (?, ?, ?, ?, '', '')`,
		newID("seg_"), seg.BotID, seg.Index+1, fmtTime(time.Now().UTC())); err != nil {
		return fmt.Errorf("open next segment: %w", err)
	}
	return tx.Commit()
}

// DeleteBot removes a bot, its entire conversation and its segment chain.
func (s *Store) DeleteBot(id BotID) error {
	// The projects this bot owned lose their owner explicitly rather than being
	// left with a pointer at a thread that no longer exists. One statement, and
	// SQLite's row triggers fire per row, so the change feed carries a real
	// project-updated row for each — the DeleteProject cascade's shape exactly.
	if _, err := s.db.Exec(`UPDATE projects SET owner_bot_id = '', updated_at = ? WHERE owner_bot_id = ?`,
		fmtEventTime(time.Now().UTC().Truncate(time.Second)), id); err != nil {
		return fmt.Errorf("clear the projects this bot owned: %w", err)
	}
	if _, err := s.db.Exec(`DELETE FROM messages WHERE bot_id = ?`, id); err != nil {
		return fmt.Errorf("delete bot messages: %w", err)
	}
	if _, err := s.db.Exec(`DELETE FROM segments WHERE bot_id = ?`, id); err != nil {
		return fmt.Errorf("delete bot segments: %w", err)
	}
	if _, err := s.db.Exec(`DELETE FROM bots WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delete bot: %w", err)
	}
	return nil
}

const botColumns = `id, display_name, created_at, system_prompt, model, memory, last_message_at, last_message_text, read_at`

func scanBot(sc interface{ Scan(...any) error }) (Bot, error) {
	var b Bot
	var createdAt, lastAt, readAt string
	if err := sc.Scan(&b.ID, &b.DisplayName, &createdAt, &b.SystemPrompt, &b.Model,
		&b.Memory, &lastAt, &b.LastMessageText, &readAt); err != nil {
		return Bot{}, err
	}
	var err error
	if b.CreatedAt, err = parseTime(createdAt); err != nil {
		return Bot{}, fmt.Errorf("parse created_at: %w", err)
	}
	if b.LastMessageAt, err = parseTime(lastAt); err != nil {
		return Bot{}, fmt.Errorf("parse last_message_at: %w", err)
	}
	if b.ReadAt, err = parseTime(readAt); err != nil {
		return Bot{}, fmt.Errorf("parse read_at: %w", err)
	}
	// Derived, never stored: a roster change must not need a data migration,
	// and an edit's version moves the moment the row does.
	_, b.ModelValid = modelselector.ByID(b.Model)
	b.Version = botVersion(b)
	return b, nil
}

// botVersion derives the opaque edit version If-Match compares against: a
// hash over the authored fields only, so message traffic (which touches the
// row's list metadata constantly) can never invalidate an edit in progress.
// Memory is deliberately excluded for the same reason: the model writes it
// mid-chat via the memory tools, and that must never 412 a user's edit.
func botVersion(b Bot) string {
	h := sha256.Sum256([]byte(b.DisplayName + "\x00" + b.SystemPrompt + "\x00" + string(b.Model)))
	return "v" + hex.EncodeToString(h[:8])
}

// ListBots returns every bot in a net, most recently active first. A bot with no
// messages yet sorts by its creation time, so a just-created bot appears at the
// top rather than the bottom.
func (s *Store) ListBots(netID string) ([]Bot, error) {
	rows, err := s.db.Query(`SELECT `+botColumns+` FROM bots WHERE net_id = ?
		 ORDER BY COALESCE(NULLIF(last_message_at, ''), created_at) DESC, rowid DESC`, netID)
	if err != nil {
		return nil, fmt.Errorf("list bots: %w", err)
	}
	defer rows.Close()
	var out []Bot
	for rows.Next() {
		b, err := scanBot(rows)
		if err != nil {
			return nil, fmt.Errorf("scan bot: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// AllBots returns every bot in the database, in display-name order. It is the
// roster the project tool resolves an owner NAME against and lists back when
// the name matches none or several — a net-less read, because the tool has a
// calling bot rather than a net, and the MVP has exactly one net anyway.
func (s *Store) AllBots() ([]Bot, error) {
	rows, err := s.db.Query(`SELECT ` + botColumns + ` FROM bots ORDER BY display_name COLLATE NOCASE, rowid`)
	if err != nil {
		return nil, fmt.Errorf("list every bot: %w", err)
	}
	defer rows.Close()
	var out []Bot
	for rows.Next() {
		b, err := scanBot(rows)
		if err != nil {
			return nil, fmt.Errorf("scan bot: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// EnsureDefaultNet returns the first net, creating a "default" one if none
// exists. The single-user MVP has exactly one net; this gives bots a home
// without the UI having to manage nets yet.
func (s *Store) EnsureDefaultNet() (PrivateBotNet, error) {
	var id string
	err := s.db.QueryRow(`SELECT id FROM nets ORDER BY id LIMIT 1`).Scan(&id)
	if err == nil {
		return s.GetNet(id)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return PrivateBotNet{}, fmt.Errorf("ensure default net: %w", err)
	}
	return s.CreateNet("default")
}

// GetBot loads one bot by id. No model validation happens here — a bot holding a
// stale model id must stay readable so it can be repaired.
func (s *Store) GetBot(id BotID) (Bot, error) { return getBot(s.db, id) }

func getBot(q dbtx, id BotID) (Bot, error) {
	b, err := scanBot(q.QueryRow(`SELECT `+botColumns+` FROM bots WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Bot{}, ErrNotFound
	}
	if err != nil {
		return Bot{}, fmt.Errorf("get bot: %w", err)
	}
	return b, nil
}

// ── Calendars ─────────────────────────────────────────────────────────────────
// Named calendars partition the events table. Everything here is an ordinary
// INSERT/UPDATE/DELETE, so the chg_calendar_* triggers capture every write with
// no Go code remembering to. Timestamps use the events table's fixed-width
// format: ListCalendars orders by created_at as TEXT, and only fixed width
// makes that agree with chronology — the same lesson the events table carries.

const calendarColumns = `id, name, color, created_by, created_at, updated_at, executable`

// calendarColors is the color enum, in the order create cycles through when
// the caller names no color.
var calendarColors = []string{"blue", "green", "orange", "purple", "red", "teal"}

// personalCalendarName is the name the default-calendar ensure keys on — see
// the Calendar DECISION in schema.go: an ensure by name, not a flag.
const personalCalendarName = "Personal"

func scanCalendar(sc interface{ Scan(...any) error }) (Calendar, error) {
	var c Calendar
	var createdAt, updatedAt string
	var executable int
	if err := sc.Scan(&c.ID, &c.Name, &c.Color, &c.CreatedBy, &createdAt, &updatedAt, &executable); err != nil {
		return Calendar{}, err
	}
	c.Executable = executable != 0
	var err error
	if c.CreatedAt, err = parseEventTime(createdAt); err != nil {
		return Calendar{}, fmt.Errorf("parse created_at: %w", err)
	}
	if c.UpdatedAt, err = parseEventTime(updatedAt); err != nil {
		return Calendar{}, fmt.Errorf("parse updated_at: %w", err)
	}
	return c, nil
}

// validateCalendar is the ONE place a calendar's own rules live, so the REST
// handlers and the tool commands — which all come through the store methods
// below — cannot end up enforcing different ones. It runs on the calendar as
// it would be STORED, name already trimmed and color already assigned.
func validateCalendar(c Calendar) error {
	if c.Name == "" {
		return fmt.Errorf("%w: name must not be empty", ErrInvalid)
	}
	if len([]rune(c.Name)) > 64 {
		return fmt.Errorf("%w: name must be at most 64 characters", ErrInvalid)
	}
	valid := false
	for _, v := range calendarColors {
		valid = valid || v == c.Color
	}
	if !valid {
		return fmt.Errorf("%w: color %q is not one of %s", ErrInvalid, c.Color, strings.Join(calendarColors, ", "))
	}
	return nil
}

// CreateCalendar stores one calendar and returns it as stored. The id, the
// author and both timestamps are stamped here, exactly as CreateEvent stamps
// an event's. An empty color is assigned by cycling calendarColors on the
// count of existing calendars, so a run of unnamed creates comes out visually
// distinct rather than uniformly blue. executable marks the calendar whose
// events may fire automations (see the Calendar DECISION); it is authored on
// create like name and color, and flippable later via UpdateCalendar.
func (s *Store) CreateCalendar(name, color, createdBy string, executable bool) (Calendar, error) {
	var cal Calendar
	err := s.tx(func(q dbtx) error {
		var err error
		cal, err = createCalendar(q, name, color, createdBy, executable)
		return err
	})
	if err != nil {
		return Calendar{}, err
	}
	return cal, nil
}

// createCalendar is the one insert path, shared by CreateCalendar and the
// Personal ensure. It runs inside the caller's transaction so the dup-name
// check and the insert cannot interleave; the NOCASE unique index is the
// structural backstop, and the check exists so a duplicate reports ErrInvalid
// rather than a raw constraint violation.
func createCalendar(q dbtx, name, color, createdBy string, executable bool) (Calendar, error) {
	now := time.Now().UTC().Truncate(time.Second)
	cal := Calendar{
		ID:         CalendarID(newID("cal_")),
		Name:       strings.TrimSpace(name),
		Color:      color,
		CreatedBy:  createdBy,
		CreatedAt:  now,
		UpdatedAt:  now,
		Executable: executable,
	}
	if cal.Color == "" {
		var n int
		if err := q.QueryRow(`SELECT COUNT(*) FROM calendars`).Scan(&n); err != nil {
			return Calendar{}, fmt.Errorf("count calendars: %w", err)
		}
		cal.Color = calendarColors[n%len(calendarColors)]
	}
	if err := validateCalendar(cal); err != nil {
		return Calendar{}, err
	}
	if existing, err := calendarByName(q, cal.Name); err == nil {
		return Calendar{}, fmt.Errorf("%w: a calendar named %q already exists", ErrInvalid, existing.Name)
	} else if !errors.Is(err, ErrNotFound) {
		return Calendar{}, err
	}
	if _, err := q.Exec(
		`INSERT INTO calendars (`+calendarColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		cal.ID, cal.Name, cal.Color, cal.CreatedBy,
		fmtEventTime(cal.CreatedAt), fmtEventTime(cal.UpdatedAt), boolInt(cal.Executable)); err != nil {
		return Calendar{}, fmt.Errorf("create calendar: %w", err)
	}
	return cal, nil
}

// boolInt is SQLite's boolean: the executable column stores 0/1.
func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// GetCalendar loads one calendar by id.
func (s *Store) GetCalendar(id CalendarID) (Calendar, error) { return getCalendar(s.db, id) }

func getCalendar(q dbtx, id CalendarID) (Calendar, error) {
	c, err := scanCalendar(q.QueryRow(`SELECT `+calendarColumns+` FROM calendars WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Calendar{}, ErrNotFound
	}
	if err != nil {
		return Calendar{}, fmt.Errorf("get calendar: %w", err)
	}
	return c, nil
}

// CalendarByName finds a calendar by its name, case-insensitively — how the
// calendar tool resolves the name a model typed, and how the Personal ensure
// decides whether it has anything to do.
func (s *Store) CalendarByName(name string) (Calendar, error) { return calendarByName(s.db, name) }

func calendarByName(q dbtx, name string) (Calendar, error) {
	c, err := scanCalendar(q.QueryRow(
		`SELECT `+calendarColumns+` FROM calendars WHERE name = ? COLLATE NOCASE`,
		strings.TrimSpace(name)))
	if errors.Is(err, sql.ErrNoRows) {
		return Calendar{}, ErrNotFound
	}
	if err != nil {
		return Calendar{}, fmt.Errorf("calendar by name: %w", err)
	}
	return c, nil
}

// EnsurePersonalCalendar returns the calendar named "Personal"
// (case-insensitively), creating it — color "blue", createdBy "user" — if it
// does not exist. Idempotent by construction: the lookup and the create share
// a transaction, so two ensures cannot both insert. Every write path that
// needs a calendar and was given none comes here, the migration backfill
// included, which is what makes deleting "Personal" legal — it self-heals on
// the next unqualified write.
func (s *Store) EnsurePersonalCalendar() (Calendar, error) {
	var cal Calendar
	err := s.tx(func(q dbtx) error {
		var err error
		cal, err = ensurePersonalCalendar(q)
		return err
	})
	if err != nil {
		return Calendar{}, err
	}
	return cal, nil
}

func ensurePersonalCalendar(q dbtx) (Calendar, error) {
	return ensureCalendar(q, personalCalendarName, "blue")
}

// ensureCalendar is the name-keyed ensure both defaults share: look the name up
// case-insensitively, create it (non-executable, authored by "user") if it is
// missing. Inside the caller's transaction, so two ensures cannot both insert.
// An empty color is assigned by cycling, like any other unnamed create.
func ensureCalendar(q dbtx, name, color string) (Calendar, error) {
	cal, err := calendarByName(q, name)
	if err == nil {
		return cal, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return Calendar{}, err
	}
	return createCalendar(q, name, color, userAuthor, false)
}

// CalendarPatch is the set of fields an update may change; a nil field is left
// alone. No version to condition on — calendars are last-write-wins, the same
// DECISION events carry.
type CalendarPatch struct {
	Name       *string `json:"name"`
	Color      *string `json:"color"`
	Executable *bool   `json:"executable"` // flips whether its events may fire automations
}

// UpdateCalendar applies a partial patch and returns the calendar as stored.
// The read, the dup-name check and the write share a transaction, for the same
// reason UpdateEvent's do: last-write-wins is about which value survives, not
// about letting a concurrent patch clobber a field it never named.
func (s *Store) UpdateCalendar(id CalendarID, p CalendarPatch) (Calendar, error) {
	var cal Calendar
	err := s.tx(func(q dbtx) error {
		var err error
		if cal, err = getCalendar(q, id); err != nil {
			return err
		}
		if p.Name != nil {
			cal.Name = strings.TrimSpace(*p.Name)
		}
		if p.Color != nil {
			cal.Color = *p.Color
		}
		if p.Executable != nil {
			cal.Executable = *p.Executable
		}
		if err := validateCalendar(cal); err != nil {
			return err
		}
		if existing, err := calendarByName(q, cal.Name); err == nil && existing.ID != id {
			return fmt.Errorf("%w: a calendar named %q already exists", ErrInvalid, existing.Name)
		} else if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		cal.UpdatedAt = time.Now().UTC().Truncate(time.Second)
		if _, err := q.Exec(`UPDATE calendars SET name = ?, color = ?, executable = ?, updated_at = ? WHERE id = ?`,
			cal.Name, cal.Color, boolInt(cal.Executable), fmtEventTime(cal.UpdatedAt), id); err != nil {
			return fmt.Errorf("update calendar: %w", err)
		}
		return nil
	})
	if err != nil {
		return Calendar{}, err
	}
	return cal, nil
}

// DeleteCalendar removes a calendar AND its events, in one transaction — the
// cascade behind REST DELETE /v1/calendars/{id}, which the UI confirms before
// calling (the tool's delete_calendar refuses instead; see the DECISION on
// Calendar). The events go through an explicit DELETE on their own table, and
// SQLite row triggers fire once per deleted row, so a sync client gets a real
// chg_event tombstone for every event alongside the calendar's own — exactly
// as DeleteBot tombstones a bot's messages.
func (s *Store) DeleteCalendar(id CalendarID) error {
	return s.tx(func(q dbtx) error {
		if _, err := getCalendar(q, id); err != nil {
			return err
		}
		if _, err := q.Exec(`DELETE FROM events WHERE calendar_id = ?`, id); err != nil {
			return fmt.Errorf("delete calendar events: %w", err)
		}
		if _, err := q.Exec(`DELETE FROM calendars WHERE id = ?`, id); err != nil {
			return fmt.Errorf("delete calendar: %w", err)
		}
		return nil
	})
}

// ListCalendars returns every calendar, oldest first. Ascending createdAt is a
// stable order for the UI's chip row — a new calendar appends rather than
// reshuffling the ones the user knows the position of. rowid breaks the tie
// two same-second creates leave in the truncated timestamp: insertion order,
// which two ULIDs minted in the same millisecond cannot promise.
func (s *Store) ListCalendars() ([]Calendar, error) {
	rows, err := s.db.Query(`SELECT ` + calendarColumns + ` FROM calendars ORDER BY created_at, rowid`)
	if err != nil {
		return nil, fmt.Errorf("list calendars: %w", err)
	}
	defer rows.Close()
	var out []Calendar
	for rows.Next() {
		c, err := scanCalendar(rows)
		if err != nil {
			return nil, fmt.Errorf("scan calendar: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// EventCount reports how many events a calendar holds — what the tool's
// delete_calendar refusal and list_calendars rendering are built from.
func (s *Store) EventCount(id CalendarID) (int, error) {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM events WHERE calendar_id = ?`, id).Scan(&n); err != nil {
		return 0, fmt.Errorf("count events: %w", err)
	}
	return n, nil
}

// resolveCalendarID verifies a caller-named calendar id exists, mapping a
// missing one to ErrInvalid: the caller named a bad CALENDAR, which is that
// write's 400, not a 404 about the event it was writing.
func resolveCalendarID(q dbtx, id CalendarID) error {
	_, err := getCalendar(q, id)
	if errors.Is(err, ErrNotFound) {
		return fmt.Errorf("%w: no calendar %q — GET /v1/calendars lists them", ErrInvalid, id)
	}
	return err
}

// ── Events ────────────────────────────────────────────────────────────────────
// The calendar service. Events belong to the net rather than to a bot, so the
// REST path (the user's Calendar panel) and the calendar tool (a bot, mid-turn)
// are two writers on ONE table — which is the whole point of a service. Every
// write here is an ordinary INSERT/UPDATE/DELETE, so the chg_event_* triggers
// capture it into change_log with no Go code remembering to.

const eventColumns = `id, calendar_id, title, starts_at, ends_at, location, notes, created_by, created_at, updated_at, rrule, tz, automation`

func scanEvent(sc interface{ Scan(...any) error }) (Event, error) {
	var e Event
	var startsAt, endsAt, createdAt, updatedAt string
	if err := sc.Scan(&e.ID, &e.CalendarID, &e.Title, &startsAt, &endsAt, &e.Location, &e.Notes,
		&e.CreatedBy, &createdAt, &updatedAt, &e.RRule, &e.TZ, &e.Automation); err != nil {
		return Event{}, err
	}
	for _, f := range []struct {
		raw  string
		name string
		into *time.Time
	}{
		{startsAt, "starts_at", &e.StartsAt},
		{endsAt, "ends_at", &e.EndsAt},
		{createdAt, "created_at", &e.CreatedAt},
		{updatedAt, "updated_at", &e.UpdatedAt},
	} {
		t, err := parseEventTime(f.raw)
		if err != nil {
			return Event{}, fmt.Errorf("parse %s: %w", f.name, err)
		}
		*f.into = t
	}
	return e, nil
}

// EventPatch is the set of fields an update may change; a nil field is left
// alone. There is no version to condition on — event edits are last-write-wins
// (the no-If-Match DECISION on Event) — and neither CreatedBy nor the
// timestamps are patchable, because they are the write path's to stamp.
type EventPatch struct {
	Title      *string     `json:"title"`
	StartsAt   *time.Time  `json:"startsAt"`
	EndsAt     *time.Time  `json:"endsAt"`
	Location   *string     `json:"location"`
	Notes      *string     `json:"notes"`
	CalendarID *CalendarID `json:"calendarId"` // moves the event; must resolve
	RRule      *string     `json:"rrule"`      // "" clears: the event becomes single again
	TZ         *string     `json:"tz"`
	Automation *string     `json:"automation"` // "" clears: the event stops firing
}

// validateEvent is the ONE place an event's own rules live, so the REST handler
// and the bot's calendar tool cannot end up enforcing different ones. It runs
// on the event as it would be STORED, so a patch is checked against the merged
// result rather than against the fields it happened to carry.
func validateEvent(e Event) error {
	if strings.TrimSpace(e.Title) == "" {
		return fmt.Errorf("%w: title must not be empty", ErrInvalid)
	}
	if e.StartsAt.IsZero() || e.EndsAt.IsZero() {
		return fmt.Errorf("%w: startsAt and endsAt are required", ErrInvalid)
	}
	if e.EndsAt.Before(e.StartsAt) {
		return fmt.Errorf("%w: endsAt %s precedes startsAt %s",
			ErrInvalid, fmtEventTime(e.EndsAt), fmtEventTime(e.StartsAt))
	}
	if e.RRule != "" {
		if e.TZ == "" {
			return fmt.Errorf("%w: a recurring event needs a tz (an IANA id like \"America/New_York\") — "+
				"the rrule's wall clock has no meaning without one", ErrInvalid)
		}
		if _, err := parseRRULE(e.RRule); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalid, err)
		}
	}
	if e.TZ != "" {
		if _, err := time.LoadLocation(e.TZ); err != nil {
			return fmt.Errorf("%w: unknown tz %q (want an IANA id like \"America/New_York\")", ErrInvalid, e.TZ)
		}
	}
	return nil
}

// requireExecutable enforces the automation DECISION (see Event in schema.go)
// inside the caller's transaction: an event naming an automation must sit on
// an executable calendar — a lunch cannot fire a fetcher. It runs on the
// event as it would be STORED, after the calendar is resolved, so a patch
// that moves a firing event to a plain calendar is caught the same as a
// create that starts there. The error names the calendar and the fix.
func requireExecutable(q dbtx, e Event) error {
	if e.Automation == "" {
		return nil
	}
	cal, err := getCalendar(q, e.CalendarID)
	if err != nil {
		return err
	}
	if !cal.Executable {
		return fmt.Errorf("%w: automation %q is only allowed on an executable calendar, and %q is not — "+
			"make it executable first, or use one that is", ErrInvalid, e.Automation, cal.Name)
	}
	return nil
}

// CreateEvent stores one event and returns it as stored. The caller authors
// title, times, location, notes and (optionally) the calendar; the id, the
// author and both timestamps are stamped HERE, so an event can never claim an
// author or a creation time it did not have. createdBy is a BotID for a tool
// write and "user" for a UI one. A zero CalendarID gets the Personal ensure; a
// named one must resolve or the write is ErrInvalid — either way no event row
// can dangle. The resolution and the insert share a transaction, so the
// calendar cannot be deleted out from under the row between the two.
func (s *Store) CreateEvent(e Event, createdBy string) (Event, error) {
	now := time.Now().UTC().Truncate(time.Second)
	e.ID = EventID(newID("evt_"))
	e.CreatedBy = createdBy
	e.CreatedAt, e.UpdatedAt = now, now
	e.StartsAt = e.StartsAt.UTC().Truncate(time.Second)
	e.EndsAt = e.EndsAt.UTC().Truncate(time.Second)
	if err := validateEvent(e); err != nil {
		return Event{}, err
	}
	err := s.tx(func(q dbtx) error {
		if e.CalendarID == "" {
			cal, err := ensurePersonalCalendar(q)
			if err != nil {
				return err
			}
			e.CalendarID = cal.ID
		} else if err := resolveCalendarID(q, e.CalendarID); err != nil {
			return err
		}
		if err := requireExecutable(q, e); err != nil {
			return err
		}
		return insertEventRow(q, e)
	})
	if err != nil {
		return Event{}, err
	}
	return e, nil
}

// GetEvent loads one event by id.
func (s *Store) GetEvent(id EventID) (Event, error) { return getEvent(s.db, id) }

func getEvent(q dbtx, id EventID) (Event, error) {
	e, err := scanEvent(q.QueryRow(`SELECT `+eventColumns+` FROM events WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Event{}, ErrNotFound
	}
	if err != nil {
		return Event{}, fmt.Errorf("get event: %w", err)
	}
	return e, nil
}

// UpdateEvent applies a partial patch and returns the event as stored. The read
// and the write share a transaction so a concurrent patch cannot be merged into
// a stale copy — last-write-wins is about which VALUE survives, not about
// letting one writer lose a field it never touched.
func (s *Store) UpdateEvent(id EventID, p EventPatch) (Event, error) {
	var e Event
	err := s.tx(func(q dbtx) error {
		var err error
		if e, err = getEvent(q, id); err != nil {
			return err
		}
		if p.Title != nil {
			e.Title = *p.Title
		}
		if p.StartsAt != nil {
			e.StartsAt = p.StartsAt.UTC().Truncate(time.Second)
		}
		if p.EndsAt != nil {
			e.EndsAt = p.EndsAt.UTC().Truncate(time.Second)
		}
		if p.Location != nil {
			e.Location = *p.Location
		}
		if p.Notes != nil {
			e.Notes = *p.Notes
		}
		if p.CalendarID != nil {
			if err := resolveCalendarID(q, *p.CalendarID); err != nil {
				return err
			}
			e.CalendarID = *p.CalendarID
		}
		if p.RRule != nil {
			e.RRule = *p.RRule
		}
		if p.TZ != nil {
			e.TZ = *p.TZ
		}
		if p.Automation != nil {
			e.Automation = *p.Automation
		}
		if err := validateEvent(e); err != nil {
			return err
		}
		// The merged event, not the patch: a calendar move under a live
		// automation is exactly what this must catch.
		if err := requireExecutable(q, e); err != nil {
			return err
		}
		e.UpdatedAt = time.Now().UTC().Truncate(time.Second)
		return updateEventRow(q, e)
	})
	if err != nil {
		return Event{}, err
	}
	return e, nil
}

// DeleteEvent removes one event, or reports ErrNotFound if there was none. The
// tombstone a client learns it by comes from the trigger, not from here.
func (s *Store) DeleteEvent(id EventID) error {
	res, err := s.db.Exec(`DELETE FROM events WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete event: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ErrNotFound
	}
	return nil
}

// insertEventRow and updateEventRow are the two event-row writes, extracted so
// the REST/tool path (CreateEvent, UpdateEvent) and the fact projection write
// the SAME columns — a projected event that skipped a column would be a second,
// subtly different kind of event.

func insertEventRow(q dbtx, e Event) error {
	if _, err := q.Exec(
		`INSERT INTO events (`+eventColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ID, e.CalendarID, e.Title, fmtEventTime(e.StartsAt), fmtEventTime(e.EndsAt), e.Location, e.Notes,
		e.CreatedBy, fmtEventTime(e.CreatedAt), fmtEventTime(e.UpdatedAt), e.RRule, e.TZ, e.Automation); err != nil {
		return fmt.Errorf("create event: %w", err)
	}
	return nil
}

func updateEventRow(q dbtx, e Event) error {
	if _, err := q.Exec(
		`UPDATE events SET calendar_id = ?, title = ?, starts_at = ?, ends_at = ?, location = ?,
		        notes = ?, rrule = ?, tz = ?, automation = ?, updated_at = ? WHERE id = ?`,
		e.CalendarID, e.Title, fmtEventTime(e.StartsAt), fmtEventTime(e.EndsAt), e.Location, e.Notes,
		e.RRule, e.TZ, e.Automation, fmtEventTime(e.UpdatedAt), e.ID); err != nil {
		return fmt.Errorf("update event: %w", err)
	}
	return nil
}

// ListEvents returns the calendar in start order, optionally windowed to
// [from, to). The window is an OVERLAP test, not a containment one — an event
// that began before the window and is still running belongs in it, which is
// what makes "what's on today" answer correctly for a meeting that started
// yesterday. A zero bound is unbounded, so ListEvents(zero, zero) is the whole
// calendar.
//
// The comparison is TEXT, which is only correct because every stored time is
// fixed-width RFC3339 UTC — see the storage DECISION on Event.
func (s *Store) ListEvents(from, to time.Time) ([]Event, error) {
	where := []string{}
	var args []any
	if !from.IsZero() {
		where = append(where, `ends_at > ?`)
		args = append(args, fmtEventTime(from))
	}
	if !to.IsZero() {
		where = append(where, `starts_at < ?`)
		args = append(args, fmtEventTime(to))
	}
	query := `SELECT ` + eventColumns + ` FROM events`
	if len(where) > 0 {
		query += ` WHERE ` + strings.Join(where, ` AND `)
	}
	// id breaks ties so a page is deterministic; evt_ ULIDs sort by creation.
	query += ` ORDER BY starts_at, id`
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("list events: %w", err)
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Instances returns the expanded calendar over [from, to), sorted by start
// (event id breaking ties, so a page is deterministic): single events pass
// through under ListEvents' overlap rule, recurring events expand through
// their RRULE (expandEvent). Instances are DERIVED — nothing here writes, and
// nothing here reaches the change feed.
//
// Candidate selection differs from ListEvents on purpose: a recurring event's
// stored StartsAt/EndsAt are its FIRST occurrence, so the master row of a
// series can lie far before a window its instances land in — the overlap test
// would wrongly drop it. A rule cannot produce an instance before its own
// DTSTART, so starts_at < to is the one bound that is safe to push into SQL.
func (s *Store) Instances(from, to time.Time) ([]Instance, error) {
	rows, err := s.db.Query(`SELECT `+eventColumns+` FROM events
		 WHERE (rrule = '' AND ends_at > ? AND starts_at < ?) OR (rrule != '' AND starts_at < ?)`,
		fmtEventTime(from), fmtEventTime(to), fmtEventTime(to))
	if err != nil {
		return nil, fmt.Errorf("instance candidates: %w", err)
	}
	defer rows.Close()
	var out []Instance
	for rows.Next() {
		ev, err := scanEvent(rows)
		if err != nil {
			return nil, fmt.Errorf("scan instance candidate: %w", err)
		}
		ins, err := expandEvent(ev, from, to)
		if err != nil {
			// Validation keeps bad rules out of the store; reaching this means
			// the store itself is inconsistent, which is not the caller's 400.
			return nil, fmt.Errorf("expand: %w", err)
		}
		out = append(out, ins...)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("instance candidates: %w", err)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].StartsAt.Equal(out[j].StartsAt) {
			return out[i].StartsAt.Before(out[j].StartsAt)
		}
		return out[i].EventID < out[j].EventID
	})
	return out, nil
}

// Fireable returns the automations due at the given instant — the read behind
// GET /v1/fireable, execcal's one query. A row is an instance that is ACTIVE
// (startsAt <= at < endsAt), on an executable calendar, naming an automation;
// its window bounds are the instance's own, the frame the automations
// service's idempotence checks run in. The candidate filter lives in SQL so a
// calendar full of lunches costs nothing here.
func (s *Store) Fireable(at time.Time) ([]Fireable, error) {
	rows, err := s.db.Query(`SELECT ` + eventColumns + ` FROM events
		 WHERE automation != ''
		   AND calendar_id IN (SELECT id FROM calendars WHERE executable = 1)`)
	if err != nil {
		return nil, fmt.Errorf("fireable candidates: %w", err)
	}
	defer rows.Close()
	var out []Fireable
	for rows.Next() {
		ev, err := scanEvent(rows)
		if err != nil {
			return nil, fmt.Errorf("scan fireable candidate: %w", err)
		}
		// A [at, at+1s) window catches exactly the instances overlapping the
		// instant; the explicit bounds check below is the contract's
		// half-open rule, kept separate so it cannot drift into the overlap.
		ins, err := expandEvent(ev, at, at.Add(time.Second))
		if err != nil {
			return nil, fmt.Errorf("expand: %w", err)
		}
		for _, in := range ins {
			if !in.StartsAt.After(at) && at.Before(in.EndsAt) {
				out = append(out, Fireable{
					Automation:  in.Automation,
					EventID:     in.EventID,
					WindowStart: in.StartsAt,
					WindowEnd:   in.EndsAt,
				})
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("fireable candidates: %w", err)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Automation != out[j].Automation {
			return out[i].Automation < out[j].Automation
		}
		return out[i].EventID < out[j].EventID
	})
	return out, nil
}

// GetNet loads a net and populates its Bots membership from the bots table.
func (s *Store) GetNet(id string) (PrivateBotNet, error) {
	var net PrivateBotNet
	err := s.db.QueryRow(`SELECT id, name FROM nets WHERE id = ?`, id).Scan(&net.ID, &net.Name)
	if errors.Is(err, sql.ErrNoRows) {
		return PrivateBotNet{}, ErrNotFound
	}
	if err != nil {
		return PrivateBotNet{}, fmt.Errorf("get net: %w", err)
	}
	rows, err := s.db.Query(`SELECT id FROM bots WHERE net_id = ? ORDER BY rowid`, id)
	if err != nil {
		return PrivateBotNet{}, fmt.Errorf("get net bots: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var bid BotID
		if err := rows.Scan(&bid); err != nil {
			return PrivateBotNet{}, fmt.Errorf("scan bot id: %w", err)
		}
		net.Bots = append(net.Bots, bid)
	}
	return net, rows.Err()
}
