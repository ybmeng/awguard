package artifacts

import (
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// shortRoot returns a root directly under /tmp: unix socket paths are capped
// around 104 bytes, and the default test temp dir can exceed that.
func shortRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "artsvc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// startService runs svc in the background and waits until its API answers.
func startService(t *testing.T, svc *Service) (context.CancelFunc, chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- svc.Run(ctx) }()

	deadline := time.Now().Add(2 * time.Second)
	for {
		c, err := Dial(ctx, svc.Root())
		if err == nil {
			c.Close()
			return cancel, done
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("service API did not come up: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestServiceAPIEndToEnd(t *testing.T) {
	syncer := newFakeSyncer()
	svc, err := New(Config{Root: shortRoot(t), Syncer: syncer, Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	cancel, done := startService(t, svc)
	ctx := context.Background()

	client, err := Dial(ctx, svc.Root())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer client.Close()

	// Insert through the service: the machine runs in the service process.
	src := writeFile(t, filepath.Join(t.TempDir(), "a.txt"), "via socket")
	id, err := client.Insert(ctx, src)
	if err != nil {
		t.Fatalf("client.Insert: %v", err)
	}
	if id != 1 {
		t.Errorf("id = %s, want 1", id)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Errorf("source should be consumed by the service: %v", err)
	}

	status, err := client.Status(ctx, id)
	if err != nil || status.Stage != StageComplete {
		t.Fatalf("client.Status = %+v (err=%v), want COMPLETE", status, err)
	}
	statuses, err := client.List(ctx)
	if err != nil || len(statuses) != 1 || statuses[0].ID != id {
		t.Fatalf("client.List = %+v (err=%v)", statuses, err)
	}

	r, err := client.Open(ctx, id, "a.txt")
	if err != nil {
		t.Fatalf("client.Open: %v", err)
	}
	got, _ := io.ReadAll(r)
	r.Close()
	if string(got) != "via socket" {
		t.Errorf("read = %q", got)
	}

	// Errors surface through the API with their message.
	if _, err := client.Insert(ctx, "relative/path.txt"); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Errorf("relative-path insert error = %v, want absolute-path refusal", err)
	}
	if _, err := client.Insert(ctx, filepath.Join(t.TempDir(), "missing.txt")); err == nil {
		t.Error("expected error inserting a missing file")
	}
	if _, err := client.Open(ctx, 999, "nope"); err == nil {
		t.Error("expected error opening an unknown managed dir")
	}

	// Shutdown removes the socket; Dial then fails.
	cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Errorf("Run returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	if _, err := os.Stat(SocketPath(svc.Root())); !os.IsNotExist(err) {
		t.Errorf("socket should be removed on shutdown: %v", err)
	}
	if c, err := Dial(ctx, svc.Root()); err == nil {
		c.Close()
		t.Error("Dial should fail once the service is down")
	}
}

func TestSecondServiceRefusesBusyRoot(t *testing.T) {
	root := shortRoot(t)
	quiet := log.New(io.Discard, "", 0)
	svc1, err := New(Config{Root: root, Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	cancel, _ := startService(t, svc1)
	defer cancel()

	svc2, err := New(Config{Root: root, Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()
	err = svc2.Run(ctx)
	if err == nil || !strings.Contains(err.Error(), "already serving") {
		t.Errorf("second Run = %v, want already-serving refusal", err)
	}
}
