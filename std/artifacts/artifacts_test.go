package artifacts

import (
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTestService(t *testing.T, root string) *Service {
	t.Helper()
	s, err := New(Config{Root: root, Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func writeInbox(t *testing.T, s *Service, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(s.root, InboxDir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func readSynced(t *testing.T, s *Service, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(s.root, SyncedDir, name))
	if err != nil {
		t.Fatalf("read synced/%s: %v", name, err)
	}
	return string(b)
}

func TestNewCreatesDirs(t *testing.T) {
	s := newTestService(t, t.TempDir())
	for _, dir := range []string{InboxDir, SyncedDir} {
		info, err := os.Stat(filepath.Join(s.root, dir))
		if err != nil || !info.IsDir() {
			t.Fatalf("expected directory %s: err=%v", dir, err)
		}
	}
}

func TestNewRequiresRoot(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("expected error for empty root")
	}
}

func TestSyncOnceMovesFiles(t *testing.T) {
	s := newTestService(t, t.TempDir())
	writeInbox(t, s, "a.txt", "alpha")
	writeInbox(t, s, "b.txt", "bravo")

	moved, err := s.SyncOnce()
	if err != nil {
		t.Fatalf("SyncOnce: %v", err)
	}
	if moved != 2 {
		t.Fatalf("moved = %d, want 2", moved)
	}
	if got := readSynced(t, s, "a.txt"); got != "alpha" {
		t.Errorf("a.txt content = %q, want %q", got, "alpha")
	}
	if got := readSynced(t, s, "b.txt"); got != "bravo" {
		t.Errorf("b.txt content = %q, want %q", got, "bravo")
	}

	entries, err := os.ReadDir(filepath.Join(s.root, InboxDir))
	if err != nil {
		t.Fatalf("read inbox: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("inbox not empty after sync: %d entries", len(entries))
	}
}

func TestSyncOnceSkipsDirsAndDotfiles(t *testing.T) {
	s := newTestService(t, t.TempDir())
	if err := os.Mkdir(filepath.Join(s.root, InboxDir, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeInbox(t, s, ".hidden", "secret")

	moved, err := s.SyncOnce()
	if err != nil {
		t.Fatalf("SyncOnce: %v", err)
	}
	if moved != 0 {
		t.Fatalf("moved = %d, want 0", moved)
	}
	for _, name := range []string{"subdir", ".hidden"} {
		if _, err := os.Stat(filepath.Join(s.root, InboxDir, name)); err != nil {
			t.Errorf("%s should remain in inbox: %v", name, err)
		}
	}
}

func TestSyncOnceResolvesNameCollisions(t *testing.T) {
	s := newTestService(t, t.TempDir())

	for i, content := range []string{"first", "second", "third"} {
		writeInbox(t, s, "report.txt", content)
		if _, err := s.SyncOnce(); err != nil {
			t.Fatalf("SyncOnce #%d: %v", i+1, err)
		}
	}

	if got := readSynced(t, s, "report.txt"); got != "first" {
		t.Errorf("report.txt = %q, want %q", got, "first")
	}
	if got := readSynced(t, s, "report-1.txt"); got != "second" {
		t.Errorf("report-1.txt = %q, want %q", got, "second")
	}
	if got := readSynced(t, s, "report-2.txt"); got != "third" {
		t.Errorf("report-2.txt = %q, want %q", got, "third")
	}
}

func TestRunSyncsInBackground(t *testing.T) {
	s, err := New(Config{
		Root:     t.TempDir(),
		Interval: 10 * time.Millisecond,
		Logger:   log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	writeInbox(t, s, "bg.txt", "background")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(s.root, SyncedDir, "bg.txt")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("bg.txt was not synced within 2s")
		}
		time.Sleep(5 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Errorf("Run returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}
