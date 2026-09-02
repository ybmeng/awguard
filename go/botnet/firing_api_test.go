package botnet

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Calendar-driven firing, wire half: the instances and fireable endpoints the
// UI and the execcal bridge read, the recurrence fields on the events and
// calendars REST faces, and the calendar tool's grown surface.

// ── REST: events and calendars carry the new fields ───────────────────────────

// TestEventsAPIRecurrenceFields: POST and PATCH accept rrule, tz and
// automation; PATCH clears with an explicit ""; and the stored values ride the
// Event JSON.
func TestEventsAPIRecurrenceFields(t *testing.T) {
	h := newHarness(t, &fakeLLM{})
	var auto Calendar
	postExpect(t, http.StatusOK, h.ts.URL+"/v1/calendars",
		`{"name":"Automations","executable":true}`, &auto)
	if !auto.Executable {
		t.Fatalf("POST /v1/calendars with executable:true = %+v, want it executable", auto)
	}

	ev := postEvent(t, h, `{"title":"fred-m2","calendarId":"`+string(auto.ID)+`",`+
		`"startsAt":"2026-08-25T17:00:00Z","endsAt":"2026-08-25T21:00:00Z",`+
		`"rrule":"FREQ=MONTHLY;BYDAY=4TU","tz":"America/New_York","automation":"fred-m2"}`)
	if ev.RRule != "FREQ=MONTHLY;BYDAY=4TU" || ev.TZ != "America/New_York" || ev.Automation != "fred-m2" {
		t.Fatalf("created event = %+v, want the recurrence fields stored", ev)
	}

	// The wire carries them; an eventless field is an absent key (omitempty).
	raw := rawGet(t, h.ts.URL+"/v1/events")
	for _, want := range []string{`"rrule":"FREQ=MONTHLY;BYDAY=4TU"`, `"tz":"America/New_York"`, `"automation":"fred-m2"`} {
		if !strings.Contains(raw, want) {
			t.Errorf("GET /v1/events %s misses %s", raw, want)
		}
	}

	// PATCH with "" clears; the event becomes a plain single event again.
	var cleared Event
	patch(t, h.ts.URL+"/v1/events/"+string(ev.ID), `{"rrule":"","tz":"","automation":""}`, &cleared)
	if cleared.RRule != "" || cleared.TZ != "" || cleared.Automation != "" {
		t.Errorf("cleared event = %+v, want all three fields empty", cleared)
	}
	if raw := rawGet(t, h.ts.URL+"/v1/events"); strings.Contains(raw, `"rrule"`) {
		t.Errorf("a cleared rrule still ships a key: %s", raw)
	}

	// Validation rides the existing 400 shape: a rule without a zone, a bad
	// zone, an unsupported param, an automation on a non-executable calendar.
	var personal Calendar
	postExpect(t, http.StatusOK, h.ts.URL+"/v1/calendars", `{"name":"Personal"}`, &personal)
	if personal.Executable {
		t.Errorf("POST without executable = %+v, want it not executable", personal)
	}
	bad := []struct{ name, body string }{
		{"rrule without tz", `{"title":"x","startsAt":"2026-08-25T17:00:00Z","endsAt":"2026-08-25T18:00:00Z","rrule":"FREQ=DAILY"}`},
		{"bad tz", `{"title":"x","startsAt":"2026-08-25T17:00:00Z","endsAt":"2026-08-25T18:00:00Z","rrule":"FREQ=DAILY","tz":"Mars/Olympus"}`},
		{"unsupported rrule param", `{"title":"x","startsAt":"2026-08-25T17:00:00Z","endsAt":"2026-08-25T18:00:00Z","rrule":"FREQ=DAILY;BYHOUR=9","tz":"UTC"}`},
		{"automation on a non-executable calendar", `{"title":"x","calendarId":"` + string(personal.ID) + `",` +
			`"startsAt":"2026-08-25T17:00:00Z","endsAt":"2026-08-25T18:00:00Z","automation":"fred-m2"}`},
	}
	for _, c := range bad {
		code, raw := postRaw(t, h.ts.URL+"/v1/events", c.body)
		if code != http.StatusBadRequest {
			t.Errorf("%s: POST = %d, want 400: %s", c.name, code, raw)
		}
	}

	// Calendars PATCH flips executable both ways.
	var flipped Calendar
	patch(t, h.ts.URL+"/v1/calendars/"+string(personal.ID), `{"executable":true}`, &flipped)
	if !flipped.Executable {
		t.Errorf("PATCH executable:true = %+v, want it executable", flipped)
	}
	if raw := rawGet(t, h.ts.URL + "/v1/calendars"); !strings.Contains(raw, `"executable":true`) {
		t.Errorf("GET /v1/calendars %s misses executable:true", raw)
	}
	patch(t, h.ts.URL+"/v1/calendars/"+string(personal.ID), `{"executable":false}`, &flipped)
	if flipped.Executable {
		t.Errorf("PATCH executable:false = %+v, want it back off", flipped)
	}
}

