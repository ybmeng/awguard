package botnet

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// The change feed: how a second client learns what moved without refetching
// the world. The write side is the change_log table and its triggers in
// store.go — pointers only, never payloads, so five edits to one message
// coalesce to one id and replay from an old cursor yields current state, not a
// history of intermediates. This file is the read side: an opaque cursor in,
// buckets of ids out, and the client fetches the objects it wants.
//
// Consumers must be idempotent — the same id can appear again on a retried
// poll, and fetching an id that has since been destroyed simply finds nothing.

// ErrCannotCalculateChanges is the resync signal (JMAP's
// cannotCalculateChanges): the server cannot enumerate what changed since the
// given state — the token is not one it issued, or the log no longer reaches
// back that far. The client must drop its cache and refetch from scratch; this
// error is what will make pruning the log safe rather than a silent lie to
// stale clients.
var ErrCannotCalculateChanges = errors.New("botnet: cannot calculate changes from that state; resync")

// Changes is one page of the feed. Ids only, coalesced per entity — see
// ChangesSince for the exact rules.
type Changes struct {
	OldState       string     `json:"oldState"`
	NewState       string     `json:"newState"`
	HasMoreChanges bool       `json:"hasMoreChanges"`
	Changed        ChangedIDs `json:"changed"`
}

// ChangedIDs buckets one page's changes by entity type, so a client watching
// only the sidebar can ignore message churn without a separate cursor.
type ChangedIDs struct {
	Bots      ChangeBucket `json:"bots"`
	Messages  ChangeBucket `json:"messages"`
	Segments  ChangeBucket `json:"segments"`
	Events    ChangeBucket `json:"events"`
	Calendars ChangeBucket `json:"calendars"`
}

// ChangeBucket lists what happened to one entity type since the client's
// state. Destroyed is the tombstone list — the only way an observer ever
// learns a hard delete happened.
type ChangeBucket struct {
	Created   []string `json:"created"`
	Updated   []string `json:"updated"`
	Destroyed []string `json:"destroyed"`
}

// ── State tokens ──────────────────────────────────────────────────────────────
// Internally the cursor is the change_log's AUTOINCREMENT seq; at the boundary
// it is opaque. Opacity is the point: a client can never do arithmetic on it
// (compute state+1, infer gaps by subtraction), so the encoding is free to
// change without touching a client.

const statePrefix = "s"

func formatState(seq int64) string {
	return statePrefix + strconv.FormatInt(seq, 36)
}

func parseState(token string) (int64, error) {
	raw, ok := strings.CutPrefix(token, statePrefix)
	if !ok {
		return 0, fmt.Errorf("state %q is not a token this server issued", token)
	}
	seq, err := strconv.ParseInt(raw, 36, 64)
	if err != nil || seq < 0 {
		return 0, fmt.Errorf("state %q is not a token this server issued", token)
	}
	return seq, nil
}

// State returns the current sync token — the value X-BotNet-State carries, and
// the `since` a client hands back to ChangesSince. It moves on every mutation
// of any synced entity.
func (s *Store) State() (string, error) {
	seq, err := s.maxSeq(s.db)
	if err != nil {
		return "", err
	}
	return formatState(seq), nil
}

func (s *Store) maxSeq(q dbtx) (int64, error) {
	var seq int64
	if err := q.QueryRow(`SELECT COALESCE(MAX(seq), 0) FROM change_log`).Scan(&seq); err != nil {
		return 0, fmt.Errorf("current state: %w", err)
	}
	return seq, nil
}

