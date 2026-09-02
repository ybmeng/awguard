// Package automations is the std automations runner service: it discovers
// automation-123 manifests in a repo checkout, runs their form-3 commands
// when the botnet calendar fires them, and records every run's result
// envelope, all served over a unix-socket REST API (see
// skills/automation_123/SKILL.md for the paradigm this serves).
//
// The service owns no clock and no schedule. The botnet calendar is the
// single source of truth for what fires: the ping service ticks execcal,
// execcal reads the calendar's fireable instances and POSTs them here as
// /fire requests, and this service is the idempotent arbiter — it answers
// "satisfied", "paced" or "enqueued" purely from the runs table, so a
// repeated, late or double fire never misbehaves. Its own /tick (pinged
// every few minutes) rescans manifests and ensures each scheduled automation
// has a recurring calendar event to fire it (ensure-if-absent: the calendar
// stays authoritative, user and bot edits stick).
//
// The service owns <Root>/automations/automations.db (sqlite) and serves its
// API on <Root>/automations/automations.sock — single writer, so callers
// route through the service instead of racing the DB. Runs are serial across
// all automations: one subprocess at a time, so repo-tree writes stay
// single-writer too.
//
// MVP scope is form 3 only. OPEN: forms 2/1 (cheap-driver recipes,
// frontier-model repair) are deferred until the user settles how the service
// should drive models — see the OPEN marker on Envelope in envelope.go.
package automations

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "time/tzdata" // embed the IANA zone db; never depend on host /usr/share/zoneinfo
)

// Dir is the subdirectory of Root the service owns.
const Dir = "automations"

const (
	// runTimeout is the hard per-run wall-clock limit; on expiry the whole
	// process group is killed so python children die with their sh parent.
	runTimeout = 15 * time.Minute

	// queueCap bounds the serial runner's backlog. It only ever holds one
	// entry per automation (enqueue refuses a second), so this is generous.
	queueCap = 64
)

// DefaultRepoDir is the manifest repo: $AUTOMATIONS_REPO, else "" (disabled).
func DefaultRepoDir() string {
	return os.Getenv("AUTOMATIONS_REPO")
}

// Config configures a Service.
type Config struct {
	// Root is the local directory the service operates in. The service
	// creates Root/automations if it does not exist.
	Root string

	// RepoDir is the checkout scanned for automation manifests. Empty is
	// legal: the service runs with zero automations, logs how to enable
	// itself, and the API answers with an empty list.
	RepoDir string

	// BotnetAddr is the botnet HTTP listen address (host:port)
	// registration-ensure talks to. Empty disables registration — the
	// service still discovers, fires and records.
	BotnetAddr string

	// Logger receives lifecycle and per-run lines. Nil means the standard
	// logger.
	Logger *log.Logger
}

// Service is the automations registry, its serial runner and its API server.
// It implements bgservices.Service.
type Service struct {
	root       string
	repoDir    string
	botnetAddr string
	timeout    time.Duration
	logger     *log.Logger
	store      *Store
	queue      chan job

	mu      sync.Mutex
	autos   []Automation    // latest discovery snapshot
	pending map[string]bool // automations with a run queued or in flight
}

// New validates cfg, creates the automations directory, opens the store, and
// returns a ready-to-run Service.
func New(cfg Config) (*Service, error) {
	if cfg.Root == "" {
		return nil, fmt.Errorf("automations: root directory is required")
	}
	root, err := filepath.Abs(cfg.Root)
	if err != nil {
		return nil, fmt.Errorf("automations: resolve root: %w", err)
	}

	s := &Service{
		root: root, repoDir: cfg.RepoDir, botnetAddr: cfg.BotnetAddr,
		timeout: runTimeout, logger: cfg.Logger,
		queue: make(chan job, queueCap), autos: []Automation{}, pending: map[string]bool{},
	}
	if s.logger == nil {
		s.logger = log.Default()
	}
	if s.repoDir != "" {
		if s.repoDir, err = filepath.Abs(s.repoDir); err != nil {
			return nil, fmt.Errorf("automations: resolve repo dir: %w", err)
		}
		// DECISION: a nonexistent RepoDir fails New rather than degrading to
		// zero automations — it is an install-time misconfiguration and must
		// be loud at the one moment someone is watching, not a silent empty
		// registry. Empty RepoDir stays legal (feature off, not misconfigured).
		if info, err := os.Stat(s.repoDir); err != nil || !info.IsDir() {
			return nil, fmt.Errorf("automations: repo dir %s is not a directory (pass -automations-repo, or empty to run zero automations): %v", s.repoDir, err)
		}
	}
	dir := filepath.Join(root, Dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("automations: create %s: %w", dir, err)
	}
	if s.store, err = OpenStore(DBPath(root)); err != nil {
		return nil, fmt.Errorf("automations: %w", err)
	}
	return s, nil
}

// Name implements bgservices.Service.
func (s *Service) Name() string { return "automations" }

// Root returns the absolute root directory the service operates in.
func (s *Service) Root() string { return s.root }