// ── REST: GET /v1/instances ───────────────────────────────────────────────────

// TestInstancesEndpoint: the expanded calendar over a bounded window — single
// events pass through, recurring events multiply, sorted by start, never null.
func TestInstancesEndpoint(t *testing.T) {
	h := newHarness(t, &fakeLLM{})
	postEvent(t, h, `{"title":"dentist","startsAt":"2026-08-03T09:00:00Z","endsAt":"2026-08-03T10:00:00Z"}`)
	weekly := postEvent(t, h, `{"title":"standup","startsAt":"2026-08-01T13:00:00Z","endsAt":"2026-08-01T14:00:00Z",`+
		`"rrule":"FREQ=WEEKLY;COUNT=8","tz":"UTC"}`)

	var got []Instance
	get(t, h.ts.URL+"/v1/instances?from=2026-08-01T00:00:00Z&to=2026-08-15T00:00:00Z", &got)
	if len(got) != 3 {
		t.Fatalf("instances = %+v, want two weekly + one single", got)
	}
	if got[0].EventID != weekly.ID || !got[0].Recurring ||
		got[1].Title != "dentist" || got[1].Recurring ||
		got[2].EventID != weekly.ID {
		t.Errorf("instances = %+v, want [weekly@1st, dentist@3rd, weekly@8th]", got)
	}
	if !got[2].StartsAt.Equal(time.Date(2026, 8, 8, 13, 0, 0, 0, time.UTC)) {
		t.Errorf("second weekly instance starts %s, want the 8th 13:00Z", got[2].StartsAt)
	}

	// An empty window is [] and never null.
	if raw := rawGet(t, h.ts.URL+"/v1/instances?from=2027-01-01T00:00:00Z&to=2027-01-02T00:00:00Z"); strings.TrimSpace(raw) != "[]" {
		t.Errorf("empty instances window = %s, want []", raw)
	}
}

// TestInstancesEndpointValidation: both bounds required, ordered, RFC3339,
// and capped at 400 days — each refusal a 400 in the existing error shape.
func TestInstancesEndpointValidation(t *testing.T) {
	h := newHarness(t, &fakeLLM{})
	bad := []struct{ name, query string }{
		{"no bounds", ""},
		{"missing to", "?from=2026-08-01T00:00:00Z"},
		{"missing from", "?to=2026-08-01T00:00:00Z"},
		{"unparseable from", "?from=yesterday&to=2026-08-15T00:00:00Z"},
		{"from equals to", "?from=2026-08-01T00:00:00Z&to=2026-08-01T00:00:00Z"},
		{"from after to", "?from=2026-08-15T00:00:00Z&to=2026-08-01T00:00:00Z"},
		{"window past the cap", "?from=2026-01-01T00:00:00Z&to=2027-02-10T00:00:00Z"},
	}
	for _, c := range bad {
		code, raw := getRaw(t, h.ts.URL+"/v1/instances"+c.query)
		if code != http.StatusBadRequest {
			t.Errorf("%s: GET = %d, want 400: %s", c.name, code, raw)
			continue
		}
		var body map[string]string
		if err := json.Unmarshal([]byte(raw), &body); err != nil || body["error"] == "" {
			t.Errorf("%s: 400 body = %s, want the existing {\"error\":...} shape", c.name, raw)
		}
	}
	// A 400-day window is exactly legal.
	if code, raw := getRaw(t, h.ts.URL+"/v1/instances?from=2026-01-01T00:00:00Z&to=2027-02-05T00:00:00Z"); code != http.StatusOK {
		t.Errorf("a window at the cap = %d, want 200: %s", code, raw)
	}
}

// ── REST: GET /v1/fireable ────────────────────────────────────────────────────

