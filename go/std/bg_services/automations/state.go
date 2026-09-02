package automations

import (
	"encoding/json"
	"fmt"
	"time"
)

// windowState is everything the fire arbiter and the API derive from the
// runs table for one automation at one instant. The calendar owns the future:
// the latest known window comes from recorded fire runs, and nothing here
// lives in memory between requests — it is recomputed from disk every time,
// which is what makes firing idempotent across restarts, repeats and
// double-fires.
type windowState struct {
	start       time.Time // latest recorded fire window's start; zero when none yet
	end         time.Time // that window's exclusive end
	open        bool      // now < end
	satisfied   bool      // some run started ≥ start: status ok AND advanced(baseline)
	lastAttempt time.Time // start of the latest run started ≥ start; zero when none
}

// windowFromFires evaluates a's latest known fire window against the runs
// table.
func (s *Service) windowFromFires(a Automation, now time.Time) (windowState, error) {
	var st windowState
	ws, we, ok, err := s.store.LatestWindow(a.Name)
	if err != nil || !ok {
		return st, err
	}
	if st.start, err = time.Parse(time.RFC3339, ws); err != nil {
		return st, fmt.Errorf("recorded window start %q: %w", ws, err)
	}
	if st.end, err = time.Parse(time.RFC3339, we); err != nil {
		return st, fmt.Errorf("recorded window end %q: %w", we, err)
	}
	st.open = now.Before(st.end)
	st.satisfied, st.lastAttempt, err = s.windowRuns(a.Name, st.start)
	return st, err
}

// windowRuns scans the runs started at or after windowStart: whether one of
// them satisfied the window (envelope ok and advanced past the pre-window
// baseline) and when the latest attempt started.
func (s *Service) windowRuns(name string, windowStart time.Time) (satisfied bool, lastAttempt time.Time, err error) {
	baseline, err := s.baseline(name, windowStart)
	if err != nil {
		return false, time.Time{}, err
	}
	runs, err := s.store.StartedSince(name, fmtTime(windowStart))
	if err != nil {
		return false, time.Time{}, err
	}
	for _, r := range runs {
		if t, err := time.Parse(time.RFC3339, r.Started); err == nil && t.After(lastAttempt) {
			lastAttempt = t
		}
		if r.Status != "ok" {
			continue
		}
		var env Envelope
		if json.Unmarshal([]byte(r.Envelope), &env) != nil {
			continue
		}
		if advanced(env, baseline) {
			satisfied = true
		}
	}
	return satisfied, lastAttempt, nil
}

// baseline returns the envelope of the latest run with envelope status ok
// (manual or scheduled) that started before the window opened, or nil when
// there is none.
func (s *Service) baseline(name string, windowStart time.Time) (*Envelope, error) {
	r, ok, err := s.store.LatestOKBefore(name, fmtTime(windowStart))
	if err != nil || !ok {
		return nil, err
	}
	var env Envelope
	if json.Unmarshal([]byte(r.Envelope), &env) != nil {
		return nil, nil // recorded ok runs always carry a parsed envelope; tolerate anyway
	}
	return &env, nil
}

// fireVerdict is the idempotent arbiter's decision for one fire of the window
// [ws, we): "satisfied" when an ok+advanced run already landed since ws,
// "paced" when the latest in-window attempt is younger than the template's
// retry_every, "enqueued" otherwise (the caller then actually enqueues). An
// automation without a schedule template has no retry_every and is never
// paced — satisfaction alone guards its re-runs.
func (s *Service) fireVerdict(a Automation, ws, we, now time.Time) (string, error) {
	satisfied, lastAttempt, err := s.windowRuns(a.Name, ws)
	if err != nil {
		return "", err
	}
	switch {
	case satisfied:
		return "satisfied", nil
	case a.Schedule != nil && !lastAttempt.IsZero() && now.Sub(lastAttempt) < a.Schedule.RetryEvery:
		return "paced", nil
	default:
		return "enqueued", nil
	}
}

// freshness classifies one automation for the API.
// Precedence: unscheduled > never > pending > stale > failed > ok.
func freshness(a Automation, st windowState, lastRun *Run) string {
	switch {
	case a.Schedule == nil:
		return "unscheduled"
	case st.start.IsZero() && lastRun == nil:
		return "never" // scheduled, but no fire has ever opened a window and nothing ran
	case st.open && !st.satisfied:
		return "pending"
	case !st.start.IsZero() && !st.open && !st.satisfied:
		return "stale" // latest known window closed unsatisfied
	case lastRun != nil && (lastRun.Status == "failed" || lastRun.Status == StatusError):
		return "failed"
	default:
		return "ok"
	}
}
