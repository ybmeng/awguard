package botnet

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// The RRULE expander, ported from go/std/bg_services/calendar. The engine
// iterates on a wall-clock carrier and anchors each occurrence into the
// event's zone, which is what keeps "9am weekly" at 9am across DST. botnet
// stores absolute UTC instants, so expandEvent first recovers the wall clock
// from StartsAt in TZ — these fixtures therefore give StartsAt WITH its local
// offset, and the wants are the same instants the std suite pinned.

// rev builds a recurring-event fixture. start and end are RFC3339 with the
// zone's offset (the first occurrence); they are stored as the UTC instants
// the botnet store would hold.
func rev(t *testing.T, tz, start, end, rrule string) Event {
	t.Helper()
	return Event{
		ID: "evt_TEST", Title: "t", TZ: tz, RRule: rrule,
		StartsAt: mustInstant(t, start), EndsAt: mustInstant(t, end),
	}
}

func mustInstant(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("bad fixture time %q: %v", s, err)
	}
	return v.UTC()
}

// TestExpandEvent pins the expander. Fixtures marked "RFC" are transcribed
// from the worked examples in RFC 5545 §3.8.5.3 (DTSTART 1997,
// America/New_York; EDT/EST offsets in the wants come from the RFC's own
// annotations).
func TestExpandEvent(t *testing.T) {
	const ny = "America/New_York"
	cases := []struct {
		name     string
		event    Event
		from, to string
		want     []string // instance starts, RFC3339 with offset
		wantEnds []string // optional
	}{
		{
			name:  "RFC daily for 10 occurrences",
			event: rev(t, ny, "1997-09-02T09:00:00-04:00", "1997-09-02T10:00:00-04:00", "FREQ=DAILY;COUNT=10"),
			from:  "1997-09-01T00:00:00Z", to: "1997-10-01T00:00:00Z",
			want: []string{
				"1997-09-02T09:00:00-04:00", "1997-09-03T09:00:00-04:00", "1997-09-04T09:00:00-04:00",
				"1997-09-05T09:00:00-04:00", "1997-09-06T09:00:00-04:00", "1997-09-07T09:00:00-04:00",
				"1997-09-08T09:00:00-04:00", "1997-09-09T09:00:00-04:00", "1997-09-10T09:00:00-04:00",
				"1997-09-11T09:00:00-04:00",
			},
		},
		{
			name:  "RFC every 10 days 5 occurrences (INTERVAL>1)",
			event: rev(t, ny, "1997-09-02T09:00:00-04:00", "1997-09-02T10:00:00-04:00", "FREQ=DAILY;INTERVAL=10;COUNT=5"),
			from:  "1997-09-01T00:00:00Z", to: "1998-01-01T00:00:00Z",
			want: []string{
				"1997-09-02T09:00:00-04:00", "1997-09-12T09:00:00-04:00", "1997-09-22T09:00:00-04:00",
				"1997-10-02T09:00:00-04:00", "1997-10-12T09:00:00-04:00",
			},
		},
		{
			name:  "RFC weekly for 10 occurrences crosses the EDT-to-EST fall-back",
			event: rev(t, ny, "1997-09-02T09:00:00-04:00", "1997-09-02T10:00:00-04:00", "FREQ=WEEKLY;COUNT=10"),
			from:  "1997-09-01T00:00:00Z", to: "1998-01-01T00:00:00Z",
			want: []string{
				"1997-09-02T09:00:00-04:00", "1997-09-09T09:00:00-04:00", "1997-09-16T09:00:00-04:00",
				"1997-09-23T09:00:00-04:00", "1997-09-30T09:00:00-04:00", "1997-10-07T09:00:00-04:00",
				"1997-10-14T09:00:00-04:00", "1997-10-21T09:00:00-04:00", "1997-10-28T09:00:00-05:00",
				"1997-11-04T09:00:00-05:00",
			},
		},
		{
			name: "RFC every other week MO WE FR until Dec 24, WKST=SU",
			event: rev(t, ny, "1997-09-01T09:00:00-04:00", "1997-09-01T10:00:00-04:00",
				"FREQ=WEEKLY;INTERVAL=2;UNTIL=19971224T000000Z;WKST=SU;BYDAY=MO,WE,FR"),
			from: "1997-09-01T00:00:00Z", to: "1998-06-01T00:00:00Z",
			want: []string{
				"1997-09-01T09:00:00-04:00", "1997-09-03T09:00:00-04:00", "1997-09-05T09:00:00-04:00",
				"1997-09-15T09:00:00-04:00", "1997-09-17T09:00:00-04:00", "1997-09-19T09:00:00-04:00",
				"1997-09-29T09:00:00-04:00", "1997-10-01T09:00:00-04:00", "1997-10-03T09:00:00-04:00",
				"1997-10-13T09:00:00-04:00", "1997-10-15T09:00:00-04:00", "1997-10-17T09:00:00-04:00",
				"1997-10-27T09:00:00-05:00", "1997-10-29T09:00:00-05:00", "1997-10-31T09:00:00-05:00",
				"1997-11-10T09:00:00-05:00", "1997-11-12T09:00:00-05:00", "1997-11-14T09:00:00-05:00",
				"1997-11-24T09:00:00-05:00", "1997-11-26T09:00:00-05:00", "1997-11-28T09:00:00-05:00",
				"1997-12-08T09:00:00-05:00", "1997-12-10T09:00:00-05:00", "1997-12-12T09:00:00-05:00",
				"1997-12-22T09:00:00-05:00",
			},
		},
		{
			// BYDAY limits the BYMONTHDAY expansion; also proves DTSTART is
			// not special-cased — Sep 2 1997 matches nothing and is not emitted.
			name:  "RFC every Friday the 13th (BYDAY+BYMONTHDAY intersection)",
			event: rev(t, ny, "1997-09-02T09:00:00-04:00", "1997-09-02T10:00:00-04:00", "FREQ=MONTHLY;BYDAY=FR;BYMONTHDAY=13"),
			from:  "1997-09-01T00:00:00Z", to: "2000-12-31T00:00:00Z",
			want: []string{
				"1998-02-13T09:00:00-05:00", "1998-03-13T09:00:00-05:00", "1998-11-13T09:00:00-05:00",
				"1999-08-13T09:00:00-04:00", "2000-10-13T09:00:00-04:00",
			},
		},
		{
			name:  "RFC second-to-last Monday of the month for 6 months",
			event: rev(t, ny, "1997-09-22T09:00:00-04:00", "1997-09-22T10:00:00-04:00", "FREQ=MONTHLY;COUNT=6;BYDAY=-2MO"),
			from:  "1997-09-01T00:00:00Z", to: "1998-06-01T00:00:00Z",
			want: []string{
				"1997-09-22T09:00:00-04:00", "1997-10-20T09:00:00-04:00", "1997-11-17T09:00:00-05:00",
				"1997-12-22T09:00:00-05:00", "1998-01-19T09:00:00-05:00", "1998-02-16T09:00:00-05:00",
			},
		},
		{
			name:  "RFC third Tuesday-Wednesday-or-Thursday via BYSETPOS=3",
			event: rev(t, ny, "1997-09-04T09:00:00-04:00", "1997-09-04T10:00:00-04:00", "FREQ=MONTHLY;COUNT=3;BYDAY=TU,WE,TH;BYSETPOS=3"),
			from:  "1997-09-01T00:00:00Z", to: "1998-01-01T00:00:00Z",
			want: []string{
				"1997-09-04T09:00:00-04:00", "1997-10-07T09:00:00-04:00", "1997-11-06T09:00:00-05:00",
			},
		},
		{
			name:  "RFC second-to-last weekday of the month via BYSETPOS=-2",
			event: rev(t, ny, "1997-09-29T09:00:00-04:00", "1997-09-29T10:00:00-04:00", "FREQ=MONTHLY;BYDAY=MO,TU,WE,TH,FR;BYSETPOS=-2"),
			from:  "1997-09-01T00:00:00Z", to: "1998-01-01T00:00:00Z",
			want: []string{
				"1997-09-29T09:00:00-04:00", "1997-10-30T09:00:00-05:00", "1997-11-27T09:00:00-05:00",
				"1997-12-30T09:00:00-05:00",
			},
		},
		{
			name:  "RFC first and last Sunday every other month for 10 occurrences",
			event: rev(t, ny, "1997-09-07T09:00:00-04:00", "1997-09-07T10:00:00-04:00", "FREQ=MONTHLY;INTERVAL=2;COUNT=10;BYDAY=1SU,-1SU"),
			from:  "1997-09-01T00:00:00Z", to: "1999-01-01T00:00:00Z",
			want: []string{
				"1997-09-07T09:00:00-04:00", "1997-09-28T09:00:00-04:00", "1997-11-02T09:00:00-05:00",
				"1997-11-30T09:00:00-05:00", "1998-01-04T09:00:00-05:00", "1998-01-25T09:00:00-05:00",
				"1998-03-01T09:00:00-05:00", "1998-03-29T09:00:00-05:00", "1998-05-03T09:00:00-04:00",
				"1998-05-31T09:00:00-04:00",
			},
		},
		{
			name:  "RFC yearly in June and July for 10 occurrences",
			event: rev(t, ny, "1997-06-10T09:00:00-04:00", "1997-06-10T10:00:00-04:00", "FREQ=YEARLY;COUNT=10;BYMONTH=6,7"),
			from:  "1997-01-01T00:00:00Z", to: "2002-01-01T00:00:00Z",
			want: []string{
				"1997-06-10T09:00:00-04:00", "1997-07-10T09:00:00-04:00", "1998-06-10T09:00:00-04:00",
				"1998-07-10T09:00:00-04:00", "1999-06-10T09:00:00-04:00", "1999-07-10T09:00:00-04:00",
				"2000-06-10T09:00:00-04:00", "2000-07-10T09:00:00-04:00", "2001-06-10T09:00:00-04:00",
				"2001-07-10T09:00:00-04:00",
			},
		},
		{
			name:  "monthly third Monday ordinal crosses spring forward",
			event: rev(t, ny, "2024-01-15T10:00:00-05:00", "2024-01-15T11:00:00-05:00", "FREQ=MONTHLY;BYDAY=3MO;COUNT=3"),
			from:  "2024-01-01T00:00:00Z", to: "2025-01-01T00:00:00Z",
			want: []string{
				"2024-01-15T10:00:00-05:00", "2024-02-19T10:00:00-05:00", "2024-03-18T10:00:00-04:00",
			},
		},
		{
			name:  "monthly fourth Tuesday ordinal (the fred-m2 shape)",
			event: rev(t, ny, "2026-08-25T13:05:00-04:00", "2026-08-25T14:05:00-04:00", "FREQ=MONTHLY;BYDAY=4TU"),
			from:  "2026-08-01T00:00:00Z", to: "2027-01-01T00:00:00Z",
			want: []string{
				"2026-08-25T13:05:00-04:00", "2026-09-22T13:05:00-04:00", "2026-10-27T13:05:00-04:00",
				"2026-11-24T13:05:00-05:00", "2026-12-22T13:05:00-05:00",
			},
		},
		{
			name:  "monthly last Friday ordinal",
			event: rev(t, ny, "2024-01-26T10:00:00-05:00", "2024-01-26T11:00:00-05:00", "FREQ=MONTHLY;BYDAY=-1FR;COUNT=3"),
			from:  "2024-01-01T00:00:00Z", to: "2025-01-01T00:00:00Z",
			want: []string{
				"2024-01-26T10:00:00-05:00", "2024-02-23T10:00:00-05:00", "2024-03-29T10:00:00-04:00",
			},
		},
		{
			name:  "weekly Mondays keep 9am wall clock across spring forward",
			event: rev(t, ny, "2024-02-26T09:00:00-05:00", "2024-02-26T10:00:00-05:00", "FREQ=WEEKLY;BYDAY=MO;COUNT=4"),
			from:  "2024-02-01T00:00:00Z", to: "2024-04-01T00:00:00Z",
			want: []string{
				"2024-02-26T09:00:00-05:00", "2024-03-04T09:00:00-05:00",
				"2024-03-11T09:00:00-04:00", "2024-03-18T09:00:00-04:00",
			},
			wantEnds: []string{
				"2024-02-26T10:00:00-05:00", "2024-03-04T10:00:00-05:00",
				"2024-03-11T10:00:00-04:00", "2024-03-18T10:00:00-04:00",
			},
		},
		{
			name:  "daily keeps 9am wall clock across fall back",
			event: rev(t, ny, "2024-11-01T09:00:00-04:00", "2024-11-01T09:30:00-04:00", "FREQ=DAILY;COUNT=5"),
			from:  "2024-11-01T00:00:00Z", to: "2024-12-01T00:00:00Z",
			want: []string{
				"2024-11-01T09:00:00-04:00", "2024-11-02T09:00:00-04:00", "2024-11-03T09:00:00-05:00",
				"2024-11-04T09:00:00-05:00", "2024-11-05T09:00:00-05:00",
			},
		},
		{
			// 02:30 does not exist on 2024-03-10 in New York; time.Date
			// resolves it to 01:30 EST by normalization (see the anchor
			// DECISION) — pinned here so a Go behavior change is noticed.
			name:  "spring-forward gap normalizes the nonexistent wall time",
			event: rev(t, ny, "2024-03-09T02:30:00-05:00", "2024-03-09T03:30:00-05:00", "FREQ=DAILY;COUNT=2"),
			from:  "2024-03-01T00:00:00Z", to: "2024-04-01T00:00:00Z",
			want: []string{"2024-03-09T02:30:00-05:00", "2024-03-10T01:30:00-05:00"},
		},
		{
			name:  "UNTIL is inclusive of an instance landing exactly on it",
			event: rev(t, ny, "2024-02-26T09:00:00-05:00", "2024-02-26T10:00:00-05:00", "FREQ=WEEKLY;BYDAY=MO;UNTIL=2024-03-11T09:00:00"),
			from:  "2024-02-01T00:00:00Z", to: "2024-05-01T00:00:00Z",
			want: []string{
				"2024-02-26T09:00:00-05:00", "2024-03-04T09:00:00-05:00", "2024-03-11T09:00:00-04:00",
			},
		},
		{
			name:  "UNTIL a second earlier excludes that instance",
			event: rev(t, ny, "2024-02-26T09:00:00-05:00", "2024-02-26T10:00:00-05:00", "FREQ=WEEKLY;BYDAY=MO;UNTIL=2024-03-11T08:59:59"),
			from:  "2024-02-01T00:00:00Z", to: "2024-05-01T00:00:00Z",
			want: []string{"2024-02-26T09:00:00-05:00", "2024-03-04T09:00:00-05:00"},
		},
		{
			name:  "BYMONTH limits a weekly rule to January",
			event: rev(t, ny, "2024-01-02T09:00:00-05:00", "2024-01-02T10:00:00-05:00", "FREQ=WEEKLY;BYDAY=TU;BYMONTH=1"),
			from:  "2024-01-01T00:00:00Z", to: "2025-01-01T00:00:00Z",
			want: []string{
				"2024-01-02T09:00:00-05:00", "2024-01-09T09:00:00-05:00", "2024-01-16T09:00:00-05:00",
				"2024-01-23T09:00:00-05:00", "2024-01-30T09:00:00-05:00",
			},
		},
		{
			name:  "monthly on the 31st skips months without one",
			event: rev(t, ny, "2024-01-31T08:00:00-05:00", "2024-01-31T09:00:00-05:00", "FREQ=MONTHLY"),
			from:  "2024-01-01T00:00:00Z", to: "2024-07-01T00:00:00Z",
			want: []string{
				"2024-01-31T08:00:00-05:00", "2024-03-31T08:00:00-04:00", "2024-05-31T08:00:00-04:00",
			},
		},
		{
			name: "BYMONTHDAY list (the korea-trass shape) picks both days each month",
			event: rev(t, "Asia/Seoul", "2026-09-05T09:00:00+09:00", "2026-09-05T10:00:00+09:00",
				"FREQ=MONTHLY;BYMONTHDAY=5,20;COUNT=5"),
			from: "2026-09-01T00:00:00Z", to: "2027-01-01T00:00:00Z",
			want: []string{
				"2026-09-05T09:00:00+09:00", "2026-09-20T09:00:00+09:00", "2026-10-05T09:00:00+09:00",
				"2026-10-20T09:00:00+09:00", "2026-11-05T09:00:00+09:00",
			},
		},
		{
			// A single event has no TZ; its stored instants pass through.
			name:  "single event inside the window",
			event: rev(t, "", "2024-05-01T12:00:00Z", "2024-05-01T13:00:00Z", ""),
			from:  "2024-05-01T00:00:00Z", to: "2024-05-02T00:00:00Z",
			want:  []string{"2024-05-01T12:00:00Z"},
			wantEnds: []string{
				"2024-05-01T13:00:00Z",
			},
		},
		{
			name:  "single event outside the window",
			event: rev(t, "", "2024-05-01T12:00:00Z", "2024-05-01T13:00:00Z", ""),
			from:  "2024-06-01T00:00:00Z", to: "2024-07-01T00:00:00Z",
			want:  nil,
		},
		{
			name:  "single event straddling the window start still intersects",
			event: rev(t, "", "2024-05-01T09:00:00Z", "2024-05-01T11:00:00Z", ""),
			from:  "2024-05-01T10:00:00Z", to: "2024-05-02T00:00:00Z",
			want:  []string{"2024-05-01T09:00:00Z"},
		},
		{
			name:  "a rule that never matches terminates at the safety cap",
			event: rev(t, ny, "2024-02-01T09:00:00-05:00", "2024-02-01T10:00:00-05:00", "FREQ=YEARLY;BYMONTH=2;BYMONTHDAY=30;COUNT=3"),
			from:  "2024-01-01T00:00:00Z", to: "2030-01-01T00:00:00Z",
			want:  nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := expandEvent(tc.event, mustInstant(t, tc.from), mustInstant(t, tc.to))
			if err != nil {
				t.Fatalf("expandEvent: %v", err)
			}
			var starts, ends []string
			for _, in := range got {
				starts = append(starts, in.StartsAt.Format(time.RFC3339))
				ends = append(ends, in.EndsAt.Format(time.RFC3339))
			}
			if !reflect.DeepEqual(starts, tc.want) {
				t.Errorf("starts =\n  %v\nwant\n  %v", starts, tc.want)
			}
			if tc.wantEnds != nil && !reflect.DeepEqual(ends, tc.wantEnds) {
				t.Errorf("ends =\n  %v\nwant\n  %v", ends, tc.wantEnds)
			}
		})
	}
}