// TestFireableEndpoint: execcal's one query — active automation instances on
// executable calendars, with the instance bounds as the window, never null.
func TestFireableEndpoint(t *testing.T) {
	h := newHarness(t, &fakeLLM{})
	var auto Calendar
	postExpect(t, http.StatusOK, h.ts.URL+"/v1/calendars",
		`{"name":"Automations","executable":true}`, &auto)
	ev := postEvent(t, h, `{"title":"fred-m2","calendarId":"`+string(auto.ID)+`",`+
		`"startsAt":"2026-08-01T17:00:00Z","endsAt":"2026-08-01T21:00:00Z",`+
		`"rrule":"FREQ=DAILY","tz":"UTC","automation":"fred-m2"}`)

	var due []Fireable
	get(t, h.ts.URL+"/v1/fireable?at=2026-08-10T19:00:00Z", &due)
	if len(due) != 1 {
		t.Fatalf("fireable = %+v, want the one automation", due)
	}
	if due[0].Automation != "fred-m2" || due[0].EventID != ev.ID ||
		!due[0].WindowStart.Equal(time.Date(2026, 8, 10, 17, 0, 0, 0, time.UTC)) ||
		!due[0].WindowEnd.Equal(time.Date(2026, 8, 10, 21, 0, 0, 0, time.UTC)) {
		t.Errorf("fireable = %+v, want fred-m2 with the day-10 instance window", due[0])
	}

	// Outside any instance: [] and never null.
	if raw := rawGet(t, h.ts.URL+"/v1/fireable?at=2026-08-10T12:00:00Z"); strings.TrimSpace(raw) != "[]" {
		t.Errorf("inactive fireable = %s, want []", raw)
	}
	// A bad at is the caller's fault.
	if code, _ := getRaw(t, h.ts.URL+"/v1/fireable?at=now-ish"); code != http.StatusBadRequest {
		t.Errorf("GET with an unparseable at = %d, want 400", code)
	}
}

// TestFireableEndpointDefaultsToNow: with no ?at, the query is "what should be
// running right now" — an event straddling the wall clock shows up.
func TestFireableEndpointDefaultsToNow(t *testing.T) {
	h := newHarness(t, &fakeLLM{})
	var auto Calendar
	postExpect(t, http.StatusOK, h.ts.URL+"/v1/calendars",
		`{"name":"Automations","executable":true}`, &auto)
	starts := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	ends := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	postEvent(t, h, `{"title":"live now","calendarId":"`+string(auto.ID)+`",`+
		`"startsAt":"`+starts+`","endsAt":"`+ends+`","automation":"live-now"}`)

	var due []Fireable
	get(t, h.ts.URL+"/v1/fireable", &due)
	if len(due) != 1 || due[0].Automation != "live-now" {
		t.Errorf("default-now fireable = %+v, want the live automation", due)
	}
}

// ── The calendar tool's grown surface ─────────────────────────────────────────

// TestCalendarToolRecurrence: bots manage the firing schedule — create with
// rrule/tz/automation, an executable calendar from the tool, and the list
// annotations that make the schedule legible.
func TestCalendarToolRecurrence(t *testing.T) {
	s := newEventStore(t)
	bot := newBot(t, s)
	tb := NewBotToolbox(s, bot.ID, nil)

	if got := runCal(t, tb, `{"command":"create_calendar","name":"Automations","color":"teal","executable":"true"}`); !strings.Contains(got, "Automations") {
		t.Fatalf("create_calendar = %q", got)
	}
	cal, err := s.CalendarByName("Automations")
	if err != nil || !cal.Executable {
		t.Fatalf("tool-created calendar = %+v (%v), want it executable", cal, err)
	}

	// create with the recurrence args; the store holds them.
	got := runCal(t, tb, `{"command":"create","title":"fred-m2","calendar":"Automations",`+
		`"start":"2026-08-25T17:00:00Z","end":"2026-08-25T21:00:00Z",`+
		`"rrule":"FREQ=MONTHLY;BYDAY=4TU","tz":"America/New_York","automation":"fred-m2"}`)
	if !strings.HasPrefix(got, "created evt_") {
		t.Fatalf("create = %q", got)
	}
	events, err := s.ListEvents(time.Time{}, time.Time{})
	if err != nil || len(events) != 1 {
		t.Fatalf("store holds %d events (%v), want 1", len(events), err)
	}
	ev := events[0]
	if ev.RRule != "FREQ=MONTHLY;BYDAY=4TU" || ev.TZ != "America/New_York" || ev.Automation != "fred-m2" {
		t.Errorf("tool-created event = %+v, want the recurrence args stored", ev)
	}

	// list annotates: the rule on recurring events, the automation it fires.
	listed := runCal(t, tb, `{"command":"list","from":"2026-08-20T00:00:00Z","to":"2026-08-30T00:00:00Z"}`)
	if !strings.Contains(listed, "FREQ=MONTHLY;BYDAY=4TU") {
		t.Errorf("list %q does not annotate the rrule", listed)
	}
	if !strings.Contains(listed, "(fires: fred-m2)") {
		t.Errorf("list %q does not annotate the automation", listed)
	}

	// update can change the rule; rename_calendar can flip executable off.
	if got := runCal(t, tb, `{"command":"update","event_id":"`+string(ev.ID)+`","rrule":"FREQ=MONTHLY;BYDAY=3TU"}`); !strings.HasPrefix(got, "updated ") {
		t.Fatalf("update rrule = %q", got)
	}
	if after, _ := s.GetEvent(ev.ID); after.RRule != "FREQ=MONTHLY;BYDAY=3TU" {
		t.Errorf("rrule after tool update = %q", after.RRule)
	}
	if got := runCal(t, tb, `{"command":"rename_calendar","calendar":"Automations","executable":"false"}`); !strings.Contains(got, "Automations") {
		t.Fatalf("rename_calendar executable=false = %q", got)
	}
	if cal, _ = s.CalendarByName("Automations"); cal.Executable {
		t.Error("rename_calendar did not flip executable off")
	}
}

