package automations

import (
	"encoding/json"
	"time"
)

// windowState is everything the scheduler and the API derive from the runs
// table for one scheduled automation at one instant. Nothing here lives in
// memory between ticks — it is recomputed from disk every time, which is what
// makes the runner idempotent across restarts.
type windowState struct {
	occ         time.Time // latest schedule occurrence ≤ now; zero when none yet
	end         time.Time // occ + retry_for (the window's exclusive end)
	open        bool      // now ∈ [occ, end)
	satisfied   bool      // some run started in the window: status ok AND advanced(baseline)
	lastAttempt time.Time // start of the latest run started in the window; zero when none
}

// scheduleState evaluates a's current window against the runs table. a must
// carry a valid Schedule.
func (s *Service) scheduleState(a Automation, now time.Time) (windowState, error) {
	var st windowState
	occ, err := a.Schedule.latestOccurrence(now)
	if err != nil {
		return st, err
	}
	if occ.IsZero() {
		return st, nil
	}
	st.occ, st.end = occ, occ.Add(a.Schedule.RetryFor)
	st.open = now.Before(st.end)

	baseline, err := s.baseline(a.Name, occ)
	if err != nil {
		return st, err
	}
	runs, err := s.store.StartedIn(a.Name, fmtTime(occ), fmtTime(st.end))
	if err != nil {
		return st, err
	}
	for _, r := range runs {
		if t, err := time.Parse(time.RFC3339, r.Started); err == nil && t.After(st.lastAttempt) {
			st.lastAttempt = t
		}
		if r.Status != "ok" {
			continue
		}
		var env Envelope
		if json.Unmarshal([]byte(r.Envelope), &env) != nil {
			continue
		}
		if advanced(env, baseline) {
			st.satisfied = true
		}
	}
	return st, nil
}

// baseline returns the envelope of the latest run with envelope status ok
// (manual or scheduled) that started before occ, or nil when there is none.
func (s *Service) baseline(name string, occ time.Time) (*Envelope, error) {
	r, ok, err := s.store.LatestOKBefore(name, fmtTime(occ))
	if err != nil || !ok {
		return nil, err
	}
	var env Envelope
	if json.Unmarshal([]byte(r.Envelope), &env) != nil {
		return nil, nil // recorded ok runs always carry a parsed envelope; tolerate anyway
	}
	return &env, nil
}

// due reports whether the scheduler should start an attempt now: the window is
// open and unsatisfied, and either no attempt has started in it or the last
// one started at least retry_every ago. The in-flight guard is the enqueue
// path's pending set, not this function.
func due(sc *Schedule, st windowState, now time.Time) bool {
	if !st.open || st.satisfied {
		return false
	}
	if st.lastAttempt.IsZero() {
		return true
	}
	return now.Sub(st.lastAttempt) >= sc.RetryEvery
}

// nextDue is when the next automatic attempt could start: inside an open
// unsatisfied window it is the next retry (which may be in the past — due
// now); otherwise the next schedule occurrence.
func (sc *Schedule) nextDue(st windowState, now time.Time) (time.Time, error) {
	if st.open && !st.satisfied {
		if st.lastAttempt.IsZero() {
			return st.occ, nil
		}
		return st.lastAttempt.Add(sc.RetryEvery), nil
	}
	return sc.nextOccurrence(now)
}

// freshness classifies one automation for the API.
// Precedence: unscheduled > never > pending > stale > failed > ok.
func freshness(a Automation, st windowState, lastRun *Run) string {
	switch {
	case a.Schedule == nil:
		return "unscheduled"
	case st.occ.IsZero() && lastRun == nil:
		return "never" // scheduled, no runs yet, first window not yet opened
	case st.open && !st.satisfied:
		return "pending"
	case !st.occ.IsZero() && !st.open && !st.satisfied:
		return "stale" // latest closed window ended unsatisfied
	case lastRun != nil && (lastRun.Status == "failed" || lastRun.Status == StatusError):
		return "failed"
	default:
		return "ok"
	}
}