// ChangesSince returns what moved after the given state token, at most limit
// raw log rows' worth. HasMoreChanges set means the page was cut short: call
// again from NewState.
//
// Coalescing, per entity id over the returned window, latest wins:
//   - created (then any updates)      → created
//   - created then destroyed         → omitted entirely; the client never saw
//     it exist, so telling it either half would only confuse
//   - updated only                   → updated
//   - anything ending in destroyed   → destroyed
//
// A short page can split a create from its later destroy across two calls;
// the client then sees a create followed by a destroy, which is exactly the
// truth, and idempotent consumption absorbs it.
//
// An unknown token, or one from before the log's earliest surviving row,
// returns ErrCannotCalculateChanges. Pruning (not implemented yet) must only
// ever remove a prefix of the log, or the gap check here cannot see the hole.
func (s *Store) ChangesSince(since string, limit int) (Changes, error) {
	seq, err := parseState(since)
	if err != nil {
		return Changes{}, fmt.Errorf("%w: %v", ErrCannotCalculateChanges, err)
	}
	out := Changes{OldState: since}
	err = s.tx(func(q dbtx) error {
		max, err := s.maxSeq(q)
		if err != nil {
			return err
		}
		if seq > max {
			return fmt.Errorf("%w: state %q is ahead of this server", ErrCannotCalculateChanges, since)
		}
		if seq < max {
			var min int64
			if err := q.QueryRow(`SELECT MIN(seq) FROM change_log`).Scan(&min); err != nil {
				return fmt.Errorf("oldest change: %w", err)
			}
			if seq+1 < min {
				return fmt.Errorf("%w: state %q predates the log", ErrCannotCalculateChanges, since)
			}
		}

		rows, err := q.Query(
			`SELECT seq, entity, entity_id, op FROM change_log WHERE seq > ? ORDER BY seq LIMIT ?`,
			seq, limit+1)
		if err != nil {
			return fmt.Errorf("read changes: %w", err)
		}
		defer rows.Close()

		type key struct{ entity, id string }
		type agg struct {
			created bool
			last    string
		}
		seen := map[key]*agg{}
		newSeq, n := seq, 0
		for rows.Next() {
			var rowSeq int64
			var k key
			var op string
			if err := rows.Scan(&rowSeq, &k.entity, &k.id, &op); err != nil {
				return fmt.Errorf("scan change: %w", err)
			}
			if n++; n > limit {
				out.HasMoreChanges = true
				break
			}
			newSeq = rowSeq
			a := seen[k]
			if a == nil {
				a = &agg{}
				seen[k] = a
			}
			if op == "created" {
				a.created = true
			}
			a.last = op
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("read changes: %w", err)
		}
		if !out.HasMoreChanges {
			newSeq = max
		}
		out.NewState = formatState(newSeq)

		out.Changed = emptyChangedIDs()
		for k, a := range seen {
			b := out.Changed.bucket(k.entity)
			if b == nil {
				continue // an entity this version does not know; skip, never fail
			}
			switch {
			case a.last == "destroyed" && a.created:
				// Born and gone inside the window: invisible.
			case a.last == "destroyed":
				b.Destroyed = append(b.Destroyed, k.id)
			case a.created:
				b.Created = append(b.Created, k.id)
			default:
				b.Updated = append(b.Updated, k.id)
			}
		}
		out.Changed.sortAll()
		return nil
	})
	if err != nil {
		return Changes{}, err
	}
	return out, nil
}

// emptyChangedIDs allocates every id slice so the JSON always carries [] and
// never null — a client should not need a nil case per bucket.
func emptyChangedIDs() ChangedIDs {
	empty := func() ChangeBucket {
		return ChangeBucket{Created: []string{}, Updated: []string{}, Destroyed: []string{}}
	}
	return ChangedIDs{Bots: empty(), Messages: empty(), Segments: empty(), Events: empty(), Calendars: empty()}
}

func (c *ChangedIDs) bucket(entity string) *ChangeBucket {
	switch entity {
	case "bot":
		return &c.Bots
	case "message":
		return &c.Messages
	case "segment":
		return &c.Segments
	case "event":
		return &c.Events
	case "calendar":
		return &c.Calendars
	}
	return nil
}

// sortAll orders every id list so a page is deterministic — map iteration must
// not leak into the API.
func (c *ChangedIDs) sortAll() {
	for _, b := range []*ChangeBucket{&c.Bots, &c.Messages, &c.Segments, &c.Events, &c.Calendars} {
		sort.Strings(b.Created)
		sort.Strings(b.Updated)
		sort.Strings(b.Destroyed)
	}
}

// maxBatchIDs caps one MessagesByIDs call; a change page can name more ids
// than this, and the client simply fetches in batches.
const maxBatchIDs = 500

// MessagesByIDs returns the messages with the given ids, in insertion order —
// the fetch half of the feed's ids-only contract. An id with no row is simply
// absent from the result: to a feed consumer, missing means destroyed since
// the page that named it, which idempotent consumption already handles.
func (s *Store) MessagesByIDs(ids []string) ([]Message, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	if len(ids) > maxBatchIDs {
		return nil, fmt.Errorf("%w: at most %d ids per fetch, got %d", ErrInvalid, maxBatchIDs, len(ids))
	}
	placeholders := strings.Repeat("?,", len(ids)-1) + "?"
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return s.messages(`WHERE id IN (`+placeholders+`) ORDER BY rowid`, args...)
}