// TestExpandEventCarriesEventFields: an instance is a projection of its master
// event — every field the contract names comes through, and Recurring reports
// whether an RRULE produced it.
func TestExpandEventCarriesEventFields(t *testing.T) {
	ev := rev(t, "America/New_York", "2024-02-26T09:00:00-05:00", "2024-02-26T10:00:00-05:00", "FREQ=WEEKLY;BYDAY=MO;COUNT=2")
	ev.CalendarID = "cal_TEST"
	ev.Location = "room 3"
	ev.Notes = "bring the charts"
	ev.Automation = "fred-m2"
	ev.CreatedBy = "bot_ADA"
	got, err := expandEvent(ev, mustInstant(t, "2024-02-01T00:00:00Z"), mustInstant(t, "2024-04-01T00:00:00Z"))
	if err != nil || len(got) != 2 {
		t.Fatalf("expandEvent = %d instances, err %v, want 2", len(got), err)
	}
	for _, in := range got {
		if in.EventID != ev.ID || in.CalendarID != ev.CalendarID || in.Title != ev.Title ||
			in.Location != ev.Location || in.Notes != ev.Notes || in.Automation != ev.Automation ||
			in.CreatedBy != ev.CreatedBy {
			t.Errorf("instance %+v does not carry the event's fields %+v", in, ev)
		}
		if !in.Recurring {
			t.Errorf("instance %+v of a recurring event is not marked recurring", in)
		}
	}

	single := rev(t, "", "2024-05-01T12:00:00Z", "2024-05-01T13:00:00Z", "")
	got, err = expandEvent(single, mustInstant(t, "2024-05-01T00:00:00Z"), mustInstant(t, "2024-05-02T00:00:00Z"))
	if err != nil || len(got) != 1 {
		t.Fatalf("expandEvent(single) = %d instances, err %v, want 1", len(got), err)
	}
	if got[0].Recurring {
		t.Error("a single event's instance is marked recurring")
	}
}

