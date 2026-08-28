package artifacts

import (
	"context"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeSyncer records ForceSync calls and can be told to fail or to serve
// remote content for Fetch.
type fakeSyncer struct {
	synced  []string          // dirs passed to ForceSync
	failErr error             // returned by ForceSync when set
	remote  map[string]string // "id/name" -> content served by Fetch
}

func (f *fakeSyncer) ForceSync(_ context.Context, dir string) error {
	if f.failErr != nil {
		return f.failErr
	}
	f.synced = append(f.synced, dir)
	return nil
}

func (f *fakeSyncer) Fetch(_ context.Context, id ID, name string) (io.ReadCloser, error) {
	content, ok := f.remote[id.String()+"/"+name]
	if !ok {
		return nil, os.ErrNotExist
	}
	return io.NopCloser(strings.NewReader(content)), nil
}

func newTestStore(t *testing.T, syncer Syncer) *Store {
	t.Helper()
	st, err := NewStore(filepath.Join(t.TempDir(), ManagedDir), syncer, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return st
}

func TestInsertMovesFilesAndSyncsBeforeReturningID(t *testing.T) {
	syncer := &fakeSyncer{}
	st := newTestStore(t, syncer)
	ctx := context.Background()

	src := t.TempDir()
	a := writeFile(t, filepath.Join(src, "a.txt"), "alpha")
	b := writeFile(t, filepath.Join(src, "b.txt"), "bravo")

	id, err := st.Insert(ctx, a, b)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if id != 1 {
		t.Errorf("id = %s, want 1", id)
	}

	// Sources are consumed.
	for _, p := range []string{a, b} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("source %s should be gone: %v", p, err)
		}
	}
	// Both files landed in the one managed dir.
	for name, want := range map[string]string{"a.txt": "alpha", "b.txt": "bravo"} {
		got, err := os.ReadFile(st.Path(id, name))
		if err != nil || string(got) != want {
			t.Errorf("managed %s = %q (err=%v), want %q", name, got, err, want)
		}
	}
	// Force sync ran for exactly this dir.
	wantDir := filepath.Join(st.Dir(), id.String())
	if len(syncer.synced) != 1 || syncer.synced[0] != wantDir {
		t.Errorf("synced dirs = %v, want [%s]", syncer.synced, wantDir)
	}
}

func TestInsertIDsAreMonotonicAcrossRestarts(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ManagedDir)
	ctx := context.Background()
	quiet := log.New(io.Discard, "", 0)

	st, err := NewStore(dir, nil, quiet)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	src := t.TempDir()
	id1, err := st.Insert(ctx, writeFile(t, filepath.Join(src, "one"), "1"))
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}

	// Simulate the managed dir being evicted locally after a Drive sync:
	// the counter file must still keep ids monotonic.
	if err := os.RemoveAll(filepath.Join(dir, id1.String())); err != nil {
		t.Fatal(err)
	}

	st2, err := NewStore(dir, nil, quiet)
	if err != nil {
		t.Fatalf("NewStore (restart): %v", err)
	}
	id2, err := st2.Insert(ctx, writeFile(t, filepath.Join(src, "two"), "2"))
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if id2 != id1+1 {
		t.Errorf("id after restart = %s, want %s", id2, id1+1)
	}
}

func TestInsertFailedSyncReturnsNoID(t *testing.T) {
	syncer := &fakeSyncer{failErr: errors.New("drive unreachable")}
	st := newTestStore(t, syncer)

	src := writeFile(t, filepath.Join(t.TempDir(), "a.txt"), "alpha")
	if _, err := st.Insert(context.Background(), src); err == nil {
		t.Fatal("expected error when force sync fails")
	}
	// The file is still safe in the managed dir, just not handed out.
	got, err := os.ReadFile(st.Path(1, "a.txt"))
	if err != nil || string(got) != "alpha" {
		t.Errorf("managed a.txt = %q (err=%v), want kept locally", got, err)
	}
}

func TestInsertRejectsMissingAndIrregularSources(t *testing.T) {
	st := newTestStore(t, nil)
	ctx := context.Background()

	if _, err := st.Insert(ctx); err == nil {
		t.Error("expected error for empty insert")
	}
	if _, err := st.Insert(ctx, filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("expected error for missing source")
	}
	if _, err := st.Insert(ctx, t.TempDir()); err == nil {
		t.Error("expected error for directory source")
	}
}

func TestInsertResolvesBasenameCollisions(t *testing.T) {
	st := newTestStore(t, nil)
	dirA, dirB := t.TempDir(), t.TempDir()
	a := writeFile(t, filepath.Join(dirA, "report.txt"), "first")
	b := writeFile(t, filepath.Join(dirB, "report.txt"), "second")

	id, err := st.Insert(context.Background(), a, b)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if got, _ := os.ReadFile(st.Path(id, "report.txt")); string(got) != "first" {
		t.Errorf("report.txt = %q, want %q", got, "first")
	}
	if got, _ := os.ReadFile(st.Path(id, "report-1.txt")); string(got) != "second" {
		t.Errorf("report-1.txt = %q, want %q", got, "second")
	}
}

func TestOpenServesLocalThenFallsBackToRemote(t *testing.T) {
	syncer := &fakeSyncer{remote: map[string]string{}}
	st := newTestStore(t, syncer)
	ctx := context.Background()

	src := writeFile(t, filepath.Join(t.TempDir(), "a.txt"), "local copy")
	id, err := st.Insert(ctx, src)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}

	// Served from local storage while present.
	r, err := st.Open(ctx, id, "a.txt")
	if err != nil {
		t.Fatalf("Open (local): %v", err)
	}
	got, _ := io.ReadAll(r)
	r.Close()
	if string(got) != "local copy" {
		t.Errorf("local read = %q", got)
	}

	// Evict the local copy; Open must fall back to the remote.
	syncer.remote[id.String()+"/a.txt"] = "remote copy"
	if err := os.Remove(st.Path(id, "a.txt")); err != nil {
		t.Fatal(err)
	}
	r, err = st.Open(ctx, id, "a.txt")
	if err != nil {
		t.Fatalf("Open (fallback): %v", err)
	}
	got, _ = io.ReadAll(r)
	r.Close()
	if string(got) != "remote copy" {
		t.Errorf("fallback read = %q, want remote copy", got)
	}

	// Missing everywhere is an error.
	if _, err := st.Open(ctx, id, "nope.txt"); err == nil {
		t.Error("expected error for file missing locally and remotely")
	}
}
