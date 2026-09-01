package botnet

// The RRULE expander: PORTED from go/std/bg_services/calendar (now retired),
// adapted to botnet's Event. The engine is unchanged — parse a supported
// RFC 5545 subset, iterate period-by-period on a wall-clock carrier, anchor
// each occurrence into the event's zone — so the std suite's table-driven
// DST/ordinal/BYSETPOS fixtures pin it here too (rrule_test.go). What the
// adaptation changes is only the boundary: botnet stores absolute RFC3339 UTC
// instants (StartsAt/EndsAt, the FIRST occurrence), so expandEvent first
// recovers the wall clock from StartsAt in TZ, and botnet's v1 has no EXDATE
// and no all-day events, so those two knobs did not come along.

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "time/tzdata" // embed the IANA zone db; never depend on host /usr/share/zoneinfo
)

// maxPeriods caps how many candidate periods expansion will walk, so a rule
// that never matches (FREQ=YEARLY;BYMONTH=2;BYMONTHDAY=30) cannot spin forever
// hunting for COUNT instances. Generous: 100k daily periods is ~274 years.
const maxPeriods = 100000

type freq int

const (
	freqDaily freq = iota
	freqWeekly
	freqMonthly
	freqYearly
)

// byDayEntry is one BYDAY value: a weekday, optionally with an ordinal (3MO =
// third Monday of the period, -1FR = last Friday). ord 0 means every such
// weekday. Ordinals are only meaningful (and only accepted) for MONTHLY and
// YEARLY rules.
type byDayEntry struct {
	ord int
	wd  time.Weekday
}

// rrule is a parsed RRULE, restricted to the supported subset. Anything
// outside it is rejected by parseRRULE with an error naming the offending
// param — never silently dropped.
type rrule struct {
	freq       freq
	interval   int       // >= 1; default 1
	count      int       // 0 = unset; mutually exclusive with until
	until      time.Time // wall-clock carrier, inclusive; zero = unset
	byDay      []byDayEntry
	byMonthDay []int        // 1..31 or -31..-1 (from month end)
	byMonth    []time.Month // sorted at parse
	bySetPos   []int        // nonzero, 1-based / negative-from-end per period
	wkst       time.Weekday // week start for WEEKLY alignment; default Monday
}

var weekdayCodes = map[string]time.Weekday{
	"SU": time.Sunday, "MO": time.Monday, "TU": time.Tuesday, "WE": time.Wednesday,
	"TH": time.Thursday, "FR": time.Friday, "SA": time.Saturday,
}

// rruleWallLayout is the wall-clock form UNTIL accepts alongside the RFC-basic
// ones: RFC3339 without an offset, matching the rule's wall-clock semantics.
const (
	rruleWallLayout = "2006-01-02T15:04:05"
	rruleDateLayout = "2006-01-02"
)

