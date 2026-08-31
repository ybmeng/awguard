package botnet

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// Named calendars, at the same three levels the calendar service is tested at:
// the store (CRUD, the Personal ensure, the cascade), the tool commands, and
// the REST face — plus the migration that gives pre-calendar events a home.

// ── Store ─────────────────────────────────────────────────────────────────────

// TestCalendarCRUDRoundTrip: a calendar survives the write, comes back the
// same, is patched a field at a time, and is gone after the delete.
func TestCalendarCRUDRoundTrip(t *testing.T) {
	s := newEventStore(t)

	cal, err := s.CreateCalendar("  Company Earnings  ", "green", "bot_ADA")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !strings.HasPrefix(string(cal.ID), "cal_") || len(cal.ID) != len("cal_")+26 {
		t.Errorf("id = %q, want a cal_-prefixed ULID", cal.ID)
	}
	if cal.Name != "Company Earnings" {
		t.Errorf("name = %q, want it trimmed", cal.Name)
	}
	if cal.Color != "green" || cal.CreatedBy != "bot_ADA" {
		t.Errorf("stored calendar = %+v, want the caller's color and author", cal)
	}
	if cal.CreatedAt.IsZero() || !cal.UpdatedAt.Equal(cal.CreatedAt) {
		t.Errorf("timestamps = %s / %s, want both stamped and equal on create", cal.CreatedAt, cal.UpdatedAt)
	}

	got, err := s.GetCalendar(cal.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got != cal {
		t.Errorf("stored calendar = %+v, want %+v", got, cal)
	}

	// A color-only patch leaves the name exactly as it was, and vice versa.
	teal := "teal"
	patched, err := s.UpdateCalendar(cal.ID, CalendarPatch{Color: &teal})
	if err != nil {
		t.Fatalf("recolor: %v", err)
	}
	if patched.Color != "teal" || patched.Name != cal.Name {
		t.Errorf("a color patch = %+v, want only the color moved", patched)
	}
	if patched.CreatedBy != cal.CreatedBy || !patched.CreatedAt.Equal(cal.CreatedAt) {
		t.Errorf("a patch rewrote the authorship: %+v", patched)
	}
	name := "Earnings"
	renamed, err := s.UpdateCalendar(cal.ID, CalendarPatch{Name: &name})
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	if renamed.Name != "Earnings" || renamed.Color != "teal" {
		t.Errorf("a name patch = %+v, want only the name moved", renamed)
	}
	// Renaming a calendar to its own name is not a duplicate of itself.
	if _, err := s.UpdateCalendar(cal.ID, CalendarPatch{Name: &name}); err != nil {
		t.Errorf("renaming a calendar to its own name: %v", err)
	}

	if err := s.DeleteCalendar(cal.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.GetCalendar(cal.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("get after delete = %v, want ErrNotFound", err)
	}
	if err := s.DeleteCalendar(cal.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("second delete = %v, want ErrNotFound", err)
	}
	if _, err := s.UpdateCalendar(cal.ID, CalendarPatch{Name: &name}); !errors.Is(err, ErrNotFound) {
		t.Errorf("update after delete = %v, want ErrNotFound", err)
	}
}

// TestCalendarValidation pins the rules that live in the store — one place, so
// the REST path and the tool path cannot enforce different ones. Every
// rejection is ErrInvalid, which the server maps to 400.
func TestCalendarValidation(t *testing.T) {
	s := newEventStore(t)
	if _, err := s.CreateCalendar("Work", "blue", userAuthor); err != nil {
		t.Fatalf("create: %v", err)
	}

	cases := []struct{ name, calName, color string }{
		{"empty name", "", ""},
		{"blank name", "   ", ""},
		{"name too long", strings.Repeat("x", 65), ""},
		{"unknown color", "Fun", "chartreuse"},
		{"dup name", "Work", ""},
		{"dup name case-insensitive", "wORK", ""},
	}
	for _, c := range cases {
		if _, err := s.CreateCalendar(c.calName, c.color, userAuthor); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: create = %v, want ErrInvalid", c.name, err)
		}
	}
	// A 64-rune name is exactly legal.
	if _, err := s.CreateCalendar(strings.Repeat("y", 64), "", userAuthor); err != nil {
		t.Errorf("a 64-char name was rejected: %v", err)
	}

	// The same rules bind a patch, checked against the MERGED calendar.
	other, err := s.CreateCalendar("Other", "", userAuthor)
	if err != nil {
		t.Fatalf("create other: %v", err)
	}
	dup := "woRK"
	if _, err := s.UpdateCalendar(other.ID, CalendarPatch{Name: &dup}); !errors.Is(err, ErrInvalid) {
		t.Errorf("renaming onto an existing name = %v, want ErrInvalid", err)
	}
	bad := "mauve"
	if _, err := s.UpdateCalendar(other.ID, CalendarPatch{Color: &bad}); !errors.Is(err, ErrInvalid) {
		t.Errorf("recoloring to an unknown color = %v, want ErrInvalid", err)
	}
	if after, _ := s.GetCalendar(other.ID); after != other {
		t.Errorf("a rejected patch still changed the calendar: %+v, was %+v", after, other)
	}
}

// TestCalendarColorAssignment: an omitted color cycles the enum in order on
// the count of existing calendars, so unnamed creates come out distinct.
func TestCalendarColorAssignment(t *testing.T) {
	s := newEventStore(t)
	names := []string{"A", "B", "C", "D", "E", "F", "G"}
	for i, name := range names {
		cal, err := s.CreateCalendar(name, "", userAuthor)
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		if want := calendarColors[i%len(calendarColors)]; cal.Color != want {
			t.Errorf("calendar %d got color %q, want %q (cycling)", i, cal.Color, want)
		}
	}
}

// TestListCalendarsAscendingCreatedAt: oldest first, [] semantics at the REST
// face are pinned separately — here just the order and the empty list.
func TestListCalendarsAscendingCreatedAt(t *testing.T) {
	s := newEventStore(t)
	if cals, err := s.ListCalendars(); err != nil || len(cals) != 0 {
		t.Fatalf("fresh store lists %v (%v), want nothing — no ensure on read", cals, err)
	}
	first, _ := s.CreateCalendar("First", "", userAuthor)
	second, _ := s.CreateCalendar("Second", "", userAuthor)
	third, _ := s.CreateCalendar("Third", "", userAuthor)
	cals, err := s.ListCalendars()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	want := []CalendarID{first.ID, second.ID, third.ID}
	if len(cals) != len(want) {
		t.Fatalf("listed %d calendars, want %d", len(cals), len(want))
	}
	for i, id := range want {
		if cals[i].ID != id {
			t.Errorf("list[%d] = %q, want %q (ascending createdAt)", i, cals[i].Name, id)
		}
	}
}

// TestEnsurePersonalCalendarIdempotent: the ensure is keyed on the NAME,
// case-insensitively, and two concurrent-ish ensures produce one row.
func TestEnsurePersonalCalendarIdempotent(t *testing.T) {
	s := newEventStore(t)

	var wg sync.WaitGroup
	got := make([]Calendar, 2)
	errs := make([]error, 2)
	for i := range got {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got[i], errs[i] = s.EnsurePersonalCalendar()
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("ensure %d: %v", i, err)
		}
	}
	if got[0].ID != got[1].ID {
		t.Errorf("two ensures produced two calendars: %q and %q", got[0].ID, got[1].ID)
	}
	cals, err := s.ListCalendars()
	if err != nil || len(cals) != 1 {
		t.Fatalf("store holds %d calendars (%v), want exactly one Personal", len(cals), err)
	}
	if cals[0].Name != "Personal" || cals[0].Color != "blue" || cals[0].CreatedBy != userAuthor {
		t.Errorf("ensured calendar = %+v, want Personal, blue, by user", cals[0])
	}

	// A user-renamed casing still satisfies the ensure — no second Personal.
	lower := "personal"
	if _, err := s.UpdateCalendar(cals[0].ID, CalendarPatch{Name: &lower}); err != nil {
		t.Fatalf("rename to lowercase: %v", err)
	}
	again, err := s.EnsurePersonalCalendar()
	if err != nil {
		t.Fatalf("ensure after rename: %v", err)
	}
	if again.ID != cals[0].ID {
		t.Errorf("ensure after a case-only rename minted %q, want the existing %q", again.ID, cals[0].ID)
	}

	// Deleting Personal is legal; the next unqualified write self-heals it.
	if err := s.DeleteCalendar(again.ID); err != nil {
		t.Fatalf("delete personal: %v", err)
	}
	ev := mustCreateEvent(t, s, "unqualified", at(31, 12), at(31, 13), userAuthor)
	healed, err := s.CalendarByName("Personal")
	if err != nil {
		t.Fatalf("personal after an unqualified write: %v", err)
	}
	if healed.ID == again.ID {
		t.Errorf("the healed Personal reused the deleted id %q", healed.ID)
	}
	if got, _ := s.GetEvent(ev.ID); got.CalendarID != healed.ID {
		t.Errorf("unqualified event landed in %q, want the healed Personal %q", got.CalendarID, healed.ID)
	}
}

