package botnet

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// Hierarchy and severity, at the same four levels the flat projects service is
// tested at: the parent pointer's storage and its three refusals, the ONE
// derivation pass that rolls health up a subtree, the severity band on the
// wire, and the tool surface a bot drives it through.

// ── Fixtures ──────────────────────────────────────────────────────────────────

// mustChild creates a project under a parent, which is the whole point of the
// rock — a fixture rather than four lines per case.
func mustChild(t *testing.T, s *Store, name string, parent ProjectID) Project {
	t.Helper()
	p, err := s.CreateProject(Project{Name: name, ParentID: parent}, userAuthor)
	if err != nil {
		t.Fatalf("create child %q: %v", name, err)
	}
	return p
}

// byName pulls one project out of a listing, so an assertion names a project
// rather than an index into a sort that is itself under test.
func byName(t *testing.T, projects []Project, name string) Project {
	t.Helper()
	for _, p := range projects {
		if p.Name == name {
			return p
		}
	}
	t.Fatalf("listing has no project named %q: %+v", name, projects)
	return Project{}
}

// ── Unit 1: the parent pointer ────────────────────────────────────────────────

// TestProjectParentRoundTrip: parent_id is stored, survives a close/reopen, is
// cleared by an explicit empty patch, and childCount counts DIRECT children
// only.
func TestProjectParentRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hierarchy.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	root := mustProject(t, s, "Document Expirations", "keep every document valid")
	passport := mustChild(t, s, "Passport", root.ID)
	visa := mustChild(t, s, "China Q2 Visa", root.ID)
	stamp := mustChild(t, s, "Biometrics appointment", passport.ID)

	if root.ParentID != "" {
		t.Errorf("a top-level project's parentId = %q, want empty", root.ParentID)
	}
	if passport.ParentID != root.ID {
		t.Errorf("child parentId = %q, want %q", passport.ParentID, root.ID)
	}

	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if s, err = Open(path); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()

	listed, err := s.ListProjects()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listed) != 4 {
		t.Fatalf("listed %d projects, want the 4 written", len(listed))
	}
	if got := byName(t, listed, "Passport"); got.ParentID != root.ID {
		t.Errorf("parentId after reopen = %q, want %q — the column did not survive", got.ParentID, root.ID)
	}
	// childCount is DIRECT children: the root has two, not three.
	for _, c := range []struct {
		name string
		want int
	}{{"Document Expirations", 2}, {"Passport", 1}, {"China Q2 Visa", 0}, {"Biometrics appointment", 0}} {
		if got := byName(t, listed, c.name).ChildCount; got != c.want {
			t.Errorf("%s childCount = %d, want %d (direct children only)", c.name, got, c.want)
		}
	}

	// A patch to the empty id promotes a project back to the top level; the
	// pointer field is what makes "clear it" distinguishable from "leave it".
	none := ProjectID("")
	promoted, err := s.UpdateProject(stamp.ID, ProjectPatch{ParentID: &none})
	if err != nil {
		t.Fatalf("clear parent: %v", err)
	}
	if promoted.ParentID != "" {
		t.Errorf("parentId after an empty patch = %q, want it cleared", promoted.ParentID)
	}
	// A patch that names no parent leaves the one it has alone.
	goal := "renew before the trip"
	kept, err := s.UpdateProject(visa.ID, ProjectPatch{Goal: &goal})
	if err != nil {
		t.Fatalf("patch goal: %v", err)
	}
	if kept.ParentID != root.ID {
		t.Errorf("a goal-only patch moved the project to %q, want it left under %q", kept.ParentID, root.ID)
	}
	// And a patch that names one moves it.
	moved, err := s.UpdateProject(visa.ID, ProjectPatch{ParentID: &passport.ID})
	if err != nil {
		t.Fatalf("reparent: %v", err)
	}
	if moved.ParentID != passport.ID {
		t.Errorf("parentId after a reparent = %q, want %q", moved.ParentID, passport.ID)
	}
}

