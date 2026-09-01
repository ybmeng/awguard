package automations

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// postFire sends one /fire request and returns the status, decoded verdict
// body (nil on non-2xx) and the error envelope message.
func postFire(t *testing.T, c *http.Client, name, windowStart, windowEnd string) (int, map[string]string, string) {
	t.Helper()
	body := `{"windowStart":"` + windowStart + `","windowEnd":"` + windowEnd + `","eventId":"evt_TEST"}`
	resp, err := c.Post("http://automations/v1/automations/"+name+"/fire", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST fire: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		return resp.StatusCode, nil, e.Error
	}
	out := map[string]string{}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode fire response %q: %v", raw, err)
	}
	return resp.StatusCode, out, ""
}

// scheduledManifest is a schedule block whose retry_every (1h) is the pacing
// the fire tests exercise. The rrule/at/tz half is the provisioning template
// and is irrelevant to firing.
const scheduledManifest = `schedule:
  rrule: "FREQ=DAILY"
  at: "12:00"
  tz: "UTC"
  retry_every: 1h
  retry_for: 6h
`

// TestFireVerdicts drives the idempotent-arbiter table over one open window:
// enqueued on a virgin window, satisfied once an ok+advanced run landed,
// paced while the last attempt is younger than retry_every, enqueued again
// once it is older, 404 for unknown names, 400 for bad windows.
func TestFireVerdicts(t *testing.T) {
	now := time.Now().UTC()
	ws := fmtTime(now.Add(-time.Hour))
	we := fmtTime(now.Add(5 * time.Hour))

	repo := t.TempDir()
	writeAutomation(t, repo, "auto", "echo '"+okEnvelope+"'", scheduledManifest)

	t.Run("virgin window enqueues and records the window on the run", func(t *testing.T) {
		svc := newSocketService(t, shortRoot(t), repo)
		cancel, _ := startService(t, svc)
		defer cancel()
		c := apiClient(svc.Root())
		waitForRegistry(t, c, 1)

		status, out, _ := postFire(t, c, "auto", ws, we)
		if status != http.StatusOK || out["verdict"] != "enqueued" || !validRunID(out["runId"]) {
			t.Fatalf("fire = %d %v, want 200 enqueued with a run id", status, out)
		}
		// The recorded run carries the window bounds and trigger "schedule".
		deadline := time.Now().Add(5 * time.Second)
		for {
			run, err := svc.store.Get(out["runId"])
			if err != nil {
				t.Fatal(err)
			}
			if run.Finished != "" {
				if run.Trigger != "schedule" || run.WindowStart != ws || run.WindowEnd != we {
					t.Fatalf("fire run = trigger %q window [%q, %q], want schedule [%q, %q]",
						run.Trigger, run.WindowStart, run.WindowEnd, ws, we)
				}
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("fire run never finished")
			}
			time.Sleep(20 * time.Millisecond)
		}

		// The run was ok and advanced (no baseline): the same window is now
		// satisfied — a repeated or late ping must not re-run anything.
		status, out, _ = postFire(t, c, "auto", ws, we)
		if status != http.StatusOK || out["verdict"] != "satisfied" {
			t.Fatalf("second fire = %d %v, want satisfied", status, out)
		}
		if runs, _ := svc.store.List("auto", 50); len(runs) != 1 {
			t.Fatalf("satisfied window re-ran: %d runs", len(runs))
		}
	})

	t.Run("non-advancing attempt paces until retry_every elapses", func(t *testing.T) {
		svc := newSocketService(t, shortRoot(t), repo)
		cancel, _ := startService(t, svc)
		defer cancel()
		c := apiClient(svc.Root())
		waitForRegistry(t, c, 1)

		// Baseline before the window, then an in-window attempt 10 minutes ago
		// that restated the same newest — ok but not advanced.
		insertFinished(t, svc, "auto", "manual", fmtTime(now.Add(-24*time.Hour)), "ok", envWith("2026-05"))
		insertFinished(t, svc, "auto", "schedule", fmtTime(now.Add(-10*time.Minute)), "ok", envWith("2026-05"))
		status, out, _ := postFire(t, c, "auto", ws, we)
		if status != http.StatusOK || out["verdict"] != "paced" {
			t.Fatalf("fire = %d %v, want paced (last attempt 10m < retry_every 1h)", status, out)
		}

		// A third row: the latest attempt is now 2h old — pacing has lapsed.
		svc2 := newSocketService(t, shortRoot(t), repo)
		cancel2, _ := startService(t, svc2)
		defer cancel2()
		c2 := apiClient(svc2.Root())
		waitForRegistry(t, c2, 1)
		insertFinished(t, svc2, "auto", "manual", fmtTime(now.Add(-24*time.Hour)), "ok", envWith("2026-05"))
		insertFinished(t, svc2, "auto", "schedule", fmtTime(now.Add(-2*time.Hour)), "ok", envWith("2026-05"))
		status, out, _ = postFire(t, c2, "auto", ws, we)
		if status != http.StatusOK || out["verdict"] != "enqueued" {
			t.Fatalf("fire = %d %v, want enqueued once retry_every has elapsed", status, out)
		}
	})

	t.Run("advanced run in window satisfies even against a baseline", func(t *testing.T) {
		svc := newSocketService(t, shortRoot(t), repo)
		cancel, _ := startService(t, svc)
		defer cancel()
		c := apiClient(svc.Root())
		waitForRegistry(t, c, 1)

		insertFinished(t, svc, "auto", "manual", fmtTime(now.Add(-24*time.Hour)), "ok", envWith("2026-05"))
		insertFinished(t, svc, "auto", "schedule", fmtTime(now.Add(-30*time.Minute)), "ok", envWith("2026-06"))
		status, out, _ := postFire(t, c, "auto", ws, we)
		if status != http.StatusOK || out["verdict"] != "satisfied" {
			t.Fatalf("fire = %d %v, want satisfied", status, out)
		}
	})

	t.Run("unknown automation is a 404", func(t *testing.T) {
		svc := newSocketService(t, shortRoot(t), repo)
		cancel, _ := startService(t, svc)
		defer cancel()
		c := apiClient(svc.Root())
		waitForRegistry(t, c, 1)

		status, _, msg := postFire(t, c, "ghost", ws, we)
		if status != http.StatusNotFound || !strings.Contains(msg, "unknown automation") {
			t.Errorf("fire unknown = %d %q, want 404 unknown automation", status, msg)
		}
	})

	t.Run("bad windows are 400", func(t *testing.T) {
		svc := newSocketService(t, shortRoot(t), repo)
		cancel, _ := startService(t, svc)
		defer cancel()
		c := apiClient(svc.Root())
		waitForRegistry(t, c, 1)

		if status, _, msg := postFire(t, c, "auto", "soon", we); status != http.StatusBadRequest || !strings.Contains(msg, "windowStart") {
			t.Errorf("unparseable windowStart = %d %q, want 400 naming the field", status, msg)
		}
		if status, _, _ := postFire(t, c, "auto", we, ws); status != http.StatusBadRequest {
			t.Errorf("windowStart after windowEnd = %d, want 400", status)
		}
	})

	t.Run("in-flight run answers 409", func(t *testing.T) {
		svc := newSocketService(t, shortRoot(t), repo)
		cancel, _ := startService(t, svc)
		defer cancel()
		c := apiClient(svc.Root())
		waitForRegistry(t, c, 1)

		// A slow manual run occupies the runner; the fire window opens after
		// the run started, so the attempt does not count as in-window pacing.
		writeAutomation(t, repo, "auto", "sleep 1\necho '"+okEnvelope+"'", scheduledManifest)
		t.Cleanup(func() { writeAutomation(t, repo, "auto", "echo '"+okEnvelope+"'", scheduledManifest) })
		if status, _ := doJSON(t, c, http.MethodPost, "/v1/automations/auto/run", nil); status != http.StatusAccepted {
			t.Fatalf("manual run = %d", status)
		}
		futureWS := fmtTime(time.Now().Add(30 * time.Second))
		futureWE := fmtTime(time.Now().Add(time.Hour))
		status, _, msg := postFire(t, c, "auto", futureWS, futureWE)
		if status != http.StatusConflict {
			t.Errorf("fire while in flight = %d %q, want 409", status, msg)
		}
	})
}

