package botnet

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// Bots, not the user, operate projects, so the tool has to ENCODE its usage
// patterns rather than trust a mid-tier model's judgment: a ladder in the
// description, guards that turn the four known misuses into instructive errors,
// and derived state on every result so the model sees what its write did.

// ── The ladder in the description ─────────────────────────────────────────────

// TestProjectToolDescriptionCarriesTheLadder: the description is the source of
// truth for how to use the tool, so it must name every kind, the two fields a
// model reliably forgets, and the read-before-write step.
func TestProjectToolDescriptionCarriesTheLadder(t *testing.T) {
	doc := projectToolDef().Function.Description
	for _, must := range []string{"deadline", "recurring", "milestone", "note", "blocker", "lead_days", "show"} {
		if !strings.Contains(doc, must) {
			t.Errorf("tool description never mentions %q", must)
		}
	}
	// A numbered ladder, not a paragraph: a model scanning for "which kind is
	// this" needs discrete steps.
	for _, step := range []string{"1.", "2.", "3.", "4.", "5.", "6.", "7."} {
		if !strings.Contains(doc, step) {
			t.Errorf("tool description has no step %q — the ladder must be a numbered list", step)
		}
	}
	// The renewal leads a model actually needs, and the two rules that keep a
	// project honest.
	for _, must := range []string{"180", "90", "60", "30", "update_fact", "evidence"} {
		if !strings.Contains(doc, must) {
			t.Errorf("tool description omits %q", must)
		}
	}
}

// TestProjectsDocQuotesTheLadder: the tool description is the source of truth
// and PROJECTS.md quotes it, never the other way round. Without this the two
// drift, and the doc a human reads stops describing the tool a bot obeys.
func TestProjectsDocQuotesTheLadder(t *testing.T) {
	doc, err := os.ReadFile("PROJECTS.md")
	if err != nil {
		t.Fatalf("read PROJECTS.md: %v", err)
	}
	if !strings.Contains(string(doc), projectLadder) {
		t.Error("PROJECTS.md does not quote projectLadder verbatim; re-copy it from tools.go")
	}
}

// ── Guard: a twin fact ────────────────────────────────────────────────────────

// TestAddFactRefusesADuplicateTitle: a bot that forgets to look first must not
// be able to grow a second copy of a fact — the tool resolves facts BY TITLE,
// so a twin makes the project uneditable by the very tool that made it.
func TestAddFactRefusesADuplicateTitle(t *testing.T) {
	s := newEventStore(t)
	p := mustProject(t, s, "Singapore Co", "")
	mustFact(t, s, p.ID, Fact{Kind: FactMilestone, Title: "notarize the deed"}, userAuthor)

	for _, dup := range []string{"notarize the deed", "NOTARIZE THE DEED", "  Notarize The Deed  "} {
		_, err := s.CreateFact(p.ID, Fact{Kind: FactNote, Title: dup, Body: "x"}, userAuthor)
		if !errors.Is(err, ErrDuplicateName) {
			t.Errorf("create %q = %v, want ErrDuplicateName", dup, err)
		}
	}
	// A twin is only a twin inside its own project.
	other := mustProject(t, s, "Passports", "")
	if _, err := s.CreateFact(other.ID, Fact{Kind: FactMilestone, Title: "notarize the deed"}, userAuthor); err != nil {
		t.Errorf("the same title in another project was refused: %v", err)
	}
	// Renaming ONTO a sibling's title is the same collision by another route,
	// and it is what would make both facts unaddressable.
	second := mustFact(t, s, p.ID, Fact{Kind: FactNote, Title: "agent", Body: "x"}, userAuthor)
	taken := "Notarize the deed"
	if _, err := s.UpdateFact(second.ID, FactPatch{Title: &taken}); !errors.Is(err, ErrDuplicateName) {
		t.Errorf("renaming onto a sibling's title = %v, want ErrDuplicateName", err)
	}
	// Renaming a fact to its own title is not a duplicate of itself.
	same := "AGENT"
	if _, err := s.UpdateFact(second.ID, FactPatch{Title: &same}); err != nil {
		t.Errorf("renaming a fact to its own title: %v", err)
	}
}

