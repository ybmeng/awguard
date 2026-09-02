package botnet

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	modelselector "stdtools/go/lib/modelSelector"
)

// The owner bot and the nudge: a project names the bot answerable for it, that
// answer INHERITS like the lead threshold does, and one tick over the forest
// tells an owner — once — that its project has got worse.

// ── Fixtures ──────────────────────────────────────────────────────────────────

// namedBot creates a bot with a chosen display name, which is how the tool
// addresses one.
func namedBot(t *testing.T, s *Store, name string) Bot {
	t.Helper()
	net, err := s.EnsureDefaultNet()
	if err != nil {
		t.Fatalf("ensure net: %v", err)
	}
	bot, err := s.CreateBot(net.ID, name, "", modelselector.DeepSeekV4.ID)
	if err != nil {
		t.Fatalf("create bot %q: %v", name, err)
	}
	return bot
}

// setOwner is the store-level owner patch, a line rather than four.
func setOwner(t *testing.T, s *Store, id ProjectID, owner BotID) Project {
	t.Helper()
	p, err := s.UpdateProject(id, ProjectPatch{OwnerBot: &owner})
	if err != nil {
		t.Fatalf("set owner of %s: %v", id, err)
	}
	return p
}

// ── Unit 4: the owner pointer ─────────────────────────────────────────────────

// TestOwnerBotRoundTrip: the column stores, survives a close/reopen, is cleared
// by an explicit empty patch, and refuses a bot that does not exist.
func TestOwnerBotRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "owner.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ada := namedBot(t, s, "Ada")
	p := mustProject(t, s, "Document Expirations", "")
	owned := setOwner(t, s, p.ID, ada.ID)
	if owned.OwnerBot != ada.ID || owned.EffectiveOwner != ada.ID {
		t.Errorf("owned project = %q / %q, want %q both stored and effective",
			owned.OwnerBot, owned.EffectiveOwner, ada.ID)
	}

	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if s, err = Open(path); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()

	reopened, _, err := s.GetProject(p.ID)
	if err != nil {
		t.Fatalf("get after reopen: %v", err)
	}
	if reopened.OwnerBot != ada.ID {
		t.Errorf("ownerBot after a reopen = %q, want %q", reopened.OwnerBot, ada.ID)
	}

	none := BotID("")
	cleared, err := s.UpdateProject(p.ID, ProjectPatch{OwnerBot: &none})
	if err != nil {
		t.Fatalf("clear the owner: %v", err)
	}
	if cleared.OwnerBot != "" || cleared.EffectiveOwner != "" {
		t.Errorf("cleared project = %q / %q, want both empty", cleared.OwnerBot, cleared.EffectiveOwner)
	}

	ghost := BotID("bot_01ARZ3NDEKTSV4RRFFQ69G5FAV")
	if _, err := s.UpdateProject(p.ID, ProjectPatch{OwnerBot: &ghost}); !errors.Is(err, ErrNotFound) {
		t.Errorf("owning a project to a bot that does not exist = %v, want ErrNotFound", err)
	}
	if _, err := s.CreateProject(Project{Name: "Ghosted", OwnerBot: ghost}, userAuthor); !errors.Is(err, ErrNotFound) {
		t.Errorf("creating a project owned by a missing bot = %v, want ErrNotFound", err)
	}
}

