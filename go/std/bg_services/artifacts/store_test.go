package artifacts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeSyncer records the order of remote-stage calls and can be told to fail
// stage 2 or stage 3. Synced content is kept in memory, keyed by the
// RemoteID it hands out, so Fetch serves it back by static reference.
type fakeSyncer struct {
	calls      []string
	failCreate error
	failSync   error
	remote     map[string]string // RemoteID -> content
}

func newFakeSyncer() *fakeSyncer { return &fakeSyncer{remote: map[string]string{}} }

func (f *fakeSyncer) CreateDir(_ context.Context, id ID) (string, error) {
	if f.failCreate != nil {
		return "", f.failCreate
	}
	f.calls = append(f.calls, "createdir:"+id.String())
	return "rdir-" + id.String(), nil
}

func (f *fakeSyncer) SyncFile(_ context.Context, remoteDir, localPath string) (FileRef, error) {
	if f.failSync != nil {
		return FileRef{}, f.failSync
	}
	name := filepath.Base(localPath)
	f.calls = append(f.calls, "syncfile:"+remoteDir+"/"+name)
	content, err := os.ReadFile(localPath)
	if err != nil {
		return FileRef{}, err
	}
	remoteID := "rid-" + remoteDir + "-" + name
	f.remote[remoteID] = string(content)
	sum := sha256.Sum256(content)
	return FileRef{Name: name, RemoteID: remoteID, Size: int64(len(content)), SHA256: hex.EncodeToString(sum[:])}, nil
}

func (f *fakeSyncer) Fetch(_ context.Context, ref FileRef) (io.ReadCloser, error) {
	content, ok := f.remote[ref.RemoteID]
	if !ok {
		return nil, fmt.Errorf("no remote content for %q", ref.RemoteID)
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

func TestInsertHappyPathWalksStages(t *testing.T) {
	syncer := newFakeSyncer()
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

	// Remote stages ran in machine order: stage 2 before any stage 3 call,
	// and no marker files were synced.
	want := []string{"createdir:1", "syncfile:rdir-1/a.txt", "syncfile:rdir-1/b.txt"}
	if fmt.Sprint(syncer.calls) != fmt.Sprint(want) {
		t.Errorf("remote calls = %v, want %v", syncer.calls, want)
	}

	// Sources consumed, files in the managed dir.
	for _, p := range []string{a, b} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("source %s should be gone: %v", p, err)
		}
	}

	// COMPLETE: WIP removed, no ERR, refs present and static.
	dir := filepath.Join(st.Dir(), id.String())
	for _, marker := range []string{wipMarker, errMarker} {
		if _, err := os.Stat(filepath.Join(dir, marker)); !os.IsNotExist(err) {
			t.Errorf("%s should not exist after complete: %v", marker, err)
		}
	}
	status, err := st.Status(id)
	if err != nil || status.Stage != StageComplete {
		t.Fatalf("Status = %+v (err=%v), want COMPLETE", status, err)
	}

	refs, err := st.Refs(id)
	if err != nil {
		t.Fatalf("Refs: %v", err)
	}
	if refs.ID != id || refs.RemoteDir != "rdir-1" || len(refs.Files) != 2 {
		t.Fatalf("refs = %+v", refs)
	}
	ref, ok := refs.Find("a.txt")
	wantSum := sha256.Sum256([]byte("alpha"))
	if !ok || ref.RemoteID != "rid-rdir-1-a.txt" || ref.Size != 5 || ref.SHA256 != hex.EncodeToString(wantSum[:]) {
		t.Errorf("a.txt ref = %+v", ref)
	}
}

func TestInsertFailureLandsInErr(t *testing.T) {
	cases := []struct {
		name      string
		configure func(*fakeSyncer)
		wantStage Stage
	}{
		{"stage2_create_remote_dir", func(f *fakeSyncer) { f.failCreate = errors.New("drive down") }, StageRemoteDir},
		{"stage3_sync_files", func(f *fakeSyncer) { f.failSync = errors.New("upload refused") }, StageSynced},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			syncer := newFakeSyncer()
			tc.configure(syncer)
			st := newTestStore(t, syncer)
			ctx := context.Background()

			src := writeFile(t, filepath.Join(t.TempDir(), "a.txt"), "alpha")
			_, err := st.Insert(ctx, src)
			if err == nil {
				t.Fatal("expected insert to fail")
			}
			if !strings.Contains(err.Error(), string(tc.wantStage)) {
				t.Errorf("error %q does not name failed stage %s", err, tc.wantStage)
			}

			// ERR is terminal and visible: .err present, .wip gone.
			status, serr := st.Status(1)
			if serr != nil || status.Stage != StageErr {
				t.Fatalf("Status = %+v (err=%v), want ERR", status, serr)
			}
			if !strings.Contains(status.Error, string(tc.wantStage)) {
				t.Errorf("status error %q does not name stage %s", status.Error, tc.wantStage)
			}
			if _, err := os.Stat(filepath.Join(st.Dir(), "1", wipMarker)); !os.IsNotExist(err) {
				t.Errorf(".wip should be gone in ERR state: %v", err)
			}
			// The moved file stays for inspection, but the dir is not servable.
			if _, err := os.Stat(st.Path(1, "a.txt")); err != nil {
				t.Errorf("moved file should remain in ERR dir: %v", err)
			}
			if _, err := st.Open(ctx, 1, "a.txt"); err == nil {
				t.Error("Open must refuse an ERR dir")
			}
			if _, err := st.Refs(1); err == nil {
				t.Error("Refs must refuse an ERR dir")
			}
		})
	}
}