// TestAddFactDuplicateOverTheWire: 409 on REST, and an instructive error naming
// the command that fixes it on the tool.
func TestAddFactDuplicateOverTheWire(t *testing.T) {
	h := newHarness(t, &fakeLLM{})
	p := postProject(t, h, `{"name":"Singapore Co"}`)
	url := h.ts.URL + "/v1/projects/" + string(p.ID) + "/facts"
	var f Fact
	postExpect(t, http.StatusCreated, url, `{"kind":"milestone","title":"notarize the deed"}`, &f)

	code, raw := postRaw(t, url, `{"kind":"milestone","title":"NOTARIZE THE DEED"}`)
	if code != http.StatusConflict {
		t.Errorf("duplicate fact title = %d, want 409 (%s)", code, raw)
	}
	if !strings.Contains(raw, "already exists") {
		t.Errorf("409 body = %s, want it to say the title is taken", raw)
	}

	bot := newBot(t, h.store)
	tb := NewBotToolbox(h.store, bot.ID, nil)
	got := runProject(t, tb, `{"command":"add_fact","project":"Singapore Co","kind":"milestone","title":"notarize the deed"}`)
	for _, want := range []string{"error:", `"notarize the deed"`, "already exists", "update_fact"} {
		if !strings.Contains(got, want) {
			t.Errorf("tool duplicate error = %q, want it to contain %q", got, want)
		}
	}
}

// ── Guard: a date hiding in a note ────────────────────────────────────────────

// TestNoteCarryingADateIsRefused: a note never moves health, so a date written
// into one is a deadline the project will never surface. The tool catches it
// and hands back the resend; REST does not, because a human typing a note in
// the UI can mean exactly what they typed.
func TestNoteCarryingADateIsRefused(t *testing.T) {
	s := newEventStore(t)
	bot := newBot(t, s)
	tb := NewBotToolbox(s, bot.ID, nil)
	mustProject(t, s, "Singapore Co", "")

	cases := []string{
		`{"command":"note","project":"Singapore Co","body":"AGM must be held by 2027-03-14"}`,
		`{"command":"add_fact","project":"Singapore Co","kind":"note","title":"agm","body":"by 2027-03-14"}`,
		`{"command":"add_fact","project":"Singapore Co","kind":"note","title":"AGM 2027-03-14","body":"see email"}`,
	}
	for _, args := range cases {
		got := runProject(t, tb, args)
		if !strings.HasPrefix(got, "error:") {
			t.Errorf("%s\n  = %q, want a refusal naming the deadline shape", args, got)
			continue
		}
		for _, want := range []string{"2027-03-14", "deadline", "due"} {
			if !strings.Contains(got, want) {
				t.Errorf("%s\n  error %q misses %q", args, got, want)
			}
		}
	}
	facts, err := s.ListFacts(mustByName(t, s, "Singapore Co").ID)
	if err != nil {
		t.Fatalf("list facts: %v", err)
	}
	if len(facts) != 0 {
		t.Errorf("a refused note was stored anyway: %+v", facts)
	}

	// A note with no date, and one whose digits are not a real date, both pass.
	for _, args := range []string{
		`{"command":"note","project":"Singapore Co","body":"corpsec@example.com is the agent"}`,
		`{"command":"note","project":"Singapore Co","body":"invoice 2027-99-45 was paid"}`,
	} {
		if got := runProject(t, tb, args); strings.HasPrefix(got, "error:") {
			t.Errorf("%s\n  = %q, want it accepted", args, got)
		}
	}

	// REST keeps the note verbatim: the regex is a tool-boundary guard only.
	h := newHarness(t, &fakeLLM{})
	p := postProject(t, h, `{"name":"Singapore Co"}`)
	var f Fact
	postExpect(t, http.StatusCreated, h.ts.URL+"/v1/projects/"+string(p.ID)+"/facts",
		`{"kind":"note","title":"agm","body":"AGM must be held by 2027-03-14"}`, &f)
	if f.Kind != FactNote {
		t.Errorf("REST note = %+v, want it stored as typed", f)
	}
}

