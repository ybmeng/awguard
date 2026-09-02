package botnet

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Projects, at the three levels the calendar service is tested at: the derived
// health function, the store (CRUD, validation, migration, the change feed),
// the REST face the Projects pane calls, and the bot's project tool.

// ── Health ────────────────────────────────────────────────────────────────────

// pt is a readable fixed instant, so an assertion names a date rather than an
// offset from an unreproducible now.
func pt(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 12, 0, 0, 0, time.UTC)
}

func deadlineFact(due time.Time, lead int) Fact {
	return Fact{Kind: FactDeadline, Title: "renew", Due: due, LeadDays: lead}
}

// TestProjectHealthPrecedence pins the derivation table: every precedence pair,
// the lead boundaries, done facts ignored, and zero facts.
func TestProjectHealthPrecedence(t *testing.T) {
	now := pt(2026, time.September, 1)
	soon := pt(2026, time.September, 20) // 19 days out
	far := pt(2027, time.September, 20)  // beyond any lead here
	past := pt(2026, time.August, 20)    // 12 days ago
	blocked := Fact{Kind: FactMilestone, Title: "notarize", Blocker: "waiting on the lawyer"}
	plain := Fact{Kind: FactMilestone, Title: "file form 1"}
	note := Fact{Kind: FactNote, Title: "n", Body: "the agent's phone number"}

	cases := []struct {
		name  string
		facts []Fact
		want  ProjectHealth
		next  time.Time // zero means "want nil"
	}{
		{"no facts", nil, HealthUnknown, time.Time{}},
		{"only a note", []Fact{note}, HealthOK, time.Time{}},
		{"only an unblocked milestone", []Fact{plain}, HealthOK, time.Time{}},
		{"a far deadline", []Fact{deadlineFact(far, 30)}, HealthOK, far},
		{"a near deadline", []Fact{deadlineFact(soon, 30)}, HealthDueSoon, soon},
		{"a passed deadline", []Fact{deadlineFact(past, 30)}, HealthOverdue, past},
		{"a blocked milestone", []Fact{blocked}, HealthBlocked, time.Time{}},

		{"overdue beats blocked", []Fact{blocked, deadlineFact(past, 30)}, HealthOverdue, past},
		{"blocked beats due_soon", []Fact{blocked, deadlineFact(soon, 30)}, HealthBlocked, soon},
		{"due_soon beats ok", []Fact{plain, deadlineFact(soon, 30)}, HealthDueSoon, soon},
		{"ok beats unknown once a fact exists", []Fact{note, plain}, HealthOK, time.Time{}},

		{"nextDue is the nearest of several", []Fact{
			deadlineFact(far, 30), deadlineFact(soon, 5), deadlineFact(pt(2026, time.October, 1), 5),
		}, HealthOK, soon},

		{"a done deadline is invisible", []Fact{
			func() Fact { f := deadlineFact(past, 30); f.Done = true; return f }(),
		}, HealthOK, time.Time{}},
		{"a done blocked milestone is invisible", []Fact{
			func() Fact { f := blocked; f.Done = true; return f }(),
		}, HealthOK, time.Time{}},
		{"a done deadline does not hide a live one", []Fact{
			func() Fact { f := deadlineFact(past, 30); f.Done = true; return f }(),
			deadlineFact(soon, 30),
		}, HealthDueSoon, soon},
	}
	for _, c := range cases {
		got, next := projectHealth(c.facts, now)
		if got != c.want {
			t.Errorf("%s: health = %q, want %q", c.name, got, c.want)
		}
		switch {
		case c.next.IsZero() && next != nil:
			t.Errorf("%s: nextDue = %s, want nil", c.name, *next)
		case !c.next.IsZero() && next == nil:
			t.Errorf("%s: nextDue = nil, want %s", c.name, c.next)
		case !c.next.IsZero() && next != nil && !next.Equal(c.next):
			t.Errorf("%s: nextDue = %s, want %s", c.name, *next, c.next)
		}
	}
}

// TestProjectHealthLeadBoundaries: the lead window is half-open at both ends —
// exactly Due-lead is already due_soon, exactly Due is already overdue. The
// second is the boundary a literal "Due < now" would drop on the floor, leaving
// a deadline that has arrived reported as fine.
func TestProjectHealthLeadBoundaries(t *testing.T) {
	due := pt(2026, time.September, 30)
	lead := 30
	cases := []struct {
		name string
		now  time.Time
		want ProjectHealth
	}{
		{"a second before the lead opens", due.AddDate(0, 0, -lead).Add(-time.Second), HealthOK},
		{"exactly at Due-lead", due.AddDate(0, 0, -lead), HealthDueSoon},
		{"a second inside the lead", due.AddDate(0, 0, -lead).Add(time.Second), HealthDueSoon},
		{"a second before Due", due.Add(-time.Second), HealthDueSoon},
		{"exactly at Due", due, HealthOverdue},
		{"a second after Due", due.Add(time.Second), HealthOverdue},
	}
	for _, c := range cases {
		got, _ := projectHealth([]Fact{deadlineFact(due, lead)}, c.now)
		if got != c.want {
			t.Errorf("%s: health = %q, want %q", c.name, got, c.want)
		}
	}
	// A zero lead means only the deadline itself counts: there is no due_soon
	// window at all, so the fact goes straight from ok to overdue.
	if got, _ := projectHealth([]Fact{deadlineFact(due, 0)}, due.Add(-time.Second)); got != HealthOK {
		t.Errorf("lead 0 a second early = %q, want ok", got)
	}
	if got, _ := projectHealth([]Fact{deadlineFact(due, 0)}, due); got != HealthOverdue {
		t.Errorf("lead 0 at the deadline = %q, want overdue", got)
	}
}

