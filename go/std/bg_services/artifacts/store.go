package artifacts

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// ID identifies one managed artifact directory. IDs are monotonically
// increasing and are never reused, even across process restarts.
type ID uint64

func (id ID) String() string { return strconv.FormatUint(uint64(id), 10) }

// Syncer pushes managed dirs to durable remote storage and fetches files back
// when the local copy is gone. The real implementation targets Google Drive;
// NopSyncer gives local-only operation and fast tests.
type Syncer interface {
	// ForceSync makes dir durable remotely, blocking until it is. Insert
	// only returns an ID after ForceSync succeeds.
	ForceSync(ctx context.Context, dir string) error
	// Fetch streams one file of a managed dir from remote storage. Used as
	// the fallback when the local copy is missing.
	Fetch(ctx context.Context, id ID, name string) (io.ReadCloser, error)
}

// NopSyncer is a local-only Syncer: ForceSync succeeds immediately and Fetch
// always fails, because there is no remote.
type NopSyncer struct{}

// ForceSync implements Syncer.
func (NopSyncer) ForceSync(context.Context, string) error { return nil }

// Fetch implements Syncer.
func (NopSyncer) Fetch(_ context.Context, id ID, name string) (io.ReadCloser, error) {
	return nil, fmt.Errorf("artifacts: no remote copy of %s/%s (local-only syncer)", id, name)
}

// counterFile persists the last allocated ID under the managed dir, so IDs
// stay monotonic even if managed subdirectories are deleted locally.
const counterFile = ".last_id"

// Store owns the global managed/ directory. Insert moves files from anywhere
// on disk into a fresh managed/<id>/ subdirectory, force-syncs it, and only
// then hands out the id. Open serves a file from local storage, falling back
// to the remote when the local copy is gone.
type Store struct {
	dir    string
	syncer Syncer
	logger *log.Logger

	mu   sync.Mutex
	last ID
}

// NewStore creates dir if needed and recovers the ID counter from the
// counter file and the existing subdirectories, whichever is further along.
func NewStore(dir string, syncer Syncer, logger *log.Logger) (*Store, error) {
	if dir == "" {
		return nil, fmt.Errorf("artifacts: managed directory is required")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("artifacts: resolve managed dir: %w", err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, fmt.Errorf("artifacts: create %s: %w", abs, err)
	}
	if syncer == nil {
		syncer = NopSyncer{}
	}
	if logger == nil {
		logger = log.Default()
	}

	st := &Store{dir: abs, syncer: syncer, logger: logger}
	if err := st.recoverLastID(); err != nil {
		return nil, err
	}
	return st, nil
}

// Dir returns the absolute managed directory.
func (st *Store) Dir() string { return st.dir }

func (st *Store) recoverLastID() error {
	if b, err := os.ReadFile(filepath.Join(st.dir, counterFile)); err == nil {
		if v, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64); err == nil {
			st.last = ID(v)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("artifacts: read id counter: %w", err)
	}

	entries, err := os.ReadDir(st.dir)
	if err != nil {
		return fmt.Errorf("artifacts: scan managed dir: %w", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if v, err := strconv.ParseUint(e.Name(), 10, 64); err == nil && ID(v) > st.last {
			st.last = ID(v)
		}
	}
	return nil
}

// allocateID reserves the next monotonic ID and durably records it before
// returning.
func (st *Store) allocateID() (ID, error) {
	st.mu.Lock()
	defer st.mu.Unlock()

	next := st.last + 1
	path := filepath.Join(st.dir, counterFile)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(next.String()+"\n"), 0o644); err != nil {
		return 0, fmt.Errorf("artifacts: persist id counter: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return 0, fmt.Errorf("artifacts: persist id counter: %w", err)
	}
	st.last = next
	return next, nil
}

// Insert moves the referenced on-disk files into a fresh managed/<id>/
// subdirectory, force-syncs that dir to remote storage, and only then returns
// the managed dir id. The source files are consumed (moved, not copied). If
// the sync fails the files stay safe in the managed dir, but no id is handed
// out and the error reports the affected dir.
func (st *Store) Insert(ctx context.Context, paths ...string) (ID, error) {
	if len(paths) == 0 {
		return 0, fmt.Errorf("artifacts: insert needs at least one file")
	}
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			return 0, fmt.Errorf("artifacts: insert %s: %w", p, err)
		}
		if !info.Mode().IsRegular() {
			return 0, fmt.Errorf("artifacts: insert %s: not a regular file", p)
		}
	}

	id, err := st.allocateID()
	if err != nil {
		return 0, err
	}
	dir := filepath.Join(st.dir, id.String())
	if err := os.Mkdir(dir, 0o755); err != nil {
		return 0, fmt.Errorf("artifacts: create %s: %w", dir, err)
	}

	for _, p := range paths {
		dst := filepath.Join(dir, availableName(dir, filepath.Base(p)))
		if err := moveFile(p, dst); err != nil {
			return 0, fmt.Errorf("artifacts: insert %s into %s: %w", p, dir, err)
		}
	}

	if err := st.syncer.ForceSync(ctx, dir); err != nil {
		return 0, fmt.Errorf("artifacts: %s written locally but force sync failed: %w", dir, err)
	}
	return id, nil
}

// Path returns where a managed file lives in local storage.
func (st *Store) Path(id ID, name string) string {
	return filepath.Join(st.dir, id.String(), name)
}

// Open serves one file of a managed dir: from local storage when present,
// otherwise falling back to the remote via the Syncer.
func (st *Store) Open(ctx context.Context, id ID, name string) (io.ReadCloser, error) {
	f, err := os.Open(st.Path(id, name))
	if err == nil {
		return f, nil
	}
	if !os.IsNotExist(err) {
		return nil, err
	}
	st.logger.Printf("std_artifacts: %s/%s not in local storage, fetching from remote", id, name)
	return st.syncer.Fetch(ctx, id, name)
}

// moveFile renames src to dst, falling back to copy+remove when src lives on
// a different filesystem than the managed dir.
func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Remove(src)
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
