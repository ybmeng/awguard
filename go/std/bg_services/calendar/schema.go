package calendar

// This file is the SPEC for the calendar domain. You author the structs here;
// storage, API and expansion are derived from them. DECISION marks settled
// calls, OPEN marks what's left to settle.

import (
	"fmt"
	"time"
)

// ── IDs ─────────────────────────────────────────────────────────────────────
// Prefixed ULID strings: sortable by creation time, generatable client-side
// with no coordinator, self-describing in logs. e.g. "evt_01J9X..." .
type EventID string // "evt_" + ULID

// Wall-clock storage layouts. Start, End, EXDATE entries and RRULE UNTIL all
// use these — RFC3339 without an offset, or a bare date when AllDay.
const (
	wallLayout = "2006-01-02T15:04:05"
	dateLayout = "2006-01-02"
)

// ── Event ────────────────────────────────────────────────────────────────────
// The master record of a single or recurring event. Concrete occurrences are
// never stored — they are expanded on demand (see Expand in rrule.go).
//
// DECISION (time representation): Start/End are wall-clock naive times plus a
// separate IANA zone id, NOT absolute UTC instants. A recurring "9am weekly"
// must stay 9am local across a DST boundary; storing UTC would drift it by an
// hour twice a year. Expansion iterates on the wall clock and anchors each
// occurrence into TZ only when emitting the absolute instant.
//
// DECISION (AllDay end convention): AllDay events use bare dates and End is
// EXCLUSIVE — the first day NOT covered — so a one-day event on 2024-05-01 has
// End "2024-05-02", and End must be at least one day after Start. Instances
// span local midnight of Start to local midnight of End in TZ.
type Event struct {
	ID          EventID `json:"id"`
	Title       string  `json:"title"`
	Description string  `json:"description"` // optional
	Location    string  `json:"location"`    // optional

	// AllDay events are date-only: Start/End are bare dates (dateLayout) and
	// the clock components do not exist.
	AllDay bool `json:"allDay"`

	// Start and End are wall-clock times in TZ (wallLayout, or dateLayout when
	// AllDay). End is strictly after Start for timed events; for AllDay see
	// the exclusive-end DECISION above.
	Start string `json:"start"`
	End   string `json:"end"`

	// TZ is the IANA zone id (e.g. "America/New_York") the wall clock is
	// anchored into. Required for all events — an AllDay event still needs it
	// to define which midnight its instances span. Validated with
	// time.LoadLocation on every write; the zone database is embedded via
	// time/tzdata so this never depends on the host's /usr/share/zoneinfo.
	TZ string `json:"tz"`

	// RRULE is an RFC 5545 recurrence rule ("FREQ=WEEKLY;BYDAY=MO;COUNT=4");
	// empty means a single event. Parsed and rejected at the write boundary —
	// see parseRRULE in rrule.go for the supported v1 subset.
	RRULE string `json:"rrule"`

	// EXDATE lists excluded instance start times, wall-clock in TZ, in the
	// same layout as Start. Excluded instances still consume COUNT (the
	// recurrence set is computed first, then EXDATE removes from it, per RFC).
	EXDATE []string `json:"exdate"`

	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// ── Instance ─────────────────────────────────────────────────────────────────
// One concrete occurrence of an event, produced by expansion. Start/End are
// absolute instants in the event's zone (RFC3339 with offset in JSON) — the
// only place absolute times appear in this package.
type Instance struct {
	EventID  EventID   `json:"eventId"`
	Title    string    `json:"title"`
	Location string    `json:"location"`
	AllDay   bool      `json:"allDay"`
	Start    time.Time `json:"start"`
	End      time.Time `json:"end"`
}

// parseWall parses a stored wall-clock string into the internal carrier: a
// time.Time constructed in time.UTC. UTC here is NOT a claim about the zone —
// it is a pure calendar-arithmetic space with no DST, so recurrence iteration
// can add days and months without ever crossing a transition. anchor() turns a
// carrier back into a real instant.
func parseWall(s string, allDay bool) (time.Time, error) {
	layout := wallLayout
	if allDay {
		layout = dateLayout
	}
	t, err := time.ParseInLocation(layout, s, time.UTC)
	if err != nil {
		return time.Time{}, fmt.Errorf("bad wall-clock time %q: want layout %s (no offset)", s, layout)
	}
	return t, nil
}

// formatWall is parseWall's inverse; EXDATE comparison happens on this
// normalized form.
func formatWall(t time.Time, allDay bool) string {
	if allDay {
		return t.Format(dateLayout)
	}
	return t.Format(wallLayout)
}

// anchor turns a wall-clock carrier into the absolute instant with those wall
// components in loc.
//
// DECISION (nonexistent wall times): a wall time inside a spring-forward gap
// (e.g. 02:30 on a US DST start date) does not exist in loc; time.Date
// normalizes it forward past the gap (02:30 EST -> 03:30 EDT). That is the
// defined behavior, not fought — the alternative (skipping or erroring) loses
// instances users expect to see.
func anchor(w time.Time, loc *time.Location) time.Time {
	return time.Date(w.Year(), w.Month(), w.Day(), w.Hour(), w.Minute(), w.Second(), 0, loc)
}
