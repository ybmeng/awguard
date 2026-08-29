# Calendar service — implementation scope

A third `std` background service, sibling to `artifacts` and `botnetsvc`: a
local calendar of events with recurrence. **Standard library only** (the repo's
one external dep is `modernc.org/sqlite`; you may use it, nothing else).

Read these first — this scope mirrors them and you should match their style:
- `go/std/bg_services/bg_services.go` — the `Service` contract you implement.
- `go/std/bg_services/artifacts/artifacts.go` — the service shape to copy
  (Config → New → Run serving an HTTP API over a unix socket → Verify).
- `go/botnet/id.go` — the ULID id scheme; copy `newID`/validator into this
  package (stdlib-only, no cross-package import of the botnet copy).
- `go/botnet/schema.go` — the "structs are the SPEC, with DECISION/OPEN
  markers" house style; author `Event` in that voice.
- `go/std/stdd/main.go` `services()` (~line 71) — where you wire the service in.

## v1 scope (decided): events + recurrence + timezones. No UI.

In: single + recurring events, RFC 5545 RRULE (a bounded subset, below),
timezone/DST-correct instance expansion, EXDATE exclusions, a unix-socket REST
API, a fast `Verify`, wired into `stdd`.

Out of v1 (note as future, don't build): a Mac UI surface; Drive/remote sync;
per-instance overrides (RECURRENCE-ID modified instances); RDATE; attendees /
invitations / free-busy; sub-daily FREQ (HOURLY/MINUTELY/SECONDLY).

## Package & wiring

- New package `go/std/bg_services/calendar/` (this dir).
- Implement `bgservices.Service`: `Name() -> "calendar"`, `Run(ctx)`,
  `Verify(ctx)`.
- `Config{ Root string; Interval time.Duration; Logger *log.Logger }`. The
  service owns `<Root>/calendar/calendar.db` (sqlite) and serves its API on
  `<Root>/calendar/calendar.sock` — single writer, exactly like artifacts, so
  callers route through the service instead of racing the DB file.
- Wire into `stdd/main.go` `services()`: construct `calendar.New(...)` and add
  it to the returned `[]bgservices.Service`. One line plus construction,
  matching how `art` is built.
- `import _ "time/tzdata"` **in the calendar package** so the IANA zone
  database is embedded in the binary — never depend on host `/usr/share/zoneinfo`.

## Data model — author this as the spec (schema.go voice)

`Event`:
- `ID` — `"evt_" + ULID` (copy the id.go scheme).
- `Title`, `Description`, `Location` — strings; last two optional.
- `AllDay bool` — all-day events are date-only, span local midnight→midnight in
  `TZ`, and ignore the clock components of Start/End.
- `Start`, `End` — **wall-clock** naive time (RFC3339 without offset, or a bare
  date when AllDay). Store the wall clock, NOT an absolute instant, so DST is
  applied at expansion time.
- `TZ` — IANA id (e.g. `"America/New_York"`). Required for timed events;
  validate it loads via `time.LoadLocation` on write, reject unknown zones.
- `RRULE` — RFC 5545 recurrence rule string; empty = single event.
- `EXDATE` — list of excluded instance start times (wall-clock in TZ).
- `CreatedAt`, `UpdatedAt`.

DECISION to record in the struct comments: wall-clock + IANA id (not absolute
UTC) is the storage form, because a recurring "9am weekly" must stay 9am local
across a DST boundary — storing UTC would drift it by an hour. Expansion
converts wall-clock → absolute `time.Time` in the zone.

## Recurrence — the hard core. Supported RRULE subset (v1):

- `FREQ` ∈ {DAILY, WEEKLY, MONTHLY, YEARLY}.
- `INTERVAL`, `COUNT`, `UNTIL` (UNTIL compared in the event's zone).
- `BYDAY` (incl. ordinals: `3MO`, `-1FR`), `BYMONTHDAY`, `BYMONTH`, `WKST`.
- `BYSETPOS` — include if it comes cheaply with BYDAY; else flag as the one
  stretch item. Needed for "last weekday of the month" style rules.
- Reject unsupported params on write with an instructive error (don't silently
  drop them).

Core algorithm: `Expand(event, from, to) -> []Instance` — given a window,
return the concrete instances (start/end absolute times) sorted ascending, with
EXDATE removed and COUNT/UNTIL honored. This is THE thing the tests and Verify
must pin. Expansion runs on wall-clock in `TZ`, then `.In(loc)` to get instants;
a single-event `Event` is the degenerate case (its one instance if it falls in
the window).

## API (REST over the unix socket, v1)

- `POST   /v1/events`                 → create (body = Event spec); returns id.
- `GET    /v1/events/{id}`            → the master event.
- `PATCH  /v1/events/{id}`            → edit (last-write-wins, like botnet memory).
- `DELETE /v1/events/{id}`           → delete.
- `GET    /v1/instances?from=..&to=..` → expanded instances across ALL events in
  the window, sorted by start. This is the "what's on my calendar" query.

Match artifacts' socket-HTTP idiom (`http://calendar/v1/...` over the unix
socket). Validate inputs at the boundary; reject bad TZ / bad RRULE / end<start.

## Verify (fast self-check, well under a second) — mirror artifacts.Verify

Throwaway temp DB, exercise end-to-end and assert exact values:
1. Create a WEEKLY event (e.g. FREQ=WEEKLY;BYDAY=MO with COUNT=4) in a zone that
   crosses a DST boundary within the window; expand and assert the 4 instance
   instants are correct — specifically that the one past the DST switch keeps the
   same wall-clock hour (proves tz correctness, the whole point).
2. Add an EXDATE for one instance; re-expand; assert it's gone and the rest
   remain.
3. Assert COUNT terminates at 4 and an UNTIL variant terminates at the boundary.
Return nil only if all hold; wrap failures with `calendar verify: ...`.

## Tests — the crown jewel is the expander

Table-driven `Expand` tests, including: DST spring-forward and fall-back;
monthly `BYDAY` ordinals (`3MO`, `-1FR`); `COUNT` vs `UNTIL`; `INTERVAL>1`;
EXDATE; all-day events; a single (no-RRULE) event. Seed a few fixtures from the
worked examples in RFC 5545 §3.8.5.3 — they're canonical and unambiguous. Plus
store round-trip tests and an API test over the socket (mirror
artifacts_test.go).

## Suggested phasing (each phase ends green)

1. `Event` struct + sqlite store (CRUD round-trip test) — no recurrence yet.
2. `Expand` + the full recurrence test suite. **The meat.**
3. Unix-socket REST API + `/v1/instances` + API test.
4. `Verify` + wire into `stdd/main.go`; `go build ./... && go test ./...` green,
   `stdd` supervises it, `calendar.sock` answers a curl.

## Follow-ups (explicitly not v1)

- Botnet model tools (`calendar_create_event`, `calendar_list_events`, …) so
  bots schedule/query — a second workstream touching `go/botnet/tools.go` plus a
  thin client to `calendar.sock`. Design it after the service is green.
- Mac UI calendar surface in `swift/botnet`.
- Drive-synced storage (reuse the artifacts Syncer idea) and per-instance
  overrides.

## Guardrails (same as all botnet/std work)

Stdlib + sqlite only. Never touch `~/.botnet/net.db`. Fast `Verify` is the
quality bar. `go build ./... && go test ./...` (with `-race`) is the gate.
Don't wire into `stdd` until phases 1–3 are green.
