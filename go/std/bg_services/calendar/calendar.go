// Package calendar is the std calendar background service: a local calendar
// of single and recurring events, with a bounded RFC 5545 RRULE subset and
// timezone/DST-correct instance expansion, served over a unix-socket REST API.
//
// The service owns <Root>/calendar/calendar.db (sqlite) and serves its API on
// <Root>/calendar/calendar.sock — single writer, exactly like artifacts, so
// callers route through the service instead of racing the DB file.
package calendar

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"time"

	_ "time/tzdata" // embed the IANA zone db; never depend on host /usr/share/zoneinfo
)

// Dir is the subdirectory of Root the service owns.
const Dir = "calendar"

// Config configures a Service.
type Config struct {
	// Root is the local directory the service operates in. The service
	// creates Root/calendar if it does not exist.
	Root string

	// Interval is reserved for future background work (reminder scanning);
	// the v1 service only serves its API. Accepted for wiring symmetry with
	// the other std services.
	Interval time.Duration

	// Logger receives lifecycle lines. Nil means the standard logger.
	Logger *log.Logger
}

// Service is the calendar store plus its API server. It implements
// bgservices.Service.
type Service struct {
	root   string
	store  *Store
	logger *log.Logger
}

// New validates cfg, creates the calendar directory, opens the store, and
// returns a ready-to-run Service.
func New(cfg Config) (*Service, error) {
	if cfg.Root == "" {
		return nil, fmt.Errorf("calendar: root directory is required")
	}
	root, err := filepath.Abs(cfg.Root)
	if err != nil {
		return nil, fmt.Errorf("calendar: resolve root: %w", err)
	}

	s := &Service{root: root, logger: cfg.Logger}
	if s.logger == nil {
		s.logger = log.Default()
	}
	dir := filepath.Join(root, Dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("calendar: create %s: %w", dir, err)
	}
	if s.store, err = OpenStore(DBPath(root)); err != nil {
		return nil, fmt.Errorf("calendar: %w", err)
	}
	return s, nil
}

// Name implements bgservices.Service.
func (s *Service) Name() string { return "calendar" }

// Root returns the absolute root directory the service operates in.
func (s *Service) Root() string { return s.root }

// Run serves the calendar API on the root's unix socket until ctx is
// canceled. All access routes through this single writer instead of racing
// the DB file.
func (s *Service) Run(ctx context.Context) error { return s.serve(ctx) }

// Verify is a fast end-to-end self-check against a throwaway in-memory
// store: create a weekly event whose window crosses a DST boundary, expand,
// and assert the exact instants — the 9am wall clock must hold on both sides
// of the switch while the UTC offset changes (tz correctness, the whole
// point). Then EXDATE must remove exactly one instance, and COUNT and UNTIL
// must terminate the rule where they should.
func (s *Service) Verify(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	probe, err := OpenStore(":memory:")
	if err != nil {
		return fmt.Errorf("calendar verify: %w", err)
	}
	defer probe.Close()

	ev := Event{
		Title: "verify probe",
		Start: "2024-02-26T09:00:00",
		End:   "2024-02-26T10:00:00",
		TZ:    "America/New_York", // springs forward 2024-03-10, inside the COUNT=4 run
		RRULE: "FREQ=WEEKLY;BYDAY=MO;COUNT=4",
	}
	if err := validateEvent(ev); err != nil {
		return fmt.Errorf("calendar verify: %w", err)
	}
	created, err := probe.Create(ev)
	if err != nil {
		return fmt.Errorf("calendar verify: %w", err)
	}

	// The window is far wider than the rule so only COUNT can terminate it.
	from := time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	want := []string{
		"2024-02-26T09:00:00-05:00",
		"2024-03-04T09:00:00-05:00",
		"2024-03-11T09:00:00-04:00",
		"2024-03-18T09:00:00-04:00",
	}
	got, err := expandStarts(probe, created.ID, from, to)
	if err != nil {
		return err
	}
	if !slices.Equal(got, want) {
		return fmt.Errorf("calendar verify: weekly expansion across DST = %v, want %v", got, want)
	}

	created.EXDATE = []string{"2024-03-04T09:00:00"}
	if _, err := probe.Update(created); err != nil {
		return fmt.Errorf("calendar verify: %w", err)
	}
	got, err = expandStarts(probe, created.ID, from, to)
	if err != nil {
		return err
	}
	if !slices.Equal(got, []string{want[0], want[2], want[3]}) {
		return fmt.Errorf("calendar verify: expansion with EXDATE = %v, want %v", got, []string{want[0], want[2], want[3]})
	}

	created.EXDATE = nil
	created.RRULE = "FREQ=WEEKLY;BYDAY=MO;UNTIL=2024-03-11T09:00:00"
	if _, err := probe.Update(created); err != nil {
		return fmt.Errorf("calendar verify: %w", err)
	}
	got, err = expandStarts(probe, created.ID, from, to)
	if err != nil {
		return err
	}
	if !slices.Equal(got, want[:3]) {
		return fmt.Errorf("calendar verify: expansion with UNTIL = %v, want %v", got, want[:3])
	}
	return nil
}

// expandStarts reads the event back through the store and returns its
// expanded instance starts as RFC3339 strings (offset included).
func expandStarts(probe *Store, id EventID, from, to time.Time) ([]string, error) {
	ev, err := probe.Get(id)
	if err != nil {
		return nil, fmt.Errorf("calendar verify: %w", err)
	}
	instances, err := Expand(ev, from, to)
	if err != nil {
		return nil, fmt.Errorf("calendar verify: %w", err)
	}
	starts := make([]string, len(instances))
	for i, in := range instances {
		starts[i] = in.Start.Format(time.RFC3339)
	}
	return starts, nil
}