func TestInsertIDsStayMonotonicPastErrAndRestarts(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ManagedDir)
	quiet := log.New(io.Discard, "", 0)
	ctx := context.Background()

	failing := newFakeSyncer()
	failing.failCreate = errors.New("boom")
	st, err := NewStore(dir, failing, quiet)
	if err != nil {
		t.Fatal(err)
	}
	src := t.TempDir()
	if _, err := st.Insert(ctx, writeFile(t, filepath.Join(src, "one"), "1")); err == nil {
		t.Fatal("expected failure")
	}

	// Restart with a healthy syncer: the ERR dir keeps its id, the next
	// insert gets a fresh one.
	st2, err := NewStore(dir, newFakeSyncer(), quiet)
	if err != nil {
		t.Fatal(err)
	}
	id, err := st2.Insert(ctx, writeFile(t, filepath.Join(src, "two"), "2"))
	if err != nil {
		t.Fatalf("Insert after restart: %v", err)
	}
	if id != 2 {
		t.Errorf("id = %s, want 2 (1 burned by the ERR insert)", id)
	}
}

func TestSweepInterruptedWIPToErr(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ManagedDir)
	quiet := log.New(io.Discard, "", 0)

	// Simulate a process that died during stage 3 of insert 5.
	interrupted := filepath.Join(dir, "5")
	if err := os.MkdirAll(interrupted, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONAtomic(filepath.Join(interrupted, wipMarker), marker{Stage: StageSynced}); err != nil {
		t.Fatal(err)
	}

	st, err := NewStore(dir, newFakeSyncer(), quiet)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := st.SweepInterrupted(); err != nil {
		t.Fatalf("SweepInterrupted: %v", err)
	}
	status, err := st.Status(5)
	if err != nil || status.Stage != StageErr {
		t.Fatalf("Status = %+v (err=%v), want swept to ERR", status, err)
	}
	if !strings.Contains(status.Error, "interrupted") || !strings.Contains(status.Error, string(StageSynced)) {
		t.Errorf("swept error %q should mention interruption at stage synced", status.Error)
	}
	// The burned id is not reused.
	id, err := st.Insert(context.Background(), writeFile(t, filepath.Join(t.TempDir(), "f"), "x"))
	if err != nil {
		t.Fatal(err)
	}
	if id != 6 {
		t.Errorf("next id = %s, want 6", id)
	}
}