// TestProjectHealthRecurring: a recurring fact's health comes from its NEXT
// occurrence, not from the stored first one — which is what makes a Singapore
// annual return whose first filing was years ago still answer "due in March".
// A recurring fact is never overdue: the occurrence it names has not happened.
func TestProjectHealthRecurring(t *testing.T) {
	annual := Fact{
		Kind:     FactRecurring,
		Title:    "annual return",
		Due:      pt(2026, time.March, 14),
		RRule:    "FREQ=YEARLY",
		TZ:       "Asia/Singapore",
		LeadDays: 180,
	}
	next := pt(2027, time.March, 14)

	// 175 days out: inside the 180-day lead. The instant comes back in UTC —
	// the expander anchors an occurrence in the fact's zone, and a nextDue that
	// serialized as +08:00 while every other instant on the wire is "…Z" is a
	// second date format for a client to handle.
	if h, n := projectHealth([]Fact{annual}, pt(2026, time.September, 20)); h != HealthDueSoon || n == nil || !n.Equal(next) {
		t.Errorf("175 days before the next filing = %q / %v, want due_soon at %s", h, n, next)
	} else if n.Location() != time.UTC {
		t.Errorf("nextDue is in %s, want UTC", n.Location())
	}
	// 185 days out: outside it.
	if h, n := projectHealth([]Fact{annual}, pt(2026, time.September, 10)); h != HealthOK || n == nil || !n.Equal(next) {
		t.Errorf("185 days before the next filing = %q / %v, want ok at %s", h, n, next)
	}
	// The stored first occurrence is long past and must not read as overdue.
	if h, _ := projectHealth([]Fact{annual}, pt(2030, time.January, 5)); h == HealthOverdue {
		t.Error("a recurring fact whose first occurrence is years past reads as overdue")
	}

	// Across a month boundary, skipping a month the rule cannot land in: from
	// mid-February the next 31st is in March, not February.
	monthly := Fact{
		Kind:     FactRecurring,
		Title:    "rent review",
		Due:      pt(2026, time.January, 31),
		RRule:    "FREQ=MONTHLY;BYMONTHDAY=31",
		TZ:       "UTC",
		LeadDays: 60,
	}
	want := pt(2026, time.March, 31)
	if h, n := projectHealth([]Fact{monthly}, pt(2026, time.February, 15)); h != HealthDueSoon || n == nil || !n.Equal(want) {
		t.Errorf("next monthly-31 occurrence from mid-February = %q / %v, want due_soon at %s", h, n, want)
	}

	// An exhausted series contributes no date and no urgency.
	spent := Fact{Kind: FactRecurring, Title: "three filings", Due: pt(2026, time.January, 1),
		RRule: "FREQ=YEARLY;COUNT=2", TZ: "UTC", LeadDays: 30}
	if h, n := projectHealth([]Fact{spent}, pt(2030, time.January, 1)); h != HealthOK || n != nil {
		t.Errorf("an exhausted series = %q / %v, want ok and no nextDue", h, n)
	}
}

// ── Store ─────────────────────────────────────────────────────────────────────

func mustProject(t *testing.T, s *Store, name, goal string) Project {
	t.Helper()
	p, err := s.CreateProject(Project{Name: name, Goal: goal}, userAuthor)
	if err != nil {
		t.Fatalf("create project %q: %v", name, err)
	}
	return p
}

func mustFact(t *testing.T, s *Store, id ProjectID, f Fact, by string) Fact {
	t.Helper()
	stored, err := s.CreateFact(id, f, by)
	if err != nil {
		t.Fatalf("create fact %q: %v", f.Title, err)
	}
	return stored
}

