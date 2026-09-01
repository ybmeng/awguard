package automations

import (
	"strings"
	"testing"
	"time"
)

func mustSchedule(t *testing.T, m map[string]string) *Schedule {
	t.Helper()
	sc, err := parseSchedule(m)
	if err != nil {
		t.Fatalf("parseSchedule(%v): %v", m, err)
	}
	return sc
}

func scheduleMap(rrule, at, tz string) map[string]string {
	return map[string]string{"rrule": rrule, "at": at, "tz": tz, "retry_every": "1h", "retry_for": "6h"}
}

// The schedule block is a provisioning template (rrule/at/tz/retry_for seed
// the calendar event) plus retry_every as fire-time pacing. Parsing validates
// the fields it can locally; the rrule's content is validated by the botnet
// when the event is created.
func TestParseScheduleErrors(t *testing.T) {
	cases := []struct {
		field, val, want string
	}{
		{"rrule", "", "rrule is required"},
		{"at", "9:05", "HH:MM"},
		{"at", "25:99", "HH:MM"},
		{"tz", "Mars/Olympus", "unknown tz"},
		{"retry_every", "fast", "positive Go duration"},
		{"retry_for", "-1h", "positive Go duration"},
	}
	for _, tc := range cases {
		m := scheduleMap("FREQ=DAILY", "09:05", "UTC")
		m[tc.field] = tc.val
		if _, err := parseSchedule(m); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("parseSchedule with %s=%q: err = %v, want containing %q", tc.field, tc.val, err, tc.want)
		}
	}
}

// dailySchedule is the template used by the window-state tests: retry every
// hour inside a six-hour window.
func dailySchedule(t *testing.T) *Schedule {
	return mustSchedule(t, scheduleMap("FREQ=DAILY", "12:00", "UTC"))
}

