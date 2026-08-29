package calendar

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func tev(tz, start, end, rrule string, exdate ...string) Event {
	return Event{ID: "evt_TEST", Title: "t", TZ: tz, Start: start, End: end, RRULE: rrule, EXDATE: exdate}
}

func mustRFC3339(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("bad test window time %q: %v", s, err)
	}
	return v
}

// TestExpand pins the expander. Fixtures marked "RFC" are transcribed from
// the worked examples in RFC 5545 §3.8.5.3 (DTSTART 1997, America/New_York;
// EDT/EST offsets in the wants come from the RFC's own annotations).
func TestExpand(t *testing.T) {
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
			event: tev(ny, "1997-09-02T09:00:00", "1997-09-02T10:00:00", "FREQ=DAILY;COUNT=10"),
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
			event: tev(ny, "1997-09-02T09:00:00", "1997-09-02T10:00:00", "FREQ=DAILY;INTERVAL=10;COUNT=5"),
			from:  "1997-09-01T00:00:00Z", to: "1998-01-01T00:00:00Z",
			want: []string{
				"1997-09-02T09:00:00-04:00", "1997-09-12T09:00:00-04:00", "1997-09-22T09:00:00-04:00",
				"1997-10-02T09:00:00-04:00", "1997-10-12T09:00:00-04:00",
			},
		},
		{
			name:  "RFC weekly for 10 occurrences crosses the EDT-to-EST fall-back",
			event: tev(ny, "1997-09-02T09:00:00", "1997-09-02T10:00:00", "FREQ=WEEKLY;COUNT=10"),
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
			event: tev(ny, "1997-09-01T09:00:00", "1997-09-01T10:00:00",
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
			event: tev(ny, "1997-09-02T09:00:00", "1997-09-02T10:00:00", "FREQ=MONTHLY;BYDAY=FR;BYMONTHDAY=13"),
			from:  "1997-09-01T00:00:00Z", to: "2000-12-31T00:00:00Z",
			want: []string{
				"1998-02-13T09:00:00-05:00", "1998-03-13T09:00:00-05:00", "1998-11-13T09:00:00-05:00",
				"1999-08-13T09:00:00-04:00", "2000-10-13T09:00:00-04:00",
			},
		},
		{
			name:  "RFC second-to-last Monday of the month for 6 months",
			event: tev(ny, "1997-09-22T09:00:00", "1997-09-22T10:00:00", "FREQ=MONTHLY;COUNT=6;BYDAY=-2MO"),
			from:  "1997-09-01T00:00:00Z", to: "1998-06-01T00:00:00Z",
			want: []string{
				"1997-09-22T09:00:00-04:00", "1997-10-20T09:00:00-04:00", "1997-11-17T09:00:00-05:00",
				"1997-12-22T09:00:00-05:00", "1998-01-19T09:00:00-05:00", "1998-02-16T09:00:00-05:00",
			},
		},
		{
			name:  "RFC third Tuesday-Wednesday-or-Thursday via BYSETPOS=3",
			event: tev(ny, "1997-09-04T09:00:00", "1997-09-04T10:00:00", "FREQ=MONTHLY;COUNT=3;BYDAY=TU,WE,TH;BYSETPOS=3"),
			from:  "1997-09-01T00:00:00Z", to: "1998-01-01T00:00:00Z",
			want: []string{
				"1997-09-04T09:00:00-04:00", "1997-10-07T09:00:00-04:00", "1997-11-06T09:00:00-05:00",
			},
		},
		{
			name:  "RFC second-to-last weekday of the month via BYSETPOS=-2",
			event: tev(ny, "1997-09-29T09:00:00", "1997-09-29T10:00:00", "FREQ=MONTHLY;BYDAY=MO,TU,WE,TH,FR;BYSETPOS=-2"),
			from:  "1997-09-01T00:00:00Z", to: "1998-01-01T00:00:00Z",
			want: []string{
				"1997-09-29T09:00:00-04:00", "1997-10-30T09:00:00-05:00", "1997-11-27T09:00:00-05:00",
				"1997-12-30T09:00:00-05:00",
			},
		},
		{
			name:  "RFC first and last Sunday every other month for 10 occurrences",
			event: tev(ny, "1997-09-07T09:00:00", "1997-09-07T10:00:00", "FREQ=MONTHLY;INTERVAL=2;COUNT=10;BYDAY=1SU,-1SU"),
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
			event: tev(ny, "1997-06-10T09:00:00", "1997-06-10T10:00:00", "FREQ=YEARLY;COUNT=10;BYMONTH=6,7"),
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
			event: tev(ny, "2024-01-15T10:00:00", "2024-01-15T11:00:00", "FREQ=MONTHLY;BYDAY=3MO;COUNT=3"),
			from:  "2024-01-01T00:00:00Z", to: "2025-01-01T00:00:00Z",
			want: []string{
				"2024-01-15T10:00:00-05:00", "2024-02-19T10:00:00-05:00", "2024-03-18T10:00:00-04:00",
			},
		},
		{
			name:  "monthly last Friday ordinal",
			event: tev(ny, "2024-01-26T10:00:00", "2024-01-26T11:00:00", "FREQ=MONTHLY;BYDAY=-1FR;COUNT=3"),
			from:  "2024-01-01T00:00:00Z", to: "2025-01-01T00:00:00Z",
			want: []string{
				"2024-01-26T10:00:00-05:00", "2024-02-23T10:00:00-05:00", "2024-03-29T10:00:00-04:00",
			},
		},
		{
			name:  "weekly Mondays keep 9am wall clock across spring forward",
			event: tev(ny, "2024-02-26T09:00:00", "2024-02-26T10:00:00", "FREQ=WEEKLY;BYDAY=MO;COUNT=4"),
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
			event: tev(ny, "2024-11-01T09:00:00", "2024-11-01T09:30:00", "FREQ=DAILY;COUNT=5"),
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
			event: tev(ny, "2024-03-09T02:30:00", "2024-03-09T03:30:00", "FREQ=DAILY;COUNT=2"),
			from:  "2024-03-01T00:00:00Z", to: "2024-04-01T00:00:00Z",
			want: []string{"2024-03-09T02:30:00-05:00", "2024-03-10T01:30:00-05:00"},
		},
		{
			name: "EXDATE removes an instance but still consumes COUNT",
			event: tev(ny, "2024-02-26T09:00:00", "2024-02-26T10:00:00", "FREQ=WEEKLY;BYDAY=MO;COUNT=4",
				"2024-03-04T09:00:00"),
			from: "2024-02-01T00:00:00Z", to: "2024-05-01T00:00:00Z",
			want: []string{
				"2024-02-26T09:00:00-05:00", "2024-03-11T09:00:00-04:00", "2024-03-18T09:00:00-04:00",
			},
		},
		{
			name:  "UNTIL is inclusive of an instance landing exactly on it",
			event: tev(ny, "2024-02-26T09:00:00", "2024-02-26T10:00:00", "FREQ=WEEKLY;BYDAY=MO;UNTIL=2024-03-11T09:00:00"),
			from:  "2024-02-01T00:00:00Z", to: "2024-05-01T00:00:00Z",
			want: []string{
				"2024-02-26T09:00:00-05:00", "2024-03-04T09:00:00-05:00", "2024-03-11T09:00:00-04:00",
			},
		},
		{
			name:  "UNTIL a second earlier excludes that instance",
			event: tev(ny, "2024-02-26T09:00:00", "2024-02-26T10:00:00", "FREQ=WEEKLY;BYDAY=MO;UNTIL=2024-03-11T08:59:59"),
			from:  "2024-02-01T00:00:00Z", to: "2024-05-01T00:00:00Z",
			want: []string{"2024-02-26T09:00:00-05:00", "2024-03-04T09:00:00-05:00"},
		},
		{
			name:  "BYMONTH limits a weekly rule to January",
			event: tev(ny, "2024-01-02T09:00:00", "2024-01-02T10:00:00", "FREQ=WEEKLY;BYDAY=TU;BYMONTH=1"),
			from:  "2024-01-01T00:00:00Z", to: "2025-01-01T00:00:00Z",
			want: []string{
				"2024-01-02T09:00:00-05:00", "2024-01-09T09:00:00-05:00", "2024-01-16T09:00:00-05:00",
				"2024-01-23T09:00:00-05:00", "2024-01-30T09:00:00-05:00",
			},
		},
		{
			name:  "monthly on the 31st skips months without one",
			event: tev(ny, "2024-01-31T08:00:00", "2024-01-31T09:00:00", "FREQ=MONTHLY"),
			from:  "2024-01-01T00:00:00Z", to: "2024-07-01T00:00:00Z",
			want: []string{
				"2024-01-31T08:00:00-05:00", "2024-03-31T08:00:00-04:00", "2024-05-31T08:00:00-04:00",
			},
		},
		{
			name:  "single event inside the window",
			event: tev("UTC", "2024-05-01T12:00:00", "2024-05-01T13:00:00", ""),
			from:  "2024-05-01T00:00:00Z", to: "2024-05-02T00:00:00Z",
			want: []string{"2024-05-01T12:00:00Z"},
			wantEnds: []string{
				"2024-05-01T13:00:00Z",
			},
		},
		{
			name:  "single event outside the window",
			event: tev("UTC", "2024-05-01T12:00:00", "2024-05-01T13:00:00", ""),
			from:  "2024-06-01T00:00:00Z", to: "2024-07-01T00:00:00Z",
			want: nil,
		},
		{
			name:  "single event straddling the window start still intersects",
			event: tev("UTC", "2024-05-01T09:00:00", "2024-05-01T11:00:00", ""),
			from:  "2024-05-01T10:00:00Z", to: "2024-05-02T00:00:00Z",
			want: []string{"2024-05-01T09:00:00Z"},
		},
		{
			name: "all-day single event spans local midnight to midnight",
			event: Event{ID: "evt_TEST", Title: "t", TZ: ny, AllDay: true,
				Start: "2024-05-01", End: "2024-05-02"},
			from: "2024-04-30T00:00:00Z", to: "2024-05-03T00:00:00Z",
			want: []string{"2024-05-01T00:00:00-04:00"},
			wantEnds: []string{
				"2024-05-02T00:00:00-04:00",
			},
		},
		{
			name: "all-day weekly Saturdays",
			event: Event{ID: "evt_TEST", Title: "t", TZ: ny, AllDay: true,
				Start: "2024-06-01", End: "2024-06-02", RRULE: "FREQ=WEEKLY;BYDAY=SA;COUNT=3"},
			from: "2024-05-01T00:00:00Z", to: "2024-07-01T00:00:00Z",
			want: []string{
				"2024-06-01T00:00:00-04:00", "2024-06-08T00:00:00-04:00", "2024-06-15T00:00:00-04:00",
			},
			wantEnds: []string{
				"2024-06-02T00:00:00-04:00", "2024-06-09T00:00:00-04:00", "2024-06-16T00:00:00-04:00",
			},
		},
		{
			name:  "a rule that never matches terminates at the safety cap",
			event: tev(ny, "2024-02-01T09:00:00", "2024-02-01T10:00:00", "FREQ=YEARLY;BYMONTH=2;BYMONTHDAY=30;COUNT=3"),
			from:  "2024-01-01T00:00:00Z", to: "2030-01-01T00:00:00Z",
			want: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Expand(tc.event, mustRFC3339(t, tc.from), mustRFC3339(t, tc.to))
			if err != nil {
				t.Fatalf("Expand: %v", err)
			}
			var starts, ends []string
			for _, in := range got {
				starts = append(starts, in.Start.Format(time.RFC3339))
				ends = append(ends, in.End.Format(time.RFC3339))
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

func TestExpandCopiesEventFields(t *testing.T) {
	ev := tev("UTC", "2024-05-01T12:00:00", "2024-05-01T13:00:00", "")
	ev.Location = "room 3"
	got, err := Expand(ev, mustRFC3339(t, "2024-05-01T00:00:00Z"), mustRFC3339(t, "2024-05-02T00:00:00Z"))
	if err != nil || len(got) != 1 {
		t.Fatalf("Expand = %v instances, err %v, want 1", len(got), err)
	}
	in := got[0]
	if in.EventID != ev.ID || in.Title != ev.Title || in.Location != ev.Location || in.AllDay != ev.AllDay {
		t.Errorf("instance %+v does not carry the event's fields %+v", in, ev)
	}
}

func TestParseRRULERejects(t *testing.T) {
	cases := []struct {
		rrule, want string
	}{
		{"FREQ=HOURLY", "not supported in v1"},
		{"COUNT=4", "FREQ is required"},
		{"FREQ=WEEKLY;COUNT=4;UNTIL=20240101T000000Z", "mutually exclusive"},
		{"FREQ=WEEKLY;BYWEEKNO=2", "BYWEEKNO is not supported in v1"},
		{"FREQ=DAILY;BYHOUR=9", "BYHOUR is not supported in v1"},
		{"FREQ=DAILY;FOO=1", "FOO is not supported in v1"},
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

func TestValidateEvent(t *testing.T) {
	valid := tev("America/New_York", "2024-03-04T09:00:00", "2024-03-04T10:00:00", "FREQ=WEEKLY;BYDAY=MO", "2024-03-11T09:00:00")
	if err := validateEvent(valid); err != nil {
		t.Fatalf("valid event rejected: %v", err)
	}

	cases := []struct {
		name string
		mut  func(*Event)
		want string
	}{
		{"missing title", func(e *Event) { e.Title = " " }, "title"},
		{"missing tz", func(e *Event) { e.TZ = "" }, "tz is required"},
		{"unknown tz", func(e *Event) { e.TZ = "Mars/Olympus" }, "unknown tz"},
		{"missing start", func(e *Event) { e.Start = "" }, "start and end are required"},
		{"offset in wall time", func(e *Event) { e.Start = "2024-03-04T09:00:00Z" }, "wall-clock"},
		{"end before start", func(e *Event) { e.End = "2024-03-04T08:00:00" }, "must be after"},
		{"end equal to start", func(e *Event) { e.End = e.Start }, "must be after"},
		{"bad rrule", func(e *Event) { e.RRULE = "FREQ=SECONDLY" }, "not supported"},
		{"bad exdate", func(e *Event) { e.EXDATE = []string{"tomorrow"} }, "exdate"},
		{"all-day with clock times", func(e *Event) { e.AllDay = true }, "wall-clock"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := valid
			ev.EXDATE = append([]string(nil), valid.EXDATE...)
			tc.mut(&ev)
			if err := validateEvent(ev); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("validateEvent = %v, want error containing %q", err, tc.want)
			}
		})
	}

	allDay := Event{Title: "t", TZ: "UTC", AllDay: true, Start: "2024-05-01", End: "2024-05-02"}
	if err := validateEvent(allDay); err != nil {
		t.Errorf("valid all-day event rejected: %v", err)
	}
	allDay.End = "2024-05-01"
	if err := validateEvent(allDay); err == nil || !strings.Contains(err.Error(), "exclusive") {
		t.Errorf("same-day all-day end = %v, want exclusive-end error", err)
	}
}