// TestEventWritesResolveTheCalendar: a named calendar must exist — at create
// and at move — and the rejection is ErrInvalid, the caller's fault.
func TestEventWritesResolveTheCalendar(t *testing.T) {
	s := newEventStore(t)
	work, err := s.CreateCalendar("Work", "", userAuthor)
	if err != nil {
		t.Fatalf("create calendar: %v", err)
	}

	if _, err := s.CreateEvent(Event{
		Title: "x", StartsAt: at(31, 12), EndsAt: at(31, 13), CalendarID: "cal_NOPE",
	}, userAuthor); !errors.Is(err, ErrInvalid) {
		t.Errorf("create into an unknown calendar = %v, want ErrInvalid", err)
	}

	ev, err := s.CreateEvent(Event{
		Title: "planning", StartsAt: at(31, 12), EndsAt: at(31, 13), CalendarID: work.ID,
	}, userAuthor)
	if err != nil {
		t.Fatalf("create into Work: %v", err)
	}
	if ev.CalendarID != work.ID {
		t.Errorf("calendarId = %q, want %q", ev.CalendarID, work.ID)
	}

	bad := CalendarID("cal_NOPE")
	if _, err := s.UpdateEvent(ev.ID, EventPatch{CalendarID: &bad}); !errors.Is(err, ErrInvalid) {
		t.Errorf("move to an unknown calendar = %v, want ErrInvalid", err)
	}
	personal, err := s.EnsurePersonalCalendar()
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	moved, err := s.UpdateEvent(ev.ID, EventPatch{CalendarID: &personal.ID})
	if err != nil {
		t.Fatalf("move: %v", err)
	}
	if moved.CalendarID != personal.ID || moved.Title != "planning" {
		t.Errorf("moved event = %+v, want only the calendar changed", moved)
	}
}