func insertFinished(t *testing.T, s *Service, name, trigger, started, status, envelope string) string {
	t.Helper()
	id := newID("run_")
	err := s.store.Insert(Run{
		ID: id, Automation: name, Trigger: trigger, Started: started,
		Finished: started, ExitCode: 0, Status: status, FormUsed: 3, Envelope: envelope,
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// insertFireRun records a finished fire-triggered run carrying its window.
func insertFireRun(t *testing.T, s *Service, name, started, status, envelope, ws, we string) string {
	t.Helper()
	id := newID("run_")
	err := s.store.Insert(Run{
		ID: id, Automation: name, Trigger: "schedule", Started: started,
		Finished: started, ExitCode: 0, Status: status, FormUsed: 3, Envelope: envelope,
		WindowStart: ws, WindowEnd: we,
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func envWith(newest string) string {
	return `{"automation":"auto","status":"ok","form_used":3,"artifacts":[{"path":"data/a.csv","rows":49,"newest":"` + newest + `"}],"escalation_reason":null}`
}

// TestWindowFromFiresAndFreshness drives the reworked freshness table: the
// latest known window comes from fire-run rows (the calendar owns the
// future), satisfaction and pacing state derive from the runs inside it, and
// nothing lives in memory.
func TestWindowFromFiresAndFreshness(t *testing.T) {
	sc := dailySchedule(t)
	a := Automation{Name: "auto", Schedule: sc}
	now := time.Date(2026, 6, 15, 14, 0, 0, 0, time.UTC)
	ws, we := "2026-06-15T12:00:00Z", "2026-06-15T18:00:00Z"

	t.Run("no fires ever: never (the calendar owns the future)", func(t *testing.T) {
		s := newProbe(t, "")
		st, err := s.windowFromFires(a, now)
		if err != nil {
			t.Fatal(err)
		}
		if !st.start.IsZero() {
			t.Fatalf("state = %+v, want no known window", st)
		}
		if got := freshness(a, st, nil); got != "never" {
			t.Errorf("freshness = %q, want never", got)
		}
	})

	t.Run("open window, unsatisfied fire run: pending", func(t *testing.T) {
		s := newProbe(t, "")
		// Baseline before the window; the in-window fire restated history.
		insertFinished(t, s, "auto", "manual", "2026-06-14T13:00:00Z", "ok", envWith("2026-05"))
		insertFireRun(t, s, "auto", "2026-06-15T12:05:00Z", "ok", envWith("2026-05"), ws, we)
		st, err := s.windowFromFires(a, now)
		if err != nil {
			t.Fatal(err)
		}
		if !st.open || st.satisfied {
			t.Fatalf("state = %+v, want open unsatisfied", st)
		}
		if !st.lastAttempt.Equal(time.Date(2026, 6, 15, 12, 5, 0, 0, time.UTC)) {
			t.Errorf("lastAttempt = %s", st.lastAttempt)
		}
		last, _, _ := s.store.Latest("auto")
		if got := freshness(a, st, &last); got != "pending" {
			t.Errorf("freshness = %q, want pending", got)
		}
	})

	t.Run("advancing fire run satisfies the window: ok", func(t *testing.T) {
		s := newProbe(t, "")
		insertFinished(t, s, "auto", "manual", "2026-06-14T13:00:00Z", "ok", envWith("2026-05"))
		insertFireRun(t, s, "auto", "2026-06-15T12:05:00Z", "ok", envWith("2026-06"), ws, we)
		st, err := s.windowFromFires(a, now)
		if err != nil {
			t.Fatal(err)
		}
		if !st.satisfied {
			t.Fatalf("state = %+v, want satisfied", st)
		}
		last, _, _ := s.store.Latest("auto")
		if got := freshness(a, st, &last); got != "ok" {
			t.Errorf("freshness = %q, want ok", got)
		}
	})

	t.Run("latest window closed unsatisfied: stale", func(t *testing.T) {
		s := newProbe(t, "")
		late := time.Date(2026, 6, 15, 19, 0, 0, 0, time.UTC) // window closed 18:00
		insertFireRun(t, s, "auto", "2026-06-15T12:05:00Z", "failed",
			`{"automation":"auto","status":"failed","form_used":3,"artifacts":[],"escalation_reason":"down"}`, ws, we)
		st, err := s.windowFromFires(a, late)
		if err != nil {
			t.Fatal(err)
		}
		if st.open || st.satisfied {
			t.Fatalf("state = %+v, want closed unsatisfied", st)
		}
		last, _, _ := s.store.Latest("auto")
		if got := freshness(a, st, &last); got != "stale" {
			t.Errorf("freshness = %q, want stale", got)
		}
	})

	t.Run("newer window supersedes an older one", func(t *testing.T) {
		s := newProbe(t, "")
		// Yesterday's window was satisfied; today's fire opened a new one.
		insertFireRun(t, s, "auto", "2026-06-14T12:05:00Z", "ok", envWith("2026-05"),
			"2026-06-14T12:00:00Z", "2026-06-14T18:00:00Z")
		insertFireRun(t, s, "auto", "2026-06-15T12:05:00Z", "ok", envWith("2026-05"), ws, we)
		st, err := s.windowFromFires(a, now)
		if err != nil {
			t.Fatal(err)
		}
		if !st.start.Equal(time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)) {
			t.Errorf("window start = %s, want today's window", st.start)
		}
		if st.satisfied {
			t.Error("today's restated fetch must not satisfy against yesterday's baseline")
		}
	})

	t.Run("satisfied window with a later failed run reads failed", func(t *testing.T) {
		s := newProbe(t, "")
		insertFireRun(t, s, "auto", "2026-06-15T12:05:00Z", "ok", envWith("2026-06"), ws, we)
		insertFinished(t, s, "auto", "manual", "2026-06-15T13:30:00Z", "failed",
			`{"automation":"auto","status":"failed","form_used":3,"artifacts":[],"escalation_reason":"FRED unreachable"}`)
		st, err := s.windowFromFires(a, now)
		if err != nil {
			t.Fatal(err)
		}
		if !st.satisfied {
			t.Fatal("the earlier ok run still satisfies the window")
		}
		last, _, _ := s.store.Latest("auto")
		if got := freshness(a, st, &last); got != "failed" {
			t.Errorf("freshness = %q, want failed", got)
		}
	})

	t.Run("unscheduled always reads unscheduled", func(t *testing.T) {
		if got := freshness(Automation{Name: "x"}, windowState{}, &Run{Status: "failed"}); got != "unscheduled" {
			t.Errorf("freshness = %q, want unscheduled", got)
		}
	})
}

// TestRestartIdempotence proves the arbiter derives everything from disk: a
// satisfied window seen through a brand-new Service on the same root stays
// satisfied.
func TestRestartIdempotence(t *testing.T) {
	root := t.TempDir()
	a := Automation{Name: "auto", Schedule: dailySchedule(t)}
	now := time.Date(2026, 6, 15, 14, 0, 0, 0, time.UTC)
	ws, we := "2026-06-15T12:00:00Z", "2026-06-15T18:00:00Z"

	first, err := New(Config{Root: root, Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	insertFireRun(t, first, "auto", "2026-06-15T12:05:00Z", "ok", envWith("2026-06"), ws, we)
	first.store.Close()

	reopened, err := New(Config{Root: root, Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.store.Close()
	st, err := reopened.windowFromFires(a, now)
	if err != nil {
		t.Fatal(err)
	}
	if !st.satisfied {
		t.Fatalf("state after reopen = %+v, want satisfied", st)
	}
	if v, err := reopened.fireVerdict(a, mustParse(t, ws), mustParse(t, we), now); err != nil || v != "satisfied" {
		t.Fatalf("fireVerdict after reopen = (%q, %v), want satisfied", v, err)
	}
}

func mustParse(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return ts
}
