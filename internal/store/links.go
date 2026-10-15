package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/airspacesh/hivemind/internal/api"
)

var (
	ErrLinkTarget   = errors.New("link target not found")
	ErrLinkLoop     = errors.New("link would form a loop")
	ErrLinkNotFound = errors.New("link not found")
	ErrLinkLimit    = errors.New("link limit reached")
	// ErrParentWorkspace refuses a parent in another workspace, so a
	// tree never leaves its workspace.
	ErrParentWorkspace = errors.New("a parent must be in the same workspace")
)

// acyclicLinks are the link names whose chains must never loop: a post
// cannot be its own ancestor, nor supersede itself.
var acyclicLinks = map[string]bool{
	api.ParentLink:     true,
	api.SupersedesLink: true,
}

// supersededClause is true for a post e that another post the reader
// can read supersedes. It takes the reader's user as its one argument.
var supersededClause = `EXISTS (
	SELECT 1 FROM post_links AS superseding CROSS JOIN posts AS successor
		ON successor.workspace_id = superseding.source_workspace_id
		AND successor.id = superseding.source_id
	WHERE superseding.target_workspace_id = e.workspace_id
		AND superseding.target_id = e.id
		AND superseding.name = 'supersedes'
		AND ` + visibleClause("successor") + `
)`

// reaches reports whether following name links out from start arrives
// at goal. Links are walked inside the caller's write transaction, and
// writes are serialised, so no other write can add a link meanwhile.
func (v view) reaches(
	ctx context.Context, start, goal Key, name string,
) (bool, error) {
	var found bool
	err := v.tx.QueryRowContext(ctx, `
		WITH RECURSIVE reach(workspace_id, id) AS (
			SELECT ?, ?
			UNION
			SELECT l.target_workspace_id, l.target_id
			FROM post_links AS l JOIN reach
				ON l.source_workspace_id = reach.workspace_id
				AND l.source_id = reach.id
			WHERE l.name = ?
		)
		SELECT EXISTS (
			SELECT 1 FROM reach WHERE workspace_id = ? AND id = ?
		)`,
		start.Workspace, start.ID, name, goal.Workspace, goal.ID,
	).Scan(&found)
	if err != nil {
		return false, fmt.Errorf("checking for a link loop: %w", err)
	}
	return found, nil
}

// linkTarget finds the post a link from source names, which must be a
// post the request can read. A target it cannot read is refused as if
// it were missing, so a link cannot probe another workspace.
func (v view) linkTarget(
	ctx context.Context, source Key, link api.Link,
) (Key, error) {
	target, err := v.store.Resolve(link.Ref)
	if err != nil {
		return Key{}, err
	}
	if link.Name == api.ParentLink && target.Workspace != source.Workspace {
		return Key{}, fmt.Errorf("%w: %s is in another workspace than %s",
			ErrParentWorkspace, link.Ref, v.store.mustRef(source))
	}
	if !v.access.CanRead(target.Workspace) {
		return Key{}, fmt.Errorf("%w: %s", ErrLinkTarget, link.Ref)
	}
	err = v.checkPostExists(ctx, target)
	if errors.Is(err, ErrPostNotFound) {
		return Key{}, fmt.Errorf("%w: %s", ErrLinkTarget, link.Ref)
	}
	if err != nil {
		return Key{}, err
	}
	return target, nil
}