// TestEffectiveOwnerInheritance: the effective owner is the nearest ANCESTOR
// answer, derived in the same forest pass the lead threshold is.
func TestEffectiveOwnerInheritance(t *testing.T) {
	s := newEventStore(t)
	ada := namedBot(t, s, "Ada")
	ben := namedBot(t, s, "Ben")

	root := mustProject(t, s, "Document Expirations", "")
	setOwner(t, s, root.ID, ada.ID)
	passport := mustChild(t, s, "Passport", root.ID)
	mustChild(t, s, "Biometrics appointment", passport.ID)
	visa := mustChild(t, s, "China Q2 Visa", root.ID)
	setOwner(t, s, visa.ID, ben.ID)
	mustChild(t, s, "Visa photos", visa.ID)
	mustProject(t, s, "Singapore Co", "")

	listed, err := s.ListProjects()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, want := range []struct {
		name  string
		owner BotID
	}{
		{"Document Expirations", ada.ID},
		{"Passport", ada.ID},               // inherited
		{"Biometrics appointment", ada.ID}, // two levels down
		{"China Q2 Visa", ben.ID},          // its own overrides
		{"Visa photos", ben.ID},            // the nearer ancestor wins
		{"Singapore Co", ""},               // nobody above it owns anything
	} {
		if got := byName(t, listed, want.name).EffectiveOwner; got != want.owner {
			t.Errorf("%s effectiveOwner = %q, want %q", want.name, got, want.owner)
		}
	}
}

// TestDeleteBotClearsTheProjectsItOwned: a deleted bot must not leave projects
// pointing at it — the tick would then look for a thread that is gone. The
// clear is a real write, so every affected project reaches the change feed.
func TestDeleteBotClearsTheProjectsItOwned(t *testing.T) {
	s := newEventStore(t)
	ada := namedBot(t, s, "Ada")
	ben := namedBot(t, s, "Ben")
	mine := mustProject(t, s, "Document Expirations", "")
	setOwner(t, s, mine.ID, ada.ID)
	child := mustChild(t, s, "Passport", mine.ID)
	setOwner(t, s, child.ID, ada.ID)
	theirs := mustProject(t, s, "Singapore Co", "")
	setOwner(t, s, theirs.ID, ben.ID)

	mark := topSeq(t, s)
	if err := s.DeleteBot(ada.ID); err != nil {
		t.Fatalf("delete bot: %v", err)
	}
	listed, err := s.ListProjects()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, name := range []string{"Document Expirations", "Passport"} {
		p := byName(t, listed, name)
		if p.OwnerBot != "" || p.EffectiveOwner != "" {
			t.Errorf("%s still owned by the deleted bot: %q / %q", name, p.OwnerBot, p.EffectiveOwner)
		}
	}
	if kept := byName(t, listed, "Singapore Co"); kept.OwnerBot != ben.ID {
		t.Errorf("another bot's project lost its owner: %q", kept.OwnerBot)
	}
	// One project-updated row per cleared project, so a second client learns it.
	updated := 0
	for _, row := range logAfter(t, s, mark) {
		if row.entity == "project" && row.op == "updated" {
			updated++
		}
	}
	if updated != 2 {
		t.Errorf("delete emitted %d project updated rows, want one per cleared project (2)", updated)
	}
}

