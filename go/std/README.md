# std tooling

Small, single-purpose background services, hosted by one macOS service you
control. Everything is standard-library Go, and every module is quickly
verifiable.

## Layout

```
go/std/
  bg_services/            Service contract + supervisor loop
    artifacts/            std_artifacts: the managed artifact store
  drive/                  minimal stdlib-only Google Drive v3 client + syncer
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

No root required — it's a per-user LaunchAgent (`com.std.bgservices`).

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

A managed artifact store. Insert is an explicit state machine, persisted on
disk in the managed dir itself:

```
INIT ──1──> MOVED ──2──> REMOTE_DIR ──3──> SYNCED ──4──> REFS ──5──> COMPLETE
  └──────────┴────────────┴───────────────┴───────────┘
                     any failure ──> ERR (terminal, irrecoverable for now)
```

- **INIT** — `managed/<id>/` created with a fresh monotonic id and tagged
  WIP (`.wip` marker) before anything else happens.
- **Stage 1 → MOVED** — the referenced on-disk files are moved in
  (sources consumed).
- **Stage 2 → REMOTE_DIR** — the Drive folder structure
  (`std_artifacts/<id>/`) is created.
- **Stage 3 → SYNCED** — every file uploaded and acknowledged.
- **Stage 4 → REFS** — static references written to `.refs.json` in the dir:
  Drive file id, size, sha256 per file. Never rewritten afterwards.
- **Stage 5 → COMPLETE** — the WIP tag is removed; only now does Insert
  return the managed dir id.
- **ERR** — any failure renames `.wip` to `.err` recording the stage that
  died and why. Terminal: nothing retries, the dir stays for inspection,
  its id is burned. An insert left mid-WIP by a dead process is swept to
  ERR on the next startup — except one interrupted between REFS and
  COMPLETE: its refs are already on disk, so the sweep finishes the last
  step and promotes it to COMPLETE.

Serving: `Open(id, name)` refuses non-COMPLETE dirs, serves from local
storage when present, and otherwise fetches from Drive by the static
`remote_id` in the refs — no name lookups.

```bash
# Insert files and get back the managed dir id (only on COMPLETE)
./stdd insert -dir ~/artifacts report.pdf data.csv
# -> 7        (files + .refs.json now live in ~/artifacts/managed/7/)

# Inspect the state machine
./stdd ls -dir ~/artifacts
# 7        complete      2026-08-28T10:41:00Z
# 8        err           2026-08-28T10:44:12Z  stage remote_dir: drive: ...

# Stream a managed file (local storage, or Drive fallback)
./stdd cat -dir ~/artifacts 7 report.pdf > report.pdf
```

The background service also watches `<dir>/inbox`: every file dropped there
is auto-inserted (one managed dir per file).

### One writer: the mac service owns the store

While the installed service is running it serves a local API on a unix
socket inside the root dir (`<dir>/.artifacts.sock`). `stdd insert`, `ls`
and `cat` detect it and route through it, so all id allocation and machine
runs happen in the one service process — no cross-process races. Without a
running service the same commands operate on the store directly. A second
service pointed at a busy root refuses to start.

The remote stages are a pluggable `Syncer` interface — `CreateDir` (stage 2),
`SyncFile` (stage 3), `Fetch` (serving fallback) — so the whole machine stays
testable in milliseconds with fakes. The real implementation is Google Drive
(below). Without Drive configured, `stdd` runs local-only via `NopSyncer`
and says so at startup.

As a library:

```go
svc, _ := artifacts.New(artifacts.Config{Root: dir, Syncer: driveSyncer})
id, err := svc.Insert(ctx, "/path/report.pdf")  // walks the machine, id on COMPLETE
st, _  := svc.Store().Status(id)                // Stage, Error, UpdatedAt
refs, _ := svc.Store().Refs(id)                 // static .refs.json contents
r, err := svc.Open(ctx, id, "report.pdf")       // local, or Drive by remote_id
```

### Google Drive setup (one time)

`go/std/drive` talks to the Drive v3 API directly — no SDK dependencies, no
desktop app. Managed dirs mirror to a `std_artifacts/<id>/` folder tree in
your Drive, uploads are acknowledged before Insert returns, and re-syncs
replace content instead of duplicating it. Transient Drive failures (429,
5xx, network errors) are retried a few times with short backoff — safe
because every remote stage is idempotent — before an insert is failed.

1. In the Google Cloud console, create an OAuth client of type **Desktop
   app** (with the Drive API enabled) and download its JSON. The token scope
   is `drive.file` — the app can only touch files it created.
2. Authorize once; the refresh token lands in `~/.config/stdd/drive.json`
   (mode 0600):

```bash
./stdd drive auth -credentials ~/Downloads/client_secret.json
# opens a consent URL; approve in the browser
```

From then on `stdd run` and `stdd insert` force-sync through Drive
automatically.

## Adding a new service

1. Create `go/std/bg_services/<name>/` implementing `bgservices.Service`,
   including a fast `Verify` and sub-second unit tests.
2. Register it in the `services()` roster in `go/std/stdd/main.go`.
3. `scripts/verify-std.sh && stdd verify`, then `stdd restart` to deploy —
   no new plist, no new install step.
