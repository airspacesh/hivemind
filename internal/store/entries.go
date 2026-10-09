package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/airspacesh/workstream/internal/api"
)

// view is one transaction seen through one request's access.
type view struct {
	store  *Store
	tx     *sql.Tx
	access Access
}

func (s *Store) begin(
	ctx context.Context, access Access, readOnly bool,
) (view, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: readOnly})
	if err != nil {
		return view{}, err
	}
	return view{store: s, tx: tx, access: access}, nil
}

const entryColumns = `e.workspace_id, e.id, e.created_utc,
	e.modified_utc, e.type, e.subject, e.body, e.origin_user,
	e.origin_host, e.origin_dir, e.origin_claude_session`

type rowScanner interface {
	Scan(...any) error
}

// scanEntry reads the entryColumns, and any extra columns into extra.
func (v view) scanEntry(row rowScanner, extra ...any) (api.Entry, error) {
	var entry api.Entry
	var key Key
	var created, modified string
	err := row.Scan(append([]any{
		&key.Workspace, &key.ID, &created, &modified,
		&entry.Type, &entry.Subject, &entry.Body,
		&entry.Origin.User, &entry.Origin.Host,
		&entry.Origin.Dir, &entry.Origin.ClaudeSession,
	}, extra...)...)
	if err != nil {
		return api.Entry{}, err
	}
	entry.Ref = v.store.mustRef(key)
	if entry.Created, err = parseTime(created); err != nil {
		return api.Entry{}, err
	}
	if entry.Modified, err = parseTime(modified); err != nil {
		return api.Entry{}, err
	}
	return entry, nil
}

func (v view) checkEntryExists(ctx context.Context, key Key) error {
	var exists bool
	if err := v.tx.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM entries WHERE workspace_id = ? AND id = ?
		)`, key.Workspace, key.ID,
	).Scan(&exists); err != nil {
		return fmt.Errorf("checking entry: %w", err)
	}
	if !exists {
		return fmt.Errorf("%w: %s", ErrEntryNotFound, v.store.mustRef(key))
	}
	return nil
}

func (v view) metadataFor(
	ctx context.Context, key Key,
) (map[string]string, error) {
	rows, err := v.tx.QueryContext(ctx, `
		SELECT key, value FROM entry_metadata
		WHERE workspace_id = ? AND entry_id = ? ORDER BY key`,
		key.Workspace, key.ID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	metadata := map[string]string{}
	for rows.Next() {
		var name, value string
		if err := rows.Scan(&name, &value); err != nil {
			return nil, err
		}
		metadata[name] = value
	}
	return metadata, rows.Err()
}

func (v view) getEntry(ctx context.Context, key Key) (api.Entry, error) {
	row := v.tx.QueryRowContext(ctx, `
		SELECT `+entryColumns+` FROM entries AS e
		WHERE e.workspace_id = ? AND e.id = ?`, key.Workspace, key.ID,
	)
	entry, err := v.scanEntry(row)
	if errors.Is(err, sql.ErrNoRows) {
		return api.Entry{}, fmt.Errorf("%w: %s", ErrEntryNotFound,
			v.store.mustRef(key))
	}
	if err != nil {
		return api.Entry{}, err
	}
	if entry.Metadata, err = v.metadataFor(ctx, key); err != nil {
		return api.Entry{}, err
	}
	entries := []api.Entry{entry}
	if err := v.attachLinks(ctx, entries); err != nil {
		return api.Entry{}, err
	}
	return entries[0], nil
}

func (v view) writeMetadata(
	ctx context.Context, key Key, metadata map[string]string,
) error {
	for name, value := range metadata {
		if value == "" {
			if _, err := v.tx.ExecContext(ctx, `
				DELETE FROM entry_metadata
				WHERE workspace_id = ? AND entry_id = ? AND key = ?`,
				key.Workspace, key.ID, name,
			); err != nil {
				return fmt.Errorf("removing metadata: %w", err)
			}
			continue
		}

		if _, err := v.tx.ExecContext(ctx, `
			INSERT INTO entry_metadata (workspace_id, entry_id, key, value)
			VALUES (?, ?, ?, ?)
			ON CONFLICT (workspace_id, entry_id, key)
			DO UPDATE SET value = excluded.value`,
			key.Workspace, key.ID, name, value,
		); err != nil {
			return fmt.Errorf("writing metadata: %w", err)
		}
	}

	var count int
	if err := v.tx.QueryRowContext(ctx, `
		SELECT count(*) FROM entry_metadata
		WHERE workspace_id = ? AND entry_id = ?`, key.Workspace, key.ID,
	).Scan(&count); err != nil {
		return fmt.Errorf("checking metadata: %w", err)
	}
	if count > api.MaxMetadata {
		return ErrMetadataLimit
	}
	return nil
}

// Add adds an entry to a workspace, numbered one past the workspace's
// highest entry. Writes are serialised through the store's one
// connection, so no other write can take the same number.
func (s *Store) Add(
	ctx context.Context,
	access Access,
	workspace int64,
	req api.AddEntryRequest,
) (api.Entry, error) {
	v, err := s.begin(ctx, access, false)
	if err != nil {
		return api.Entry{}, fmt.Errorf("adding entry: %w", err)
	}
	defer v.tx.Rollback()

	key := Key{Workspace: workspace}
	if err := v.tx.QueryRowContext(ctx, `
		SELECT coalesce(max(id), 0) + 1 FROM entries
		WHERE workspace_id = ?`, workspace,
	).Scan(&key.ID); err != nil {
		return api.Entry{}, fmt.Errorf("numbering entry: %w", err)
	}
	now := nowUTC()
	if _, err := v.tx.ExecContext(ctx, `
		INSERT INTO entries (
			workspace_id, id, created_utc, modified_utc, type, subject,
			body, origin_user, origin_host, origin_dir,
			origin_claude_session
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		key.Workspace, key.ID, now, now, req.Type, req.Subject, req.Body,
		req.Origin.User, req.Origin.Host, req.Origin.Dir,
		req.Origin.ClaudeSession,
	); err != nil {
		return api.Entry{}, fmt.Errorf("adding entry: %w", err)
	}
	if err := v.writeMetadata(ctx, key, req.Metadata); err != nil {
		return api.Entry{}, err
	}
	if err := v.writeLinks(ctx, key, nil, nil, req.Links); err != nil {
		return api.Entry{}, err
	}
	entry, err := v.getEntry(ctx, key)
	if err != nil {
		return api.Entry{}, fmt.Errorf("reading new entry: %w", err)
	}
	if err := v.tx.Commit(); err != nil {
		return api.Entry{}, fmt.Errorf("committing new entry: %w", err)
	}
	return entry, nil
}