// TestOwnerBotOverTheWire: POST and PATCH accept ownerBot, a missing bot is a
// 404, "" clears, and both fields ride every project row.
func TestOwnerBotOverTheWire(t *testing.T) {
	h := newHarness(t, &fakeLLM{})
	ada := createBot(t, h, "Ada")

	root := postProject(t, h, `{"name":"Document Expirations","ownerBot":"`+string(ada.ID)+`"}`)
	if root.OwnerBot != ada.ID || root.EffectiveOwner != ada.ID {
		t.Errorf("created project = %q / %q, want the owner stored and effective", root.OwnerBot, root.EffectiveOwner)
	}
	child := postProject(t, h, `{"name":"Passport","parentId":"`+string(root.ID)+`"}`)
	if child.OwnerBot != "" || child.EffectiveOwner != ada.ID {
		t.Errorf("child = %q / %q, want no own owner and the inherited one", child.OwnerBot, child.EffectiveOwner)
	}

	if code, body := postRaw(t, h.ts.URL+"/v1/projects",
		`{"name":"Ghosted","ownerBot":"bot_01ARZ3NDEKTSV4RRFFQ69G5FAV"}`); code != http.StatusNotFound {
		t.Errorf("an unknown ownerBot = %d (%s), want 404", code, body)
	}
	if code, body := patchRaw(t, h.ts.URL+"/v1/projects/"+string(root.ID),
		`{"ownerBot":"bot_01ARZ3NDEKTSV4RRFFQ69G5FAV"}`); code != http.StatusNotFound {
		t.Errorf("patching to an unknown ownerBot = %d (%s), want 404", code, body)
	}

	var cleared Project
	patch(t, h.ts.URL+"/v1/projects/"+string(root.ID), `{"ownerBot":""}`, &cleared)
	if cleared.OwnerBot != "" {
		t.Errorf("PATCH \"\" left the owner %q", cleared.OwnerBot)
	}

	// Both fields are omitempty, so an unowned project carries neither key —
	// which is what makes "nobody owns this" decodable as absent.
	_, raw := getRaw(t, h.ts.URL+"/v1/projects/"+string(cleared.ID))
	for _, key := range []string{`"ownerBot"`, `"effectiveOwner"`} {
		if strings.Contains(raw, key) {
			t.Errorf("an unowned project's JSON %s\n  still carries %s", raw, key)
		}
	}
}

// TestProjectToolOwnerByName: a bot names an owner the way a person would — by
// display name — and the three answers that are not one match are instructive
// errors rather than a guess.
func TestProjectToolOwnerByName(t *testing.T) {
	s := newEventStore(t)
	ada := namedBot(t, s, "Ada")
	namedBot(t, s, "Ben")
	tb := NewBotToolbox(s, ada.ID, nil)

	runProject(t, tb, `{"command":"create","project":"Document Expirations","owner":"ada"}`)
	root, err := s.ProjectByName("Document Expirations")
	if err != nil {
		t.Fatalf("project by name: %v", err)
	}
	if root.OwnerBot != ada.ID {
		t.Errorf("owner set by name = %q, want %q — the match must be case-insensitive", root.OwnerBot, ada.ID)
	}

	// A child inherits, and update moves the child's own owner.
	runProject(t, tb, `{"command":"create","project":"Passport","parent":"Document Expirations"}`)
	child, err := s.ProjectByName("Passport")
	if err != nil {
		t.Fatalf("project by name: %v", err)
	}
	if child.EffectiveOwner != ada.ID {
		t.Errorf("child effectiveOwner = %q, want the inherited %q", child.EffectiveOwner, ada.ID)
	}
	runProject(t, tb, `{"command":"update","project":"Passport","owner":"Ben"}`)
	if child, _ = s.ProjectByName("Passport"); child.OwnerBot == "" || child.OwnerBot == ada.ID {
		t.Errorf("update owner left %q, want Ben's id", child.OwnerBot)
	}
	// "none" clears it and the parent's owner applies again.
	runProject(t, tb, `{"command":"update","project":"Passport","owner":"none"}`)
	child, _ = s.ProjectByName("Passport")
	if child.OwnerBot != "" || child.EffectiveOwner != ada.ID {
		t.Errorf("after clearing = %q / %q, want none of its own and Ada inherited",
			child.OwnerBot, child.EffectiveOwner)
	}

	// An unknown name lists the bots that do exist.
	got := runProject(t, tb, `{"command":"update","project":"Passport","owner":"Zeno"}`)
	for _, want := range []string{"error:", "Zeno", "Ada", "Ben"} {
		if !strings.Contains(got, want) {
			t.Errorf("unknown owner error = %q, want it to contain %q", got, want)
		}
	}
	// Two bots sharing a name is ambiguous, and guessing would silently point
	// the project at the wrong thread.
	namedBot(t, s, "ada")
	amb := runProject(t, tb, `{"command":"update","project":"Passport","owner":"Ada"}`)
	if !strings.Contains(amb, "error:") || !strings.Contains(amb, "2") {
		t.Errorf("ambiguous owner = %q, want an instructive error naming the number of matches", amb)
	}
}

