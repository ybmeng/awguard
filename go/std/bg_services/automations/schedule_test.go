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

func TestParseScheduleErrors(t *testing.T) {
	cases := []struct {
		field, val, want string
	}{
		{"rrule", "", "rrule is required"},
		{"rrule", "FREQ=HOURLY", "not supported"},
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

// TestNextOccurrenceAcrossDST pins the fred-m2 rule: fourth Tuesday at 13:05
// America/New_York, evaluated across the 2026-03-08 spring-forward. The wall
// clock holds while the UTC offset moves from -05 to -04.
func TestNextOccurrenceAcrossDST(t *testing.T) {
	sc := mustSchedule(t, scheduleMap("FREQ=MONTHLY;BYDAY=4TU", "13:05", "America/New_York"))
	now := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

	prev, err := sc.latestOccurrence(now)
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 2, 24, 18, 5, 0, 0, time.UTC); !prev.Equal(want) {
		t.Errorf("latestOccurrence = %s, want %s (13:05 EST)", prev, want)
	}
	next, err := sc.nextOccurrence(now)
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 3, 24, 17, 5, 0, 0, time.UTC); !next.Equal(want) {
		t.Errorf("nextOccurrence = %s, want %s (13:05 EDT)", next, want)
	}
}

// TestOccurrencesByMonthDayList pins the korea-trass rule: the 1st, 11th, 15th
// and 21st at 09:05 Asia/Seoul (KST has no DST; 09:05 KST = 00:05 UTC).
func TestOccurrencesByMonthDayList(t *testing.T) {
	sc := mustSchedule(t, scheduleMap("FREQ=MONTHLY;BYMONTHDAY=1,11,15,21", "09:05", "Asia/Seoul"))
	now := time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)

	prev, err := sc.latestOccurrence(now)
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 9, 1, 0, 5, 0, 0, time.UTC); !prev.Equal(want) {
		t.Errorf("latestOccurrence = %s, want %s", prev, want)
	}
	next, err := sc.nextOccurrence(now)
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 9, 11, 0, 5, 0, 0, time.UTC); !next.Equal(want) {
		t.Errorf("nextOccurrence = %s, want %s", next, want)
	}

	// An occurrence exactly at now is "latest", never "next".
	at := time.Date(2026, 9, 11, 0, 5, 0, 0, time.UTC)
	if prev, _ = sc.latestOccurrence(at); !prev.Equal(at) {
		t.Errorf("latestOccurrence(at occurrence) = %s, want %s", prev, at)
	}
	if next, _ = sc.nextOccurrence(at); !next.Equal(time.Date(2026, 9, 15, 0, 5, 0, 0, time.UTC)) {
		t.Errorf("nextOccurrence(at occurrence) = %s, want the 15th", next)
	}
}

// dailySchedule is the schedule used by the window-state tests: noon UTC,
// retry every hour, six-hour window.
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

func envWith(newest string) string {
	return `{"automation":"auto","status":"ok","form_used":3,"artifacts":[{"path":"data/a.csv","rows":49,"newest":"` + newest + `"}],"escalation_reason":null}`
}