// TestCalendarToolListExpandsInstances: the list window now operates on
// instances — a weekly event appears once per occurrence, not once per row.
func TestCalendarToolListExpandsInstances(t *testing.T) {
	s := newEventStore(t)
	bot := newBot(t, s)
	tb := NewBotToolbox(s, bot.ID, nil)

	if got := runCal(t, tb, `{"command":"create","title":"standup","start":"2026-08-03T13:00:00Z",`+
		`"rrule":"FREQ=WEEKLY;COUNT=8","tz":"UTC"}`); !strings.HasPrefix(got, "created ") {
		t.Fatalf("create = %q", got)
	}
	listed := runCal(t, tb, `{"command":"list","from":"2026-08-01T00:00:00Z","to":"2026-08-15T00:00:00Z"}`)
	if got := strings.Count(listed, "standup"); got != 2 {
		t.Errorf("a weekly event appears %d times in a two-week window, want 2:\n%s", got, listed)
	}
	if !strings.Contains(listed, "2 event(s)") {
		t.Errorf("list header %q counts rows, want instances", listed)
	}
}

// TestCalendarToolRecurrenceErrors pins the new instructive errors: every
// mistake a model can make comes back as a correctable "error: ..." result,
// and the rrule ones echo the supported subset.
func TestCalendarToolRecurrenceErrors(t *testing.T) {
	s := newEventStore(t)
	bot := newBot(t, s)
	tb := NewBotToolbox(s, bot.ID, nil)
	if _, err := s.CreateCalendar("Personal", "", userAuthor, false); err != nil {
		t.Fatalf("seed calendar: %v", err)
	}

	cases := []struct{ name, args, want string }{
		{"rrule without tz",
			`{"command":"create","title":"x","start":"2026-08-25T17:00:00Z","rrule":"FREQ=DAILY"}`,
			"tz"},
		{"unsupported rrule param",
			`{"command":"create","title":"x","start":"2026-08-25T17:00:00Z","rrule":"FREQ=DAILY;BYHOUR=9","tz":"UTC"}`,
			"supported: FREQ, INTERVAL, COUNT, UNTIL, BYDAY, BYMONTHDAY, BYMONTH, BYSETPOS, WKST"},
		{"automation on a non-executable calendar",
			`{"command":"create","title":"x","start":"2026-08-25T17:00:00Z","calendar":"Personal","automation":"fred-m2"}`,
			"Personal"},
		{"executable that is not a boolean",
			`{"command":"create_calendar","name":"Jobs","executable":"maybe"}`,
			`'executable' must be "true" or "false"`},
	}
	for _, c := range cases {
		got, err := tb.Run(t.Context(), calendarToolName, json.RawMessage(c.args))
		if err != nil {
			t.Errorf("%s: Run returned a turn-failing error %v, want an instructive result", c.name, err)
			continue
		}
		if !strings.HasPrefix(got.text, "error: ") || !strings.Contains(got.text, c.want) {
			t.Errorf("%s: result %q, want an error mentioning %q", c.name, got.text, c.want)
		}
	}

	// Nothing above wrote anything.
	if events, _ := s.ListEvents(time.Time{}, time.Time{}); len(events) != 0 {
		t.Errorf("a rejected call booked something: %s", eventTitles(events))
	}
	if cals, _ := s.ListCalendars(); len(cals) != 1 {
		t.Errorf("a rejected call created a calendar: %d", len(cals))
	}
}
