package botnet

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Thresholds: a project owns the lead window its dated facts default to, and
// that default INHERITS down the tree. Tested at the same four levels the rest
// of the service is: the derivation pass, the store's write paths, the REST
// face, and the tool a bot drives it through.

// ── Unit 1: storage and derivation ────────────────────────────────────────────

// TestDefaultLeadDaysRoundTrip: the column stores, survives a close/reopen, is
// cleared by an explicit zero patch, and refuses a negative value.
func TestDefaultLeadDaysRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "leads.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	p, err := s.CreateProject(Project{Name: "Document Expirations", DefaultLeadDays: 180}, userAuthor)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if p.DefaultLeadDays != 180 || p.EffectiveLeadDays != 180 {
		t.Errorf("created project = %d / %d, want its own 180 both stored and effective",
			p.DefaultLeadDays, p.EffectiveLeadDays)
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
	if reopened.DefaultLeadDays != 180 {
		t.Errorf("defaultLeadDays after a reopen = %d, want 180", reopened.DefaultLeadDays)
	}

	// Zero is a real patch value: it CLEARS the project's own default and the
	// effective one falls back to the global 30.
	zero := 0
	cleared, err := s.UpdateProject(p.ID, ProjectPatch{DefaultLeadDays: &zero})
	if err != nil {
		t.Fatalf("clear the default: %v", err)
	}
	if cleared.DefaultLeadDays != 0 || cleared.EffectiveLeadDays != defaultLeadDays {
		t.Errorf("cleared project = %d / %d, want 0 stored and the global %d effective",
			cleared.DefaultLeadDays, cleared.EffectiveLeadDays, defaultLeadDays)
	}

	negative := -1
	if _, err := s.UpdateProject(p.ID, ProjectPatch{DefaultLeadDays: &negative}); err == nil {
		t.Error("a negative defaultLeadDays was accepted; a lead window cannot run backwards")
	}
	if _, err := s.CreateProject(Project{Name: "Backwards", DefaultLeadDays: -5}, userAuthor); err == nil {
		t.Error("created a project with a negative defaultLeadDays")
	}
}

// TestPreThresholdDatabaseOpensUnchanged: a database written before the column
// existed opens, and every legacy project reads the global default.
func TestPreThresholdDatabaseOpensUnchanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pre-threshold.db")
	seedPreHierarchyDB(t, path)
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open a pre-threshold database: %v", err)
	}
	defer s.Close()

	listed, err := s.ListProjects()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, p := range listed {
		if p.DefaultLeadDays != 0 {
			t.Errorf("%s gained the default lead %d on migration, want 0", p.Name, p.DefaultLeadDays)
		}
		if p.EffectiveLeadDays != defaultLeadDays {
			t.Errorf("%s effective lead = %d, want the global %d", p.Name, p.EffectiveLeadDays, defaultLeadDays)
		}
	}
	// The migrated column is writable.
	lead := 90
	if _, err := s.UpdateProject(byName(t, listed, "Passports").ID, ProjectPatch{DefaultLeadDays: &lead}); err != nil {
		t.Fatalf("patch a migrated project's default lead: %v", err)
	}
}

