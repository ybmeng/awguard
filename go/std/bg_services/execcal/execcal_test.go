package execcal

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	bgservices "stdtools/go/std/bg_services"
)

// Compile-time check that Service satisfies the bg service contract.
var _ bgservices.Service = (*Service)(nil)

func discardLogger() *log.Logger { return log.New(io.Discard, "", 0) }

// shortRoot returns a root directly under /tmp: unix socket paths are capped
// around 104 bytes, and the default test temp dir can exceed that.
func shortRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "execcal")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// fireReq is one /fire call a fake automations server recorded.
type fireReq struct {
	Path string
	Body map[string]string
}

// fakeAutomations serves the automations /fire contract on a unix socket.
// answers maps automation name to a canned response: "enqueued", "satisfied",
// "paced", "404", or "boom" (a 500).
type fakeAutomations struct {
	mu   sync.Mutex
	got  []fireReq
	resp map[string]string
}

func (f *fakeAutomations) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/automations/"), "/fire")
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.got = append(f.got, fireReq{Path: r.URL.Path, Body: body})
		verdict := f.resp[name]
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch verdict {
		case "404":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"unknown automation: \"` + name + `\""}`))
		case "boom":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"boom"}`))
		case "enqueued":
			_, _ = w.Write([]byte(`{"verdict":"enqueued","runId":"run_01TESTTESTTESTTESTTESTTEST"}`))
		default:
			_, _ = w.Write([]byte(`{"verdict":"` + verdict + `"}`))
		}
	})
}

// serveUnix serves h on a fresh unix socket and returns the socket path.
func serveUnix(t *testing.T, h http.Handler) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "fakeauto")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "automations.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return sock
}

// fakeBotnet serves GET /v1/fireable returning the given JSON body.
func fakeBotnet(t *testing.T, fireableJSON string) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/fireable" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fireableJSON))
	}))
	t.Cleanup(ts.Close)
	return ts
}

// startService runs svc in the background and waits until its socket answers.
func startService(t *testing.T, svc *Service) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- svc.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	deadline := time.Now().Add(2 * time.Second)
	for {
		c, err := Dial(ctx, svc.Root())
		if err == nil {
			c.Close()
			return cancel
		}
		if time.Now().After(deadline) {
			t.Fatalf("execcal API did not come up: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func newService(t *testing.T, root, botnetAddr, autoSock string) *Service {
	t.Helper()
	svc, err := New(Config{Root: root, BotnetAddr: botnetAddr, AutomationsSocket: autoSock, Logger: discardLogger()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return svc
}

// tickResp is the /tick response shape.
type tickResp struct {
	Fired []struct {
		Automation string `json:"automation"`
		RunID      string `json:"runId"`
	} `json:"fired"`
	Skipped []struct {
		Automation string `json:"automation"`
		Reason     string `json:"reason"`
	} `json:"skipped"`
}

func postTick(t *testing.T, root string) (int, string) {
	t.Helper()
	c := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "unix", SocketPath(root))
		},
	}}
	defer c.CloseIdleConnections()
	resp, err := c.Post("http://execcal/tick", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /tick: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

// TestTickPassThrough: every fireable row becomes exactly one /fire POST with
// the window bounds and event id passed through verbatim.
func TestTickPassThrough(t *testing.T) {
	fireable := `[
	  {"automation":"fred-m2","eventId":"evt_A","windowStart":"2026-09-22T17:05:00Z","windowEnd":"2026-09-23T23:05:00Z"},
	  {"automation":"korea-trass","eventId":"evt_B","windowStart":"2026-09-01T00:05:00Z","windowEnd":"2026-09-02T00:05:00Z"}
	]`
	bot := fakeBotnet(t, fireable)
	auto := &fakeAutomations{resp: map[string]string{"fred-m2": "enqueued", "korea-trass": "satisfied"}}
	sock := serveUnix(t, auto.handler())

	svc := newService(t, shortRoot(t), strings.TrimPrefix(bot.URL, "http://"), sock)
	startService(t, svc)

	code, raw := postTick(t, svc.Root())
	if code != http.StatusOK {
		t.Fatalf("/tick = %d %s, want 200", code, raw)
	}
	var out tickResp
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("decode %q: %v", raw, err)
	}
	if len(out.Fired) != 1 || out.Fired[0].Automation != "fred-m2" || out.Fired[0].RunID == "" {
		t.Errorf("fired = %+v, want fred-m2 with its runId", out.Fired)
	}
	if len(out.Skipped) != 1 || out.Skipped[0].Automation != "korea-trass" || out.Skipped[0].Reason != "satisfied" {
		t.Errorf("skipped = %+v, want korea-trass reason satisfied", out.Skipped)
	}

	auto.mu.Lock()
	defer auto.mu.Unlock()
	if len(auto.got) != 2 {
		t.Fatalf("automations received %d fires, want 2", len(auto.got))
	}
	if auto.got[0].Path != "/v1/automations/fred-m2/fire" {
		t.Errorf("first fire path = %q", auto.got[0].Path)
	}
	want := map[string]string{"windowStart": "2026-09-22T17:05:00Z", "windowEnd": "2026-09-23T23:05:00Z", "eventId": "evt_A"}
	for k, v := range want {
		if auto.got[0].Body[k] != v {
			t.Errorf("fire body[%s] = %q, want %q (exact pass-through)", k, auto.got[0].Body[k], v)
		}
	}
}

// TestTickFailingFireDoesNotAbortTheRest: a 500 and a 404 are recorded as
// skips and the remaining rows still fire.
func TestTickFailingFireDoesNotAbortTheRest(t *testing.T) {
	fireable := `[
	  {"automation":"broken","eventId":"evt_1","windowStart":"2026-09-01T00:00:00Z","windowEnd":"2026-09-02T00:00:00Z"},
	  {"automation":"ghost","eventId":"evt_2","windowStart":"2026-09-01T00:00:00Z","windowEnd":"2026-09-02T00:00:00Z"},
	  {"automation":"fine","eventId":"evt_3","windowStart":"2026-09-01T00:00:00Z","windowEnd":"2026-09-02T00:00:00Z"}
	]`
	bot := fakeBotnet(t, fireable)
	auto := &fakeAutomations{resp: map[string]string{"broken": "boom", "ghost": "404", "fine": "enqueued"}}
	sock := serveUnix(t, auto.handler())

	svc := newService(t, shortRoot(t), strings.TrimPrefix(bot.URL, "http://"), sock)
	startService(t, svc)

	code, raw := postTick(t, svc.Root())
	if code != http.StatusOK {
		t.Fatalf("/tick = %d %s, want 200 despite per-fire failures", code, raw)
	}
	var out tickResp
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Fired) != 1 || out.Fired[0].Automation != "fine" {
		t.Errorf("fired = %+v, want only fine", out.Fired)
	}
	if len(out.Skipped) != 2 {
		t.Fatalf("skipped = %+v, want broken and ghost", out.Skipped)
	}
	reasons := map[string]string{}
	for _, sk := range out.Skipped {
		reasons[sk.Automation] = sk.Reason
	}
	if !strings.Contains(reasons["ghost"], "unknown automation") {
		t.Errorf("ghost reason = %q, want the automations 404 message", reasons["ghost"])
	}
	if reasons["broken"] == "" {
		t.Errorf("broken reason empty, want an error string")
	}
	auto.mu.Lock()
	defer auto.mu.Unlock()
	if len(auto.got) != 3 {
		t.Errorf("automations received %d fires, want all 3 attempted", len(auto.got))
	}
}

