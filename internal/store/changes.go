package store

import (
	"context"
	"database/sql"
	"errors"
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

// revisionSeen reads the revision of the latest change a transaction
// sees. Read in the transaction that reads some data, it is a cursor
// from which a read of changes misses nothing that data left out.
func revisionSeen(ctx context.Context, tx *sql.Tx) (int64, error) {
	var revision int64
	if err := tx.QueryRowContext(ctx, `
		SELECT revision FROM revision_counter`,
	).Scan(&revision); err != nil {
		return 0, fmt.Errorf("reading the latest revision: %w", err)
	}
	return revision, nil
}

// revisionsPerRead bounds the revisions a read of changes checks, so a
// filter that matches few changes still answers quickly, however far
// behind its cursor is.
const revisionsPerRead = 10000

// OriginKey names whose a change is by the user and Claude Code session
// of its origin, compared exactly. An empty session is a value of its
// own, so a key with none names only the changes written outside a
// session.
type OriginKey struct {
	User          string
	ClaudeSession string
}

// excludeOrigin leaves out the rows of table alias whose origin has the
// key, when there is one.
func (builder *whereBuilder) excludeOrigin(alias string, key *OriginKey) {
	if key == nil {
		return
	}
	builder.clauses = append(builder.clauses, `NOT (`+alias+
		`.origin_user = ? AND `+alias+`.origin_claude_session = ?)`)
	builder.args = append(builder.args, key.User, key.ClaudeSession)
}

// addUnder keeps to posts e that are the post under or lie beneath it,
// when there is one. It walks up from each post rather than down from
// under, so its cost follows the changes read, not the subtree.
func (builder *whereBuilder) addUnder(under *Key) {
	if under == nil {
		return
	}
	builder.clauses = append(builder.clauses, `EXISTS (
		WITH RECURSIVE above(workspace_id, id) AS (
			SELECT e.workspace_id, e.id
			UNION ALL
			SELECT l.target_workspace_id, l.target_id
			FROM above JOIN post_links AS l
				ON l.source_workspace_id = above.workspace_id
				AND l.source_id = above.id AND l.name = 'parent'
		)
		SELECT 1 FROM above WHERE above.workspace_id = ? AND above.id = ?
	)`)
	builder.args = append(builder.args, under.Workspace, under.ID)
}

// changeKey names a change by its revision and the post or message that
// changed, where message 0 stands for the post itself.
type changeKey struct {
	revision int64
	post     int64
	message  int64
}

// readChangeKeys reads the changes of a workspace after the revision
// since and up to through that match a filter, leaving out those whose
// origin has the key exclude, at most limit of them, oldest first. A
// post's change matches as the post, and a message's as a message in
// its chat.
func (v view) readChangeKeys(
	ctx context.Context,
	workspace int64,
	since, through int64,
	filter Filter,
	exclude *OriginKey,
	limit int,
) ([]changeKey, error) {
	postWhere, err := buildWhere(filter)
	if err != nil {
		return nil, err
	}
	postWhere.addUnder(filter.Under)
	postWhere.excludeOrigin("e", exclude)
	postWhere.clauses = append(postWhere.clauses, `e.workspace_id = ?`,
		`e.revision > ?`, `e.revision <= ?`)
	postWhere.args = append(postWhere.args, workspace, since, through)

	messageWhere, err := buildMessageWhere(filter)
	if err != nil {
		return nil, err
	}
	messageWhere.addUnder(filter.Under)
	messageWhere.excludeOrigin("m", exclude)
	messageWhere.clauses = append(messageWhere.clauses,
		`m.workspace_id = ?`, `m.revision > ?`, `m.revision <= ?`)
	messageWhere.args = append(messageWhere.args, workspace, since, through)

	// CROSS JOIN keeps SQLite reading the messages in revision order,
	// rather than reading posts first for a filter on them.
	query := `
		SELECT revision, post_id, number FROM (
			SELECT e.revision, e.id AS post_id, 0 AS number
			FROM posts AS e` + postWhere.sql() + `
			ORDER BY e.revision LIMIT ?
		)
		UNION ALL
		SELECT revision, post_id, number FROM (
			SELECT m.revision, m.post_id, m.id AS number
			FROM post_messages AS m CROSS JOIN posts AS e
				ON e.workspace_id = m.workspace_id AND e.id = m.post_id` +
		messageWhere.sql() + `
			ORDER BY m.revision LIMIT ?
		)
		ORDER BY revision LIMIT ?`
	args := append(append(append(postWhere.args, limit),
		messageWhere.args...), limit, limit)
	rows, err := v.tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("reading changes: %w", err)
	}
	defer rows.Close()
	var keys []changeKey
	for rows.Next() {
		var key changeKey
		if err := rows.Scan(
			&key.revision, &key.post, &key.message,
		); err != nil {
			return nil, fmt.Errorf("reading changes: %w", err)
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}

// Changes returns up to limit changes in a workspace after the
// revision since that match a filter, oldest first, leaving out those
// whose origin has the key exclude, when there is one. The filter's
// conditions and Under apply, and its other fields are ignored.
//
// The page's cursor is the highest revision the read checked. Every
// change up to it is in the page or did not match, so a read from the
// cursor misses none, even when the page is empty.
func (s *Store) Changes(
	ctx context.Context,
	access Access,
	workspace int64,
	since int64,
	filter Filter,
	exclude *OriginKey,
	limit int,
) (api.ChangesPage, error) {
	return s.changes(ctx, access, workspace, since, filter, exclude, limit,
		revisionsPerRead)
}

// changes is Changes checking at most span revisions.
func (s *Store) changes(
	ctx context.Context,
	access Access,
	workspace int64,
	since int64,
	filter Filter,
	exclude *OriginKey,
	limit int,
	span int64,
) (api.ChangesPage, error) {
	v, err := s.begin(ctx, access, true)
	if err != nil {
		return api.ChangesPage{}, fmt.Errorf("reading changes: %w", err)
	}
	defer v.tx.Rollback()
	if filter.Under != nil {
		if err := v.checkPostExists(ctx, *filter.Under); err != nil {
			return api.ChangesPage{}, err
		}
	}

	// The latest revision and the changes come from one snapshot, so
	// every change up to that revision is there to be checked.
	latest, err := revisionSeen(ctx, v.tx)
	if err != nil {
		return api.ChangesPage{}, err
	}
	through := max(since, latest)
	if latest-since > span {
		through = since + span
	}
	keys, err := v.readChangeKeys(ctx, workspace, since, through, filter,
		exclude, limit+1)
	if err != nil {
		return api.ChangesPage{}, err
	}
	page := api.ChangesPage{
		Changes: []api.Change{},
		Cursor:  through,
		More:    len(keys) > limit || through < latest,
	}
	if len(keys) > limit {
		keys = keys[:limit]
		page.Cursor = keys[limit-1].revision
	}
	listed := map[Key]api.ListedPost{}
	for _, key := range keys {
		postKey := Key{Workspace: workspace, ID: key.post}
		post, ok := listed[postKey]
		if !ok {
			found, err := v.getPost(ctx, postKey)
			if err != nil {
				return api.ChangesPage{}, err
			}
			if post, err = v.listPost(ctx, found); err != nil {
				return api.ChangesPage{}, err
			}
			listed[postKey] = post
		}
		change := api.Change{Post: post}
		if key.message != 0 {
			message, err := s.getMessage(ctx, v.tx, postKey, key.message)
			if err != nil {
				return api.ChangesPage{}, err
			}
			change.Message = &message
		}
		page.Changes = append(page.Changes, change)
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

// PostedRevision returns the revision of the change that added a post,
// or that sent its message number when that is not 0. Later changes
// leave it alone, so a reader who has seen the post or message, in any
// state, has seen every change up to it.
func (s *Store) PostedRevision(
	ctx context.Context, access Access, post Key, number int64,
) (int64, error) {
	v, err := s.begin(ctx, access, true)
	if err != nil {
		return 0, fmt.Errorf("reading a posted revision: %w", err)
	}
	defer v.tx.Rollback()
	if err := v.checkPostExists(ctx, post); err != nil {
		return 0, err
	}

	var revision int64
	if number == 0 {
		err = v.tx.QueryRowContext(ctx, `
			SELECT posted_revision FROM posts
			WHERE workspace_id = ? AND id = ?`, post.Workspace, post.ID,
		).Scan(&revision)
	} else {
		err = v.tx.QueryRowContext(ctx, `
			SELECT posted_revision FROM post_messages
			WHERE workspace_id = ? AND post_id = ? AND id = ?`,
			post.Workspace, post.ID, number,
		).Scan(&revision)
	}
	if errors.Is(err, sql.ErrNoRows) {
		ref := api.MessageRef{Post: s.mustRef(post), Number: number}
		return 0, fmt.Errorf("%w: %s", ErrMessageNotFound, ref)
	}
	if err != nil {
		return 0, fmt.Errorf("reading a posted revision: %w", err)
	}
	return revision, nil
}
