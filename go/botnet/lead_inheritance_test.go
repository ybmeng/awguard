package botnet

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// A fact's lead window is ONE rule at both write paths: a stored 0 means
// "inherit the project's effective lead", and the resolution happens at READ
// time in EffectiveLeadDays. These tests pin that the two routes agree, that
// the derivation follows the tree, and the consequence that falls out of it —
// moving a project's default moves the health of every fact inheriting it.

// leadOf returns one project's fact by title, from the store.
func factNamedInTest(t *testing.T, s *Store, p Project, title string) Fact {
	t.Helper()
	facts, err := s.ListFacts(p.ID)
	if err != nil {
		t.Fatalf("list facts: %v", err)
	}
	for _, f := range facts {
		if strings.EqualFold(f.Title, title) {
			return f
		}
	}
	t.Fatalf("no fact %q in %q", title, p.Name)
	return Fact{}
}

// TestCreateAndPatchAgreeOnLeadZero is the defect this rule settles: the same
// number must mean the same thing whichever route wrote it. Create with 0 and
// patch to 0 must leave IDENTICAL stored and derived values.
func TestCreateAndPatchAgreeOnLeadZero(t *testing.T) {
	s := newEventStore(t)
	p, err := s.CreateProject(Project{Name: "Document Expirations", DefaultLeadDays: 180}, userAuthor)
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	due := time.Now().UTC().AddDate(1, 0, 0)

	// Created with an explicit 0.
	created, err := s.CreateFact(p.ID, Fact{Kind: FactDeadline, Title: "created at zero", Due: due}, userAuthor)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.LeadDays != 0 {
		t.Errorf("stored leadDays after a create with none = %d, want 0 — create must not bake the project's lead into the row", created.LeadDays)
	}

	// Created with 180, then patched back to 0.
	patched, err := s.CreateFact(p.ID, Fact{Kind: FactDeadline, Title: "patched to zero", Due: due, LeadDays: 180}, userAuthor)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	zero := 0
	if patched, err = s.UpdateFact(patched.ID, FactPatch{LeadDays: &zero}); err != nil {
		t.Fatalf("patch to zero: %v", err)
	}
	if patched.LeadDays != 0 {
		t.Errorf("stored leadDays after a patch to 0 = %d, want 0", patched.LeadDays)
	}

	// The two facts differ only in how they got there.
	a := factNamedInTest(t, s, p, "created at zero")
	b := factNamedInTest(t, s, p, "patched to zero")
	if a.LeadDays != b.LeadDays {
		t.Errorf("stored leadDays: create %d vs patch %d — the two routes still disagree", a.LeadDays, b.LeadDays)
	}
	if a.EffectiveLeadDays != b.EffectiveLeadDays {
		t.Errorf("effective leadDays: create %d vs patch %d", a.EffectiveLeadDays, b.EffectiveLeadDays)
	}
	if a.EffectiveLeadDays != 180 {
		t.Errorf("effective leadDays = %d, want the project's 180", a.EffectiveLeadDays)
	}
}

