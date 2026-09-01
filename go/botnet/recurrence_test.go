package botnet

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Calendar-driven firing, storage half: the recurrence/automation columns and
// their migration, the instances and fireable reads, and the write-boundary
// validation that keeps a bad rule or a misplaced automation out of the store.

// ── Migration ─────────────────────────────────────────────────────────────────

// seedPreRecurrenceDB writes a database in the pre-recurrence shape: calendars
// and events exactly as the previous release stored them (calendar_id already
// backfilled, no rrule/tz/automation, no executable), holding live-shaped rows.
func seedPreRecurrenceDB(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("seed open: %v", err)
	}
	defer db.Close()
	const preRecurrence = `
CREATE TABLE calendars (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL,
    color      TEXT NOT NULL,
    created_by TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE UNIQUE INDEX idx_calendars_name ON calendars(name COLLATE NOCASE);
CREATE TABLE events (
    id          TEXT PRIMARY KEY,
    title       TEXT NOT NULL,
    starts_at   TEXT NOT NULL,
    ends_at     TEXT NOT NULL,
    location    TEXT NOT NULL DEFAULT '',
    notes       TEXT NOT NULL DEFAULT '',
    created_by  TEXT NOT NULL,
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL,
    calendar_id TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_events_starts ON events(starts_at);
INSERT INTO calendars VALUES ('cal_OLD', 'Personal', 'blue', 'user',
    '2026-08-01T00:00:00Z', '2026-08-01T00:00:00Z');
INSERT INTO events VALUES ('evt_OLD1', 'dentist', '2026-08-31T09:00:00Z', '2026-08-31T10:00:00Z',
    '', '', 'user', '2026-08-01T00:00:00Z', '2026-08-01T00:00:00Z', 'cal_OLD');
INSERT INTO events VALUES ('evt_OLD2', 'lunch a bot booked', '2026-08-31T12:00:00Z', '2026-08-31T13:00:00Z',
    'the taco place', '', 'bot_GONE', '2026-08-02T00:00:00Z', '2026-08-02T00:00:00Z', 'cal_OLD');
`
	if _, err := db.Exec(preRecurrence); err != nil {
		t.Fatalf("seed pre-recurrence schema: %v", err)
	}
}

// TestMigrationAddsRecurrenceColumns: opening a pre-recurrence database grows
// the columns in place — existing rows read back exactly as before with the
// new fields at their defaults, the new fields are writable, and everything
// survives a close/reopen (the guarded addColumn must not re-add).
func TestMigrationAddsRecurrenceColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prerecurrence.db")
	seedPreRecurrenceDB(t, path)
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	// The old rows are intact, with the new fields at their absent defaults.
	old, err := s.GetEvent("evt_OLD1")
	if err != nil {
		t.Fatalf("get pre-recurrence event: %v", err)
	}
	if old.RRule != "" || old.TZ != "" || old.Automation != "" {
		t.Errorf("pre-recurrence event grew values: rrule %q tz %q automation %q",
			old.RRule, old.TZ, old.Automation)
	}
	if old.Title != "dentist" || old.CalendarID != "cal_OLD" {
		t.Errorf("migration disturbed an existing row: %+v", old)
	}
	cal, err := s.GetCalendar("cal_OLD")
	if err != nil {
		t.Fatalf("get pre-recurrence calendar: %v", err)
	}
	if cal.Executable {
		t.Error("a pre-recurrence calendar migrated as executable")
	}

	// The new fields are writable: flip the calendar executable, then store a
	// recurring automation event on it.
	yes := true
	flipped, err := s.UpdateCalendar(cal.ID, CalendarPatch{Executable: &yes})
	if err != nil {
		t.Fatalf("patch executable: %v", err)
	}
	if !flipped.Executable {
		t.Error("executable patch did not stick")
	}
	ev, err := s.CreateEvent(Event{
		Title: "fred-m2", CalendarID: cal.ID,
		StartsAt: at(25, 17), EndsAt: at(25, 21),
		RRule: "FREQ=MONTHLY;BYDAY=4TU", TZ: "America/New_York",
		Automation: "fred-m2",
	}, userAuthor)
	if err != nil {
		t.Fatalf("create recurring event on migrated db: %v", err)
	}

	// Everything survives a close/reopen, and the reopen is idempotent.
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if s, err = Open(path); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()
	got, err := s.GetEvent(ev.ID)
	if err != nil {
		t.Fatalf("get after reopen: %v", err)
	}
	if got.RRule != ev.RRule || got.TZ != ev.TZ || got.Automation != ev.Automation {
		t.Errorf("recurrence fields after reopen = %q/%q/%q, want %q/%q/%q",
			got.RRule, got.TZ, got.Automation, ev.RRule, ev.TZ, ev.Automation)
	}
	if cal, err = s.GetCalendar(cal.ID); err != nil || !cal.Executable {
		t.Errorf("executable after reopen = %v (%v), want true", cal.Executable, err)
	}
	if old, err = s.GetEvent("evt_OLD1"); err != nil || old.RRule != "" {
		t.Errorf("pre-recurrence event after reopen = %+v (%v), want untouched", old, err)
	}
}