// TestProjectParentValidation: the three refusals at the write boundary. A
// parent that does not exist is a 404's ErrNotFound; a project under itself or
// under its own descendant is ErrInvalid, because a loop has no root and would
// render nowhere at all.
func TestProjectParentValidation(t *testing.T) {
	s := newEventStore(t)
	root := mustProject(t, s, "Document Expirations", "")
	child := mustChild(t, s, "Passport", root.ID)
	grand := mustChild(t, s, "Biometrics", child.ID)

	if _, err := s.CreateProject(Project{Name: "Orphan", ParentID: "prj_NOPE"}, userAuthor); !errors.Is(err, ErrNotFound) {
		t.Errorf("create under a missing parent = %v, want ErrNotFound", err)
	}
	missing := ProjectID("prj_NOPE")
	if _, err := s.UpdateProject(child.ID, ProjectPatch{ParentID: &missing}); !errors.Is(err, ErrNotFound) {
		t.Errorf("patch to a missing parent = %v, want ErrNotFound", err)
	}
	if _, err := s.UpdateProject(child.ID, ProjectPatch{ParentID: &child.ID}); !errors.Is(err, ErrInvalid) {
		t.Errorf("a project under itself = %v, want ErrInvalid", err)
	}
	// The direct loop: the root under its own child.
	err := func() error {
		_, err := s.UpdateProject(root.ID, ProjectPatch{ParentID: &child.ID})
		return err
	}()
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("root under its own child = %v, want ErrInvalid", err)
	}
	for _, want := range []string{"Document Expirations", "Passport", "cycle"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("cycle error = %q, want it to name %q", err, want)
		}
	}
	// The loop two levels up, which a self/parent check alone would miss.
	if _, err := s.UpdateProject(root.ID, ProjectPatch{ParentID: &grand.ID}); !errors.Is(err, ErrInvalid) {
		t.Errorf("root under its own grandchild = %v, want ErrInvalid", err)
	}
	// The refusals left nothing half-applied.
	listed, err := s.ListProjects()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if got := byName(t, listed, "Document Expirations"); got.ParentID != "" {
		t.Errorf("a refused reparent stuck: root parentId = %q", got.ParentID)
	}
	// Moving a SUBTREE somewhere legal is not a cycle: the child, carrying its
	// own child, becomes a sibling of the root.
	none := ProjectID("")
	if _, err := s.UpdateProject(child.ID, ProjectPatch{ParentID: &none}); err != nil {
		t.Errorf("promoting a subtree to the top level: %v", err)
	}
}

// TestDeleteProjectCascadesTheSubtree: deleting a parent deletes every
// descendant, their facts and their projected events, as per-row deletes — so a
// sync client gets a real tombstone for each and never keeps a fact whose
// project is gone.
func TestDeleteProjectCascadesTheSubtree(t *testing.T) {
	s := newEventStore(t)
	root := mustProject(t, s, "Document Expirations", "")
	passport := mustChild(t, s, "Passport", root.ID)
	visa := mustChild(t, s, "China Q2 Visa", root.ID)
	deep := mustChild(t, s, "Biometrics", passport.ID)
	// A project OUTSIDE the subtree, to prove the cascade is bounded.
	other := mustProject(t, s, "Singapore Co", "")

	var facts []Fact
	for _, p := range []Project{root, passport, visa, deep, other} {
		facts = append(facts, mustFact(t, s, p.ID, Fact{Kind: FactDeadline,
			Title: p.Name + " expires", Due: time.Now().UTC().AddDate(1, 0, 0), LeadDays: 30}, userAuthor))
	}

	state, err := s.State()
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	if err := s.DeleteProject(root.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	listed, err := s.ListProjects()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listed) != 1 || listed[0].ID != other.ID {
		t.Fatalf("after the cascade the store holds %+v, want only the outside project", listed)
	}

	changes, err := s.ChangesSince(state, 100)
	if err != nil {
		t.Fatalf("changes: %v", err)
	}
	for _, p := range []Project{root, passport, visa, deep} {
		if !slices.Contains(changes.Changed.Projects.Destroyed, string(p.ID)) {
			t.Errorf("no tombstone for project %s (%s): %v", p.ID, p.Name, changes.Changed.Projects.Destroyed)
		}
	}
	if slices.Contains(changes.Changed.Projects.Destroyed, string(other.ID)) {
		t.Errorf("the cascade reached outside the subtree and destroyed %s", other.ID)
	}
	for _, f := range facts[:4] {
		if !slices.Contains(changes.Changed.Facts.Destroyed, string(f.ID)) {
			t.Errorf("no tombstone for fact %s (%s)", f.ID, f.Title)
		}
		if !slices.Contains(changes.Changed.Events.Destroyed, string(f.EventID)) {
			t.Errorf("no tombstone for fact %s's projected event %s", f.ID, f.EventID)
		}
	}
	if slices.Contains(changes.Changed.Facts.Destroyed, string(facts[4].ID)) {
		t.Error("the cascade destroyed a fact of a project outside the subtree")
	}
}

