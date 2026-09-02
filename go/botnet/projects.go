package botnet

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
)

// The projects service, split out of store.go: what work is ABOUT, as against
// the automations that say how it happens. Everything a project IS lives here —
// the derived health, the per-kind rules, the two entities' storage, and the
// calendar projection — while the `projects`/`facts` tables and their change-log
// triggers stay in store.go's migrate, beside every other table's.

// ── Projects and facts ────────────────────────────────────────────────────────
// The projects service. A project is a name, a goal and its facts; everything
// the UI shows about its condition — health, the next due date, the fact count
// — is DERIVED here on read and has no column (see the Project DECISIONs in
// schema.go). Every write is an ordinary INSERT/UPDATE/DELETE, so the
// chg_project_*/chg_fact_* triggers capture it with no Go code remembering to.

// defaultLeadDays is the due_soon window a dated fact gets when its creator
// named none. Thirty days is long enough to act on a passport renewal and
// short enough that a project is not permanently amber.
const defaultLeadDays = 30

// day is the unit lead windows and recurrence lookaheads are expressed in.
const day = 24 * time.Hour

// projectedEventDuration is how long a projected fact occupies the calendar. A
// deadline is an instant, not an appointment, so the hour is purely so the
// month grid has something to draw and the expander has a non-empty interval.
const projectedEventDuration = time.Hour

// Due times store like every other date the store range-compares: fixed-width
// RFC3339 UTC to the second (the Event DECISION). The zero time stores as '' so
// "this kind carries no date" is distinguishable from a fact dated at the year
// 1 — fmtEventTime alone would write a real-looking 0001-01-01 instant.

func fmtDueTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return fmtEventTime(t)
}

func parseDueTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	return parseEventTime(s)
}

// ── Derived health ────────────────────────────────────────────────────────────

// healthRank encodes the precedence overdue > blocked > due_soon > ok >
// unknown. It is a table rather than an ordered switch so "which is worse" is
// one comparison wherever it is asked.
var healthRank = map[ProjectHealth]int{
	HealthUnknown: 0,
	HealthOK:      1,
	HealthDueSoon: 2,
	HealthBlocked: 3,
	HealthOverdue: 4,
}

// severityOf collapses the five healths into the three bands a person reads as
// colours (see Severity in schema.go). It is a table for the same reason
// healthRank is: the mapping is asked in the store, in the tool's every health
// line and in the client's palette, and one table is what keeps those three
// answers the same.
var severityOf = map[ProjectHealth]Severity{
	HealthOverdue: SeverityNow,
	HealthBlocked: SeverityShould,
	HealthDueSoon: SeverityShould,
	HealthOK:      SeverityTracked,
	HealthUnknown: SeverityTracked,
}

// severityFor is the table's only reader. A health this version does not know
// bands as tracked rather than as "": a client has no colour for an empty
// string, and an unrecognised state is by definition not one we can call
// urgent.
func severityFor(h ProjectHealth) Severity {
	if s, ok := severityOf[h]; ok {
		return s
	}
	return SeverityTracked
}

// factSignals is the per-kind health table: what ONE undone fact contributes —
// a health level, and the instant it is next due (zero for the undated kinds).
// This is the only place a kind's meaning lives, so a fifth kind is one entry
// and no read path grows a branch.
//
// DECISION (the deadline boundary is half-open at both ends): due_soon covers
// [Due-lead, Due) and overdue covers [Due, ∞), matching the half-open
// convention Fireable already uses. A strict "Due < now" would leave the
// instant of the deadline itself in neither band, so a passport reported fine
// on the exact day it expired.
//
// DECISION (a recurring fact is never overdue): its health comes from the NEXT
// occurrence, which by construction has not happened — a Singapore annual
// return filed every March is not overdue in April, it is due next March. An
// exhausted series (COUNT/UNTIL spent) contributes nothing at all.
var factSignals = map[FactKind]func(f Fact, now time.Time) (ProjectHealth, time.Time){
	FactDeadline: func(f Fact, now time.Time) (ProjectHealth, time.Time) {
		switch {
		case !f.Due.After(now):
			return HealthOverdue, f.Due
		case !f.Due.AddDate(0, 0, -f.EffectiveLeadDays).After(now):
			return HealthDueSoon, f.Due
		}
		return HealthOK, f.Due
	},
	FactRecurring: func(f Fact, now time.Time) (ProjectHealth, time.Time) {
		next, ok := nextOccurrence(f, now)
		if !ok {
			return HealthOK, time.Time{}
		}
		if !next.AddDate(0, 0, -f.EffectiveLeadDays).After(now) {
			return HealthDueSoon, next
		}
		return HealthOK, next
	},
	FactMilestone: func(f Fact, _ time.Time) (ProjectHealth, time.Time) {
		if f.Blocker != "" {
			return HealthBlocked, time.Time{}
		}
		return HealthOK, time.Time{}
	},
	FactNote: func(Fact, time.Time) (ProjectHealth, time.Time) { return HealthOK, time.Time{} },
}

// projectHealth derives a project's condition and its nearest outstanding due
// instant from its facts. It is THE precedence function — handlers call it and
// never re-decide — and it is a pure read: nothing here writes or caches.
//
// A done fact is invisible to both answers. Zero facts is unknown rather than
// ok, because a project with nothing in it is not healthy, it is unstated.
func projectHealth(facts []Fact, now time.Time) (ProjectHealth, *time.Time) {
	if len(facts) == 0 {
		return HealthUnknown, nil
	}
	worst := HealthOK
	var next time.Time
	for _, f := range facts {
		if f.Done {
			continue
		}
		signal, ok := factSignals[f.Kind]
		if !ok {
			continue // a kind this version does not know: skip, never fail
		}
		health, due := signal(f, now)
		if healthRank[health] > healthRank[worst] {
			worst = health
		}
		if !due.IsZero() && (next.IsZero() || due.Before(next)) {
			next = due
		}
	}
	if next.IsZero() {
		return worst, nil
	}
	return worst, &next
}

// recurringLookaheads are the windows nextOccurrence tries in order. Expansion
// costs O(window), so the common monthly answer is found in the first, short
// one; the long tails exist for a yearly rule with an INTERVAL.
var recurringLookaheads = []time.Duration{45 * day, 400 * day, 1500 * day, 4000 * day}

// nextOccurrence is the first occurrence of a recurring fact at or after now,
// found with the SAME expander the calendar uses — one recurrence engine, so a
// fact and its projected event can never disagree about when the next filing
// is. It reports false when the series is exhausted.
//
// An unparseable rule also reports false rather than erroring: validateFact
// keeps those out of the store, so reaching it means the row is already
// inconsistent, and a health read is not the place to fail a whole listing.
func nextOccurrence(f Fact, now time.Time) (time.Time, bool) {
	ev := Event{StartsAt: f.Due, EndsAt: f.Due.Add(projectedEventDuration), RRule: f.RRule, TZ: f.TZ}
	for _, ahead := range recurringLookaheads {
		instances, err := expandEvent(ev, now, now.Add(ahead))
		if err != nil {
			return time.Time{}, false
		}
		for _, in := range instances {
			// expandEvent's window keeps an occurrence already running; the
			// NEXT one is the first that has not started. It comes back
			// anchored in the fact's own zone — UTC here, so nextDue
			// serializes as "…Z" like every other instant on the wire.
			if !in.StartsAt.Before(now) {
				return in.StartsAt.UTC(), true
			}
		}
	}
	return time.Time{}, false
}

// ── Validation ────────────────────────────────────────────────────────────────

// factRules is the per-kind table of which optional fields a kind REQUIRES and
// which it merely ALLOWS. Anything set but not allowed is rejected, so an
// illegal state — a note with a recurrence, a recurring obligation someone can
// tick off once and forget — is never stored, and never silently coerced.
// `conflicts` is the third kind of rule: two fields that are each legal alone
// but contradict each other together. "waiting on the lawyer" and "finished"
// are two claims about the same step, and storing both would render a project
// that says one thing in the health dot and the opposite in the row.
var factRules = map[FactKind]struct {
	required, allowed []string
	conflicts         [][2]string
}{
	FactDeadline:  {required: []string{"due"}, allowed: []string{"due", "done"}},
	FactRecurring: {required: []string{"due", "rrule", "tz"}, allowed: []string{"due", "rrule", "tz"}},
	FactMilestone: {allowed: []string{"done", "blocker"}, conflicts: [][2]string{{"blocker", "done"}}},
	FactNote:      {},
}