// Run operates the service until ctx is canceled: the API server on the unix
// socket and the serial runner. There is no scheduler — the ping service
// drives /tick and the calendar (via execcal) drives /fire. One startup tick
// runs immediately so the registry and calendar registration converge at
// boot instead of one ping interval later. Run refuses to start when another
// live service already serves this root.
func (s *Service) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	if c, err := Dial(ctx, s.root); err == nil {
		c.Close()
		return fmt.Errorf("automations: another service is already serving %s", s.root)
	}
	// A previous Run (Supervise restarts on the same Service) may have left
	// jobs in the queue and rows stuck at queued/running; both describe a
	// runner that no longer exists.
	s.drainQueue()
	if err := s.store.SweepInterrupted(fmtTime(time.Now())); err != nil {
		return err
	}
	if s.repoDir == "" {
		s.logger.Print("automations: no repo configured (pass -automations-repo or set $AUTOMATIONS_REPO); serving zero automations")
	}

	go func() {
		if err := s.tick(); err != nil {
			s.logger.Printf("automations: startup tick: %v (retried on the next ping)", err)
		}
	}()

	errCh := make(chan error, 2)
	go func() { errCh <- s.serve(ctx) }()
	go func() { errCh <- s.worker(ctx) }()

	err := <-errCh
	cancel()
	<-errCh
	return err
}

func (s *Service) drainQueue() {
	for {
		select {
		case <-s.queue:
		default:
			s.mu.Lock()
			s.pending = map[string]bool{}
			s.mu.Unlock()
			return
		}
	}
}

// tick is one maintenance pass, driven by the ping service (and once at
// startup): refresh the discovery snapshot from the repo, then ensure every
// scheduled automation is registered on the botnet calendar. The returned
// error is for logging only — an unreachable botnet is retried on the next
// tick, never fatal, and the rescan half always happens.
func (s *Service) tick() error {
	autos := s.discover()
	s.mu.Lock()
	s.autos = autos
	s.mu.Unlock()
	return s.ensureRegistered(autos)
}