// ── Unit 5: the tick ──────────────────────────────────────────────────────────

// tickAt drives POST /v1/projects/tick at a chosen instant and decodes it.
func tickAt(t *testing.T, h *harness, at time.Time) ProjectTick {
	t.Helper()
	url := h.ts.URL + "/v1/projects/tick"
	if !at.IsZero() {
		url += "?at=" + at.UTC().Format(time.RFC3339)
	}
	var out ProjectTick
	postExpect(t, http.StatusOK, url, "", &out)
	return out
}

// botMessages reads a bot's thread over the wire, which is where a nudge has to
// land for the app to render it.
func botMessages(t *testing.T, h *harness, id BotID) []Message {
	t.Helper()
	var out []Message
	get(t, h.bot(id, "/messages"), &out)
	return out
}

// TestTickNudgesTheOwnerOnce is the whole rock in one story: a project that got
// worse nudges its owner exactly once, the message says what changed and which
// facts drove it, and a second tick over the same state nudges nothing.
func TestTickNudgesTheOwnerOnce(t *testing.T) {
	llm := &fakeLLM{reply: "on it"}
	h := newHarness(t, llm)
	ada := createBot(t, h, "Ada")

	root := postProject(t, h, `{"name":"Document Expirations","defaultLeadDays":180,"ownerBot":"`+string(ada.ID)+`"}`)
	child := postProject(t, h, `{"name":"Passport","parentId":"`+string(root.ID)+`"}`)
	due := time.Now().UTC().AddDate(0, 0, 190)
	var f Fact
	postExpect(t, http.StatusCreated, h.ts.URL+"/v1/projects/"+string(child.ID)+"/facts",
		`{"kind":"deadline","title":"US passport expires","due":"`+due.Format(time.RFC3339)+`"}`, &f)
	if f.LeadDays != 180 {
		t.Fatalf("the seeded fact took lead %d, want the project's 180", f.LeadDays)
	}

	// The first tick over a healthy forest records and says nothing: the fact
	// is 190 days out with a 180-day window, so nothing is due_soon yet.
	first := tickAt(t, h, time.Time{})
	h.settle()
	if len(first.Nudged) != 0 {
		t.Errorf("a first tick over a healthy forest nudged %+v, want nothing", first.Nudged)
	}
	if first.Checked != 2 {
		t.Errorf("checked = %d, want both projects", first.Checked)
	}
	if len(botMessages(t, h, ada.ID)) != 0 {
		t.Error("a quiet tick wrote to the owner's thread")
	}

	// Now step the clock inside the lead window: both projects worsen to
	// due_soon and both answer to Ada. One bot takes one turn at a time, so
	// exactly one nudge lands and the other is deferred, not lost.
	inside := time.Now().UTC().AddDate(0, 0, 15)
	second := tickAt(t, h, inside)
	h.settle()
	if len(second.Nudged) != 1 || second.Nudged[0].Project != "Document Expirations" {
		t.Fatalf("nudged %+v, want the one urgency-first project", second.Nudged)
	}
	if second.Nudged[0].Bot != ada.ID {
		t.Errorf("nudge went to %q, want the owner %q", second.Nudged[0].Bot, ada.ID)
	}
	if second.Nudged[0].From != HealthOK || second.Nudged[0].To != HealthDueSoon {
		t.Errorf("nudge = %q → %q, want ok → due_soon", second.Nudged[0].From, second.Nudged[0].To)
	}
	if len(second.Skipped) != 1 || second.Skipped[0].Project != "Passport" {
		t.Fatalf("skipped %+v, want the child deferred behind the owner's turn", second.Skipped)
	}

	msgs := botMessages(t, h, ada.ID)
	var nudges []Message
	for _, m := range msgs {
		if m.Role == "user" && strings.HasPrefix(m.Content, "Project nudge — ") {
			nudges = append(nudges, m)
		}
	}
	if len(nudges) != 1 {
		t.Fatalf("the owner's thread holds %d nudges, want 1:\n%+v", len(nudges), msgs)
	}
	body := nudges[0].Content
	for _, want := range []string{
		"Project nudge — Document Expirations is now S1 due_soon (was S2 ok)",
		"Facts driving it:",
		"- US passport expires: due " + due.Format(time.DateOnly),
		"lead 180d",
		"Act on it or update the facts with the project tool; reply with what you did.",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("nudge body:\n%s\n  misses %q", body, want)
		}
	}
	// The parent's nudge names the CHILD's fact: health rolled up, so the
	// message has to point at the thing that actually moved.
	if !strings.Contains(body, "US passport expires") {
		t.Errorf("nudge body:\n%s\n  does not name the subtree fact that drove it", body)
	}

	// The deferred child is delivered on the next tick, now that Ada is free.
	third := tickAt(t, h, inside)
	h.settle()
	if len(third.Nudged) != 1 || third.Nudged[0].Project != "Passport" {
		t.Fatalf("the retry nudged %+v, want the deferred child", third.Nudged)
	}

	// Idempotence: with both recorded, the same tick again nudges nothing —
	// last_health was written in the same transaction as each message.
	settled := len(botMessages(t, h, ada.ID))
	fourth := tickAt(t, h, inside)
	h.settle()
	if len(fourth.Nudged) != 0 || len(fourth.Skipped) != 0 {
		t.Errorf("a repeated tick nudged %+v / skipped %+v, want nothing", fourth.Nudged, fourth.Skipped)
	}
	if got := len(botMessages(t, h, ada.ID)); got != settled {
		t.Errorf("the thread grew from %d to %d messages on a repeated tick", settled, got)
	}

	// Worsening again DOES nudge again: due_soon → overdue is a new fact about
	// the world, not a repeat of the last one.
	past := due.AddDate(0, 0, 1)
	fifth := tickAt(t, h, past)
	h.settle()
	if len(fifth.Nudged) != 1 {
		t.Fatalf("a further deterioration nudged %+v, want a fresh nudge", fifth.Nudged)
	}
	if fifth.Nudged[0].From != HealthDueSoon || fifth.Nudged[0].To != HealthOverdue {
		t.Errorf("second nudge = %q → %q, want due_soon → overdue",
			fifth.Nudged[0].From, fifth.Nudged[0].To)
	}
}

