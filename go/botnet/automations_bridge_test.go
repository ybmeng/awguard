package botnet

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// stubAutomations plays the mounted automations service: canned bodies with
// the real service's status codes, recording every request that reaches it so
// a test can prove the pipeline-internal routes never do.
type stubAutomations struct {
	mu   sync.Mutex
	seen []string
	mux  *http.ServeMux
}

func newStubAutomations() *stubAutomations {
	s := &stubAutomations{mux: http.NewServeMux()}
	canned := map[string]string{
		"GET /v1/automations":                  `[{"name":"alpha","path":"/abs/repo/alpha"}]`,
		"GET /v1/automations/alpha":            `{"name":"alpha","runs":[]}`,
		"GET /v1/automations/alpha/runs":       `[]`,
		"POST /v1/automations/alpha/run":       `{"runId":"run_01HZZZZZZZZZZZZZZZZZZZZZZZ"}`,
		"GET /v1/runs/run_01HZZZZZZZZZZZZZZZZZZZZZZZ": `{"id":"run_01HZZZZZZZZZZZZZZZZZZZZZZZ","status":"ok"}`,
		// The routes botnet must never bridge, so a leak is loud, not a 404
		// that could pass for the allowlist working.
		"POST /v1/automations/alpha/fire": `{"verdict":"enqueued"}`,
		"POST /tick":                      `{"ok":true}`,
	}
	for pattern, body := range canned {
		s.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/run") {
				w.WriteHeader(http.StatusAccepted)
			}
			io.WriteString(w, body)
		})
	}
	s.mux.HandleFunc("GET /v1/automations/{name}", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"error":"unknown automation: `+r.PathValue("name")+`"}`)
	})
	return s
}

func (s *stubAutomations) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.seen = append(s.seen, r.Method+" "+r.URL.Path)
	s.mu.Unlock()
	s.mux.ServeHTTP(w, r)
}

func (s *stubAutomations) requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.seen...)
}

// bridgeServer builds a server with (or without) a mounted automations
// backend. Mounting happens before Handler(), which is when the mux is built —
// the same order botnetsvc wires it in.
func bridgeServer(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	store, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	srv, err := NewServer(store, &fakeLLM{})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	srv.MountAutomations(h)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// bridgeDo drives one request against the botnet server and returns status
// and raw body.
func bridgeDo(t *testing.T, ts *httptest.Server, method, path string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, ts.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s %s: read body: %v", method, path, err)
	}
	return resp.StatusCode, string(raw)
}

// A mounted automations backend answers the five bridged routes verbatim:
// same paths, same status codes, same bodies.
func TestMountedAutomationsRoutesPassThrough(t *testing.T) {
	stub := newStubAutomations()
	ts := bridgeServer(t, stub)

	cases := []struct {
		method, path string
		wantStatus   int
		wantBody     string
	}{
		{"GET", "/v1/automations", 200, `[{"name":"alpha","path":"/abs/repo/alpha"}]`},
		{"GET", "/v1/automations/alpha", 200, `{"name":"alpha","runs":[]}`},
		{"GET", "/v1/automations/alpha/runs", 200, `[]`},
		{"POST", "/v1/automations/alpha/run", 202, `{"runId":"run_01HZZZZZZZZZZZZZZZZZZZZZZZ"}`},
		{"GET", "/v1/runs/run_01HZZZZZZZZZZZZZZZZZZZZZZZ", 200, `{"id":"run_01HZZZZZZZZZZZZZZZZZZZZZZZ","status":"ok"}`},
		{"GET", "/v1/automations/nope", 404, `{"error":"unknown automation: nope"}`},
	}
	for _, c := range cases {
		status, body := bridgeDo(t, ts, c.method, c.path)
		if status != c.wantStatus || body != c.wantBody {
			t.Errorf("%s %s = %d %q, want %d %q", c.method, c.path, status, body, c.wantStatus, c.wantBody)
		}
	}
}

// The pipeline-internal routes stay on the unix socket: even mounted, botnet
// 404s them and the backend never sees the request.
func TestFireAndTickNeverBridge(t *testing.T) {
	stub := newStubAutomations()
	ts := bridgeServer(t, stub)

	for _, path := range []string{"/v1/automations/alpha/fire", "/tick"} {
		if status, body := bridgeDo(t, ts, "POST", path); status != http.StatusNotFound {
			t.Errorf("POST %s through botnet = %d %q, want 404", path, status, body)
		}
	}
	for _, saw := range stub.requests() {
		if strings.HasSuffix(saw, "/fire") || strings.HasSuffix(saw, "/tick") {
			t.Errorf("pipeline-internal request leaked through the bridge: %s", saw)
		}
	}
}

// Unmounted (botnetd, tests), the routes are absent: 404, which is the app's
// hide-the-section signal.
func TestUnmountedAutomationsRoutesAbsent(t *testing.T) {
	ts := bridgeServer(t, nil)
	for _, c := range []struct{ method, path string }{
		{"GET", "/v1/automations"},
		{"GET", "/v1/automations/alpha"},
		{"GET", "/v1/automations/alpha/runs"},
		{"POST", "/v1/automations/alpha/run"},
		{"GET", "/v1/runs/run_01HZZZZZZZZZZZZZZZZZZZZZZZ"},
	} {
		if status, _ := bridgeDo(t, ts, c.method, c.path); status != http.StatusNotFound {
			t.Errorf("unmounted %s %s = %d, want 404", c.method, c.path, status)
		}
	}
}