// ── Migration ─────────────────────────────────────────────────────────────────

// seedPreCalendarDB writes a database in the pre-calendar shape: the events
// table exactly as it was before calendars existed, holding rows.
func seedPreCalendarDB(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("seed open: %v", err)
	}
	defer db.Close()
	const preCalendar = `
CREATE TABLE events (
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
CREATE INDEX idx_events_starts ON events(starts_at);
INSERT INTO events VALUES ('evt_OLD1', 'dentist', '2026-08-31T09:00:00Z', '2026-08-31T10:00:00Z',
    '', '', 'user', '2026-08-01T00:00:00Z', '2026-08-01T00:00:00Z');
INSERT INTO events VALUES ('evt_OLD2', 'lunch a bot booked', '2026-08-31T12:00:00Z', '2026-08-31T13:00:00Z',
    '', '', 'bot_GONE', '2026-08-02T00:00:00Z', '2026-08-02T00:00:00Z');
`
	if _, err := db.Exec(preCalendar); err != nil {
		t.Fatalf("seed pre-calendar schema: %v", err)
	}
}

// TestMigrationBackfillsEventCalendars: opening a pre-calendar database with
// event rows lands every one of them in the ensured Personal calendar — the
// SAME ensure every unqualified write uses — and doing it twice changes
// nothing.
func TestMigrationBackfillsEventCalendars(t *testing.T) {
	path := filepath.Join(t.TempDir(), "precalendar.db")
	seedPreCalendarDB(t, path)
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	personal, err := s.CalendarByName("Personal")
	if err != nil {
		t.Fatalf("migration ensured no Personal calendar: %v", err)
	}
	if personal.Color != "blue" || personal.CreatedBy != userAuthor {
		t.Errorf("ensured Personal = %+v, want blue, by user", personal)
	}
	events, err := s.ListEvents(time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("migrated calendar holds %s, want both seeded events", eventTitles(events))
	}
	for _, e := range events {
		if e.CalendarID != personal.ID {
			t.Errorf("event %s carries calendar %q, want the Personal backfill %q", e.ID, e.CalendarID, personal.ID)
		}
	}
	// The backfill is a real write and the triggers were already in place, so a
	// reconnecting client hears about it: Personal's birth, then an update per
	// re-homed event (rows whose calendarId just appeared must be refetched).
	rows := logAfter(t, s, 0)
	wantRows := []changeRow{
		{"calendar", string(personal.ID), "created"},
		{"event", "evt_OLD1", "updated"},
		{"event", "evt_OLD2", "updated"},
	}
	if !reflect.DeepEqual(rows, wantRows) {
		t.Errorf("migration emitted %v, want %v", rows, wantRows)
	}

	// Close and reopen: the backfill's guard is the '' it erased, so a second
	// migrate must not mint a second Personal or move a row.
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	cals, err := reopened.ListCalendars()
	if err != nil || len(cals) != 1 {
		t.Fatalf("after reopen the store holds %d calendars (%v), want still just Personal", len(cals), err)
	}
	if cals[0].ID != personal.ID {
		t.Errorf("reopen re-minted Personal: %q, was %q", cals[0].ID, personal.ID)
	}
}