// TestWindowStateAndFreshness drives the runner-semantics table: baseline,
// advanced, satisfied, retry pacing, stale windows and freshness precedence —
// all derived from the runs table, nothing from memory.
func TestWindowStateAndFreshness(t *testing.T) {
	sc := dailySchedule(t)
	a := Automation{Name: "auto", Schedule: sc}
	now := time.Date(2026, 6, 15, 14, 0, 0, 0, time.UTC) // occ 12:00, window until 18:00

	t.Run("no runs: pending window, due immediately", func(t *testing.T) {
		s := newProbe(t, "")
		st, err := s.scheduleState(a, now)
		if err != nil {
			t.Fatal(err)
		}
		if !st.open || st.satisfied || !st.occ.Equal(time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)) {
			t.Fatalf("state = %+v", st)
		}
		if !due(sc, st, now) {
			t.Error("want due with no attempts in an open window")
		}
		if got := freshness(a, st, nil); got != "pending" {
			t.Errorf("freshness = %q, want pending", got)
		}
		if nd, _ := sc.nextDue(st, now); !nd.Equal(st.occ) {
			t.Errorf("nextDue = %s, want the occurrence itself", nd)
		}
	})

	t.Run("first ok run with no baseline satisfies", func(t *testing.T) {
		s := newProbe(t, "")
		insertFinished(t, s, "auto", "schedule", "2026-06-15T13:00:00Z", "ok", envWith("2026-05"))
		st, err := s.scheduleState(a, now)
		if err != nil {
			t.Fatal(err)
		}
		if !st.satisfied || due(sc, st, now) {
			t.Fatalf("state = %+v, want satisfied and not due", st)
		}
		last, _, _ := s.store.Latest("auto")
		if got := freshness(a, st, &last); got != "ok" {
			t.Errorf("freshness = %q, want ok", got)
		}
	})

	t.Run("restated history at constant rows does not satisfy", func(t *testing.T) {
		s := newProbe(t, "")
		// Baseline: an ok run before the occurrence, manual — trigger is irrelevant.
		insertFinished(t, s, "auto", "manual", "2026-06-14T13:00:00Z", "ok", envWith("2026-05"))
		// In-window run re-fetches the same newest period.
		insertFinished(t, s, "auto", "schedule", "2026-06-15T12:05:00Z", "ok", envWith("2026-05"))
		st, err := s.scheduleState(a, now)
		if err != nil {
			t.Fatal(err)
		}
		if st.satisfied {
			t.Fatal("a run that did not advance past the baseline must not satisfy")
		}
		// Pacing: last attempt 12:05, retry_every 1h → due again at 13:05 ≤ now.
		if !due(sc, st, now) {
			t.Error("want due once retry_every has elapsed")
		}
		if due(sc, st, time.Date(2026, 6, 15, 12, 30, 0, 0, time.UTC)) {
			t.Error("must not be due before retry_every elapses")
		}
		if nd, _ := sc.nextDue(st, now); !nd.Equal(time.Date(2026, 6, 15, 13, 5, 0, 0, time.UTC)) {
			t.Errorf("nextDue = %s, want lastAttempt+retry_every", nd)
		}
	})

	t.Run("advancing past the baseline satisfies", func(t *testing.T) {
		s := newProbe(t, "")
		insertFinished(t, s, "auto", "manual", "2026-06-14T13:00:00Z", "ok", envWith("2026-05"))
		insertFinished(t, s, "auto", "schedule", "2026-06-15T12:05:00Z", "ok", envWith("2026-06"))
		st, err := s.scheduleState(a, now)
		if err != nil {
			t.Fatal(err)
		}
		if !st.satisfied || due(sc, st, now) {
			t.Fatalf("state = %+v, want satisfied", st)
		}
	})

	t.Run("degraded never satisfies but keeps the retry cadence", func(t *testing.T) {
		s := newProbe(t, "")
		degraded := strings.Replace(envWith("2026-06"), `"status":"ok"`, `"status":"degraded"`, 1)
		insertFinished(t, s, "auto", "schedule", "2026-06-15T12:05:00Z", "degraded", degraded)
		st, err := s.scheduleState(a, now)
		if err != nil {
			t.Fatal(err)
		}
		if st.satisfied {
			t.Fatal("degraded must not satisfy")
		}
		if !due(sc, st, now) {
			t.Error("degraded does not stop the retry cadence")
		}
	})

	t.Run("expired unsatisfied window is stale and never due", func(t *testing.T) {
		s := newProbe(t, "")
		late := time.Date(2026, 6, 15, 19, 0, 0, 0, time.UTC) // window closed 18:00
		st, err := s.scheduleState(a, late)
		if err != nil {
			t.Fatal(err)
		}
		if st.open || due(sc, st, late) {
			t.Fatalf("state = %+v, want closed and not due", st)
		}
		if got := freshness(a, st, nil); got != "stale" {
			t.Errorf("freshness = %q, want stale", got)
		}
		// nextDue moves to the next occurrence.
		if nd, _ := sc.nextDue(st, late); !nd.Equal(time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC)) {
			t.Errorf("nextDue = %s, want tomorrow noon", nd)
		}
	})

	t.Run("satisfied window with a later failed run reads failed", func(t *testing.T) {
		s := newProbe(t, "")
		insertFinished(t, s, "auto", "schedule", "2026-06-15T12:05:00Z", "ok", envWith("2026-06"))
		insertFinished(t, s, "auto", "manual", "2026-06-15T13:30:00Z", "failed",
			`{"automation":"auto","status":"failed","form_used":3,"artifacts":[],"escalation_reason":"FRED unreachable"}`)
		st, err := s.scheduleState(a, now)
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

// TestRestartIdempotence proves the runner derives everything from disk: a
// satisfied window seen through a brand-new Service on the same root stays
// satisfied, so a restart never re-runs it.
func TestRestartIdempotence(t *testing.T) {
	root := t.TempDir()
	sc := dailySchedule(t)
	a := Automation{Name: "auto", Schedule: sc}
	now := time.Date(2026, 6, 15, 14, 0, 0, 0, time.UTC)

	first, err := New(Config{Root: root, Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	insertFinished(t, first, "auto", "schedule", "2026-06-15T12:05:00Z", "ok", envWith("2026-06"))
	first.store.Close()

	reopened, err := New(Config{Root: root, Logger: discardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.store.Close()
	st, err := reopened.scheduleState(a, now)
	if err != nil {
		t.Fatal(err)
	}
	if !st.satisfied || due(sc, st, now) {
		t.Fatalf("state after reopen = %+v, want satisfied and not due", st)
	}
}
