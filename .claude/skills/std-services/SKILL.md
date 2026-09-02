---
name: std-services
description: Working on go/std — stdd and its bg_services (artifacts, botnetsvc, automations, execcal, ping). Read before adding a service, touching stdd wiring, or working on the automations service or firing pipeline.
---

# std services work

## Territory and hard rules

- Scope is `go/std/` (plus, for the automations service, the automation README frontmatters and `skills/automation_123/SKILL.md`). Never touch `swift/`, never `go/botnet/` internals from here.
- Never run a hand-launched stdd against default paths: a smoke `stdd run` MUST pass `-dir <tmpdir>`, `-botnet-addr` on a free port (the installed stdd owns 127.0.0.1:8730), and `-botnet-db <tmpdir>/net.db`. Talk to it over its unix sockets only. Kill it by pid when done.
- `stdd verify` is the only stdd subcommand agents run — never `install`/`start`/`restart`.
- Done means `go vet ./go/std/...` clean, `go test ./go/std/... -race -count=1` green, `scripts/verify-std.sh` green, and a scratch-built `./stdd verify` showing every service ok.

## The service idiom (copy it, don't reinvent)

- Shape: `Config -> New -> Run` serving HTTP over a unix socket in `<Root>/<dir>/`, plus a hermetic sub-second `Verify` that touches only throwaway temp dirs. `New` does validation and opens stores; `Run` does all serving. Single-writer via either flock (artifacts) or socket-detect (automations, execcal: `Dial` the root's socket and refuse if it answers). ping is the exception — it serves nothing and owns only `<Root>/ping/targets.json`.
- A new service is wired in three stdd places: `services()` roster, per-service flags (env default resolved in the flag default, so flag > env > built-in), and the plist template + `installService` signature — launchd inherits no shell env, so every env-driven knob must be baked into the plist as a flag. execcal and automations reuse the existing `-botnet-addr`; no new flags were needed for the firing pipeline.
- Prefixed-ULID ids: copy id.go locally (automations' is a copy of the botnet scheme); the newID helpers are deliberately unexported, so no cross-service id imports.
- Unix socket paths cap around 104 bytes: tests that bind sockets use `os.MkdirTemp("/tmp", ...)`, never `t.TempDir()` — and that includes `Verify` bodies that bind sockets.
- Loopback `httptest`/`net.Listen("tcp", "127.0.0.1:0")` fakes are fine inside `Verify` (botnetsvc set the precedent); keep the whole thing well under a second.
- Guarded sqlite column adds: `SELECT COUNT(*) FROM pragma_table_info('<table>') WHERE name = ?` then `ALTER TABLE ... ADD COLUMN` — the automations runs table did this for window_start/window_end.

## The firing pipeline (calendar-driven; the botnet calendar owns the schedule)

- Roster: artifacts, botnetsvc, automations, execcal, ping. The old std calendar service is DELETED — its RRULE expander lives in go/botnet now. Nothing under go/std may import bg_services/calendar; its `<root>/calendar/` data dir is orphaned on deployed roots.
- `ping` owns ALL clocks: one goroutine per target, POST then sleep interval, first ping immediate, no backoff (the pinged endpoints are idempotent by contract, the next interval is the retry). Target URLs: `http(s)://...` or `unix://<socket>.sock/<path>` — the `.sock` suffix is the split point. Extra targets merge from `<Root>/ping/targets.json` (interval as a Go duration string); malformed file/entries are logged and ignored.
- `execcal` is stateless: POST /tick → GET botnet `/v1/fireable` → POST automations `/v1/automations/{name}/fire` per row, windows passed through verbatim; response `{fired, skipped}` with per-row reasons; one failing fire never aborts the rest; botnet down = 502 on the tick, logged, next tick retries.
- `automations` is the idempotent arbiter. /fire verdicts derive purely from the runs table: satisfied = an ok run started ≥ windowStart that advanced past the pre-window baseline; paced = latest in-window attempt younger than the template's retry_every (no template → no pacing, satisfaction alone guards); else enqueue (trigger "schedule", window bounds recorded on the run row). 404 unknown, 409 in-flight, 400 bad windows.
- Freshness derives from the latest RECORDED fire window (window-from-fires), not from expanding any rule; `nextDue` is gone from the API — the calendar owns the future. A scheduled automation that has never been fired reads "never".
- `POST /tick` on automations (pinged every 5m, plus once at startup) = manifest rescan + registration-ensure: create the executable "Automations" calendar and, per scheduled automation with no event naming it ANYWHERE, one recurring event seeded from the manifest `schedule:` template. Ensure-if-absent only — never PATCH or move an existing event; user/bot calendar edits are authoritative.
- The manifest `schedule:` block is a provisioning template (rrule/at/tz/retry_for seed the calendar event) plus `retry_every` as fire-time pacing. parseSchedule validates fields locally but NOT the rrule's content — the botnet validates that at event creation and its errors surface in the ensure log.

## Automations runner specifics

- Everything is derived from the runs table — baseline, satisfied, pacing, freshness. No scheduler state lives in memory (there is no scheduler at all anymore), which is what makes restarts and duplicate fires idempotent; keep it that way. Rows are inserted at enqueue (status queued), stamped at exec start, finished with envelope/exit recorded independently; `SweepInterrupted` at Run start converts stranded queued/running rows to error.
- `trigger` is a reserved word in SQLite — quote it as `"trigger"` in every statement or the schema fails to parse.
- Subprocess runs need `Setpgid` + `cmd.Cancel = kill(-pid)` or a timed-out `sh -c` leaves its python children alive; `cmd.WaitDelay` keeps Wait from hanging on inherited pipes.
- Manifest parsing is a minimal hand-rolled frontmatter reader (flat scalars + one nesting level). Both live automation READMEs are test fixtures parsed verbatim — a parser change that breaks either fails the suite, which is the point.
- Fire-verdict tests need no clock injection: insert runs with started_at crafted relative to real `time.Now()` and window bounds around it.
- The automations API is double-surfaced: `Handler()` exports the SAME mux the socket serves, and stdd mounts it into botnet — `services()` constructs automations FIRST and passes `auto.Handler()` via `botnetsvc.Config.Automations`; only the five client-facing routes cross the bridge, /fire and /tick stay socket-only. Rows carry `path` (absolute RepoDir+Dir, for Open-in-Cursor) alongside the repo-relative `dir`.
- `runs,omitempty` on the shared automationView means a zero-run detail omits the `runs` key entirely (removing omitempty would put `"runs":null` on every list row, which is worse) — clients must treat an absent runs as empty, and a test asserting `Runs != nil` on a fresh automation is wrong, not the server.
