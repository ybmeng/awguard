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

Filesystem-only artifact sync. Watches `<dir>/inbox` in the background and
moves every file that appears there into `<dir>/synced`.

- `inbox/` and `synced/` are created under the root dir if missing.
- Files move via atomic `os.Rename` (same filesystem, no copies).
- Name collisions in `synced/` get a numeric suffix (`report.txt` → `report-1.txt`).
- Dotfiles and subdirectories in `inbox/` are left alone.

Embeddable as a library too:

```go
svc, err := artifacts.New(artifacts.Config{Root: "/path/to/dir"})
if err != nil { ... }
go svc.Run(ctx) // background sync until ctx is canceled
```

## Adding a new service

1. Create `go/std/bg_services/<name>/` implementing `bgservices.Service`,
   including a fast `Verify` and sub-second unit tests.
2. Register it in the `services()` roster in `go/std/stdd/main.go`.
3. `scripts/verify-std.sh && stdd verify`, then `stdd restart` to deploy —
   no new plist, no new install step.