// TestEffectiveLeadDaysInheritance: the effective lead is the nearest ANCESTOR
// answer — own default, else the closest ancestor that set one, else the global
// 30. It is derived in the same pass health is, so a listing already carries it.
func TestEffectiveLeadDaysInheritance(t *testing.T) {
	s := newEventStore(t)
	root := mustProject(t, s, "Document Expirations", "")
	lead := 180
	if _, err := s.UpdateProject(root.ID, ProjectPatch{DefaultLeadDays: &lead}); err != nil {
		t.Fatalf("set the root's default lead: %v", err)
	}
	passport := mustChild(t, s, "Passport", root.ID)
	stamp := mustChild(t, s, "Biometrics appointment", passport.ID)
	visa := mustChild(t, s, "China Q2 Visa", root.ID)
	ninety := 90
	if _, err := s.UpdateProject(visa.ID, ProjectPatch{DefaultLeadDays: &ninety}); err != nil {
		t.Fatalf("set the visa's default lead: %v", err)
	}
	loose := mustProject(t, s, "Singapore Co", "")

	listed, err := s.ListProjects()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, want := range []struct {
		name string
		lead int
	}{
		{"Document Expirations", 180}, // its own
		{"Passport", 180},             // inherited from the parent
		{"Biometrics appointment", 180},
		{"China Q2 Visa", 90},             // its own overrides the ancestor's
		{"Singapore Co", defaultLeadDays}, // no ancestor set one: the global default
	} {
		if got := byName(t, listed, want.name).EffectiveLeadDays; got != want.lead {
			t.Errorf("%s effectiveLeadDays = %d, want %d", want.name, got, want.lead)
		}
	}
	// A grandchild under an overriding child takes the CLOSER answer.
	under := mustChild(t, s, "Visa photos", visa.ID)
	fresh, _, err := s.GetProject(under.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if fresh.EffectiveLeadDays != 90 {
		t.Errorf("a grandchild's effectiveLeadDays = %d, want the nearer ancestor's 90", fresh.EffectiveLeadDays)
	}
	_ = stamp
	_ = loose
}

// TestCreateFactTakesTheProjectsLead: a dated fact created with no lead is
// JUDGED by the project's effective lead, not the flat 30 — which is the whole
// point of setting the default once on "Document Expirations". It is resolved
// at read time onto EffectiveLeadDays rather than baked into the row, so an
// explicit lead still wins and moving the project's default now moves every
// fact inheriting it (see the LeadDays DECISION in schema.go).
func TestCreateFactTakesTheProjectsLead(t *testing.T) {
	s := newEventStore(t)
	root := mustProject(t, s, "Document Expirations", "")
	lead := 180
	if _, err := s.UpdateProject(root.ID, ProjectPatch{DefaultLeadDays: &lead}); err != nil {
		t.Fatalf("set the default lead: %v", err)
	}
	child := mustChild(t, s, "Passport", root.ID)
	due := time.Now().UTC().AddDate(0, 0, 300)

	inherited := mustFact(t, s, child.ID, Fact{Kind: FactDeadline, Title: "US passport expires", Due: due}, userAuthor)
	if inherited.LeadDays != 0 {
		t.Errorf("a fact created with no lead stored %d, want 0 — the row keeps what it was given", inherited.LeadDays)
	}
	if inherited.EffectiveLeadDays != 180 {
		t.Errorf("a fact created with no lead is judged by %d, want the project's inherited 180", inherited.EffectiveLeadDays)
	}
	explicit := mustFact(t, s, child.ID,
		Fact{Kind: FactDeadline, Title: "UK passport expires", Due: due, LeadDays: 45}, userAuthor)
	if explicit.LeadDays != 45 || explicit.EffectiveLeadDays != 45 {
		t.Errorf("an explicit lead = stored %d / effective %d, want 45 / 45", explicit.LeadDays, explicit.EffectiveLeadDays)
	}
	// A project with no default anywhere above it still gets the global one.
	loose := mustProject(t, s, "Singapore Co", "")
	global := mustFact(t, s, loose.ID, Fact{Kind: FactDeadline, Title: "annual return", Due: due}, userAuthor)
	if global.EffectiveLeadDays != defaultLeadDays {
		t.Errorf("a fact under no default is judged by %d, want the global %d", global.EffectiveLeadDays, defaultLeadDays)
	}
	// Changing the project's default rewrites NO row — and now moves what the
	// inheriting facts mean, on the very next read. That is the point of
	// resolving at read time rather than at write time.
	sixty := 60
	if _, err := s.UpdateProject(root.ID, ProjectPatch{DefaultLeadDays: &sixty}); err != nil {
		t.Fatalf("change the default lead: %v", err)
	}
	after := factNamedInTest(t, s, child, "US passport expires")
	if after.LeadDays != 0 {
		t.Errorf("an inheriting fact's STORED lead = %d after the default moved, want it untouched at 0", after.LeadDays)
	}
	if after.EffectiveLeadDays != 60 {
		t.Errorf("an inheriting fact is now judged by %d, want the project's new 60", after.EffectiveLeadDays)
	}
	stillExplicit := factNamedInTest(t, s, child, "UK passport expires")
	if stillExplicit.EffectiveLeadDays != 45 {
		t.Errorf("a fact with its own window moved to %d; an explicit lead does not inherit", stillExplicit.EffectiveLeadDays)
	}
	// An undated kind keeps 0: it has no window to open.
	note := mustFact(t, s, child.ID, Fact{Kind: FactNote, Title: "agent", Body: "phone number"}, userAuthor)
	if note.LeadDays != 0 {
		t.Errorf("a note's lead = %d, want 0", note.LeadDays)
	}
}

// ── Unit 2: the wire ──────────────────────────────────────────────────────────

// TestDefaultLeadDaysOverTheWire: POST and PATCH accept the field, every
// project row carries both the stored and the derived value, and a negative one
// is a 400.
func TestDefaultLeadDaysOverTheWire(t *testing.T) {
	h := newHarness(t, &fakeLLM{})
	root := postProject(t, h, `{"name":"Document Expirations","defaultLeadDays":180}`)
	if root.DefaultLeadDays != 180 || root.EffectiveLeadDays != 180 {
		t.Errorf("created project = %+v, want 180 stored and effective", root)
	}
	child := postProject(t, h, `{"name":"Passport","parentId":"`+string(root.ID)+`"}`)
	if child.DefaultLeadDays != 0 || child.EffectiveLeadDays != 180 {
		t.Errorf("child = %d / %d, want 0 stored and 180 inherited", child.DefaultLeadDays, child.EffectiveLeadDays)
	}

	// PATCH 0 clears, and the child's inheritance falls back with it.
	var patched Project
	patch(t, h.ts.URL+"/v1/projects/"+string(root.ID), `{"defaultLeadDays":0}`, &patched)
	if patched.DefaultLeadDays != 0 || patched.EffectiveLeadDays != defaultLeadDays {
		t.Errorf("cleared project = %d / %d, want 0 and the global %d",
			patched.DefaultLeadDays, patched.EffectiveLeadDays, defaultLeadDays)
	}

	if code, body := patchRaw(t, h.ts.URL+"/v1/projects/"+string(root.ID), `{"defaultLeadDays":-1}`); code != http.StatusBadRequest {
		t.Errorf("a negative defaultLeadDays = %d (%s), want 400", code, body)
	}
	if code, body := postRaw(t, h.ts.URL+"/v1/projects", `{"name":"Backwards","defaultLeadDays":-1}`); code != http.StatusBadRequest {
		t.Errorf("a negative defaultLeadDays on create = %d (%s), want 400", code, body)
	}

	// Both fields are on every listing row, so the sheet can pre-fill the
	// inherited value without a second call.
	resp, err := http.Get(h.ts.URL + "/v1/projects")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	defer resp.Body.Close()
	var raw json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	for _, key := range []string{`"defaultLeadDays"`, `"effectiveLeadDays"`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("listing %s\n  misses %s", raw, key)
		}
	}
}

