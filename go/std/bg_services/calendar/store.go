package calendar

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver (no cgo)
)

// ErrNotFound is returned when the referenced event does not exist.
var ErrNotFound = errors.New("calendar: event not found")

// Store is the event library backed by SQLite. Everything lives in one disk
// file; the schema is derived from schema.go's Event.
//
// There is no flock sidecar: single-writer is enforced the artifacts way — the
// service refuses to serve a root whose socket already answers (see serve).
type Store struct {
	db *sql.DB
}

// OpenStore opens (creating if needed) the SQLite database at path and
// ensures the schema exists. Pass ":memory:" for an ephemeral store (used by
// tests and Verify). WAL mode is enabled for concurrent readers; sidecar
// -wal/-shm files appear next to a file-backed path.
func OpenStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %q: %w", path, err)
	}
	// database/sql pools connections; a file DB with WAL tolerates that, but a
	// single conn keeps :memory: coherent and avoids cross-conn lock churn.
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
CREATE TABLE IF NOT EXISTS events (
    id          TEXT PRIMARY KEY,
    title       TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    location    TEXT NOT NULL DEFAULT '',
    all_day     INTEGER NOT NULL,
    start_wall  TEXT NOT NULL,
    end_wall    TEXT NOT NULL,
    tz          TEXT NOT NULL,
    rrule       TEXT NOT NULL DEFAULT '',
    exdate      TEXT NOT NULL DEFAULT '[]',
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL
);`
	if _, err := s.db.Exec(schema); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	return nil
}

// Create assigns the event a fresh id and timestamps and inserts it. The
// caller validates first (validateEvent); the store is dumb CRUD.
func (s *Store) Create(ev Event) (Event, error) {
	ev.ID = EventID(newID("evt_"))
	now := time.Now().UTC()
	ev.CreatedAt, ev.UpdatedAt = now, now
	if ev.EXDATE == nil {
		ev.EXDATE = []string{}
	}
	exdate, err := json.Marshal(ev.EXDATE)
	if err != nil {
		return Event{}, fmt.Errorf("marshal exdate: %w", err)
	}
	_, err = s.db.Exec(`INSERT INTO events
		(id, title, description, location, all_day, start_wall, end_wall, tz, rrule, exdate, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ev.ID, ev.Title, ev.Description, ev.Location, ev.AllDay, ev.Start, ev.End, ev.TZ, ev.RRULE,
		string(exdate), ev.CreatedAt.Format(time.RFC3339Nano), ev.UpdatedAt.Format(time.RFC3339Nano))
	if err != nil {
		return Event{}, fmt.Errorf("insert event: %w", err)
	}
	return ev, nil
}

// Get returns one event by id.
func (s *Store) Get(id EventID) (Event, error) {
	row := s.db.QueryRow(`SELECT id, title, description, location, all_day, start_wall, end_wall,
		tz, rrule, exdate, created_at, updated_at FROM events WHERE id = ?`, id)
	return scanEvent(row.Scan)
}

// List returns every event, in creation order (ULID ids sort by time).
func (s *Store) List() ([]Event, error) {
	rows, err := s.db.Query(`SELECT id, title, description, location, all_day, start_wall, end_wall,
		tz, rrule, exdate, created_at, updated_at FROM events ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list events: %w", err)
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		ev, err := scanEvent(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

// Update writes every authored field of ev over the stored row (last-write-
// wins; the caller validated the merged event). CreatedAt is immutable;
// UpdatedAt is bumped here.
func (s *Store) Update(ev Event) (Event, error) {
	ev.UpdatedAt = time.Now().UTC()
	if ev.EXDATE == nil {
		ev.EXDATE = []string{}
	}
	exdate, err := json.Marshal(ev.EXDATE)
	if err != nil {
		return Event{}, fmt.Errorf("marshal exdate: %w", err)
	}
	res, err := s.db.Exec(`UPDATE events SET title = ?, description = ?, location = ?, all_day = ?,
		start_wall = ?, end_wall = ?, tz = ?, rrule = ?, exdate = ?, updated_at = ? WHERE id = ?`,
		ev.Title, ev.Description, ev.Location, ev.AllDay, ev.Start, ev.End, ev.TZ, ev.RRULE,
		string(exdate), ev.UpdatedAt.Format(time.RFC3339Nano), ev.ID)
	if err != nil {
		return Event{}, fmt.Errorf("update event: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Event{}, ErrNotFound
	}
	return ev, nil
}

// Delete removes one event by id.
func (s *Store) Delete(id EventID) error {
	res, err := s.db.Exec(`DELETE FROM events WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete event: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func scanEvent(scan func(...any) error) (Event, error) {
	var ev Event
	var exdate, createdAt, updatedAt string
	err := scan(&ev.ID, &ev.Title, &ev.Description, &ev.Location, &ev.AllDay, &ev.Start, &ev.End,
		&ev.TZ, &ev.RRULE, &exdate, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Event{}, ErrNotFound
	}
	if err != nil {
		return Event{}, fmt.Errorf("scan event: %w", err)
	}
	if err := json.Unmarshal([]byte(exdate), &ev.EXDATE); err != nil {
		return Event{}, fmt.Errorf("unmarshal exdate for %s: %w", ev.ID, err)
	}
	if ev.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt); err != nil {
		return Event{}, fmt.Errorf("parse created_at for %s: %w", ev.ID, err)
	}
	if ev.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt); err != nil {
		return Event{}, fmt.Errorf("parse updated_at for %s: %w", ev.ID, err)
	}
	return ev, nil
}
