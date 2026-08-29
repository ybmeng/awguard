package artifacts

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
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

func TestOpenAPIRejectsTraversalNames(t *testing.T) {
	svc, err := New(Config{Root: shortRoot(t), Syncer: newFakeSyncer(), Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	if _, err := svc.Insert(ctx, writeFile(t, filepath.Join(t.TempDir(), "a.txt"), "fine")); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	// A secret outside the managed dir, reachable only by escaping it.
	writeFile(t, filepath.Join(svc.Root(), "secret.txt"), "top secret")

	mux := svc.apiMux()
	for _, target := range []string{
		"/v1/open/1/..%2Fsecret.txt",          // managed/secret.txt (none, but must not even try)
		"/v1/open/1/..%2F..%2Fsecret.txt",     // root/secret.txt — the planted file
		"/v1/open/1/x%2F..%2F..%2Fsecret.txt", // cleaned variant
	} {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code == http.StatusOK {
			t.Errorf("GET %s = 200, want rejection", target)
		}
		if strings.Contains(rec.Body.String(), "top secret") {
			t.Errorf("GET %s leaked file content outside the managed dir", target)
		}
	}
}

func TestSecondServiceRefusesBusyRoot(t *testing.T) {
	root := shortRoot(t)
	quiet := log.New(io.Discard, "", 0)
	// A huge interval keeps svc1 from re-draining the inbox after its
	// initial (empty) scan, so anything appearing there later can only have
	// been consumed by the losing service — which must never happen.
	svc1, err := New(Config{Root: root, Interval: time.Hour, Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	cancel, _ := startService(t, svc1)
	defer cancel()
	time.Sleep(100 * time.Millisecond) // let svc1's initial empty drain finish
	writeFile(t, filepath.Join(svc1.inbox, "later.txt"), "must stay put")

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

	// The loser must not have unlinked the winner's live socket: a busy but
	// healthy service keeps serving.
	if c, err := Dial(context.Background(), root); err != nil {
		t.Errorf("winner's socket unusable after losing Run: %v", err)
	} else {
		c.Close()
	}
	// And the loser must not have drained the inbox on its way out.
	if _, err := os.Stat(filepath.Join(svc1.inbox, "later.txt")); err != nil {
		t.Errorf("losing service consumed an inbox file: %v", err)
	}
	if statuses, err := svc1.Store().List(); err != nil || len(statuses) != 0 {
		t.Errorf("losing service inserted into the store: %+v (err=%v)", statuses, err)
	}
}