// seedPreHierarchyDB writes the projects and facts tables EXACTLY as they
// shipped before parent_id existed, with rows in them. Opening it is the only
// honest test of the guarded addColumn: a fresh database would have the column
// from the CREATE TABLE and prove nothing.
func seedPreHierarchyDB(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("seed open: %v", err)
	}
	defer db.Close()
	const legacy = `
CREATE TABLE projects (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL,
    goal       TEXT NOT NULL DEFAULT '',
    created_by TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE UNIQUE INDEX idx_projects_name ON projects(name COLLATE NOCASE);
CREATE TABLE facts (
    id         TEXT PRIMARY KEY,
    project_id TEXT NOT NULL REFERENCES projects(id),
    kind       TEXT NOT NULL,
    title      TEXT NOT NULL,
    due        TEXT NOT NULL DEFAULT '',
    lead_days  INTEGER NOT NULL DEFAULT 0,
    rrule      TEXT NOT NULL DEFAULT '',
    tz         TEXT NOT NULL DEFAULT '',
    done       INTEGER NOT NULL DEFAULT 0,
    blocker    TEXT NOT NULL DEFAULT '',
    body       TEXT NOT NULL DEFAULT '',
    event_id   TEXT NOT NULL DEFAULT '',
    created_by TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
INSERT INTO projects VALUES ('prj_OLD1', 'Passports', 'keep them valid', 'user',
    '2026-01-01T10:00:00Z', '2026-01-01T10:00:00Z');
INSERT INTO projects VALUES ('prj_OLD2', 'Singapore Co', '', 'bot_ADA',
    '2026-01-02T10:00:00Z', '2026-01-02T10:00:00Z');
INSERT INTO facts VALUES ('fct_OLD1', 'prj_OLD1', 'deadline', 'US passport expires',
    '2027-03-14T00:00:00Z', 180, '', '', 0, '', '', '', 'user',
    '2026-01-01T10:00:00Z', '2026-01-01T10:00:00Z');
`
	if _, err := db.Exec(legacy); err != nil {
		t.Fatalf("seed pre-hierarchy schema: %v", err)
	}
}

// TestPreHierarchyDatabaseOpensUnchanged: a database written before parent_id
// existed opens, migrates, and lists exactly what it held — every project at
// the top level, with the derived fields still derived.
func TestPreHierarchyDatabaseOpensUnchanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pre-hierarchy.db")
	seedPreHierarchyDB(t, path)
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open a pre-hierarchy database: %v", err)
	}
	defer s.Close()

	listed, err := s.ListProjects()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listed) != 2 {
		t.Fatalf("listed %d projects, want the 2 already stored", len(listed))
	}
	for _, p := range listed {
		if p.ParentID != "" {
			t.Errorf("%s gained the parent %q on migration, want every legacy row at the top level", p.Name, p.ParentID)
		}
		if p.ChildCount != 0 {
			t.Errorf("%s childCount = %d, want 0", p.Name, p.ChildCount)
		}
	}
	passports := byName(t, listed, "Passports")
	if passports.Goal != "keep them valid" || passports.FactCount != 1 {
		t.Errorf("legacy project = %+v, want its goal and its one fact", passports)
	}
	if passports.Health != HealthOK || passports.NextDue == nil {
		t.Errorf("legacy project health = %q / %v, want it derived from the stored fact", passports.Health, passports.NextDue)
	}
	// Re-running the migration is a no-op, and the new column is writable.
	child := mustChild(t, s, "China Q2 Visa", passports.ID)
	if child.ParentID != passports.ID {
		t.Errorf("a child of a legacy project = %+v, want it parented", child)
	}
}

// ── Unit 2: the rollup ────────────────────────────────────────────────────────