// parseRRULE parses an RFC 5545 recurrence rule string like
// "FREQ=WEEKLY;BYDAY=MO;COUNT=4". It runs at the write boundary (so bad rules
// never reach the store) and again before expansion. Its errors are the
// instructive text the tool and REST paths surface, so they teach: an
// unsupported param echoes the whole supported subset.
func parseRRULE(s string) (*rrule, error) {
	r := &rrule{interval: 1, wkst: time.Monday}
	seenFreq := false
	for _, part := range strings.Split(strings.TrimPrefix(s, "RRULE:"), ";") {
		key, val, ok := strings.Cut(part, "=")
		if !ok || val == "" {
			return nil, fmt.Errorf("rrule: %q is not a KEY=VALUE param", part)
		}
		key, val = strings.ToUpper(key), strings.ToUpper(val)
		switch key {
		case "FREQ":
			seenFreq = true
			switch val {
			case "DAILY":
				r.freq = freqDaily
			case "WEEKLY":
				r.freq = freqWeekly
			case "MONTHLY":
				r.freq = freqMonthly
			case "YEARLY":
				r.freq = freqYearly
			case "HOURLY", "MINUTELY", "SECONDLY":
				return nil, fmt.Errorf("rrule: FREQ=%s is not supported (sub-daily frequencies are out of scope)", val)
			default:
				return nil, fmt.Errorf("rrule: unknown FREQ %q (want DAILY, WEEKLY, MONTHLY or YEARLY)", val)
			}
		case "INTERVAL":
			n, err := strconv.Atoi(val)
			if err != nil || n < 1 {
				return nil, fmt.Errorf("rrule: INTERVAL %q must be a positive integer", val)
			}
			r.interval = n
		case "COUNT":
			n, err := strconv.Atoi(val)
			if err != nil || n < 1 {
				return nil, fmt.Errorf("rrule: COUNT %q must be a positive integer", val)
			}
			r.count = n
		case "UNTIL":
			t, err := parseUntil(val)
			if err != nil {
				return nil, err
			}
			r.until = t
		case "BYDAY":
			for _, entry := range strings.Split(val, ",") {
				bd, err := parseByDay(entry)
				if err != nil {
					return nil, err
				}
				r.byDay = append(r.byDay, bd)
			}
		case "BYMONTHDAY":
			for _, entry := range strings.Split(val, ",") {
				n, err := strconv.Atoi(entry)
				if err != nil || n == 0 || n > 31 || n < -31 {
					return nil, fmt.Errorf("rrule: BYMONTHDAY entry %q must be in 1..31 or -31..-1", entry)
				}
				r.byMonthDay = append(r.byMonthDay, n)
			}
		case "BYMONTH":
			for _, entry := range strings.Split(val, ",") {
				n, err := strconv.Atoi(entry)
				if err != nil || n < 1 || n > 12 {
					return nil, fmt.Errorf("rrule: BYMONTH entry %q must be in 1..12", entry)
				}
				r.byMonth = append(r.byMonth, time.Month(n))
			}
			sort.Slice(r.byMonth, func(i, j int) bool { return r.byMonth[i] < r.byMonth[j] })
		case "BYSETPOS":
			for _, entry := range strings.Split(val, ",") {
				n, err := strconv.Atoi(entry)
				if err != nil || n == 0 || n > 366 || n < -366 {
					return nil, fmt.Errorf("rrule: BYSETPOS entry %q must be a nonzero integer in -366..366", entry)
				}
				r.bySetPos = append(r.bySetPos, n)
			}
		case "WKST":
			wd, ok := weekdayCodes[val]
			if !ok {
				return nil, fmt.Errorf("rrule: WKST %q must be a weekday code (SU..SA)", val)
			}
			r.wkst = wd
		default:
			return nil, fmt.Errorf("rrule: param %s is not supported (supported: FREQ, INTERVAL, COUNT, UNTIL, BYDAY, BYMONTHDAY, BYMONTH, BYSETPOS, WKST)", key)
		}
	}
	if !seenFreq {
		return nil, fmt.Errorf("rrule: FREQ is required")
	}
	if r.count > 0 && !r.until.IsZero() {
		return nil, fmt.Errorf("rrule: COUNT and UNTIL are mutually exclusive (RFC 5545)")
	}
	if r.freq == freqDaily || r.freq == freqWeekly {
		for _, bd := range r.byDay {
			if bd.ord != 0 {
				return nil, fmt.Errorf("rrule: ordinal BYDAY (%+d%s) requires FREQ=MONTHLY or YEARLY", bd.ord, dayCode(bd.wd))
			}
		}
	}
	if r.freq == freqWeekly && len(r.byMonthDay) > 0 {
		return nil, fmt.Errorf("rrule: BYMONTHDAY cannot be combined with FREQ=WEEKLY (RFC 5545)")
	}
	if r.freq == freqYearly && len(r.byDay) > 0 && len(r.byMonth) == 0 {
		return nil, fmt.Errorf("rrule: FREQ=YEARLY with BYDAY requires BYMONTH (full-year weekday expansion is not supported)")
	}
	if len(r.bySetPos) > 0 && len(r.byDay)+len(r.byMonthDay)+len(r.byMonth) == 0 {
		return nil, fmt.Errorf("rrule: BYSETPOS requires another BY* part to select from (RFC 5545)")
	}
	return r, nil
}

