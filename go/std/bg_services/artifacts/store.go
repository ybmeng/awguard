package artifacts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ID identifies one managed artifact directory. IDs are monotonically
// increasing and are never reused, even across process restarts.
type ID uint64

func (id ID) String() string { return strconv.FormatUint(uint64(id), 10) }

// Syncer performs the remote stages of the insert state machine and the
// serving fallback. The real implementation targets Google Drive; NopSyncer
// gives local-only operation and fast tests.
type Syncer interface {
	// CreateDir performs stage 2: create the remote dir structure for a
	// managed dir and return its remote identifier.
	CreateDir(ctx context.Context, id ID) (remoteDir string, err error)
	// SyncFile performs one upload of stage 3, blocking until the remote
	// acknowledged it, and returns the static reference to the file.
	SyncFile(ctx context.Context, remoteDir, localPath string) (FileRef, error)
	// Fetch streams a file back from remote storage by its static
	// reference. Used as the fallback when the local copy is gone.
	Fetch(ctx context.Context, ref FileRef) (io.ReadCloser, error)
}

// NopSyncer is a local-only Syncer: remote stages succeed without doing
// anything remote (refs carry an empty RemoteID) and Fetch always fails.
type NopSyncer struct{}

// CreateDir implements Syncer.
func (NopSyncer) CreateDir(_ context.Context, id ID) (string, error) { return "local", nil }

// SyncFile implements Syncer.
func (NopSyncer) SyncFile(_ context.Context, _, localPath string) (FileRef, error) {
	size, sum, err := hashFile(localPath)
	if err != nil {
		return FileRef{}, err
	}
	return FileRef{Name: filepath.Base(localPath), Size: size, SHA256: sum}, nil
}

// Fetch implements Syncer.
func (NopSyncer) Fetch(_ context.Context, ref FileRef) (io.ReadCloser, error) {
	return nil, fmt.Errorf("artifacts: no remote copy of %s (local-only syncer)", ref.Name)
}

// counterFile persists the last allocated ID under the managed dir, so IDs
// stay monotonic even if managed subdirectories are deleted locally.
const counterFile = ".last_id"

// Store owns the global managed/ directory. Insert runs the state machine:
// tag WIP, move files in, create the remote dir, sync files, write static
// refs, complete — handing out the id only on COMPLETE. Open serves a file
// from local storage, falling back to the remote via its static reference.
type Store struct {
	dir    string
	syncer Syncer
	logger *log.Logger

	mu   sync.Mutex
	last ID
}

// NewStore creates dir if needed and recovers the ID counter. It never
// mutates existing managed dirs: sweeping interrupted inserts is the job of
// the one process that owns the store (see SweepInterrupted), so read-path
// stores (stdd ls/cat/insert in direct mode) cannot damage a live service's
// in-flight work.
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

	ids, err := st.ids()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if id > st.last {
			st.last = id
		}
	}
	return nil
}

// SweepInterrupted converts dirs still tagged WIP by a previous process into
// ERR: an interrupted insert is a failure, and failures are irrecoverable
// for now. The one exception is a dir interrupted between REFS and COMPLETE:
// every stage already succeeded and the static refs are on disk, so the sweep
// finishes the machine's last step (remove the WIP tag) instead of burning
// the insert.
//
// Only the process that exclusively owns the store (the running service) may
// call this: a WIP marker is indistinguishable from another process's live
// insert, so sweeping from a casually constructed Store would convert
// in-flight work into terminal ERR.
func (st *Store) SweepInterrupted() error {
	ids, err := st.ids()
	if err != nil {
		return err
	}
	for _, id := range ids {
		dir := filepath.Join(st.dir, id.String())
		wip := filepath.Join(dir, wipMarker)
		m, err := readMarker(wip)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			m = marker{Stage: StageInit, StartedAt: time.Now().UTC()}
		}
		if m.Stage == StageRefs && validRefs(filepath.Join(dir, refsFile), id) {
			if err := os.Remove(wip); err != nil {
				return fmt.Errorf("artifacts: sweep %s: %w", dir, err)
			}
			st.logger.Printf("std_artifacts: promoted interrupted managed/%s to COMPLETE (refs were already written)", id)
			continue
		}
		m.Error = fmt.Sprintf("interrupted: process exited during stage %s", m.Stage)
		m.UpdatedAt = time.Now().UTC()
		if err := writeJSONAtomic(filepath.Join(dir, errMarker), m); err != nil {
			return fmt.Errorf("artifacts: sweep %s: %w", dir, err)
		}
		if err := os.Remove(wip); err != nil {
			return fmt.Errorf("artifacts: sweep %s: %w", dir, err)
		}
		st.logger.Printf("std_artifacts: swept interrupted managed/%s to ERR (was at stage %s)", id, m.Stage)
	}
	return nil
}

