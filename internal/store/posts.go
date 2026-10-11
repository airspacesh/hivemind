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

// begin starts a transaction seen through access, on a read
// connection when it is readOnly and on the write connection otherwise.
func (s *Store) begin(
	ctx context.Context, access Access, readOnly bool,
) (view, error) {
	db := s.db
	if readOnly {
		db = s.reads
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: readOnly})
	if err != nil {
		return view{}, err
	}
	return view{store: s, tx: tx, access: access}, nil
}

const postColumns = `e.workspace_id, e.id, e.created_utc,
	e.modified_utc, e.project, e.subject, e.body, e.deleted,
	e.origin_user, e.origin_host, e.origin_dir, e.origin_claude_session`

type rowScanner interface {
	Scan(...any) error
}

// scanPost reads the postColumns, and any extra columns into extra.
func (v view) scanPost(row rowScanner, extra ...any) (api.Post, error) {
	var post api.Post
	var key Key
	var created, modified string
	err := row.Scan(append([]any{
		&key.Workspace, &key.ID, &created, &modified,
		&post.Project, &post.Subject, &post.Body, &post.Deleted,
		&post.Origin.User, &post.Origin.Host,
		&post.Origin.Dir, &post.Origin.ClaudeSession,
	}, extra...)...)
	if err != nil {
		return api.Post{}, err
	}
	post.Ref = v.store.mustRef(key)
	if post.Created, err = parseTime(created); err != nil {
		return api.Post{}, err
	}
	if post.Modified, err = parseTime(modified); err != nil {
		return api.Post{}, err
	}
	return post, nil
}

func (v view) checkPostExists(ctx context.Context, key Key) error {
	var exists bool
	if err := v.tx.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM posts WHERE workspace_id = ? AND id = ?
		)`, key.Workspace, key.ID,
	).Scan(&exists); err != nil {
		return fmt.Errorf("checking post: %w", err)
	}
	if !exists {
		return fmt.Errorf("%w: %s", ErrPostNotFound, v.store.mustRef(key))
	}
	return nil
}

func (v view) metadataFor(
	ctx context.Context, key Key,
) (map[string]string, error) {
	rows, err := v.tx.QueryContext(ctx, `
		SELECT key, value FROM post_metadata
		WHERE workspace_id = ? AND post_id = ? ORDER BY key`,
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

func (v view) getPost(ctx context.Context, key Key) (api.Post, error) {
	row := v.tx.QueryRowContext(ctx, `
		SELECT `+postColumns+` FROM posts AS e
		WHERE e.workspace_id = ? AND e.id = ?`, key.Workspace, key.ID,
	)
	post, err := v.scanPost(row)
	if errors.Is(err, sql.ErrNoRows) {
		return api.Post{}, fmt.Errorf("%w: %s", ErrPostNotFound,
			v.store.mustRef(key))
	}
	if err != nil {
		return api.Post{}, err
	}
	if post.Metadata, err = v.metadataFor(ctx, key); err != nil {
		return api.Post{}, err
	}
	posts := []api.Post{post}
	if err := v.attachLinks(ctx, posts); err != nil {
		return api.Post{}, err
	}
	return posts[0], nil
}

func (v view) writeMetadata(
	ctx context.Context, key Key, metadata map[string]string,
) error {
	for name, value := range metadata {
		if value == "" {
			if _, err := v.tx.ExecContext(ctx, `
				DELETE FROM post_metadata
				WHERE workspace_id = ? AND post_id = ? AND key = ?`,
				key.Workspace, key.ID, name,
			); err != nil {
				return fmt.Errorf("removing metadata: %w", err)
			}
			continue
		}

		if _, err := v.tx.ExecContext(ctx, `
			INSERT INTO post_metadata (workspace_id, post_id, key, value)
			VALUES (?, ?, ?, ?)
			ON CONFLICT (workspace_id, post_id, key)
			DO UPDATE SET value = excluded.value`,
			key.Workspace, key.ID, name, value,
		); err != nil {
			return fmt.Errorf("writing metadata: %w", err)
		}
	}

	var count int
	if err := v.tx.QueryRowContext(ctx, `
		SELECT count(*) FROM post_metadata
		WHERE workspace_id = ? AND post_id = ?`, key.Workspace, key.ID,
	).Scan(&count); err != nil {
		return fmt.Errorf("checking metadata: %w", err)
	}
	if count > api.MaxMetadata {
		return ErrMetadataLimit
	}
	return nil
}

// Add adds a post to a workspace, numbered one past the workspace's
// highest post. Writes are serialised through the store's one
// connection, so no other write can take the same number.
func (s *Store) Add(
	ctx context.Context,
	access Access,
	workspace int64,
	req api.AddPostRequest,
) (api.Post, error) {
	v, err := s.begin(ctx, access, false)
	if err != nil {
		return api.Post{}, fmt.Errorf("adding post: %w", err)
	}
	defer v.tx.Rollback()

	key := Key{Workspace: workspace}
	if err := v.tx.QueryRowContext(ctx, `
		SELECT coalesce(max(id), 0) + 1 FROM posts
		WHERE workspace_id = ?`, workspace,
	).Scan(&key.ID); err != nil {
		return api.Post{}, fmt.Errorf("numbering post: %w", err)
	}
	revision, err := nextRevision(ctx, v.tx)
	if err != nil {
		return api.Post{}, err
	}
	now := nowUTC()
	if _, err := v.tx.ExecContext(ctx, `
		INSERT INTO posts (
			workspace_id, id, created_utc, modified_utc, activity_utc,
			revision, activity_revision, project, subject, body,
			origin_user, origin_host, origin_dir, origin_claude_session
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		key.Workspace, key.ID, now, now, now, revision, revision, req.Project,
		req.Subject, req.Body, req.Origin.User, req.Origin.Host,
		req.Origin.Dir, req.Origin.ClaudeSession,
	); err != nil {
		return api.Post{}, fmt.Errorf("adding post: %w", err)
	}
	if err := v.writeMetadata(ctx, key, req.Metadata); err != nil {
		return api.Post{}, err
	}
	if err := v.writeLinks(ctx, key, nil, nil, req.Links); err != nil {
		return api.Post{}, err
	}
	post, err := v.getPost(ctx, key)
	if err != nil {
		return api.Post{}, fmt.Errorf("reading new post: %w", err)
	}
	if err := v.tx.Commit(); err != nil {
		return api.Post{}, fmt.Errorf("committing new post: %w", err)
	}
	return post, nil
}