// TestFreshDatabaseHasNoCalendars: migration must not conjure Personal out of
// nothing — with no stragglers to backfill, opening is not a write.
func TestFreshDatabaseHasNoCalendars(t *testing.T) {
	s := newEventStore(t)
	cals, err := s.ListCalendars()
	if err != nil || len(cals) != 0 {
		t.Errorf("a fresh database holds %v (%v), want no calendars until one is needed", cals, err)
	}
}

// ── The tool commands ─────────────────────────────────────────────────────────

// runCal dispatches one calendar tool call and fails the test on a
// turn-failing error — the instructive-result text is the return.
func runCal(t *testing.T, tb *BotToolbox, args string) string {
	t.Helper()
	res, err := tb.Run(context.Background(), calendarToolName, json.RawMessage(args))
	if err != nil {
		t.Fatalf("Run(%s): %v", args, err)
	}
	return res.text
}

// TestCalendarManagementCommands walks the four new commands through the
// dispatch seam and checks the store afterwards.
func TestCalendarManagementCommands(t *testing.T) {
	s := newEventStore(t)
	bot := newBot(t, s)
	tb := NewBotToolbox(s, bot.ID, nil)

	// create_calendar, color assigned by cycling (first calendar → blue).
	if got := runCal(t, tb, `{"command":"create_calendar","name":"Company Earnings"}`); got != `created calendar "Company Earnings" (blue)` {
		t.Errorf("create_calendar = %q", got)
	}
	cal, err := s.CalendarByName("Company Earnings")
	if err != nil {
		t.Fatalf("created calendar not in store: %v", err)
	}
	// The CALLING bot is the author, exactly as event creates stamp it.
	if cal.CreatedBy != string(bot.ID) {
		t.Errorf("createdBy = %q, want the calling bot %q", cal.CreatedBy, bot.ID)
	}
	// And with an explicit color.
	if got := runCal(t, tb, `{"command":"create_calendar","name":"Financial Updates","color":"red"}`); got != `created calendar "Financial Updates" (red)` {
		t.Errorf("create_calendar with color = %q", got)
	}

	// list_calendars renders name, color and event count.
	runCal(t, tb, `{"command":"create","title":"Q3 call","start":"2026-09-15T21:00:00Z","calendar":"Company Earnings"}`)
	got := runCal(t, tb, `{"command":"list_calendars"}`)
	for _, want := range []string{"2 calendar(s):", "Company Earnings (blue) — 1 event(s)", "Financial Updates (red) — 0 event(s)"} {
		if !strings.Contains(got, want) {
			t.Errorf("list_calendars = %q, want it to contain %q", got, want)
		}
	}

	// rename_calendar renames and recolors in one call.
	if got := runCal(t, tb, `{"command":"rename_calendar","calendar":"financial updates","name":"Macro","color":"teal"}`); got != `updated calendar "Macro" (teal)` {
		t.Errorf("rename_calendar = %q", got)
	}
	if _, err := s.CalendarByName("Macro"); err != nil {
		t.Errorf("renamed calendar not findable: %v", err)
	}

	// delete_calendar deletes an empty calendar…
	if got := runCal(t, tb, `{"command":"delete_calendar","calendar":"Macro"}`); got != `deleted calendar "Macro"` {
		t.Errorf("delete_calendar = %q", got)
	}
	if _, err := s.CalendarByName("Macro"); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleted calendar still resolves: %v", err)
	}
	// …and REFUSES a non-empty one, naming the count — the cascade is UI-only.
	if got := runCal(t, tb, `{"command":"delete_calendar","calendar":"Company Earnings"}`); got != `error: calendar "Company Earnings" has 1 event(s); delete or move them first` {
		t.Errorf("delete_calendar on a non-empty calendar = %q, want the refusal", got)
	}
	if _, err := s.CalendarByName("Company Earnings"); err != nil {
		t.Errorf("a refused delete still removed the calendar: %v", err)
	}
}