// TestParseRRULERejects: every unsupported or malformed rule is refused with
// an error naming the offense — never silently dropped — and unsupported
// params echo the supported subset so the errors teach.
func TestParseRRULERejects(t *testing.T) {
	cases := []struct {
		rrule, want string
	}{
		{"FREQ=HOURLY", "not supported"},
		{"COUNT=4", "FREQ is required"},
		{"FREQ=WEEKLY;COUNT=4;UNTIL=20240101T000000Z", "mutually exclusive"},
		{"FREQ=WEEKLY;BYWEEKNO=2", "BYWEEKNO is not supported"},
		{"FREQ=DAILY;BYHOUR=9", "BYHOUR is not supported"},
		{"FREQ=DAILY;FOO=1", "FOO is not supported"},
		{"FREQ=DAILY;FOO=1", "supported: FREQ, INTERVAL, COUNT, UNTIL, BYDAY, BYMONTHDAY, BYMONTH, BYSETPOS, WKST"},
		{"FREQ=WEEKLY;BYDAY=3MO", "ordinal BYDAY"},
		{"FREQ=WEEKLY;BYMONTHDAY=13", "FREQ=WEEKLY"},
		{"FREQ=YEARLY;BYDAY=MO", "requires BYMONTH"},
		{"FREQ=MONTHLY;BYSETPOS=1", "BYSETPOS requires another BY"},
		{"FREQ=MONTHLY;INTERVAL=0", "INTERVAL"},
		{"FREQ=MONTHLY;COUNT=0", "COUNT"},
		{"FREQ=MONTHLY;BYDAY=XX", "BYDAY"},
		{"FREQ=MONTHLY;BYMONTHDAY=0", "BYMONTHDAY"},
		{"FREQ=MONTHLY;BYMONTH=13", "BYMONTH"},
		{"FREQ=WEEKLY;UNTIL=notatime", "UNTIL"},
		{"FREQ=WEEKLY;;COUNT=2", "KEY=VALUE"},
	}
	for _, tc := range cases {
		if _, err := parseRRULE(tc.rrule); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("parseRRULE(%q) = %v, want error containing %q", tc.rrule, err, tc.want)
		}
	}
}