// Verify is a fast, self-contained end-to-end check against throwaway dirs
// and fakes: a fake automation manifest (with a schedule template) whose
// form-3 command emits a valid envelope is discovered, run manually through
// the real runner path, and its recorded envelope round-tripped; the
// advanced() rules and the fire arbiter's verdicts (enqueued → satisfied,
// paced) are asserted against crafted runs; and one registration-ensure pass
// against an in-memory fake botnet must create the Automations calendar and
// event exactly once. Loopback only, well under a second.
func (s *Service) Verify(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp("", "std_automations_verify_")
	if err != nil {
		return fmt.Errorf("automations verify: temp dir: %w", err)
	}
	defer os.RemoveAll(tmp)

	autoDir := filepath.Join(tmp, "repo", "probe")
	if err := os.MkdirAll(autoDir, 0o755); err != nil {
		return fmt.Errorf("automations verify: %w", err)
	}
	manifest := `---
name: probe
goal: verify probe
forms:
  "3": sh emit.sh
verify: "true"
cadence: monthly, fourth Tuesday
schedule:
  rrule: "FREQ=MONTHLY;BYDAY=4TU"
  at: "13:05"
  tz: "America/New_York"
  retry_every: 2h
  retry_for: 30h
---
verify probe body
`
	script := "#!/bin/sh\necho progress line\n" +
		`echo '{"automation":"probe","status":"ok","form_used":3,"artifacts":[{"path":"data/probe.csv","rows":2,"newest":"2026-01"}],"escalation_reason":null}'` + "\n"
	if err := os.WriteFile(filepath.Join(autoDir, "README.md"), []byte(manifest), 0o644); err != nil {
		return fmt.Errorf("automations verify: %w", err)
	}
	if err := os.WriteFile(filepath.Join(autoDir, "emit.sh"), []byte(script), 0o644); err != nil {
		return fmt.Errorf("automations verify: %w", err)
	}

	// A fake botnet calendar: empty until POSTed to, then remembers.
	var fakeMu sync.Mutex
	var calendars, events []map[string]any
	var writes int
	fake := httptest.NewServer(newVerifyBotnet(&fakeMu, &calendars, &events, &writes))
	defer fake.Close()

	probe, err := New(Config{
		Root: filepath.Join(tmp, "root"), RepoDir: filepath.Join(tmp, "repo"),
		BotnetAddr: strings.TrimPrefix(fake.URL, "http://"),
		Logger:     log.New(io.Discard, "", 0),
	})
	if err != nil {
		return fmt.Errorf("automations verify: %w", err)
	}
	defer probe.store.Close()

	// One tick: discovery plus registration-ensure.
	if err := probe.tick(); err != nil {
		return fmt.Errorf("automations verify: tick: %w", err)
	}
	autos := probe.autos
	if len(autos) != 1 || autos[0].Name != "probe" || autos[0].Schedule == nil {
		return fmt.Errorf("automations verify: discovery = %+v, want one scheduled automation named probe", autos)
	}
	fakeMu.Lock()
	firstWrites := writes
	fakeMu.Unlock()
	if firstWrites != 2 {
		return fmt.Errorf("automations verify: registration made %d writes, want 2 (calendar + event)", firstWrites)
	}
	// Ensure-if-absent: a second tick must write nothing.
	if err := probe.tick(); err != nil {
		return fmt.Errorf("automations verify: second tick: %w", err)
	}
	fakeMu.Lock()
	secondWrites := writes
	fakeMu.Unlock()
	if secondWrites != firstWrites {
		return fmt.Errorf("automations verify: second tick wrote %d more times; registration must be ensure-if-absent", secondWrites-firstWrites)
	}

	// One manual run through the real runner path: enqueue, execute, read back.
	id, err := probe.enqueue("probe", "manual", "", "")
	if err != nil {
		return fmt.Errorf("automations verify: enqueue: %w", err)
	}
	probe.execute(ctx, <-probe.queue)
	run, err := probe.store.Get(id)
	if err != nil {
		return fmt.Errorf("automations verify: %w", err)
	}
	if run.Status != "ok" || run.ExitCode != 0 || run.Finished == "" {
		return fmt.Errorf("automations verify: run = status %q exit %d finished %q, want a clean ok run (error: %s)", run.Status, run.ExitCode, run.Finished, run.Error)
	}
	var env Envelope
	if err := json.Unmarshal([]byte(run.Envelope), &env); err != nil {
		return fmt.Errorf("automations verify: recorded envelope does not parse: %v", err)
	}
	if env.Automation != "probe" || len(env.Artifacts) != 1 || env.Artifacts[0].Newest != "2026-01" {
		return fmt.Errorf("automations verify: envelope did not round-trip: %+v", env)
	}

	// advanced() on crafted envelopes.
	base := Envelope{Status: "ok", Artifacts: []ArtifactEntry{{Path: "data/x.csv", Rows: 10, Newest: "2026-01"}}}
	newer := Envelope{Status: "ok", Artifacts: []ArtifactEntry{{Path: "data/x.csv", Rows: 10, Newest: "2026-02"}}}
	newPath := Envelope{Status: "ok", Artifacts: []ArtifactEntry{{Path: "data/y.csv", Rows: 1, Newest: "2026-01"}}}
	switch {
	case !advanced(base, nil):
		return fmt.Errorf("automations verify: first ok run must advance with no baseline")
	case advanced(Envelope{Status: "failed"}, nil):
		return fmt.Errorf("automations verify: a failed run must not advance with no baseline")
	case advanced(base, &base):
		return fmt.Errorf("automations verify: restated history (same paths, same newest) must not advance")
	case !advanced(newer, &base) || !advanced(newPath, &base):
		return fmt.Errorf("automations verify: a newer period or a new path must advance")
	}

	// Fire arbiter verdicts. The manual run above started inside this window
	// and advanced (no baseline), so the window is already satisfied.
	a := autos[0]
	now := time.Now().UTC()
	ws, we := now.Add(-time.Hour), now.Add(5*time.Hour)
	if v, err := probe.fireVerdict(a, ws, we, now); err != nil || v != "satisfied" {
		return fmt.Errorf("automations verify: fireVerdict after an advancing run = (%q, %v), want satisfied", v, err)
	}
	// A later window has that run as its pre-window baseline; a fresh
	// restating attempt inside it paces, and pacing lapses after retry_every.
	ws2, we2 := now.Add(time.Minute), now.Add(6*time.Hour)
	later := now.Add(3 * time.Minute)
	if err := probe.store.Insert(Run{
		ID: newID("run_"), Automation: "probe", Trigger: "schedule",
		Started: fmtTime(now.Add(2 * time.Minute)), Finished: fmtTime(now.Add(2 * time.Minute)),
		Status: "ok", FormUsed: 3, Envelope: run.Envelope,
		WindowStart: fmtTime(ws2), WindowEnd: fmtTime(we2),
	}); err != nil {
		return fmt.Errorf("automations verify: %w", err)
	}
	if v, err := probe.fireVerdict(a, ws2, we2, later); err != nil || v != "paced" {
		return fmt.Errorf("automations verify: fireVerdict on a fresh restating attempt = (%q, %v), want paced", v, err)
	}
	if v, err := probe.fireVerdict(a, ws2, we2, later.Add(3*time.Hour)); err != nil || v != "enqueued" {
		return fmt.Errorf("automations verify: fireVerdict after retry_every lapsed = (%q, %v), want enqueued", v, err)
	}

	// Freshness derives from the recorded fire window.
	st, err := probe.windowFromFires(a, later)
	if err != nil {
		return fmt.Errorf("automations verify: %w", err)
	}
	if !st.open || st.satisfied {
		return fmt.Errorf("automations verify: window state = %+v, want open and unsatisfied", st)
	}
	if got := freshness(a, st, &run); got != "pending" {
		return fmt.Errorf("automations verify: freshness = %q, want pending", got)
	}
	if got := freshness(Automation{}, windowState{}, nil); got != "unscheduled" {
		return fmt.Errorf("automations verify: freshness(no schedule) = %q, want unscheduled", got)
	}
	return nil
}