// TestRollupOverTheSubtree: a parent's health, severity and nextDue answer for
// its WHOLE subtree — the sidebar's dot has to mean "something under here needs
// me", or a parent of four quiet children hides the one that is overdue.
// factCount stays OWN facts, because it counts what is written here.
func TestRollupOverTheSubtree(t *testing.T) {
	s := newEventStore(t)
	now := time.Now().UTC()
	root := mustProject(t, s, "Document Expirations", "")
	passport := mustChild(t, s, "Passport", root.ID)
	visa := mustChild(t, s, "China Q2 Visa", root.ID)
	licence := mustChild(t, s, "Driver's License", root.ID)

	// One S2, one S1, one S0 — the tree the rock's live proof builds.
	far := now.AddDate(0, 6, 0)
	soon := now.AddDate(0, 0, 20)
	past := now.AddDate(0, 0, -1)
	mustFact(t, s, passport.ID, Fact{Kind: FactDeadline, Title: "passport expires", Due: far, LeadDays: 30}, userAuthor)
	mustFact(t, s, visa.ID, Fact{Kind: FactDeadline, Title: "visa expires", Due: soon, LeadDays: 90}, userAuthor)
	mustFact(t, s, licence.ID, Fact{Kind: FactDeadline, Title: "licence expires", Due: past, LeadDays: 30}, userAuthor)

	listed, err := s.ListProjects()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, c := range []struct {
		name     string
		health   ProjectHealth
		severity Severity
	}{
		{"Document Expirations", HealthOverdue, SeverityNow},
		{"Driver's License", HealthOverdue, SeverityNow},
		{"China Q2 Visa", HealthDueSoon, SeverityShould},
		{"Passport", HealthOK, SeverityTracked},
	} {
		got := byName(t, listed, c.name)
		if got.Health != c.health || got.Severity != c.severity {
			t.Errorf("%s = %q/%q, want %q/%q", c.name, got.Health, got.Severity, c.health, c.severity)
		}
	}
	// The parent holds no facts of its own, and says so, while still reporting
	// the worst thing under it.
	parent := byName(t, listed, "Document Expirations")
	if parent.FactCount != 0 {
		t.Errorf("parent factCount = %d, want 0 — factCount is OWN facts", parent.FactCount)
	}
	if parent.NextDue == nil || !parent.NextDue.Equal(past.Truncate(time.Second)) {
		t.Errorf("parent nextDue = %v, want the nearest outstanding date in the subtree (%v)", parent.NextDue, past)
	}

	// The worst child settled, the parent falls back to the next-worst — the
	// rollup is derived every time, not remembered.
	licenceFacts, err := s.ListFacts(licence.ID)
	if err != nil {
		t.Fatalf("list facts: %v", err)
	}
	done := true
	if _, err := s.UpdateFact(licenceFacts[0].ID, FactPatch{Done: &done}); err != nil {
		t.Fatalf("mark done: %v", err)
	}
	listed, err = s.ListProjects()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	parent = byName(t, listed, "Document Expirations")
	if parent.Health != HealthDueSoon || parent.Severity != SeverityShould {
		t.Errorf("parent after the overdue child settled = %q/%q, want due_soon/S1", parent.Health, parent.Severity)
	}
	if parent.NextDue == nil || !parent.NextDue.Equal(soon.Truncate(time.Second)) {
		t.Errorf("parent nextDue = %v, want the visa's date %v", parent.NextDue, soon)
	}

	// A grandchild rolls the whole way up, not just one level.
	deep := mustChild(t, s, "Biometrics", passport.ID)
	mustFact(t, s, deep.ID, Fact{Kind: FactMilestone, Title: "book it", Blocker: "the consulate must call back"}, userAuthor)
	listed, err = s.ListProjects()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if got := byName(t, listed, "Passport"); got.Health != HealthBlocked {
		t.Errorf("Passport (parent of the blocked grandchild) = %q, want blocked", got.Health)
	}
	if got := byName(t, listed, "Document Expirations"); got.Health != HealthBlocked {
		t.Errorf("root after a blocked grandchild = %q, want blocked — the rollup stops one level short", got.Health)
	}
	// The grandchild's own health is unaffected by anything above it.
	if got := byName(t, listed, "Biometrics"); got.Health != HealthBlocked || got.ChildCount != 0 {
		t.Errorf("the grandchild itself = %+v, want its own blocked health", got)
	}
}