// TestFreshDBSupportsRecurrence: a database born on this version has the
// columns from its first Open — the guarded adds run on fresh schemas too.
func TestFreshDBSupportsRecurrence(t *testing.T) {
	s := newEventStore(t)
	cal, err := s.CreateCalendar("Automations", "teal", userAuthor, true)
	if err != nil {
		t.Fatalf("create executable calendar: %v", err)
	}
	if !cal.Executable {
		t.Error("create did not store executable")
	}
	ev, err := s.CreateEvent(Event{
		Title: "korea-trass", CalendarID: cal.ID,
		StartsAt: at(5, 0), EndsAt: at(5, 4),
		RRule: "FREQ=MONTHLY;BYMONTHDAY=5,20", TZ: "Asia/Seoul",
		Automation: "korea-trass",
	}, userAuthor)
	if err != nil {
		t.Fatalf("create recurring event: %v", err)
	}
	got, err := s.GetEvent(ev.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got != ev {
		t.Errorf("stored recurring event = %+v, want %+v", got, ev)
	}
	if cals, err := s.ListCalendars(); err != nil || len(cals) != 1 || !cals[0].Executable {
		t.Errorf("listed calendar = %+v (%v), want it executable", cals, err)
	}
}

// ── Instances ─────────────────────────────────────────────────────────────────

// TestInstancesMixedSingleAndRecurring: the instances read is the expanded
// calendar — single events pass through, recurring events multiply, and the
// merged list is sorted by start.
func TestInstancesMixedSingleAndRecurring(t *testing.T) {
	s := newEventStore(t)
	single := mustCreateEvent(t, s, "dentist", at(3, 9), at(3, 10), userAuthor)
	weekly, err := s.CreateEvent(Event{
		Title: "standup", StartsAt: at(1, 13), EndsAt: at(1, 14), // Sat Aug 1 2026? (day 1) — fixture detail below
		RRule: "FREQ=WEEKLY;COUNT=8", TZ: "UTC",
	}, userAuthor)
	if err != nil {
		t.Fatalf("create recurring: %v", err)
	}

	got, err := s.Instances(at(1, 0), at(15, 0))
	if err != nil {
		t.Fatalf("instances: %v", err)
	}
	// Weekly from day 1 inside [1, 15): days 1 and 8 — plus the single on day 3.
	var starts []time.Time
	recurring := 0
	for _, in := range got {
		starts = append(starts, in.StartsAt)
		if in.Recurring {
			recurring++
		}
	}
	if len(got) != 3 || recurring != 2 {
		t.Fatalf("instances = %+v, want two weekly + one single", starts)
	}
	if !starts[0].Equal(at(1, 13)) || !starts[1].Equal(at(3, 9)) || !starts[2].Equal(at(8, 13)) {
		t.Errorf("instance order = %v, want sorted by start (1st, 3rd, 8th)", starts)
	}
	for _, in := range got {
		if in.EventID == single.ID && in.Recurring {
			t.Error("the single event's instance is marked recurring")
		}
		if in.EventID == weekly.ID && !in.Recurring {
			t.Error("a weekly instance is not marked recurring")
		}
	}

	// A later window still finds the recurring event even though its MASTER row
	// (first occurrence) is long before the window.
	later, err := s.Instances(at(15, 0), at(29, 0))
	if err != nil {
		t.Fatalf("instances (later window): %v", err)
	}
	if len(later) != 2 || later[0].EventID != weekly.ID || !later[0].StartsAt.Equal(at(15, 13)) {
		t.Errorf("later window = %+v, want the weekly instances on the 15th and 22nd", later)
	}
}

// TestInstancesWindowEdges: the window rule is the ListEvents overlap rule,
// per instance — [from, to), touching a boundary is out, straddling is in.
func TestInstancesWindowEdges(t *testing.T) {
	s := newEventStore(t)
	mustCreateEvent(t, s, "ends exactly at from", at(31, 10), at(31, 12), userAuthor)
	mustCreateEvent(t, s, "straddles from", at(31, 11), at(31, 13), userAuthor)
	mustCreateEvent(t, s, "starts exactly at to", at(31, 14), at(31, 15), userAuthor)

	got, err := s.Instances(at(31, 12), at(31, 14))
	if err != nil {
		t.Fatalf("instances: %v", err)
	}
	if len(got) != 1 || got[0].Title != "straddles from" {
		t.Errorf("window edge instances = %+v, want only the straddling event", got)
	}
}

// ── Fireable ──────────────────────────────────────────────────────────────────

// fireFixture seeds one executable calendar with a daily 17:00–21:00 UTC
// automation event, plus the distractors that must never fire: the same shape
// on a NON-executable calendar, and an automation-less event on the executable
// one.
func fireFixture(t *testing.T, s *Store) (Calendar, Event) {
	t.Helper()
	auto, err := s.CreateCalendar("Automations", "teal", userAuthor, true)
	if err != nil {
		t.Fatalf("create executable calendar: %v", err)
	}
	ev, err := s.CreateEvent(Event{
		Title: "fred-m2", CalendarID: auto.ID,
		StartsAt: at(1, 17), EndsAt: at(1, 21),
		RRule: "FREQ=DAILY", TZ: "UTC", Automation: "fred-m2",
	}, userAuthor)
	if err != nil {
		t.Fatalf("create automation event: %v", err)
	}
	if _, err := s.CreateEvent(Event{
		Title: "no automation", CalendarID: auto.ID,
		StartsAt: at(1, 17), EndsAt: at(1, 21),
		RRule: "FREQ=DAILY", TZ: "UTC",
	}, userAuthor); err != nil {
		t.Fatalf("create automation-less event: %v", err)
	}
	return auto, ev
}

// TestFireableActiveWindow: fireable is exactly the active instances
// (startsAt <= at < endsAt) of automation events on executable calendars,
// with the instance's bounds as the fire window.
func TestFireableActiveWindow(t *testing.T) {
	s := newEventStore(t)
	_, ev := fireFixture(t, s)

	// Mid-window on a later DAY than the master row: the recurrence is what
	// makes it fireable today.
	got, err := s.Fireable(at(10, 19))
	if err != nil {
		t.Fatalf("fireable: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("fireable = %+v, want exactly the automation event", got)
	}
	f := got[0]
	if f.Automation != "fred-m2" || f.EventID != ev.ID {
		t.Errorf("fireable = %+v, want fred-m2 / %s", f, ev.ID)
	}
	if !f.WindowStart.Equal(at(10, 17)) || !f.WindowEnd.Equal(at(10, 21)) {
		t.Errorf("window = %s → %s, want the day-10 instance bounds", f.WindowStart, f.WindowEnd)
	}

	// The window edges: startsAt is inclusive, endsAt exclusive.
	if got, _ = s.Fireable(at(10, 17)); len(got) != 1 {
		t.Errorf("at == startsAt fired %d, want 1 (inclusive start)", len(got))
	}
	if got, _ = s.Fireable(at(10, 21)); len(got) != 0 {
		t.Errorf("at == endsAt fired %d, want 0 (exclusive end)", len(got))
	}
	// Outside any instance: nothing.
	if got, _ = s.Fireable(at(10, 12)); len(got) != 0 {
		t.Errorf("outside the window fired %+v, want nothing", got)
	}
}

// TestFireableExcludesNonExecutable: the same automation-naming event on a
// plain calendar never fires — a lunch cannot fire a fetcher, and flipping the
// calendar off silences its events without touching them.
func TestFireableExcludesNonExecutable(t *testing.T) {
	s := newEventStore(t)
	auto, _ := fireFixture(t, s)

	// Turning the calendar off silences the automation without event edits.
	no := false
	if _, err := s.UpdateCalendar(auto.ID, CalendarPatch{Executable: &no}); err != nil {
		t.Fatalf("patch executable off: %v", err)
	}
	if got, err := s.Fireable(at(10, 19)); err != nil || len(got) != 0 {
		t.Errorf("fireable with the calendar off = %+v (%v), want nothing", got, err)
	}
	yes := true
	if _, err := s.UpdateCalendar(auto.ID, CalendarPatch{Executable: &yes}); err != nil {
		t.Fatalf("patch executable back on: %v", err)
	}
	if got, err := s.Fireable(at(10, 19)); err != nil || len(got) != 1 {
		t.Errorf("fireable with the calendar back on = %+v (%v), want the automation", got, err)
	}
}

// TestFireableSingleEvent: recurrence is not required — a one-off automation
// event fires during its single window.
func TestFireableSingleEvent(t *testing.T) {
	s := newEventStore(t)
	auto, err := s.CreateCalendar("Automations", "", userAuthor, true)
	if err != nil {
		t.Fatalf("create calendar: %v", err)
	}
	if _, err := s.CreateEvent(Event{
		Title: "one-off", CalendarID: auto.ID,
		StartsAt: at(3, 9), EndsAt: at(3, 10),
		Automation: "one-off",
	}, userAuthor); err != nil {
		t.Fatalf("create one-off automation: %v", err)
	}
	if got, _ := s.Fireable(at(3, 9).Add(30 * time.Minute)); len(got) != 1 || got[0].Automation != "one-off" {
		t.Errorf("one-off mid-window = %+v, want it fireable", got)
	}
	if got, _ := s.Fireable(at(4, 9)); len(got) != 0 {
		t.Errorf("one-off the day after = %+v, want nothing", got)
	}
}

// ── Validation ────────────────────────────────────────────────────────────────

// TestRecurrenceValidation pins the write-boundary rules in the store, so the
// REST path and the tool path cannot end up enforcing different ones.
func TestRecurrenceValidation(t *testing.T) {
	s := newEventStore(t)
	plain, err := s.CreateCalendar("Personal", "", userAuthor, false)
	if err != nil {
		t.Fatalf("create plain calendar: %v", err)
	}
	auto, err := s.CreateCalendar("Automations", "", userAuthor, true)
	if err != nil {
		t.Fatalf("create executable calendar: %v", err)
	}

	base := Event{Title: "x", StartsAt: at(1, 9), EndsAt: at(1, 10), CalendarID: plain.ID}
	cases := []struct {
		name string
		mut  func(*Event)
		want string // a fragment of the instructive error
	}{
		{"rrule without tz", func(e *Event) { e.RRule = "FREQ=DAILY" }, "tz"},
		{"bad tz", func(e *Event) { e.RRule = "FREQ=DAILY"; e.TZ = "Mars/Olympus" }, "tz"},
		{"unsupported rrule param", func(e *Event) { e.RRule = "FREQ=DAILY;BYHOUR=9"; e.TZ = "UTC" },
			"supported: FREQ, INTERVAL, COUNT, UNTIL, BYDAY, BYMONTHDAY, BYMONTH, BYSETPOS, WKST"},
		{"malformed rrule", func(e *Event) { e.RRule = "EVERYDAY"; e.TZ = "UTC" }, "KEY=VALUE"},
		{"automation on a non-executable calendar", func(e *Event) { e.Automation = "fred-m2" }, plain.Name},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := base
			tc.mut(&ev)
			_, err := s.CreateEvent(ev, userAuthor)
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("create = %v, want ErrInvalid", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not teach — want it to mention %q", err, tc.want)
			}
		})
	}

	// The valid forms of each rejected shape go through.
	ok, err := s.CreateEvent(Event{
		Title: "valid recurring automation", CalendarID: auto.ID,
		StartsAt: at(1, 9), EndsAt: at(1, 10),
		RRule: "FREQ=DAILY", TZ: "UTC", Automation: "fred-m2",
	}, userAuthor)
	if err != nil {
		t.Fatalf("valid recurring automation event rejected: %v", err)
	}

	// A patch is validated against the MERGED event: moving an automation
	// event to a non-executable calendar is a rejection that names the target.
	if _, err := s.UpdateEvent(ok.ID, EventPatch{CalendarID: &plain.ID}); !errors.Is(err, ErrInvalid) ||
		!strings.Contains(err.Error(), plain.Name) {
		t.Errorf("moving an automation event to a plain calendar = %v, want ErrInvalid naming %q", err, plain.Name)
	}
	// Stripping the rrule while leaving tz is fine; stripping tz while leaving
	// the rrule is not.
	empty := ""
	if _, err := s.UpdateEvent(ok.ID, EventPatch{TZ: &empty}); !errors.Is(err, ErrInvalid) {
		t.Errorf("clearing tz under a live rrule = %v, want ErrInvalid", err)
	}
	// Clearing automation with an explicit "" works, and then the move is legal.
	if _, err := s.UpdateEvent(ok.ID, EventPatch{Automation: &empty}); err != nil {
		t.Fatalf("clearing automation: %v", err)
	}
	cleared, err := s.UpdateEvent(ok.ID, EventPatch{CalendarID: &plain.ID})
	if err != nil {
		t.Fatalf("moving after clearing automation: %v", err)
	}
	if cleared.Automation != "" || cleared.CalendarID != plain.ID {
		t.Errorf("cleared+moved event = %+v", cleared)
	}
	// And clearing the rrule turns it back into a single event.
	if _, err := s.UpdateEvent(ok.ID, EventPatch{RRule: &empty}); err != nil {
		t.Fatalf("clearing rrule: %v", err)
	}
	if got, _ := s.GetEvent(ok.ID); got.RRule != "" {
		t.Errorf("rrule after clear = %q, want empty", got.RRule)
	}
}

