package botnet

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

// The calendar projection: every undone deadline or recurring fact has exactly
// one event on the "Projects" calendar, maintained in the same transaction as
// the fact write. These tests pin the convergence — create, update, done,
// delete, rename, a dangling event and a repeated write all end at "one event
// per fact that needs one, none for the facts that do not".

// projectedEvents returns every event on the Projects calendar, or nothing if
// the calendar has never been ensured.
func projectedEvents(t *testing.T, s *Store) []Event {
	t.Helper()
	cal, err := s.CalendarByName(projectsCalendarName)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		t.Fatalf("projects calendar: %v", err)
	}
	all, err := s.ListEvents(time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	var out []Event
	for _, e := range all {
		if e.CalendarID == cal.ID {
			out = append(out, e)
		}
	}
	return out
}

// TestProjectionCreatesOneEventPerDatedFact: the calendar is ensured by the
// first fact that needs it, and the event mirrors the fact.
func TestProjectionCreatesOneEventPerDatedFact(t *testing.T) {
	s := newEventStore(t)
	p := mustProject(t, s, "Passports", "keep every passport valid")
	due := pt(2027, time.March, 14)

	f := mustFact(t, s, p.ID, Fact{Kind: FactDeadline, Title: "US passport expires",
		Due: due, LeadDays: 180, Body: "renew at the consulate"}, "bot_ADA")
	if f.EventID == "" {
		t.Fatal("a dated fact carries no eventId, so nothing points at its projection")
	}
	cal, err := s.CalendarByName(projectsCalendarName)
	if err != nil {
		t.Fatalf("projects calendar after the first dated fact: %v", err)
	}
	if cal.Executable {
		t.Error("the Projects calendar is executable; a passport expiry must not be able to fire an automation")
	}
	if cal.CreatedBy != userAuthor {
		t.Errorf("projects calendar createdBy = %q, want %q", cal.CreatedBy, userAuthor)
	}

	events := projectedEvents(t, s)
	if len(events) != 1 {
		t.Fatalf("projected %d events, want exactly 1", len(events))
	}
	ev := events[0]
	if ev.ID != f.EventID {
		t.Errorf("fact points at %q, but the projected event is %q", f.EventID, ev.ID)
	}
	if ev.Title != "Passports: US passport expires" {
		t.Errorf("event title = %q, want \"<project>: <fact>\"", ev.Title)
	}
	if !ev.StartsAt.Equal(due) || !ev.EndsAt.Equal(due.Add(time.Hour)) {
		t.Errorf("event runs %s → %s, want the due instant plus an hour", ev.StartsAt, ev.EndsAt)
	}
	if ev.Notes != "renew at the consulate" {
		t.Errorf("event notes = %q, want the fact's body", ev.Notes)
	}
	if ev.CreatedBy != "bot_ADA" {
		t.Errorf("event createdBy = %q, want the fact's author", ev.CreatedBy)
	}
	if ev.Automation != "" {
		t.Errorf("event automation = %q, want none", ev.Automation)
	}

	// A recurring fact carries its rule onto its event, so the month grid
	// repeats it exactly as the health derivation does.
	r := mustFact(t, s, p.ID, Fact{Kind: FactRecurring, Title: "China visa run",
		Due: pt(2027, time.January, 5), RRule: "FREQ=YEARLY", TZ: "Asia/Shanghai"}, userAuthor)
	rev, err := s.GetEvent(r.EventID)
	if err != nil {
		t.Fatalf("projected recurring event: %v", err)
	}
	if rev.RRule != "FREQ=YEARLY" || rev.TZ != "Asia/Shanghai" {
		t.Errorf("projected event rule = %q / %q, want the fact's", rev.RRule, rev.TZ)
	}
}

// TestProjectionSkipsUndatedAndDoneFacts: only an undone dated fact belongs on
// a calendar, and a project of milestones and notes never conjures one.
func TestProjectionSkipsUndatedAndDoneFacts(t *testing.T) {
	s := newEventStore(t)
	p := mustProject(t, s, "Shanghai Co", "")

	for _, f := range []Fact{
		{Kind: FactMilestone, Title: "notarize", Blocker: "waiting on the lawyer"},
		{Kind: FactNote, Title: "agent", Body: "corpsec@example.com"},
		{Kind: FactDeadline, Title: "already filed", Due: pt(2026, time.January, 5), Done: true},
	} {
		stored := mustFact(t, s, p.ID, f, userAuthor)
		if stored.EventID != "" {
			t.Errorf("a %s fact (done=%v) was projected onto the calendar", f.Kind, f.Done)
		}
	}
	if _, err := s.CalendarByName(projectsCalendarName); !errors.Is(err, ErrNotFound) {
		t.Errorf("the Projects calendar exists = %v, want it ensured only by a fact that needs it", err)
	}

	// A read must never create it either.
	if _, err := s.ListProjects(); err != nil {
		t.Fatalf("list: %v", err)
	}
	if _, _, err := s.GetProject(p.ID); err != nil {
		t.Fatalf("get: %v", err)
	}
	if _, err := s.CalendarByName(projectsCalendarName); !errors.Is(err, ErrNotFound) {
		t.Error("a read ensured the Projects calendar; reads must not create state")
	}
}