// TestFactEffectiveLeadDaysDerivation: own value wins, 0 inherits, and the
// inheritance is the PROJECT's effective answer — so it reaches through
// ancestors exactly as the project's own does.
func TestFactEffectiveLeadDaysDerivation(t *testing.T) {
	s := newEventStore(t)
	root, err := s.CreateProject(Project{Name: "Document Expirations", DefaultLeadDays: 180}, userAuthor)
	if err != nil {
		t.Fatalf("create root: %v", err)
	}
	// A child with no default of its own inherits 180 from the root.
	child, err := s.CreateProject(Project{Name: "Passport", ParentID: root.ID}, userAuthor)
	if err != nil {
		t.Fatalf("create child: %v", err)
	}
	// A grandchild that sets its own 90 overrides it.
	grand, err := s.CreateProject(Project{Name: "China Visa", ParentID: child.ID, DefaultLeadDays: 90}, userAuthor)
	if err != nil {
		t.Fatalf("create grandchild: %v", err)
	}
	due := time.Now().UTC().AddDate(1, 0, 0)

	cases := []struct {
		name    string
		project Project
		lead    int
		want    int
	}{
		{"inherits the root's own default", root, 0, 180},
		{"inherits through a silent child", child, 0, 180},
		{"a nearer ancestor's own default wins", grand, 0, 90},
		{"the fact's own lead beats every project", grand, 45, 45},
	}
	for _, c := range cases {
		f, err := s.CreateFact(c.project.ID,
			Fact{Kind: FactDeadline, Title: c.name, Due: due, LeadDays: c.lead}, userAuthor)
		if err != nil {
			t.Fatalf("%s: create: %v", c.name, err)
		}
		if f.LeadDays != c.lead {
			t.Errorf("%s: stored leadDays = %d, want the value as given (%d)", c.name, f.LeadDays, c.lead)
		}
		got := factNamedInTest(t, s, c.project, c.name)
		if got.EffectiveLeadDays != c.want {
			t.Errorf("%s: effectiveLeadDays = %d, want %d", c.name, got.EffectiveLeadDays, c.want)
		}
	}

	// With no project default anywhere, the global 30 is the answer.
	plain, err := s.CreateProject(Project{Name: "Loose ends"}, userAuthor)
	if err != nil {
		t.Fatalf("create plain: %v", err)
	}
	if _, err := s.CreateFact(plain.ID, Fact{Kind: FactDeadline, Title: "x", Due: due}, userAuthor); err != nil {
		t.Fatalf("create fact: %v", err)
	}
	if got := factNamedInTest(t, s, plain, "x"); got.EffectiveLeadDays != defaultLeadDays {
		t.Errorf("effectiveLeadDays with no default anywhere = %d, want the global %d", got.EffectiveLeadDays, defaultLeadDays)
	}
}