// TestCalendarToolInstructiveErrors pins the new commands' correctable
// results: every model mistake is an "error: ..." RESULT, never a failed turn.
func TestCalendarToolInstructiveErrors(t *testing.T) {
	s := newEventStore(t)
	bot := newBot(t, s)
	tb := NewBotToolbox(s, bot.ID, nil)

	// Before any calendar exists, an unknown name says so rather than listing
	// an empty set.
	if got := runCal(t, tb, `{"command":"create","title":"x","start":"2026-08-31T12:00:00Z","calendar":"Wrok"}`); got != `error: no calendar named "Wrok" — none exist yet; create_calendar makes one` {
		t.Errorf("unknown calendar with none existing = %q", got)
	}

	if _, err := s.CreateCalendar("Personal", "", userAuthor); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.CreateCalendar("Work", "", userAuthor); err != nil {
		t.Fatalf("create: %v", err)
	}

	cases := []struct{ name, args, want string }{
		{"create into a typo'd calendar (no auto-create)",
			`{"command":"create","title":"x","start":"2026-08-31T12:00:00Z","calendar":"Wrok"}`,
			`error: no calendar named "Wrok" — existing calendars: Personal, Work`},
		{"list of an unknown calendar",
			`{"command":"list","calendar":"Wrok"}`,
			`error: no calendar named "Wrok" — existing calendars: Personal, Work`},
		{"create_calendar without a name",
			`{"command":"create_calendar"}`,
			"error: 'create_calendar' requires a 'name' field"},
		{"create_calendar duplicate name",
			`{"command":"create_calendar","name":"wORK"}`,
			`error: a calendar named "Work" already exists`},
		{"create_calendar bad color",
			`{"command":"create_calendar","name":"Fun","color":"chartreuse"}`,
			`error: color "chartreuse" is not one of blue, green, orange, purple, red, teal`},
		{"rename_calendar without a target",
			`{"command":"rename_calendar","name":"New"}`,
			"error: 'rename_calendar' requires a 'calendar' field"},
		{"rename_calendar of an unknown calendar",
			`{"command":"rename_calendar","calendar":"Wrok","name":"Work2"}`,
			`error: no calendar named "Wrok" — existing calendars: Personal, Work`},
		{"rename_calendar with nothing to change",
			`{"command":"rename_calendar","calendar":"Work"}`,
			"error: 'rename_calendar' needs a 'name' (the new name) or a 'color' to change"},
		{"delete_calendar without a name",
			`{"command":"delete_calendar"}`,
			"error: 'delete_calendar' requires a 'calendar' field"},
		{"delete_calendar of an unknown calendar",
			`{"command":"delete_calendar","calendar":"Wrok"}`,
			`error: no calendar named "Wrok" — existing calendars: Personal, Work`},
	}
	for _, c := range cases {
		if got := runCal(t, tb, c.args); got != c.want {
			t.Errorf("%s: result %q, want %q", c.name, got, c.want)
		}
	}

	// No typo above wrote anything: still two calendars, zero events.
	if cals, _ := s.ListCalendars(); len(cals) != 2 {
		t.Errorf("a rejected call changed the calendars: %d, want 2", len(cals))
	}
	if events, _ := s.ListEvents(time.Time{}, time.Time{}); len(events) != 0 {
		t.Errorf("a rejected call booked an event: %s", eventTitles(events))
	}
}