// TestProjectionFollowsTheFact: an update moves the event, marking the fact
// done deletes it, and un-marking it brings one back.
func TestProjectionFollowsTheFact(t *testing.T) {
	s := newEventStore(t)
	p := mustProject(t, s, "Passports", "")
	due := pt(2027, time.March, 14)
	f := mustFact(t, s, p.ID, Fact{Kind: FactDeadline, Title: "US passport expires", Due: due}, userAuthor)

	moved := pt(2027, time.April, 1)
	title := "US passport expires (renewed early)"
	body := "at the consulate"
	updated, err := s.UpdateFact(f.ID, FactPatch{Due: &moved, Title: &title, Body: &body})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.EventID != f.EventID {
		t.Errorf("the update replaced the event (%q → %q) instead of moving it", f.EventID, updated.EventID)
	}
	ev, err := s.GetEvent(updated.EventID)
	if err != nil {
		t.Fatalf("get moved event: %v", err)
	}
	if !ev.StartsAt.Equal(moved) || !ev.EndsAt.Equal(moved.Add(time.Hour)) {
		t.Errorf("event runs %s → %s, want the new due instant plus an hour", ev.StartsAt, ev.EndsAt)
	}
	if ev.Title != "Passports: "+title || ev.Notes != body {
		t.Errorf("event = %q / %q, want the fact's new title and body", ev.Title, ev.Notes)
	}
	if len(projectedEvents(t, s)) != 1 {
		t.Errorf("after an update the calendar holds %d events, want 1", len(projectedEvents(t, s)))
	}

	done := true
	settled, err := s.UpdateFact(f.ID, FactPatch{Done: &done})
	if err != nil {
		t.Fatalf("mark done: %v", err)
	}
	if settled.EventID != "" {
		t.Errorf("a done fact still points at event %q", settled.EventID)
	}
	if _, err := s.GetEvent(ev.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("the event of a done fact is still on the calendar: %v", err)
	}
	if got := projectedEvents(t, s); len(got) != 0 {
		t.Errorf("the calendar holds %d events after the only fact was done, want 0", len(got))
	}

	// Reopening the fact puts it back on the calendar.
	done = false
	reopened, err := s.UpdateFact(f.ID, FactPatch{Done: &done})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if reopened.EventID == "" || len(projectedEvents(t, s)) != 1 {
		t.Errorf("reopening the fact left eventId %q and %d events, want a fresh event",
			reopened.EventID, len(projectedEvents(t, s)))
	}
}

// TestProjectionIsIdempotent: writing the same fact twice converges on ONE
// event rather than accumulating one per write.
func TestProjectionIsIdempotent(t *testing.T) {
	s := newEventStore(t)
	p := mustProject(t, s, "Passports", "")
	f := mustFact(t, s, p.ID, Fact{Kind: FactDeadline, Title: "renew", Due: pt(2027, time.March, 14)}, userAuthor)

	body := "same body"
	for i := 0; i < 3; i++ {
		if _, err := s.UpdateFact(f.ID, FactPatch{Body: &body}); err != nil {
			t.Fatalf("update %d: %v", i, err)
		}
	}
	if got := projectedEvents(t, s); len(got) != 1 {
		t.Fatalf("three identical writes left %d events, want 1", len(got))
	}
	cals, err := s.ListCalendars()
	if err != nil {
		t.Fatalf("list calendars: %v", err)
	}
	if len(cals) != 1 {
		t.Errorf("the ensure ran more than once: %d calendars", len(cals))
	}
}

// TestProjectionRepairsADanglingEvent: the user can delete a projected event in
// the Calendar panel — it is an ordinary event row. The next fact write notices
// the pointer dangles and projects a fresh one rather than failing.
func TestProjectionRepairsADanglingEvent(t *testing.T) {
	s := newEventStore(t)
	p := mustProject(t, s, "Passports", "")
	f := mustFact(t, s, p.ID, Fact{Kind: FactDeadline, Title: "renew", Due: pt(2027, time.March, 14)}, userAuthor)

	if err := s.DeleteEvent(f.EventID); err != nil {
		t.Fatalf("delete the projected event as the UI would: %v", err)
	}
	body := "still true"
	repaired, err := s.UpdateFact(f.ID, FactPatch{Body: &body})
	if err != nil {
		t.Fatalf("update over a dangling event: %v", err)
	}
	if repaired.EventID == "" || repaired.EventID == f.EventID {
		t.Errorf("eventId after the repair = %q, want a fresh event (was %q)", repaired.EventID, f.EventID)
	}
	if got := projectedEvents(t, s); len(got) != 1 {
		t.Errorf("the repair left %d events, want 1", len(got))
	}
}

