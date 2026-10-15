package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/airspacesh/hivemind/internal/api"
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
	e.modified_utc, e.revision, e.project, e.subject, e.body, e.deleted,
	e.private, e.private_to, e.origin_user, e.origin_host, e.origin_dir,
	e.origin_claude_session`

// visibleClause is true for a post, by its alias, that the reader can
// read: one private to nobody, or to the reader. It takes the reader's
// user as its one argument. A residual check on the row, it leaves the
// indexes a query reads by to the query's other terms.
func visibleClause(alias string) string {
	return alias + `.private_to IN ('', ?)`
}

type rowScanner interface {
	Scan(...any) error
}

// scanPost reads the postColumns.
func (v view) scanPost(row rowScanner) (api.Post, error) {
	var post api.Post
	var key Key
	var created, modified string
	err := row.Scan(&key.Workspace, &key.ID, &created, &modified,
		&post.Revision, &post.Project, &post.Subject, &post.Body, &post.Deleted,
		&post.Private, &post.PrivateTo, &post.Origin.User, &post.Origin.Host,
		&post.Origin.Dir, &post.Origin.ClaudeSession)
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

// checkPostExists refuses a post that does not exist, or that the
// reader cannot read.
func (v view) checkPostExists(ctx context.Context, key Key) error {
	var exists bool
	if err := v.tx.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM posts AS e
			WHERE e.workspace_id = ? AND e.id = ? AND `+visibleClause("e")+`
		)`, key.Workspace, key.ID, v.access.user,
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
		WHERE e.workspace_id = ? AND e.id = ? AND `+visibleClause("e"),
		key.Workspace, key.ID, v.access.user,
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
	if post.Links, err = v.linksFrom(ctx, key); err != nil {
		return api.Post{}, err
	}
	if post.SupersededBy, err = v.supersedersOf(ctx, key); err != nil {
		return api.Post{}, err
	}
	return post, nil
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

// requestedPost finds the post a request with the same id added to a
// workspace, if any, and refuses an id whose post has another digest.
func (v view) requestedPost(
	ctx context.Context, workspace int64, requestID string, digest int64,
) (Key, bool, error) {
	key := Key{Workspace: workspace}
	var stored int64
	err := v.tx.QueryRowContext(ctx, `
		SELECT id, request_digest FROM posts
		WHERE workspace_id = ? AND request_id = ? AND request_id <> ''`,
		workspace, requestID,
	).Scan(&key.ID, &stored)
	if errors.Is(err, sql.ErrNoRows) {
		return Key{}, false, nil
	}
	if err != nil {
		return Key{}, false, fmt.Errorf("finding the post of request "+
			"%q: %w", requestID, err)
	}
	if stored != digest {
		return Key{}, false, fmt.Errorf("%w: %q added %s with other "+
			"content", ErrRequestReused, requestID, v.store.mustRef(key))
	}
	return key, true, nil
}

// Add adds a post to a workspace, numbered one past the workspace's
// highest post, and reports whether it did. A request whose id already
// added a post adds nothing, and returns that post. Writes are
// serialised through the store's one connection, so no other write can
// take the same number or request id.
func (s *Store) Add(
	ctx context.Context,
	access Access,
	workspace int64,
	req api.AddPostRequest,
) (api.Post, bool, error) {
	v, err := s.begin(ctx, access, false)
	if err != nil {
		return api.Post{}, false, fmt.Errorf("adding post: %w", err)
	}
	defer v.tx.Rollback()

	digest := postDigest(req)
	if req.RequestID != "" {
		key, found, err := v.requestedPost(ctx, workspace, req.RequestID,
			digest)
		if err != nil {
			return api.Post{}, false, err
		}
		if found {
			post, err := v.getPost(ctx, key)
			return post, false, err
		}
	}

	key := Key{Workspace: workspace}
	if err := v.tx.QueryRowContext(ctx, `
		SELECT coalesce(max(id), 0) + 1 FROM posts
		WHERE workspace_id = ?`, workspace,
	).Scan(&key.ID); err != nil {
		return api.Post{}, false, fmt.Errorf("numbering post: %w", err)
	}
	// The new post's own id names no post until the insert, so its
	// links' targets are checked before it.
	for _, link := range req.Links {
		if _, err := v.linkTarget(ctx, key, link); err != nil {
			return api.Post{}, false, err
		}
	}
	revision, err := nextRevision(ctx, v.tx)
	if err != nil {
		return api.Post{}, false, err
	}
	now := nowUTC()
	if _, err := v.tx.ExecContext(ctx, `
		INSERT INTO posts (
			workspace_id, id, created_utc, modified_utc, activity_utc,
			revision, activity_revision, posted_revision, project, subject,
			body, origin_user, origin_host, origin_dir, origin_claude_session,
			request_id, request_digest
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		key.Workspace, key.ID, now, now, now, revision, revision, revision,
		req.Project, req.Subject, req.Body, req.Origin.User,
		req.Origin.Host, req.Origin.Dir, req.Origin.ClaudeSession,
		req.RequestID, digest,
	); err != nil {
		return api.Post{}, false, fmt.Errorf("adding post: %w", err)
	}
	if err := v.writeMetadata(ctx, key, req.Metadata); err != nil {
		return api.Post{}, false, err
	}
	if err := v.writeLinks(ctx, key, nil, nil, req.Links); err != nil {
		return api.Post{}, false, err
	}
	if err := v.recordVersion(ctx, key, req.Origin, now); err != nil {
		return api.Post{}, false, err
	}
	added := postState{owner: req.Origin.User}
	if _, err := v.writeVisibility(ctx, key, added,
		&req.Private); err != nil {
		return api.Post{}, false, err
	}
	post, err := v.getPost(ctx, key)
	if err != nil {
		return api.Post{}, false, fmt.Errorf("reading new post: %w", err)
	}
	if err := v.tx.Commit(); err != nil {
		return api.Post{}, false, fmt.Errorf("committing new post: %w", err)
	}
	return post, true, nil
}

