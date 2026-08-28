// Package artifacts is the std_artifacts background service: it watches
// <root>/inbox and moves every file that appears there into <root>/synced.
// Filesystem only — no network, no database.
package artifacts

import (
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
	// InboxDir is the subdirectory of Root that is watched for new files.
	InboxDir = "inbox"
	// SyncedDir is the subdirectory of Root that files are moved into.
	SyncedDir = "synced"

	// DefaultInterval is the poll interval used when Config.Interval is zero.
	DefaultInterval = time.Second
)

// Config configures a Service.
type Config struct {
	// Root is the local directory the service operates in. The service
	// creates Root/inbox and Root/synced if they do not exist.
	Root string

	// Interval is how often the inbox is scanned. Zero means DefaultInterval.
	Interval time.Duration

	// Logger receives one line per moved file. Nil means the standard logger.
	Logger *log.Logger
}

// Service moves files from Root/inbox to Root/synced. It implements
// bgservices.Service.
type Service struct {
	root     string
	inbox    string
	synced   string
	interval time.Duration
	logger   *log.Logger
}

// New validates cfg, creates the inbox and synced directories, and returns a
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
		synced:   filepath.Join(root, SyncedDir),
		interval: cfg.Interval,
		logger:   cfg.Logger,
	}
	if s.interval <= 0 {
		s.interval = DefaultInterval
	}
	if s.logger == nil {
		s.logger = log.Default()
	}

	for _, dir := range []string{s.inbox, s.synced} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("artifacts: create %s: %w", dir, err)
		}
	}
	return s, nil
}

// Name implements bgservices.Service.
func (s *Service) Name() string { return "artifacts" }

// Root returns the absolute root directory the service operates in.
func (s *Service) Root() string { return s.root }

// Run polls the inbox until ctx is canceled. It performs one sync immediately
// on start, then one per interval. Per-file errors are logged and do not stop
// the loop; Run only returns when ctx ends.
func (s *Service) Run(ctx context.Context) error {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	for {
		if _, err := s.SyncOnce(); err != nil {
			s.logger.Printf("std_artifacts: sync: %v", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// Verify is a fast end-to-end self-check: it runs a full inbox -> synced
// cycle in a throwaway temp directory and confirms the file arrives intact.
func (s *Service) Verify(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	tmp, err := os.MkdirTemp("", "std_artifacts_verify_")
	if err != nil {
		return fmt.Errorf("artifacts verify: temp dir: %w", err)
	}
	defer os.RemoveAll(tmp)

	probe, err := New(Config{Root: tmp, Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		return fmt.Errorf("artifacts verify: %w", err)
	}
	const name, content = "probe.txt", "std_artifacts verify probe"
	if err := os.WriteFile(filepath.Join(probe.inbox, name), []byte(content), 0o644); err != nil {
		return fmt.Errorf("artifacts verify: write probe: %w", err)
	}
	if _, err := probe.SyncOnce(); err != nil {
		return fmt.Errorf("artifacts verify: sync: %w", err)
	}
	got, err := os.ReadFile(filepath.Join(probe.synced, name))
	if err != nil {
		return fmt.Errorf("artifacts verify: probe not synced: %w", err)
	}
	if string(got) != content {
		return fmt.Errorf("artifacts verify: probe corrupted after sync")
	}

	// Also confirm the configured root is actually usable.
	for _, dir := range []string{s.inbox, s.synced} {
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			return fmt.Errorf("artifacts verify: %s is not a usable directory (%v)", dir, err)
		}
	}
	return nil
}

// SyncOnce scans the inbox a single time and moves every regular file into
// synced. It returns the number of files moved. Hidden files (dotfiles) and
// subdirectories are left in place.
func (s *Service) SyncOnce() (int, error) {
	entries, err := os.ReadDir(s.inbox)
	if err != nil {
		return 0, fmt.Errorf("read inbox: %w", err)
	}

	moved := 0
	var firstErr error
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || strings.HasPrefix(name, ".") {
			continue
		}
		dst, err := s.moveFile(name)
		if err != nil {
			s.logger.Printf("std_artifacts: move %s: %v", name, err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		moved++
		s.logger.Printf("std_artifacts: synced %s -> %s", name, filepath.Base(dst))
	}
	return moved, firstErr
}

// moveFile renames inbox/name into synced, picking a non-colliding
// destination name, and returns the destination path.
func (s *Service) moveFile(name string) (string, error) {
	src := filepath.Join(s.inbox, name)
	dst := filepath.Join(s.synced, availableName(s.synced, name))
	if err := os.Rename(src, dst); err != nil {
		return "", err
	}
	return dst, nil
}

// availableName returns name if it is free in dir, otherwise name with a
// numeric suffix before the extension (report.txt -> report-1.txt).
func availableName(dir, name string) string {
	if _, err := os.Lstat(filepath.Join(dir, name)); os.IsNotExist(err) {
		return name
	}
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	for i := 1; ; i++ {
		candidate := fmt.Sprintf("%s-%d%s", base, i, ext)
		if _, err := os.Lstat(filepath.Join(dir, candidate)); os.IsNotExist(err) {
			return candidate
		}
	}
}
