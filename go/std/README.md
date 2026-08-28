# std tooling

Small, single-purpose background services, hosted by one macOS service you
control. Everything is standard-library Go, and every module is quickly
verifiable.

## Layout

```
go/std/
  bg_services/            Service contract + supervisor loop
    artifacts/            std_artifacts: inbox -> synced file sync
  stdd/                   the service binary launchd runs, plus its control CLI
```

Every background service implements the same tiny contract
(`bgservices.Service`):

- `Name()` — stable identifier
- `Run(ctx)` — do the job until canceled
- `Verify(ctx)` — fast self-check (well under a second) proving the service
  can do its job right now

`stdd` hosts all registered services in one process, restarting any that fail
with exponential backoff.

## The mac service

```bash
go build -o stdd ./go/std/stdd

# Install + start as a LaunchAgent (runs at login, kept alive, logs to
# ~/Library/Logs/stdd.log)
./stdd install -dir ~/artifacts

# Control it
./stdd status
./stdd restart
./stdd stop
./stdd start
./stdd uninstall

# Or just run it in the foreground
./stdd run -dir ~/artifacts -interval 500ms
```

No root required — it's a per-user LaunchAgent (`com.std.bgservices`), unlike
awguard's packet-capture LaunchDaemon.

## Quick verification

Two layers, both fast:

```bash
# 1. Source-level: vet + unit tests for every module under go/
scripts/verify-std.sh

# 2. Binary-level: each service's own self-check (runs a real inbox -> synced
#    cycle in a throwaway temp dir)
./stdd verify
```

## std_artifacts

A managed artifact store. The insert pipeline:

1. **Insert** takes on-disk file locations and moves them (consumes the
   sources) into a fresh subdirectory of the global `managed/` dir.
2. The subdirectory gets a **monotonically increasing id** — durable across
   restarts via a counter file, never reused even if local dirs are evicted.
3. The dir is **force-synced** to remote storage (Google Drive).
4. Only after the sync succeeds is the **managed dir id returned** — from then
   on the files are referenced by `id` in our system.
5. **Open(id, name)** serves the file from local storage, or falls back to
   fetching it from Drive when the local copy is gone.

```bash
# Insert files and get back the managed dir id
./stdd insert -dir ~/artifacts report.pdf data.csv
# -> 7        (files now live in ~/artifacts/managed/7/)
```

The background service also watches `<dir>/inbox`: every file dropped there
is auto-inserted (one managed dir per file).

The remote side is a pluggable `Syncer` interface (`ForceSync`, `Fetch`), so
the whole pipeline is testable in milliseconds with a fake; `NopSyncer` gives
local-only operation until the Google Drive syncer is wired in.

As a library:

```go
svc, err := artifacts.New(artifacts.Config{Root: "/path/to/dir", Syncer: drive})
id, err := svc.Insert(ctx, "/path/to/report.pdf")   // blocks until synced
r, err := svc.Open(ctx, id, "report.pdf")           // local, or Drive fallback
```

## Adding a new service

1. Create `go/std/bg_services/<name>/` implementing `bgservices.Service`,
   including a fast `Verify` and sub-second unit tests.
2. Register it in the `services()` roster in `go/std/stdd/main.go`.
3. `scripts/verify-std.sh && stdd verify`, then `stdd restart` to deploy —
   no new plist, no new install step.
