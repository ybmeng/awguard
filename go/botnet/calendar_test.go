package botnet

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The calendar service, at three levels: the store (CRUD and the overlap rule),
// the bot's calendar tool (dispatch, instructive errors, the whole tool loop),
// and the REST face the Calendar panel calls.

// ── Store ─────────────────────────────────────────────────────────────────────

// at is a readable fixed instant, so an assertion names a wall-clock time
// rather than a duration offset from an unreproducible now.
func at(day, hour int) time.Time {
	return time.Date(2026, 8, day, hour, 0, 0, 0, time.UTC)
}

func newEventStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// mustCreateEvent books an event and fails the test if it could not.
func mustCreateEvent(t *testing.T, s *Store, title string, starts, ends time.Time, by string) Event {
	t.Helper()
	ev, err := s.CreateEvent(Event{Title: title, StartsAt: starts, EndsAt: ends}, by)
	if err != nil {
		t.Fatalf("create %q: %v", title, err)
	}
	return ev
}

// TestEventCRUDRoundTrip: an event survives the write, comes back the same, is
// patched a field at a time, and is gone after the delete.
func TestEventCRUDRoundTrip(t *testing.T) {
	s := newEventStore(t)

	ev, err := s.CreateEvent(Event{
		Title:    "Lunch with Alex",
		StartsAt: at(31, 12),
		EndsAt:   at(31, 13),
		Location: "the good taco place",
		Notes:    "he's paying",
	}, "bot_ADA")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !strings.HasPrefix(string(ev.ID), "evt_") || len(ev.ID) != len("evt_")+26 {
		t.Errorf("id = %q, want an evt_-prefixed ULID", ev.ID)
	}
	if ev.CreatedBy != "bot_ADA" {
		t.Errorf("createdBy = %q, want the caller the write path stamped", ev.CreatedBy)
	}
	if ev.CreatedAt.IsZero() || !ev.UpdatedAt.Equal(ev.CreatedAt) {
		t.Errorf("timestamps = %s / %s, want both stamped and equal on create", ev.CreatedAt, ev.UpdatedAt)
	}

	got, err := s.GetEvent(ev.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got != ev {
		t.Errorf("stored event = %+v, want %+v", got, ev)
	}

	// A one-field patch leaves everything else exactly as it was.
	title := "Lunch with Alex and Sam"
	patched, err := s.UpdateEvent(ev.ID, EventPatch{Title: &title})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if patched.Title != title {
		t.Errorf("title = %q, want the patched one", patched.Title)
	}
	if patched.Location != ev.Location || patched.Notes != ev.Notes ||
		!patched.StartsAt.Equal(ev.StartsAt) || !patched.EndsAt.Equal(ev.EndsAt) {
		t.Errorf("a title patch changed other fields: %+v, was %+v", patched, ev)
	}
	if patched.CreatedBy != ev.CreatedBy || !patched.CreatedAt.Equal(ev.CreatedAt) {
		t.Errorf("a patch rewrote the authorship: %+v", patched)
	}
	if patched.UpdatedAt.Before(ev.UpdatedAt) {
		t.Errorf("updatedAt went backwards: %s, was %s", patched.UpdatedAt, ev.UpdatedAt)
	}

	// "" is a real value: it clears the field rather than being ignored.
	empty := ""
	cleared, err := s.UpdateEvent(ev.ID, EventPatch{Location: &empty})
	if err != nil {
		t.Fatalf("clear location: %v", err)
	}
	if cleared.Location != "" {
		t.Errorf("location = %q, want the explicit empty value to clear it", cleared.Location)
	}
	if cleared.Notes != ev.Notes {
		t.Errorf("clearing the location also cleared the notes: %+v", cleared)
	}

	if err := s.DeleteEvent(ev.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.GetEvent(ev.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("get after delete = %v, want ErrNotFound", err)
	}
	// Deleting what is not there says so, rather than silently succeeding.
	if err := s.DeleteEvent(ev.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("second delete = %v, want ErrNotFound", err)
	}
	if _, err := s.UpdateEvent(ev.ID, EventPatch{Title: &title}); !errors.Is(err, ErrNotFound) {
		t.Errorf("update after delete = %v, want ErrNotFound", err)
	}
}

// TestEventValidation pins the rules that live in the store, so the REST path
// and the tool path cannot enforce different ones. Every rejection is
// ErrInvalid, which the server maps to 400.
func TestEventValidation(t *testing.T) {
	s := newEventStore(t)

	cases := []struct {
		name  string
		event Event
	}{
		{"empty title", Event{StartsAt: at(31, 12), EndsAt: at(31, 13)}},
		{"blank title", Event{Title: "   ", StartsAt: at(31, 12), EndsAt: at(31, 13)}},
		{"no start", Event{Title: "x", EndsAt: at(31, 13)}},
		{"no end", Event{Title: "x", StartsAt: at(31, 12)}},
		{"end before start", Event{Title: "x", StartsAt: at(31, 13), EndsAt: at(31, 12)}},
	}
	for _, c := range cases {
		if _, err := s.CreateEvent(c.event, userAuthor); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: create = %v, want ErrInvalid", c.name, err)
		}
	}

	// A zero-length event is legal: endsAt must not PRECEDE startsAt.
	if _, err := s.CreateEvent(Event{Title: "instant", StartsAt: at(31, 12), EndsAt: at(31, 12)}, userAuthor); err != nil {
		t.Errorf("a zero-length event was rejected: %v", err)
	}

	// A patch is validated against the MERGED event, not against the fields it
	// carried: moving only the start past the existing end is a rejection.
	ev := mustCreateEvent(t, s, "standup", at(31, 9), at(31, 10), userAuthor)
	late := at(31, 11)
	if _, err := s.UpdateEvent(ev.ID, EventPatch{StartsAt: &late}); !errors.Is(err, ErrInvalid) {
		t.Errorf("patching the start past the end = %v, want ErrInvalid", err)
	}
	if after, _ := s.GetEvent(ev.ID); !after.StartsAt.Equal(at(31, 9)) {
		t.Errorf("a rejected patch still moved the event: %s", after.StartsAt)
	}
}

// TestListEventsOverlapEdges pins the window rule exactly: an event is in
// [from, to) when endsAt > from AND startsAt < to. The edges are the whole
// point — an event that merely touches a boundary is out, and one already
// running when the window opens is in.
func TestListEventsOverlapEdges(t *testing.T) {
	s := newEventStore(t)

	// The window under test is day 31, 12:00 → 14:00.
	from, to := at(31, 12), at(31, 14)
	before := mustCreateEvent(t, s, "ends exactly at from", at(31, 10), at(31, 12), userAuthor)
	straddles := mustCreateEvent(t, s, "started yesterday, still running", at(30, 9), at(31, 13), userAuthor)
	inside := mustCreateEvent(t, s, "wholly inside", at(31, 12), at(31, 13), userAuthor)
	touchesEnd := mustCreateEvent(t, s, "starts exactly at to", at(31, 14), at(31, 15), userAuthor)
	after := mustCreateEvent(t, s, "wholly after", at(31, 16), at(31, 17), userAuthor)

	got, err := s.ListEvents(from, to)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	want := []EventID{straddles.ID, inside.ID} // sorted by startsAt: day 30 first
	if len(got) != len(want) {
		t.Fatalf("window returned %s, want the straddling and inside events", eventTitles(got))
	}
	for i, id := range want {
		if got[i].ID != id {
			t.Errorf("window[%d] = %q, want %q (start order)", i, got[i].Title, id)
		}
	}
	for _, out := range []Event{before, touchesEnd, after} {
		for _, in := range got {
			if in.ID == out.ID {
				t.Errorf("%q is outside [from, to) but was returned", out.Title)
			}
		}
	}

	// Unbounded ends: a zero bound means "no limit that way".
	if all, err := s.ListEvents(time.Time{}, time.Time{}); err != nil || len(all) != 5 {
		t.Errorf("unbounded list = %s (%v), want all five", eventTitles(all), err)
	}
	if tail, err := s.ListEvents(at(31, 15), time.Time{}); err != nil || len(tail) != 1 || tail[0].ID != after.ID {
		t.Errorf("open-ended from = %s (%v), want just the last event", eventTitles(tail), err)
	}
	if head, err := s.ListEvents(time.Time{}, at(31, 11)); err != nil || len(head) != 2 {
		t.Errorf("open-ended to = %s (%v), want the two that started before 11:00", eventTitles(head), err)
	}

	// An empty window is an empty list, not an error.
	if none, err := s.ListEvents(at(31, 20), at(31, 21)); err != nil || len(none) != 0 {
		t.Errorf("empty window = %s (%v), want nothing", eventTitles(none), err)
	}
}

// TestListEventsOrdersChronologically is the test for the storage DECISION: the
// range filter and the ordering are TEXT operations, so a sub-second time
// stored in RFC3339Nano's variable-length form would sort wrong ("12:00:00Z"
// after "12:00:00.5Z"). Truncating to fixed-width seconds is what makes
// lexicographic order agree with chronological order.
func TestListEventsOrdersChronologically(t *testing.T) {
	s := newEventStore(t)

	base := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	// Deliberately out of order, and deliberately carrying a fraction.
	second := mustCreateEvent(t, s, "second", base.Add(90*time.Second), base.Add(time.Hour), userAuthor)
	first := mustCreateEvent(t, s, "first", base.Add(500*time.Millisecond), base.Add(time.Hour), userAuthor)
	third := mustCreateEvent(t, s, "third", base.Add(2*time.Minute), base.Add(time.Hour), userAuthor)

	got, err := s.ListEvents(time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	want := []EventID{first.ID, second.ID, third.ID}
	for i, id := range want {
		if got[i].ID != id {
			t.Fatalf("order = %s, want first, second, third", eventTitles(got))
		}
	}
	// The fraction was dropped, not rounded up past the next event.
	if !got[0].StartsAt.Equal(base) {
		t.Errorf("stored start = %s, want it truncated to the second (%s)", got[0].StartsAt, base)
	}
	// A window that opens after the truncated start still finds it running.
	if in, _ := s.ListEvents(base.Add(time.Minute), base.Add(2*time.Hour)); len(in) != 3 {
		t.Errorf("mid-event window = %s, want all three still running", eventTitles(in))
	}
}

// TestEventsSurviveReopen: the calendar is a table, so it outlives the process.
func TestEventsSurviveReopen(t *testing.T) {
	path := t.TempDir() + "/calendar.db"
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ev := mustCreateEvent(t, s, "dentist", at(31, 9), at(31, 10), userAuthor)
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	got, err := reopened.GetEvent(ev.ID)
	if err != nil {
		t.Fatalf("get after reopen: %v", err)
	}
	if got != ev {
		t.Errorf("event after reopen = %+v, want %+v", got, ev)
	}
}

// TestDeleteBotLeavesItsEvents: events belong to the NET, not to the bot that
// booked them. Deleting a bot cascades to its messages and segments; an event
// it put on the user's calendar is not the bot's to take with it, and removing
// it would be data loss disguised as cleanup. CreatedBy is then an id that no
// longer resolves — the same condition the UI already handles for a bot whose
// model has left the roster.
func TestDeleteBotLeavesItsEvents(t *testing.T) {
	s := newEventStore(t)
	bot := newBot(t, s)
	ev := mustCreateEvent(t, s, "lunch the bot booked", at(31, 12), at(31, 13), string(bot.ID))

	if err := s.DeleteBot(bot.ID); err != nil {
		t.Fatalf("delete bot: %v", err)
	}
	got, err := s.GetEvent(ev.ID)
	if err != nil {
		t.Fatalf("the event went with the bot: %v", err)
	}
	if got.CreatedBy != string(bot.ID) {
		t.Errorf("createdBy = %q, want the departed bot's id preserved", got.CreatedBy)
	}
}

func eventTitles(events []Event) string {
	titles := make([]string, len(events))
	for i, e := range events {
		titles[i] = e.Title
	}
	return "[" + strings.Join(titles, " | ") + "]"
}

// ── The calendar tool ─────────────────────────────────────────────────────────

// TestCalendarToolCommands walks every command through the dispatch seam and
// checks the store afterwards — the tool is only worth anything if its writes
// are real.
func TestCalendarToolCommands(t *testing.T) {
	s := newEventStore(t)
	bot := newBot(t, s)
	tb := NewBotToolbox(s, bot.ID, nil)

	// create
	res, err := tb.Run(context.Background(), calendarToolName, json.RawMessage(
		`{"command":"create","title":"Lunch with Alex","start":"2026-08-31T12:00:00Z",`+
			`"end":"2026-08-31T13:30:00Z","location":"the good taco place","notes":"he's paying"}`))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !strings.HasPrefix(res.text, "created evt_") || !strings.Contains(res.text, "Lunch with Alex") {
		t.Errorf("create result = %q, want \"created <id>: <title> <start>\"", res.text)
	}
	events, err := s.ListEvents(time.Time{}, time.Time{})
	if err != nil || len(events) != 1 {
		t.Fatalf("store holds %s (%v), want the created event", eventTitles(events), err)
	}
	ev := events[0]
	// The CALLING bot is the author — the model never gets to name one.
	if ev.CreatedBy != string(bot.ID) {
		t.Errorf("createdBy = %q, want the calling bot %q", ev.CreatedBy, bot.ID)
	}
	if !ev.StartsAt.Equal(at(31, 12)) || !ev.EndsAt.Equal(time.Date(2026, 8, 31, 13, 30, 0, 0, time.UTC)) {
		t.Errorf("times = %s → %s, want the ones the model gave", ev.StartsAt, ev.EndsAt)
	}
	if ev.Location != "the good taco place" || ev.Notes != "he's paying" {
		t.Errorf("optional fields = %q / %q, want the ones the model gave", ev.Location, ev.Notes)
	}
	if !strings.Contains(res.text, string(ev.ID)) {
		t.Errorf("create result %q does not name the id the model needs to update it", res.text)
	}

	// list — the id it names is the one update and delete take
	res, err = tb.Run(context.Background(), calendarToolName, json.RawMessage(
		`{"command":"list","from":"2026-08-01T00:00:00Z","to":"2026-09-30T00:00:00Z"}`))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(res.text, string(ev.ID)) || !strings.Contains(res.text, "Lunch with Alex") {
		t.Errorf("list result = %q, want the event with its id", res.text)
	}
	if !strings.Contains(res.text, "the good taco place") {
		t.Errorf("list result = %q, want the location", res.text)
	}
	if !strings.Contains(res.text, string(bot.ID)) {
		t.Errorf("list result = %q, want the author, so a bot can tell its own bookings apart", res.text)
	}

	// update — partial, and it moves the row
	res, err = tb.Run(context.Background(), calendarToolName, json.RawMessage(
		`{"command":"update","event_id":"`+string(ev.ID)+`","start":"2026-08-31T12:30:00Z"}`))
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if !strings.HasPrefix(res.text, "updated "+string(ev.ID)) {
		t.Errorf("update result = %q, want it to name the event", res.text)
	}
	after, err := s.GetEvent(ev.ID)
	if err != nil {
		t.Fatalf("get after update: %v", err)
	}
	if !after.StartsAt.Equal(time.Date(2026, 8, 31, 12, 30, 0, 0, time.UTC)) {
		t.Errorf("start after update = %s, want 12:30", after.StartsAt)
	}
	if after.Title != ev.Title || after.Location != ev.Location {
		t.Errorf("a start-only update changed other fields: %+v", after)
	}

	// delete
	res, err = tb.Run(context.Background(), calendarToolName, json.RawMessage(
		`{"command":"delete","event_id":"`+string(ev.ID)+`"}`))
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if !strings.Contains(res.text, string(ev.ID)) {
		t.Errorf("delete result = %q, want it to name the event", res.text)
	}
	if _, err := s.GetEvent(ev.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("event still there after the tool deleted it: %v", err)
	}
}

// TestCalendarToolDefaultsEndToOneHour: a model asked to "book lunch at noon"
// reliably omits the end, so create supplies one rather than costing a loop
// iteration to say what it wants.
func TestCalendarToolDefaultsEndToOneHour(t *testing.T) {
	s := newEventStore(t)
	bot := newBot(t, s)
	tb := NewBotToolbox(s, bot.ID, nil)

	if _, err := tb.Run(context.Background(), calendarToolName, json.RawMessage(
		`{"command":"create","title":"lunch","start":"2026-08-31T12:00:00Z"}`)); err != nil {
		t.Fatalf("create: %v", err)
	}
	events, err := s.ListEvents(time.Time{}, time.Time{})
	if err != nil || len(events) != 1 {
		t.Fatalf("store holds %s (%v), want one event", eventTitles(events), err)
	}
	if got := events[0].EndsAt.Sub(events[0].StartsAt); got != calendarDefaultDuration {
		t.Errorf("default duration = %v, want %v", got, calendarDefaultDuration)
	}
}

// TestCalendarToolListLeadsWithNow: the "now:" line is the tool's whole reason
// to be usable — without it "book lunch tomorrow" has no anchor — so it is
// first whether or not there is anything on the calendar, and the default
// window looks forward.
func TestCalendarToolListLeadsWithNow(t *testing.T) {
	s := newEventStore(t)
	bot := newBot(t, s)
	tb := NewBotToolbox(s, bot.ID, nil)

	empty, err := tb.Run(context.Background(), calendarToolName, json.RawMessage(`{"command":"list"}`))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	anchor, rest, _ := strings.Cut(empty.text, "\n")
	stamp, ok := strings.CutPrefix(anchor, "now: ")
	if !ok {
		t.Fatalf("first line = %q, want the now: anchor", anchor)
	}
	got, err := time.Parse(time.RFC3339, stamp)
	if err != nil {
		t.Fatalf("now: line carries %q, which is not RFC3339: %v", stamp, err)
	}
	if d := time.Since(got); d < -time.Minute || d > time.Minute {
		t.Errorf("now: says %s, which is %v from the actual now", got, d)
	}
	if !strings.Contains(rest, "no events") {
		t.Errorf("empty calendar listed %q, want it to say so explicitly", rest)
	}

	// The default window is forward-looking: an event last week is not "coming
	// up", one tomorrow is.
	past := mustCreateEvent(t, s, "last week", time.Now().Add(-7*24*time.Hour), time.Now().Add(-7*24*time.Hour+time.Hour), userAuthor)
	soon := mustCreateEvent(t, s, "tomorrow", time.Now().Add(24*time.Hour), time.Now().Add(25*time.Hour), userAuthor)
	res, err := tb.Run(context.Background(), calendarToolName, json.RawMessage(`{"command":"list"}`))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.HasPrefix(res.text, "now: ") {
		t.Errorf("a non-empty listing = %q, want the now: line first too", res.text)
	}
	if !strings.Contains(res.text, string(soon.ID)) {
		t.Errorf("default window %q misses tomorrow's event", res.text)
	}
	if strings.Contains(res.text, string(past.ID)) {
		t.Errorf("default window %q includes last week's event", res.text)
	}
}

// TestCalendarToolValidation pins every instructive error at the Run seam: a
// malformed call answers the model with a correctable "error: ..." RESULT and a
// nil error, so no mistake the model can make fails a turn.
func TestCalendarToolValidation(t *testing.T) {
	s := newEventStore(t)
	bot := newBot(t, s)
	tb := NewBotToolbox(s, bot.ID, nil)
	existing := mustCreateEvent(t, s, "standup", at(31, 9), at(31, 10), userAuthor)

	cases := []struct{ name, args, want string }{
		{"bad json", `not json`,
			`error: arguments must be a JSON object like {"command": "list"}`},
		{"missing command", `{}`,
			"error: missing 'command' — valid: create, list, update, delete"},
		{"unknown command", `{"command":"reschedule"}`,
			"error: unknown command 'reschedule' — valid: create, list, update, delete"},
		{"create without title", `{"command":"create","start":"2026-08-31T12:00:00Z"}`,
			"error: 'create' requires a 'title' field"},
		{"create without start", `{"command":"create","title":"lunch"}`,
			"error: 'create' requires a 'start' field"},
		{"create with a bad start", `{"command":"create","title":"lunch","start":"tomorrow at noon"}`,
			`error: 'start' value "tomorrow at noon" is not an RFC3339 time like 2026-08-31T15:00:00Z`},
		{"create with a bad end", `{"command":"create","title":"lunch","start":"2026-08-31T12:00:00Z","end":"1pm"}`,
			`error: 'end' value "1pm" is not an RFC3339 time like 2026-08-31T15:00:00Z`},
		{"create ending before it starts",
			`{"command":"create","title":"lunch","start":"2026-08-31T13:00:00Z","end":"2026-08-31T12:00:00Z"}`,
			"error: endsAt 2026-08-31T12:00:00Z precedes startsAt 2026-08-31T13:00:00Z"},
		{"update without an id", `{"command":"update","title":"x"}`,
			"error: 'update' requires a 'event_id' field"},
		{"update with nothing to change", `{"command":"update","event_id":"` + string(existing.ID) + `"}`,
			"error: 'update' needs at least one of title, start, end, location, notes to change"},
		{"update of an unknown event", `{"command":"update","event_id":"evt_NOPE","title":"x"}`,
			"error: no such event — call list to see the current ids"},
		{"delete without an id", `{"command":"delete"}`,
			"error: 'delete' requires a 'event_id' field"},
		{"delete of an unknown event", `{"command":"delete","event_id":"evt_NOPE"}`,
			"error: no such event — call list to see the current ids"},
		{"list with a bad window", `{"command":"list","from":"last tuesday"}`,
			`error: 'from' value "last tuesday" is not an RFC3339 time like 2026-08-31T15:00:00Z`},
	}
	for _, c := range cases {
		got, err := tb.Run(context.Background(), calendarToolName, json.RawMessage(c.args))
		if err != nil {
			t.Errorf("%s: Run returned a turn-failing error %v, want an instructive result", c.name, err)
		}
		if got.text != c.want {
			t.Errorf("%s: result %q, want %q", c.name, got.text, c.want)
		}
	}

	// Nothing above wrote or removed anything.
	events, err := s.ListEvents(time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(events) != 1 || events[0].ID != existing.ID || events[0].Title != "standup" {
		t.Errorf("a rejected call changed the calendar: %s", eventTitles(events))
	}
}

// calendarCall is one assistant turn calling the calendar tool with the given
// raw arguments object.
func calendarCall(callID, args string) string {
	return `{"choices":[{"message":{"role":"assistant","content":"",` +
		`"tool_calls":[{"id":"` + callID + `","type":"function",` +
		`"function":{"name":"calendar","arguments":` + strconv.Quote(args) + `}}]}}]}`
}

// TestToolLoopBooksAnEvent walks the whole feature the way it will actually be
// used: the model checks the calendar, books something, then answers — and the
// event is in the store afterwards, authored by the bot that booked it.
func TestToolLoopBooksAnEvent(t *testing.T) {
	s := newEventStore(t)
	bot := newBot(t, s)
	mark := topSeq(t, s)

	sc := &scriptedUpstream{responses: []string{
		calendarCall("call_1", `{"command":"list"}`),
		calendarCall("call_2", `{"command":"create","title":"Lunch with Alex","start":"2026-08-31T12:00:00Z"}`),
		contentResponse("booked!"),
	}}
	or := newScriptedOpenRouter(t, sc)

	reply, err := or.Complete(context.Background(), Prompt{
		Bot:      bot,
		Messages: []Message{{Role: "user", Content: "book lunch with Alex on the 31st at noon UTC"}},
		Tools:    NewBotToolbox(s, bot.ID, nil),
	})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if reply.Content != "booked!" {
		t.Errorf("reply = %q, want the final answer", reply.Content)
	}
	if got := sc.requestCount(); got != 3 {
		t.Fatalf("the loop made %d requests, want 3 (list, create, answer)", got)
	}

	// The model was told what time it is, unconditionally — that is what makes
	// a relative booking resolvable at all.
	first := sc.request(t, 0)
	stamps := 0
	for _, m := range first.Messages {
		if m.Role == "system" && strings.HasPrefix(m.Content, "Current date and time: ") {
			stamps++
		}
	}
	if stamps != 1 {
		t.Errorf("request carried %d date-time lines, want exactly 1: %+v", stamps, first.Messages)
	}

	// The list result led with the anchor…
	if got := findToolResult(t, sc.request(t, 1), "call_1"); !strings.HasPrefix(got.Content, "now: ") {
		t.Errorf("list result = %q, want the now: anchor first", got.Content)
	}
	// …and the create acknowledged with the id.
	created := findToolResult(t, sc.request(t, 2), "call_2")
	if !strings.HasPrefix(created.Content, "created evt_") {
		t.Errorf("create result = %q, want the created acknowledgement", created.Content)
	}

	// The booking is durable, authored by the bot, and an hour long by default.
	events, err := s.ListEvents(time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("store holds %s, want the booked event", eventTitles(events))
	}
	ev := events[0]
	if ev.Title != "Lunch with Alex" || !ev.StartsAt.Equal(at(31, 12)) || !ev.EndsAt.Equal(at(31, 13)) {
		t.Errorf("booked event = %+v, want the model's lunch with a default hour", ev)
	}
	if ev.CreatedBy != string(bot.ID) {
		t.Errorf("createdBy = %q, want the bot that booked it (%q)", ev.CreatedBy, bot.ID)
	}
	// And a second client sees it: the write went through the triggers.
	expectRows(t, logAfter(t, s, mark),
		[]changeRow{{"event", string(ev.ID), "created"}}, "calendar tool create")
}

// TestToolsEndpointIncludesCalendar: the inspector reads /v1/tools, so the
// calendar must be there in its flat-schema shape. (The byte-for-byte no-drift
// guarantee is pinned by TestToolsEndpointServesTheWireTools.)
func TestToolsEndpointIncludesCalendar(t *testing.T) {
	h := newHarness(t, &fakeLLM{})
	var tools []wireTool
	get(t, h.ts.URL+"/v1/tools", &tools)

	var def wireToolFunction
	for _, tool := range tools {
		if tool.Function.Name == calendarToolName {
			def = tool.Function
		}
	}
	if def.Name == "" {
		t.Fatalf("/v1/tools served %d tools, none of them the calendar", len(tools))
	}

	props, _ := def.Parameters["properties"].(map[string]any)
	command, _ := props["command"].(map[string]any)
	var enum []string
	if raw, ok := command["enum"].([]any); ok {
		for _, v := range raw {
			enum = append(enum, v.(string))
		}
	}
	if strings.Join(enum, ",") != "create,list,update,delete" {
		t.Errorf("command enum = %v, want create,list,update,delete", enum)
	}
	if req, ok := def.Parameters["required"].([]any); !ok || len(req) != 1 || req[0] != "command" {
		t.Errorf("required = %v, want just command", def.Parameters["required"])
	}
	// Flat strings, no nested union — the whole reason this shape was chosen.
	for _, field := range []string{"event_id", "title", "start", "end", "location", "notes", "from", "to"} {
		spec, ok := props[field].(map[string]any)
		if !ok {
			t.Errorf("parameters miss the %q field", field)
			continue
		}
		if spec["type"] != "string" {
			t.Errorf("%q is %v, want a flat string", field, spec["type"])
		}
	}
	for _, must := range []string{`"create"`, `"list"`, `"update"`, `"delete"`, "RFC3339"} {
		if !strings.Contains(def.Description, must) {
			t.Errorf("tool description misses %s: %q", must, def.Description)
		}
	}
}

// ── REST ──────────────────────────────────────────────────────────────────────

// postEvent books one event over HTTP and returns it, asserting the 201.
func postEvent(t *testing.T, h *harness, body string) Event {
	t.Helper()
	var ev Event
	postExpect(t, http.StatusCreated, h.ts.URL+"/v1/events", body, &ev)
	return ev
}

func deleteRaw(t *testing.T, url string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodDelete, url, nil)
	if err != nil {
		t.Fatalf("DELETE %s: %v", url, err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE %s: %v", url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

// TestEventsAPIRoundTrip drives the API the Calendar panel uses: book an event,
// list it back, and get a sync token to poll from.
func TestEventsAPIRoundTrip(t *testing.T) {
	h := newHarness(t, &fakeLLM{})

	ev := postEvent(t, h, `{"title":"Lunch with Alex","startsAt":"2026-08-31T12:00:00Z",`+
		`"endsAt":"2026-08-31T13:00:00Z","location":"the good taco place"}`)
	if ev.CreatedBy != userAuthor {
		t.Errorf("createdBy = %q, want %q for a UI-created event", ev.CreatedBy, userAuthor)
	}
	if !strings.HasPrefix(string(ev.ID), "evt_") {
		t.Errorf("id = %q, want an evt_ id", ev.ID)
	}

	// The collection GET carries a sync token, like the other collections.
	resp, err := http.Get(h.ts.URL + "/v1/events")
	if err != nil {
		t.Fatalf("get events: %v", err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("X-BotNet-State"); got == "" {
		t.Error("GET /v1/events carries no X-BotNet-State, so a client cannot start syncing from it")
	}
	var listed []Event
	if err := json.NewDecoder(resp.Body).Decode(&listed); err != nil {
		t.Fatalf("decode events: %v", err)
	}
	if len(listed) != 1 || listed[0].ID != ev.ID || listed[0].Location != "the good taco place" {
		t.Fatalf("GET /v1/events = %+v, want the booked event", listed)
	}

	// The window filter is the same overlap rule the store implements.
	var windowed []Event
	get(t, h.ts.URL+"/v1/events?from=2026-08-31T13:00:00Z&to=2026-08-31T14:00:00Z", &windowed)
	if len(windowed) != 0 {
		t.Errorf("a window starting exactly at the event's end returned %+v, want nothing", windowed)
	}
	get(t, h.ts.URL+"/v1/events?from=2026-08-31T12:30:00Z", &windowed)
	if len(windowed) != 1 {
		t.Errorf("a window opening mid-event returned %+v, want the running event", windowed)
	}

	// An empty calendar is [] and never null — the client needs no nil case.
	if _, raw := deleteRaw(t, h.ts.URL+"/v1/events/"+string(ev.ID)); raw != "" {
		t.Errorf("DELETE body = %q, want it empty", raw)
	}
	if got := rawGet(t, h.ts.URL+"/v1/events"); strings.TrimSpace(got) != "[]" {
		t.Errorf("empty calendar = %s, want []", got)
	}
}

// TestEventsAPIPatchIsPartial: an omitted field is left alone, an explicitly
// empty one is cleared, and the whole event comes back either way.
func TestEventsAPIPatchIsPartial(t *testing.T) {
	h := newHarness(t, &fakeLLM{})
	ev := postEvent(t, h, `{"title":"standup","startsAt":"2026-08-31T09:00:00Z",`+
		`"endsAt":"2026-08-31T09:15:00Z","location":"zoom","notes":"daily"}`)

	var patched Event
	patch(t, h.ts.URL+"/v1/events/"+string(ev.ID), `{"startsAt":"2026-08-31T09:05:00Z"}`, &patched)
	if !patched.StartsAt.Equal(time.Date(2026, 8, 31, 9, 5, 0, 0, time.UTC)) {
		t.Errorf("startsAt = %s, want the patched time", patched.StartsAt)
	}
	if patched.Title != "standup" || patched.Location != "zoom" || patched.Notes != "daily" {
		t.Errorf("a time-only patch changed other fields: %+v", patched)
	}
	if patched.CreatedBy != userAuthor {
		t.Errorf("createdBy = %q, want it untouched by a patch", patched.CreatedBy)
	}

	// "" is a real value that clears the field. Each patch decodes into a FRESH
	// Event: location and notes are omitempty on the wire (per the contract), so
	// a cleared field is an ABSENT key, which would leave a reused struct's old
	// value in place and quietly pass this assertion.
	var cleared Event
	patch(t, h.ts.URL+"/v1/events/"+string(ev.ID), `{"location":""}`, &cleared)
	if cleared.Location != "" {
		t.Errorf("location = %q, want the explicit empty value to clear it", cleared.Location)
	}
	if cleared.Notes != "daily" {
		t.Errorf("clearing the location also cleared the notes: %+v", cleared)
	}
	if raw := rawGet(t, h.ts.URL+"/v1/events"); strings.Contains(raw, `"location"`) {
		t.Errorf("a cleared location still ships a key: %s", raw)
	}

	// createdBy in the body is not a field — it is ignored, not honoured.
	var impostor Event
	patch(t, h.ts.URL+"/v1/events/"+string(ev.ID), `{"createdBy":"bot_IMPOSTOR"}`, &impostor)
	if impostor.CreatedBy != userAuthor {
		t.Errorf("createdBy = %q, want the body to be unable to claim an author", impostor.CreatedBy)
	}
}

// TestEventsAPIValidation: every bad body is a 400 in the existing error shape,
// and a missing event is a 404 rather than a silent success.
func TestEventsAPIValidation(t *testing.T) {
	h := newHarness(t, &fakeLLM{})
	ev := postEvent(t, h, `{"title":"standup","startsAt":"2026-08-31T09:00:00Z","endsAt":"2026-08-31T09:15:00Z"}`)

	bad := []struct{ name, body string }{
		{"no title", `{"startsAt":"2026-08-31T09:00:00Z","endsAt":"2026-08-31T09:15:00Z"}`},
		{"empty title", `{"title":"  ","startsAt":"2026-08-31T09:00:00Z","endsAt":"2026-08-31T09:15:00Z"}`},
		{"no start", `{"title":"x","endsAt":"2026-08-31T09:15:00Z"}`},
		{"no end", `{"title":"x","startsAt":"2026-08-31T09:00:00Z"}`},
		{"unparseable start", `{"title":"x","startsAt":"tomorrow","endsAt":"2026-08-31T09:15:00Z"}`},
		{"end before start", `{"title":"x","startsAt":"2026-08-31T10:00:00Z","endsAt":"2026-08-31T09:00:00Z"}`},
		{"not json", `{`},
	}
	for _, c := range bad {
		code, raw := postRaw(t, h.ts.URL+"/v1/events", c.body)
		if code != http.StatusBadRequest {
			t.Errorf("%s: POST status %d, want 400: %s", c.name, code, raw)
			continue
		}
		var body map[string]string
		if err := json.Unmarshal([]byte(raw), &body); err != nil || body["error"] == "" {
			t.Errorf("%s: 400 body = %s, want the existing {\"error\":...} shape", c.name, raw)
		}
	}

	// A patch that would make the event invalid is a 400 and changes nothing.
	if code, raw := patchRaw(t, h.ts.URL+"/v1/events/"+string(ev.ID), `{"endsAt":"2026-08-01T00:00:00Z"}`); code != http.StatusBadRequest {
		t.Errorf("patching the end before the start = %d, want 400: %s", code, raw)
	}
	if code, raw := patchRaw(t, h.ts.URL+"/v1/events/"+string(ev.ID), `{"startsAt":"soon"}`); code != http.StatusBadRequest {
		t.Errorf("patching an unparseable time = %d, want 400: %s", code, raw)
	}
	after, err := h.store.GetEvent(ev.ID)
	if err != nil || !after.EndsAt.Equal(time.Date(2026, 8, 31, 9, 15, 0, 0, time.UTC)) {
		t.Errorf("a rejected patch still moved the event: %+v (%v)", after, err)
	}

	// A missing event is a 404 on both write verbs.
	if code, _ := patchRaw(t, h.ts.URL+"/v1/events/evt_NOPE", `{"title":"x"}`); code != http.StatusNotFound {
		t.Errorf("PATCH of an unknown event = %d, want 404", code)
	}
	if code, _ := deleteRaw(t, h.ts.URL+"/v1/events/evt_NOPE"); code != http.StatusNotFound {
		t.Errorf("DELETE of an unknown event = %d, want 404", code)
	}
	// A bad window is the caller's fault too.
	if code, _ := getRaw(t, h.ts.URL+"/v1/events?from=yesterday"); code != http.StatusBadRequest {
		t.Errorf("GET with an unparseable from = %d, want 400", code)
	}

	// The method-pattern mux 405s the rest for free; prove it.
	if code, _ := postRaw(t, h.ts.URL+"/v1/events/"+string(ev.ID), `{}`); code != http.StatusMethodNotAllowed {
		t.Errorf("POST to an event = %d, want 405", code)
	}
}

// TestEventsAPIDeleteReturns204: the delete answers with no content, and the
// event is gone from the collection.
func TestEventsAPIDeleteReturns204(t *testing.T) {
	h := newHarness(t, &fakeLLM{})
	ev := postEvent(t, h, `{"title":"cancel me","startsAt":"2026-08-31T09:00:00Z","endsAt":"2026-08-31T10:00:00Z"}`)

	if code, raw := deleteRaw(t, h.ts.URL+"/v1/events/"+string(ev.ID)); code != http.StatusNoContent {
		t.Fatalf("DELETE = %d, want 204: %s", code, raw)
	}
	var listed []Event
	get(t, h.ts.URL+"/v1/events", &listed)
	if len(listed) != 0 {
		t.Errorf("calendar still holds %+v after the delete", listed)
	}
}

// TestEventsInTheChangeFeed: the calendar is a fourth synced entity, so a second
// client learns about every event write from /v1/changes — including the
// tombstone, which is the only way it ever learns about a delete.
func TestEventsInTheChangeFeed(t *testing.T) {
	h := newHarness(t, &fakeLLM{})

	state, err := h.store.State()
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	ev := postEvent(t, h, `{"title":"Lunch with Alex","startsAt":"2026-08-31T12:00:00Z","endsAt":"2026-08-31T13:00:00Z"}`)

	var changes Changes
	get(t, h.ts.URL+"/v1/changes?since="+state, &changes)
	if len(changes.Changed.Events.Created) != 1 || changes.Changed.Events.Created[0] != string(ev.ID) {
		t.Fatalf("events bucket after a create = %+v, want the new event", changes.Changed.Events)
	}

	// An update coalesces to updated, not created.
	state = changes.NewState
	var patched Event
	patch(t, h.ts.URL+"/v1/events/"+string(ev.ID), `{"title":"Lunch with Alex and Sam"}`, &patched)
	get(t, h.ts.URL+"/v1/changes?since="+state, &changes)
	if len(changes.Changed.Events.Updated) != 1 || changes.Changed.Events.Updated[0] != string(ev.ID) {
		t.Errorf("events bucket after a patch = %+v, want it updated", changes.Changed.Events)
	}
	if len(changes.Changed.Events.Created) != 0 {
		t.Errorf("a patch reported a create: %+v", changes.Changed.Events)
	}

	// The delete is a tombstone.
	state = changes.NewState
	if code, raw := deleteRaw(t, h.ts.URL+"/v1/events/"+string(ev.ID)); code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", code, raw)
	}
	get(t, h.ts.URL+"/v1/changes?since="+state, &changes)
	if len(changes.Changed.Events.Destroyed) != 1 || changes.Changed.Events.Destroyed[0] != string(ev.ID) {
		t.Errorf("events bucket after a delete = %+v, want the tombstone", changes.Changed.Events)
	}

	// Born and gone inside one window stays invisible, like every other entity.
	state = changes.NewState
	short := postEvent(t, h, `{"title":"oops","startsAt":"2026-08-31T12:00:00Z","endsAt":"2026-08-31T13:00:00Z"}`)
	if code, _ := deleteRaw(t, h.ts.URL+"/v1/events/"+string(short.ID)); code != http.StatusNoContent {
		t.Fatal("delete of the short-lived event failed")
	}
	get(t, h.ts.URL+"/v1/changes?since="+state, &changes)
	b := changes.Changed.Events
	if len(b.Created)+len(b.Updated)+len(b.Destroyed) != 0 {
		t.Errorf("an event born and gone in one window reported %+v, want nothing", b)
	}
}

// TestEventsBucketIsAlwaysPresent: like the other buckets, the events lists are
// [] rather than null, so a client needs no nil case per bucket.
func TestEventsBucketIsAlwaysPresent(t *testing.T) {
	h := newHarness(t, &fakeLLM{})
	state, err := h.store.State()
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	createBot(t, h, "Ada") // move the log without touching the calendar

	raw := rawGet(t, h.ts.URL+"/v1/changes?since="+state)
	for _, want := range []string{`"events"`, `"created":[]`, `"destroyed":[]`} {
		if !strings.Contains(raw, want) {
			t.Errorf("changes body %s misses %s", raw, want)
		}
	}
}
