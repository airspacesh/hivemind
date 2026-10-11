package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/airspacesh/workstream/internal/api"
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

// acyclicLinks are the link names whose chains must never loop: an
// entry cannot be its own ancestor, nor supersede itself.
var acyclicLinks = map[string]bool{
	api.ParentLink:     true,
	api.SupersedesLink: true,
}

// supersededClause is true for an entry e that another entry
// supersedes.
const supersededClause = `EXISTS (
	SELECT 1 FROM entry_links AS superseding
	WHERE superseding.target_workspace_id = e.workspace_id
		AND superseding.target_id = e.id
		AND superseding.name = 'supersedes'
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
			FROM entry_links AS l JOIN reach
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

// addLink links source to an entry the request can read. A target it
// cannot read is refused as if it were missing, so a link cannot probe
// another workspace.
func (v view) addLink(ctx context.Context, source Key, link api.Link) error {
	target, err := v.store.Resolve(link.Ref)
	if err != nil {
		return err
	}
	if link.Name == api.ParentLink && target.Workspace != source.Workspace {
		return fmt.Errorf("%w: %s is in another workspace than %s",
			ErrParentWorkspace, link.Ref, v.store.mustRef(source))
	}
	if !v.access.CanRead(target.Workspace) {
		return fmt.Errorf("%w: %s", ErrLinkTarget, link.Ref)
	}
	err = v.checkEntryExists(ctx, target)
	if errors.Is(err, ErrEntryNotFound) {
		return fmt.Errorf("%w: %s", ErrLinkTarget, link.Ref)
	}
	if err != nil {
		return err
	}

	if link.Name == api.ParentLink {
		if _, err := v.tx.ExecContext(ctx, `
			DELETE FROM entry_links
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
		INSERT INTO entry_links (source_workspace_id, source_id, name,
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
		DELETE FROM entry_links
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

// writeLinks applies an entry's link changes: removals first, then
// additions, then the cap on how many links the entry holds.
func (v view) writeLinks(
	ctx context.Context,
	source Key,
	removeNames []string,
	remove []api.Link,
	add []api.Link,
) error {
	for _, name := range removeNames {
		if _, err := v.tx.ExecContext(ctx, `
			DELETE FROM entry_links
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
		SELECT count(*) FROM entry_links
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

// readLinks reads (name, workspace, id) rows into links, leaving out
// entries in workspaces the config no longer names, which have no ref.
func (v view) readLinks(
	ctx context.Context, query string, args ...any,
) ([]api.Link, error) {
	rows, err := v.tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var links []api.Link
	for rows.Next() {
		var name string
		var key Key
		if err := rows.Scan(&name, &key.Workspace, &key.ID); err != nil {
			return nil, err
		}
		if ref, ok := v.store.ref(key); ok {
			links = append(links, api.Link{Name: name, Ref: ref})
		}
	}
	return links, rows.Err()
}

func (v view) linksFrom(ctx context.Context, key Key) ([]api.Link, error) {
	links, err := v.readLinks(ctx, `
		SELECT name, target_workspace_id, target_id FROM entry_links
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
	links, err := v.readLinks(ctx, `
		SELECT name, source_workspace_id, source_id FROM entry_links
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
	links, err := v.readLinks(ctx, `
		SELECT name, source_workspace_id, source_id FROM entry_links
		WHERE target_workspace_id = ? AND target_id = ? AND name = ?
		ORDER BY source_workspace_id, source_id`,
		key.Workspace, key.ID, api.SupersedesLink,
	)
	if err != nil {
		return nil, fmt.Errorf("reading superseding entries: %w", err)
	}
	var refs []api.Ref
	for _, link := range links {
		refs = append(refs, link.Ref)
	}
	return refs, nil
}

// attachLinks fills in each entry's outgoing links and the entries that
// supersede it.
func (v view) attachLinks(ctx context.Context, entries []api.Post) error {
	for i := range entries {
		key := v.store.mustKey(entries[i].Ref)
		links, err := v.linksFrom(ctx, key)
		if err != nil {
			return err
		}
		supersededBy, err := v.supersedersOf(ctx, key)
		if err != nil {
			return err
		}
		entries[i].Links = links
		entries[i].SupersededBy = supersededBy
	}
	return nil
}

// entryRef describes an entry by its ref, and by its subject, whether
// it is deleted, and its superseders when the request can read it.
func (v view) entryRef(ctx context.Context, key Key) (api.PostRef, error) {
	described := api.PostRef{Ref: v.store.mustRef(key)}
	if !v.access.CanRead(key.Workspace) {
		return described, nil
	}
	if err := v.tx.QueryRowContext(ctx, `
		SELECT subject, deleted FROM entries
		WHERE workspace_id = ? AND id = ?`, key.Workspace, key.ID,
	).Scan(&described.Subject, &described.Deleted); err != nil {
		return api.PostRef{}, fmt.Errorf("reading %s: %w",
			described.Ref, err)
	}
	var err error
	described.SupersededBy, err = v.supersedersOf(ctx, key)
	return described, err
}

// pathUp lists an entry's ancestors, its parent first.
func (v view) pathUp(ctx context.Context, key Key) ([]Key, error) {
	rows, err := v.tx.QueryContext(ctx, `
		WITH RECURSIVE up(workspace_id, id, depth) AS (
			SELECT target_workspace_id, target_id, 1 FROM entry_links
			WHERE source_workspace_id = ? AND source_id = ?
				AND name = 'parent'
			UNION ALL
			SELECT l.target_workspace_id, l.target_id, up.depth + 1
			FROM entry_links AS l JOIN up
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

// ancestors lists an entry's ancestors, from the root down to its
// parent.
func (v view) ancestors(
	ctx context.Context, key Key,
) ([]api.PostRef, error) {
	upward, err := v.pathUp(ctx, key)
	if err != nil {
		return nil, err
	}

	path := make([]api.PostRef, len(upward))
	for i, ancestor := range upward {
		described, err := v.entryRef(ctx, ancestor)
		if err != nil {
			return nil, err
		}
		path[len(upward)-1-i] = described
	}
	return path, nil
}

// Detail returns an entry with its place in its tree, the links that
// point at it, its child count, and a summary of its chat.
func (s *Store) Detail(
	ctx context.Context, access Access, key Key,
) (api.PostDetail, error) {
	v, err := s.begin(ctx, access, true)
	if err != nil {
		return api.PostDetail{}, fmt.Errorf("getting entry: %w", err)
	}
	defer v.tx.Rollback()

	entry, err := v.getEntry(ctx, key)
	if err != nil {
		return api.PostDetail{}, err
	}
	listed, err := v.listEntries(ctx, []api.Post{entry})
	if err != nil {
		return api.PostDetail{}, err
	}
	detail := api.PostDetail{ListedPost: listed[0]}
	if detail.Backlinks, err = v.linksTo(ctx, key); err != nil {
		return api.PostDetail{}, err
	}
	if err := v.tx.QueryRowContext(ctx, `
		SELECT count(*) FROM entry_links AS l JOIN entries AS e
			ON e.workspace_id = l.source_workspace_id AND e.id = l.source_id
		WHERE l.target_workspace_id = ? AND l.target_id = ?
			AND l.name = 'parent' AND NOT `+supersededClause,
		key.Workspace, key.ID,
	).Scan(&detail.Children); err != nil {
		return api.PostDetail{}, fmt.Errorf("counting children: %w", err)
	}
	if err := v.tx.QueryRowContext(ctx, `
		SELECT count(*), coalesce(max(id), 0) FROM entry_messages
		WHERE workspace_id = ? AND entry_id = ?`, key.Workspace, key.ID,
	).Scan(&detail.Messages, &detail.LastMessage); err != nil {
		return api.PostDetail{}, fmt.Errorf("reading chat summary: %w", err)
	}
	if err := v.tx.Commit(); err != nil {
		return api.PostDetail{}, fmt.Errorf("finishing entry read: %w", err)
	}
	return detail, nil
}