// factOptionalFields names the fields factRules governs, each with its "was it
// supplied" test — what lets the rules above be two lists of names rather than
// a per-kind branch. title, body and leadDays are common to every kind and are
// checked separately.
var factOptionalFields = []struct {
	name string
	set  func(Fact) bool
}{
	{"due", func(f Fact) bool { return !f.Due.IsZero() }},
	{"rrule", func(f Fact) bool { return f.RRule != "" }},
	{"tz", func(f Fact) bool { return f.TZ != "" }},
	{"done", func(f Fact) bool { return f.Done }},
	{"blocker", func(f Fact) bool { return f.Blocker != "" }},
}

// factKinds lists the kinds in a stable order, for the enum and the errors.
func factKinds() []string {
	return []string{string(FactDeadline), string(FactRecurring), string(FactMilestone), string(FactNote)}
}

// validateFact is the ONE place a fact's rules live, so the REST handler and
// the bot's project tool cannot end up enforcing different ones. It runs on the
// fact as it would be STORED, so a patch is checked against the merged result
// rather than against the fields it happened to carry.
func validateFact(f Fact) error {
	rule, ok := factRules[f.Kind]
	if !ok {
		return fmt.Errorf("%w: kind %q is not one of %s", ErrInvalid, f.Kind, strings.Join(factKinds(), ", "))
	}
	if strings.TrimSpace(f.Title) == "" {
		return fmt.Errorf("%w: title must not be empty", ErrInvalid)
	}
	if len([]rune(f.Title)) > 120 {
		return fmt.Errorf("%w: title must be at most 120 characters", ErrInvalid)
	}
	if f.LeadDays < 0 {
		return fmt.Errorf("%w: leadDays must be 0 or more, not %d", ErrInvalid, f.LeadDays)
	}
	set := map[string]bool{}
	for _, field := range factOptionalFields {
		set[field.name] = field.set(f)
		if set[field.name] && !slices.Contains(rule.allowed, field.name) {
			return fmt.Errorf("%w: %s is not allowed on a %s fact", ErrInvalid, field.name, f.Kind)
		}
	}
	for _, need := range rule.required {
		if !set[need] {
			return fmt.Errorf("%w: a %s fact requires a %s", ErrInvalid, f.Kind, need)
		}
	}
	for _, pair := range rule.conflicts {
		if set[pair[0]] && set[pair[1]] {
			return fmt.Errorf("%w: a %s fact cannot have both %s and %s — a step waiting on someone is not "+
				"finished; clear the blocker first", ErrInvalid, f.Kind, pair[0], pair[1])
		}
	}
	if f.RRule != "" {
		if _, err := parseRRULE(f.RRule); err != nil {
			return fmt.Errorf("%w: rrule: %v", ErrInvalid, err)
		}
	}
	if f.TZ != "" {
		if _, err := time.LoadLocation(f.TZ); err != nil {
			return fmt.Errorf("%w: unknown tz %q (want an IANA id like \"Asia/Singapore\")", ErrInvalid, f.TZ)
		}
	}
	return nil
}

// validateProject holds a project's own rules, for the same reason.
func validateProject(p Project) error {
	if p.Name == "" {
		return fmt.Errorf("%w: name must not be empty", ErrInvalid)
	}
	if len([]rune(p.Name)) > 64 {
		return fmt.Errorf("%w: name must be at most 64 characters", ErrInvalid)
	}
	if p.DefaultLeadDays < 0 {
		return fmt.Errorf("%w: defaultLeadDays must be 0 or more, not %d", ErrInvalid, p.DefaultLeadDays)
	}
	return nil
}

// ── Projects ──────────────────────────────────────────────────────────────────

const projectColumns = `id, name, goal, parent_id, default_lead_days, owner_bot_id, last_health,
	created_by, created_at, updated_at`

func scanProject(sc interface{ Scan(...any) error }) (Project, error) {
	var p Project
	var createdAt, updatedAt string
	if err := sc.Scan(&p.ID, &p.Name, &p.Goal, &p.ParentID, &p.DefaultLeadDays,
		&p.OwnerBot, &p.LastHealth, &p.CreatedBy, &createdAt, &updatedAt); err != nil {
		return Project{}, err
	}
	var err error
	if p.CreatedAt, err = parseEventTime(createdAt); err != nil {
		return Project{}, fmt.Errorf("parse created_at: %w", err)
	}
	if p.UpdatedAt, err = parseEventTime(updatedAt); err != nil {
		return Project{}, fmt.Errorf("parse updated_at: %w", err)
	}
	p.Health = HealthUnknown
	p.Severity = severityFor(HealthUnknown)
	// The un-inherited answers, so a project read on its own — getProject inside
	// a write transaction — still names a usable window and its own owner.
	// loadForest overwrites both with the ancestor's the moment the tree is in
	// hand, and it is loadForest that decides whether the owner still exists.
	p.EffectiveLeadDays = leadOrGlobal(p.DefaultLeadDays)
	p.EffectiveOwner = p.OwnerBot
	return p, nil
}

// leadOrGlobal is the bottom of the inheritance chain: a project's own default
// when it set one, else the global 30. One function, so "0 means unset" is
// decided once rather than at every reader.
func leadOrGlobal(own int) int {
	if own > 0 {
		return own
	}
	return defaultLeadDays
}

// hydrate fills the derived fields from the project's OWN facts. It is the only
// caller of projectHealth, and the rollup below starts from what it computed —
// so a subtree's answer and a leaf's answer come from the same derivation, and
// there is never a second opinion about what one project's facts mean.
func hydrate(p *Project, facts []Fact, now time.Time) {
	p.FactCount = len(facts)
	// The window a fact inherits is the PROJECT's, so it can only be resolved
	// here, where both are in hand — and it is resolved in place, so the
	// forest's own copy of these facts carries the answer out to every reader.
	resolveFactLeads(facts, p.EffectiveLeadDays)
	p.Health, p.NextDue = projectHealth(facts, now)
	p.Severity = severityFor(p.Health)
}

// leadFor is the fact-level half of the "0 means unset" rule, and the ONLY
// place it is decided: the fact's own window when it set one, else the
// project's effective answer.
func leadFor(f Fact, projectLead int) int {
	if f.LeadDays > 0 {
		return f.LeadDays
	}
	return projectLead
}

// resolveFactLeads fills the derived EffectiveLeadDays across a project's
// facts. It mutates in place, so a caller holding the slice sees it too.
func resolveFactLeads(facts []Fact, projectLead int) {
	for i := range facts {
		facts[i].EffectiveLeadDays = leadFor(facts[i], projectLead)
	}
}

// ── The tree ──────────────────────────────────────────────────────────────────
// Health rolls UP, so no project can be read in isolation: a parent's condition
// is its own facts' worst plus every descendant's. That makes the whole forest
// the unit of derivation, and loading it is deliberately TWO queries — every
// project, every fact — rather than a walk that costs a query per node.

// projectForest is one derivation pass over every project: own facts hydrated,
// the subtree rolled up, severity banded, and both orderings (the flat list and
// each project's children) already sorted the way they are served.
type projectForest struct {
	byID     map[ProjectID]*Project
	children map[ProjectID][]*Project // direct children, in list order
	facts    map[ProjectID][]Fact     // own facts, in insertion order
	sorted   []Project                // every project, most urgent first
	now      time.Time                // the clock every project here was judged against
}