// TestProjectCRUDRoundTrip: a project survives the write, comes back the same,
// is patched a field at a time, and takes its facts with it when deleted.
func TestProjectCRUDRoundTrip(t *testing.T) {
	s := newEventStore(t)

	p, err := s.CreateProject(Project{Name: "  Passports  ", Goal: "keep every passport valid"}, "bot_ADA")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !strings.HasPrefix(string(p.ID), "prj_") || len(p.ID) != len("prj_")+26 {
		t.Errorf("id = %q, want a prj_-prefixed ULID", p.ID)
	}
	if p.Name != "Passports" {
		t.Errorf("name = %q, want it trimmed", p.Name)
	}
	if p.CreatedBy != "bot_ADA" || p.Goal != "keep every passport valid" {
		t.Errorf("stored project = %+v, want the caller's goal and author", p)
	}
	if p.CreatedAt.IsZero() || !p.UpdatedAt.Equal(p.CreatedAt) {
		t.Errorf("timestamps = %s / %s, want both stamped and equal on create", p.CreatedAt, p.UpdatedAt)
	}
	// A project with no facts is unknown, not ok: there is nothing to be well.
	if p.Health != HealthUnknown || p.NextDue != nil || p.FactCount != 0 {
		t.Errorf("a fresh project = %q / %v / %d, want unknown, no date, no facts", p.Health, p.NextDue, p.FactCount)
	}

	got, facts, err := s.GetProject(p.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(facts) != 0 {
		t.Errorf("a fresh project has %d facts, want none", len(facts))
	}
	if got.ID != p.ID || got.Name != p.Name || got.Goal != p.Goal {
		t.Errorf("stored project = %+v, want %+v", got, p)
	}

	byName, err := s.ProjectByName("  passports ")
	if err != nil {
		t.Fatalf("by name: %v", err)
	}
	if byName.ID != p.ID {
		t.Errorf("ProjectByName resolved %q, want %q — the lookup must be case-insensitive", byName.ID, p.ID)
	}

	goal := "renew before every trip"
	patched, err := s.UpdateProject(p.ID, ProjectPatch{Goal: &goal})
	if err != nil {
		t.Fatalf("patch goal: %v", err)
	}
	if patched.Goal != goal || patched.Name != "Passports" {
		t.Errorf("a goal patch = %+v, want only the goal moved", patched)
	}
	if patched.CreatedBy != "bot_ADA" || !patched.CreatedAt.Equal(p.CreatedAt) {
		t.Errorf("a patch rewrote the authorship: %+v", patched)
	}
	name := "Travel documents"
	if patched, err = s.UpdateProject(p.ID, ProjectPatch{Name: &name}); err != nil {
		t.Fatalf("patch name: %v", err)
	}
	if patched.Name != name || patched.Goal != goal {
		t.Errorf("a name patch = %+v, want only the name moved", patched)
	}
	// Renaming a project to its own name is not a duplicate of itself.
	if _, err := s.UpdateProject(p.ID, ProjectPatch{Name: &name}); err != nil {
		t.Errorf("renaming a project to its own name: %v", err)
	}

	f := mustFact(t, s, p.ID, Fact{Kind: FactDeadline, Title: "US passport expires",
		Due: pt(2027, time.March, 14), LeadDays: 180}, userAuthor)
	if !strings.HasPrefix(string(f.ID), "fct_") || len(f.ID) != len("fct_")+26 {
		t.Errorf("fact id = %q, want a fct_-prefixed ULID", f.ID)
	}
	if f.ProjectID != p.ID || f.CreatedBy != userAuthor {
		t.Errorf("stored fact = %+v, want it owned by the project and stamped by the write path", f)
	}

	if err := s.DeleteProject(p.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, _, err := s.GetProject(p.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("get after delete = %v, want ErrNotFound", err)
	}
	if err := s.DeleteProject(p.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("second delete = %v, want ErrNotFound", err)
	}
	remaining, err := s.ListFacts(p.ID)
	if err != nil {
		t.Fatalf("list facts after delete: %v", err)
	}
	if len(remaining) != 0 {
		t.Errorf("delete left %d facts behind, want the cascade to take them", len(remaining))
	}
}

// TestProjectNameValidation: names are trimmed, bounded and unique
// case-insensitively. A duplicate is its OWN error, because the caller fixes it
// by choosing another name — that is a 409, not a 400 about a malformed field.
func TestProjectNameValidation(t *testing.T) {
	s := newEventStore(t)
	mustProject(t, s, "Passports", "")

	for _, c := range []struct{ name, arg string }{
		{"empty", ""},
		{"blank", "   "},
		{"too long", strings.Repeat("x", 65)},
	} {
		if _, err := s.CreateProject(Project{Name: c.arg}, userAuthor); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s name: create = %v, want ErrInvalid", c.name, err)
		}
	}
	if _, err := s.CreateProject(Project{Name: strings.Repeat("y", 64)}, userAuthor); err != nil {
		t.Errorf("a 64-char name was rejected: %v", err)
	}
	for _, dup := range []string{"Passports", "passports", "  PASSPORTS  "} {
		if _, err := s.CreateProject(Project{Name: dup}, userAuthor); !errors.Is(err, ErrDuplicateName) {
			t.Errorf("duplicate %q: create = %v, want ErrDuplicateName", dup, err)
		}
	}
	other := mustProject(t, s, "Singapore Co", "")
	taken := "passports"
	if _, err := s.UpdateProject(other.ID, ProjectPatch{Name: &taken}); !errors.Is(err, ErrDuplicateName) {
		t.Errorf("renaming onto a taken name = %v, want ErrDuplicateName", err)
	}
}

// TestFactValidation pins the per-kind table: each kind's required fields, and
// the fields it must never carry. Illegal states are rejected, never coerced.
func TestFactValidation(t *testing.T) {
	s := newEventStore(t)
	p := mustProject(t, s, "Singapore Co", "")
	due := pt(2027, time.March, 14)

	bad := []struct {
		name string
		fact Fact
	}{
		{"unknown kind", Fact{Kind: "reminder", Title: "x"}},
		{"empty kind", Fact{Title: "x"}},
		{"empty title", Fact{Kind: FactNote, Title: "  "}},
		{"title too long", Fact{Kind: FactNote, Title: strings.Repeat("x", 121)}},
		{"negative lead", Fact{Kind: FactDeadline, Title: "x", Due: due, LeadDays: -1}},
		{"deadline without a due", Fact{Kind: FactDeadline, Title: "x"}},
		{"recurring without a due", Fact{Kind: FactRecurring, Title: "x", RRule: "FREQ=YEARLY", TZ: "UTC"}},
		{"recurring without an rrule", Fact{Kind: FactRecurring, Title: "x", Due: due, TZ: "UTC"}},
		{"recurring without a tz", Fact{Kind: FactRecurring, Title: "x", Due: due, RRule: "FREQ=YEARLY"}},
		{"recurring with a bad rrule", Fact{Kind: FactRecurring, Title: "x", Due: due, RRule: "FREQ=FORTNIGHTLY", TZ: "UTC"}},
		{"recurring with a bad tz", Fact{Kind: FactRecurring, Title: "x", Due: due, RRule: "FREQ=YEARLY", TZ: "Mars/Olympus"}},
		{"milestone with a due", Fact{Kind: FactMilestone, Title: "x", Due: due}},
		{"milestone with an rrule", Fact{Kind: FactMilestone, Title: "x", RRule: "FREQ=YEARLY"}},
		{"note with a due", Fact{Kind: FactNote, Title: "x", Due: due}},
		{"note with a blocker", Fact{Kind: FactNote, Title: "x", Blocker: "the lawyer"}},
		{"note marked done", Fact{Kind: FactNote, Title: "x", Done: true}},
		{"deadline with a blocker", Fact{Kind: FactDeadline, Title: "x", Due: due, Blocker: "the lawyer"}},
		{"recurring marked done", Fact{Kind: FactRecurring, Title: "x", Due: due, RRule: "FREQ=YEARLY", TZ: "UTC", Done: true}},
	}
	for _, c := range bad {
		_, err := s.CreateFact(p.ID, c.fact, userAuthor)
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: create = %v, want ErrInvalid", c.name, err)
			continue
		}
		// The message must name the field, or the model and the UI get "invalid".
		for _, field := range []string{"kind", "title", "due", "rrule", "tz", "blocker", "done", "lead"} {
			if strings.Contains(c.name, field) && !strings.Contains(err.Error(), field) {
				t.Errorf("%s: error %q does not name %q", c.name, err, field)
			}
		}
	}

	good := []Fact{
		{Kind: FactDeadline, Title: "ACRA filing", Due: due},
		{Kind: FactDeadline, Title: "done already", Due: due, Done: true},
		{Kind: FactRecurring, Title: "annual return", Due: due, RRule: "FREQ=YEARLY", TZ: "Asia/Singapore"},
		{Kind: FactMilestone, Title: "notarize", Blocker: "waiting on the lawyer"},
		{Kind: FactMilestone, Title: "open the bank account", Done: true},
		{Kind: FactNote, Title: "agent", Body: "corpsec@example.com"},
	}
	for _, f := range good {
		if _, err := s.CreateFact(p.ID, f, userAuthor); err != nil {
			t.Errorf("a legal %s fact %q was rejected: %v", f.Kind, f.Title, err)
		}
	}

	// The same rules bind a patch, checked against the MERGED fact.
	milestone, err := s.CreateFact(p.ID, Fact{Kind: FactMilestone, Title: "sign the lease"}, userAuthor)
	if err != nil {
		t.Fatalf("create milestone: %v", err)
	}
	if _, err := s.UpdateFact(milestone.ID, FactPatch{Due: &due}); !errors.Is(err, ErrInvalid) {
		t.Errorf("patching a due onto a milestone = %v, want ErrInvalid", err)
	}
	blank := " "
	if _, err := s.UpdateFact(milestone.ID, FactPatch{Title: &blank}); !errors.Is(err, ErrInvalid) {
		t.Errorf("patching a blank title = %v, want ErrInvalid", err)
	}
	// A fact of an unknown id is a 404's worth of not-found, not a validation error.
	if _, err := s.UpdateFact("fct_NOPE", FactPatch{Title: &blank}); !errors.Is(err, ErrNotFound) {
		t.Errorf("patching an unknown fact = %v, want ErrNotFound", err)
	}
	if err := s.DeleteFact("fct_NOPE"); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleting an unknown fact = %v, want ErrNotFound", err)
	}
	if _, err := s.CreateFact("prj_NOPE", Fact{Kind: FactNote, Title: "x"}, userAuthor); !errors.Is(err, ErrNotFound) {
		t.Errorf("a fact on an unknown project = %v, want ErrNotFound", err)
	}
}