// addLink links source to the post a link names.
func (v view) addLink(ctx context.Context, source Key, link api.Link) error {
	target, err := v.linkTarget(ctx, source, link)
	if err != nil {
		return err
	}

	if link.Name == api.ParentLink {
		if _, err := v.tx.ExecContext(ctx, `
			DELETE FROM post_links
			WHERE source_workspace_id = ? AND source_id = ? AND name = ?
				AND NOT (target_workspace_id = ? AND target_id = ?)`,
			source.Workspace, source.ID, api.ParentLink,
			target.Workspace, target.ID,
		); err != nil {
			return fmt.Errorf("replacing parent: %w", err)
		}
	}
	if acyclicLinks[link.Name] {
		loops, err := v.reaches(ctx, target, source, link.Name)
		if err != nil {
			return err
		}
		if loops {
			return fmt.Errorf("%w: %s %s %s", ErrLinkLoop,
				v.store.mustRef(source), link.Name, link.Ref)
		}
	}
	if _, err := v.tx.ExecContext(ctx, `
		INSERT INTO post_links (source_workspace_id, source_id, name,
			target_workspace_id, target_id)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT DO NOTHING`,
		source.Workspace, source.ID, link.Name, target.Workspace, target.ID,
	); err != nil {
		return fmt.Errorf("adding link: %w", err)
	}
	return nil
}

func (v view) removeLink(
	ctx context.Context, source Key, link api.Link,
) error {
	target, err := v.store.Resolve(link.Ref)
	if err != nil {
		return err
	}
	result, err := v.tx.ExecContext(ctx, `
		DELETE FROM post_links
		WHERE source_workspace_id = ? AND source_id = ? AND name = ?
			AND target_workspace_id = ? AND target_id = ?`,
		source.Workspace, source.ID, link.Name, target.Workspace, target.ID,
	)
	if err != nil {
		return fmt.Errorf("removing link: %w", err)
	}
	removed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("removing link: %w", err)
	}
	if removed == 0 {
		return fmt.Errorf("%w: %s has no link %s %s", ErrLinkNotFound,
			v.store.mustRef(source), link.Name, link.Ref)
	}
	return nil
}

// writeLinks applies a post's link changes: removals first, then
// additions, then the cap on how many links the post holds.
func (v view) writeLinks(
	ctx context.Context,
	source Key,
	removeNames []string,
	remove []api.Link,
	add []api.Link,
) error {
	for _, name := range removeNames {
		if _, err := v.tx.ExecContext(ctx, `
			DELETE FROM post_links
			WHERE source_workspace_id = ? AND source_id = ? AND name = ?`,
			source.Workspace, source.ID, name,
		); err != nil {
			return fmt.Errorf("removing %s links: %w", name, err)
		}
	}
	for _, link := range remove {
		if err := v.removeLink(ctx, source, link); err != nil {
			return err
		}
	}
	for _, link := range add {
		if err := v.addLink(ctx, source, link); err != nil {
			return err
		}
	}
	if len(add) == 0 {
		return nil
	}

	var count int
	if err := v.tx.QueryRowContext(ctx, `
		SELECT count(*) FROM post_links
		WHERE source_workspace_id = ? AND source_id = ?`,
		source.Workspace, source.ID,
	).Scan(&count); err != nil {
		return fmt.Errorf("counting links: %w", err)
	}
	if count > api.MaxLinks {
		return fmt.Errorf("%w: %s would hold %d links (max %d)",
			ErrLinkLimit, v.store.mustRef(source), count, api.MaxLinks)
	}
	return nil
}

