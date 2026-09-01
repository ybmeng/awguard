package ping

import (
	"bytes"
	"context"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	bgservices "stdtools/go/std/bg_services"
)

// Compile-time check that Service satisfies the bg service contract.
var _ bgservices.Service = (*Service)(nil)

func discardLogger() *log.Logger { return log.New(io.Discard, "", 0) }

// countingServer returns an httptest server that counts POSTs it receives.
func countingServer(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("target received %s, want POST", r.Method)
		}
		hits.Add(1)
	}))
	t.Cleanup(ts.Close)
	return ts, &hits
}

// runFor runs svc in the background for d, then cancels and waits for Run to
// return.
func runFor(t *testing.T, svc *Service, d time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	if err := svc.Run(ctx); err != nil && err != context.DeadlineExceeded && err != context.Canceled {
		t.Fatalf("Run: %v", err)
	}
}

func TestPingFiresHTTPTarget(t *testing.T) {
	ts, hits := countingServer(t)
	svc, err := New(Config{
		Root:    t.TempDir(),
		Targets: []Target{{Name: "probe", URL: ts.URL, Interval: 20 * time.Millisecond}},
		Logger:  discardLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	runFor(t, svc, 200*time.Millisecond)
	if n := hits.Load(); n < 2 {
		t.Errorf("target hit %d times over 10 intervals, want at least 2 (first ping immediate, then per interval)", n)
	}
}

func TestPingFiresUnixTarget(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "pingtest")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "svc.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })

	var hits atomic.Int64
	var gotPath atomic.Value
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		gotPath.Store(r.URL.Path)
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })

	svc, err := New(Config{
		Root:    t.TempDir(),
		Targets: []Target{{Name: "sock", URL: "unix://" + sock + "/tick", Interval: 20 * time.Millisecond}},
		Logger:  discardLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	runFor(t, svc, 150*time.Millisecond)
	if hits.Load() < 1 {
		t.Fatal("unix target never pinged")
	}
	if p, _ := gotPath.Load().(string); p != "/tick" {
		t.Errorf("request path = %q, want /tick (the part after the socket)", p)
	}
}

// TestTargetsJSONMerge: extra targets from <Root>/ping/targets.json are pinged
// alongside the built-in ones.
func TestTargetsJSONMerge(t *testing.T) {
	builtin, builtinHits := countingServer(t)
	extra, extraHits := countingServer(t)

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, Dir), 0o755); err != nil {
		t.Fatal(err)
	}
	fileJSON := `[{"name":"extra","url":"` + extra.URL + `","interval":"20ms"}]`
	if err := os.WriteFile(filepath.Join(root, Dir, "targets.json"), []byte(fileJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	svc, err := New(Config{
		Root:    root,
		Targets: []Target{{Name: "builtin", URL: builtin.URL, Interval: 20 * time.Millisecond}},
		Logger:  discardLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	runFor(t, svc, 150*time.Millisecond)
	if builtinHits.Load() < 1 || extraHits.Load() < 1 {
		t.Errorf("hits builtin=%d extra=%d, want both pinged", builtinHits.Load(), extraHits.Load())
	}
}

// TestMalformedTargetsJSON: a broken targets.json is logged and ignored — the
// built-in targets still fire.
func TestMalformedTargetsJSON(t *testing.T) {
	ts, hits := countingServer(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, Dir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, Dir, "targets.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	svc, err := New(Config{
		Root:    root,
		Targets: []Target{{Name: "builtin", URL: ts.URL, Interval: 20 * time.Millisecond}},
		Logger:  log.New(&buf, "", 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	runFor(t, svc, 150*time.Millisecond)
	if hits.Load() < 1 {
		t.Error("built-in target must still fire when targets.json is malformed")
	}
	if !strings.Contains(buf.String(), "targets.json") {
		t.Errorf("log = %q, want a line about the malformed targets.json", buf.String())
	}
}

// TestNon2xxIsLoggedAndPingingContinues: a 500 from the target is logged; the
// next interval still fires (no backoff logic — the next interval IS the retry).
func TestNon2xxIsLoggedAndPingingContinues(t *testing.T) {
	var hits atomic.Int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(ts.Close)

	var buf bytes.Buffer
	svc, err := New(Config{
		Root:    t.TempDir(),
		Targets: []Target{{Name: "failing", URL: ts.URL, Interval: 20 * time.Millisecond}},
		Logger:  log.New(&buf, "", 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	runFor(t, svc, 200*time.Millisecond)
	if hits.Load() < 2 {
		t.Errorf("target hit %d times, want the cadence to survive non-2xx answers", hits.Load())
	}
	if !strings.Contains(buf.String(), "failing") || !strings.Contains(buf.String(), "500") {
		t.Errorf("log = %q, want a line naming the target and the 500", buf.String())
	}
}

func TestNewValidation(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Error("want error for empty root")
	}
	bad := []Target{
		{Name: "", URL: "http://x", Interval: time.Second},
		{Name: "x", URL: "ftp://nope", Interval: time.Second},
		{Name: "x", URL: "http://x", Interval: 0},
		{Name: "x", URL: "unix:///no/sock/suffix/tick", Interval: time.Second},
	}
	for _, tgt := range bad {
		if _, err := New(Config{Root: t.TempDir(), Targets: []Target{tgt}}); err == nil {
			t.Errorf("New with target %+v: want validation error", tgt)
		}
	}
}

func TestName(t *testing.T) {
	svc, err := New(Config{Root: t.TempDir(), Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	if svc.Name() != "ping" {
		t.Errorf("Name = %q, want ping", svc.Name())
	}
}

func TestVerify(t *testing.T) {
	svc, err := New(Config{Root: t.TempDir(), Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Verify(context.Background()); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}