// TestTickRecordsImprovementsSilently: getting better is not news. The tick
// still records the new health, so the NEXT deterioration is measured from it.
func TestTickRecordsImprovementsSilently(t *testing.T) {
	h := newHarness(t, &fakeLLM{reply: "ok"})
	ada := createBot(t, h, "Ada")
	p := postProject(t, h, `{"name":"Passport","ownerBot":"`+string(ada.ID)+`","defaultLeadDays":30}`)
	url := h.ts.URL + "/v1/projects/" + string(p.ID) + "/facts"
	var f Fact
	postExpect(t, http.StatusCreated, url,
		`{"kind":"deadline","title":"US passport expires","due":"`+
			time.Now().UTC().AddDate(0, 0, -1).Format(time.RFC3339)+`"}`, &f)

	if got := tickAt(t, h, time.Time{}); len(got.Nudged) != 1 {
		t.Fatalf("an overdue project on its first tick = %+v, want one nudge", got.Nudged)
	}
	h.settle()
	before := len(botMessages(t, h, ada.ID))

	// The user renews the passport: overdue → ok. Silent, but recorded.
	var updated Fact
	patch(t, h.ts.URL+"/v1/projects/"+string(p.ID)+"/facts/"+string(f.ID),
		`{"due":"`+time.Now().UTC().AddDate(0, 0, 400).Format(time.RFC3339)+`"}`, &updated)
	improved := tickAt(t, h, time.Time{})
	h.settle()
	if len(improved.Nudged) != 0 {
		t.Errorf("an improvement nudged %+v, want silence", improved.Nudged)
	}
	if got := len(botMessages(t, h, ada.ID)); got != before {
		t.Errorf("an improvement wrote to the thread (%d → %d)", before, got)
	}
	// Recorded: sliding back into the window is a fresh deterioration, so it
	// nudges again rather than being swallowed by the old high-water mark.
	back := time.Now().UTC().AddDate(0, 0, 380)
	if got := tickAt(t, h, back); len(got.Nudged) != 1 {
		t.Errorf("relapsing after an improvement = %+v, want a fresh nudge", got.Nudged)
	}
}

