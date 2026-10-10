package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/airspacesh/workstream/internal/api"
)

var ErrInvalidCursor = errors.New("invalid cursor")

// The changes feed lists a workspace's changes in the order of a
// changeKey. The server stamps each change inside its write
// transaction, and writes take turns on one connection, so a change
// that commits after a read is stamped later than every change that
// read saw. A cursor therefore never passes a change still to commit,
// as long as the clock does not step back.

// changeKey orders changes by their stamp, then by entry, then by
// message, where message 0 stands for the entry itself.
type changeKey struct {
	at      string
	entry   int64
	message int64
}

// changeKeyOf finds where a cursor of a workspace points. The zero
// cursor points before every change.
func (s *Store) changeKeyOf(
	workspace int64, cursor api.ChangeCursor,
) (changeKey, error) {
	if cursor.IsZero() {
		return changeKey{}, nil
	}
	key, err := s.Resolve(cursor.Entry)
	if err != nil || key.Workspace != workspace {
		return changeKey{}, fmt.Errorf("%w: %s is from another workspace",
			ErrInvalidCursor, cursor)
	}
	return changeKey{
		at:      cursor.At.UTC().Format(timeLayout),
		entry:   cursor.Entry.ID,
		message: cursor.Message,
	}, nil
}

func (s *Store) changeCursor(
	workspace int64, key changeKey,
) (api.ChangeCursor, error) {
	at, err := parseTime(key.at)
	if err != nil {
		return api.ChangeCursor{}, err
	}
	return api.ChangeCursor{
		At:      at,
		Entry:   s.mustRef(Key{Workspace: workspace, ID: key.entry}),
		Message: key.message,
	}, nil
}

// changesSQL reads the changes of workspace ?1 after the change key
// (?2, ?3, ?4), at most ?5 of them, oldest first.
const changesSQL = `
	SELECT at, entry_id, number FROM (
		SELECT modified_utc AS at, id AS entry_id, 0 AS number
		FROM entries
		WHERE workspace_id = ?1 AND (modified_utc, id) > (?2, ?3)
		ORDER BY modified_utc, id LIMIT ?5
	)
	UNION ALL
	SELECT at, entry_id, number FROM (
		SELECT modified_utc AS at, entry_id, id AS number
		FROM entry_messages
		WHERE workspace_id = ?1
			AND (modified_utc, entry_id, id) > (?2, ?3, ?4)
		ORDER BY modified_utc, entry_id, id LIMIT ?5
	)
	ORDER BY at, entry_id, number LIMIT ?5`

// latestChangeSQL reads the key of the latest change in workspace ?1.
// The messages side finds the latest stamp, then breaks a tie between
// the messages stamped then. Ordering by all three columns at once
// makes SQLite sort every message of the workspace, and left to
// itself, SQLite walks the messages in entry order to find the stamp.
const latestChangeSQL = `
	SELECT at, entry_id, number FROM (
		SELECT modified_utc AS at, id AS entry_id, 0 AS number
		FROM entries WHERE workspace_id = ?1
		ORDER BY modified_utc DESC, id DESC LIMIT 1
	)
	UNION ALL
	SELECT at, entry_id, number FROM (
		SELECT modified_utc AS at, entry_id, id AS number
		FROM entry_messages INDEXED BY entry_messages_modified
		WHERE workspace_id = ?1 AND modified_utc = (
			SELECT max(modified_utc) FROM entry_messages
			WHERE workspace_id = ?1)
		ORDER BY entry_id DESC, id DESC LIMIT 1
	)
	ORDER BY at DESC, entry_id DESC, number DESC LIMIT 1`

func (v view) readChangeKeys(
	ctx context.Context, query string, args ...any,
) ([]changeKey, error) {
	rows, err := v.tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("reading changes: %w", err)
	}
	defer rows.Close()
	var keys []changeKey
	for rows.Next() {
		var key changeKey
		if err := rows.Scan(&key.at, &key.entry, &key.message); err != nil {
			return nil, fmt.Errorf("reading changes: %w", err)
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}

// Changes returns up to limit changes in a workspace after the cursor
// since, oldest first.
func (s *Store) Changes(
	ctx context.Context,
	access Access,
	workspace int64,
	since api.ChangeCursor,
	limit int,
) (api.ChangesPage, error) {
	from, err := s.changeKeyOf(workspace, since)
	if err != nil {
		return api.ChangesPage{}, err
	}
	v, err := s.begin(ctx, access, true)
	if err != nil {
		return api.ChangesPage{}, fmt.Errorf("reading changes: %w", err)
	}
	defer v.tx.Rollback()

	keys, err := v.readChangeKeys(ctx, changesSQL, workspace, from.at,
		from.entry, from.message, limit+1)
	if err != nil {
		return api.ChangesPage{}, err
	}
	page := api.ChangesPage{
		Changes: []api.Change{},
		Cursor:  since,
		More:    len(keys) > limit,
	}
	keys = keys[:min(len(keys), limit)]
	listed := map[Key]api.ListedEntry{}
	for _, key := range keys {
		entryKey := Key{Workspace: workspace, ID: key.entry}
		entry, ok := listed[entryKey]
		if !ok {
			found, err := v.getEntry(ctx, entryKey)
			if err != nil {
				return api.ChangesPage{}, err
			}
			entries, err := v.listEntries(ctx, []api.Entry{found})
			if err != nil {
				return api.ChangesPage{}, err
			}
			entry = entries[0]
			listed[entryKey] = entry
		}
		change := api.Change{Entry: entry}
		if key.message != 0 {
			message, err := s.getMessage(ctx, v.tx, entryKey, key.message)
			if err != nil {
				return api.ChangesPage{}, err
			}
			change.Message = &message
		}
		page.Changes = append(page.Changes, change)
	}
	if len(keys) > 0 {
		page.Cursor, err = s.changeCursor(workspace, keys[len(keys)-1])
		if err != nil {
			return api.ChangesPage{}, err
		}
	}
	if err := v.tx.Commit(); err != nil {
		return api.ChangesPage{}, fmt.Errorf("finishing changes read: %w",
			err)
	}
	return page, nil
}

// LatestChange returns the cursor of the latest change in a
// workspace, or the zero cursor when it has none.
func (s *Store) LatestChange(
	ctx context.Context, workspace int64,
) (api.ChangeCursor, error) {
	v, err := s.begin(ctx, NewAccess(workspace), true)
	if err != nil {
		return api.ChangeCursor{}, fmt.Errorf("reading changes: %w", err)
	}
	defer v.tx.Rollback()
	keys, err := v.readChangeKeys(ctx, latestChangeSQL, workspace)
	if err != nil || len(keys) == 0 {
		return api.ChangeCursor{}, err
	}
	return s.changeCursor(workspace, keys[0])
}