// TestCalendarToolEventCommandsUseCalendars: create books into the named
// calendar, update moves, list filters and annotates.
func TestCalendarToolEventCommandsUseCalendars(t *testing.T) {
	s := newEventStore(t)
	bot := newBot(t, s)
	tb := NewBotToolbox(s, bot.ID, nil)

	runCal(t, tb, `{"command":"create_calendar","name":"Company Earnings"}`)

	// An unqualified create Personal-ensures, exactly like REST.
	runCal(t, tb, `{"command":"create","title":"dentist","start":"2026-08-31T09:00:00Z"}`)
	personal, err := s.CalendarByName("Personal")
	if err != nil {
		t.Fatalf("no Personal after an unqualified tool create: %v", err)
	}

	// A qualified create stamps the named calendar's id (case-insensitive).
	runCal(t, tb, `{"command":"create","title":"Q3 call","start":"2026-09-15T21:00:00Z","calendar":"company earnings"}`)
	earnings, err := s.CalendarByName("Company Earnings")
	if err != nil {
		t.Fatalf("calendar: %v", err)
	}
	events, err := s.ListEvents(time.Time{}, time.Time{})
	if err != nil || len(events) != 2 {
		t.Fatalf("store holds %s (%v), want both events", eventTitles(events), err)
	}
	byTitle := map[string]Event{}
	for _, e := range events {
		byTitle[e.Title] = e
	}
	if byTitle["dentist"].CalendarID != personal.ID {
		t.Errorf("dentist landed in %q, want Personal %q", byTitle["dentist"].CalendarID, personal.ID)
	}
	if byTitle["Q3 call"].CalendarID != earnings.ID {
		t.Errorf("Q3 call landed in %q, want Company Earnings %q", byTitle["Q3 call"].CalendarID, earnings.ID)
	}

	// With more than one calendar, the listing annotates every event.
	got := runCal(t, tb, `{"command":"list","from":"2026-08-01T00:00:00Z","to":"2026-10-01T00:00:00Z"}`)
	if !strings.Contains(got, "[Personal]") || !strings.Contains(got, "[Company Earnings]") {
		t.Errorf("multi-calendar listing = %q, want [CalendarName] annotations", got)
	}
	// A calendar filter narrows the listing to that calendar's events.
	got = runCal(t, tb, `{"command":"list","from":"2026-08-01T00:00:00Z","to":"2026-10-01T00:00:00Z","calendar":"Company Earnings"}`)
	if !strings.Contains(got, "Q3 call") || strings.Contains(got, "dentist") {
		t.Errorf("filtered listing = %q, want only the Company Earnings event", got)
	}

	// update moves an event between calendars.
	runCal(t, tb, `{"command":"update","event_id":"`+string(byTitle["dentist"].ID)+`","calendar":"Company Earnings"}`)
	if moved, _ := s.GetEvent(byTitle["dentist"].ID); moved.CalendarID != earnings.ID {
		t.Errorf("update moved dentist to %q, want %q", moved.CalendarID, earnings.ID)
	}

	// With every event now in ONE remaining populated set but two calendars
	// still existing, annotations stay; delete the extra calendar's events and
	// the calendar, and a single-calendar listing drops them.
	for _, e := range events {
		runCal(t, tb, `{"command":"delete","event_id":"`+string(e.ID)+`"}`)
	}
	runCal(t, tb, `{"command":"delete_calendar","calendar":"Company Earnings"}`)
	runCal(t, tb, `{"command":"create","title":"solo","start":"2026-08-31T12:00:00Z"}`)
	got = runCal(t, tb, `{"command":"list","from":"2026-08-01T00:00:00Z","to":"2026-10-01T00:00:00Z"}`)
	if strings.Contains(got, "[Personal]") {
		t.Errorf("single-calendar listing = %q, want no annotation", got)
	}
}

// ── REST ──────────────────────────────────────────────────────────────────────

// postCalendar creates one calendar over HTTP and returns it, asserting the
// contract's 200.
func postCalendar(t *testing.T, h *harness, body string) Calendar {
	t.Helper()
	var cal Calendar
	postExpect(t, http.StatusOK, h.ts.URL+"/v1/calendars", body, &cal)
	return cal
}