// TestTickSkipsAnOwnerlessOrBusyProject: without an owner there is nobody to
// tell, and a bot mid-turn already has a message it has not answered. Both
// leave last_health untouched, so the NEXT tick retries rather than losing the
// news forever.
func TestTickSkipsAnOwnerlessOrBusyProject(t *testing.T) {
	llm := &fakeLLM{reply: "eventually"}
	llm.hold()
	h := newHarness(t, llm)
	ada := createBot(t, h, "Ada")

	orphan := postProject(t, h, `{"name":"Singapore Co"}`)
	owned := postProject(t, h, `{"name":"Passport","ownerBot":"`+string(ada.ID)+`"}`)
	overdue := time.Now().UTC().AddDate(0, 0, -1).Format(time.RFC3339)
	for _, id := range []ProjectID{orphan.ID, owned.ID} {
		var f Fact
		postExpect(t, http.StatusCreated, h.ts.URL+"/v1/projects/"+string(id)+"/facts",
			`{"kind":"deadline","title":"expires","due":"`+overdue+`"}`, &f)
	}

	// Hold the bot with a real in-flight turn.
	var sent Message
	postExpect(t, http.StatusAccepted, h.bot(ada.ID, "/messages"), `{"content":"hello"}`, &sent)
	llm.waitForCall(t, 1)

	got := tickAt(t, h, time.Time{})
	if len(got.Nudged) != 0 {
		t.Errorf("nudged %+v, want nothing: one project has no owner and the other's bot is busy", got.Nudged)
	}
	if len(got.Skipped) != 2 {
		t.Fatalf("skipped %+v, want both projects with a reason each", got.Skipped)
	}
	reasons := map[string]string{}
	for _, sk := range got.Skipped {
		reasons[sk.Project] = sk.Reason
	}
	if !strings.Contains(reasons["Singapore Co"], "owner") {
		t.Errorf("ownerless skip reason = %q, want it to say there is no owner", reasons["Singapore Co"])
	}
	if !strings.Contains(reasons["Passport"], "busy") {
		t.Errorf("busy skip reason = %q, want it to say the bot is busy", reasons["Passport"])
	}

	// Release the turn; the retry nudges, because last_health never moved.
	llm.release()
	h.settle()
	retry := tickAt(t, h, time.Time{})
	h.settle()
	if len(retry.Nudged) != 1 || retry.Nudged[0].Project != "Passport" {
		t.Errorf("the retry nudged %+v, want the once-busy project", retry.Nudged)
	}
	if len(retry.Skipped) != 1 || retry.Skipped[0].Project != "Singapore Co" {
		t.Errorf("the retry skipped %+v, want the still-ownerless project", retry.Skipped)
	}
}

