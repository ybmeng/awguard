// Package automations is the std automations runner service: it discovers
// automation-123 manifests in a repo checkout, invokes their form-3 commands
// on the manifests' schedules, and records every run's result envelope, all
// served over a unix-socket REST API (see skills/automation_123/SKILL.md for
// the paradigm this serves).
//
// The service owns <Root>/automations/automations.db (sqlite) and serves its
// API on <Root>/automations/automations.sock — single writer, exactly like
// calendar, so callers route through the service instead of racing the DB.
// Runs are serial across all automations: one subprocess at a time, so
// repo-tree writes stay single-writer too.
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
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "time/tzdata" // embed the IANA zone db; never depend on host /usr/share/zoneinfo
)

// Dir is the subdirectory of Root the service owns.
const Dir = "automations"

const (
	// DefaultInterval is the scheduler tick used when Config.Interval is zero.
	DefaultInterval = time.Minute

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

	// Interval is the scheduler tick. Zero means DefaultInterval.
	Interval time.Duration

	// Logger receives lifecycle and per-run lines. Nil means the standard
	// logger.
	Logger *log.Logger
}

// Service is the automations registry, its serial runner and its API server.
// It implements bgservices.Service.
type Service struct {
	root     string
	repoDir  string
	interval time.Duration
	timeout  time.Duration
	logger   *log.Logger
	store    *Store
	queue    chan job

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
		root: root, repoDir: cfg.RepoDir, interval: cfg.Interval,
		timeout: runTimeout, logger: cfg.Logger,
		queue: make(chan job, queueCap), autos: []Automation{}, pending: map[string]bool{},
	}
	if s.interval <= 0 {
		s.interval = DefaultInterval
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
// socket, the serial runner, and the scheduler loop that rediscovers manifests
// and enqueues due attempts each tick. It refuses to start when another live
// service already serves this root.
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

	errCh := make(chan error, 3)
	go func() { errCh <- s.serve(ctx) }()
	go func() { errCh <- s.worker(ctx) }()
	go func() { errCh <- s.scheduleLoop(ctx) }()

	err := <-errCh
	cancel()
	<-errCh
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

// scheduleLoop rediscovers the registry and enqueues due scheduled runs, every
// Interval until ctx ends. The first tick runs immediately.
func (s *Service) scheduleLoop(ctx context.Context) error {
	if s.repoDir == "" {
		s.logger.Print("automations: no repo configured (pass -automations-repo or set $AUTOMATIONS_REPO); serving zero automations")
	}
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		s.tick(time.Now())
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// tick is one scheduler pass: refresh the discovery snapshot, then evaluate
// every scheduled automation's window against the runs table and enqueue an
// attempt where one is due. All state is derived from disk, so a restart
// between ticks changes nothing.
func (s *Service) tick(now time.Time) {
	autos := s.discover()
	s.mu.Lock()
	s.autos = autos
	s.mu.Unlock()

	for _, a := range autos {
		if a.Schedule == nil {
			continue
		}
		st, err := s.scheduleState(a, now)
		if err != nil {
			s.logger.Printf("automations: %s: %v", a.Name, err)
			continue
		}
		if !due(a.Schedule, st, now) {
			continue
		}
		id, err := s.enqueue(a.Name, "schedule")
		if err != nil {
			continue // in flight or queued — the guard, not a failure
		}
		s.logger.Printf("automations: %s due (window opened %s), enqueued %s", a.Name, fmtTime(st.occ), id)
	}
}

// Verify is a fast, self-contained end-to-end check against throwaway dirs: a
// fake automation manifest (with a schedule block) whose form-3 command is a
// tiny sh script emitting a valid envelope is discovered, run manually through
// the real runner path, and its recorded envelope round-tripped; then the
// advanced()/freshness derivations and the schedule expander's next-occurrence
// math (FREQ=MONTHLY;BYDAY=4TU across a DST boundary) are asserted. No
// network, well under a second.
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

	probe, err := New(Config{
		Root: filepath.Join(tmp, "root"), RepoDir: filepath.Join(tmp, "repo"),
		Logger: log.New(io.Discard, "", 0),
	})
	if err != nil {
		return fmt.Errorf("automations verify: %w", err)
	}
	defer probe.store.Close()

	autos := probe.discover()
	if len(autos) != 1 || autos[0].Name != "probe" || autos[0].Schedule == nil {
		return fmt.Errorf("automations verify: discovery = %+v, want one scheduled automation named probe", autos)
	}
	probe.mu.Lock()
	probe.autos = autos
	probe.mu.Unlock()

	// One manual run through the real runner path: enqueue, execute, read back.
	id, err := probe.enqueue("probe", "manual")
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

	// freshness derivations.
	a := autos[0]
	occ := time.Date(2026, 2, 24, 18, 5, 0, 0, time.UTC)
	open := windowState{occ: occ, end: occ.Add(30 * time.Hour), open: true}
	closed := windowState{occ: occ, end: occ.Add(30 * time.Hour)}
	failedRun := &Run{Status: "failed"}
	if got := freshness(a, open, nil); got != "pending" {
		return fmt.Errorf("automations verify: freshness(open unsatisfied) = %q, want pending", got)
	}
	if got := freshness(a, closed, nil); got != "stale" {
		return fmt.Errorf("automations verify: freshness(closed unsatisfied) = %q, want stale", got)
	}
	if got := freshness(a, windowState{occ: occ, end: occ.Add(30 * time.Hour), satisfied: true}, failedRun); got != "failed" {
		return fmt.Errorf("automations verify: freshness(satisfied, last run failed) = %q, want failed", got)
	}
	if got := freshness(Automation{}, windowState{}, nil); got != "unscheduled" {
		return fmt.Errorf("automations verify: freshness(no schedule) = %q, want unscheduled", got)
	}

	// Next-occurrence math across the 2026-03-08 US DST switch: the 13:05 wall
	// clock must hold on both sides while the UTC offset changes.
	now := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	next, err := a.Schedule.nextOccurrence(now)
	if err != nil {
		return fmt.Errorf("automations verify: %w", err)
	}
	if want := time.Date(2026, 3, 24, 17, 5, 0, 0, time.UTC); !next.Equal(want) {
		return fmt.Errorf("automations verify: next 4TU after %s = %s, want %s (13:05 EDT)", now, next, want)
	}
	prev, err := a.Schedule.latestOccurrence(now)
	if err != nil {
		return fmt.Errorf("automations verify: %w", err)
	}
	if want := time.Date(2026, 2, 24, 18, 5, 0, 0, time.UTC); !prev.Equal(want) {
		return fmt.Errorf("automations verify: latest 4TU before %s = %s, want %s (13:05 EST)", now, prev, want)
	}
	return nil
}
