package botnet

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	modelselector "stdtools/go/lib/modelSelector"

	_ "modernc.org/sqlite" // pure-Go SQLite driver (no cgo)
)

// Store is the bot library backed by SQLite. Everything lives in one disk file;
// the whole domain (nets, bots, messages) is derived from the schema.go structs.
//
// The backend is deliberately behind this one type — if the SQLite dependency
// is ever reversed, only this file changes.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the SQLite database at path and ensures the
// schema exists. Pass ":memory:" for an ephemeral store (used by tests).
// WAL mode is enabled for concurrent readers; sidecar -wal/-shm files appear
// next to a file-backed path.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %q: %w", path, err)
	}
	// database/sql pools connections; a file DB with WAL tolerates that, but a
	// single conn keeps :memory: coherent and avoids cross-conn lock churn.
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if _, err := db.Exec(`PRAGMA journal_mode = WAL; PRAGMA foreign_keys = ON;`); err != nil {
		db.Close()
		return nil, fmt.Errorf("set pragmas: %w", err)
	}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }

// migrate creates the tables if absent. Idempotent — safe on every Open.
func (s *Store) migrate() error {
	const schema = `
CREATE TABLE IF NOT EXISTS nets (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS bots (
    id            TEXT PRIMARY KEY,
    net_id        TEXT NOT NULL REFERENCES nets(id),
    display_name  TEXT NOT NULL,
    created_at    TEXT NOT NULL,
    system_prompt TEXT NOT NULL,
    model         TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_bots_net ON bots(net_id);
CREATE TABLE IF NOT EXISTS messages (
    id       TEXT PRIMARY KEY,
    bot_id   TEXT NOT NULL REFERENCES bots(id),
    role     TEXT NOT NULL,
    content  TEXT NOT NULL,
    sent_at  TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_messages_bot ON messages(bot_id, id);
`
	if _, err := s.db.Exec(schema); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	return nil
}

// ErrNotFound is returned when a lookup by id has no row.
var ErrNotFound = errors.New("botnet: not found")

// CreateNet inserts a new PrivateBotNet and returns it.
func (s *Store) CreateNet(name string) (PrivateBotNet, error) {
	net := PrivateBotNet{ID: newID("net_"), Name: name}
	if _, err := s.db.Exec(`INSERT INTO nets (id, name) VALUES (?, ?)`, net.ID, net.Name); err != nil {
		return PrivateBotNet{}, fmt.Errorf("create net: %w", err)
	}
	return net, nil
}

// CreateBot adds a bot to a net and returns it. CreatedAt is set here.
func (s *Store) CreateBot(netID, displayName, systemPrompt string, model modelselector.ModelID) (Bot, error) {
	bot := Bot{
		ID:           BotID(newID("bot_")),
		DisplayName:  displayName,
		CreatedAt:    time.Now().UTC(),
		SystemPrompt: systemPrompt,
		Model:        model,
	}
	_, err := s.db.Exec(
		`INSERT INTO bots (id, net_id, display_name, created_at, system_prompt, model) VALUES (?, ?, ?, ?, ?, ?)`,
		bot.ID, netID, bot.DisplayName, bot.CreatedAt.Format(time.RFC3339Nano), bot.SystemPrompt, bot.Model,
	)
	if err != nil {
		return Bot{}, fmt.Errorf("create bot: %w", err)
	}
	return bot, nil
}

// AppendMessage records one message in a bot's conversation and returns it.
func (s *Store) AppendMessage(botID BotID, role, content string) (Message, error) {
	msg := Message{
		ID:      newID("msg_"),
		BotID:   botID,
		Role:    role,
		Content: content,
		SentAt:  time.Now().UTC(),
	}
	_, err := s.db.Exec(
		`INSERT INTO messages (id, bot_id, role, content, sent_at) VALUES (?, ?, ?, ?, ?)`,
		msg.ID, msg.BotID, msg.Role, msg.Content, msg.SentAt.Format(time.RFC3339Nano),
	)
	if err != nil {
		return Message{}, fmt.Errorf("append message: %w", err)
	}
	return msg, nil
}

// Conversation returns a bot's messages in send order (by id, which is
// time-sortable).
func (s *Store) Conversation(botID BotID) ([]Message, error) {
	rows, err := s.db.Query(
		`SELECT id, bot_id, role, content, sent_at FROM messages WHERE bot_id = ? ORDER BY id`, botID)
	if err != nil {
		return nil, fmt.Errorf("conversation: %w", err)
	}
	defer rows.Close()

	var out []Message
	for rows.Next() {
		var m Message
		var sentAt string
		if err := rows.Scan(&m.ID, &m.BotID, &m.Role, &m.Content, &sentAt); err != nil {
			return nil, fmt.Errorf("scan message: %w", err)
		}
		if m.SentAt, err = time.Parse(time.RFC3339Nano, sentAt); err != nil {
			return nil, fmt.Errorf("parse sent_at: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// GetBot loads one bot by id.
func (s *Store) GetBot(id BotID) (Bot, error) {
	var b Bot
	var createdAt string
	err := s.db.QueryRow(
		`SELECT id, display_name, created_at, system_prompt, model FROM bots WHERE id = ?`, id).
		Scan(&b.ID, &b.DisplayName, &createdAt, &b.SystemPrompt, &b.Model)
	if errors.Is(err, sql.ErrNoRows) {
		return Bot{}, ErrNotFound
	}
	if err != nil {
		return Bot{}, fmt.Errorf("get bot: %w", err)
	}
	if b.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt); err != nil {
		return Bot{}, fmt.Errorf("parse created_at: %w", err)
	}
	return b, nil
}

// GetNet loads a net and populates its Bots membership from the bots table.
func (s *Store) GetNet(id string) (PrivateBotNet, error) {
	var net PrivateBotNet
	err := s.db.QueryRow(`SELECT id, name FROM nets WHERE id = ?`, id).Scan(&net.ID, &net.Name)
	if errors.Is(err, sql.ErrNoRows) {
		return PrivateBotNet{}, ErrNotFound
	}
	if err != nil {
		return PrivateBotNet{}, fmt.Errorf("get net: %w", err)
	}
	rows, err := s.db.Query(`SELECT id FROM bots WHERE net_id = ? ORDER BY id`, id)
	if err != nil {
		return PrivateBotNet{}, fmt.Errorf("get net bots: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var bid BotID
		if err := rows.Scan(&bid); err != nil {
			return PrivateBotNet{}, fmt.Errorf("scan bot id: %w", err)
		}
		net.Bots = append(net.Bots, bid)
	}
	return net, rows.Err()
}