// validRefs reports whether path holds a parseable stage-4 refs file for id —
// the proof that every stage of the insert succeeded.
func validRefs(path string, id ID) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var refs Refs
	return json.Unmarshal(b, &refs) == nil && refs.ID == id
}

// ids lists the numeric managed subdirectories in ascending order.
func (st *Store) ids() ([]ID, error) {
	entries, err := os.ReadDir(st.dir)
	if err != nil {
		return nil, fmt.Errorf("artifacts: scan managed dir: %w", err)
	}
	var ids []ID
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if v, err := strconv.ParseUint(e.Name(), 10, 64); err == nil {
			ids = append(ids, ID(v))
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids, nil
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

// machine drives the insert state machine of one managed dir.
type machine struct {
	id      ID
	dir     string
	started time.Time
}

// setStage records the machine's current stage in the WIP marker.
func (m *machine) setStage(stage Stage) error {
	return writeJSONAtomic(filepath.Join(m.dir, wipMarker), marker{
		Stage:     stage,
		StartedAt: m.started,
		UpdatedAt: time.Now().UTC(),
	})
}

// fail moves the machine into the terminal ERR state, recording the stage
// whose transition failed, and returns the error Insert surfaces.
func (m *machine) fail(stage Stage, cause error) error {
	if err := writeJSONAtomic(filepath.Join(m.dir, errMarker), marker{
		Stage:     stage,
		Error:     cause.Error(),
		StartedAt: m.started,
		UpdatedAt: time.Now().UTC(),
	}); err != nil {
		cause = fmt.Errorf("%w (and writing the err marker failed: %v)", cause, err)
	}
	_ = os.Remove(filepath.Join(m.dir, wipMarker))
	return fmt.Errorf("artifacts: insert %s failed at stage %s: %w", m.id, stage, cause)
}

// step runs one transition: fn's failure is terminal (ERR), success advances
// the WIP marker to next.
func (m *machine) step(next Stage, fn func() error) error {
	if err := fn(); err != nil {
		return m.fail(next, err)
	}
	if err := m.setStage(next); err != nil {
		return m.fail(next, err)
	}
	return nil
}

// Insert runs the state machine for the referenced on-disk files:
//
//	INIT (dir + WIP tag) -> MOVED -> REMOTE_DIR -> SYNCED -> REFS -> COMPLETE
//
// The managed dir id is returned only on COMPLETE. Any failure lands the dir
// in ERR (terminal, irrecoverable for now) and Insert returns the error with
// no id. The source files are consumed (moved, not copied) from stage 1 on.
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
	m := &machine{id: id, dir: dir, started: time.Now().UTC()}
	if err := m.setStage(StageInit); err != nil {
		return 0, m.fail(StageInit, err)
	}

	// Stage 1: move the insert files into the managed dir.
	if err := m.step(StageMoved, func() error {
		for _, p := range paths {
			dst := filepath.Join(dir, availableName(dir, filepath.Base(p)))
			if err := moveFile(p, dst); err != nil {
				return fmt.Errorf("move %s: %w", p, err)
			}
		}
		return nil
	}); err != nil {
		return 0, err
	}

	// Stage 2: create the remote dir structure.
	var remoteDir string
	if err := m.step(StageRemoteDir, func() error {
		var err error
		remoteDir, err = st.syncer.CreateDir(ctx, id)
		return err
	}); err != nil {
		return 0, err
	}

	// Stage 3: sync every file, collecting its static reference.
	var refs []FileRef
	if err := m.step(StageSynced, func() error {
		names, err := contentFiles(dir)
		if err != nil {
			return err
		}
		for _, name := range names {
			ref, err := st.syncer.SyncFile(ctx, remoteDir, filepath.Join(dir, name))
			if err != nil {
				return fmt.Errorf("sync %s: %w", name, err)
			}
			refs = append(refs, ref)
		}
		return nil
	}); err != nil {
		return 0, err
	}

	// Stage 4: write the static references into the dir.
	if err := m.step(StageRefs, func() error {
		return writeJSONAtomic(filepath.Join(dir, refsFile), Refs{
			ID:        id,
			RemoteDir: remoteDir,
			SyncedAt:  time.Now().UTC(),
			Files:     refs,
		})
	}); err != nil {
		return 0, err
	}

	// Stage 5: COMPLETE — remove the WIP tag and hand out the id.
	if err := os.Remove(filepath.Join(dir, wipMarker)); err != nil {
		return 0, m.fail(StageComplete, err)
	}
	return id, nil
}

// Status reports the externally visible state of one managed dir.
func (st *Store) Status(id ID) (Status, error) {
	dir := filepath.Join(st.dir, id.String())
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return Status{}, fmt.Errorf("artifacts: managed dir %s not found", id)
	}

	if m, err := readMarker(filepath.Join(dir, errMarker)); err == nil {
		return Status{
			ID:        id,
			Stage:     StageErr,
			Error:     fmt.Sprintf("stage %s: %s", m.Stage, m.Error),
			UpdatedAt: m.UpdatedAt,
		}, nil
	}
	if m, err := readMarker(filepath.Join(dir, wipMarker)); err == nil {
		return Status{ID: id, Stage: m.Stage, UpdatedAt: m.UpdatedAt}, nil
	}
	if info, err := os.Stat(filepath.Join(dir, refsFile)); err == nil {
		return Status{ID: id, Stage: StageComplete, UpdatedAt: info.ModTime()}, nil
	}
	return Status{
		ID:    id,
		Stage: StageErr,
		Error: "no state markers (legacy or corrupted dir)",
	}, nil
}