// TestFactLeadDaysDefault: a deadline created with no lead gets the 30-day
// default, and an explicit zero survives as a real zero rather than being
// re-defaulted.
func TestFactLeadDaysDefault(t *testing.T) {
	s := newEventStore(t)
	p := mustProject(t, s, "Passports", "")
	due := pt(2027, time.March, 14)

	f := mustFact(t, s, p.ID, Fact{Kind: FactDeadline, Title: "no lead given", Due: due}, userAuthor)
	if f.LeadDays != defaultLeadDays {
		t.Errorf("lead days = %d, want the %d-day default", f.LeadDays, defaultLeadDays)
	}
	zero := 0
	cleared, err := s.UpdateFact(f.ID, FactPatch{LeadDays: &zero})
	if err != nil {
		t.Fatalf("patch lead: %v", err)
	}
	if cleared.LeadDays != 0 {
		t.Errorf("lead days after an explicit zero = %d, want 0", cleared.LeadDays)
	}
	// A milestone has no lead to default; it stays zero.
	m := mustFact(t, s, p.ID, Fact{Kind: FactMilestone, Title: "book the appointment"}, userAuthor)
	if m.LeadDays != 0 {
		t.Errorf("a milestone's lead = %d, want 0 — only dated kinds get the default", m.LeadDays)
	}
}

// TestListProjectsSortsByUrgency: health precedence first, then the nearest due
// date, then the name — so the sidebar's top row is always the thing to look at.
func TestListProjectsSortsByUrgency(t *testing.T) {
	s := newEventStore(t)
	now := time.Now().UTC()

	empty := mustProject(t, s, "Empty", "")
	ok := mustProject(t, s, "Fine", "")
	mustFact(t, s, ok.ID, Fact{Kind: FactDeadline, Title: "far off",
		Due: now.AddDate(2, 0, 0), LeadDays: 30}, userAuthor)
	soonLater := mustProject(t, s, "Soon later", "")
	mustFact(t, s, soonLater.ID, Fact{Kind: FactDeadline, Title: "in 20 days",
		Due: now.AddDate(0, 0, 20), LeadDays: 30}, userAuthor)
	soonSooner := mustProject(t, s, "Soon sooner", "")
	mustFact(t, s, soonSooner.ID, Fact{Kind: FactDeadline, Title: "in 5 days",
		Due: now.AddDate(0, 0, 5), LeadDays: 30}, userAuthor)
	blocked := mustProject(t, s, "Blocked", "")
	mustFact(t, s, blocked.ID, Fact{Kind: FactMilestone, Title: "notarize",
		Blocker: "waiting on the lawyer"}, userAuthor)
	overdue := mustProject(t, s, "Overdue", "")
	mustFact(t, s, overdue.ID, Fact{Kind: FactDeadline, Title: "gone",
		Due: now.AddDate(0, 0, -3), LeadDays: 30}, userAuthor)

	got, err := s.ListProjects()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var names []string
	for _, p := range got {
		names = append(names, p.Name)
	}
	want := []string{"Overdue", "Blocked", "Soon sooner", "Soon later", "Fine", "Empty"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("list order = %v, want %v", names, want)
	}
	for _, p := range got {
		switch p.Name {
		case "Empty":
			if p.Health != HealthUnknown || p.FactCount != 0 {
				t.Errorf("Empty = %q / %d facts, want unknown / 0", p.Health, p.FactCount)
			}
		case "Overdue":
			if p.Health != HealthOverdue || p.NextDue == nil {
				t.Errorf("Overdue = %q / %v, want overdue with the date it passed", p.Health, p.NextDue)
			}
		case "Blocked":
			if p.Health != HealthBlocked || p.NextDue != nil || p.FactCount != 1 {
				t.Errorf("Blocked = %q / %v / %d, want blocked, no date, one fact", p.Health, p.NextDue, p.FactCount)
			}
		}
	}
	_ = empty
}

// TestListFactsUrgencyFirst pins the detail order the pane renders: undone
// dated facts by date, then blocked milestones, then the rest of the undone
// milestones, then notes, then everything done — newest first inside every
// undated group.
func TestListFactsUrgencyFirst(t *testing.T) {
	s := newEventStore(t)
	p := mustProject(t, s, "Shanghai Co", "")
	now := time.Now().UTC()

	// Created deliberately out of order, so passing cannot be an accident of
	// insertion order.
	oldNote := mustFact(t, s, p.ID, Fact{Kind: FactNote, Title: "older note", Body: "b"}, userAuthor)
	doneEarly := mustFact(t, s, p.ID, Fact{Kind: FactMilestone, Title: "reserve the name", Done: true}, userAuthor)
	later := mustFact(t, s, p.ID, Fact{Kind: FactDeadline, Title: "capital injection",
		Due: now.AddDate(0, 0, 60), LeadDays: 30}, userAuthor)
	plain := mustFact(t, s, p.ID, Fact{Kind: FactMilestone, Title: "open the bank account"}, userAuthor)
	sooner := mustFact(t, s, p.ID, Fact{Kind: FactDeadline, Title: "lease signed",
		Due: now.AddDate(0, 0, 10), LeadDays: 30}, userAuthor)
	blocked := mustFact(t, s, p.ID, Fact{Kind: FactMilestone, Title: "notarize",
		Blocker: "waiting on the lawyer"}, userAuthor)
	newNote := mustFact(t, s, p.ID, Fact{Kind: FactNote, Title: "newer note", Body: "b"}, userAuthor)
	doneLate := mustFact(t, s, p.ID, Fact{Kind: FactDeadline, Title: "articles filed",
		Due: now.AddDate(0, 0, -20), LeadDays: 30, Done: true}, userAuthor)

	facts, err := s.ListFacts(p.ID)
	if err != nil {
		t.Fatalf("list facts: %v", err)
	}
	var got []string
	for _, f := range facts {
		got = append(got, f.Title)
	}
	want := []string{
		sooner.Title, later.Title, // undone dated, by due
		blocked.Title,  // blocked milestone
		plain.Title,    // other undone milestone
		newNote.Title,  // notes, newest first
		oldNote.Title,  //
		doneLate.Title, // done, newest first
		doneEarly.Title,
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("fact order = %v,\n                want %v", got, want)
	}
}