// parseUntil reads an UNTIL value.
//
// DECISION (kept from the std expander): UNTIL is compared on the wall clock
// in the event's zone, inclusive, per RFC. The RFC-basic forms
// ("19971224T000000Z", with or without the Z) are accepted so canonical rules
// transcribe directly, but a Z suffix is still read as wall clock — the rule
// carries no absolute times.
func parseUntil(val string) (time.Time, error) {
	v := strings.TrimSuffix(val, "Z")
	for _, layout := range []string{rruleWallLayout, rruleDateLayout, "20060102T150405", "20060102"} {
		if t, err := time.ParseInLocation(layout, v, time.UTC); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("rrule: bad UNTIL %q (want %s, %s, or RFC basic 20060102T150405)", val, rruleWallLayout, rruleDateLayout)
}

func parseByDay(entry string) (byDayEntry, error) {
	code := entry
	ord := 0
	if len(entry) > 2 {
		n, err := strconv.Atoi(entry[:len(entry)-2])
		if err != nil || n == 0 || n > 53 || n < -53 {
			return byDayEntry{}, fmt.Errorf("rrule: bad BYDAY entry %q (want a weekday code like MO, optionally with an ordinal like 3MO or -1FR)", entry)
		}
		ord = n
		code = entry[len(entry)-2:]
	}
	wd, ok := weekdayCodes[code]
	if !ok {
		return byDayEntry{}, fmt.Errorf("rrule: bad BYDAY weekday %q (want SU, MO, TU, WE, TH, FR or SA)", entry)
	}
	return byDayEntry{ord: ord, wd: wd}, nil
}

func dayCode(wd time.Weekday) string {
	return [...]string{"SU", "MO", "TU", "WE", "TH", "FR", "SA"}[wd]
}

// expandEvent returns the concrete instances of ev intersecting [from, to),
// sorted ascending by start. A single event (no RRule) passes through as one
// instance under the same overlap rule ListEvents uses (endsAt > from AND
// startsAt < to). A recurring event iterates on the wall-clock carrier
// recovered from StartsAt in TZ; only the emitted instants are anchored back
// into the zone, which is what keeps a "9am weekly" at 9am on both sides of a
// DST switch.
//
// DECISION (DTSTART, kept from the std expander): the event's StartsAt is not
// special-cased — instances come purely from the rule, iterated
// period-by-period from StartsAt (never from `from`, because COUNT counts
// from DTSTART). An event whose start does not itself match its RRULE simply
// does not yield that phantom first instance.
//
// DECISION (instance end, kept): each instance's end is the wall-clock delta
// between the event's start and end carriers added to the instance's wall
// start, then anchored — so a 9–10am meeting is 9–10am on both sides of a DST
// switch, not a fixed absolute duration.
func expandEvent(ev Event, from, to time.Time) ([]Instance, error) {
	if ev.RRule == "" {
		if ev.EndsAt.After(from) && ev.StartsAt.Before(to) {
			return []Instance{instanceOf(ev, ev.StartsAt, ev.EndsAt, false)}, nil
		}
		return nil, nil
	}
	loc, err := time.LoadLocation(ev.TZ)
	if err != nil {
		return nil, fmt.Errorf("event %s: unknown tz %q: %w", ev.ID, ev.TZ, err)
	}
	r, err := parseRRULE(ev.RRule)
	if err != nil {
		return nil, fmt.Errorf("event %s: %w", ev.ID, err)
	}
	startW := wallCarrier(ev.StartsAt, loc)
	dur := wallCarrier(ev.EndsAt, loc).Sub(startW)

	var out []Instance
	for _, w := range r.occurrences(startW, wallLimit(to, loc)) {
		st := anchor(w, loc)
		en := anchor(w.Add(dur), loc)
		if !st.Before(to) || !en.After(from) {
			continue
		}
		out = append(out, instanceOf(ev, st, en, true))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartsAt.Before(out[j].StartsAt) })
	return out, nil
}

