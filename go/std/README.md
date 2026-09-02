# std tooling

Small, single-purpose background services, hosted by one macOS service you
control. Everything is standard-library Go, and every module is quickly
verifiable.

## Layout

```
go/std/
  bg_services/            Service contract + supervisor loop
    artifacts/            std_artifacts: the managed artifact store
    botnetsvc/            botnet: the PrivateBotNet HTTP server, hosted in-process
    automations/          automation-123 runner: discovery, serial runs, fire arbiter
    execcal/              stateless bridge: calendar fireable instances -> automation fires
    ping/                 the only clock: POSTs /tick to execcal (1m) and automations (5m)
  drive/                  minimal stdlib-only Google Drive v3 client + syncer
  stdd/                   the service binary launchd runs, plus its control CLI
```

The firing pipeline: the botnet calendar is the single source of truth for
what fires. Recurring events on *executable* calendars name an automation;
`ping` ticks `execcal` every minute; `execcal` asks the botnet
(`GET /v1/fireable`) which of those instances are active and forwards each to
the automations service (`POST /v1/automations/{name}/fire`), which answers
satisfied / paced / enqueued purely from its runs table — so repeated, late,
or double ticks are always safe. The automations service's own tick (every
5m) rescans manifests and ensures each scheduled automation has a calendar
event (ensure-if-absent; user and bot edits to the calendar stick).

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

# The botnet server's address and database can be pinned at install time
./stdd install -dir ~/artifacts -botnet-addr 127.0.0.1:8730 -botnet-db ~/.botnet/net.db

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
  ERR when the service next starts — except one interrupted between REFS
  and COMPLETE: its refs are already on disk, so the sweep finishes the
  last step and promotes it to COMPLETE. Only the running service sweeps;
  `stdd ls`/`cat`/`insert` operating directly (no service up) never touch
  another process's markers.

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
running service the same commands operate on the store directly. Ownership
of a root is an exclusive `flock` on `<dir>/.artifacts.lock`, held for the
service's whole life (and released by the kernel if it crashes): a second
service pointed at a busy root refuses to start before touching anything —
it never unlinks the live socket, sweeps in-flight inserts, or drains the
inbox.

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
# prints a consent URL — open it in your browser and approve
```

From then on `stdd run` and `stdd insert` force-sync through Drive
automatically.

## botnet

The PrivateBotNet server, hosted in-process by `stdd` instead of being started
by hand. It owns all bot state in one SQLite file and makes the OpenRouter
calls; the UI is a thin HTTP client. Installing the mac service means the API
is up at login, restarted with backoff if it ever dies.

| Setting | Flag (`run`, `install`) | Env | Default |
| --- | --- | --- | --- |
| Listen address | `-botnet-addr` | `BOTNET_ADDR` | `127.0.0.1:8730` |
| SQLite file | `-botnet-db` | `BOTNET_DB` | `~/.botnet/net.db` |
| OpenRouter key | — | `OPENROUTER_API_KEY` | `~/.config/botnet/openrouter.txt` |

A flag beats the env var, which beats the default. The plist carries the flags
because a LaunchAgent inherits none of your shell environment.

No key is not a failure: the server starts, the UI works, and only chat calls
fail. Set one at runtime with `POST /v1/config` (`{"openRouterKey": "..."}`) —
the server persists it to the key file at 0600, so it survives a restart.

Only one process can hold the port. Once installed, `stdd` owns
`127.0.0.1:8730` from login onward, and `./botnetd` — still the standalone dev
entry point, reading the same env vars and the same database — will fail to
bind while the daemon runs. `stdd status` reports two independent facts,
launchd's view of the agent and whether the port answers, so it stays useful
even when the agent is not installed and a hand-run `botnetd` is the one
holding it:

```bash
./stdd status
# ...launchctl output...
# stdd: botnet answering on http://127.0.0.1:8730 — that port is taken, so a
# second listener (./botnetd by hand) cannot bind it
```

The service loses that race cleanly: it claims the port before it opens the
database, so a `stdd` that cannot bind never touches `~/.botnet/net.db`, and
`Supervise` retries it with backoff. The reason lands in
`~/Library/Logs/stdd.log`, naming the address.

Use `stdd stop` to hand the port back to `botnetd`, or `-botnet-addr` to move
the daemon's copy aside.

`Verify` builds a complete server — store, key-less LLM, routed handler — over
a throwaway database and serves a real `GET /v1/bots` through it. It never
touches your `~/.botnet` and needs no key or network.

## Adding a new service

1. Create `go/std/bg_services/<name>/` implementing `bgservices.Service`,
   including a fast `Verify` and sub-second unit tests.
2. Register it in the `services()` roster in `go/std/stdd/main.go`.
3. `scripts/verify-std.sh && stdd verify`, then `stdd restart` to deploy —
   no new plist, no new install step.
