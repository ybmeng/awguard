package artifacts

import (
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"
	"time"

	bgservices "awguard/go/std/bg_services"
)

// Compile-time check that Service satisfies the bg service contract.
var _ bgservices.Service = (*Service)(nil)

func newTestService(t *testing.T, root string) *Service {
	t.Helper()
	s, err := New(Config{Root: root, Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func writeFile(t *testing.T, path, content string) string {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func TestNewCreatesDirs(t *testing.T) {
	s := newTestService(t, t.TempDir())
	for _, dir := range []string{InboxDir, ManagedDir} {
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

func TestDrainInboxInsertsFiles(t *testing.T) {
	s := newTestService(t, t.TempDir())
	ctx := context.Background()
	writeFile(t, filepath.Join(s.inbox, "a.txt"), "alpha")
	writeFile(t, filepath.Join(s.inbox, "b.txt"), "bravo")

	inserted, err := s.DrainInbox(ctx)
	if err != nil {
		t.Fatalf("DrainInbox: %v", err)
	}
	if inserted != 2 {
		t.Fatalf("inserted = %d, want 2", inserted)
	}

	entries, err := os.ReadDir(s.inbox)
	if err != nil {
		t.Fatalf("read inbox: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("inbox not empty after drain: %d entries", len(entries))
	}

	// Each file got its own COMPLETE managed dir with a monotonic id.
	seen := map[string]bool{}
	for id := ID(1); id <= 2; id++ {
		status, err := s.store.Status(id)
		if err != nil || status.Stage != StageComplete {
			t.Fatalf("managed/%s status = %+v (err=%v), want COMPLETE", id, status, err)
		}
		files, err := contentFiles(filepath.Join(s.store.Dir(), id.String()))
		if err != nil || len(files) != 1 {
			t.Fatalf("managed/%s: err=%v files=%v, want 1 content file", id, err, files)
		}
		seen[files[0]] = true
	}
	if !seen["a.txt"] || !seen["b.txt"] {
		t.Errorf("managed dirs hold %v, want a.txt and b.txt", seen)
	}
}

func TestDrainInboxSkipsDirsAndDotfiles(t *testing.T) {
	s := newTestService(t, t.TempDir())
	if err := os.Mkdir(filepath.Join(s.inbox, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(s.inbox, ".hidden"), "secret")

	inserted, err := s.DrainInbox(context.Background())
	if err != nil {
		t.Fatalf("DrainInbox: %v", err)
	}
	if inserted != 0 {
		t.Fatalf("inserted = %d, want 0", inserted)
	}
	for _, name := range []string{"subdir", ".hidden"} {
		if _, err := os.Stat(filepath.Join(s.inbox, name)); err != nil {
			t.Errorf("%s should remain in inbox: %v", name, err)
		}
	}
}

func TestVerify(t *testing.T) {
	s := newTestService(t, t.TempDir())
	if err := s.Verify(context.Background()); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

func TestRunInsertsInBackground(t *testing.T) {
	s, err := New(Config{
		Root:     t.TempDir(),
		Interval: 10 * time.Millisecond,
		Logger:   log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	writeFile(t, filepath.Join(s.inbox, "bg.txt"), "background")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	target := filepath.Join(s.store.Dir(), "1", "bg.txt")
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(target); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("bg.txt was not inserted within 2s")
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