// instanceOf projects one occurrence of a master event.
func instanceOf(ev Event, st, en time.Time, recurring bool) Instance {
	return Instance{
		EventID: ev.ID, CalendarID: ev.CalendarID, Title: ev.Title,
		StartsAt: st, EndsAt: en,
		Location: ev.Location, Notes: ev.Notes, Automation: ev.Automation,
		Recurring: recurring, CreatedBy: ev.CreatedBy,
	}
}

// wallCarrier strips an absolute instant to its wall-clock components in loc,
// carried in time.UTC. UTC here is NOT a claim about the zone — it is a pure
// calendar-arithmetic space with no DST, so recurrence iteration can add days
// and months without ever crossing a transition. anchor() is the inverse.
func wallCarrier(t time.Time, loc *time.Location) time.Time {
	w := t.In(loc)
	return time.Date(w.Year(), w.Month(), w.Day(), w.Hour(), w.Minute(), w.Second(), 0, time.UTC)
}

// anchor turns a wall-clock carrier into the absolute instant with those wall
// components in loc.
//
// DECISION (nonexistent wall times, kept from the std expander): a wall time
// inside a spring-forward gap (e.g. 02:30 on a US DST start date) does not
// exist in loc; time.Date resolves it to a real instant by normalization (for
// the US gap, 02:30 comes back as 01:30 standard time). That is the defined
// behavior, not fought — skipping or erroring would lose instances users
// expect to see.
func anchor(w time.Time, loc *time.Location) time.Time {
	return time.Date(w.Year(), w.Month(), w.Day(), w.Hour(), w.Minute(), w.Second(), 0, loc)
}

// wallLimit converts the window's exclusive end into a wall-clock carrier
// bound (plus a day of slack for zone skew): once a period starts past it, no
// candidate can fall inside the window and iteration stops.
func wallLimit(to time.Time, loc *time.Location) time.Time {
	t := to.In(loc)
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), 0, time.UTC).AddDate(0, 0, 1)
}

// occurrences generates the rule's wall-clock instance starts from startW
// (DTSTART) up to limit. Per period: candidates are generated (expansions) or
// filtered (limits), sorted, BYSETPOS selects, candidates before DTSTART are
// dropped, then COUNT/UNTIL terminate. The window filter is the caller's,
// applied after.
func (r *rrule) occurrences(startW, limit time.Time) []time.Time {
	var out []time.Time
	emitted := 0
	emit := func(cands []time.Time) (stop bool) {
		for _, c := range applySetPos(cands, r.bySetPos) {
			if c.Before(startW) {
				continue
			}
			if !r.until.IsZero() && c.After(r.until) {
				return true
			}
			if r.count > 0 {
				if emitted == r.count {
					return true
				}
				emitted++
			}
			out = append(out, c)
		}
		return false
	}

	switch r.freq {
	case freqDaily:
		r.daily(startW, limit, emit)
	case freqWeekly:
		r.weekly(startW, limit, emit)
	case freqMonthly:
		r.monthly(startW, limit, emit)
	case freqYearly:
		r.yearly(startW, limit, emit)
	}
	return out
}

func (r *rrule) daily(startW, limit time.Time, emit func([]time.Time) bool) {
	clock := clockOf(startW)
	day := dayOf(startW)
	for i := 0; i < maxPeriods; i++ {
		if day.After(limit) || r.pastUntil(day) {
			return
		}
		if r.dayAllowed(day) && emit([]time.Time{day.Add(clock)}) {
			return
		}
		day = day.AddDate(0, 0, r.interval)
	}
}

