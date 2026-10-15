package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/airspacesh/hivemind/internal/api"
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
// of whoever made it, compared exactly. An empty session is a value of
// its own, so a key with none names only the changes written outside a
// session.
type OriginKey struct {
	User          string
	ClaudeSession string
}

// excludeOrigin leaves out the rows whose origin, the user and Claude
// Code session the two expressions give, has the key, when there is
// one.
func (builder *whereBuilder) excludeOrigin(
	user, session string, key *OriginKey,
) {
	if key == nil {
		return
	}
	builder.clauses = append(builder.clauses,
		`NOT (`+user+` = ? AND `+session+` = ?)`)
	builder.args = append(builder.args, key.User, key.ClaudeSession)
}

// postChangeMaker gives a column of the origin of whoever made the
// latest change to a post e: the maker of the version that change made,
// or else the post's own origin, since only its owner makes a change
// that makes no version, a delete or a change of who can read it.
func postChangeMaker(column string) string {
	return `coalesce((
		SELECT pv.` + column + ` FROM post_versions AS pv
		WHERE pv.workspace_id = e.workspace_id AND pv.post_id = e.id
			AND pv.revision = e.revision), e.` + column + `)`
}

// addUnder keeps to posts e that are the post under or lie beneath it,
// when there is one.
func (builder *whereBuilder) addUnder(under *Key) {
	if under == nil {
		return
	}
	clause, args := beneathClause([]Key{*under})
	builder.clauses = append(builder.clauses, clause)
	builder.args = append(builder.args, args...)
}

// addNotUnder leaves out the posts e that are one of the posts notUnder
// or lie beneath one, and so the messages in their chats.
func (builder *whereBuilder) addNotUnder(notUnder []Key) {
	if len(notUnder) == 0 {
		return
	}
	clause, args := beneathClause(notUnder)
	builder.clauses = append(builder.clauses, `NOT `+clause)
	builder.args = append(builder.args, args...)
}

