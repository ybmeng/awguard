package automations

import (
	"errors"
	"fmt"
	"time"
)

// Schedule is a manifest's machine-readable schedule: block (the cadence:
// prose stays for humans). It is the PROVISIONING TEMPLATE for the calendar
// event registration-ensure creates — rrule + at + tz + retry_for describe
// the recurring event; the botnet calendar is then authoritative and this
// block never overrides it. RetryEvery is the one fire-time field: the pacing
// between attempts inside a window.
type Schedule struct {
	RRULE      string
	At         string // wall-clock "HH:MM" in TZ
	TZ         string // IANA zone id
	RetryEvery time.Duration
	RetryFor   time.Duration

	// The manifest's duration strings, echoed verbatim by the API.
	retryEveryRaw, retryForRaw string
}

// parseSchedule validates a manifest schedule block. Every error names the
// offending field and the expected form — it is surfaced verbatim on the
// automation in the API. The rrule's CONTENT is not validated here: the
// botnet calendar validates it when the event is created, and its expander
// is the one authority on the supported subset.
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