// mustByName resolves a project by name or fails the test.
func mustByName(t *testing.T, s *Store, name string) Project {
	t.Helper()
	p, err := s.ProjectByName(name)
	if err != nil {
		t.Fatalf("project %q: %v", name, err)
	}
	return p
}

// ── Guard: a blocked milestone cannot be done ─────────────────────────────────

// TestBlockedMilestoneCannotBeDone: "waiting on the lawyer" and "finished" are
// contradictory claims about the same step, so the pair is rejected at the write
// boundary rather than stored and rendered as both.
func TestBlockedMilestoneCannotBeDone(t *testing.T) {
	s := newEventStore(t)
	p := mustProject(t, s, "Singapore Co", "")

	_, err := s.CreateFact(p.ID, Fact{Kind: FactMilestone, Title: "notarize",
		Blocker: "waiting on the lawyer", Done: true}, userAuthor)
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("a blocked, done milestone = %v, want ErrInvalid", err)
	}
	if !strings.Contains(err.Error(), "clear the blocker first") {
		t.Errorf("error %q does not name the fix", err)
	}

	// The same rule binds a patch, checked against the MERGED fact: marking a
	// still-blocked milestone done is the common way to hit this.
	blocked := mustFact(t, s, p.ID, Fact{Kind: FactMilestone, Title: "notarize",
		Blocker: "waiting on the lawyer"}, userAuthor)
	done := true
	if _, err := s.UpdateFact(blocked.ID, FactPatch{Done: &done}); !errors.Is(err, ErrInvalid) {
		t.Errorf("marking a blocked milestone done = %v, want ErrInvalid", err)
	}
	// Clearing the blocker in the SAME call is the legal way through.
	clear := ""
	settled, err := s.UpdateFact(blocked.ID, FactPatch{Done: &done, Blocker: &clear})
	if err != nil {
		t.Fatalf("clearing the blocker while marking it done: %v", err)
	}
	if !settled.Done || settled.Blocker != "" {
		t.Errorf("settled milestone = %+v, want done and unblocked", settled)
	}

	// REST is a 400, the tool an instructive error.
	h := newHarness(t, &fakeLLM{})
	hp := postProject(t, h, `{"name":"Singapore Co"}`)
	code, raw := postRaw(t, h.ts.URL+"/v1/projects/"+string(hp.ID)+"/facts",
		`{"kind":"milestone","title":"notarize","blocker":"the lawyer","done":true}`)
	if code != http.StatusBadRequest || !strings.Contains(raw, "clear the blocker first") {
		t.Errorf("REST blocked+done = %d %s, want 400 naming the fix", code, raw)
	}

	bot := newBot(t, s)
	tb := NewBotToolbox(s, bot.ID, nil)
	still := mustFact(t, s, p.ID, Fact{Kind: FactMilestone, Title: "open the bank account",
		Blocker: "the director must sign"}, userAuthor)
	_ = still
	got := runProject(t, tb, `{"command":"update_fact","project":"Singapore Co","title":"open the bank account","done":"true"}`)
	if !strings.HasPrefix(got, "error:") || !strings.Contains(got, "clear the blocker first") {
		t.Errorf("tool blocked+done = %q, want an instructive refusal", got)
	}
}

// ── Guard: done well before the due date ──────────────────────────────────────