// TestNudgeNamesTheDrivingFacts: the message has to say WHAT to act on, or the
// bot has to go and call the tool to find out. A blocked milestone names its
// blocker; the list is capped so a project of thirty facts is still readable.
func TestNudgeNamesTheDrivingFacts(t *testing.T) {
	h := newHarness(t, &fakeLLM{reply: "ok"})
	ada := createBot(t, h, "Ada")
	p := postProject(t, h, `{"name":"Shanghai Co","ownerBot":"`+string(ada.ID)+`"}`)
	url := h.ts.URL + "/v1/projects/" + string(p.ID) + "/facts"
	var f Fact
	postExpect(t, http.StatusCreated, url,
		`{"kind":"milestone","title":"Photos taken","blocker":"Studio has not sent the digital copies"}`, &f)
	for i := 0; i < 6; i++ {
		postExpect(t, http.StatusCreated, url,
			`{"kind":"milestone","title":"step `+string(rune('a'+i))+`","blocker":"waiting on the lawyer"}`, &f)
	}

	got := tickAt(t, h, time.Time{})
	h.settle()
	if len(got.Nudged) != 1 {
		t.Fatalf("nudged %+v, want the blocked project", got.Nudged)
	}
	var body string
	for _, m := range botMessages(t, h, ada.ID) {
		if strings.HasPrefix(m.Content, "Project nudge — ") {
			body = m.Content
		}
	}
	if !strings.Contains(body, "- Photos taken: blocked — Studio has not sent the digital copies") {
		t.Errorf("nudge body:\n%s\n  does not name the blocked milestone and its blocker", body)
	}
	if !strings.Contains(body, "is now S1 blocked (was S2 ok)") {
		t.Errorf("nudge body:\n%s\n  does not name the transition", body)
	}
	facts := strings.Count(body, "\n- ")
	if facts != 5 {
		t.Errorf("nudge listed %d facts, want it capped at 5:\n%s", facts, body)
	}
}

// TestTickAtIsAQueryParam: the clock is injectable so a test can drive a
// deadline past, and a malformed one is a 400 rather than a silent "now".
func TestTickAtIsAQueryParam(t *testing.T) {
	h := newHarness(t, &fakeLLM{})
	if code, body := postRaw(t, h.ts.URL+"/v1/projects/tick?at=next+tuesday", ""); code != http.StatusBadRequest {
		t.Errorf("a malformed ?at = %d (%s), want 400", code, body)
	}
	// The empty forest is a valid tick, and the two lists are arrays, never
	// null — a client has no nil case for either.
	code, raw := postRaw(t, h.ts.URL+"/v1/projects/tick", "")
	if code != http.StatusOK {
		t.Fatalf("tick over an empty forest = %d (%s), want 200", code, raw)
	}
	for _, want := range []string{`"checked":0`, `"nudged":[]`, `"skipped":[]`} {
		if !strings.Contains(raw, want) {
			t.Errorf("tick body %s\n  misses %s", raw, want)
		}
	}
}

// TestTickEmitsItsChangeRows: the nudge is an ordinary message append plus the
// project's own row moving, so a second client learns both from the feed with
// no new entity and no new trigger.
func TestTickEmitsItsChangeRows(t *testing.T) {
	h := newHarness(t, &fakeLLM{reply: "ok"})
	ada := createBot(t, h, "Ada")
	p := postProject(t, h, `{"name":"Passport","ownerBot":"`+string(ada.ID)+`"}`)
	var f Fact
	postExpect(t, http.StatusCreated, h.ts.URL+"/v1/projects/"+string(p.ID)+"/facts",
		`{"kind":"deadline","title":"expires","due":"`+
			time.Now().UTC().AddDate(0, 0, -1).Format(time.RFC3339)+`"}`, &f)

	mark := topSeq(t, h.store)
	if got := tickAt(t, h, time.Time{}); len(got.Nudged) != 1 {
		t.Fatalf("nudged %+v, want one", got.Nudged)
	}
	rows := logAfter(t, h.store, mark)
	want := map[string]int{"message": 0, "bot": 0, "project": 0}
	for _, row := range rows {
		want[row.entity]++
	}
	if want["message"] < 1 || want["bot"] < 1 || want["project"] != 1 {
		t.Errorf("tick emitted %+v, want the message created, the bot's list metadata updated, "+
			"and exactly one project updated for last_health", rows)
	}
	h.settle()
}