// ── Unit 3: the tool ──────────────────────────────────────────────────────────

// TestProjectToolDefaultLeadDays: a bot sets the project's default once, and
// every health line it reads back names the effective window.
func TestProjectToolDefaultLeadDays(t *testing.T) {
	s := newEventStore(t)
	bot := newBot(t, s)
	tb := NewBotToolbox(s, bot.ID, nil)

	got := runProject(t, tb,
		`{"command":"create","project":"Document Expirations","default_lead_days":"180"}`)
	if !strings.Contains(got, "lead 180d") {
		t.Errorf("create with a default lead = %q, want the health line to name it", got)
	}
	root, err := s.ProjectByName("Document Expirations")
	if err != nil {
		t.Fatalf("project by name: %v", err)
	}
	if root.DefaultLeadDays != 180 {
		t.Errorf("stored defaultLeadDays = %d, want 180", root.DefaultLeadDays)
	}

	// A child inherits it, and a fact added there with no lead takes 180.
	runProject(t, tb, `{"command":"create","project":"Passport","parent":"Document Expirations"}`)
	added := runProject(t, tb,
		`{"command":"add_fact","project":"Passport","kind":"deadline","title":"US passport expires","due":"2027-03-14T00:00:00Z"}`)
	if !strings.Contains(added, "lead 180d") {
		t.Errorf("add_fact under an inherited default = %q, want the health line to say lead 180d", added)
	}
	child, err := s.ProjectByName("Passport")
	if err != nil {
		t.Fatalf("project by name: %v", err)
	}
	facts, err := s.ListFacts(child.ID)
	if err != nil {
		t.Fatalf("list facts: %v", err)
	}
	if len(facts) != 1 || facts[0].LeadDays != 0 || facts[0].EffectiveLeadDays != 180 {
		t.Fatalf("facts = %+v, want one fact storing 0 and judged by the inherited 180", facts)
	}
	// show names the effective lead too, so a model reading a project knows
	// what window its next fact will take.
	if shown := runProject(t, tb, `{"command":"show","project":"Passport"}`); !strings.Contains(shown, "lead 180d") {
		t.Errorf("show = %q, want the header to carry the effective lead", shown)
	}

	// update sets it on the child, overriding the inheritance.
	if got := runProject(t, tb,
		`{"command":"update","project":"Passport","default_lead_days":"45"}`); !strings.Contains(got, "lead 45d") {
		t.Errorf("update = %q, want the health line to name the new lead", got)
	}
	// "0" is a real value: it clears the override and inheritance resumes.
	if got := runProject(t, tb,
		`{"command":"update","project":"Passport","default_lead_days":"0"}`); !strings.Contains(got, "lead 180d") {
		t.Errorf("clearing the override = %q, want the inherited 180 back", got)
	}
	// A non-number is an instructive error, not a failed turn.
	if got := runProject(t, tb,
		`{"command":"update","project":"Passport","default_lead_days":"soon"}`); !strings.Contains(got, "error:") {
		t.Errorf("a non-numeric default_lead_days = %q, want an instructive error", got)
	}
}