// TestProjectsSurviveReopen: the tables are created by migrate, so a project
// written before a restart is still there after it, and re-running migrate over
// a database that already has them changes nothing.
func TestProjectsSurviveReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "projects.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	p := mustProject(t, s, "Passports", "keep them valid")
	f := mustFact(t, s, p.ID, Fact{Kind: FactDeadline, Title: "US passport expires",
		Due: pt(2027, time.March, 14), LeadDays: 180, Body: "renew at the consulate"}, "bot_ADA")
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if s, err = Open(path); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()
	got, facts, err := s.GetProject(p.ID)
	if err != nil {
		t.Fatalf("get after reopen: %v", err)
	}
	if got.Name != "Passports" || got.Goal != "keep them valid" || got.FactCount != 1 {
		t.Errorf("project after reopen = %+v, want it unchanged", got)
	}
	if len(facts) != 1 {
		t.Fatalf("facts after reopen = %d, want 1", len(facts))
	}
	if facts[0].ID != f.ID || !facts[0].Due.Equal(pt(2027, time.March, 14)) ||
		facts[0].LeadDays != 180 || facts[0].Body != "renew at the consulate" ||
		facts[0].CreatedBy != "bot_ADA" {
		t.Errorf("fact after reopen = %+v, want %+v", facts[0], f)
	}
}

// TestLegacyDatabaseGainsProjects: an existing database — one written before
// projects existed — opens, migrates and can hold a project, with no backfill
// conjuring one on a fresh DB.
func TestLegacyDatabaseGainsProjects(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	seedLegacyDB(t, path)
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open legacy: %v", err)
	}
	defer s.Close()

	before, err := s.ListProjects()
	if err != nil {
		t.Fatalf("list on a migrated legacy db: %v", err)
	}
	if len(before) != 0 {
		t.Errorf("migrating a legacy database invented %d projects, want none", len(before))
	}
	p := mustProject(t, s, "Passports", "")
	mustFact(t, s, p.ID, Fact{Kind: FactMilestone, Title: "book the appointment"}, userAuthor)
	after, err := s.ListProjects()
	if err != nil {
		t.Fatalf("list after write: %v", err)
	}
	if len(after) != 1 || after[0].FactCount != 1 {
		t.Errorf("legacy database after a project write = %+v, want one project with one fact", after)
	}
}

// TestProjectsInTheChangeFeed: both new entities are captured by triggers and
// reach a second client through their OWN buckets. A bucket the feed does not
// know is silently skipped, so this is what proves they are wired in.
func TestProjectsInTheChangeFeed(t *testing.T) {
	s := newEventStore(t)
	state0, err := s.State()
	if err != nil {
		t.Fatalf("state: %v", err)
	}

	p := mustProject(t, s, "Passports", "")
	f := mustFact(t, s, p.ID, Fact{Kind: FactMilestone, Title: "book the appointment"}, userAuthor)
	changes, err := s.ChangesSince(state0, 100)
	if err != nil {
		t.Fatalf("changes: %v", err)
	}
	if got := changes.Changed.Projects.Created; len(got) != 1 || got[0] != string(p.ID) {
		t.Errorf("projects created = %v, want [%s] — the bucket is not wired in", got, p.ID)
	}
	if got := changes.Changed.Facts.Created; len(got) != 1 || got[0] != string(f.ID) {
		t.Errorf("facts created = %v, want [%s] — the bucket is not wired in", got, f.ID)
	}

	// A field-only update fires the row trigger, like every other entity here.
	state1, err := s.State()
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	body := "at the consulate"
	if _, err := s.UpdateFact(f.ID, FactPatch{Body: &body}); err != nil {
		t.Fatalf("update fact: %v", err)
	}
	goal := "keep them valid"
	if _, err := s.UpdateProject(p.ID, ProjectPatch{Goal: &goal}); err != nil {
		t.Fatalf("update project: %v", err)
	}
	changes, err = s.ChangesSince(state1, 100)
	if err != nil {
		t.Fatalf("changes: %v", err)
	}
	if got := changes.Changed.Facts.Updated; len(got) != 1 || got[0] != string(f.ID) {
		t.Errorf("facts updated = %v, want [%s]", got, f.ID)
	}
	if got := changes.Changed.Projects.Updated; len(got) != 1 || got[0] != string(p.ID) {
		t.Errorf("projects updated = %v, want [%s]", got, p.ID)
	}

	// The delete cascade tombstones the facts as well as the project.
	state2, err := s.State()
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	if err := s.DeleteProject(p.ID); err != nil {
		t.Fatalf("delete project: %v", err)
	}
	changes, err = s.ChangesSince(state2, 100)
	if err != nil {
		t.Fatalf("changes: %v", err)
	}
	if got := changes.Changed.Facts.Destroyed; len(got) != 1 || got[0] != string(f.ID) {
		t.Errorf("facts destroyed = %v, want [%s] — a client would keep a fact of a dead project", got, f.ID)
	}
	if got := changes.Changed.Projects.Destroyed; len(got) != 1 || got[0] != string(p.ID) {
		t.Errorf("projects destroyed = %v, want [%s]", got, p.ID)
	}
}

// TestProjectBucketsAreAlwaysPresent: every bucket marshals as [] and never
// null, so a client needs no nil case per entity.
func TestProjectBucketsAreAlwaysPresent(t *testing.T) {
	h := newHarness(t, &fakeLLM{})
	state, err := h.store.State()
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	raw := rawGet(t, h.ts.URL+"/v1/changes?since="+state)
	for _, bucket := range []string{`"projects":{"created":[],"updated":[],"destroyed":[]}`,
		`"facts":{"created":[],"updated":[],"destroyed":[]}`} {
		if !strings.Contains(raw, bucket) {
			t.Errorf("/v1/changes body %s\n  misses %s", raw, bucket)
		}
	}
}

// ── REST ──────────────────────────────────────────────────────────────────────

func postProject(t *testing.T, h *harness, body string) Project {
	t.Helper()
	var p Project
	postExpect(t, http.StatusCreated, h.ts.URL+"/v1/projects", body, &p)
	return p
}

// projectDetail is the GET /v1/projects/{id} envelope.
type projectDetail struct {
	Project Project `json:"project"`
	Facts   []Fact  `json:"facts"`
}