// TestLastHealthStaysOffTheWire: it is bookkeeping for the tick, not state a
// client renders — health is derived and refetched, so shipping a second,
// stale copy of it would only invite a client to trust the wrong one.
func TestLastHealthStaysOffTheWire(t *testing.T) {
	h := newHarness(t, &fakeLLM{reply: "ok"})
	ada := createBot(t, h, "Ada")
	p := postProject(t, h, `{"name":"Passport","ownerBot":"`+string(ada.ID)+`"}`)
	var f Fact
	postExpect(t, http.StatusCreated, h.ts.URL+"/v1/projects/"+string(p.ID)+"/facts",
		`{"kind":"deadline","title":"expires","due":"`+
			time.Now().UTC().AddDate(0, 0, -1).Format(time.RFC3339)+`"}`, &f)
	tickAt(t, h, time.Time{})
	h.settle()

	stored, _, err := h.store.GetProject(p.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if stored.LastHealth != HealthOverdue {
		t.Errorf("stored lastHealth = %q, want the tick to have recorded overdue", stored.LastHealth)
	}
	_, raw := getRaw(t, h.ts.URL+"/v1/projects/"+string(p.ID))
	for _, key := range []string{"lastHealth", "last_health", "LastHealth"} {
		if strings.Contains(raw, key) {
			t.Errorf("project JSON %s\n  leaks %s", raw, key)
		}
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
}

// TestPreOwnerDatabaseOpensUnchanged: a database written before either column
// existed opens and reads as unowned, never nudged.
func TestPreOwnerDatabaseOpensUnchanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pre-owner.db")
	seedPreHierarchyDB(t, path)
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open a pre-owner database: %v", err)
	}
	defer s.Close()
	listed, err := s.ListProjects()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, p := range listed {
		if p.OwnerBot != "" || p.EffectiveOwner != "" || p.LastHealth != "" {
			t.Errorf("%s = %q / %q / %q, want unowned and never ticked",
				p.Name, p.OwnerBot, p.EffectiveOwner, p.LastHealth)
		}
	}
	// The migrated columns are writable: a tick over the legacy rows records.
	if _, err := s.TickProjects(time.Now().UTC()); err != nil {
		t.Fatalf("tick a migrated database: %v", err)
	}
	after, _ := s.ListProjects()
	if byName(t, after, "Passports").LastHealth == "" {
		t.Error("the tick recorded no health on a migrated project")
	}
}

// ── Unit 7: the doc ───────────────────────────────────────────────────────────

// readProjectsDoc reads the doc beside this package, so the drift tests below
// read the shipped file rather than a copy.
func readProjectsDoc() (string, error) {
	raw, err := os.ReadFile("PROJECTS.md")
	return string(raw), err
}

// TestProjectsDocDescribesOwnersAndNudges: the operator system prompt is what a
// user pastes into the bot that receives these, so it has to say what a nudge
// is and what to do with one.
func TestProjectsDocDescribesOwnersAndNudges(t *testing.T) {
	doc, err := readProjectsDoc()
	if err != nil {
		t.Fatalf("read PROJECTS.md: %v", err)
	}
	for _, must := range []string{
		"## Owner and nudges",
		"effectiveOwner",
		"lastHealth",
		"/v1/projects/tick",
		"Project nudge — ",
	} {
		if !strings.Contains(doc, must) {
			t.Errorf("PROJECTS.md never mentions %q", must)
		}
	}
	// The pasteable system prompt has to prepare the bot for the message it
	// will actually receive.
	prompt := doc[strings.Index(doc, "## A system prompt for a project-operator bot"):]
	for _, must := range []string{"Project nudge", "reply"} {
		if !strings.Contains(prompt, must) {
			t.Errorf("the operator system prompt never mentions %q", must)
		}
	}
}