// liveBots is the set of bot ids that still exist. It is read once per
// derivation so a dangling owner — a hand-edited row, or a bot deleted by a
// process that predates the clearing cascade — reads as unset rather than as an
// owner whose thread nobody can open.
func liveBots(q dbtx) (map[BotID]bool, error) {
	rows, err := q.Query(`SELECT id FROM bots`)
	if err != nil {
		return nil, fmt.Errorf("read the bot roster: %w", err)
	}
	defer rows.Close()
	live := map[BotID]bool{}
	for rows.Next() {
		var id BotID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan a bot id: %w", err)
		}
		live[id] = true
	}
	return live, rows.Err()
}

// loadForest reads the projects and the facts once each and derives everything
// the wire carries. It takes a dbtx so a write path can derive inside its own
// transaction, and a `now` so a caller's clock is the one every project is
// judged against.
func loadForest(q dbtx, now time.Time) (*projectForest, error) {
	rows, err := q.Query(`SELECT ` + projectColumns + ` FROM projects`)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	defer rows.Close()
	var nodes []*Project
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, fmt.Errorf("scan project: %w", err)
		}
		nodes = append(nodes, &p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	facts, err := queryFacts(q, `ORDER BY rowid`)
	if err != nil {
		return nil, err
	}
	byProject := map[ProjectID][]Fact{}
	for _, f := range facts {
		byProject[f.ProjectID] = append(byProject[f.ProjectID], f)
	}
	live, err := liveBots(q)
	if err != nil {
		return nil, err
	}

	f := &projectForest{
		byID:     map[ProjectID]*Project{},
		children: map[ProjectID][]*Project{},
		facts:    byProject,
		now:      now,
	}
	for _, p := range nodes {
		hydrate(p, byProject[p.ID], now)
		// An owner whose bot is gone reads as no owner at all, in BOTH fields:
		// a client must never render, or a tick address, a thread that is not
		// there. DeleteBot clears the stored value to agree.
		if p.OwnerBot != "" && !live[p.OwnerBot] {
			p.OwnerBot, p.EffectiveOwner = "", ""
		}
		f.byID[p.ID] = p
	}
	// A pointer at a project that is gone makes an ORPHAN, and an orphan is a
	// root: it must render somewhere, and the alternative is a row no tree
	// contains. The write boundary keeps these out; a hand-edited database can
	// still hold one.
	var roots []*Project
	for _, p := range nodes {
		if parent, ok := f.byID[p.ParentID]; ok && parent != p {
			f.children[p.ParentID] = append(f.children[p.ParentID], p)
			continue
		}
		roots = append(roots, p)
	}
	for _, p := range nodes {
		p.ChildCount = len(f.children[p.ID])
	}
	f.rollUp(roots)

	f.sorted = make([]Project, 0, len(nodes))
	for _, p := range nodes {
		f.sorted = append(f.sorted, *p)
	}
	sortProjects(f.sorted)
	for _, kids := range f.children {
		sortProjectPtrs(kids)
	}
	return f, nil
}

// rollUp is the forest's ONE tree walk, and it carries both directions at once:
// the inherited answers (the lead threshold, the owner) flow DOWN into each
// child before the recursion, and the worst health and nearest date fold UP into
// the parent after it. Every project is visited once whatever the depth.
//
// A project not reachable from any root can only be part of a stored cycle,
// which the write boundary refuses; it keeps the un-inherited values scanProject
// and hydrate already gave it rather than hanging the read.
func (f *projectForest) rollUp(roots []*Project) {
	done := map[ProjectID]bool{}
	var walk func(p *Project, inheritedLead int, inheritedOwner BotID)
	walk = func(p *Project, inheritedLead int, inheritedOwner BotID) {
		if done[p.ID] {
			return
		}
		done[p.ID] = true
		// Own value wins; otherwise the nearest ancestor's answer, which is
		// exactly what the parent resolved a level up.
		p.EffectiveLeadDays = inheritedLead
		if p.DefaultLeadDays > 0 {
			p.EffectiveLeadDays = p.DefaultLeadDays
		}
		p.EffectiveOwner = inheritedOwner
		if p.OwnerBot != "" {
			p.EffectiveOwner = p.OwnerBot
		}
		// Only NOW is the inherited window known, and a fact's health depends
		// on it — so own-fact health is judged here rather than in the first
		// pass, before any child folds into it. The first pass's hydrate stands
		// only for a project this walk never reaches.
		hydrate(p, f.facts[p.ID], f.now)
		for _, c := range f.children[p.ID] {
			walk(c, p.EffectiveLeadDays, p.EffectiveOwner)
			if healthRank[c.Health] > healthRank[p.Health] {
				p.Health = c.Health
			}
			if c.NextDue != nil && (p.NextDue == nil || c.NextDue.Before(*p.NextDue)) {
				due := *c.NextDue
				p.NextDue = &due
			}
		}
		p.Severity = severityFor(p.Health)
	}
	for _, r := range roots {
		// A root inherits from nothing: the global lead default, and no owner.
		walk(r, defaultLeadDays, "")
	}
}

// lessProject is the ONE ordering projects are served in, and it is the answer
// to "what should I look at": the worst condition first, then the nearest date,
// then the name. Both the flat list and a detail's children use it, so the
// sidebar and the pane cannot disagree about which child is most urgent.
func lessProject(a, b Project) bool {
	if healthRank[a.Health] != healthRank[b.Health] {
		return healthRank[a.Health] > healthRank[b.Health]
	}
	switch {
	case a.NextDue != nil && b.NextDue != nil:
		if !a.NextDue.Equal(*b.NextDue) {
			return a.NextDue.Before(*b.NextDue)
		}
	case a.NextDue != nil:
		return true // something dated outranks nothing dated
	case b.NextDue != nil:
		return false
	}
	return strings.ToLower(a.Name) < strings.ToLower(b.Name)
}

func sortProjects(ps []Project) {
	sort.SliceStable(ps, func(i, j int) bool { return lessProject(ps[i], ps[j]) })
}

func sortProjectPtrs(ps []*Project) {
	sort.SliceStable(ps, func(i, j int) bool { return lessProject(*ps[i], *ps[j]) })
}

// childrenOf copies out one project's direct children, hydrated and ordered
// like the list — the "children" block of the detail route.
func (f *projectForest) childrenOf(id ProjectID) []Project {
	out := make([]Project, 0, len(f.children[id]))
	for _, c := range f.children[id] {
		out = append(out, *c)
	}
	return out
}

// forest is the read path's entry point: derive everything, once, against the
// caller's clock.
func (s *Store) forest() (*projectForest, error) {
	return loadForest(s.db, time.Now().UTC())
}

// ── Hierarchy rules ───────────────────────────────────────────────────────────

// requireParentable is the parent pointer's whole validation, run at the write
// boundary by BOTH the create and the update path so REST and the tool cannot
// enforce different shapes. Three refusals, in the order a caller hits them:
// the parent must exist (ErrNotFound → a 404, because the caller named a row
// that is not there), a project may not be its own parent, and the move must
// not close a loop. A loop is ErrInvalid rather than ErrNotFound: every row
// named exists, it is the RELATION that is impossible.
func requireParentable(q dbtx, child Project, parentID ProjectID) error {
	if parentID == "" {
		return nil
	}
	if parentID == child.ID {
		return fmt.Errorf("%w: a project cannot be its own parent", ErrInvalid)
	}
	parent, err := getProject(q, parentID)
	if errors.Is(err, ErrNotFound) {
		return fmt.Errorf("%w: no project %s to be the parent of %q", ErrNotFound, parentID, child.Name)
	}
	if err != nil {
		return err
	}
	// Walk the PARENT's ancestors. Reaching the child means the child already
	// contains its would-be parent, so the move would make a ring — and a ring
	// has no root, so neither project would appear in any tree again.
	seen := map[ProjectID]bool{parent.ID: true}
	for at := parent; at.ParentID != ""; {
		if at.ParentID == child.ID {
			return fmt.Errorf("%w: moving %q under %q would create a cycle",
				ErrInvalid, child.Name, parent.Name)
		}
		if seen[at.ParentID] {
			break // a loop that was already stored upstream: not this move's doing
		}
		seen[at.ParentID] = true
		next, err := getProject(q, at.ParentID)
		if errors.Is(err, ErrNotFound) {
			break // an orphaned pointer ends the chain, exactly as loadForest treats it
		}
		if err != nil {
			return err
		}
		at = next
	}
	return nil
}