// List reports the status of every managed dir, in ascending id order.
func (st *Store) List() ([]Status, error) {
	ids, err := st.ids()
	if err != nil {
		return nil, err
	}
	statuses := make([]Status, 0, len(ids))
	for _, id := range ids {
		s, err := st.Status(id)
		if err != nil {
			return nil, err
		}
		statuses = append(statuses, s)
	}
	return statuses, nil
}

// Refs returns the static references of a COMPLETE managed dir.
func (st *Store) Refs(id ID) (Refs, error) {
	s, err := st.Status(id)
	if err != nil {
		return Refs{}, err
	}
	if s.Stage != StageComplete {
		return Refs{}, fmt.Errorf("artifacts: managed dir %s is not complete (stage %s)", id, s.Stage)
	}
	b, err := os.ReadFile(filepath.Join(st.dir, id.String(), refsFile))
	if err != nil {
		return Refs{}, fmt.Errorf("artifacts: read refs of %s: %w", id, err)
	}
	var refs Refs
	if err := json.Unmarshal(b, &refs); err != nil {
		return Refs{}, fmt.Errorf("artifacts: parse refs of %s: %w", id, err)
	}
	return refs, nil
}

// Path returns where a managed file lives in local storage.
func (st *Store) Path(id ID, name string) string {
	return filepath.Join(st.dir, id.String(), name)
}

// validName reports whether name is a plain file name — non-empty, no path
// separators, no traversal — so joining it under a managed dir cannot escape
// that dir.
func validName(name string) bool {
	return name != "" && name != "." && name != ".." &&
		!strings.ContainsAny(name, `/\`) && name == filepath.Base(name)
}

// Open serves one file of a COMPLETE managed dir: from local storage when
// present, otherwise fetched from the remote by its static reference.
// Dirs in any other stage (WIP or ERR) are not servable. Names with path
// separators or traversal are rejected outright.
func (st *Store) Open(ctx context.Context, id ID, name string) (io.ReadCloser, error) {
	if !validName(name) {
		return nil, fmt.Errorf("artifacts: invalid file name %q", name)
	}
	s, err := st.Status(id)
	if err != nil {
		return nil, err
	}
	if s.Stage != StageComplete {
		return nil, fmt.Errorf("artifacts: managed dir %s is not servable (stage %s)", id, s.Stage)
	}

	f, err := os.Open(st.Path(id, name))
	if err == nil {
		return f, nil
	}
	if !os.IsNotExist(err) {
		return nil, err
	}

	refs, err := st.Refs(id)
	if err != nil {
		return nil, err
	}
	ref, ok := refs.Find(name)
	if !ok {
		return nil, fmt.Errorf("artifacts: %s/%s is not a file of this managed dir", id, name)
	}
	st.logger.Printf("std_artifacts: %s/%s not in local storage, fetching by remote ref %q", id, name, ref.RemoteID)
	return st.syncer.Fetch(ctx, ref)
}

// contentFiles lists the regular, non-hidden files of a managed dir — the
// files stage 3 syncs; markers and refs stay local.
func contentFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		names = append(names, e.Name())
	}
	return names, nil
}

// hashFile returns the size and hex SHA-256 of a file.
func hashFile(path string) (int64, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, "", err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return 0, "", err
	}
	return n, hex.EncodeToString(h.Sum(nil)), nil
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
