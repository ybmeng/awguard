// Package artifacts is the std_artifacts background service: a managed
// artifact store.
//
// Insert moves referenced on-disk files into a fresh subdirectory of the
// global managed/ dir, assigns the dir a monotonically increasing id,
// force-syncs it to remote storage (Google Drive), and only then returns the
// managed dir id. From that point the files are referenced by id and served
// from local storage, falling back to the remote copy.
//
// The background service additionally watches <root>/inbox: every file
// dropped there is inserted automatically.
package artifacts

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// InboxDir is the subdirectory of Root watched for files to auto-insert.
	InboxDir = "inbox"
	// ManagedDir is the subdirectory of Root holding the managed store.
	ManagedDir = "managed"

	// DefaultInterval is the poll interval used when Config.Interval is zero.
	DefaultInterval = time.Second
)

// Config configures a Service.
type Config struct {
	// Root is the local directory the service operates in. The service
	// creates Root/inbox and Root/managed if they do not exist.
	Root string

	// Interval is how often the inbox is scanned. Zero means DefaultInterval.
	Interval time.Duration

	// Syncer pushes managed dirs to remote storage. Nil means NopSyncer
	// (local-only).
	Syncer Syncer

	// Logger receives one line per insert. Nil means the standard logger.
	Logger *log.Logger
}

// Service is the artifacts store plus its inbox watcher. It implements
// bgservices.Service.
type Service struct {
	root     string
	inbox    string
	store    *Store
	interval time.Duration
	logger   *log.Logger
}

// New validates cfg, creates the inbox and managed directories, and returns a
// ready-to-run Service.
func New(cfg Config) (*Service, error) {
	if cfg.Root == "" {
		return nil, fmt.Errorf("artifacts: root directory is required")
	}
	root, err := filepath.Abs(cfg.Root)
	if err != nil {
		return nil, fmt.Errorf("artifacts: resolve root: %w", err)
	}

	s := &Service{
		root:     root,
		inbox:    filepath.Join(root, InboxDir),
		interval: cfg.Interval,
		logger:   cfg.Logger,
	}
	if s.interval <= 0 {
		s.interval = DefaultInterval
	}
	if s.logger == nil {
		s.logger = log.Default()
	}
	if err := os.MkdirAll(s.inbox, 0o755); err != nil {
		return nil, fmt.Errorf("artifacts: create %s: %w", s.inbox, err)
	}

	s.store, err = NewStore(filepath.Join(root, ManagedDir), cfg.Syncer, s.logger)
	if err != nil {
		return nil, err
	}
	return s, nil
}

// Name implements bgservices.Service.
func (s *Service) Name() string { return "artifacts" }

// Root returns the absolute root directory the service operates in.
func (s *Service) Root() string { return s.root }

// Store exposes the underlying managed store.
func (s *Service) Store() *Store { return s.store }

// Insert moves the referenced files into a fresh managed dir, force-syncs it,
// and returns the managed dir id. See Store.Insert.
func (s *Service) Insert(ctx context.Context, paths ...string) (ID, error) {
	return s.store.Insert(ctx, paths...)
}

// Open serves one file of a managed dir, from local storage or the remote
// fallback. See Store.Open.
func (s *Service) Open(ctx context.Context, id ID, name string) (io.ReadCloser, error) {
	return s.store.Open(ctx, id, name)
}

// Run polls the inbox until ctx is canceled, inserting every file that
// appears there. Per-file errors are logged and do not stop the loop; Run
// only returns when ctx ends.
func (s *Service) Run(ctx context.Context) error {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	for {
		if _, err := s.DrainInbox(ctx); err != nil {
			s.logger.Printf("std_artifacts: inbox: %v", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// DrainInbox scans the inbox a single time and inserts every regular file
// into the managed store, one managed dir per file. It returns the number of
// files inserted. Hidden files (dotfiles) and subdirectories are left in
// place.
func (s *Service) DrainInbox(ctx context.Context) (int, error) {
	entries, err := os.ReadDir(s.inbox)
	if err != nil {
		return 0, fmt.Errorf("read inbox: %w", err)
	}

	inserted := 0
	var firstErr error
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || strings.HasPrefix(name, ".") {
			continue
		}
		id, err := s.store.Insert(ctx, filepath.Join(s.inbox, name))
		if err != nil {
			s.logger.Printf("std_artifacts: insert %s: %v", name, err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		inserted++
		s.logger.Printf("std_artifacts: inserted %s -> managed/%s", name, id)
	}
	return inserted, firstErr
}

// Verify is a fast end-to-end self-check: it runs a full insert cycle
// (insert -> monotonic id -> sync -> read back) in a throwaway temp store and
// confirms the configured directories are usable. It uses a local-only
// syncer, so it proves the store pipeline, not remote connectivity.
func (s *Service) Verify(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	tmp, err := os.MkdirTemp("", "std_artifacts_verify_")
	if err != nil {
		return fmt.Errorf("artifacts verify: temp dir: %w", err)
	}
	defer os.RemoveAll(tmp)

	probe, err := NewStore(filepath.Join(tmp, ManagedDir), NopSyncer{}, log.New(io.Discard, "", 0))
	if err != nil {
		return fmt.Errorf("artifacts verify: %w", err)
	}
	const content = "std_artifacts verify probe"
	src := filepath.Join(tmp, "probe.txt")
	if err := os.WriteFile(src, []byte(content), 0o644); err != nil {
		return fmt.Errorf("artifacts verify: write probe: %w", err)
	}

	id, err := probe.Insert(ctx, src)
	if err != nil {
		return fmt.Errorf("artifacts verify: %w", err)
	}
	id2, err := probe.Insert(ctx, mustWrite(tmp, "probe2.txt", content))
	if err != nil {
		return fmt.Errorf("artifacts verify: %w", err)
	}
	if id2 != id+1 {
		return fmt.Errorf("artifacts verify: ids not monotonic (%s then %s)", id, id2)
	}

	r, err := probe.Open(ctx, id, "probe.txt")
	if err != nil {
		return fmt.Errorf("artifacts verify: open managed file: %w", err)
	}
	defer r.Close()
	got, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("artifacts verify: read managed file: %w", err)
	}
	if !bytes.Equal(got, []byte(content)) {
		return fmt.Errorf("artifacts verify: managed file corrupted")
	}

	for _, dir := range []string{s.inbox, s.store.Dir()} {
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			return fmt.Errorf("artifacts verify: %s is not a usable directory (%v)", dir, err)
		}
	}
	return nil
}

// mustWrite writes a throwaway probe file and returns its path; errors
// surface later as insert failures.
func mustWrite(dir, name, content string) string {
	path := filepath.Join(dir, name)
	_ = os.WriteFile(path, []byte(content), 0o644)
	return path
}