// TestRollupIsOnePass: the list is derived from two reads whatever the tree's
// shape, so a net of many projects does not cost a query per project. The
// assertion is the derived answer over a wide, deep tree; the guarantee is that
// ListProjects reads projects once and facts once.
func TestRollupIsOnePass(t *testing.T) {
	s := newEventStore(t)
	root := mustProject(t, s, "Root", "")
	parent := root.ID
	// A 12-deep chain with the only dated fact at the bottom.
	var deepest ProjectID
	for i := 0; i < 12; i++ {
		p := mustChild(t, s, "level "+string(rune('a'+i)), parent)
		parent, deepest = p.ID, p.ID
	}
	mustFact(t, s, deepest, Fact{Kind: FactDeadline, Title: "the one date",
		Due: time.Now().UTC().AddDate(0, 0, -2), LeadDays: 30}, userAuthor)

	listed, err := s.ListProjects()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listed) != 13 {
		t.Fatalf("listed %d projects, want 13", len(listed))
	}
	for _, p := range listed {
		if p.Health != HealthOverdue {
			t.Errorf("%s = %q, want overdue — every ancestor of an overdue leaf is overdue", p.Name, p.Health)
		}
	}
	// The most urgent thing sorts first even though it is the deepest row.
	if listed[0].Severity != SeverityNow {
		t.Errorf("the listing leads with %q, want an S0 row", listed[0].Severity)
	}
}

// TestListSortsByRolledUpUrgency: the flat array the client trees itself is
// ordered by the ROLLED-UP severity, then the nearest date, then the name.
func TestListSortsByRolledUpUrgency(t *testing.T) {
	s := newEventStore(t)
	now := time.Now().UTC()
	quiet := mustProject(t, s, "Aardvark", "") // sorts first by name, last by urgency
	loud := mustProject(t, s, "Zebra", "")
	child := mustChild(t, s, "Zebra's overdue child", loud.ID)
	mustFact(t, s, quiet.ID, Fact{Kind: FactNote, Title: "n", Body: "nothing dated"}, userAuthor)
	mustFact(t, s, child.ID, Fact{Kind: FactDeadline, Title: "late", Due: now.AddDate(0, 0, -4), LeadDays: 30}, userAuthor)

	listed, err := s.ListProjects()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	want := []string{"Zebra", "Zebra's overdue child", "Aardvark"}
	var got []string
	for _, p := range listed {
		got = append(got, p.Name)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("list order = %v, want %v — a parent must sort on what its subtree says", got, want)
	}
}

// ── Unit 3: severity on the wire ──────────────────────────────────────────────

// TestSeverityTable pins the band every health falls in, in one place.
func TestSeverityTable(t *testing.T) {
	for health, want := range map[ProjectHealth]Severity{
		HealthOverdue: "S0",
		HealthBlocked: "S1",
		HealthDueSoon: "S1",
		HealthOK:      "S2",
		HealthUnknown: "S2",
	} {
		if got := severityFor(health); got != want {
			t.Errorf("severityFor(%q) = %q, want %q", health, got, want)
		}
	}
	// A health this version does not know renders as the quiet band rather than
	// as an empty string the client has no colour for.
	if got := severityFor(ProjectHealth("on_fire")); got != SeverityTracked {
		t.Errorf("severityFor(an unknown health) = %q, want %q", got, SeverityTracked)
	}
}

// TestSeverityAndHierarchyOnTheWire: severity, parentId and childCount are on
// every row the client decodes, and the detail carries its direct children.
func TestSeverityAndHierarchyOnTheWire(t *testing.T) {
	h := newHarness(t, &fakeLLM{})
	root := postProject(t, h, `{"name":"Document Expirations","goal":"keep every document valid"}`)
	child := postProject(t, h, `{"name":"Driver's License","parentId":"`+string(root.ID)+`"}`)
	if child.ParentID != root.ID {
		t.Fatalf("POST with parentId = %+v, want it parented", child)
	}
	var fact Fact
	postExpect(t, http.StatusCreated, h.ts.URL+"/v1/projects/"+string(child.ID)+"/facts",
		`{"kind":"deadline","title":"licence expires","due":"2020-01-01T00:00:00Z","leadDays":30}`, &fact)

	raw := rawGet(t, h.ts.URL+"/v1/projects")
	if !strings.Contains(raw, `"severity":"S0"`) {
		t.Errorf("list body %s\n  carries no S0 severity", raw)
	}
	if !strings.Contains(raw, `"parentId":"`+string(root.ID)+`"`) {
		t.Errorf("list body %s\n  carries no parentId", raw)
	}
	if !strings.Contains(raw, `"childCount":1`) {
		t.Errorf("list body %s\n  carries no childCount", raw)
	}

	var listed []Project
	get(t, h.ts.URL+"/v1/projects", &listed)
	if got := byName(t, listed, "Document Expirations"); got.Severity != SeverityNow || got.Health != HealthOverdue {
		t.Errorf("the parent = %q/%q, want the child's overdue rolled up", got.Severity, got.Health)
	}

	// The detail carries the direct children, hydrated and in the list's order.
	var detail struct {
		Project  Project   `json:"project"`
		Facts    []Fact    `json:"facts"`
		Children []Project `json:"children"`
	}
	get(t, h.ts.URL+"/v1/projects/"+string(root.ID), &detail)
	if len(detail.Children) != 1 || detail.Children[0].ID != child.ID {
		t.Fatalf("detail children = %+v, want the one child", detail.Children)
	}
	if detail.Children[0].Severity != SeverityNow || detail.Children[0].FactCount != 1 {
		t.Errorf("a detail child = %+v, want it hydrated like a list row", detail.Children[0])
	}
	// A childless project answers with an empty array, never null: the client
	// has no nil case, exactly as the change feed's buckets do not.
	childRaw := rawGet(t, h.ts.URL+"/v1/projects/"+string(child.ID))
	if !strings.Contains(childRaw, `"children":[]`) {
		t.Errorf("a childless detail = %s, want an empty children array", childRaw)
	}
}

