package calendar

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// shortRoot returns a root directly under /tmp: unix socket paths are capped
// around 104 bytes, and the default test temp dir can exceed that.
func shortRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "calsvc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func newTestService(t *testing.T, root string) *Service {
	t.Helper()
	svc, err := New(Config{Root: root, Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return svc
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

func apiClient(root string) *http.Client {
	sock := SocketPath(root)
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "unix", sock)
		},
	}}
}

// doJSON sends one API request and decodes the JSON response into out (which
// may be nil). It returns the HTTP status and the error message from the
// service's error envelope, if any.
func doJSON(t *testing.T, c *http.Client, method, path string, body, out any) (int, string) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, "http://calendar"+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s %s: read body: %v", method, path, err)
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		return resp.StatusCode, e.Error
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("%s %s: decode %q: %v", method, path, raw, err)
		}
	}
	return resp.StatusCode, ""
}

func TestServiceAPIEndToEnd(t *testing.T) {
	svc := newTestService(t, shortRoot(t))
	cancel, done := startService(t, svc)
	c := apiClient(svc.Root())

	// Create a recurring event through the socket.
	var created struct {
		ID EventID `json:"id"`
	}
	status, _ := doJSON(t, c, http.MethodPost, "/v1/events", map[string]any{
		"title": "standup",
		"start": "2024-02-26T09:00:00",
		"end":   "2024-02-26T09:30:00",
		"tz":    "America/New_York",
		"rrule": "FREQ=WEEKLY;BYDAY=MO;COUNT=4",
	}, &created)
	if status != http.StatusOK || !validEventID(created.ID) {
		t.Fatalf("create = %d, id %q", status, created.ID)
	}

	var ev Event
	if status, _ := doJSON(t, c, http.MethodGet, "/v1/events/"+string(created.ID), nil, &ev); status != http.StatusOK {
		t.Fatalf("get = %d", status)
	}
	if ev.Title != "standup" || ev.RRULE != "FREQ=WEEKLY;BYDAY=MO;COUNT=4" || ev.TZ != "America/New_York" {
		t.Errorf("stored event = %+v", ev)
	}

	// Instances across the DST boundary keep the 9am wall clock.
	var instances []Instance
	window := "?from=2024-02-01T00:00:00Z&to=2024-04-01T00:00:00Z"
	if status, _ := doJSON(t, c, http.MethodGet, "/v1/instances"+window, nil, &instances); status != http.StatusOK {
		t.Fatalf("instances = %d", status)
	}
	wantStarts := []string{
		"2024-02-26T09:00:00-05:00", "2024-03-04T09:00:00-05:00",
		"2024-03-11T09:00:00-04:00", "2024-03-18T09:00:00-04:00",
	}
	if len(instances) != len(wantStarts) {
		t.Fatalf("instances = %d, want %d", len(instances), len(wantStarts))
	}
	for i, in := range instances {
		if got := in.Start.Format(time.RFC3339); got != wantStarts[i] {
			t.Errorf("instance[%d].Start = %s, want %s", i, got, wantStarts[i])
		}
		if in.EventID != created.ID {
			t.Errorf("instance[%d].EventID = %s", i, in.EventID)
		}
	}

	// PATCH is a partial update: only the sent fields change.
	var patched Event
	status, _ = doJSON(t, c, http.MethodPatch, "/v1/events/"+string(created.ID), map[string]any{
		"title":  "renamed",
		"exdate": []string{"2024-03-04T09:00:00"},
	}, &patched)
	if status != http.StatusOK || patched.Title != "renamed" || patched.RRULE != ev.RRULE {
		t.Fatalf("patch = %d, event %+v", status, patched)
	}
	if status, _ := doJSON(t, c, http.MethodGet, "/v1/instances"+window, nil, &instances); status != http.StatusOK {
		t.Fatal("instances after patch failed")
	}
	if len(instances) != 3 {
		t.Errorf("instances after EXDATE = %d, want 3", len(instances))
	}

	// A patch producing an invalid event is rejected and changes nothing.
	if status, msg := doJSON(t, c, http.MethodPatch, "/v1/events/"+string(created.ID),
		map[string]any{"tz": "Mars/Olympus"}, nil); status != http.StatusBadRequest || !strings.Contains(msg, "unknown tz") {
		t.Errorf("bad patch = %d %q, want 400 unknown tz", status, msg)
	}
	if _, err := svc.store.Get(created.ID); err != nil {
		t.Errorf("event lost after rejected patch: %v", err)
	}

	// Validation errors surface through the API with their message.
	badCreates := []struct {
		body map[string]any
		want string
	}{
		{map[string]any{"title": "x", "start": "2024-01-01T09:00:00", "end": "2024-01-01T10:00:00"}, "tz is required"},
		{map[string]any{"title": "x", "start": "2024-01-01T09:00:00", "end": "2024-01-01T08:00:00", "tz": "UTC"}, "must be after"},
		{map[string]any{"title": "x", "start": "2024-01-01T09:00:00", "end": "2024-01-01T10:00:00", "tz": "UTC", "rrule": "FREQ=HOURLY"}, "not supported"},
	}
	for _, bc := range badCreates {
		if status, msg := doJSON(t, c, http.MethodPost, "/v1/events", bc.body, nil); status != http.StatusBadRequest || !strings.Contains(msg, bc.want) {
			t.Errorf("create %v = %d %q, want 400 containing %q", bc.body, status, msg, bc.want)
		}
	}

	// Bad ids and bad windows.
	if status, _ := doJSON(t, c, http.MethodGet, "/v1/events/evt_nope", nil, nil); status != http.StatusBadRequest {
		t.Errorf("malformed id = %d, want 400", status)
	}
	missing := EventID(newID("evt_"))
	if status, _ := doJSON(t, c, http.MethodGet, "/v1/events/"+string(missing), nil, nil); status != http.StatusNotFound {
		t.Errorf("unknown id = %d, want 404", status)
	}
	if status, msg := doJSON(t, c, http.MethodGet, "/v1/instances?from=2024-01-01T00:00:00Z", nil, nil); status != http.StatusBadRequest || !strings.Contains(msg, "required") {
		t.Errorf("instances without to = %d %q, want 400", status, msg)
	}
	if status, msg := doJSON(t, c, http.MethodGet, "/v1/instances?from=2024-02-01T00:00:00Z&to=2024-02-01T00:00:00Z", nil, nil); status != http.StatusBadRequest || !strings.Contains(msg, "after") {
		t.Errorf("empty window = %d %q, want 400", status, msg)
	}

	// Delete removes the event and its instances.
	if status, _ := doJSON(t, c, http.MethodDelete, "/v1/events/"+string(created.ID), nil, nil); status != http.StatusOK {
		t.Fatalf("delete = %d", status)
	}
	if status, _ := doJSON(t, c, http.MethodGet, "/v1/events/"+string(created.ID), nil, nil); status != http.StatusNotFound {
		t.Errorf("get after delete = %d, want 404", status)
	}
	if status, _ := doJSON(t, c, http.MethodGet, "/v1/instances"+window, nil, &instances); status != http.StatusOK || len(instances) != 0 {
		t.Errorf("instances after delete = %d, %d instances, want empty", status, len(instances))
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
	if c, err := Dial(context.Background(), svc.Root()); err == nil {
		c.Close()
		t.Error("Dial should fail once the service is down")
	}
}

func TestInstancesMergeAcrossEvents(t *testing.T) {
	svc := newTestService(t, shortRoot(t))
	cancel, _ := startService(t, svc)
	defer cancel()
	c := apiClient(svc.Root())

	for _, body := range []map[string]any{
		{"title": "later", "start": "2024-05-01T15:00:00", "end": "2024-05-01T16:00:00", "tz": "UTC"},
		{"title": "earlier", "start": "2024-05-01T09:00:00", "end": "2024-05-01T10:00:00", "tz": "UTC"},
	} {
		if status, msg := doJSON(t, c, http.MethodPost, "/v1/events", body, nil); status != http.StatusOK {
			t.Fatalf("create %v = %d %q", body, status, msg)
		}
	}

	var instances []Instance
	if status, _ := doJSON(t, c, http.MethodGet, "/v1/instances?from=2024-05-01T00:00:00Z&to=2024-05-02T00:00:00Z", nil, &instances); status != http.StatusOK {
		t.Fatalf("instances = %d", status)
	}
	if len(instances) != 2 || instances[0].Title != "earlier" || instances[1].Title != "later" {
		t.Errorf("instances = %+v, want [earlier later] sorted by start", instances)
	}
}

func TestSecondServiceRefusesBusyRoot(t *testing.T) {
	root := shortRoot(t)
	svc1 := newTestService(t, root)
	cancel, _ := startService(t, svc1)
	defer cancel()

	svc2 := newTestService(t, root)
	ctx, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()
	if err := svc2.Run(ctx); err == nil || !strings.Contains(err.Error(), "already serving") {
		t.Errorf("second Run = %v, want already-serving refusal", err)
	}
}

func TestNewRequiresRoot(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("expected error for empty root")
	}
}