// ── Change feed ───────────────────────────────────────────────────────────────

// TestRecurringEventUpdateEmitsOneChangeRow: recurrence adds NO new synced
// entities — instances are derived — so editing a recurring event is exactly
// one event row in the feed, and an executable flip is exactly one calendar
// row.
func TestRecurringEventUpdateEmitsOneChangeRow(t *testing.T) {
	s := newEventStore(t)
	auto, err := s.CreateCalendar("Automations", "", userAuthor, true)
	if err != nil {
		t.Fatalf("create calendar: %v", err)
	}
	ev, err := s.CreateEvent(Event{
		Title: "fred-m2", CalendarID: auto.ID,
		StartsAt: at(1, 17), EndsAt: at(1, 21),
		RRule: "FREQ=MONTHLY;BYDAY=4TU", TZ: "America/New_York", Automation: "fred-m2",
	}, userAuthor)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	mark := topSeq(t, s)
	rule := "FREQ=MONTHLY;BYDAY=3TU"
	if _, err := s.UpdateEvent(ev.ID, EventPatch{RRule: &rule}); err != nil {
		t.Fatalf("update rrule: %v", err)
	}
	expectRows(t, logAfter(t, s, mark), []changeRow{
		{"event", string(ev.ID), "updated"},
	}, "recurring-event rrule update")

	mark = topSeq(t, s)
	no := false
	if _, err := s.UpdateCalendar(auto.ID, CalendarPatch{Executable: &no}); err != nil {
		t.Fatalf("flip executable: %v", err)
	}
	expectRows(t, logAfter(t, s, mark), []changeRow{
		{"calendar", string(auto.ID), "updated"},
	}, "executable flip")
}