// TestProjectParentIDOverTheWire: the REST boundary maps the store's three
// refusals onto the codes the app branches on.
func TestProjectParentIDOverTheWire(t *testing.T) {
	h := newHarness(t, &fakeLLM{})
	root := postProject(t, h, `{"name":"Document Expirations"}`)
	child := postProject(t, h, `{"name":"Passport","parentId":"`+string(root.ID)+`"}`)

	if code, body := postRaw(t, h.ts.URL+"/v1/projects", `{"name":"Nowhere","parentId":"prj_NOPE"}`); code != http.StatusNotFound {
		t.Errorf("POST under a missing parent = %d (%s), want 404", code, body)
	}
	if code, body := patchRaw(t, h.ts.URL+"/v1/projects/"+string(root.ID),
		`{"parentId":"`+string(child.ID)+`"}`); code != http.StatusBadRequest || !strings.Contains(body, "cycle") {
		t.Errorf("PATCH creating a cycle = %d (%s), want 400 naming the cycle", code, body)
	}
	if code, body := patchRaw(t, h.ts.URL+"/v1/projects/"+string(child.ID),
		`{"parentId":"`+string(child.ID)+`"}`); code != http.StatusBadRequest {
		t.Errorf("PATCH parenting a project to itself = %d (%s), want 400", code, body)
	}
	// The documented clear.
	var cleared Project
	patch(t, h.ts.URL+"/v1/projects/"+string(child.ID), `{"parentId":""}`, &cleared)
	if cleared.ParentID != "" {
		t.Errorf("PATCH parentId:\"\" = %+v, want the parent cleared", cleared)
	}
}

// ── Unit 4: the tool ──────────────────────────────────────────────────────────