// editContent updates a post's own fields, its modified and activity
// times, and its revisions. A change to links alone leaves them all as
// they are, so organising old posts does not reorder listings or show
// in the changes feed.
func (v view) editContent(
	ctx context.Context, key Key, req api.EditPostRequest,
) error {
	revision, err := nextRevision(ctx, v.tx)
	if err != nil {
		return err
	}
	now := nowUTC()
	query := `UPDATE posts SET modified_utc = ?, activity_utc = ?,
		revision = ?, activity_revision = ?`
	args := []any{now, now, revision, revision}
	if req.Project != nil {
		query += `, project = ?`
		args = append(args, *req.Project)
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
	if _, err := v.tx.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("editing post: %w", err)
	}
	return nil
}

// NotOwnerError refuses an edit or delete by anyone but the user who
// wrote the post or message it changes. Nobody can change one that
// records no user.
type NotOwnerError struct {
	posted string
	owner  string
}

func (e *NotOwnerError) Error() string {
	if e.owner == "" {
		return e.posted + " records no user, so nobody can edit or " +
			"delete it"
	}
	return fmt.Sprintf("%s was posted by %s, so only %s can edit or "+
		"delete it", e.posted, e.owner, e.owner)
}

// checkEditor refuses an edit or delete by editor of something owner
// posted, unless they are the same user.
func checkEditor(posted fmt.Stringer, owner, editor string) error {
	if owner == "" || owner != editor {
		return &NotOwnerError{posted: posted.String(), owner: owner}
	}
	return nil
}

// DeletedError refuses an edit of a deleted post or message, which
// would bring back what the delete removed.
type DeletedError struct {
	deleted string
}

func (e *DeletedError) Error() string {
	return e.deleted + " was deleted, so it cannot be edited"
}