// TestEarlyDoneWarnsAboutRenewal: ticking a deadline months early is almost
// always a renewal recorded the wrong way, so the write is ALLOWED (the model
// may be right) and the result says what the alternative was.
func TestEarlyDoneWarnsAboutRenewal(t *testing.T) {
	s := newEventStore(t)
	bot := newBot(t, s)
	tb := NewBotToolbox(s, bot.ID, nil)
	p := mustProject(t, s, "Passports", "")
	mustFact(t, s, p.ID, Fact{Kind: FactDeadline, Title: "US passport expires",
		Due: time.Now().UTC().AddDate(0, 0, 200), LeadDays: 180}, userAuthor)
	mustFact(t, s, p.ID, Fact{Kind: FactDeadline, Title: "UK passport expires",
		Due: time.Now().UTC().AddDate(0, 0, -5), LeadDays: 180}, userAuthor)

	early := runProject(t, tb, `{"command":"update_fact","project":"Passports","title":"US passport expires","done":"true"}`)
	if strings.HasPrefix(early, "error:") {
		t.Fatalf("an early done was refused: %q — the write must be allowed", early)
	}
	for _, want := range []string{"200 days before due", "renewal", "set due to the new date"} {
		if !strings.Contains(early, want) {
			t.Errorf("early-done result = %q, want it to contain %q", early, want)
		}
	}
	f, err := s.ProjectByName("Passports")
	if err != nil {
		t.Fatalf("reread: %v", err)
	}
	facts, err := s.ListFacts(f.ID)
	if err != nil {
		t.Fatalf("list facts: %v", err)
	}
	for _, fact := range facts {
		if fact.Title == "US passport expires" && !fact.Done {
			t.Error("the early done was not applied; the caution must not block the write")
		}
	}

	// A deadline ticked at or after its due date is the ordinary case and says
	// nothing extra.
	late := runProject(t, tb, `{"command":"update_fact","project":"Passports","title":"UK passport expires","done":"true"}`)
	if strings.Contains(late, "before due") {
		t.Errorf("an on-time done = %q, want no renewal caution", late)
	}

	// A milestone has no due date, so it can never be early.
	mustFact(t, s, p.ID, Fact{Kind: FactMilestone, Title: "book the appointment"}, userAuthor)
	m := runProject(t, tb, `{"command":"update_fact","project":"Passports","title":"book the appointment","done":"true"}`)
	if strings.Contains(m, "before due") {
		t.Errorf("a done milestone = %q, want no renewal caution", m)
	}
}

// ── Derived state on every result ─────────────────────────────────────────────

// TestMutatingResultsEndWithHealth: health is derived, so the copy a handler
// held before its write is already stale. Every mutating command re-reads and
// ends with the project's health line, which is how the model learns what its
// write actually did.
func TestMutatingResultsEndWithHealth(t *testing.T) {
	s := newEventStore(t)
	bot := newBot(t, s)
	tb := NewBotToolbox(s, bot.ID, nil)

	created := runProject(t, tb, `{"command":"create","project":"Passports","goal":"keep them valid"}`)
	if !strings.HasSuffix(created, "Passports: unknown") {
		t.Errorf("create = %q, want it to end with the new project's health line", created)
	}

	due := time.Now().UTC().AddDate(0, 0, 20).Format(time.RFC3339)
	added := runProject(t, tb, `{"command":"add_fact","project":"Passports","kind":"deadline",`+
		`"title":"US passport expires","due":"`+due+`","lead_days":"30"}`)
	// 20d, not 19: the count is to the NEAREST day, so the microseconds the
	// write itself takes cannot shave a day off the answer.
	last := lastLine(added)
	for _, want := range []string{"Passports: due_soon", "next due", "(in 20d)"} {
		if !strings.Contains(last, want) {
			t.Errorf("add_fact health line = %q, want it to contain %q", last, want)
		}
	}

	noted := runProject(t, tb, `{"command":"note","project":"Passports","body":"the consulate takes walk-ins"}`)
	if !strings.Contains(lastLine(noted), "Passports: due_soon") {
		t.Errorf("note = %q, want it to end with the health line", noted)
	}

	updated := runProject(t, tb, `{"command":"update_fact","project":"Passports","title":"US passport expires","lead_days":"5"}`)
	if !strings.Contains(lastLine(updated), "Passports: ok") {
		t.Errorf("update_fact = %q, want the health line to show the narrowed lead took effect", updated)
	}

	// An overdue project counts the other way.
	past := time.Now().UTC().AddDate(0, 0, -3).Format(time.RFC3339)
	overdue := runProject(t, tb, `{"command":"update_fact","project":"Passports","title":"US passport expires","due":"`+past+`"}`)
	if l := lastLine(overdue); !strings.Contains(l, "Passports: overdue") || !strings.Contains(l, "overdue)") {
		t.Errorf("overdue health line = %q, want \"<name>: overdue, next due <date> (3d overdue)\"", l)
	}
}