// TestCalendarsAPIRoundTrip drives the API the manage-calendars sheet uses.
func TestCalendarsAPIRoundTrip(t *testing.T) {
	h := newHarness(t, &fakeLLM{})

	// An empty list is [] and never null, and it carries the sync token.
	resp, err := http.Get(h.ts.URL + "/v1/calendars")
	if err != nil {
		t.Fatalf("get calendars: %v", err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("X-BotNet-State"); got == "" {
		t.Error("GET /v1/calendars carries no X-BotNet-State, so a client cannot start syncing from it")
	}
	var listed []Calendar
	if err := json.NewDecoder(resp.Body).Decode(&listed); err != nil {
		t.Fatalf("decode calendars: %v", err)
	}
	if listed == nil || len(listed) != 0 {
		t.Fatalf("fresh GET /v1/calendars = %+v, want []", listed)
	}
	if raw := rawGet(t, h.ts.URL+"/v1/calendars"); strings.TrimSpace(raw) != "[]" {
		t.Errorf("empty calendar list = %s, want []", raw)
	}

	cal := postCalendar(t, h, `{"name":"Company Earnings"}`)
	if cal.Name != "Company Earnings" || cal.Color != "blue" {
		t.Errorf("created calendar = %+v, want the name with the first cycled color", cal)
	}
	if cal.CreatedBy != userAuthor {
		t.Errorf("createdBy = %q, want %q for a UI-created calendar", cal.CreatedBy, userAuthor)
	}
	second := postCalendar(t, h, `{"name":"Financial Updates","color":"red"}`)
	if second.Color != "red" {
		t.Errorf("explicit color = %q, want red", second.Color)
	}

	get(t, h.ts.URL+"/v1/calendars", &listed)
	if len(listed) != 2 || listed[0].ID != cal.ID || listed[1].ID != second.ID {
		t.Fatalf("GET /v1/calendars = %+v, want both, ascending createdAt", listed)
	}

	// PATCH is partial: a color-only patch leaves the name alone.
	var patched Calendar
	patch(t, h.ts.URL+"/v1/calendars/"+string(cal.ID), `{"color":"teal"}`, &patched)
	if patched.Color != "teal" || patched.Name != "Company Earnings" {
		t.Errorf("color patch = %+v, want only the color moved", patched)
	}

	if code, raw := deleteRaw(t, h.ts.URL+"/v1/calendars/"+string(second.ID)); code != http.StatusNoContent || raw != "" {
		t.Errorf("DELETE = %d %q, want a bare 204", code, raw)
	}
	get(t, h.ts.URL+"/v1/calendars", &listed)
	if len(listed) != 1 || listed[0].ID != cal.ID {
		t.Errorf("after delete, list = %+v, want just the first calendar", listed)
	}
}

// TestCalendarsAPIValidation: every bad body is a 400 in the existing error
// shape; a missing calendar is a 404.
func TestCalendarsAPIValidation(t *testing.T) {
	h := newHarness(t, &fakeLLM{})
	postCalendar(t, h, `{"name":"Work"}`)

	bad := []struct{ name, body string }{
		{"no name", `{"color":"blue"}`},
		{"empty name", `{"name":"   "}`},
		{"name too long", `{"name":"` + strings.Repeat("x", 65) + `"}`},
		{"unknown color", `{"name":"Fun","color":"chartreuse"}`},
		{"dup name", `{"name":"Work"}`},
		{"dup name case-insensitive", `{"name":"wORK"}`},
		{"not json", `{`},
	}
	for _, c := range bad {
		code, raw := postRaw(t, h.ts.URL+"/v1/calendars", c.body)
		if code != http.StatusBadRequest {
			t.Errorf("%s: POST status %d, want 400: %s", c.name, code, raw)
			continue
		}
		var body map[string]string
		if err := json.Unmarshal([]byte(raw), &body); err != nil || body["error"] == "" {
			t.Errorf("%s: 400 body = %s, want the existing {\"error\":...} shape", c.name, raw)
		}
	}

	if code, _ := patchRaw(t, h.ts.URL+"/v1/calendars/cal_NOPE", `{"name":"x"}`); code != http.StatusNotFound {
		t.Errorf("PATCH of an unknown calendar = %d, want 404", code)
	}
	if code, _ := deleteRaw(t, h.ts.URL+"/v1/calendars/cal_NOPE"); code != http.StatusNotFound {
		t.Errorf("DELETE of an unknown calendar = %d, want 404", code)
	}
	// The method-pattern mux 405s the rest for free; prove it.
	if code, _ := getRaw(t, h.ts.URL+"/v1/calendars/cal_NOPE"); code != http.StatusMethodNotAllowed {
		t.Errorf("GET of one calendar = %d, want 405 (no such route)", code)
	}
}

// TestEventsAPIUsesCalendars: POST without calendarId Personal-ensures, with
// an unknown one 400s, and PATCH moves an event between calendars.
func TestEventsAPIUsesCalendars(t *testing.T) {
	h := newHarness(t, &fakeLLM{})

	// Unqualified POST lands in Personal — created on demand, visible in the
	// collection afterwards.
	ev := postEvent(t, h, `{"title":"dentist","startsAt":"2026-08-31T09:00:00Z","endsAt":"2026-08-31T10:00:00Z"}`)
	if ev.CalendarID == "" {
		t.Fatal("POST /v1/events answered with no calendarId")
	}
	personal, err := h.store.CalendarByName("Personal")
	if err != nil {
		t.Fatalf("no Personal after an unqualified POST: %v", err)
	}
	if ev.CalendarID != personal.ID {
		t.Errorf("calendarId = %q, want the ensured Personal %q", ev.CalendarID, personal.ID)
	}
	// The wire always carries the key.
	if raw := rawGet(t, h.ts.URL+"/v1/events"); !strings.Contains(raw, `"calendarId":"`+string(personal.ID)+`"`) {
		t.Errorf("GET /v1/events = %s, want calendarId on every row", raw)
	}

	// A named calendar is honoured; an unknown one is the caller's 400.
	work := postCalendar(t, h, `{"name":"Work"}`)
	ev2 := postEvent(t, h, `{"title":"planning","startsAt":"2026-08-31T11:00:00Z",`+
		`"endsAt":"2026-08-31T12:00:00Z","calendarId":"`+string(work.ID)+`"}`)
	if ev2.CalendarID != work.ID {
		t.Errorf("calendarId = %q, want the named %q", ev2.CalendarID, work.ID)
	}
	if code, raw := postRaw(t, h.ts.URL+"/v1/events",
		`{"title":"x","startsAt":"2026-08-31T11:00:00Z","endsAt":"2026-08-31T12:00:00Z","calendarId":"cal_NOPE"}`); code != http.StatusBadRequest {
		t.Errorf("POST into an unknown calendar = %d (%s), want 400", code, raw)
	}

	// PATCH moves; an unknown target is a 400 and moves nothing.
	var moved Event
	patch(t, h.ts.URL+"/v1/events/"+string(ev.ID), `{"calendarId":"`+string(work.ID)+`"}`, &moved)
	if moved.CalendarID != work.ID || moved.Title != "dentist" {
		t.Errorf("moved event = %+v, want only the calendar changed", moved)
	}
	if code, _ := patchRaw(t, h.ts.URL+"/v1/events/"+string(ev.ID), `{"calendarId":"cal_NOPE"}`); code != http.StatusBadRequest {
		t.Errorf("PATCH to an unknown calendar = %d, want 400", code)
	}
	if after, _ := h.store.GetEvent(ev.ID); after.CalendarID != work.ID {
		t.Errorf("a rejected move still moved the event: %q", after.CalendarID)
	}
}

// TestCalendarDeleteCascadesWithTombstones: the REST cascade removes the
// events and the feed reports every death — per-event tombstones AND the
// calendar's — which is the only way a sync client ever learns.
func TestCalendarDeleteCascadesWithTombstones(t *testing.T) {
	h := newHarness(t, &fakeLLM{})

	work := postCalendar(t, h, `{"name":"Work"}`)
	kept := postEvent(t, h, `{"title":"keep me","startsAt":"2026-08-31T09:00:00Z","endsAt":"2026-08-31T10:00:00Z"}`)
	doomed1 := postEvent(t, h, `{"title":"doomed 1","startsAt":"2026-08-31T11:00:00Z",`+
		`"endsAt":"2026-08-31T12:00:00Z","calendarId":"`+string(work.ID)+`"}`)
	doomed2 := postEvent(t, h, `{"title":"doomed 2","startsAt":"2026-08-31T13:00:00Z",`+
		`"endsAt":"2026-08-31T14:00:00Z","calendarId":"`+string(work.ID)+`"}`)

	state, err := h.store.State()
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	if code, raw := deleteRaw(t, h.ts.URL+"/v1/calendars/"+string(work.ID)); code != http.StatusNoContent {
		t.Fatalf("DELETE = %d, want 204: %s", code, raw)
	}

	// The events are really gone, and the unrelated one is untouched.
	var events []Event
	get(t, h.ts.URL+"/v1/events", &events)
	if len(events) != 1 || events[0].ID != kept.ID {
		t.Fatalf("after the cascade the calendar holds %+v, want only the kept event", events)
	}

	var changes Changes
	get(t, h.ts.URL+"/v1/changes?since="+state, &changes)
	if got := changes.Changed.Calendars.Destroyed; len(got) != 1 || got[0] != string(work.ID) {
		t.Errorf("calendars bucket = %+v, want the calendar's tombstone", changes.Changed.Calendars)
	}
	wantDead := []string{string(doomed1.ID), string(doomed2.ID)}
	sort.Strings(wantDead)
	if got := changes.Changed.Events.Destroyed; !reflect.DeepEqual(got, wantDead) {
		t.Errorf("events bucket destroyed = %v, want per-event tombstones %v", got, wantDead)
	}
}

// TestCalendarsInTheChangeFeed: the calendars bucket behaves like every other
// entity's — create, coalesced update, tombstone, and always-present [].
func TestCalendarsInTheChangeFeed(t *testing.T) {
	h := newHarness(t, &fakeLLM{})

	state, err := h.store.State()
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	cal := postCalendar(t, h, `{"name":"Work"}`)
	var changes Changes
	get(t, h.ts.URL+"/v1/changes?since="+state, &changes)
	if got := changes.Changed.Calendars.Created; len(got) != 1 || got[0] != string(cal.ID) {
		t.Fatalf("calendars bucket after a create = %+v", changes.Changed.Calendars)
	}

	state = changes.NewState
	var patched Calendar
	patch(t, h.ts.URL+"/v1/calendars/"+string(cal.ID), `{"color":"teal"}`, &patched)
	get(t, h.ts.URL+"/v1/changes?since="+state, &changes)
	if got := changes.Changed.Calendars.Updated; len(got) != 1 || got[0] != string(cal.ID) {
		t.Errorf("calendars bucket after a patch = %+v, want it updated", changes.Changed.Calendars)
	}

	// The bucket is always present and never null, like the others.
	raw := rawGet(t, h.ts.URL+"/v1/changes?since="+changes.NewState)
	if !strings.Contains(raw, `"calendars"`) {
		t.Errorf("changes body %s misses the calendars bucket", raw)
	}
}