// TestTickEmptyFireable answers literal empty arrays, never null.
func TestTickEmptyFireable(t *testing.T) {
	bot := fakeBotnet(t, `[]`)
	sock := serveUnix(t, (&fakeAutomations{resp: map[string]string{}}).handler())
	svc := newService(t, shortRoot(t), strings.TrimPrefix(bot.URL, "http://"), sock)
	startService(t, svc)

	code, raw := postTick(t, svc.Root())
	if code != http.StatusOK {
		t.Fatalf("/tick = %d", code)
	}
	if !strings.Contains(raw, `"fired":[]`) || !strings.Contains(raw, `"skipped":[]`) {
		t.Errorf("body = %q, want literal empty arrays", raw)
	}
}

// TestTickBotnetDown: an unreachable botnet is a non-2xx tick with an
// instructive error; the service itself stays up.
func TestTickBotnetDown(t *testing.T) {
	sock := serveUnix(t, (&fakeAutomations{resp: map[string]string{}}).handler())
	svc := newService(t, shortRoot(t), "127.0.0.1:1", sock)
	startService(t, svc)

	code, raw := postTick(t, svc.Root())
	if code < 500 {
		t.Errorf("/tick with botnet down = %d %s, want a 5xx", code, raw)
	}
	if !strings.Contains(raw, "fireable") {
		t.Errorf("body = %q, want an error naming the fireable fetch", raw)
	}
	// Still serving.
	if code, _ := postTick(t, svc.Root()); code < 500 {
		t.Errorf("second tick = %d, service must survive", code)
	}
}

func TestSecondServiceRefusesBusyRoot(t *testing.T) {
	root := shortRoot(t)
	sock := serveUnix(t, (&fakeAutomations{resp: map[string]string{}}).handler())
	svc1 := newService(t, root, "127.0.0.1:1", sock)
	startService(t, svc1)

	svc2 := newService(t, root, "127.0.0.1:1", sock)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := svc2.Run(ctx); err == nil || !strings.Contains(err.Error(), "already serving") {
		t.Errorf("second Run = %v, want already-serving refusal", err)
	}
}

func TestNewValidation(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Error("want error for empty root")
	}
}

func TestName(t *testing.T) {
	svc := newService(t, shortRoot(t), "", "")
	if svc.Name() != "execcal" {
		t.Errorf("Name = %q, want execcal", svc.Name())
	}
}

func TestVerify(t *testing.T) {
	svc := newService(t, shortRoot(t), "", "")
	if err := svc.Verify(context.Background()); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}