// TestUpdateFactChangesTheRule: update_fact can change a recurring fact's rrule
// and tz, and the change RE-PROJECTS — the calendar event the fact owns carries
// the new rule, so /v1/instances lands on the new dates rather than the old.
func TestUpdateFactChangesTheRule(t *testing.T) {
	s := newEventStore(t)
	bot := newBot(t, s)
	tb := NewBotToolbox(s, bot.ID, nil)

	runProject(t, tb, `{"command":"create","project":"Singapore Co"}`)
	runProject(t, tb, `{"command":"add_fact","project":"Singapore Co","kind":"recurring",`+
		`"title":"annual return","due":"2027-03-14T00:00:00Z","rrule":"FREQ=YEARLY","tz":"Asia/Singapore"}`)

	p, err := s.ProjectByName("Singapore Co")
	if err != nil {
		t.Fatalf("project by name: %v", err)
	}
	before, err := s.ListFacts(p.ID)
	if err != nil || len(before) != 1 {
		t.Fatalf("facts = %+v (%v), want the one recurring fact", before, err)
	}
	ev, err := s.GetEvent(before[0].EventID)
	if err != nil {
		t.Fatalf("get the projected event: %v", err)
	}
	if ev.RRule != "FREQ=YEARLY" || ev.TZ != "Asia/Singapore" {
		t.Fatalf("projected event = %q / %q, want the fact's rule", ev.RRule, ev.TZ)
	}

	got := runProject(t, tb, `{"command":"update_fact","project":"Singapore Co","title":"annual return",`+
		`"rrule":"FREQ=MONTHLY;COUNT=3","tz":"Asia/Tokyo"}`)
	if strings.Contains(got, "error:") {
		t.Fatalf("update_fact with a new rule = %q, want it accepted", got)
	}
	after, err := s.GetFact(before[0].ID)
	if err != nil {
		t.Fatalf("get fact: %v", err)
	}
	if after.RRule != "FREQ=MONTHLY;COUNT=3" || after.TZ != "Asia/Tokyo" {
		t.Errorf("stored fact = %q / %q, want the new rule and zone", after.RRule, after.TZ)
	}
	if after.EventID != before[0].EventID {
		t.Errorf("event id moved %q → %q; the projection must converge on ONE event", before[0].EventID, after.EventID)
	}
	reprojected, err := s.GetEvent(after.EventID)
	if err != nil {
		t.Fatalf("get the re-projected event: %v", err)
	}
	if reprojected.RRule != "FREQ=MONTHLY;COUNT=3" || reprojected.TZ != "Asia/Tokyo" {
		t.Errorf("re-projected event = %q / %q, want the fact's NEW rule — the calendar would show the old dates",
			reprojected.RRule, reprojected.TZ)
	}
	// The expansion the calendar reads is monthly now, not yearly.
	instances, err := s.Instances(time.Date(2027, time.March, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2027, time.June, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("instances: %v", err)
	}
	count := 0
	for _, in := range instances {
		if in.EventID == after.EventID {
			count++
		}
	}
	if count != 3 {
		t.Errorf("the fact's event expanded to %d instances in the quarter, want the new rule's 3", count)
	}

	// An unparseable rule is an instructive error, and the fact is untouched.
	if bad := runProject(t, tb, `{"command":"update_fact","project":"Singapore Co","title":"annual return",`+
		`"rrule":"FREQ=FORTNIGHTLY"}`); !strings.Contains(bad, "error:") {
		t.Errorf("a bad rrule = %q, want an instructive error", bad)
	}
	if unchanged, _ := s.GetFact(after.ID); unchanged.RRule != "FREQ=MONTHLY;COUNT=3" {
		t.Errorf("a refused rule change still wrote %q", unchanged.RRule)
	}
	// A non-recurring fact may not grow a rule: the kind's rules refuse it.
	runProject(t, tb, `{"command":"add_fact","project":"Singapore Co","kind":"milestone","title":"notarize"}`)
	if got := runProject(t, tb, `{"command":"update_fact","project":"Singapore Co","title":"notarize",`+
		`"rrule":"FREQ=YEARLY","tz":"Asia/Singapore"}`); !strings.Contains(got, "error:") {
		t.Errorf("an rrule on a milestone = %q, want an instructive refusal", got)
	}
}

// TestLadderNamesTheProjectDefault: step 1 must tell a bot the lead comes from
// the project, or it goes on typing 180 into every passport fact by hand.
func TestLadderNamesTheProjectDefault(t *testing.T) {
	doc := projectToolDef().Function.Description
	for _, must := range []string{"default_lead_days", "rrule", "tz"} {
		if !strings.Contains(doc, must) {
			t.Errorf("tool description never mentions %q", must)
		}
	}
	if !strings.Contains(projectLadder, "default_lead_days") {
		t.Error("ladder step 1 does not name default_lead_days; a bot will keep setting the lead per fact")
	}
}