// editContent updates an entry's own fields and its modified time. A
// change to links alone leaves the modified time as it is, so
// organising old entries does not reorder listings.
func (v view) editContent(
	ctx context.Context, key Key, req api.EditEntryRequest,
) error {
	query := `UPDATE entries SET modified_utc = ?`
	args := []any{nowUTC()}
	if req.Type != nil {
		query += `, type = ?`
		args = append(args, *req.Type)
	}
	if req.Subject != nil {
		query += `, subject = ?`
		args = append(args, *req.Subject)
	}
	if req.Body != nil {
		query += `, body = ?`
		args = append(args, *req.Body)
	}
	query += ` WHERE workspace_id = ? AND id = ?`
	args = append(args, key.Workspace, key.ID)
	result, err := v.tx.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("editing entry: %w", err)
	}
	matched, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("checking edited entry: %w", err)
	}
	if matched == 0 {
		return fmt.Errorf("%w: %s", ErrEntryNotFound, v.store.mustRef(key))
	}
	return nil
}

// Edit overwrites an entry's fields and changes its metadata and
// links. No earlier text is kept.
func (s *Store) Edit(
	ctx context.Context,
	access Access,
	key Key,
	req api.EditEntryRequest,
) (api.Entry, error) {
	if !req.ChangesContent() && len(req.AddLinks) == 0 &&
		len(req.RemoveLinks) == 0 && len(req.RemoveLinkNames) == 0 {
		return api.Entry{}, errors.New("nothing to edit")
	}
	v, err := s.begin(ctx, access, false)
	if err != nil {
		return api.Entry{}, fmt.Errorf("editing entry: %w", err)
	}
	defer v.tx.Rollback()

	if req.ChangesContent() {
		if err := v.editContent(ctx, key, req); err != nil {
			return api.Entry{}, err
		}
	} else if err := v.checkEntryExists(ctx, key); err != nil {
		return api.Entry{}, err
	}
	if len(req.Metadata) > 0 {
		if err := v.writeMetadata(ctx, key, req.Metadata); err != nil {
			return api.Entry{}, err
		}
	}
	if err := v.writeLinks(ctx, key, req.RemoveLinkNames,
		req.RemoveLinks, req.AddLinks); err != nil {
		return api.Entry{}, err
	}

	entry, err := v.getEntry(ctx, key)
	if err != nil {
		return api.Entry{}, fmt.Errorf("reading edited entry: %w", err)
	}
	if err := v.tx.Commit(); err != nil {
		return api.Entry{}, fmt.Errorf("committing edited entry: %w", err)
	}
	return entry, nil
}

// Projects lists the projects of a workspace's entries, the most
// recently active first.
func (s *Store) Projects(
	ctx context.Context, workspace int64,
) ([]api.Project, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT m.value, count(*), max(`+activityExpr+`) AS latest
		FROM entry_metadata AS m JOIN entries AS e
			ON e.workspace_id = m.workspace_id AND e.id = m.entry_id
		WHERE m.workspace_id = ? AND m.key = ?
		GROUP BY m.value
		ORDER BY latest DESC, m.value`, workspace, api.ProjectKey,
	)
	if err != nil {
		return nil, fmt.Errorf("listing projects: %w", err)
	}
	defer rows.Close()
	projects := []api.Project{}
	for rows.Next() {
		var project api.Project
		var latest string
		if err := rows.Scan(
			&project.Name, &project.Entries, &latest,
		); err != nil {
			return nil, fmt.Errorf("listing projects: %w", err)
		}
		if project.Latest, err = parseTime(latest); err != nil {
			return nil, err
		}
		projects = append(projects, project)
	}
	return projects, rows.Err()
}