// TestProjectToolHierarchy: create takes a parent by NAME, the new update
// command moves and renames a project, and both refuse the same things the
// store does — as instructive errors, never a failed turn.
func TestProjectToolHierarchy(t *testing.T) {
	s := newEventStore(t)
	bot := newBot(t, s)
	tb := NewBotToolbox(s, bot.ID, nil)

	runProject(t, tb, `{"command":"create","project":"Document Expirations","goal":"keep every document valid"}`)
	if got := runProject(t, tb, `{"command":"create","project":"Passport","parent":"document expirations"}`); strings.HasPrefix(got, "error:") {
		t.Fatalf("create with a parent = %q, want it to succeed", got)
	}
	root, err := s.ProjectByName("Document Expirations")
	if err != nil {
		t.Fatalf("by name: %v", err)
	}
	passport, err := s.ProjectByName("Passport")
	if err != nil {
		t.Fatalf("by name: %v", err)
	}
	if passport.ParentID != root.ID {
		t.Errorf("tool-created child parentId = %q, want %q", passport.ParentID, root.ID)
	}
	if got := runProject(t, tb, `{"command":"create","project":"Nowhere","parent":"Atlantis"}`); !strings.HasPrefix(got, "error:") ||
		!strings.Contains(got, "Atlantis") {
		t.Errorf("create under an unknown parent = %q, want an instructive error naming it", got)
	}

	// update renames, re-goals and re-parents.
	if got := runProject(t, tb, `{"command":"update","project":"Passport","new_name":"US Passport","goal":"renew in time"}`); strings.HasPrefix(got, "error:") {
		t.Fatalf("update = %q, want it to succeed", got)
	}
	renamed, err := s.ProjectByName("US Passport")
	if err != nil {
		t.Fatalf("by name after rename: %v", err)
	}
	if renamed.Goal != "renew in time" || renamed.ParentID != root.ID {
		t.Errorf("after update = %+v, want the new name and goal with the parent left alone", renamed)
	}
	// "none" is the documented clear.
	if got := runProject(t, tb, `{"command":"update","project":"US Passport","parent":"none"}`); strings.HasPrefix(got, "error:") {
		t.Fatalf("clearing a parent = %q, want it to succeed", got)
	}
	if promoted, _ := s.ProjectByName("US Passport"); promoted.ParentID != "" {
		t.Errorf("after parent:none the project sits under %q, want the top level", promoted.ParentID)
	}
	// And back under, so the cycle case below has a tree to close.
	runProject(t, tb, `{"command":"update","project":"US Passport","parent":"Document Expirations"}`)
	if got := runProject(t, tb, `{"command":"update","project":"Document Expirations","parent":"US Passport"}`); !strings.HasPrefix(got, "error:") ||
		!strings.Contains(got, "cycle") {
		t.Errorf("a cycle through the tool = %q, want an instructive refusal naming the cycle", got)
	}
	if got := runProject(t, tb, `{"command":"update","project":"US Passport"}`); !strings.HasPrefix(got, "error:") ||
		!strings.Contains(got, "new_name") {
		t.Errorf("an empty update = %q, want it to name the fields it could change", got)
	}
	if got := runProject(t, tb, `{"command":"update","project":"US Passport","new_name":"Document Expirations"}`); !strings.HasPrefix(got, "error:") ||
		!strings.Contains(got, "already exists") {
		t.Errorf("renaming onto a taken name = %q, want the duplicate refusal", got)
	}
	// update is not a delete in disguise: the project is still there.
	if _, err := s.ProjectByName("US Passport"); err != nil {
		t.Errorf("the project vanished during the refusals: %v", err)
	}
}

// TestProjectToolRendersTheTree: list indents children under their parents and
// leads every line with the severity, and show names a project's sub-projects
// before its facts — the two places a model learns the shape exists.
func TestProjectToolRendersTheTree(t *testing.T) {
	s := newEventStore(t)
	bot := newBot(t, s)
	tb := NewBotToolbox(s, bot.ID, nil)
	now := time.Now().UTC()

	root := mustProject(t, s, "Document Expirations", "keep every document valid")
	licence := mustChild(t, s, "Driver's License", root.ID)
	deep := mustChild(t, s, "Biometrics", licence.ID)
	mustProject(t, s, "Singapore Co", "")
	mustFact(t, s, licence.ID, Fact{Kind: FactDeadline, Title: "licence expires",
		Due: now.AddDate(0, 0, -1), LeadDays: 30}, userAuthor)
	mustFact(t, s, deep.ID, Fact{Kind: FactMilestone, Title: "book the appointment"}, userAuthor)

	list := runProject(t, tb, `{"command":"list"}`)
	lines := strings.Split(list, "\n")
	var tree []string
	for _, l := range lines {
		if strings.Contains(l, "—") && !strings.HasPrefix(l, "now:") {
			tree = append(tree, l)
		}
	}
	if len(tree) != 4 {
		t.Fatalf("list rendered %d project lines, want 4:\n%s", len(tree), list)
	}
	// Children sit under their parent, two spaces per level.
	wantIndent := map[string]string{
		"Document Expirations": "",
		"Driver's License":     "  ",
		"Biometrics":           "    ",
		"Singapore Co":         "",
	}
	seen := map[string]bool{}
	for _, l := range tree {
		for name, indent := range wantIndent {
			if !strings.Contains(l, name) {
				continue
			}
			seen[name] = true
			if !strings.HasPrefix(l, indent+"- ") {
				t.Errorf("line %q for %s, want it indented by %d spaces", l, name, len(indent))
			}
			if !strings.Contains(l, string(SeverityNow)) && !strings.Contains(l, string(SeverityShould)) &&
				!strings.Contains(l, string(SeverityTracked)) {
				t.Errorf("line %q carries no severity", l)
			}
		}
	}
	if len(seen) != 4 {
		t.Errorf("list %q did not name every project (saw %v)", list, seen)
	}
	// The parent's line leads with the severity its subtree earns.
	for _, l := range tree {
		if strings.Contains(l, "Document Expirations") && !strings.Contains(l, "S0") {
			t.Errorf("the parent's line = %q, want the rolled-up S0", l)
		}
	}

	show := runProject(t, tb, `{"command":"show","project":"Document Expirations"}`)
	if !strings.Contains(show, "Sub-projects:") {
		t.Errorf("show on a parent = %q, want a Sub-projects block", show)
	}
	if !strings.Contains(show, "Driver's License") || !strings.Contains(show, "S0") {
		t.Errorf("show = %q, want the child named with its severity", show)
	}
	// Only DIRECT children: the grandchild belongs on its parent's page.
	if strings.Contains(show, "Biometrics") {
		t.Errorf("show = %q, want direct children only", show)
	}
	if leaf := runProject(t, tb, `{"command":"show","project":"Singapore Co"}`); strings.Contains(leaf, "Sub-projects:") {
		t.Errorf("show on a childless project = %q, want no Sub-projects block", leaf)
	}
}