// TestProjectsAPIRoundTrip drives the whole surface the pane calls.
func TestProjectsAPIRoundTrip(t *testing.T) {
	h := newHarness(t, &fakeLLM{})

	p := postProject(t, h, `{"name":"Passports","goal":"keep every passport valid"}`)
	if p.CreatedBy != userAuthor {
		t.Errorf("createdBy = %q, want %q for a UI-created project", p.CreatedBy, userAuthor)
	}
	if p.Health != HealthUnknown {
		t.Errorf("a fresh project's health = %q, want unknown", p.Health)
	}

	// The collection GET carries a sync token, like the other collections.
	resp, err := http.Get(h.ts.URL + "/v1/projects")
	if err != nil {
		t.Fatalf("get projects: %v", err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("X-BotNet-State"); got == "" {
		t.Error("GET /v1/projects carries no X-BotNet-State, so a client cannot start syncing from it")
	}
	var listed []Project
	if err := json.NewDecoder(resp.Body).Decode(&listed); err != nil {
		t.Fatalf("decode projects: %v", err)
	}
	if len(listed) != 1 || listed[0].ID != p.ID {
		t.Fatalf("GET /v1/projects = %+v, want the created project", listed)
	}

	var fact Fact
	postExpect(t, http.StatusCreated, h.ts.URL+"/v1/projects/"+string(p.ID)+"/facts",
		`{"kind":"deadline","title":"US passport expires","due":"2027-03-14T00:00:00Z","leadDays":180}`, &fact)
	if fact.ProjectID != p.ID || fact.CreatedBy != userAuthor || fact.LeadDays != 180 {
		t.Errorf("created fact = %+v, want it owned, authored and carrying its lead", fact)
	}

	var detail projectDetail
	get(t, h.ts.URL+"/v1/projects/"+string(p.ID), &detail)
	if detail.Project.FactCount != 1 || len(detail.Facts) != 1 || detail.Facts[0].ID != fact.ID {
		t.Errorf("detail = %+v, want the project with its one fact", detail)
	}
	if detail.Project.NextDue == nil || !detail.Project.NextDue.Equal(pt(2027, time.March, 14).Add(-12*time.Hour)) {
		t.Errorf("nextDue = %v, want the deadline's instant", detail.Project.NextDue)
	}

	var patched Project
	patch(t, h.ts.URL+"/v1/projects/"+string(p.ID), `{"goal":"renew before every trip"}`, &patched)
	if patched.Goal != "renew before every trip" || patched.Name != "Passports" {
		t.Errorf("patched project = %+v, want only the goal moved", patched)
	}

	var patchedFact Fact
	patch(t, h.ts.URL+"/v1/projects/"+string(p.ID)+"/facts/"+string(fact.ID), `{"done":true}`, &patchedFact)
	if !patchedFact.Done {
		t.Errorf("patched fact = %+v, want it done", patchedFact)
	}
	// A FRESH envelope: nextDue is omitted when there is none, and an absent key
	// would leave the reused struct's old date in place and quietly pass.
	var settled projectDetail
	get(t, h.ts.URL+"/v1/projects/"+string(p.ID), &settled)
	if settled.Project.Health != HealthOK || settled.Project.NextDue != nil {
		t.Errorf("after the only deadline was done: %q / %v, want ok and no nextDue",
			settled.Project.Health, settled.Project.NextDue)
	}

	if code, body := deleteRaw(t, h.ts.URL+"/v1/projects/"+string(p.ID)+"/facts/"+string(fact.ID)); code != http.StatusNoContent || body != "" {
		t.Errorf("DELETE fact = %d %q, want 204 with an empty body", code, body)
	}
	if code, _ := deleteRaw(t, h.ts.URL+"/v1/projects/"+string(p.ID)); code != http.StatusNoContent {
		t.Errorf("DELETE project = %d, want 204", code)
	}
	if got := rawGet(t, h.ts.URL+"/v1/projects"); strings.TrimSpace(got) != "[]" {
		t.Errorf("empty project list = %s, want []", got)
	}
}

// TestProjectsAPIErrors: every rejection the contract names, at its own status.
func TestProjectsAPIErrors(t *testing.T) {
	h := newHarness(t, &fakeLLM{})
	p := postProject(t, h, `{"name":"Passports"}`)
	factURL := h.ts.URL + "/v1/projects/" + string(p.ID) + "/facts"
	var fact Fact
	postExpect(t, http.StatusCreated, factURL, `{"kind":"milestone","title":"book it"}`, &fact)

	cases := []struct {
		name, method, url, body string
		want                    int
	}{
		{"no name", "POST", h.ts.URL + "/v1/projects", `{"goal":"x"}`, http.StatusBadRequest},
		{"blank name", "POST", h.ts.URL + "/v1/projects", `{"name":"   "}`, http.StatusBadRequest},
		{"duplicate name", "POST", h.ts.URL + "/v1/projects", `{"name":"passports"}`, http.StatusConflict},
		{"malformed body", "POST", h.ts.URL + "/v1/projects", `{`, http.StatusBadRequest},
		{"unknown project", "GET", h.ts.URL + "/v1/projects/prj_NOPE", "", http.StatusNotFound},
		{"patch unknown project", "PATCH", h.ts.URL + "/v1/projects/prj_NOPE", `{"goal":"x"}`, http.StatusNotFound},
		{"delete unknown project", "DELETE", h.ts.URL + "/v1/projects/prj_NOPE", "", http.StatusNotFound},
		{"fact on an unknown project", "POST", h.ts.URL + "/v1/projects/prj_NOPE/facts",
			`{"kind":"note","title":"x","body":"y"}`, http.StatusNotFound},
		{"unknown fact kind", "POST", factURL, `{"kind":"reminder","title":"x"}`, http.StatusBadRequest},
		{"deadline with no due", "POST", factURL, `{"kind":"deadline","title":"x"}`, http.StatusBadRequest},
		{"unparseable due", "POST", factURL, `{"kind":"deadline","title":"x","due":"next tuesday"}`, http.StatusBadRequest},
		{"milestone with a due", "POST", factURL,
			`{"kind":"milestone","title":"x","due":"2027-03-14T00:00:00Z"}`, http.StatusBadRequest},
		{"negative lead", "POST", factURL,
			`{"kind":"deadline","title":"x","due":"2027-03-14T00:00:00Z","leadDays":-1}`, http.StatusBadRequest},
		{"patch unknown fact", "PATCH", h.ts.URL + "/v1/projects/" + string(p.ID) + "/facts/fct_NOPE",
			`{"title":"x"}`, http.StatusNotFound},
		{"delete unknown fact", "DELETE", h.ts.URL + "/v1/projects/" + string(p.ID) + "/facts/fct_NOPE",
			"", http.StatusNotFound},
	}
	for _, c := range cases {
		var code int
		var raw string
		switch c.method {
		case "POST":
			code, raw = postRaw(t, c.url, c.body)
		case "PATCH":
			code, raw = patchRaw(t, c.url, c.body)
		case "DELETE":
			code, raw = deleteRaw(t, c.url)
		default:
			code, raw = getRaw(t, c.url)
		}
		if code != c.want {
			t.Errorf("%s: %s %s = %d, want %d (%s)", c.name, c.method, c.url, code, c.want, raw)
		}
		if c.want != http.StatusNoContent && !strings.Contains(raw, `"error"`) {
			t.Errorf("%s: body %q carries no error message", c.name, raw)
		}
	}

	// A fact addressed under the WRONG project is not that project's fact.
	other := postProject(t, h, `{"name":"Singapore Co"}`)
	if code, _ := patchRaw(t, h.ts.URL+"/v1/projects/"+string(other.ID)+"/facts/"+string(fact.ID),
		`{"title":"x"}`); code != http.StatusNotFound {
		t.Errorf("patching a fact through another project = %d, want 404", code)
	}
	if code, _ := deleteRaw(t, h.ts.URL+"/v1/projects/"+string(other.ID)+"/facts/"+string(fact.ID)); code != http.StatusNotFound {
		t.Errorf("deleting a fact through another project = %d, want 404", code)
	}
}

// TestFactWireOmitsAbsentFields: a milestone carries no due on the wire, and a
// project with nothing coming carries no nextDue — the shapes the client's
// optional decoding depends on.
func TestFactWireOmitsAbsentFields(t *testing.T) {
	h := newHarness(t, &fakeLLM{})
	p := postProject(t, h, `{"name":"Shanghai Co"}`)
	factURL := h.ts.URL + "/v1/projects/" + string(p.ID) + "/facts"
	var f Fact
	postExpect(t, http.StatusCreated, factURL, `{"kind":"milestone","title":"notarize"}`, &f)

	raw := rawGet(t, h.ts.URL+"/v1/projects/"+string(p.ID))
	for _, absent := range []string{`"due"`, `"rrule"`, `"tz"`, `"blocker"`, `"body"`, `"eventId"`, `"nextDue"`} {
		if strings.Contains(raw, absent) {
			t.Errorf("detail body %s\n  carries %s, want the key absent when empty", raw, absent)
		}
	}
	for _, present := range []string{`"done":false`, `"leadDays":0`, `"kind":"milestone"`, `"health":"ok"`, `"factCount":1`} {
		if !strings.Contains(raw, present) {
			t.Errorf("detail body %s\n  misses %s", raw, present)
		}
	}

	postExpect(t, http.StatusCreated, factURL,
		`{"kind":"deadline","title":"lease","due":"2027-03-14T00:00:00Z"}`, &f)
	raw = rawGet(t, h.ts.URL+"/v1/projects")
	if !strings.Contains(raw, `"nextDue":"2027-03-14T00:00:00Z"`) {
		t.Errorf("list body %s\n  misses the nextDue of a project with a deadline", raw)
	}
}

// ── Tool ──────────────────────────────────────────────────────────────────────

func runProject(t *testing.T, tb *BotToolbox, args string) string {
	t.Helper()
	res, err := tb.Run(context.Background(), projectToolName, json.RawMessage(args))
	if err != nil {
		t.Fatalf("project tool %s: %v", args, err)
	}
	return res.text
}

// TestProjectToolCommands walks every command a bot has.
func TestProjectToolCommands(t *testing.T) {
	s := newEventStore(t)
	bot := newBot(t, s)
	tb := NewBotToolbox(s, bot.ID, nil)

	if got := runProject(t, tb, `{"command":"list"}`); !strings.Contains(got, "no projects") {
		t.Errorf("list on an empty net = %q, want it to say there are none", got)
	}

	if got := runProject(t, tb, `{"command":"create","project":"Singapore Co","goal":"keep the entity in good standing"}`); !strings.Contains(got, "Singapore Co") {
		t.Errorf("create = %q, want it to name the project", got)
	}
	p, err := s.ProjectByName("Singapore Co")
	if err != nil {
		t.Fatalf("project by name: %v", err)
	}
	// The CALLING bot is the author — the model never gets to name one.
	if p.CreatedBy != string(bot.ID) {
		t.Errorf("createdBy = %q, want the calling bot %q", p.CreatedBy, bot.ID)
	}

	if got := runProject(t, tb, `{"command":"add_fact","project":"singapore co","kind":"recurring",`+
		`"title":"annual return","due":"2026-03-14T12:00:00Z","rrule":"FREQ=YEARLY","tz":"Asia/Singapore","lead_days":"180"}`); !strings.Contains(got, "annual return") {
		t.Errorf("add_fact = %q, want it to name the fact", got)
	}
	if got := runProject(t, tb, `{"command":"add_fact","project":"Singapore Co","kind":"milestone",`+
		`"title":"notarize the deed","blocker":"waiting on the lawyer"}`); !strings.Contains(got, "notarize") {
		t.Errorf("add_fact milestone = %q, want it to name the fact", got)
	}
	if got := runProject(t, tb, `{"command":"note","project":"Singapore Co","body":"corpsec@example.com is the agent"}`); !strings.Contains(got, "corpsec@example.com") {
		t.Errorf("note = %q, want it to echo the body", got)
	}
	facts, err := s.ListFacts(p.ID)
	if err != nil {
		t.Fatalf("list facts: %v", err)
	}
	if len(facts) != 3 {
		t.Fatalf("store holds %d facts, want 3", len(facts))
	}
	var note Fact
	for _, f := range facts {
		if f.Kind == FactNote {
			note = f
		}
		if f.CreatedBy != string(bot.ID) {
			t.Errorf("fact %q createdBy = %q, want the calling bot", f.Title, f.CreatedBy)
		}
	}
	if note.Title != "corpsec@example.com is the agent" {
		t.Errorf("note title = %q, want the first 60 characters of the body", note.Title)
	}

	// list and show render health and the facts.
	list := runProject(t, tb, `{"command":"list"}`)
	if !strings.Contains(list, "Singapore Co") || !strings.Contains(list, string(HealthBlocked)) {
		t.Errorf("list = %q, want the project with its derived health", list)
	}
	show := runProject(t, tb, `{"command":"show","project":"Singapore Co"}`)
	for _, want := range []string{"annual return", "notarize the deed", "waiting on the lawyer", "corpsec@example.com"} {
		if !strings.Contains(show, want) {
			t.Errorf("show = %q, want it to carry %q", show, want)
		}
	}

	// update_fact resolves the fact by title, case-insensitively.
	if got := runProject(t, tb, `{"command":"update_fact","project":"Singapore Co","title":"NOTARIZE THE DEED","done":"true","blocker":""}`); !strings.Contains(got, "notarize the deed") {
		t.Errorf("update_fact = %q, want it to name the fact", got)
	}
	facts, err = s.ListFacts(p.ID)
	if err != nil {
		t.Fatalf("list facts: %v", err)
	}
	for _, f := range facts {
		if f.Kind == FactMilestone && (!f.Done || f.Blocker != "") {
			t.Errorf("milestone after update_fact = %+v, want it done and unblocked", f)
		}
	}
	if got := runProject(t, tb, `{"command":"update_fact","project":"Singapore Co","title":"annual return","new_title":"ACRA annual return","lead_days":"90"}`); !strings.Contains(got, "ACRA annual return") {
		t.Errorf("rename via update_fact = %q, want the new title", got)
	}
}

// TestProjectToolInstructiveErrors: a malformed call answers the model with
// something it can fix, and never fails the turn.
func TestProjectToolInstructiveErrors(t *testing.T) {
	s := newEventStore(t)
	bot := newBot(t, s)
	tb := NewBotToolbox(s, bot.ID, nil)
	p := mustProject(t, s, "Singapore Co", "")
	mustFact(t, s, p.ID, Fact{Kind: FactMilestone, Title: "file the form"}, userAuthor)
	mustFact(t, s, p.ID, Fact{Kind: FactMilestone, Title: "sign the deed"}, userAuthor)

	cases := []struct{ name, args, want string }{
		{"no command", `{}`, "missing 'command'"},
		{"unknown command", `{"command":"destroy"}`, "unknown command 'destroy'"},
		{"missing project", `{"command":"show"}`, "requires a 'project' field"},
		{"unknown project", `{"command":"show","project":"Atlantis"}`, `no project named "Atlantis"`},
		{"duplicate project", `{"command":"create","project":"singapore co"}`, "already"},
		{"missing kind", `{"command":"add_fact","project":"Singapore Co","title":"x"}`, "requires a 'kind' field"},
		{"bad kind", `{"command":"add_fact","project":"Singapore Co","kind":"reminder","title":"x"}`, "kind"},
		{"bad due", `{"command":"add_fact","project":"Singapore Co","kind":"deadline","title":"x","due":"next tuesday"}`, "RFC3339"},
		{"bad lead", `{"command":"add_fact","project":"Singapore Co","kind":"deadline","title":"x","due":"2027-03-14T00:00:00Z","lead_days":"soon"}`, "lead_days"},
		{"bad done", `{"command":"update_fact","project":"Singapore Co","title":"sign the deed","done":"yes"}`, `"true" or "false"`},
		{"unknown fact", `{"command":"update_fact","project":"Singapore Co","title":"nothing"}`, "no fact"},
		{"nothing to update", `{"command":"update_fact","project":"Singapore Co","title":"sign the deed"}`, "needs"},
		{"note with no body", `{"command":"note","project":"Singapore Co"}`, "requires a 'body' field"},
	}
	for _, c := range cases {
		res, err := tb.Run(context.Background(), projectToolName, json.RawMessage(c.args))
		if err != nil {
			t.Errorf("%s: returned a turn-failing error: %v", c.name, err)
			continue
		}
		if !strings.HasPrefix(res.text, "error:") || !strings.Contains(res.text, c.want) {
			t.Errorf("%s: result = %q, want an instructive error containing %q", c.name, res.text, c.want)
		}
	}
}

// TestProjectToolHasNoDelete: deletion is the UI's, behind a confirmation —
// the delete_calendar precedent. A cheap model must not be able to drop a
// project's whole history in one call.
func TestProjectToolHasNoDelete(t *testing.T) {
	for _, c := range projectCommands {
		if strings.Contains(c.name, "delete") || strings.Contains(c.name, "remove") {
			t.Errorf("the project tool advertises %q; deletion is UI-only", c.name)
		}
	}
	s := newEventStore(t)
	bot := newBot(t, s)
	tb := NewBotToolbox(s, bot.ID, nil)
	p := mustProject(t, s, "Passports", "")
	res, err := tb.Run(context.Background(), projectToolName,
		json.RawMessage(`{"command":"delete","project":"Passports"}`))
	if err != nil {
		t.Fatalf("delete attempt: %v", err)
	}
	if !strings.HasPrefix(res.text, "error:") {
		t.Errorf("delete = %q, want an instructive refusal", res.text)
	}
	if _, _, err := s.GetProject(p.ID); err != nil {
		t.Errorf("the project is gone after a tool delete attempt: %v", err)
	}
}

// TestToolsEndpointIncludesProject: the inspector reads /v1/tools, so the
// project tool must be there in its flat-schema shape.
func TestToolsEndpointIncludesProject(t *testing.T) {
	h := newHarness(t, &fakeLLM{})
	var tools []wireTool
	get(t, h.ts.URL+"/v1/tools", &tools)

	var def wireToolFunction
	for _, tool := range tools {
		if tool.Function.Name == projectToolName {
			def = tool.Function
		}
	}
	if def.Name == "" {
		t.Fatalf("/v1/tools served %d tools, none of them the project tool", len(tools))
	}
	props, _ := def.Parameters["properties"].(map[string]any)
	command, _ := props["command"].(map[string]any)
	var enum []string
	if raw, ok := command["enum"].([]any); ok {
		for _, v := range raw {
			enum = append(enum, v.(string))
		}
	}
	want := "list,show,create,update,add_fact,update_fact,note"
	if strings.Join(enum, ",") != want {
		t.Errorf("command enum = %v, want %s", enum, want)
	}
	for _, field := range []string{"project", "parent", "new_name", "goal", "kind", "title",
		"new_title", "due", "lead_days", "rrule", "tz", "done", "blocker", "body"} {
		spec, ok := props[field].(map[string]any)
		if !ok {
			t.Errorf("parameters miss the %q field", field)
			continue
		}
		if spec["type"] != "string" {
			t.Errorf("%q is %v, want a flat string", field, spec["type"])
		}
	}
	if req, ok := def.Parameters["required"].([]any); !ok || len(req) != 1 || req[0] != "command" {
		t.Errorf("required = %v, want just command", def.Parameters["required"])
	}
	for _, must := range []string{"deadline", "recurring", "milestone", "note"} {
		if !strings.Contains(def.Description, must) {
			t.Errorf("tool description misses the %q kind: %q", must, def.Description)
		}
	}
}