// beneathClause holds when a post e is one of the posts keys or lies
// beneath one. It walks up from e rather than down from keys, so its
// cost follows the posts read and their depth, not the subtrees.
func beneathClause(keys []Key) (string, []any) {
	matches := make([]string, len(keys))
	var args []any
	for i, key := range keys {
		matches[i] = `(above.workspace_id = ? AND above.id = ?)`
		args = append(args, key.Workspace, key.ID)
	}
	return `EXISTS (
		WITH RECURSIVE above(workspace_id, id) AS (
			SELECT e.workspace_id, e.id
			UNION ALL
			SELECT l.target_workspace_id, l.target_id
			FROM above JOIN post_links AS l
				ON l.source_workspace_id = above.workspace_id
				AND l.source_id = above.id AND l.name = 'parent'
		)
		SELECT 1 FROM above WHERE ` + strings.Join(matches, ` OR `) + `
	)`, args
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
// maker has the key exclude, at most limit of them, oldest first. The
// filter's Items says whether to read the changes to posts, to messages,
// or to both. A post's change matches as the post, and a message's as a
// message in its chat. The changes to posts the reader cannot read, and
// to their chats, are left out, so a post made private drops out of the
// feed of everyone else.
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
	postWhere.addNotUnder(filter.NotUnder)
	postWhere.excludeOrigin(postChangeMaker("origin_user"),
		postChangeMaker("origin_claude_session"), exclude)
	postWhere.clauses = append(postWhere.clauses, `e.workspace_id = ?`,
		`e.revision > ?`, `e.revision <= ?`, visibleClause("e"))
	postWhere.args = append(postWhere.args, workspace, since, through,
		v.access.user)

	messageWhere, err := buildMessageWhere(filter)
	if err != nil {
		return nil, err
	}
	messageWhere.addUnder(filter.Under)
	messageWhere.addNotUnder(filter.NotUnder)
	messageWhere.excludeOrigin("m.origin_user", "m.origin_claude_session",
		exclude)
	messageWhere.clauses = append(messageWhere.clauses,
		`m.workspace_id = ?`, `m.revision > ?`, `m.revision <= ?`,
		visibleClause("e"))
	messageWhere.args = append(messageWhere.args, workspace, since, through,
		v.access.user)

	// CROSS JOIN keeps SQLite reading the messages in revision order,
	// rather than reading posts first for a filter on them.
	var selects []string
	var args []any
	if filter.Items.HasPosts() {
		selects = append(selects, `
			SELECT revision, post_id, number FROM (
				SELECT e.revision, e.id AS post_id, 0 AS number
				FROM posts AS e`+postWhere.sql()+`
				ORDER BY e.revision LIMIT ?
			)`)
		args = append(append(args, postWhere.args...), limit)
	}
	if filter.Items.HasChat() {
		selects = append(selects, `
			SELECT revision, post_id, number FROM (
				SELECT m.revision, m.post_id, m.id AS number
				FROM post_messages AS m CROSS JOIN posts AS e
					ON e.workspace_id = m.workspace_id
					AND e.id = m.post_id`+messageWhere.sql()+`
				ORDER BY m.revision LIMIT ?
			)`)
		args = append(append(args, messageWhere.args...), limit)
	}
	query := strings.Join(selects, " UNION ALL ") +
		` ORDER BY revision LIMIT ?`
	rows, err := v.tx.QueryContext(ctx, query, append(args, limit)...)
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
// conditions, Under, NotUnder and Items apply, and its other fields
// are ignored.
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
	if err := v.checkSubtreesExist(ctx, filter); err != nil {
		return api.ChangesPage{}, err
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

// firstRankFrom reads the first revision in a workspace at or after the
// revision from and before the revision to that a read ranks by, and
// the time of its change. A read ranks by the changes to posts and
// messages, as the changes feed does, by the latest activity in a post
// or its chat, as a listing without chat does, or by the posting of a
// message, as a read of chat does. Each of these columns sits in an
// index after workspace_id. It reports false when there is none.
func firstRankFrom(
	ctx context.Context, tx *sql.Tx, workspace int64, from, to int64,
) (int64, string, bool, error) {
	var revision int64
	var at string
	err := tx.QueryRowContext(ctx, `
		SELECT revision, at FROM (
			SELECT * FROM (
				SELECT revision, modified_utc AS at FROM posts
				WHERE workspace_id = ?1 AND revision >= ?2 AND revision < ?3
				ORDER BY revision LIMIT 1
			)
			UNION ALL
			SELECT * FROM (
				SELECT activity_revision, activity_utc FROM posts
				WHERE workspace_id = ?1 AND activity_revision >= ?2
					AND activity_revision < ?3
				ORDER BY activity_revision LIMIT 1
			)
			UNION ALL
			SELECT * FROM (
				SELECT revision, modified_utc FROM post_messages
				WHERE workspace_id = ?1 AND revision >= ?2 AND revision < ?3
				ORDER BY revision LIMIT 1
			)
			UNION ALL
			SELECT * FROM (
				SELECT posted_revision, created_utc FROM post_messages
				WHERE workspace_id = ?1 AND posted_revision >= ?2
					AND posted_revision < ?3
				ORDER BY posted_revision LIMIT 1
			)
		)
		ORDER BY revision LIMIT 1`, workspace, from, to,
	).Scan(&revision, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", false, nil
	}
	if err != nil {
		return 0, "", false, fmt.Errorf("reading a change: %w", err)
	}
	return revision, at, true, nil
}

// LastRevisionBefore returns the last revision in a workspace, of those
// firstRankFrom reads, whose change was made before a time, or 0 when
// there is none. Every read ranks by one of those revisions, so what a
// read ranks after it changed or was posted at or after the time, and
// what it ranks up to it came before the time. A read bounded by a time
// thus reads a range of the index it ranks by.
//
// No index orders changes by time, so it searches by revision. A change
// takes its revision and its time in its write transaction, and those
// run one at a time, so a later revision has a later or equal time,
// unless the clock steps back. Every change before a revision with a
// time before at was made before at, and every change from one at or
// after at was made at or after it. Halving the span of revisions left
// takes a few dozen index seeks however many changes there are. When the
// clock steps back, the revision found may fall short of the true
// boundary or pass it, by the changes made while the clock was behind.
func (s *Store) LastRevisionBefore(
	ctx context.Context, access Access, workspace int64, at time.Time,
) (int64, error) {
	v, err := s.begin(ctx, access, true)
	if err != nil {
		return 0, fmt.Errorf("finding a revision by time: %w", err)
	}
	defer v.tx.Rollback()
	latest, err := revisionSeen(ctx, v.tx)
	if err != nil {
		return 0, err
	}

	// Every change up to low was made before at, and every change from
	// high on at or after it.
	low, high := int64(0), latest+1
	limit := formatTime(at)
	for high-low > 1 {
		middle := low + (high-low)/2
		revision, changed, found, err := firstRankFrom(ctx, v.tx,
			workspace, middle, high)
		if err != nil {
			return 0, err
		}
		switch {
		case !found:
			high = middle
		case changed < limit:
			low = revision
		default:
			high = middle
		}
	}
	if err := v.tx.Commit(); err != nil {
		return 0, fmt.Errorf("finding a revision by time: %w", err)
	}
	return low, nil
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
