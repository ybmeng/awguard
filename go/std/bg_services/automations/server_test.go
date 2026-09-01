package automations

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
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
	dir, err := os.MkdirTemp("/tmp", "autosvc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func newSocketService(t *testing.T, root, repo string, interval time.Duration) *Service {
	t.Helper()
	svc, err := New(Config{Root: root, RepoDir: repo, Interval: interval, Logger: discardLogger()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { svc.store.Close() })
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
// may be nil). It returns the HTTP status and, on a non-2xx, the message from
// the service's error envelope.
func doJSON(t *testing.T, c *http.Client, method, path string, out any) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, "http://automations"+path, nil)
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
	if resp.StatusCode >= 400 {
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

// waitForRegistry polls the list endpoint until the first scheduler tick has
// published a discovery snapshot of n automations.
func waitForRegistry(t *testing.T, c *http.Client, n int) []automationView {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		var views []automationView
		if status, _ := doJSON(t, c, http.MethodGet, "/v1/automations", &views); status == http.StatusOK && len(views) >= n {
			return views
		}
		if time.Now().After(deadline) {
			t.Fatalf("registry never reached %d automations", n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestServiceAPIEndToEnd(t *testing.T) {
	repo := t.TempDir()
	// "slow" is unscheduled and takes long enough to observe the 409;
	// "old" carries a schedule whose window (1s) is long closed → stale, and
	// the scheduler will never auto-run it during the test.
	writeAutomation(t, repo, "slow", "sleep 0.2\necho '"+okEnvelope+"'", "")
	writeAutomation(t, repo, "old", "echo '"+okEnvelope+"'", `schedule:
  rrule: "FREQ=MONTHLY;BYMONTHDAY=1,11,15,21"
  at: "09:05"
  tz: "Asia/Seoul"
  retry_every: 1h
  retry_for: 1s
`)
	svc := newSocketService(t, shortRoot(t), repo, 50*time.Millisecond)
	cancel, done := startService(t, svc)
	c := apiClient(svc.Root())

	views := waitForRegistry(t, c, 2)
	byName := map[string]automationView{}
	for _, v := range views {
		byName[v.Name] = v
	}
	if v := byName["slow"]; v.Freshness != "unscheduled" || v.Schedule != nil || v.NextDue != nil || v.LastRun != nil {
		t.Errorf("slow = %+v, want a bare unscheduled row", v)
	}
	v := byName["old"]
	if v.Schedule == nil || v.Schedule.RRULE != "FREQ=MONTHLY;BYMONTHDAY=1,11,15,21" ||
		v.Schedule.RetryEvery != "1h" || v.Schedule.TZ != "Asia/Seoul" {
		t.Errorf("old.schedule = %+v, want the manifest block echoed", v.Schedule)
	}
	if v.Freshness != "stale" || v.NextDue == nil {
		t.Errorf("old = freshness %q nextDue %v, want stale with a nextDue", v.Freshness, v.NextDue)
	}

	// Manual trigger: 202 with a run id, 409 while in flight, 404 unknown.
	var accepted struct {
		RunID string `json:"runId"`
	}
	req, _ := http.NewRequest(http.MethodPost, "http://automations/v1/automations/slow/run", bytes.NewReader(nil))
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST run = %d %s, want 202", resp.StatusCode, raw)
	}
	if json.Unmarshal(raw, &accepted); !validRunID(accepted.RunID) {
		t.Fatalf("runId %q is not a run_ ULID", accepted.RunID)
	}
	if status, _ := doJSON(t, c, http.MethodPost, "/v1/automations/slow/run", nil); status != http.StatusConflict {
		t.Errorf("second POST while in flight = %d, want 409", status)
	}
	if status, _ := doJSON(t, c, http.MethodPost, "/v1/automations/nope/run", nil); status != http.StatusNotFound {
		t.Errorf("POST unknown = %d, want 404", status)
	}

	// Poll the run until finished and check the full row.
	var run runJSON
	deadline := time.Now().Add(5 * time.Second)
	for {
		if status, _ := doJSON(t, c, http.MethodGet, "/v1/runs/"+accepted.RunID, &run); status != http.StatusOK {
			t.Fatalf("GET run = %d", status)
		}
		if run.Finished != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("run never finished: %+v", run)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if run.Status != "ok" || run.ExitCode != 0 || run.Trigger != "manual" || len(run.Envelope) == 0 {
		t.Fatalf("finished run = %+v", run)
	}

	// The per-automation row now carries the run as lastRun and in runs.
	var detail automationView
	if status, _ := doJSON(t, c, http.MethodGet, "/v1/automations/slow", &detail); status != http.StatusOK {
		t.Fatal("GET automation failed")
	}
	if detail.LastRun == nil || detail.LastRun.ID != accepted.RunID || len(detail.Runs) != 1 {
		t.Errorf("detail = %+v, want the finished run attached", detail)
	}
	var list []runSummary
	if status, _ := doJSON(t, c, http.MethodGet, "/v1/automations/slow/runs?limit=5", &list); status != http.StatusOK || len(list) != 1 {
		t.Errorf("runs list = %d entries, want 1", len(list))
	}

	// Malformed and missing ids.
	if status, _ := doJSON(t, c, http.MethodGet, "/v1/runs/run_nope", nil); status != http.StatusBadRequest {
		t.Errorf("malformed id = %d, want 400", status)
	}
	if status, _ := doJSON(t, c, http.MethodGet, "/v1/runs/"+newID("run_"), nil); status != http.StatusNotFound {
		t.Errorf("unknown id = %d, want 404", status)
	}

	// Shutdown removes the socket.
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
}

// TestListNeverNull: an empty registry answers [] — never null.
func TestListNeverNull(t *testing.T) {
	svc := newSocketService(t, shortRoot(t), "", DefaultInterval)
	cancel, _ := startService(t, svc)
	defer cancel()
	c := apiClient(svc.Root())

	resp, err := c.Get("http://automations/v1/automations")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if strings.TrimSpace(string(raw)) != "[]" {
		t.Errorf("empty list body = %q, want []", raw)
	}
}

// TestSchedulerAutoRuns: an automation whose window is open gets exactly one
// scheduled run — satisfied stops the cadence, derived purely from disk.
func TestSchedulerAutoRuns(t *testing.T) {
	repo := t.TempDir()
	at := time.Now().UTC().Add(-2 * time.Minute).Format("15:04")
	writeAutomation(t, repo, "fresh", "echo '"+okEnvelope+"'", `schedule:
  rrule: "FREQ=DAILY"
  at: "`+at+`"
  tz: "UTC"
  retry_every: 1h
  retry_for: 23h
`)
	svc := newSocketService(t, shortRoot(t), repo, 50*time.Millisecond)
	cancel, _ := startService(t, svc)
	defer cancel()
	c := apiClient(svc.Root())

	var runs []runSummary
	deadline := time.Now().Add(5 * time.Second)
	for {
		doJSON(t, c, http.MethodGet, "/v1/automations/fresh/runs", &runs)
		if len(runs) == 1 && runs[0].Finished != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("scheduled run never completed: %+v", runs)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if runs[0].Trigger != "schedule" || runs[0].Status != "ok" {
		t.Fatalf("auto run = %+v", runs[0])
	}

	// Several ticks later the satisfied window must not have re-run.
	time.Sleep(200 * time.Millisecond)
	doJSON(t, c, http.MethodGet, "/v1/automations/fresh/runs", &runs)
	if len(runs) != 1 {
		t.Fatalf("satisfied window re-ran: %d runs", len(runs))
	}
	var view automationView
	doJSON(t, c, http.MethodGet, "/v1/automations/fresh", &view)
	if view.Freshness != "ok" {
		t.Errorf("freshness after satisfied run = %q, want ok", view.Freshness)
	}
}

func TestSecondServiceRefusesBusyRoot(t *testing.T) {
	root := shortRoot(t)
	svc1 := newSocketService(t, root, "", DefaultInterval)
	cancel, _ := startService(t, svc1)
	defer cancel()

	svc2 := newSocketService(t, root, "", DefaultInterval)
	ctx, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()
	if err := svc2.Run(ctx); err == nil || !strings.Contains(err.Error(), "already serving") {
		t.Errorf("second Run = %v, want already-serving refusal", err)
	}
}

func TestVerify(t *testing.T) {
	svc := newProbe(t, "")
	if err := svc.Verify(context.Background()); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

func TestNewValidation(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("expected error for empty root")
	}
	if _, err := New(Config{Root: t.TempDir(), RepoDir: "/nonexistent/automations/repo"}); err == nil ||
		!strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("New with missing repo dir = %v, want instructive refusal", err)
	}
}

func TestDefaultRepoDir(t *testing.T) {
	t.Setenv("AUTOMATIONS_REPO", "")
	if got := DefaultRepoDir(); got != "" {
		t.Errorf("DefaultRepoDir() = %q, want empty", got)
	}
	t.Setenv("AUTOMATIONS_REPO", "/some/repo")
	if got := DefaultRepoDir(); got != "/some/repo" {
		t.Errorf("DefaultRepoDir() ignored AUTOMATIONS_REPO: %q", got)
	}
}