// TestFirePacingWithoutScheduleTemplate: an automation with no schedule block
// can still be fired from the calendar; with no retry_every there is no
// pacing, but satisfaction still guards re-runs.
func TestFirePacingWithoutScheduleTemplate(t *testing.T) {
	now := time.Now().UTC()
	ws, we := fmtTime(now.Add(-time.Hour)), fmtTime(now.Add(5*time.Hour))
	repo := t.TempDir()
	writeAutomation(t, repo, "bare", "echo '"+okEnvelope+"'", "")

	svc := newSocketService(t, shortRoot(t), repo)
	cancel, _ := startService(t, svc)
	defer cancel()
	c := apiClient(svc.Root())
	waitForRegistry(t, c, 1)

	// A fresh non-advancing attempt: with no template there is no pacing, so
	// the verdict is enqueued — never paced.
	insertFinished(t, svc, "bare", "manual", fmtTime(now.Add(-24*time.Hour)), "ok",
		strings.Replace(envWith("2026-05"), `"automation":"auto"`, `"automation":"bare"`, 1))
	insertFinished(t, svc, "bare", "schedule", fmtTime(now.Add(-time.Minute)), "ok",
		strings.Replace(envWith("2026-05"), `"automation":"auto"`, `"automation":"bare"`, 1))
	status, out, _ := postFire(t, c, "bare", ws, we)
	if status != http.StatusOK || out["verdict"] != "enqueued" {
		t.Fatalf("fire = %d %v, want enqueued (no template, no pacing)", status, out)
	}
}