// TestToolHealthLinesCarrySeverity: every health line a mutating command ends
// with leads with the band, so a model reading one result knows how loud it is
// without a table of five healths.
func TestToolHealthLinesCarrySeverity(t *testing.T) {
	s := newEventStore(t)
	bot := newBot(t, s)
	tb := NewBotToolbox(s, bot.ID, nil)

	created := runProject(t, tb, `{"command":"create","project":"Passports"}`)
	if !strings.HasSuffix(created, "Passports: S2 unknown, lead 30d") {
		t.Errorf("create = %q, want it to end with a severity-led health line naming the lead", created)
	}
	due := time.Now().UTC().AddDate(0, 0, 20).Format(time.RFC3339)
	added := runProject(t, tb, `{"command":"add_fact","project":"Passports","kind":"deadline",`+
		`"title":"US passport expires","due":"`+due+`","lead_days":"30"}`)
	if !strings.Contains(lastLine(added), "Passports: S1 due_soon") {
		t.Errorf("add_fact health line = %q, want S1 before the health", lastLine(added))
	}
	updated := runProject(t, tb, `{"command":"update","project":"Passports","goal":"keep them valid"}`)
	if !strings.Contains(lastLine(updated), "Passports: S1 due_soon") {
		t.Errorf("update = %q, want it to end with the health line too", updated)
	}
}

// TestToolsEndpointCarriesTheHierarchy: the enum, the two new flat fields and
// the ladder's eighth rung all derive from the registry, so /v1/tools is where
// a drift shows up first.
func TestToolsEndpointCarriesTheHierarchy(t *testing.T) {
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
		t.Fatalf("/v1/tools served no project tool")
	}
	props, _ := def.Parameters["properties"].(map[string]any)
	for _, field := range []string{"parent", "new_name"} {
		spec, ok := props[field].(map[string]any)
		if !ok {
			t.Errorf("parameters miss the %q field", field)
			continue
		}
		if spec["type"] != "string" {
			t.Errorf("%q is %v, want a flat string", field, spec["type"])
		}
	}
	command, _ := props["command"].(map[string]any)
	raw, _ := command["enum"].([]any)
	var enum []string
	for _, v := range raw {
		enum = append(enum, v.(string))
	}
	if !slices.Contains(enum, "update") {
		t.Errorf("command enum = %v, want the update command", enum)
	}
	for _, must := range []string{"8.", "sub-project", "parent"} {
		if !strings.Contains(def.Description, must) {
			t.Errorf("tool description omits %q — step 8 is how a bot learns sub-projects exist", must)
		}
	}
}

// TestHierarchyDetailJSONShape pins the exact detail envelope the app decodes,
// so a field rename is caught here rather than in a Swift decoder.
func TestHierarchyDetailJSONShape(t *testing.T) {
	h := newHarness(t, &fakeLLM{})
	root := postProject(t, h, `{"name":"Document Expirations"}`)
	postProject(t, h, `{"name":"Passport","parentId":"`+string(root.ID)+`"}`)
	raw := rawGet(t, h.ts.URL+"/v1/projects/"+string(root.ID))
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		t.Fatalf("decode detail: %v (%s)", err, raw)
	}
	for _, key := range []string{"project", "facts", "children"} {
		if _, ok := envelope[key]; !ok {
			t.Errorf("detail envelope %s\n  has no %q key", raw, key)
		}
	}
}