// TestShowPromptsForAHealthBearingFact: a project of nothing but notes derives
// no health at all, and the model needs telling why rather than reading "ok".
func TestShowPromptsForAHealthBearingFact(t *testing.T) {
	s := newEventStore(t)
	bot := newBot(t, s)
	tb := NewBotToolbox(s, bot.ID, nil)
	mustProject(t, s, "Passports", "")

	const hint = "health unknown: add a deadline, recurring or milestone fact"
	empty := runProject(t, tb, `{"command":"show","project":"Passports"}`)
	if !strings.HasSuffix(empty, hint) {
		t.Errorf("show on an empty project = %q, want it to end with %q", empty, hint)
	}

	runProject(t, tb, `{"command":"note","project":"Passports","body":"the consulate takes walk-ins"}`)
	notesOnly := runProject(t, tb, `{"command":"show","project":"Passports"}`)
	if !strings.HasSuffix(notesOnly, hint) {
		t.Errorf("show on a notes-only project = %q, want it to end with %q", notesOnly, hint)
	}

	runProject(t, tb, `{"command":"add_fact","project":"Passports","kind":"milestone","title":"book the appointment"}`)
	withWork := runProject(t, tb, `{"command":"show","project":"Passports"}`)
	if strings.Contains(withWork, hint) {
		t.Errorf("show on a project with a milestone = %q, want no prompt", withWork)
	}
}

// TestToolStillRefusesToGuessBetweenTwins: the duplicate guard closes the door
// on NEW twins, but a database written before it can hold a pair. The tool must
// still refuse to pick one rather than silently editing the wrong fact.
func TestToolStillRefusesToGuessBetweenTwins(t *testing.T) {
	s := newEventStore(t)
	bot := newBot(t, s)
	tb := NewBotToolbox(s, bot.ID, nil)
	p := mustProject(t, s, "Singapore Co", "")
	first := mustFact(t, s, p.ID, Fact{Kind: FactMilestone, Title: "file the form"}, userAuthor)

	// A legacy row, inserted the way a pre-guard release would have.
	now := fmtEventTime(time.Now().UTC().Truncate(time.Second))
	if _, err := s.db.Exec(
		`INSERT INTO facts (`+factColumns+`) VALUES (?, ?, ?, ?, '', 0, '', '', 0, '', '', '', ?, ?, ?)`,
		newID("fct_"), p.ID, FactMilestone, "File The Form", userAuthor, now, now); err != nil {
		t.Fatalf("seed a legacy twin: %v", err)
	}
	_ = first

	got := runProject(t, tb, `{"command":"update_fact","project":"Singapore Co","title":"file the form","done":"true"}`)
	if !strings.HasPrefix(got, "error:") || !strings.Contains(got, "matches") {
		t.Errorf("update over twins = %q, want a refusal to guess", got)
	}
}

// lastLine returns a rendered result's final line.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	return lines[len(lines)-1]
}

// TestProjectToolResultsAreStillPlainText guards the shape the loop feeds back:
// a tool result is text for the model, never JSON it has to parse.
func TestProjectToolResultsAreStillPlainText(t *testing.T) {
	s := newEventStore(t)
	bot := newBot(t, s)
	tb := NewBotToolbox(s, bot.ID, nil)
	res, err := tb.Run(context.Background(), projectToolName,
		json.RawMessage(`{"command":"create","project":"Passports"}`))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if strings.HasPrefix(strings.TrimSpace(res.text), "{") {
		t.Errorf("result = %q, want plain text", res.text)
	}
	if res.backend != "" || len(res.results) != 0 {
		t.Errorf("result carried search fields: %+v", res)
	}
}
