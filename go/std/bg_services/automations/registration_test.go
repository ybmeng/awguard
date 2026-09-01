package automations

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeBotnet is an in-memory stand-in for the botnet calendar REST API:
// GET/POST /v1/calendars, GET/POST /v1/events, per the firing contract.
type fakeBotnet struct {
	mu        sync.Mutex
	calendars []map[string]any
	events    []map[string]any
	posts     []string // "calendar:<name>" / "event:<automation>" in order
	patches   int
}

func (f *fakeBotnet) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/calendars", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		writeJSON(w, http.StatusOK, f.calendars)
	})
	mux.HandleFunc("POST /v1/calendars", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		defer f.mu.Unlock()
		name, _ := body["name"].(string)
		body["id"] = "cal_FAKE" + name
		f.calendars = append(f.calendars, body)
		f.posts = append(f.posts, "calendar:"+name)
		writeJSON(w, http.StatusOK, body)
	})
	mux.HandleFunc("GET /v1/events", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		writeJSON(w, http.StatusOK, f.events)
	})
	mux.HandleFunc("POST /v1/events", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		defer f.mu.Unlock()
		auto, _ := body["automation"].(string)
		body["id"] = "evt_FAKE" + auto
		f.events = append(f.events, body)
		f.posts = append(f.posts, "event:"+auto)
		writeJSON(w, http.StatusCreated, body)
	})
	mux.HandleFunc("PATCH /", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.patches++
		f.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{})
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func (f *fakeBotnet) postLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.posts...)
}

