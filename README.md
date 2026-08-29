# std tooling

Small, single-purpose background services for macOS, hosted by one
user-controlled service process. Standard-library Go only — no dependencies —
and every module is quickly verifiable.

All the code lives under [`go/std/`](go/std/README.md):

- `bg_services/` — the service contract (`Name`/`Run`/`Verify`) and supervisor
- `bg_services/artifacts/` — `std_artifacts`, a managed artifact store with an
  explicit on-disk insert state machine and Google Drive sync
- `drive/` — minimal stdlib-only Google Drive v3 client + OAuth flow
- `stdd/` — the service binary launchd runs, plus its control CLI

## Quick start

```bash
go build -o stdd ./go/std/stdd
./stdd install -dir ~/artifacts   # per-user LaunchAgent, runs at login
./stdd verify                     # each service's fast self-check
```

Full docs, the artifact state machine, and Drive setup: [`go/std/README.md`](go/std/README.md).

## Verification

```bash
scripts/verify-std.sh   # vet + unit tests for everything under go/
./stdd verify           # binary-level self-checks
```

## License

MIT