func (r *rrule) weekly(startW, limit time.Time, emit func([]time.Time) bool) {
	clock := clockOf(startW)
	weekStart := startOfWeek(dayOf(startW), r.wkst)
	weekdays := make(map[time.Weekday]bool, len(r.byDay))
	for _, bd := range r.byDay {
		weekdays[bd.wd] = true
	}
	if len(weekdays) == 0 {
		weekdays[startW.Weekday()] = true
	}
	for i := 0; i < maxPeriods; i++ {
		if weekStart.After(limit) || r.pastUntil(weekStart) {
			return
		}
		var cands []time.Time
		for j := 0; j < 7; j++ {
			d := weekStart.AddDate(0, 0, j)
			if weekdays[d.Weekday()] && r.monthAllowed(d.Month()) {
				cands = append(cands, d.Add(clock))
			}
		}
		if emit(cands) {
			return
		}
		weekStart = weekStart.AddDate(0, 0, 7*r.interval)
	}
}

func (r *rrule) monthly(startW, limit time.Time, emit func([]time.Time) bool) {
	clock := clockOf(startW)
	idx := startW.Year()*12 + int(startW.Month()) - 1
	for i := 0; i < maxPeriods; i++ {
		y, m := idx/12, time.Month(idx%12+1)
		periodStart := time.Date(y, m, 1, 0, 0, 0, 0, time.UTC)
		if periodStart.After(limit) || r.pastUntil(periodStart) {
			return
		}
		var cands []time.Time
		if r.monthAllowed(m) {
			for _, d := range r.monthDays(y, m, startW.Day()) {
				cands = append(cands, time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Add(clock))
			}
		}
		if emit(cands) {
			return
		}
		idx += r.interval
	}
}

// yearly expands BYMONTH (or the start's month) then applies the monthly
// day logic within each month. BYSETPOS still selects over the whole year's
// candidate list — the period is the year, per RFC.
func (r *rrule) yearly(startW, limit time.Time, emit func([]time.Time) bool) {
	clock := clockOf(startW)
	months := r.byMonth
	if len(months) == 0 {
		months = []time.Month{startW.Month()}
	}
	y := startW.Year()
	for i := 0; i < maxPeriods; i++ {
		periodStart := time.Date(y, time.January, 1, 0, 0, 0, 0, time.UTC)
		if periodStart.After(limit) || r.pastUntil(periodStart) {
			return
		}
		var cands []time.Time
		for _, m := range months {
			for _, d := range r.monthDays(y, m, startW.Day()) {
				cands = append(cands, time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Add(clock))
			}
		}
		if emit(cands) {
			return
		}
		y += r.interval
	}
}

// monthDays returns the sorted days of (y, m) the rule selects.
//
// DECISION (short months, kept): a MONTHLY/YEARLY rule with neither
// BYMONTHDAY nor BYDAY recurs on the start's day-of-month, and a month
// lacking that day (Jan 31 -> February) produces no instance rather than
// clamping to the month's last day.
func (r *rrule) monthDays(y int, m time.Month, startDay int) []int {
	n := daysIn(y, m)
	var set map[int]bool
	switch {
	case len(r.byMonthDay) > 0 && len(r.byDay) > 0:
		// Both present: BYDAY limits the BYMONTHDAY expansion (RFC 5545) —
		// "every Friday the 13th" is BYDAY=FR;BYMONTHDAY=13.
		set = monthDaySet(n, r.byMonthDay)
		byday := r.byDaySet(y, m, n)
		for d := range set {
			if !byday[d] {
				delete(set, d)
			}
		}
	case len(r.byMonthDay) > 0:
		set = monthDaySet(n, r.byMonthDay)
	case len(r.byDay) > 0:
		set = r.byDaySet(y, m, n)
	default:
		if startDay <= n {
			return []int{startDay}
		}
		return nil
	}
	days := make([]int, 0, len(set))
	for d := range set {
		days = append(days, d)
	}
	sort.Ints(days)
	return days
}