// subtreeIDs is the project and every descendant, parents before children. It
// reads the whole (id, parent_id) column pair once rather than a query per
// level, and the seen set means a stored cycle bounds the walk instead of
// hanging a delete.
func subtreeIDs(q dbtx, root ProjectID) ([]ProjectID, error) {
	rows, err := q.Query(`SELECT id, parent_id FROM projects`)
	if err != nil {
		return nil, fmt.Errorf("read the project tree: %w", err)
	}
	defer rows.Close()
	children := map[ProjectID][]ProjectID{}
	for rows.Next() {
		var id, parent ProjectID
		if err := rows.Scan(&id, &parent); err != nil {
			return nil, fmt.Errorf("scan the project tree: %w", err)
		}
		if parent != "" && parent != id {
			children[parent] = append(children[parent], id)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := []ProjectID{root}
	seen := map[ProjectID]bool{root: true}
	for i := 0; i < len(out); i++ {
		for _, c := range children[out[i]] {
			if seen[c] {
				continue
			}
			seen[c] = true
			out = append(out, c)
		}
	}
	return out, nil
}

// placeholders renders an id list as `?, ?, ?` plus its args, so a cascade is
// one statement per table however wide the subtree — and one statement is what
// makes SQLite fire the row triggers itself, per row, with no Go code
// remembering to log anything.
func placeholders[T ~string](ids []T) (string, []any) {
	marks := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		marks[i], args[i] = "?", string(id)
	}
	return strings.Join(marks, ", "), args
}

// CreateProject stores one project and returns it as stored. It takes the
// project rather than a widening list of strings — Name, Goal and ParentID are
// the three authored fields, and CreateFact already reads this way. The id, the
// author and both timestamps are stamped here, exactly as CreateEvent stamps an
// event's; createdBy is a BotID for a tool write and "user" for a REST one.
func (s *Store) CreateProject(in Project, createdBy string) (Project, error) {
	var p Project
	err := s.tx(func(q dbtx) error {
		var err error
		p, err = createProject(q, in, createdBy)
		return err
	})
	if err != nil {
		return Project{}, err
	}
	// Re-derived after the write for the same reason UpdateProject is: the
	// inherited threshold and the rolled-up health are answers about the TREE,
	// and the row the insert produced knows nothing about where it landed.
	p, _, err = s.GetProject(p.ID)
	return p, err
}

// createProject is the one insert path. The dup-name check, the parent check
// and the insert share the caller's transaction so they cannot interleave; the
// NOCASE unique index is the structural backstop, and the check exists so a
// collision reports ErrDuplicateName rather than a raw constraint violation.
func createProject(q dbtx, in Project, createdBy string) (Project, error) {
	now := time.Now().UTC().Truncate(time.Second)
	p := Project{
		ID:              ProjectID(newID("prj_")),
		Name:            strings.TrimSpace(in.Name),
		Goal:            in.Goal,
		ParentID:        in.ParentID,
		DefaultLeadDays: in.DefaultLeadDays,
		OwnerBot:        in.OwnerBot,
		CreatedBy:       createdBy,
		CreatedAt:       now,
		UpdatedAt:       now,
		Health:          HealthUnknown,
		Severity:        severityFor(HealthUnknown),
	}
	if err := validateProject(p); err != nil {
		return Project{}, err
	}
	if existing, err := projectByName(q, p.Name); err == nil {
		return Project{}, fmt.Errorf("%w: a project named %q already exists", ErrDuplicateName, existing.Name)
	} else if !errors.Is(err, ErrNotFound) {
		return Project{}, err
	}
	// A brand-new id cannot be anyone's ancestor, so only the existence check
	// can fire here — running the same guard anyway is what keeps create and
	// update enforcing one rule rather than two that drift.
	if err := requireParentable(q, p, p.ParentID); err != nil {
		return Project{}, err
	}
	if err := requireOwner(q, p.OwnerBot); err != nil {
		return Project{}, err
	}
	if _, err := q.Exec(`INSERT INTO projects (`+projectColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ID, p.Name, p.Goal, p.ParentID, p.DefaultLeadDays, p.OwnerBot, p.LastHealth, p.CreatedBy,
		fmtEventTime(p.CreatedAt), fmtEventTime(p.UpdatedAt)); err != nil {
		return Project{}, fmt.Errorf("create project: %w", err)
	}
	return p, nil
}

// requireOwner is the owner pointer's whole validation, run at the write
// boundary by BOTH create and update for the same reason requireParentable is:
// naming an owner that does not exist would give a project a thread to be
// nudged in that nobody can read. "" is legal and means nobody owns it.
func requireOwner(q dbtx, owner BotID) error {
	if owner == "" {
		return nil
	}
	if _, err := getBot(q, owner); errors.Is(err, ErrNotFound) {
		return fmt.Errorf("%w: no bot %s to own this project", ErrNotFound, owner)
	} else if err != nil {
		return err
	}
	return nil
}

// GetProject loads one project with its facts, both hydrated: the project's
// health rolled up over its subtree, and the facts in the pane's urgency-first
// order.
func (s *Store) GetProject(id ProjectID) (Project, []Fact, error) {
	p, facts, _, err := s.GetProjectDetail(id)
	return p, facts, err
}

// GetProjectDetail is GetProject plus the project's DIRECT children, hydrated
// and ordered exactly as the listing orders them — the whole of what
// GET /v1/projects/{id} serves, from one derivation pass.
func (s *Store) GetProjectDetail(id ProjectID) (Project, []Fact, []Project, error) {
	f, err := s.forest()
	if err != nil {
		return Project{}, nil, nil, err
	}
	p, ok := f.byID[id]
	if !ok {
		return Project{}, nil, nil, ErrNotFound
	}
	facts, err := s.ListFacts(id)
	if err != nil {
		return Project{}, nil, nil, err
	}
	return *p, facts, f.childrenOf(id), nil
}

func getProject(q dbtx, id ProjectID) (Project, error) {
	p, err := scanProject(q.QueryRow(`SELECT `+projectColumns+` FROM projects WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Project{}, ErrNotFound
	}
	if err != nil {
		return Project{}, fmt.Errorf("get project: %w", err)
	}
	return p, nil
}

// ProjectByName finds a project case-insensitively — how the project tool
// resolves the name a model typed, since bots address projects by name and
// never by id.
func (s *Store) ProjectByName(name string) (Project, error) {
	row, err := projectByName(s.db, name)
	if err != nil {
		return Project{}, err
	}
	f, err := s.forest()
	if err != nil {
		return Project{}, err
	}
	p, ok := f.byID[row.ID]
	if !ok {
		return Project{}, ErrNotFound
	}
	return *p, nil
}

func projectByName(q dbtx, name string) (Project, error) {
	p, err := scanProject(q.QueryRow(
		`SELECT `+projectColumns+` FROM projects WHERE name = ? COLLATE NOCASE`, strings.TrimSpace(name)))
	if errors.Is(err, sql.ErrNoRows) {
		return Project{}, ErrNotFound
	}
	if err != nil {
		return Project{}, fmt.Errorf("project by name: %w", err)
	}
	return p, nil
}

// ListProjects returns every project as a FLAT array — the client builds the
// tree from parentId — hydrated and most urgent first: health precedence, then
// the nearest due date, then the name. The order IS the answer to "what should
// I look at", and it is the ROLLED-UP condition that orders a parent, so a
// project whose only trouble is three levels down still sorts to the top.
//
// The whole listing is two queries whatever the tree's shape: projects once,
// facts once, then one pass to derive.
func (s *Store) ListProjects() ([]Project, error) {
	f, err := s.forest()
	if err != nil {
		return nil, err
	}
	return f.sorted, nil
}

// ProjectPatch is the set of fields an update may change; a nil field is left
// alone. ParentID is a pointer for the same reason the others are: a patch to
// the empty id PROMOTES a project to the top level, which must not read the
// same as leaving its parent alone. No version to condition on — projects are
// last-write-wins, the same DECISION calendars and events carry.
type ProjectPatch struct {
	Name     *string    `json:"name"`
	Goal     *string    `json:"goal"`
	ParentID *ProjectID `json:"parentId"`

	// DefaultLeadDays and OwnerBot are pointers for the same reason ParentID is:
	// a patch to 0 / "" CLEARS the project's own value and lets the ancestor's
	// apply again, which must not read the same as leaving it alone.
	DefaultLeadDays *int   `json:"defaultLeadDays"`
	OwnerBot        *BotID `json:"ownerBot"`
}

// UpdateProject applies a partial patch and returns the project as stored,
// hydrated. The read, the dup-name check, the write and the projected events'
// retitling share one transaction: last-write-wins is about which value
// survives, not about letting a concurrent patch clobber a field it never
// named, and a rename must not leave the calendar showing the old project name.
func (s *Store) UpdateProject(id ProjectID, p ProjectPatch) (Project, error) {
	var out Project
	err := s.tx(func(q dbtx) error {
		var err error
		if out, err = getProject(q, id); err != nil {
			return err
		}
		renamed := false
		if p.Name != nil && strings.TrimSpace(*p.Name) != out.Name {
			out.Name = strings.TrimSpace(*p.Name)
			renamed = true
		}
		if p.Goal != nil {
			out.Goal = *p.Goal
		}
		if p.ParentID != nil {
			out.ParentID = *p.ParentID
		}
		if p.DefaultLeadDays != nil {
			out.DefaultLeadDays = *p.DefaultLeadDays
		}
		if p.OwnerBot != nil {
			out.OwnerBot = *p.OwnerBot
		}
		if err := validateProject(out); err != nil {
			return err
		}
		if existing, err := projectByName(q, out.Name); err == nil && existing.ID != id {
			return fmt.Errorf("%w: a project named %q already exists", ErrDuplicateName, existing.Name)
		} else if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		// Checked against the tree as it stands, BEFORE this row moves: the
		// question is whether the new parent already sits inside this project.
		if err := requireParentable(q, out, out.ParentID); err != nil {
			return err
		}
		if err := requireOwner(q, out.OwnerBot); err != nil {
			return err
		}
		out.UpdatedAt = time.Now().UTC().Truncate(time.Second)
		if _, err := q.Exec(
			`UPDATE projects SET name = ?, goal = ?, parent_id = ?, default_lead_days = ?,
			        owner_bot_id = ?, updated_at = ? WHERE id = ?`,
			out.Name, out.Goal, out.ParentID, out.DefaultLeadDays, out.OwnerBot,
			fmtEventTime(out.UpdatedAt), id); err != nil {
			return fmt.Errorf("update project: %w", err)
		}
		if renamed {
			return retitleProjectedEvents(q, out)
		}
		return nil
	})
	if err != nil {
		return Project{}, err
	}
	// Re-derived after the write, not patched in place: a reparent changes what
	// this project's subtree IS, so its health is a different question than it
	// was a statement ago.
	out, _, err = s.GetProject(id)
	return out, err
}

// DeleteProject removes a project, its WHOLE subtree, every one of their facts
// AND the calendar events those facts were projected onto, in one transaction.
//
// DECISION (delete takes the subtree): a sub-project only exists inside its
// parent, so leaving descendants behind would orphan them into top-level rows
// the user never made — the DeleteCalendar cascade, one level deeper. Each
// table is an explicit DELETE and SQLite row triggers fire once per deleted
// row, so a sync client gets a real tombstone for every project, every fact and
// every projected event, and never keeps a row whose owner is gone.
func (s *Store) DeleteProject(id ProjectID) error {
	return s.tx(func(q dbtx) error {
		if _, err := getProject(q, id); err != nil {
			return err
		}
		ids, err := subtreeIDs(q, id)
		if err != nil {
			return err
		}
		marks, args := placeholders(ids)
		if _, err := q.Exec(
			`DELETE FROM events WHERE id IN (
			     SELECT event_id FROM facts WHERE project_id IN (`+marks+`) AND event_id != '')`,
			args...); err != nil {
			return fmt.Errorf("delete projected events: %w", err)
		}
		if _, err := q.Exec(`DELETE FROM facts WHERE project_id IN (`+marks+`)`, args...); err != nil {
			return fmt.Errorf("delete project facts: %w", err)
		}
		if _, err := q.Exec(`DELETE FROM projects WHERE id IN (`+marks+`)`, args...); err != nil {
			return fmt.Errorf("delete project: %w", err)
		}
		return nil
	})
}

// ── Facts ─────────────────────────────────────────────────────────────────────

const factColumns = `id, project_id, kind, title, due, lead_days, rrule, tz, done, blocker, body, event_id, created_by, created_at, updated_at`

func scanFact(sc interface{ Scan(...any) error }) (Fact, error) {
	var f Fact
	var due, createdAt, updatedAt string
	var done int
	if err := sc.Scan(&f.ID, &f.ProjectID, &f.Kind, &f.Title, &due, &f.LeadDays, &f.RRule, &f.TZ,
		&done, &f.Blocker, &f.Body, &f.EventID, &f.CreatedBy, &createdAt, &updatedAt); err != nil {
		return Fact{}, err
	}
	f.Done = done != 0
	var err error
	if f.Due, err = parseDueTime(due); err != nil {
		return Fact{}, fmt.Errorf("parse due: %w", err)
	}
	if f.CreatedAt, err = parseEventTime(createdAt); err != nil {
		return Fact{}, fmt.Errorf("parse created_at: %w", err)
	}
	if f.UpdatedAt, err = parseEventTime(updatedAt); err != nil {
		return Fact{}, fmt.Errorf("parse updated_at: %w", err)
	}
	return f, nil
}

// CreateFact stores one fact on a project and returns it as stored. The id, the
// author and both timestamps are stamped here. The lead is stored EXACTLY as
// given, 0 included: 0 means unset, and the project's window is applied at read
// time (see the LeadDays DECISION in schema.go). Substituting it here is what
// made create and patch disagree about the same number, so this path no longer
// resolves anything. The project lookup, the projection and the insert share a
// transaction, so the project cannot be deleted out from under the fact.
//
// The returned fact carries its resolved EffectiveLeadDays, so a caller that
// writes and renders in one breath sees the same window every reader will.
func (s *Store) CreateFact(projectID ProjectID, f Fact, createdBy string) (Fact, error) {
	now := time.Now().UTC().Truncate(time.Second)
	f.ID = FactID(newID("fct_"))
	f.ProjectID = projectID
	f.CreatedBy = createdBy
	f.CreatedAt, f.UpdatedAt = now, now
	f.Title = strings.TrimSpace(f.Title)
	f.Due = f.Due.UTC().Truncate(time.Second)
	err := s.tx(func(q dbtx) error {
		forest, err := loadForest(q, now)
		if err != nil {
			return err
		}
		p, ok := forest.byID[projectID]
		if !ok {
			return ErrNotFound
		}
		// Validated against the fact as it will be STORED, lead included, so
		// the write boundary judges the same value every reader will see.
		if err := validateFact(f); err != nil {
			return err
		}
		if err := requireUniqueTitle(q, projectID, "", f.Title); err != nil {
			return err
		}
		if f, err = projectFact(q, *p, f); err != nil {
			return err
		}
		f.EffectiveLeadDays = leadFor(f, p.EffectiveLeadDays)
		return insertFact(q, f)
	})
	if err != nil {
		return Fact{}, err
	}
	return f, nil
}

func insertFact(q dbtx, f Fact) error {
	if _, err := q.Exec(
		`INSERT INTO facts (`+factColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		f.ID, f.ProjectID, f.Kind, f.Title, fmtDueTime(f.Due), f.LeadDays, f.RRule, f.TZ,
		boolInt(f.Done), f.Blocker, f.Body, f.EventID, f.CreatedBy,
		fmtEventTime(f.CreatedAt), fmtEventTime(f.UpdatedAt)); err != nil {
		return fmt.Errorf("create fact: %w", err)
	}
	return nil
}

// FactPatch is the set of fields an update may change; a nil field is left
// alone. Kind is deliberately absent: a fact's kind is what its other fields
// mean, so changing it would reinterpret every one of them at once — delete and
// re-add instead.
type FactPatch struct {
	Title    *string    `json:"title"`
	Due      *time.Time `json:"due"`
	LeadDays *int       `json:"leadDays"`
	RRule    *string    `json:"rrule"`
	TZ       *string    `json:"tz"`
	Done     *bool      `json:"done"`
	Blocker  *string    `json:"blocker"` // "" clears: the milestone is no longer waiting on anyone
	Body     *string    `json:"body"`
}

// UpdateFact applies a partial patch and returns the fact as stored. The read,
// the validation, the projection and the write share a transaction, so a
// concurrent patch cannot be merged into a stale copy and the fact's calendar
// event can never be left describing the previous version of it.
func (s *Store) UpdateFact(id FactID, p FactPatch) (Fact, error) {
	var f Fact
	err := s.tx(func(q dbtx) error {
		var err error
		if f, err = getFact(q, id); err != nil {
			return err
		}
		if p.Title != nil {
			f.Title = strings.TrimSpace(*p.Title)
		}
		if p.Due != nil {
			f.Due = p.Due.UTC().Truncate(time.Second)
		}
		if p.LeadDays != nil {
			f.LeadDays = *p.LeadDays
		}
		if p.RRule != nil {
			f.RRule = *p.RRule
		}
		if p.TZ != nil {
			f.TZ = *p.TZ
		}
		if p.Done != nil {
			f.Done = *p.Done
		}
		if p.Blocker != nil {
			f.Blocker = *p.Blocker
		}
		if p.Body != nil {
			f.Body = *p.Body
		}
		if err := validateFact(f); err != nil {
			return err
		}
		if err := requireUniqueTitle(q, f.ProjectID, f.ID, f.Title); err != nil {
			return err
		}
		project, err := getProject(q, f.ProjectID)
		if err != nil {
			return err
		}
		if f, err = projectFact(q, project, f); err != nil {
			return err
		}
		// getProject reads a bare row, whose EffectiveLeadDays is only the
		// un-inherited answer; the tree's is what a reader gets, so resolve
		// from the forest rather than from the row beside us.
		forest, err := loadForest(q, time.Now().UTC())
		if err != nil {
			return err
		}
		lead := project.EffectiveLeadDays
		if p, ok := forest.byID[f.ProjectID]; ok {
			lead = p.EffectiveLeadDays
		}
		f.EffectiveLeadDays = leadFor(f, lead)
		f.UpdatedAt = time.Now().UTC().Truncate(time.Second)
		if _, err := q.Exec(
			`UPDATE facts SET title = ?, due = ?, lead_days = ?, rrule = ?, tz = ?, done = ?,
			        blocker = ?, body = ?, event_id = ?, updated_at = ? WHERE id = ?`,
			f.Title, fmtDueTime(f.Due), f.LeadDays, f.RRule, f.TZ, boolInt(f.Done),
			f.Blocker, f.Body, f.EventID, fmtEventTime(f.UpdatedAt), id); err != nil {
			return fmt.Errorf("update fact: %w", err)
		}
		return nil
	})
	if err != nil {
		return Fact{}, err
	}
	return f, nil
}

// DeleteFact removes one fact and the calendar event it was projected onto, or
// reports ErrNotFound if there was none. The tombstones a sync client learns
// them by come from the triggers, not from here.
func (s *Store) DeleteFact(id FactID) error {
	return s.tx(func(q dbtx) error {
		f, err := getFact(q, id)
		if err != nil {
			return err
		}
		if f.EventID != "" {
			if _, err := q.Exec(`DELETE FROM events WHERE id = ?`, f.EventID); err != nil {
				return fmt.Errorf("delete projected event: %w", err)
			}
		}
		if _, err := q.Exec(`DELETE FROM facts WHERE id = ?`, id); err != nil {
			return fmt.Errorf("delete fact: %w", err)
		}
		return nil
	})
}

// GetFact loads one fact by id.
func (s *Store) GetFact(id FactID) (Fact, error) { return getFact(s.db, id) }

// factByTitle finds a project's fact by title, case-insensitively — the way the
// project tool addresses one, and therefore the uniqueness the guard below
// enforces.
func factByTitle(q dbtx, projectID ProjectID, title string) (Fact, error) {
	f, err := scanFact(q.QueryRow(
		`SELECT `+factColumns+` FROM facts WHERE project_id = ? AND title = ? COLLATE NOCASE ORDER BY rowid`,
		projectID, strings.TrimSpace(title)))
	if errors.Is(err, sql.ErrNoRows) {
		return Fact{}, ErrNotFound
	}
	if err != nil {
		return Fact{}, fmt.Errorf("fact by title: %w", err)
	}
	return f, nil
}

// requireUniqueTitle refuses a title another fact of the same project already
// holds, on both create and rename. A bot addresses a fact BY TITLE, so a twin
// makes BOTH copies unaddressable by the very tool that wrote them — this is
// what keeps a model's "add" from silently becoming a second copy of the fact it
// should have updated. Pass the fact's own id on a rename, so renaming a fact to
// its own title is not a collision with itself.
//
// DECISION (a Go check, not a unique index): a database written before this
// guard can already hold a pair, and `CREATE UNIQUE INDEX` over it would fail
// and leave the whole database unopenable. A Go check refuses new twins while
// old ones stay readable and repairable — and the tool's ambiguity error is what
// keeps a legacy pair from being silently edited at random.
func requireUniqueTitle(q dbtx, projectID ProjectID, self FactID, title string) error {
	existing, err := factByTitle(q, projectID, title)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if existing.ID == self {
		return nil
	}
	return fmt.Errorf("%w: a fact titled %q already exists in this project", ErrDuplicateName, existing.Title)
}

func getFact(q dbtx, id FactID) (Fact, error) {
	f, err := scanFact(q.QueryRow(`SELECT `+factColumns+` FROM facts WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Fact{}, ErrNotFound
	}
	if err != nil {
		return Fact{}, fmt.Errorf("get fact: %w", err)
	}
	return f, nil
}

// ListFacts returns a project's facts URGENCY-FIRST, which is the order the
// detail pane renders top to bottom: undone dated facts by when they are next
// due, then the milestones waiting on a human, then the rest of the open
// milestones, then notes, then everything already done.
//
// The SQL orders by rowid DESCENDING and the sort below is stable, so every
// undated band comes out newest-first for free — created_at is second-truncated
// and would tie for facts written in the same second, and two ULIDs minted in
// the same millisecond do not order by insertion either.
func (s *Store) ListFacts(projectID ProjectID) ([]Fact, error) {
	facts, err := queryFacts(s.db, `WHERE project_id = ? ORDER BY rowid DESC`, projectID)
	if err != nil {
		return nil, err
	}
	// The inherited window comes from the tree, and the sort below asks
	// factSignals, which judges by it — so resolution has to precede ordering,
	// not just rendering.
	forest, err := s.forest()
	if err != nil {
		return nil, err
	}
	lead := defaultLeadDays
	if p, ok := forest.byID[projectID]; ok {
		lead = p.EffectiveLeadDays
	}
	resolveFactLeads(facts, lead)
	now := time.Now().UTC()
	dues := make(map[FactID]time.Time, len(facts))
	for _, f := range facts {
		if signal, ok := factSignals[f.Kind]; ok {
			_, due := signal(f, now)
			dues[f.ID] = due
		}
	}
	sort.SliceStable(facts, func(i, j int) bool {
		a, b := facts[i], facts[j]
		if factBand(a) != factBand(b) {
			return factBand(a) < factBand(b)
		}
		if factBand(a) == bandDated && !dues[a.ID].Equal(dues[b.ID]) {
			return dues[a.ID].Before(dues[b.ID])
		}
		return false
	})
	return facts, nil
}

// The urgency bands ListFacts sorts into. They are the pane's reading order.
const (
	bandDated     = iota // undone deadline or recurring: the dated work
	bandBlocked          // undone milestone waiting on a human
	bandMilestone        // the rest of the open milestones
	bandNote             // undated context
	bandDone             // finished, kept for the record
)

func factBand(f Fact) int {
	switch {
	case f.Done:
		return bandDone
	case f.Kind == FactDeadline || f.Kind == FactRecurring:
		return bandDated
	case f.Kind == FactMilestone && f.Blocker != "":
		return bandBlocked
	case f.Kind == FactMilestone:
		return bandMilestone
	}
	return bandNote
}

// queryFacts is the one fact read, taking a dbtx so the projection walks can
// run inside the transaction that is rewriting them.
func queryFacts(q dbtx, where string, args ...any) ([]Fact, error) {
	rows, err := q.Query(`SELECT `+factColumns+` FROM facts `+where, args...)
	if err != nil {
		return nil, fmt.Errorf("list facts: %w", err)
	}
	defer rows.Close()
	var out []Fact
	for rows.Next() {
		f, err := scanFact(rows)
		if err != nil {
			return nil, fmt.Errorf("scan fact: %w", err)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// ── The tick ──────────────────────────────────────────────────────────────────
// The nudge: one pass over the forest that tells each project's owner, once,
// that its project has got worse. It is the only thing in the service with
// stored state (last_health) and the only thing that writes on a schedule.
//
// DECISION (compare against what the LAST TICK saw, not against a timer): the
// question a nudge answers is "has this changed for the worse since I last
// looked", so the memory it needs is one health per project, not a delivery log
// and not a cooldown. Two consequences fall out for free: a project that stays
// overdue is not re-announced every hour, and a project that goes overdue,
// gets fixed and goes overdue again IS announced twice, because that is two
// pieces of news.
//
// DECISION (an empty last_health counts as ok): so a project that is ALREADY
// overdue when the tick first meets it — the migration case, and a project
// created between two ticks — nudges immediately rather than being silently
// adopted as the new normal. A project that is healthy on its first tick just
// records, because ok-to-ok is not a worsening.

// projectNudgePrefix opens every nudge message. It is the one recognisable
// thing about it: a nudge is an ordinary user-role message so no client needs a
// new rendering, and this is what lets a reader (or a test) tell one apart.
const projectNudgePrefix = "Project nudge — "

// nudgeFactLimit caps the facts a nudge lists. A project of thirty blocked
// milestones is a wall of text the model will skim; five is what the message
// can say and still be read.
const nudgeFactLimit = 5

// ProjectNudge is one delivered nudge, as the tick's response reports it.
type ProjectNudge struct {
	Project string        `json:"project"`
	Bot     BotID         `json:"bot"`
	From    ProjectHealth `json:"from"`
	To      ProjectHealth `json:"to"`

	// The append this nudge made, so the server can start the model turn for it
	// the way sendMessage does. Unexported: it is a handoff inside the package,
	// not part of what the tick serves.
	bot     Bot
	message Message
}

// ProjectSkip is a project that WORSENED and could not be told, with the reason
// — the tick's most useful diagnostic, because those are the ones whose news is
// still pending on the next run.
type ProjectSkip struct {
	Project string `json:"project"`
	Reason  string `json:"reason"`
}

// ProjectTick is what POST /v1/projects/tick answers. Both lists are always
// arrays: a client has no nil case for either.
type ProjectTick struct {
	Checked int            `json:"checked"`
	Nudged  []ProjectNudge `json:"nudged"`
	Skipped []ProjectSkip  `json:"skipped"`
}

// TickProjects is the whole nudge, as ONE function over ONE derivation of the
// forest at `at`. Each project's verdict is decided from that derivation and
// applied in its own transaction, so a failure part-way leaves the projects
// already handled correctly recorded and the rest simply untouched — the next
// tick picks them up, because nothing about the decision depends on this run
// having happened.
//
// The append and the last_health write share that transaction, which is the
// whole of the idempotence argument: there is no instant at which a bot has
// been told and the project does not know, or the project is marked and the bot
// never heard.
func (s *Store) TickProjects(at time.Time) (ProjectTick, error) {
	at = at.UTC()
	forest, err := loadForest(s.db, at)
	if err != nil {
		return ProjectTick{}, err
	}
	out := ProjectTick{Nudged: []ProjectNudge{}, Skipped: []ProjectSkip{}}
	// forest.sorted is urgency-first, so the worst news is delivered first and
	// the response reads in the order a person would want it.
	for _, snapshot := range forest.sorted {
		p := forest.byID[snapshot.ID]
		out.Checked++
		prev := p.LastHealth
		if prev == "" {
			prev = HealthOK
		}
		if healthRank[p.Health] <= healthRank[prev] {
			// Not worse: record it silently, so the NEXT deterioration is
			// measured from where the project actually is. Writing only on a
			// change keeps a steady-state tick from emitting change rows.
			if p.Health != p.LastHealth {
				if err := s.recordHealth(p.ID, p.Health); err != nil {
					return out, err
				}
			}
			continue
		}
		owner := p.EffectiveOwner
		if owner == "" {
			out.Skipped = append(out.Skipped, ProjectSkip{p.Name,
				"got worse but no bot owns it — set an owner on it or on a project above it"})
			continue
		}
		nudge, skipped, err := s.nudge(at, *p, prev, forest.drivingFacts(p.ID))
		if err != nil {
			return out, err
		}
		if skipped != "" {
			out.Skipped = append(out.Skipped, ProjectSkip{p.Name, skipped})
			continue
		}
		out.Nudged = append(out.Nudged, nudge)
	}
	return out, nil
}

// recordHealth stamps what the tick observed. It is a field-only UPDATE, so the
// projects row trigger captures it like any other and a second client refetches
// a project whose condition the tick just noticed.
func (s *Store) recordHealth(id ProjectID, h ProjectHealth) error {
	if _, err := s.db.Exec(`UPDATE projects SET last_health = ? WHERE id = ?`, h, id); err != nil {
		return fmt.Errorf("record project health: %w", err)
	}
	return nil
}

// nudge appends one nudge to the owner's thread and stamps last_health in the
// SAME transaction. It returns a non-empty reason instead when the bot cannot
// take the message right now — a turn already in flight — and then writes
// nothing at all, so the news survives to the next tick.
//
// The append is claimBot + appendMessage: exactly what POST /v1/bots/{id}/
// messages does, which is what makes the turn start, the reply land in the
// transcript, and the UI need no new rendering.
func (s *Store) nudge(at time.Time, p Project, prev ProjectHealth, facts []Fact) (ProjectNudge, string, error) {
	out := ProjectNudge{Project: p.Name, Bot: p.EffectiveOwner, From: prev, To: p.Health}
	busy := ""
	err := s.tx(func(q dbtx) error {
		bot, err := getBot(q, p.EffectiveOwner)
		if errors.Is(err, ErrNotFound) {
			// The bot went between the derivation and this transaction. Treat
			// it as the ownerless case: nothing written, next tick re-decides.
			busy = "got worse but its owner bot no longer exists"
			return nil
		}
		if err != nil {
			return err
		}
		if err := claimBot(q, p.EffectiveOwner); errors.Is(err, ErrBusy) {
			busy = "got worse but " + bot.DisplayName + " is busy with a turn already in flight"
			return nil
		} else if err != nil {
			return err
		}
		msg, err := appendMessage(q, "", p.EffectiveOwner, "user",
			nudgeMessage(at, p, prev, facts), StatusAwaiting, nil, nil)
		if err != nil {
			return err
		}
		if _, err := q.Exec(`UPDATE projects SET last_health = ? WHERE id = ?`, p.Health, p.ID); err != nil {
			return fmt.Errorf("record project health: %w", err)
		}
		out.bot, out.message = bot, msg
		return nil
	})
	if err != nil {
		return ProjectNudge{}, "", err
	}
	return out, busy, nil
}

// drivingFacts is what the nudge lists: the undone facts anywhere in the
// project's SUBTREE that are as loud as the project now is.
//
// DECISION (the band, not the exact health): a project reading blocked is
// driven by its blocked milestones AND by anything else in the same severity
// band, because those are the things a person would act on together — and
// matching on the exact health would leave a due_soon deadline unmentioned
// beside a blocked step that outranks it by one. Subtree, not own facts,
// because health rolled up: a parent's nudge has to name the child's fact that
// caused it, or the message points at nothing.
func (f *projectForest) drivingFacts(root ProjectID) []Fact {
	want := severityFor(f.byID[root].Health)
	var out []Fact
	seen := map[ProjectID]bool{}
	var walk func(id ProjectID)
	walk = func(id ProjectID) {
		if seen[id] {
			return
		}
		seen[id] = true
		for _, fact := range f.facts[id] {
			if fact.Done {
				continue
			}
			signal, ok := factSignals[fact.Kind]
			if !ok {
				continue
			}
			health, _ := signal(fact, f.now)
			if severityFor(health) == want && health != HealthOK {
				out = append(out, fact)
			}
		}
		for _, c := range f.children[id] {
			walk(c.ID)
		}
	}
	walk(root)
	// Most urgent first, so a truncated list keeps the facts that matter: the
	// dated ones by date, then everything undated in the order it was written.
	sort.SliceStable(out, func(i, j int) bool {
		a, b := factBand(out[i]), factBand(out[j])
		if a != b {
			return a < b
		}
		if a == bandDated {
			return out[i].Due.Before(out[j].Due)
		}
		return false
	})
	if len(out) > nudgeFactLimit {
		out = out[:nudgeFactLimit]
	}
	return out
}

// nudgeMessage renders the message the owner reads. It says what changed, which
// facts drove it, and what to do — in that order, because a model that stops
// reading after the first line has still learned the thing that matters.
func nudgeMessage(at time.Time, p Project, prev ProjectHealth, facts []Fact) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s%s is now %s %s (was %s %s). Facts driving it:",
		projectNudgePrefix, p.Name, p.Severity, p.Health, severityFor(prev), prev)
	if len(facts) == 0 {
		// Only reachable if the facts moved between the derivation and here.
		// Saying so beats an empty list the model reads as "nothing to do".
		b.WriteString("\n- (the facts have changed since; call the project tool to see them)")
	}
	for _, f := range facts {
		fmt.Fprintf(&b, "\n- %s", nudgeFactLine(at, f))
	}
	b.WriteString("\nAct on it or update the facts with the project tool; reply with what you did.")
	return b.String()
}

// nudgeFactLine is one fact as the nudge names it: enough to act on without
// calling the tool, and short enough that five of them still read as a list.
func nudgeFactLine(now time.Time, f Fact) string {
	if f.Blocker != "" {
		return fmt.Sprintf("%s: blocked — %s", f.Title, f.Blocker)
	}
	signal, ok := factSignals[f.Kind]
	if !ok {
		return f.Title
	}
	_, due := signal(f, now)
	if due.IsZero() {
		return f.Title
	}
	local := due.Local()
	if local.Before(now) {
		return fmt.Sprintf("%s: due %s (%dd overdue, lead %dd)",
			f.Title, local.Format(time.DateOnly), wholeDays(now.Sub(local)), f.EffectiveLeadDays)
	}
	return fmt.Sprintf("%s: due %s (in %dd, lead %dd)",
		f.Title, local.Format(time.DateOnly), wholeDays(local.Sub(now)), f.EffectiveLeadDays)
}

// ── Calendar projection ───────────────────────────────────────────────────────
// Every UNDONE deadline or recurring fact has exactly one Event on the calendar
// named "Projects", so a passport expiry appears in the month grid the user
// already reads without a second scheduler and without a sync job.
//
// DECISION (maintained in the fact's own transaction, by ONE function): every
// write path calls projectFact and nothing else touches the projection. That is
// what makes it converge — writing the same fact twice yields one event, a fact
// marked done loses its event, and a dangling pointer (the user deleted the
// event in the Calendar panel, which is legal: it is an ordinary event row)
// repairs itself on the next write.
//
// DECISION (the projection OWNS the row): the event's calendar, title, times,
// notes and rule are rewritten from the fact on every write, so moving a
// projected event in the UI does not stick. The fact is the truth; the event is
// its shadow. Deleting the event is the one edit that survives, because it is
// indistinguishable from the row never having existed — and the next fact write
// simply projects a fresh one.

// projectsCalendarName is the name the projection's ensure keys on — a name,
// not a flag, exactly as the Personal default is (see Calendar in schema.go).
const projectsCalendarName = "Projects"

func ensureProjectsCalendar(q dbtx) (Calendar, error) {
	return ensureCalendar(q, projectsCalendarName, "")
}

// factNeedsEvent reports whether a fact belongs on the calendar at all: an
// undone, dated obligation does; a milestone, a note and anything already done
// do not.
func factNeedsEvent(f Fact) bool {
	return !f.Done && (f.Kind == FactDeadline || f.Kind == FactRecurring)
}

// projectedTitle is what the calendar shows: the project name carries the
// context "renew" alone would not.
func projectedTitle(p Project, f Fact) string { return p.Name + ": " + f.Title }

// projectFact brings the fact's calendar event into line with the fact and
// returns the fact with its EventID as it now stands. It is called from every
// fact write path INSIDE that write's transaction, and it is the only place the
// projection is decided.
func projectFact(q dbtx, p Project, f Fact) (Fact, error) {
	now := time.Now().UTC().Truncate(time.Second)
	if !factNeedsEvent(f) {
		if f.EventID != "" {
			if _, err := q.Exec(`DELETE FROM events WHERE id = ?`, f.EventID); err != nil {
				return Fact{}, fmt.Errorf("delete projected event: %w", err)
			}
			f.EventID = ""
		}
		return f, nil
	}
	// The ensure runs only here — on a WRITE that needs the calendar — so a net
	// of milestones and notes never grows one, and a read never creates state.
	cal, err := ensureProjectsCalendar(q)
	if err != nil {
		return Fact{}, err
	}
	ev := Event{
		CalendarID: cal.ID,
		Title:      projectedTitle(p, f),
		StartsAt:   f.Due,
		EndsAt:     f.Due.Add(projectedEventDuration),
		Notes:      f.Body,
		RRule:      f.RRule,
		TZ:         f.TZ,
		CreatedBy:  f.CreatedBy,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if f.EventID != "" {
		existing, err := getEvent(q, f.EventID)
		if err == nil {
			ev.ID, ev.CreatedAt = existing.ID, existing.CreatedAt
			return f, updateEventRow(q, ev)
		}
		if !errors.Is(err, ErrNotFound) {
			return Fact{}, err
		}
		// The pointer dangles: the event was deleted out from under the fact.
		// Project a fresh one rather than failing a write over it.
	}
	ev.ID = EventID(newID("evt_"))
	f.EventID = ev.ID
	return f, insertEventRow(q, ev)
}

// retitleProjectedEvents rewrites the titles of a renamed project's projected
// events, in the transaction that renamed it — the events carry the project's
// name, so skipping this would leave the calendar naming a project that no
// longer exists. A fact whose event has since been deleted simply matches no
// row; the next fact write repairs it.
func retitleProjectedEvents(q dbtx, p Project) error {
	facts, err := queryFacts(q, `WHERE project_id = ? AND event_id != '' ORDER BY rowid`, p.ID)
	if err != nil {
		return err
	}
	now := fmtEventTime(time.Now().UTC().Truncate(time.Second))
	for _, f := range facts {
		if _, err := q.Exec(`UPDATE events SET title = ?, updated_at = ? WHERE id = ?`,
			projectedTitle(p, f), now, f.EventID); err != nil {
			return fmt.Errorf("retitle projected event: %w", err)
		}
	}
	return nil
}
