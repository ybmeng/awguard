package automations

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver (no cgo)
)

// ErrRunNotFound is returned when the referenced run does not exist.
var ErrRunNotFound = errors.New("automations: run not found")

// Service-side run statuses. A finished run otherwise carries its envelope's
// status verbatim (ok, degraded, failed, needs_human); envelope status and
// exit code are recorded independently — never infer one from the other
// (degraded exits 1 by spec).
const (
	StatusQueued  = "queued"  // enqueued, the serial runner has not reached it yet
	StatusRunning = "running" // subprocess in flight
	StatusError   = "error"   // no envelope: parse failure, timeout, or interrupted
)

// Run is one recorded invocation of an automation.
type Run struct {
	ID         string
	Automation string
	Trigger    string // "schedule" | "manual"
	Started    string // fixed-width RFC3339 UTC to the second ("" until known)
	Finished   string // same, "" while queued/running
	ExitCode   int    // -1 until the process exits (or when it never ran)
	Status     string
	FormUsed   int
	Envelope   string // the raw envelope JSON line, "" when none was parsed
	StderrTail string // last 8KB of stderr
	Error      string // service-side error text ("" on a clean envelope run)
}

// fmtTime is the storage layout for run timestamps: fixed-width RFC3339 UTC to
// the second, so TEXT ORDER BY and lexicographic window comparisons are
// chronologically correct (the recorded house lesson — RFC3339Nano drops
// trailing zeros and missorts). Ties within a second break on rowid.
func fmtTime(t time.Time) string {
	return t.UTC().Truncate(time.Second).Format(time.RFC3339)
}

// Store is the runs table backed by SQLite. Everything the runner decides is
// derived from it, which is what makes the service idempotent across restarts.
type Store struct {
	db *sql.DB
}

// OpenStore opens (creating if needed) the SQLite database at path and ensures
// the schema exists. Pass ":memory:" for an ephemeral store.
func OpenStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %q: %w", path, err)
	}
	// A single conn keeps :memory: coherent and avoids cross-conn lock churn.
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if _, err := db.Exec(`PRAGMA journal_mode = WAL; PRAGMA foreign_keys = ON;`); err != nil {
		s.Close()
		return nil, fmt.Errorf("set pragmas: %w", err)
	}
	if err := s.migrate(); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	const schema = `
CREATE TABLE IF NOT EXISTS runs (
    id          TEXT PRIMARY KEY,
    automation  TEXT NOT NULL,
    "trigger"   TEXT NOT NULL,
    started_at  TEXT NOT NULL,
    finished_at TEXT NOT NULL DEFAULT '',
    exit_code   INTEGER NOT NULL DEFAULT -1,
    status      TEXT NOT NULL,
    form_used   INTEGER NOT NULL DEFAULT 0,
    envelope    TEXT NOT NULL DEFAULT '',
    stderr_tail TEXT NOT NULL DEFAULT '',
    error       TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS runs_automation_started ON runs(automation, started_at);`
	if _, err := s.db.Exec(schema); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	return nil
}

const runColumns = `id, automation, "trigger", started_at, finished_at, exit_code, status, form_used, envelope, stderr_tail, error`

// Insert records a freshly enqueued run. Started holds the enqueue instant as
// a provisional value; MarkStarted overwrites it when execution begins.
func (s *Store) Insert(r Run) error {
	_, err := s.db.Exec(`INSERT INTO runs (`+runColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.Automation, r.Trigger, r.Started, r.Finished, r.ExitCode, r.Status,
		r.FormUsed, r.Envelope, r.StderrTail, r.Error)
	if err != nil {
		return fmt.Errorf("insert run: %w", err)
	}
	return nil
}

// MarkStarted stamps the actual execution start and moves the run to running.
func (s *Store) MarkStarted(id, started string) error {
	_, err := s.db.Exec(`UPDATE runs SET started_at = ?, status = ? WHERE id = ?`, started, StatusRunning, id)
	if err != nil {
		return fmt.Errorf("mark run started: %w", err)
	}
	return nil
}

// Finish records a run's outcome.
func (s *Store) Finish(id, finished string, exitCode int, status string, formUsed int, envelope, stderrTail, errText string) error {
	_, err := s.db.Exec(`UPDATE runs SET finished_at = ?, exit_code = ?, status = ?, form_used = ?,
		envelope = ?, stderr_tail = ?, error = ? WHERE id = ?`,
		finished, exitCode, status, formUsed, envelope, stderrTail, errText, id)
	if err != nil {
		return fmt.Errorf("finish run: %w", err)
	}
	return nil
}

// SweepInterrupted marks every queued or running run as errored. Only the
// single serving writer calls it, at startup: such rows describe a runner that
// no longer exists.
func (s *Store) SweepInterrupted(finished string) error {
	_, err := s.db.Exec(`UPDATE runs SET status = ?, error = 'interrupted by service restart', finished_at = ?
		WHERE status IN (?, ?)`, StatusError, finished, StatusQueued, StatusRunning)
	if err != nil {
		return fmt.Errorf("sweep interrupted runs: %w", err)
	}
	return nil
}

// Get returns one run by id.
func (s *Store) Get(id string) (Run, error) {
	row := s.db.QueryRow(`SELECT `+runColumns+` FROM runs WHERE id = ?`, id)
	return scanRun(row.Scan)
}

// Latest returns an automation's most recent run (by start, rowid tie-break).
func (s *Store) Latest(automation string) (Run, bool, error) {
	row := s.db.QueryRow(`SELECT `+runColumns+` FROM runs WHERE automation = ?
		ORDER BY started_at DESC, rowid DESC LIMIT 1`, automation)
	r, err := scanRun(row.Scan)
	if errors.Is(err, ErrRunNotFound) {
		return Run{}, false, nil
	}
	return r, err == nil, err
}

// LatestOKBefore returns the latest run with envelope status ok that started
// before the given instant — the baseline of the window beginning there.
func (s *Store) LatestOKBefore(automation, before string) (Run, bool, error) {
	row := s.db.QueryRow(`SELECT `+runColumns+` FROM runs WHERE automation = ? AND status = 'ok'
		AND started_at < ? ORDER BY started_at DESC, rowid DESC LIMIT 1`, automation, before)
	r, err := scanRun(row.Scan)
	if errors.Is(err, ErrRunNotFound) {
		return Run{}, false, nil
	}
	return r, err == nil, err
}

// StartedIn returns the runs started in [from, to), oldest first.
func (s *Store) StartedIn(automation, from, to string) ([]Run, error) {
	return s.query(`SELECT `+runColumns+` FROM runs WHERE automation = ? AND started_at >= ? AND started_at < ?
		ORDER BY started_at, rowid`, automation, from, to)
}

// List returns an automation's runs, newest first, capped at limit.
func (s *Store) List(automation string, limit int) ([]Run, error) {
	return s.query(`SELECT `+runColumns+` FROM runs WHERE automation = ?
		ORDER BY started_at DESC, rowid DESC LIMIT ?`, automation, limit)
}

func (s *Store) query(q string, args ...any) ([]Run, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}
	defer rows.Close()
	out := []Run{}
	for rows.Next() {
		r, err := scanRun(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func scanRun(scan func(...any) error) (Run, error) {
	var r Run
	err := scan(&r.ID, &r.Automation, &r.Trigger, &r.Started, &r.Finished, &r.ExitCode,
		&r.Status, &r.FormUsed, &r.Envelope, &r.StderrTail, &r.Error)
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, ErrRunNotFound
	}
	if err != nil {
		return Run{}, fmt.Errorf("scan run: %w", err)
	}
	return r, nil
}
