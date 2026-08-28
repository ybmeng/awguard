# std tooling

A set of small, single-purpose services. Each tool has one job, uses only the
Go standard library, and ships as a package under `std/` plus a thin CLI under
`cmd/`.

## std_artifacts

Filesystem-only artifact sync. Watches `<dir>/inbox` in the background and
moves every file that appears there into `<dir>/synced`.

```bash
go build -o std_artifacts ./cmd/std_artifacts

# Run as a background service against a local dir you specify
./std_artifacts -dir ~/artifacts

# Custom poll interval
./std_artifacts -dir ~/artifacts -interval 500ms

# One-shot sync (no background loop)
./std_artifacts -dir ~/artifacts -once
```

Behavior:

- `inbox/` and `synced/` are created under the root dir if missing.
- Files are moved with an atomic rename (same filesystem, no copies).
- Name collisions in `synced/` get a numeric suffix (`report.txt` → `report-1.txt`).
- Dotfiles and subdirectories in `inbox/` are left alone.

Or embed it as a library:

```go
svc, err := artifacts.New(artifacts.Config{Root: "/path/to/dir"})
if err != nil { ... }
go svc.Run(ctx) // background sync until ctx is canceled
```

Tests:

```bash
go test ./std/...
```
