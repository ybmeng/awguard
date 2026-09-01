---
name: std-services
description: Working on go/std — stdd and its bg_services (artifacts, calendar, botnetsvc, automations). Read before adding a service, touching stdd wiring, or working on the automations runner.
---

# std services work

## Territory and hard rules

- Scope is `go/std/` (plus, for the automations service, the automation README frontmatters and `skills/automation_123/SKILL.md`). Never touch `swift/`, never `go/botnet/` internals from here.
- Never run a hand-launched stdd against default paths: a smoke `stdd run` MUST pass `-dir <tmpdir>`, `-botnet-addr` on a free port (the installed stdd owns 127.0.0.1:8730), and `-botnet-db <tmpdir>/net.db`. Talk to it over its unix sockets only. Kill it by pid when done.
- `stdd verify` is the only stdd subcommand agents run — never `install`/`start`/`restart`.
- Done means `go vet ./go/std/...` clean, `go test ./go/std/... -race -count=1` green, `scripts/verify-std.sh` green, and a scratch-built `./stdd verify` showing every service ok.

## The service idiom (copy it, don't reinvent)

- Shape: `Config -> New -> Run` serving HTTP over a unix socket in `<Root>/<dir>/`, plus a hermetic sub-second `Verify` that touches only throwaway temp dirs. `New` does validation and opens stores; `Run` does all serving. Single-writer via either flock (artifacts) or socket-detect (calendar, automations: `Dial` the root's socket and refuse if it answers).
- A new service is wired in three stdd places: `services()` roster, per-service flags (env default resolved in the flag default, so flag > env > built-in), and the plist template + `installService` signature — launchd inherits no shell env, so every env-driven knob must be baked into the plist as a flag.
- Prefixed-ULID ids: copy id.go locally (calendar's and automations' are copies of the botnet scheme); the newID helpers are deliberately unexported, so no cross-service id imports.
- Unix socket paths cap around 104 bytes: tests that bind sockets use `os.MkdirTemp("/tmp", ...)`, never `t.TempDir()`.

## Automations runner specifics

- The runner derives EVERYTHING from the runs table — baseline, satisfied, retry pacing, freshness. No scheduler state lives in memory, which is what makes restarts idempotent; keep it that way. Rows are inserted at enqueue (status queued), stamped at exec start, finished with envelope/exit recorded independently; `SweepInterrupted` at Run start converts stranded queued/running rows to error.
- Schedules reuse `calendar.Expand` via a synthetic degenerate event (fixed anchor date 2020-01-01 at the manifest's `at`, 1-minute duration, the manifest tz/rrule). Validate an rrule by expanding a tiny window — `parseRRULE` is unexported and Expand surfaces its instructive errors.
- `trigger` is a reserved word in SQLite — quote it as `"trigger"` in every statement or the schema fails to parse.
- Subprocess runs need `Setpgid` + `cmd.Cancel = kill(-pid)` or a timed-out `sh -c` leaves its python children alive; `cmd.WaitDelay` keeps Wait from hanging on inherited pipes.
- The scheduler's first tick fires immediately: a daemon started on the real repo will auto-run any automation whose window is open and unsatisfied right at startup (observed live: korea-trass on release day). Expect repo-tree writes from that; they are the service's normal behavior.
- Manifest parsing is a minimal hand-rolled frontmatter reader (flat scalars + one nesting level). Both live automation READMEs are test fixtures parsed verbatim — a parser change that breaks either fails the suite, which is the point.