// TestProjectRenameRewritesEventTitles: the event titles carry the project's
// name, so a rename that did not rewrite them would leave the calendar naming a
// project that no longer exists.
func TestProjectRenameRewritesEventTitles(t *testing.T) {
	s := newEventStore(t)
	p := mustProject(t, s, "Passports", "")
	a := mustFact(t, s, p.ID, Fact{Kind: FactDeadline, Title: "US expires", Due: pt(2027, time.March, 14)}, userAuthor)
	b := mustFact(t, s, p.ID, Fact{Kind: FactRecurring, Title: "renewal reminder",
		Due: pt(2027, time.January, 5), RRule: "FREQ=YEARLY", TZ: "UTC"}, userAuthor)
	mustFact(t, s, p.ID, Fact{Kind: FactNote, Title: "consulate", Body: "phone number"}, userAuthor)

	name := "Travel documents"
	if _, err := s.UpdateProject(p.ID, ProjectPatch{Name: &name}); err != nil {
		t.Fatalf("rename: %v", err)
	}
	for _, f := range []Fact{a, b} {
		ev, err := s.GetEvent(f.EventID)
		if err != nil {
			t.Fatalf("get event of %q: %v", f.Title, err)
		}
		if !strings.HasPrefix(ev.Title, name+": ") {
			t.Errorf("event title = %q, want it to carry the new project name", ev.Title)
		}
	}
	// A goal-only patch is not a rename and must not touch the calendar.
	before, err := s.GetEvent(a.EventID)
	if err != nil {
		t.Fatalf("get event: %v", err)
	}
	goal := "keep them valid"
	if _, err := s.UpdateProject(p.ID, ProjectPatch{Goal: &goal}); err != nil {
		t.Fatalf("patch goal: %v", err)
	}
	after, err := s.GetEvent(a.EventID)
	if err != nil {
		t.Fatalf("get event: %v", err)
	}
	if after.Title != before.Title || !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Errorf("a goal-only patch rewrote the event: %+v", after)
	}
}

// TestDeleteFactAndProjectTakeTheirEvents: both cascades reach the calendar, so
// no projected event outlives the fact it mirrors.
func TestDeleteFactAndProjectTakeTheirEvents(t *testing.T) {
	s := newEventStore(t)
	p := mustProject(t, s, "Passports", "")
	a := mustFact(t, s, p.ID, Fact{Kind: FactDeadline, Title: "US expires", Due: pt(2027, time.March, 14)}, userAuthor)
	b := mustFact(t, s, p.ID, Fact{Kind: FactDeadline, Title: "UK expires", Due: pt(2028, time.March, 14)}, userAuthor)

	if err := s.DeleteFact(a.ID); err != nil {
		t.Fatalf("delete fact: %v", err)
	}
	if _, err := s.GetEvent(a.EventID); !errors.Is(err, ErrNotFound) {
		t.Errorf("the deleted fact's event survives: %v", err)
	}
	if got := projectedEvents(t, s); len(got) != 1 {
		t.Fatalf("after one delete the calendar holds %d events, want 1", len(got))
	}

	if err := s.DeleteProject(p.ID); err != nil {
		t.Fatalf("delete project: %v", err)
	}
	if _, err := s.GetEvent(b.EventID); !errors.Is(err, ErrNotFound) {
		t.Errorf("the deleted project's event survives: %v", err)
	}
	// The calendar itself stays: it is the user's now, and it self-heals into
	// existence anyway on the next dated fact.
	if _, err := s.CalendarByName(projectsCalendarName); err != nil {
		t.Errorf("the Projects calendar was deleted with the project: %v", err)
	}
}

