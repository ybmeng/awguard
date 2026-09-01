package automations

import (
	"errors"
	"fmt"
	"time"

	"stdtools/go/std/bg_services/calendar"
)

// Schedule is a manifest's machine-readable schedule: block (the cadence:
// prose stays for humans). Occurrences are rrule + at, anchored in tz; each
// occurrence opens a retry window retry_for long, paced by retry_every.
type Schedule struct {
	RRULE      string
	At         string // wall-clock "HH:MM" in TZ
	TZ         string // IANA zone id
	RetryEvery time.Duration
	RetryFor   time.Duration

	// The manifest's duration strings, echoed verbatim by the API.
	retryEveryRaw, retryForRaw string
}

// anchorDay is the fixed wall-clock date schedule expansion starts from. Any
// date before every window the service will ever evaluate works; occurrences
// come purely from the rule, so the anchor never shows up as an instance
// unless the rule selects it.
const anchorDay = "2020-01-01"

// anchorInstant is an absolute lower bound predating every anchored occurrence.
var anchorInstant = time.Date(2019, 12, 30, 0, 0, 0, 0, time.UTC)

// parseSchedule validates a manifest schedule block. Every error names the
// offending field and the expected form — it is surfaced verbatim on the
// automation in the API.
func parseSchedule(m map[string]string) (*Schedule, error) {
	sc := &Schedule{
		RRULE: m["rrule"], At: m["at"], TZ: m["tz"],
		retryEveryRaw: m["retry_every"], retryForRaw: m["retry_for"],
	}
	if sc.RRULE == "" {
		return nil, errors.New(`schedule: rrule is required (e.g. "FREQ=MONTHLY;BYDAY=4TU")`)
	}
	if _, err := time.Parse("15:04", sc.At); err != nil || len(sc.At) != 5 {
		return nil, fmt.Errorf(`schedule: at %q must be wall-clock HH:MM (e.g. "13:05")`, sc.At)
	}
	if sc.TZ == "" {
		return nil, errors.New(`schedule: tz is required (an IANA id like "America/New_York")`)
	}
	if _, err := time.LoadLocation(sc.TZ); err != nil {
		return nil, fmt.Errorf(`schedule: unknown tz %q (want an IANA id like "America/New_York")`, sc.TZ)
	}
	var err error
	if sc.RetryEvery, err = positiveDuration("retry_every", m["retry_every"]); err != nil {
		return nil, err
	}
	if sc.RetryFor, err = positiveDuration("retry_for", m["retry_for"]); err != nil {
		return nil, err
	}
	// Validate the rrule through the calendar expander itself, so exactly the
	// supported subset is accepted and rejections carry its instructive errors.
	if _, err := sc.expand(anchorInstant, anchorInstant.AddDate(0, 0, 1)); err != nil {
		return nil, fmt.Errorf("schedule: %w", err)
	}
	return sc, nil
}

func positiveDuration(field, raw string) (time.Duration, error) {
	if raw == "" {
		return 0, fmt.Errorf(`schedule: %s is required (a Go duration like "2h")`, field)
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf(`schedule: %s %q must be a positive Go duration like "2h"`, field, raw)
	}
	return d, nil
}

// event builds the synthetic calendar event the schedule expands through:
// wall-clock start = the fixed anchor date at the schedule's at time, the
// schedule's tz and rrule, no EXDATE.
//
// DECISION: reuse the calendar package's Expand over copying it — the expander
// is the tested, DST-correct core, and schedules are exactly degenerate
// recurring events (a title-less 1-minute event whose instances we read only
// the starts of).
func (sc *Schedule) event() calendar.Event {
	start, _ := time.Parse("2006-01-02T15:04", anchorDay+"T"+sc.At)
	const wall = "2006-01-02T15:04:05"
	return calendar.Event{
		ID: "schedule", Title: "schedule", TZ: sc.TZ, RRULE: sc.RRULE,
		Start: start.Format(wall), End: start.Add(time.Minute).Format(wall),
	}
}

func (sc *Schedule) expand(from, to time.Time) ([]calendar.Instance, error) {
	return calendar.Expand(sc.event(), from, to)
}

// latestOccurrence returns the newest occurrence at or before now, or the zero
// time when the rule has produced none yet.
func (sc *Schedule) latestOccurrence(now time.Time) (time.Time, error) {
	ins, err := sc.expand(anchorInstant, now.Add(time.Second))
	if err != nil {
		return time.Time{}, err
	}
	var last time.Time
	for _, in := range ins {
		if !in.Start.After(now) {
			last = in.Start
		}
	}
	return last, nil
}

// nextOccurrence returns the first occurrence strictly after now, or the zero
// time when none falls within the next two years.
func (sc *Schedule) nextOccurrence(now time.Time) (time.Time, error) {
	ins, err := sc.expand(now, now.AddDate(2, 0, 2))
	if err != nil {
		return time.Time{}, err
	}
	for _, in := range ins {
		if in.Start.After(now) {
			return in.Start, nil
		}
	}
	return time.Time{}, nil
}