// ensureService builds a Service against repo and the fake botnet, with the
// registry snapshot already rescanned (no Run needed).
func ensureService(t *testing.T, repo, botnetAddr string) *Service {
	t.Helper()
	svc, err := New(Config{Root: t.TempDir(), RepoDir: repo, BotnetAddr: botnetAddr, Logger: discardLogger()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { svc.store.Close() })
	return svc
}

// TestRegistrationEnsure: the first tick creates the Automations calendar
// (executable) and one recurring event per scheduled automation; the second
// tick creates nothing; an existing event — wherever the user moved it — is
// never touched.
func TestRegistrationEnsure(t *testing.T) {
	repo := t.TempDir()
	writeAutomation(t, repo, "fred-m2", "echo x", `schedule:
  rrule: "FREQ=MONTHLY;BYDAY=4TU"
  at: "13:05"
  tz: "America/New_York"
  retry_every: 2h
  retry_for: 30h
`)
	writeAutomation(t, repo, "bare", "echo x", "") // no template: never registered

	fake := &fakeBotnet{}
	ts := fake.server(t)
	svc := ensureService(t, repo, strings.TrimPrefix(ts.URL, "http://"))

	if err := svc.tick(); err != nil {
		t.Fatalf("tick: %v", err)
	}
	posts := fake.postLog()
	if len(posts) != 2 || posts[0] != "calendar:Automations" || posts[1] != "event:fred-m2" {
		t.Fatalf("first tick posts = %v, want the calendar then the fred-m2 event", posts)
	}

	fake.mu.Lock()
	cal := fake.calendars[0]
	ev := fake.events[0]
	fake.mu.Unlock()
	if cal["executable"] != true {
		t.Errorf("calendar = %v, want executable true", cal)
	}
	if _, hasColor := cal["color"]; hasColor {
		t.Errorf("calendar = %v, color must be omitted (botnet assigns)", cal)
	}
	if ev["title"] != "fred-m2" || ev["rrule"] != "FREQ=MONTHLY;BYDAY=4TU" ||
		ev["tz"] != "America/New_York" || ev["automation"] != "fred-m2" || ev["calendarId"] != "cal_FAKEAutomations" {
		t.Errorf("event = %v", ev)
	}

	// startsAt is today's date at 13:05 America/New_York as a UTC instant;
	// endsAt is startsAt + retry_for (30h).
	loc, _ := time.LoadLocation("America/New_York")
	todayLocal := time.Now().In(loc)
	wantStart := time.Date(todayLocal.Year(), todayLocal.Month(), todayLocal.Day(), 13, 5, 0, 0, loc).UTC()
	gotStart, err := time.Parse(time.RFC3339, ev["startsAt"].(string))
	if err != nil || !gotStart.Equal(wantStart) {
		t.Errorf("startsAt = %v (%v), want %s", ev["startsAt"], err, wantStart.Format(time.RFC3339))
	}
	gotEnd, err := time.Parse(time.RFC3339, ev["endsAt"].(string))
	if err != nil || !gotEnd.Equal(wantStart.Add(30*time.Hour)) {
		t.Errorf("endsAt = %v (%v), want startsAt+retry_for", ev["endsAt"], err)
	}

	// Second tick: ensure-if-absent means NOTHING is created or patched.
	if err := svc.tick(); err != nil {
		t.Fatalf("second tick: %v", err)
	}
	if posts := fake.postLog(); len(posts) != 2 {
		t.Errorf("second tick posts = %v, want no new writes", posts)
	}
	if fake.patches != 0 {
		t.Errorf("tick PATCHed %d times; an existing event is NEVER updated", fake.patches)
	}
}

// TestRegistrationEnsureRespectsUserEdits: an event that already names the
// automation — even on another calendar at another time — suppresses
// creation forever.
func TestRegistrationEnsureRespectsUserEdits(t *testing.T) {
	repo := t.TempDir()
	writeAutomation(t, repo, "fred-m2", "echo x", `schedule:
  rrule: "FREQ=MONTHLY;BYDAY=4TU"
  at: "13:05"
  tz: "America/New_York"
  retry_every: 2h
  retry_for: 30h
`)
	fake := &fakeBotnet{
		calendars: []map[string]any{{"id": "cal_OTHER", "name": "Automations", "executable": true}},
		events: []map[string]any{{
			"id": "evt_MOVED", "calendarId": "cal_ELSEWHERE", "title": "my custom fred",
			"automation": "fred-m2", "startsAt": "2026-01-01T00:00:00Z", "endsAt": "2026-01-02T00:00:00Z",
		}},
	}
	ts := fake.server(t)
	svc := ensureService(t, repo, strings.TrimPrefix(ts.URL, "http://"))

	if err := svc.tick(); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if posts := fake.postLog(); len(posts) != 0 {
		t.Errorf("tick posts = %v, want none: the user's event stands", posts)
	}
}

// TestRegistrationEnsureBotnetDown: an unreachable botnet is logged and
// retried next tick, never fatal — and the rescan half still happens.
func TestRegistrationEnsureBotnetDown(t *testing.T) {
	repo := t.TempDir()
	writeAutomation(t, repo, "auto", "echo x", scheduledManifest)
	svc := ensureService(t, repo, "127.0.0.1:1")

	if err := svc.tick(); err == nil {
		t.Fatal("tick with botnet down should surface the ensure error for logging")
	}
	// The rescan half still landed.
	if _, ok := svc.automation("auto"); !ok {
		t.Error("rescan must happen even when the botnet is down")
	}

	// Next tick against a live fake succeeds and registers.
	fake := &fakeBotnet{}
	ts := fake.server(t)
	svc.botnetAddr = strings.TrimPrefix(ts.URL, "http://")
	if err := svc.tick(); err != nil {
		t.Fatalf("tick after botnet came back: %v", err)
	}
	if posts := fake.postLog(); len(posts) != 2 {
		t.Errorf("posts = %v, want calendar + event on the retry", posts)
	}
}

// TestTickEndpoint: POST /tick rescans and answers 200 even with no botnet
// configured; the response never fails the ping pipeline.
func TestTickEndpoint(t *testing.T) {
	repo := t.TempDir()
	writeAutomation(t, repo, "auto", "echo '"+okEnvelope+"'", "")
	svc := newSocketService(t, shortRoot(t), repo)
	cancel, _ := startService(t, svc)
	defer cancel()
	c := apiClient(svc.Root())
	waitForRegistry(t, c, 1)

	resp, err := c.Post("http://automations/tick", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("POST /tick = %d, want 200 (botnet trouble is logged, not returned)", resp.StatusCode)
	}
}