// TestProjectDefaultLeadMovesInheritingFactHealth is the consequence of
// resolving at read time, and the point of doing it: widening a project's
// window changes what its inheriting facts mean on the very next read, with no
// migration and no touch of the fact rows.
func TestProjectDefaultLeadMovesInheritingFactHealth(t *testing.T) {
	s := newEventStore(t)
	root, err := s.CreateProject(Project{Name: "Document Expirations", DefaultLeadDays: 30}, userAuthor)
	if err != nil {
		t.Fatalf("create root: %v", err)
	}
	child, err := s.CreateProject(Project{Name: "Passport", ParentID: root.ID}, userAuthor)
	if err != nil {
		t.Fatalf("create child: %v", err)
	}
	// 200 days out: outside a 30-day window, inside a 400-day one.
	due := time.Now().UTC().AddDate(0, 0, 200)
	if _, err := s.CreateFact(child.ID, Fact{Kind: FactDeadline, Title: "inherits", Due: due}, userAuthor); err != nil {
		t.Fatalf("create inheriting fact: %v", err)
	}
	// A sibling that names its own narrow window must NOT move.
	if _, err := s.CreateFact(child.ID,
		Fact{Kind: FactDeadline, Title: "own lead", Due: due, LeadDays: 10}, userAuthor); err != nil {
		t.Fatalf("create explicit fact: %v", err)
	}

	before, _, err := s.GetProject(child.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if before.Health != HealthOK {
		t.Fatalf("health with a 30-day window = %q, want ok", before.Health)
	}

	// Widen the ROOT's default. The child inherits it, and so does the fact.
	wide := 400
	if _, err := s.UpdateProject(root.ID, ProjectPatch{DefaultLeadDays: &wide}); err != nil {
		t.Fatalf("widen the default: %v", err)
	}
	after, facts, err := s.GetProject(child.ID)
	if err != nil {
		t.Fatalf("get after: %v", err)
	}
	if after.Health != HealthDueSoon {
		t.Errorf("health after widening the ancestor's default = %q, want due_soon — the fact inherits", after.Health)
	}
	for _, f := range facts {
		switch f.Title {
		case "inherits":
			if f.LeadDays != 0 || f.EffectiveLeadDays != 400 {
				t.Errorf("inheriting fact = stored %d / effective %d, want 0 / 400", f.LeadDays, f.EffectiveLeadDays)
			}
		case "own lead":
			if f.LeadDays != 10 || f.EffectiveLeadDays != 10 {
				t.Errorf("explicit fact = stored %d / effective %d, want 10 / 10 — its own window is untouched", f.LeadDays, f.EffectiveLeadDays)
			}
		}
	}
	// The root rolls the child's new urgency up.
	rootAfter, _, err := s.GetProject(root.ID)
	if err != nil {
		t.Fatalf("get root: %v", err)
	}
	if rootAfter.Health != HealthDueSoon {
		t.Errorf("root health = %q, want the descendant's due_soon rolled up", rootAfter.Health)
	}
}

// TestExplicitLeadRowsSurviveUntouched: rows the OLD create baked a resolved
// lead into keep it. There is no migration rewriting them, and an explicit
// value is indistinguishable from one the user meant — so they simply stop
// inheriting, which is what an explicit value has always meant.
func TestExplicitLeadRowsSurviveUntouched(t *testing.T) {
	s := newEventStore(t)
	p, err := s.CreateProject(Project{Name: "Document Expirations", DefaultLeadDays: 60}, userAuthor)
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	due := time.Now().UTC().AddDate(1, 0, 0)
	// A row exactly as the old create would have left it: 180 baked in from a
	// project default that has since become 60.
	baked, err := s.CreateFact(p.ID,
		Fact{Kind: FactDeadline, Title: "legacy row", Due: due, LeadDays: 180}, userAuthor)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if baked.LeadDays != 180 {
		t.Fatalf("stored leadDays = %d, want the explicit 180", baked.LeadDays)
	}
	got := factNamedInTest(t, s, p, "legacy row")
	if got.LeadDays != 180 || got.EffectiveLeadDays != 180 {
		t.Errorf("legacy row = stored %d / effective %d, want 180 / 180 — an explicit value does not inherit",
			got.LeadDays, got.EffectiveLeadDays)
	}
}

// TestFactWireCarriesEffectiveLeadDays: the client renders the effective
// window, so it has to be on the wire beside the stored one.
func TestFactWireCarriesEffectiveLeadDays(t *testing.T) {
	h := newHarness(t, &fakeLLM{})
	var p Project
	postExpect(t, http.StatusCreated, h.ts.URL+"/v1/projects",
		`{"name":"Document Expirations","defaultLeadDays":180}`, &p)
	var f Fact
	postExpect(t, http.StatusCreated, h.ts.URL+"/v1/projects/"+string(p.ID)+"/facts",
		`{"kind":"deadline","title":"US passport expires","due":"2027-03-14T00:00:00Z"}`, &f)

	raw := rawGet(t, h.ts.URL+"/v1/projects/"+string(p.ID))
	if !strings.Contains(raw, `"leadDays":0`) {
		t.Errorf("detail body %s\n  does not carry the stored leadDays 0", raw)
	}
	if !strings.Contains(raw, `"effectiveLeadDays":180`) {
		t.Errorf("detail body %s\n  does not carry effectiveLeadDays 180", raw)
	}

	// PATCHing the fact to an explicit window moves both numbers together.
	var patched Fact
	patch(t, h.ts.URL+"/v1/projects/"+string(p.ID)+"/facts/"+string(f.ID), `{"leadDays":30}`, &patched)
	if patched.LeadDays != 30 || patched.EffectiveLeadDays != 30 {
		t.Errorf("after an explicit patch = stored %d / effective %d, want 30 / 30", patched.LeadDays, patched.EffectiveLeadDays)
	}
	// And back to 0 restores the inheritance.
	var cleared Fact
	patch(t, h.ts.URL+"/v1/projects/"+string(p.ID)+"/facts/"+string(f.ID), `{"leadDays":0}`, &cleared)
	if cleared.LeadDays != 0 || cleared.EffectiveLeadDays != 180 {
		t.Errorf("after clearing = stored %d / effective %d, want 0 / 180", cleared.LeadDays, cleared.EffectiveLeadDays)
	}
}

// TestToolRendersEffectiveLeadAndMarksInherited: a bot reading "lead 180d" must
// be able to tell a window the fact owns from one it is borrowing, or it will
// patch the wrong thing.
func TestToolRendersEffectiveLeadAndMarksInherited(t *testing.T) {
	s := newEventStore(t)
	bot := newBot(t, s)
	tb := NewBotToolbox(s, bot.ID, nil)
	p, err := s.CreateProject(Project{Name: "Document Expirations", DefaultLeadDays: 180}, userAuthor)
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	due := time.Now().UTC().AddDate(1, 0, 0)
	if _, err := s.CreateFact(p.ID, Fact{Kind: FactDeadline, Title: "inherited one", Due: due}, userAuthor); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.CreateFact(p.ID,
		Fact{Kind: FactDeadline, Title: "own one", Due: due, LeadDays: 45}, userAuthor); err != nil {
		t.Fatalf("create: %v", err)
	}

	show := runProject(t, tb, `{"command":"show","project":"Document Expirations"}`)
	for _, want := range []string{"lead 180d (project default)", "lead 45d"} {
		if !strings.Contains(show, want) {
			t.Errorf("show = %q\n  wants %q", show, want)
		}
	}
	if strings.Contains(show, "lead 0d") {
		t.Errorf("show = %q\n  renders the STORED 0 rather than the effective window", show)
	}
}

// TestToolAddFactLeadZeroInherits: the tool's own contract said "0 is a real
// supplied value"; under the settled rule a supplied 0 stores 0 and inherits,
// exactly as the REST route does.
func TestToolAddFactLeadZeroInherits(t *testing.T) {
	s := newEventStore(t)
	bot := newBot(t, s)
	tb := NewBotToolbox(s, bot.ID, nil)
	p, err := s.CreateProject(Project{Name: "Document Expirations", DefaultLeadDays: 180}, userAuthor)
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	due := time.Now().UTC().AddDate(1, 0, 0).Format(time.RFC3339)
	runProject(t, tb, `{"command":"add_fact","project":"Document Expirations","kind":"deadline",`+
		`"title":"explicit zero","due":"`+due+`","lead_days":"0"}`)
	runProject(t, tb, `{"command":"add_fact","project":"Document Expirations","kind":"deadline",`+
		`"title":"omitted","due":"`+due+`"}`)

	zero := factNamedInTest(t, s, p, "explicit zero")
	omitted := factNamedInTest(t, s, p, "omitted")
	if zero.LeadDays != omitted.LeadDays || zero.EffectiveLeadDays != omitted.EffectiveLeadDays {
		t.Errorf("lead_days \"0\" = %d/%d but omitted = %d/%d; one rule means one answer",
			zero.LeadDays, zero.EffectiveLeadDays, omitted.LeadDays, omitted.EffectiveLeadDays)
	}
	if zero.EffectiveLeadDays != 180 {
		t.Errorf("effective lead = %d, want the project's 180", zero.EffectiveLeadDays)
	}
}

// TestFactMarshalKeepsBothLeads: effectiveLeadDays is DERIVED, so it must be
// present on the wire but must never be read back in as authored state.
func TestFactMarshalKeepsBothLeads(t *testing.T) {
	raw, err := json.Marshal(Fact{Kind: FactDeadline, Title: "x", LeadDays: 0, EffectiveLeadDays: 180})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{`"leadDays":0`, `"effectiveLeadDays":180`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("fact JSON %s misses %s", raw, want)
		}
	}
}