// byDaySet resolves the rule's BYDAY entries within one month: ord 0 selects
// every such weekday, ord n the nth, ord -n the nth from the month's end.
// Out-of-range ordinals (a fifth Monday in a four-Monday month) select
// nothing.
func (r *rrule) byDaySet(y int, m time.Month, n int) map[int]bool {
	set := map[int]bool{}
	for _, bd := range r.byDay {
		var days []int
		for d := 1; d <= n; d++ {
			if time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Weekday() == bd.wd {
				days = append(days, d)
			}
		}
		switch {
		case bd.ord == 0:
			for _, d := range days {
				set[d] = true
			}
		case bd.ord > 0 && bd.ord <= len(days):
			set[days[bd.ord-1]] = true
		case bd.ord < 0 && -bd.ord <= len(days):
			set[days[len(days)+bd.ord]] = true
		}
	}
	return set
}

func monthDaySet(n int, byMonthDay []int) map[int]bool {
	set := map[int]bool{}
	for _, md := range byMonthDay {
		d := md
		if md < 0 {
			d = n + 1 + md
		}
		if d >= 1 && d <= n {
			set[d] = true
		}
	}
	return set
}

// applySetPos selects the BYSETPOS-th candidates (1-based, negative from the
// end) from one period's sorted candidate list; out-of-range positions select
// nothing.
func applySetPos(cands []time.Time, pos []int) []time.Time {
	if len(pos) == 0 || len(cands) == 0 {
		return cands
	}
	keep := make([]bool, len(cands))
	for _, p := range pos {
		i := p - 1
		if p < 0 {
			i = len(cands) + p
		}
		if i >= 0 && i < len(cands) {
			keep[i] = true
		}
	}
	var out []time.Time
	for i, c := range cands {
		if keep[i] {
			out = append(out, c)
		}
	}
	return out
}

func (r *rrule) pastUntil(periodStart time.Time) bool {
	return !r.until.IsZero() && periodStart.After(r.until)
}

func (r *rrule) monthAllowed(m time.Month) bool {
	if len(r.byMonth) == 0 {
		return true
	}
	for _, x := range r.byMonth {
		if x == m {
			return true
		}
	}
	return false
}

// dayAllowed applies the DAILY limits: BYMONTH, BYMONTHDAY and BYDAY all
// filter the day rather than expanding anything.
func (r *rrule) dayAllowed(day time.Time) bool {
	if !r.monthAllowed(day.Month()) {
		return false
	}
	if len(r.byMonthDay) > 0 && !matchMonthDay(day.Day(), daysIn(day.Year(), day.Month()), r.byMonthDay) {
		return false
	}
	if len(r.byDay) > 0 {
		ok := false
		for _, bd := range r.byDay {
			if bd.wd == day.Weekday() {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

func matchMonthDay(d, n int, byMonthDay []int) bool {
	for _, md := range byMonthDay {
		if md == d || (md < 0 && n+1+md == d) {
			return true
		}
	}
	return false
}

func startOfWeek(day time.Time, wkst time.Weekday) time.Time {
	return day.AddDate(0, 0, -((int(day.Weekday()) - int(wkst) + 7) % 7))
}

func dayOf(w time.Time) time.Time {
	return time.Date(w.Year(), w.Month(), w.Day(), 0, 0, 0, 0, time.UTC)
}

func clockOf(w time.Time) time.Duration { return w.Sub(dayOf(w)) }

func daysIn(y int, m time.Month) int {
	return time.Date(y, m+1, 0, 0, 0, 0, 0, time.UTC).Day()
}