// editContent updates a post's own fields, sets its modified and
// activity times to now, and moves its revisions. A change to links
// alone leaves them all as they are, so organising old posts does not
// reorder listings or show in the changes feed.
func (v view) editContent(
	ctx context.Context, key Key, req api.EditPostRequest, now string,
) error {
	revision, err := nextRevision(ctx, v.tx)
	if err != nil {
		return err
	}
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

// NotOwnerError refuses a change that only the user who wrote the post
// or message can make, by anyone else. Nobody can make it to one that
// records no user. action says what the change does to it, like
// "delete it".
type NotOwnerError struct {
	posted string
	owner  string
	action string
}

func (e *NotOwnerError) Error() string {
	if e.owner == "" {
		return fmt.Sprintf("%s records no user, so nobody can %s",
			e.posted, e.action)
	}
	return fmt.Sprintf("%s was posted by %s, so only %s can %s",
		e.posted, e.owner, e.owner, e.action)
}

// checkEditor refuses a change by editor to something owner posted,
// unless they are the same user. action says what the change does.
func checkEditor(posted fmt.Stringer, owner, editor, action string) error {
	if owner == "" || owner != editor {
		return &NotOwnerError{posted: posted.String(), owner: owner,
			action: action}
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

// postState is what decides whether a post can be changed, and how a
// change to its visibility goes: the user who posted it, whether it is
// deleted, whether its owner made it private, and who alone can read
// it, as the private_to column has it.
type postState struct {
	owner     string
	deleted   bool
	private   bool
	privateTo string
}

// readPostState reads the state of a post the reader can read.
func (v view) readPostState(ctx context.Context, key Key) (postState, error) {
	var state postState
	err := v.tx.QueryRowContext(ctx, `
		SELECT e.origin_user, e.deleted, e.private, e.private_to
		FROM posts AS e
		WHERE e.workspace_id = ? AND e.id = ? AND `+visibleClause("e"),
		key.Workspace, key.ID, v.access.user,
	).Scan(&state.owner, &state.deleted, &state.private, &state.privateTo)
	if errors.Is(err, sql.ErrNoRows) {
		return postState{}, fmt.Errorf("%w: %s", ErrPostNotFound,
			v.store.mustRef(key))
	}
	if err != nil {
		return postState{}, fmt.Errorf("reading post state: %w", err)
	}
	return state, nil
}

// SharedError refuses to make a post private, directly or by moving it
// under a private post, when other users have posts or messages under
// it that they have not deleted, since what they wrote would vanish for
// them.
type SharedError struct {
	post string
}

func (e *SharedError) Error() string {
	return "others have posts or messages under " + e.post + " that " +
		"they have not deleted, so it cannot be made private"
}

// othersPostedUnder reports whether anyone but owner wrote a post or a
// message anywhere in a post's subtree, the post itself included, that
// is not deleted.
func (v view) othersPostedUnder(
	ctx context.Context, key Key, owner string,
) (bool, error) {
	var found bool
	err := v.tx.QueryRowContext(ctx, `
		WITH RECURSIVE `+subtreeCTE("subtree")+`
		SELECT EXISTS (
			SELECT 1 FROM subtree CROSS JOIN posts AS e
				ON e.workspace_id = subtree.workspace_id
				AND e.id = subtree.id
			WHERE e.origin_user <> ? AND NOT e.deleted
		) OR EXISTS (
			SELECT 1 FROM subtree `+eachChat+`
				ON m.workspace_id = subtree.workspace_id
				AND m.post_id = subtree.id
			WHERE m.origin_user <> ? AND NOT m.deleted
		)`, key.Workspace, key.ID, owner, owner,
	).Scan(&found)
	if err != nil {
		return false, fmt.Errorf("checking who posted under %s: %w",
			v.store.mustRef(key), err)
	}
	return found, nil
}

// parentPrivateTo reads who alone can read a post's parent, or nothing
// for a post with no parent.
func (v view) parentPrivateTo(ctx context.Context, key Key) (string, error) {
	var privateTo string
	err := v.tx.QueryRowContext(ctx, `
		SELECT p.private_to FROM post_links AS l CROSS JOIN posts AS p
			ON p.workspace_id = l.target_workspace_id AND p.id = l.target_id
		WHERE l.source_workspace_id = ? AND l.source_id = ?
			AND l.name = 'parent'`, key.Workspace, key.ID,
	).Scan(&privateTo)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("reading the parent of %s: %w",
			v.store.mustRef(key), err)
	}
	return privateTo, nil
}

// setPrivateTo sets who alone can read a post, and the posts under it,
// each of which takes the owner of the highest private post at or above
// it. The walk skips a post its owner made private whose reader stays
// the same, since nothing under it changes either.
func (v view) setPrivateTo(
	ctx context.Context, key Key, privateTo string,
) error {
	if _, err := v.tx.ExecContext(ctx, `
		WITH RECURSIVE inheriting(workspace_id, id, private_to) AS (
			SELECT ?, ?, ?
			UNION ALL
			SELECT child.workspace_id, child.id,
				CASE WHEN inheriting.private_to = '' AND child.private
					THEN child.origin_user ELSE inheriting.private_to END
			FROM inheriting JOIN post_links AS l
				ON l.target_workspace_id = inheriting.workspace_id
				AND l.target_id = inheriting.id AND l.name = 'parent'
			JOIN posts AS child
				ON child.workspace_id = l.source_workspace_id
				AND child.id = l.source_id
			WHERE NOT child.private
				OR child.private_to <> CASE WHEN inheriting.private_to = ''
					THEN child.origin_user ELSE inheriting.private_to END
		)
		UPDATE posts SET private_to = inheriting.private_to
		FROM inheriting
		WHERE posts.workspace_id = inheriting.workspace_id
			AND posts.id = inheriting.id`,
		key.Workspace, key.ID, privateTo,
	); err != nil {
		return fmt.Errorf("setting who can read %s: %w",
			v.store.mustRef(key), err)
	}
	return nil
}

// writeVisibility makes a post private or public, when private is set,
// and brings who alone can read it and the posts under it in line with
// its own flag and its parent. It reports whether the post's
// visibility changed, either way.
func (v view) writeVisibility(
	ctx context.Context, key Key, state postState, private *bool,
) (bool, error) {
	isPrivate := state.private
	if private != nil {
		isPrivate = *private
	}
	privateTo, err := v.parentPrivateTo(ctx, key)
	if err != nil {
		return false, err
	}
	if privateTo == "" && isPrivate {
		privateTo = state.owner
	}

	if isPrivate != state.private {
		if _, err := v.tx.ExecContext(ctx, `
			UPDATE posts SET private = ? WHERE workspace_id = ? AND id = ?`,
			isPrivate, key.Workspace, key.ID,
		); err != nil {
			return false, fmt.Errorf("making post private: %w", err)
		}
	}
	if privateTo == state.privateTo {
		return isPrivate != state.private, nil
	}
	if privateTo != "" {
		shared, err := v.othersPostedUnder(ctx, key, privateTo)
		if err != nil {
			return false, err
		}
		if shared {
			return false, &SharedError{post: v.store.mustRef(key).String()}
		}
	}
	if err := v.setPrivateTo(ctx, key, privateTo); err != nil {
		return false, err
	}
	return true, nil
}

// Edit overwrites a post's fields and changes its metadata and links.
// An edit that changes any of them makes the post's next version, and
// the versions before keep what it overwrote. Anyone can edit a public
// post. Only the user who wrote the post can edit it while it is
// private or under a private post, or make it private or public. Nobody
// can edit a deleted post.
func (s *Store) Edit(
	ctx context.Context,
	access Access,
	key Key,
	req api.EditPostRequest,
) (api.Post, error) {
	if !req.ChangesContent() && len(req.AddLinks) == 0 &&
		len(req.RemoveLinks) == 0 && len(req.RemoveLinkNames) == 0 &&
		req.Private == nil {
		return api.Post{}, errors.New("nothing to edit")
	}
	v, err := s.begin(ctx, access, false)
	if err != nil {
		return api.Post{}, fmt.Errorf("editing post: %w", err)
	}
	defer v.tx.Rollback()

	state, err := v.readPostState(ctx, key)
	if err != nil {
		return api.Post{}, err
	}
	ref := v.store.mustRef(key)
	if state.privateTo != "" {
		if err := checkEditor(ref, state.owner, req.Origin.User,
			"edit it"); err != nil {
			return api.Post{}, err
		}
	}
	if req.Private != nil {
		if err := checkEditor(ref, state.owner, req.Origin.User,
			"make it private or public"); err != nil {
			return api.Post{}, err
		}
	}
	if state.deleted {
		return api.Post{}, &DeletedError{deleted: ref.String()}
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
	// A post that becomes public, or private, changes for its readers,
	// as an edit of its content does, so it moves to the top of the
	// listings and shows in the changes feed. A move into or out of a
	// private tree counts too.
	visibilityChanged, err := v.writeVisibility(ctx, key, state,
		req.Private)
	if err != nil {
		return api.Post{}, err
	}
	now := nowUTC()
	if req.ChangesContent() || visibilityChanged {
		if err := v.editContent(ctx, key, req, now); err != nil {
			return api.Post{}, err
		}
	}
	if err := v.recordVersion(ctx, key, req.Origin, now); err != nil {
		return api.Post{}, err
	}

	post, err := v.getPost(ctx, key)
	if err != nil {
		return api.Post{}, fmt.Errorf("reading edited post: %w", err)
	}
	if post.Version, err = v.currentVersion(ctx, key); err != nil {
		return api.Post{}, err
	}
	if err := v.tx.Commit(); err != nil {
		return api.Post{}, fmt.Errorf("committing edited post: %w", err)
	}
	return post, nil
}

// Delete clears a post's subject and body, removes its metadata, and
// marks it deleted, and clears the same from every version of it, so no
// earlier text is kept. Its id, project, origin, creation time, links
// and chat stay, so the posts under it stay in place. Deleting a
// deleted post changes nothing. Only the user who wrote the post can
// delete it.
func (s *Store) Delete(
	ctx context.Context, access Access, key Key, req api.DeleteRequest,
) (api.Post, error) {
	v, err := s.begin(ctx, access, false)
	if err != nil {
		return api.Post{}, fmt.Errorf("deleting post: %w", err)
	}
	defer v.tx.Rollback()

	state, err := v.readPostState(ctx, key)
	if err != nil {
		return api.Post{}, err
	}
	if err := checkEditor(v.store.mustRef(key), state.owner, req.User,
		"delete it"); err != nil {
		return api.Post{}, err
	}
	if state.deleted {
		return v.getPost(ctx, key)
	}

	empty := ""
	if err := v.editContent(ctx, key, api.EditPostRequest{
		Subject: &empty, Body: &empty,
	}, nowUTC()); err != nil {
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
	if err := v.purgeVersions(ctx, key); err != nil {
		return api.Post{}, err
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
