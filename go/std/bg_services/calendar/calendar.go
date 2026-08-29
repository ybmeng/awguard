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