// TestProjectedFactEmitsItsChangeRows extends the call-site oracle to the
// projection: a dated fact's write is THREE rows in one transaction, and the
// cascades tombstone the events too.
func TestProjectedFactEmitsItsChangeRows(t *testing.T) {
	s := newEventStore(t)
	p := mustProject(t, s, "Passports", "")

	mark := topSeq(t, s)
	f, err := s.CreateFact(p.ID, Fact{Kind: FactDeadline, Title: "US expires",
		Due: pt(2027, time.March, 14)}, userAuthor)
	if err != nil {
		t.Fatalf("create fact: %v", err)
	}
	cal, err := s.CalendarByName(projectsCalendarName)
	if err != nil {
		t.Fatalf("projects calendar: %v", err)
	}
	expectRows(t, logAfter(t, s, mark), []changeRow{
		{"calendar", string(cal.ID), "created"},
		{"event", string(f.EventID), "created"},
		{"fact", string(f.ID), "created"},
	}, "CreateFact (dated, first ever)")

	// A second dated fact reuses the calendar: no second ensure row.
	mark = topSeq(t, s)
	g, err := s.CreateFact(p.ID, Fact{Kind: FactDeadline, Title: "UK expires",
		Due: pt(2028, time.March, 14)}, userAuthor)
	if err != nil {
		t.Fatalf("create second fact: %v", err)
	}
	expectRows(t, logAfter(t, s, mark), []changeRow{
		{"event", string(g.EventID), "created"},
		{"fact", string(g.ID), "created"},
	}, "CreateFact (dated, calendar already ensured)")

	mark = topSeq(t, s)
	body := "at the consulate"
	if _, err := s.UpdateFact(f.ID, FactPatch{Body: &body}); err != nil {
		t.Fatalf("update fact: %v", err)
	}
	expectRows(t, logAfter(t, s, mark), []changeRow{
		{"event", string(f.EventID), "updated"},
		{"fact", string(f.ID), "updated"},
	}, "UpdateFact (dated)")

	mark = topSeq(t, s)
	done := true
	if _, err := s.UpdateFact(f.ID, FactPatch{Done: &done}); err != nil {
		t.Fatalf("mark done: %v", err)
	}
	expectRows(t, logAfter(t, s, mark), []changeRow{
		{"event", string(f.EventID), "destroyed"},
		{"fact", string(f.ID), "updated"},
	}, "UpdateFact (marked done)")

	mark = topSeq(t, s)
	name := "Travel documents"
	if _, err := s.UpdateProject(p.ID, ProjectPatch{Name: &name}); err != nil {
		t.Fatalf("rename: %v", err)
	}
	expectRows(t, logAfter(t, s, mark), []changeRow{
		{"project", string(p.ID), "updated"},
		{"event", string(g.EventID), "updated"},
	}, "UpdateProject (rename)")

	mark = topSeq(t, s)
	if err := s.DeleteProject(p.ID); err != nil {
		t.Fatalf("delete project: %v", err)
	}
	// The events go first, then the facts in insertion order (SQLite deletes a
	// table scan's rows by rowid), then the project itself.
	expectRows(t, logAfter(t, s, mark), []changeRow{
		{"event", string(g.EventID), "destroyed"},
		{"fact", string(f.ID), "destroyed"},
		{"fact", string(g.ID), "destroyed"},
		{"project", string(p.ID), "destroyed"},
	}, "DeleteProject (with a projection)")
}

// TestProjectedFactsAppearInInstances: the point of projecting at all is that a
// deadline shows up in the calendar the user already reads, expanded like any
// other event.
func TestProjectedFactsAppearInInstances(t *testing.T) {
	h := newHarness(t, &fakeLLM{})
	p := postProject(t, h, `{"name":"Singapore Co","goal":"good standing"}`)
	var fact Fact
	postExpect(t, http.StatusCreated, h.ts.URL+"/v1/projects/"+string(p.ID)+"/facts",
		`{"kind":"recurring","title":"annual return","due":"2027-03-14T04:00:00Z",`+
			`"rrule":"FREQ=YEARLY","tz":"Asia/Singapore","leadDays":180}`, &fact)

	var cals []Calendar
	get(t, h.ts.URL+"/v1/calendars", &cals)
	var projects *Calendar
	for i := range cals {
		if cals[i].Name == projectsCalendarName {
			projects = &cals[i]
		}
	}
	if projects == nil {
		t.Fatalf("GET /v1/calendars = %+v, want the Projects calendar", cals)
	}

	var instances []Instance
	get(t, h.ts.URL+"/v1/instances?from=2029-01-01T00:00:00Z&to=2029-12-31T00:00:00Z", &instances)
	if len(instances) != 1 {
		t.Fatalf("instances in 2029 = %+v, want the projected annual return", instances)
	}
	in := instances[0]
	if in.EventID != fact.EventID || in.CalendarID != projects.ID {
		t.Errorf("instance = %+v, want the fact's event on the Projects calendar", in)
	}
	if in.Title != "Singapore Co: annual return" || !in.Recurring {
		t.Errorf("instance title = %q (recurring %v), want the projected recurring title", in.Title, in.Recurring)
	}
	if !in.StartsAt.Equal(time.Date(2029, time.March, 14, 4, 0, 0, 0, time.UTC)) {
		t.Errorf("instance starts %s, want the 2029 occurrence", in.StartsAt)
	}
}