// unreadable finds which of the given posts the reader cannot read. It
// reads nothing when there are none to check.
func (v view) unreadable(
	ctx context.Context, keys []Key,
) (map[Key]bool, error) {
	hidden := map[Key]bool{}
	if len(keys) == 0 {
		return hidden, nil
	}
	packed := make([]int64, len(keys))
	for i, key := range keys {
		packed[i] = key.Workspace<<32 | key.ID
	}
	rows, err := v.tx.QueryContext(ctx, `
		SELECT e.workspace_id, e.id
		FROM json_each(?) AS checked CROSS JOIN posts AS e
			ON e.workspace_id = checked.value >> 32
			AND e.id = checked.value & 0xffffffff
		WHERE NOT `+visibleClause("e"), idsJSON(packed), v.access.user)
	if err != nil {
		return nil, fmt.Errorf("checking linked posts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var key Key
		if err := rows.Scan(&key.Workspace, &key.ID); err != nil {
			return nil, fmt.Errorf("checking linked posts: %w", err)
		}
		hidden[key] = true
	}
	return hidden, rows.Err()
}

// readLinks reads (name, workspace, id) rows into links, leaving out
// posts in workspaces the config no longer names, which have no ref,
// and posts the reader cannot read. The query reads links from a post
// the reader can read when fromReadable is set. That post's parent is
// readable too, since everything under a private post is private to
// the same user, so the usual post, whose one link is to its parent,
// needs no check. Joining the posts in the query instead would cost
// every read the time SQLite takes to plan the join, which this
// avoids for the many small reads of a listing.
func (v view) readLinks(
	ctx context.Context, fromReadable bool, query string, args ...any,
) ([]api.Link, error) {
	rows, err := v.tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type linkRow struct {
		name string
		key  Key
	}
	var read []linkRow
	var unchecked []Key
	for rows.Next() {
		var row linkRow
		if err := rows.Scan(&row.name, &row.key.Workspace,
			&row.key.ID); err != nil {
			return nil, err
		}
		read = append(read, row)
		if !(fromReadable && row.name == api.ParentLink) {
			unchecked = append(unchecked, row.key)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	hidden, err := v.unreadable(ctx, unchecked)
	if err != nil {
		return nil, err
	}

	var links []api.Link
	for _, row := range read {
		ref, ok := v.store.ref(row.key)
		if ok && !hidden[row.key] {
			links = append(links, api.Link{Name: row.name, Ref: ref})
		}
	}
	return links, nil
}

func (v view) linksFrom(ctx context.Context, key Key) ([]api.Link, error) {
	links, err := v.readLinks(ctx, true, `
		SELECT name, target_workspace_id, target_id FROM post_links
		WHERE source_workspace_id = ? AND source_id = ?
		ORDER BY name, target_workspace_id, target_id`,
		key.Workspace, key.ID,
	)
	if err != nil {
		return nil, fmt.Errorf("reading links: %w", err)
	}
	return links, nil
}

func (v view) linksTo(ctx context.Context, key Key) ([]api.Link, error) {
	links, err := v.readLinks(ctx, false, `
		SELECT name, source_workspace_id, source_id FROM post_links
		WHERE target_workspace_id = ? AND target_id = ?
		ORDER BY name, source_workspace_id, source_id`,
		key.Workspace, key.ID,
	)
	if err != nil {
		return nil, fmt.Errorf("reading backlinks: %w", err)
	}
	return links, nil
}

func (v view) supersedersOf(
	ctx context.Context, key Key,
) ([]api.Ref, error) {
	// The name is literal because SQLite compiles a statement again on
	// every run when a bound value decides whether a partial index, here
	// post_links_one_parent, can serve it.
	links, err := v.readLinks(ctx, false, `
		SELECT name, source_workspace_id, source_id FROM post_links
		WHERE target_workspace_id = ? AND target_id = ?
			AND name = 'supersedes'
		ORDER BY source_workspace_id, source_id`,
		key.Workspace, key.ID,
	)
	if err != nil {
		return nil, fmt.Errorf("reading superseding posts: %w", err)
	}
	var refs []api.Ref
	for _, link := range links {
		refs = append(refs, link.Ref)
	}
	return refs, nil
}

// postRef describes a post by its ref, and by its subject, whether it
// is deleted, who alone can read it, and its superseders when the
// request can read its workspace. Every post the request meets a ref
// to, an ancestor of a post it can read or the post of a message it
// can read, is one the reader can read too.
func (v view) postRef(ctx context.Context, key Key) (api.PostRef, error) {
	described := api.PostRef{Ref: v.store.mustRef(key)}
	if !v.access.CanRead(key.Workspace) {
		return described, nil
	}
	if err := v.tx.QueryRowContext(ctx, `
		SELECT subject, deleted, private_to FROM posts
		WHERE workspace_id = ? AND id = ?`, key.Workspace, key.ID,
	).Scan(&described.Subject, &described.Deleted,
		&described.PrivateTo); err != nil {
		return api.PostRef{}, fmt.Errorf("reading %s: %w",
			described.Ref, err)
	}
	var err error
	described.SupersededBy, err = v.supersedersOf(ctx, key)
	return described, err
}

// pathUp lists a post's ancestors, its parent first.
func (v view) pathUp(ctx context.Context, key Key) ([]Key, error) {
	rows, err := v.tx.QueryContext(ctx, `
		WITH RECURSIVE up(workspace_id, id, depth) AS (
			SELECT target_workspace_id, target_id, 1 FROM post_links
			WHERE source_workspace_id = ? AND source_id = ?
				AND name = 'parent'
			UNION ALL
			SELECT l.target_workspace_id, l.target_id, up.depth + 1
			FROM post_links AS l JOIN up
				ON l.source_workspace_id = up.workspace_id
				AND l.source_id = up.id
			WHERE l.name = 'parent'
		)
		SELECT workspace_id, id FROM up ORDER BY depth`,
		key.Workspace, key.ID)
	if err != nil {
		return nil, fmt.Errorf("reading ancestors: %w", err)
	}
	defer rows.Close()
	var path []Key
	for rows.Next() {
		var ancestor Key
		if err := rows.Scan(&ancestor.Workspace, &ancestor.ID); err != nil {
			return nil, err
		}
		path = append(path, ancestor)
	}
	return path, rows.Err()
}

// ancestors lists a post's ancestors, from the root down to its parent.
func (v view) ancestors(
	ctx context.Context, key Key,
) ([]api.PostRef, error) {
	upward, err := v.pathUp(ctx, key)
	if err != nil {
		return nil, err
	}

	path := make([]api.PostRef, len(upward))
	for i, ancestor := range upward {
		described, err := v.postRef(ctx, ancestor)
		if err != nil {
			return nil, err
		}
		path[len(upward)-1-i] = described
	}
	return path, nil
}

// Detail returns a post with its place in its tree, the links that
// point at it, its child count, and a summary of its chat.
func (s *Store) Detail(
	ctx context.Context, access Access, key Key,
) (api.PostDetail, error) {
	v, err := s.begin(ctx, access, true)
	if err != nil {
		return api.PostDetail{}, fmt.Errorf("getting post: %w", err)
	}
	defer v.tx.Rollback()

	post, err := v.getPost(ctx, key)
	if err != nil {
		return api.PostDetail{}, err
	}
	listed, err := v.listPost(ctx, post)
	if err != nil {
		return api.PostDetail{}, err
	}
	detail := api.PostDetail{ListedPost: listed}
	if detail.Backlinks, err = v.linksTo(ctx, key); err != nil {
		return api.PostDetail{}, err
	}
	if err := v.tx.QueryRowContext(ctx, `
		SELECT count(*) FROM post_links AS l JOIN posts AS e
			ON e.workspace_id = l.source_workspace_id AND e.id = l.source_id
		WHERE l.target_workspace_id = ? AND l.target_id = ?
			AND l.name = 'parent' AND NOT e.deleted
			AND NOT `+supersededClause+` AND `+visibleClause("e"),
		key.Workspace, key.ID, v.access.user, v.access.user,
	).Scan(&detail.Children); err != nil {
		return api.PostDetail{}, fmt.Errorf("counting children: %w", err)
	}
	if err := v.tx.QueryRowContext(ctx, `
		SELECT count(*), coalesce(max(id), 0) FROM post_messages
		WHERE workspace_id = ? AND post_id = ? AND NOT deleted`,
		key.Workspace, key.ID,
	).Scan(&detail.Messages, &detail.LastMessage); err != nil {
		return api.PostDetail{}, fmt.Errorf("reading chat summary: %w", err)
	}
	if err := v.tx.Commit(); err != nil {
		return api.PostDetail{}, fmt.Errorf("finishing post read: %w", err)
	}
	return detail, nil
}