// editState reads what decides whether a post can be changed: the user
// who posted it, and whether it is deleted.
func (v view) editState(
	ctx context.Context, key Key,
) (owner string, deleted bool, err error) {
	err = v.tx.QueryRowContext(ctx, `
		SELECT origin_user, deleted FROM posts
		WHERE workspace_id = ? AND id = ?`, key.Workspace, key.ID,
	).Scan(&owner, &deleted)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, fmt.Errorf("%w: %s", ErrPostNotFound,
			v.store.mustRef(key))
	}
	if err != nil {
		return "", false, fmt.Errorf("reading post state: %w", err)
	}
	return owner, deleted, nil
}

// Edit overwrites a post's fields and changes its metadata and links.
// No earlier text is kept. Only the user who wrote the post can edit
// it, and nobody can edit a deleted post.
func (s *Store) Edit(
	ctx context.Context,
	access Access,
	key Key,
	req api.EditPostRequest,
) (api.Post, error) {
	if !req.ChangesContent() && len(req.AddLinks) == 0 &&
		len(req.RemoveLinks) == 0 && len(req.RemoveLinkNames) == 0 {
		return api.Post{}, errors.New("nothing to edit")
	}
	v, err := s.begin(ctx, access, false)
	if err != nil {
		return api.Post{}, fmt.Errorf("editing post: %w", err)
	}
	defer v.tx.Rollback()

	owner, deleted, err := v.editState(ctx, key)
	if err != nil {
		return api.Post{}, err
	}
	ref := v.store.mustRef(key)
	if err := checkEditor(ref, owner, req.User); err != nil {
		return api.Post{}, err
	}
	if deleted {
		return api.Post{}, &DeletedError{deleted: ref.String()}
	}
	if req.ChangesContent() {
		if err := v.editContent(ctx, key, req); err != nil {
			return api.Post{}, err
		}
	}
	if len(req.Metadata) > 0 {
		if err := v.writeMetadata(ctx, key, req.Metadata); err != nil {
			return api.Post{}, err
		}
	}
	if err := v.writeLinks(ctx, key, req.RemoveLinkNames,
		req.RemoveLinks, req.AddLinks); err != nil {
		return api.Post{}, err
	}

	post, err := v.getPost(ctx, key)
	if err != nil {
		return api.Post{}, fmt.Errorf("reading edited post: %w", err)
	}
	if err := v.tx.Commit(); err != nil {
		return api.Post{}, fmt.Errorf("committing edited post: %w", err)
	}
	return post, nil
}

// Delete clears a post's subject and body, removes its metadata, and
// marks it deleted, and no earlier text is kept. Its id, project,
// origin, creation time, links and chat stay, so the posts under it
// stay in place. Deleting a deleted post changes nothing. Only the user
// who wrote the post can delete it.
func (s *Store) Delete(
	ctx context.Context, access Access, key Key, req api.DeleteRequest,
) (api.Post, error) {
	v, err := s.begin(ctx, access, false)
	if err != nil {
		return api.Post{}, fmt.Errorf("deleting post: %w", err)
	}
	defer v.tx.Rollback()

	owner, deleted, err := v.editState(ctx, key)
	if err != nil {
		return api.Post{}, err
	}
	if err := checkEditor(v.store.mustRef(key), owner,
		req.User); err != nil {
		return api.Post{}, err
	}
	if deleted {
		return v.getPost(ctx, key)
	}

	empty := ""
	if err := v.editContent(ctx, key, api.EditPostRequest{
		Subject: &empty, Body: &empty,
	}); err != nil {
		return api.Post{}, err
	}
	if _, err := v.tx.ExecContext(ctx, `
		UPDATE posts SET deleted = 1 WHERE workspace_id = ? AND id = ?`,
		key.Workspace, key.ID,
	); err != nil {
		return api.Post{}, fmt.Errorf("marking post deleted: %w", err)
	}
	if _, err := v.tx.ExecContext(ctx, `
		DELETE FROM post_metadata WHERE workspace_id = ? AND post_id = ?`,
		key.Workspace, key.ID,
	); err != nil {
		return api.Post{}, fmt.Errorf("removing metadata: %w", err)
	}
	post, err := v.getPost(ctx, key)
	if err != nil {
		return api.Post{}, fmt.Errorf("reading deleted post: %w", err)
	}
	if err := v.tx.Commit(); err != nil {
		return api.Post{}, fmt.Errorf("committing deleted post: %w", err)
	}
	return post, nil
}
