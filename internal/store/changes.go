package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/airspacesh/workstream/internal/api"
)

// The changes feed lists a workspace's changes in revision order. A
// change takes its revision with a write inside its transaction, and
// SQLite runs one write transaction at a time, so changes commit in
// revision order. A read that sees a revision therefore sees every
// earlier one, and a cursor never passes a change still to commit.

// nextRevision takes the next revision on the server for a change the
// transaction makes.
func nextRevision(ctx context.Context, tx *sql.Tx) (int64, error) {
	var revision int64
	if err := tx.QueryRowContext(ctx, `
		UPDATE revision_counter SET revision = revision + 1
		RETURNING revision`,
	).Scan(&revision); err != nil {
		return 0, fmt.Errorf("taking a revision: %w", err)
	}
	return revision, nil
}

// changeKey names a change by its revision and the entry or message
// that changed, where message 0 stands for the entry itself.
type changeKey struct {
	revision int64
	entry    int64
	message  int64
}

// changesSQL reads the changes of workspace ?1 after revision ?2, at
// most ?3 of them, oldest first.
const changesSQL = `
	SELECT revision, entry_id, number FROM (
		SELECT revision, id AS entry_id, 0 AS number
		FROM entries
		WHERE workspace_id = ?1 AND revision > ?2
		ORDER BY revision LIMIT ?3
	)
	UNION ALL
	SELECT revision, entry_id, number FROM (
		SELECT revision, entry_id, id AS number
		FROM entry_messages
		WHERE workspace_id = ?1 AND revision > ?2
		ORDER BY revision LIMIT ?3
	)
	ORDER BY revision LIMIT ?3`

func (v view) readChangeKeys(
	ctx context.Context, workspace int64, since int64, limit int,
) ([]changeKey, error) {
	rows, err := v.tx.QueryContext(ctx, changesSQL, workspace, since, limit)
	if err != nil {
		return nil, fmt.Errorf("reading changes: %w", err)
	}
	defer rows.Close()
	var keys []changeKey
	for rows.Next() {
		var key changeKey
		if err := rows.Scan(
			&key.revision, &key.entry, &key.message,
		); err != nil {
			return nil, fmt.Errorf("reading changes: %w", err)
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}

// Changes returns up to limit changes in a workspace after the
// revision since, oldest first.
func (s *Store) Changes(
	ctx context.Context,
	access Access,
	workspace int64,
	since int64,
	limit int,
) (api.ChangesPage, error) {
	v, err := s.begin(ctx, access, true)
	if err != nil {
		return api.ChangesPage{}, fmt.Errorf("reading changes: %w", err)
	}
	defer v.tx.Rollback()

	keys, err := v.readChangeKeys(ctx, workspace, since, limit+1)
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
		page.Cursor = keys[len(keys)-1].revision
	}
	if err := v.tx.Commit(); err != nil {
		return api.ChangesPage{}, fmt.Errorf("finishing changes read: %w",
			err)
	}
	return page, nil
}

// LatestRevision returns the revision of the latest change on the
// server, in any workspace.
func (s *Store) LatestRevision(ctx context.Context) (int64, error) {
	var revision int64
	if err := s.reads.QueryRowContext(ctx, `
		SELECT revision FROM revision_counter`,
	).Scan(&revision); err != nil {
		return 0, fmt.Errorf("reading the latest revision: %w", err)
	}
	return revision, nil
}