func TestSweepPromotesRefsStageWIPToComplete(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ManagedDir)
	quiet := log.New(io.Discard, "", 0)

	// Simulate a process that died between stage REFS and COMPLETE of insert
	// 3: every stage succeeded, .refs.json is valid, only the WIP tag removal
	// never ran.
	interrupted := filepath.Join(dir, "3")
	if err := os.MkdirAll(interrupted, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(interrupted, "a.txt"), "alpha")
	if err := writeJSONAtomic(filepath.Join(interrupted, refsFile), Refs{
		ID:        3,
		RemoteDir: "rdir-3",
		Files:     []FileRef{{Name: "a.txt", RemoteID: "rid-3", Size: 5, SHA256: "ab"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONAtomic(filepath.Join(interrupted, wipMarker), marker{Stage: StageRefs}); err != nil {
		t.Fatal(err)
	}

	st, err := NewStore(dir, newFakeSyncer(), quiet)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := st.SweepInterrupted(); err != nil {
		t.Fatalf("SweepInterrupted: %v", err)
	}
	status, err := st.Status(3)
	if err != nil || status.Stage != StageComplete {
		t.Fatalf("Status = %+v (err=%v), want promoted to COMPLETE", status, err)
	}
	if _, err := os.Stat(filepath.Join(interrupted, wipMarker)); !os.IsNotExist(err) {
		t.Errorf(".wip should be gone after promotion: %v", err)
	}
	if _, err := os.Stat(filepath.Join(interrupted, errMarker)); !os.IsNotExist(err) {
		t.Errorf(".err should not exist after promotion: %v", err)
	}
	// The promoted dir is fully servable.
	r, err := st.Open(context.Background(), 3, "a.txt")
	if err != nil {
		t.Fatalf("Open promoted dir: %v", err)
	}
	defer r.Close()
	if got, _ := io.ReadAll(r); string(got) != "alpha" {
		t.Errorf("content = %q, want alpha", got)
	}
}

func TestSweepDoesNotPromoteRefsStageWithBadRefs(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ManagedDir)
	interrupted := filepath.Join(dir, "4")
	if err := os.MkdirAll(interrupted, 0o755); err != nil {
		t.Fatal(err)
	}
	// WIP says refs, but the refs file is corrupt: not provably complete.
	writeFile(t, filepath.Join(interrupted, refsFile), "{not json")
	if err := writeJSONAtomic(filepath.Join(interrupted, wipMarker), marker{Stage: StageRefs}); err != nil {
		t.Fatal(err)
	}

	st, err := NewStore(dir, newFakeSyncer(), log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := st.SweepInterrupted(); err != nil {
		t.Fatalf("SweepInterrupted: %v", err)
	}
	status, err := st.Status(4)
	if err != nil || status.Stage != StageErr {
		t.Fatalf("Status = %+v (err=%v), want swept to ERR", status, err)
	}
}

func TestNewStoreDoesNotSweepInFlightWIP(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ManagedDir)

	// A live service is mid-insert: managed/9 is tagged WIP at stage synced.
	inFlight := filepath.Join(dir, "9")
	if err := os.MkdirAll(inFlight, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONAtomic(filepath.Join(inFlight, wipMarker), marker{Stage: StageSynced}); err != nil {
		t.Fatal(err)
	}

	// A read-path Store (what stdd ls/cat/insert build in direct mode) must
	// leave the marker untouched.
	st, err := NewStore(dir, newFakeSyncer(), log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if _, err := os.Stat(filepath.Join(inFlight, wipMarker)); err != nil {
		t.Errorf(".wip must survive NewStore: %v", err)
	}
	if _, err := os.Stat(filepath.Join(inFlight, errMarker)); !os.IsNotExist(err) {
		t.Errorf(".err must not appear from NewStore alone: %v", err)
	}
	status, err := st.Status(9)
	if err != nil || status.Stage != StageSynced {
		t.Fatalf("Status = %+v (err=%v), want in-flight stage synced", status, err)
	}
}

func TestListReportsAllDirs(t *testing.T) {
	syncer := newFakeSyncer()
	st := newTestStore(t, syncer)
	ctx := context.Background()
	src := t.TempDir()

	if _, err := st.Insert(ctx, writeFile(t, filepath.Join(src, "ok"), "fine")); err != nil {
		t.Fatal(err)
	}
	syncer.failSync = errors.New("upload refused")
	if _, err := st.Insert(ctx, writeFile(t, filepath.Join(src, "bad"), "nope")); err == nil {
		t.Fatal("expected failure")
	}

	statuses, err := st.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(statuses) != 2 {
		t.Fatalf("List = %+v, want 2 entries", statuses)
	}
	if statuses[0].ID != 1 || statuses[0].Stage != StageComplete {
		t.Errorf("statuses[0] = %+v, want 1/COMPLETE", statuses[0])
	}
	if statuses[1].ID != 2 || statuses[1].Stage != StageErr {
		t.Errorf("statuses[1] = %+v, want 2/ERR", statuses[1])
	}
}

func TestOpenServesLocalThenFallsBackByStaticRef(t *testing.T) {
	syncer := newFakeSyncer()
	st := newTestStore(t, syncer)
	ctx := context.Background()

	src := writeFile(t, filepath.Join(t.TempDir(), "a.txt"), "the content")
	id, err := st.Insert(ctx, src)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}

	read := func() string {
		t.Helper()
		r, err := st.Open(ctx, id, "a.txt")
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer r.Close()
		b, _ := io.ReadAll(r)
		return string(b)
	}

	if got := read(); got != "the content" {
		t.Errorf("local read = %q", got)
	}

	// Evict the local copy: Open must fetch by the static RemoteID from
	// .refs.json, which stayed in the dir.
	if err := os.Remove(st.Path(id, "a.txt")); err != nil {
		t.Fatal(err)
	}
	if got := read(); got != "the content" {
		t.Errorf("fallback read = %q", got)
	}

	if _, err := st.Open(ctx, id, "not-a-file.txt"); err == nil {
		t.Error("expected error for a name not in the refs")
	}
}

func TestOpenRejectsPathTraversal(t *testing.T) {
	st := newTestStore(t, newFakeSyncer())
	ctx := context.Background()
	id, err := st.Insert(ctx, writeFile(t, filepath.Join(t.TempDir(), "a.txt"), "fine"))
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}

	// Plant secrets at every level an escaping name could reach.
	writeFile(t, filepath.Join(st.Dir(), "in-managed.txt"), "secret")
	writeFile(t, filepath.Join(filepath.Dir(st.Dir()), "outside.txt"), "secret")

	for _, name := range []string{
		"../in-managed.txt",
		"../../outside.txt",
		"..", ".", "",
		"x/../a.txt",
		`..\outside.txt`,
	} {
		r, err := st.Open(ctx, id, name)
		if err == nil {
			r.Close()
			t.Errorf("Open(%q) succeeded, want rejection", name)
		}
	}

	// The legitimate name still works.
	r, err := st.Open(ctx, id, "a.txt")
	if err != nil {
		t.Fatalf("Open(a.txt): %v", err)
	}
	r.Close()
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
	// Nothing above got far enough to create a managed dir.
	if statuses, err := st.List(); err != nil || len(statuses) != 0 {
		t.Errorf("List = %+v (err=%v), want empty", statuses, err)
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