// TestLeadZeroRoundTripsOnEveryWritePath is the regression rock1-ui asked for,
// stated as the property rather than as three separate walks: for EVERY route
// that can write a lead, creating with 0 and patching to 0 must leave the same
// stored value and the same derived one. The routes are the REST handlers, the
// project tool, and the store beneath both.
func TestLeadZeroRoundTripsOnEveryWritePath(t *testing.T) {
	h := newHarness(t, &fakeLLM{})
	bot := newBot(t, h.store)
	tb := NewBotToolbox(h.store, bot.ID, nil)

	var p Project
	postExpect(t, http.StatusCreated, h.ts.URL+"/v1/projects",
		`{"name":"Document Expirations","defaultLeadDays":180}`, &p)
	factsURL := h.ts.URL + "/v1/projects/" + string(p.ID) + "/facts"
	due := time.Now().UTC().AddDate(1, 0, 0).Format(time.RFC3339)

	// Each route writes a lead of 0 twice: once on the way in, once as a patch
	// over an explicit non-zero. Both must land in the same place.
	restCreate := func(title, body string) Fact {
		var f Fact
		postExpect(t, http.StatusCreated, factsURL, body, &f)
		return f
	}
	var got []struct {
		route string
		fact  Fact
	}
	add := func(route string, f Fact) {
		got = append(got, struct {
			route string
			fact  Fact
		}{route, f})
	}

	add("REST create with an explicit 0", restCreate("a",
		`{"kind":"deadline","title":"rest zero","due":"`+due+`","leadDays":0}`))
	add("REST create omitting the field", restCreate("b",
		`{"kind":"deadline","title":"rest omitted","due":"`+due+`"}`))

	seeded := restCreate("c", `{"kind":"deadline","title":"rest patched","due":"`+due+`","leadDays":45}`)
	var patched Fact
	patch(t, factsURL+"/"+string(seeded.ID), `{"leadDays":0}`, &patched)
	add("REST patch back to 0", patched)

	runProject(t, tb, `{"command":"add_fact","project":"Document Expirations","kind":"deadline",`+
		`"title":"tool zero","due":"`+due+`","lead_days":"0"}`)
	add("tool add_fact with lead_days 0", factNamedInTest(t, h.store, p, "tool zero"))

	runProject(t, tb, `{"command":"add_fact","project":"Document Expirations","kind":"deadline",`+
		`"title":"tool patched","due":"`+due+`","lead_days":"45"}`)
	runProject(t, tb, `{"command":"update_fact","project":"Document Expirations",`+
		`"title":"tool patched","lead_days":"0"}`)
	add("tool update_fact to 0", factNamedInTest(t, h.store, p, "tool patched"))

	storeMade, err := h.store.CreateFact(p.ID,
		Fact{Kind: FactDeadline, Title: "store zero", Due: time.Now().UTC().AddDate(1, 0, 0)}, userAuthor)
	if err != nil {
		t.Fatalf("store create: %v", err)
	}
	add("store CreateFact with 0", storeMade)

	for _, c := range got {
		if c.fact.LeadDays != 0 {
			t.Errorf("%s: stored leadDays = %d, want 0 — no route may bake a resolved lead into the row",
				c.route, c.fact.LeadDays)
		}
		if c.fact.EffectiveLeadDays != 180 {
			t.Errorf("%s: effectiveLeadDays = %d, want the project's 180",
				c.route, c.fact.EffectiveLeadDays)
		}
	}
	// And the facts are indistinguishable afterwards, which is the actual
	// property: how a lead of 0 got there must not survive in the data.
	for _, c := range got[1:] {
		if c.fact.LeadDays != got[0].fact.LeadDays || c.fact.EffectiveLeadDays != got[0].fact.EffectiveLeadDays {
			t.Errorf("%s left %d/%d but %s left %d/%d; the route must not be readable from the result",
				c.route, c.fact.LeadDays, c.fact.EffectiveLeadDays,
				got[0].route, got[0].fact.LeadDays, got[0].fact.EffectiveLeadDays)
		}
	}
}
