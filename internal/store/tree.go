package store

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/airspacesh/workstream/internal/api"
)

const MaxTreeLimit = 500

var ErrInvalidTreeLimit = errors.New("invalid tree limit")

// subtreeCTE names a post and its descendants as name(workspace_id,
// id). It takes the post's workspace id and id as its two arguments. A
// parent is always in its child's workspace, so the subtree is too.
func subtreeCTE(name string) string {
	return name + `(workspace_id, id) AS (
	SELECT ?, ?
	UNION ALL
	SELECT l.source_workspace_id, l.source_id FROM post_links AS l
	JOIN ` + name + ` ON l.target_workspace_id = ` + name + `.workspace_id
		AND l.target_id = ` + name + `.id
	WHERE l.name = 'parent'
)`
}

type treeFacts struct {
	activity    time.Time
	descendants int
}

// readTreeFacts reads the latest activity anywhere in a post's subtree,
// and how many current posts are under it, among the posts the reader
// can read. Superseded and deleted posts count towards activity but not
// towards descendants.
func (v view) readTreeFacts(ctx context.Context, key Key) (treeFacts, error) {
	var facts treeFacts
	var activity string
	err := v.tx.QueryRowContext(ctx, `
		WITH RECURSIVE `+subtreeCTE("subtree")+`
		SELECT max(e.activity_utc),
			coalesce(sum(NOT (e.workspace_id = ? AND e.id = ?)
				AND NOT e.deleted AND NOT `+supersededClause+`), 0)
		FROM subtree JOIN posts AS e
			ON e.workspace_id = subtree.workspace_id
			AND e.id = subtree.id
		WHERE `+visibleClause("e"),
		key.Workspace, key.ID, key.Workspace, key.ID, v.access.user,
		v.access.user,
	).Scan(&activity, &facts.descendants)
	if err != nil {
		return treeFacts{}, fmt.Errorf("reading the subtree of %s: %w",
			v.store.mustRef(key), err)
	}
	if facts.activity, err = parseTime(activity); err != nil {
		return treeFacts{}, err
	}
	return facts, nil
}

// listPost adds a post's ancestors and subtree facts.
func (v view) listPost(
	ctx context.Context, post api.Post,
) (api.ListedPost, error) {
	key := v.store.mustKey(post.Ref)
	path, err := v.ancestors(ctx, key)
	if err != nil {
		return api.ListedPost{}, err
	}
	facts, err := v.readTreeFacts(ctx, key)
	if err != nil {
		return api.ListedPost{}, err
	}
	return api.ListedPost{
		Post:        post,
		Ancestors:   path,
		Activity:    facts.activity,
		Descendants: facts.descendants,
	}, nil
}

// treeNode is one post of a subtree while Tree decides what to show.
type treeNode struct {
	key        Key
	created    string
	superseded bool
	deleted    bool
	children   []*treeNode
	// size counts the node and the visible nodes under it.
	size  int
	shown bool
}

// readSubtree reads the subtree under root that the reader can read,
// each node's children oldest first. A post the reader cannot read has
// only such posts under it, so the subtree stays whole.
func (v view) readSubtree(ctx context.Context, root Key) (*treeNode, error) {
	rows, err := v.tx.QueryContext(ctx, `
		WITH RECURSIVE `+subtreeCTE("subtree")+`
		SELECT e.workspace_id, e.id, e.created_utc, `+supersededClause+`,
			e.deleted, p.target_workspace_id, p.target_id
		FROM subtree JOIN posts AS e
			ON e.workspace_id = subtree.workspace_id
			AND e.id = subtree.id
		LEFT JOIN post_links AS p
			ON p.source_workspace_id = e.workspace_id
			AND p.source_id = e.id AND p.name = 'parent'
		WHERE `+visibleClause("e"),
		root.Workspace, root.ID, v.access.user, v.access.user,
	)
	if err != nil {
		return nil, fmt.Errorf("reading tree: %w", err)
	}
	defer rows.Close()
	nodes := map[Key]*treeNode{}
	parents := map[Key]Key{}
	for rows.Next() {
		node := &treeNode{}
		var parentWorkspace, parentID *int64
		if err := rows.Scan(&node.key.Workspace, &node.key.ID,
			&node.created, &node.superseded, &node.deleted,
			&parentWorkspace, &parentID); err != nil {
			return nil, fmt.Errorf("reading tree: %w", err)
		}
		nodes[node.key] = node
		if node.key != root {
			parents[node.key] = Key{Workspace: *parentWorkspace, ID: *parentID}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading tree: %w", err)
	}

	for key, parent := range parents {
		nodes[parent].children = append(nodes[parent].children, nodes[key])
	}
	for _, node := range nodes {
		sort.Slice(node.children, func(i, j int) bool {
			a, b := node.children[i], node.children[j]
			if a.created != b.created {
				return a.created < b.created
			}
			if a.key.Workspace != b.key.Workspace {
				return a.key.Workspace < b.key.Workspace
			}
			return a.key.ID < b.key.ID
		})
	}
	return nodes[root], nil
}

// liftDeleted replaces each deleted node under node with its children,
// in its place, so the live posts under a deleted post stay in the tree.
func liftDeleted(node *treeNode) {
	var kept []*treeNode
	for _, child := range node.children {
		liftDeleted(child)
		if child.deleted {
			kept = append(kept, child.children...)
		} else {
			kept = append(kept, child)
		}
	}
	node.children = kept
}

// pruneSuperseded drops each superseded node with no current node under
// it, and sets every remaining node's size. It reports whether the node
// holds a current post, itself included.
func pruneSuperseded(node *treeNode) bool {
	node.size = 1
	current := !node.superseded
	var kept []*treeNode
	for _, child := range node.children {
		if pruneSuperseded(child) {
			kept = append(kept, child)
			node.size += child.size
			current = true
		}
	}
	node.children = kept
	return current
}

func countSizes(node *treeNode) {
	node.size = 1
	for _, child := range node.children {
		countSizes(child)
		node.size += child.size
	}
}

// showLevels marks up to limit nodes as shown, a level at a time, so
// every post near the root is shown before any deeper one.
func showLevels(root *treeNode, limit int) {
	root.shown = true
	count := 1
	level := []*treeNode{root}
	for len(level) > 0 {
		var next []*treeNode
		for _, node := range level {
			for _, child := range node.children {
				if count == limit {
					return
				}
				child.shown = true
				count++
				next = append(next, child)
			}
		}
		level = next
	}
}

// shownDepthFirst appends the shown nodes to out, depth first.
func shownDepthFirst(node *treeNode, out []*treeNode) []*treeNode {
	out = append(out, node)
	for _, child := range node.children {
		if child.shown {
			out = shownDepthFirst(child, out)
		}
	}
	return out
}

// treeCuts counts the visible posts under a shown node that are not
// shown, as more below it when none of its children are shown, and
// otherwise as more under it.
func treeCuts(node *treeNode) (moreBelow, moreUnder int) {
	hidden := 0
	anyShown := false
	for _, child := range node.children {
		if child.shown {
			anyShown = true
		} else {
			hidden += child.size
		}
	}
	if anyShown {
		return 0, hidden
	}
	return hidden, 0
}

// Tree returns up to limit posts of a post's subtree, filled level by
// level, with where it was cut. A superseded post is left out unless
// includeSuperseded is set or a current post is under it, which it then
// gives context to. A deleted post is left out unless includeDeleted is
// set, and the posts under it take its place. The requested post is
// always included.
func (s *Store) Tree(
	ctx context.Context,
	access Access,
	key Key,
	includeSuperseded bool,
	includeDeleted bool,
	limit int,
) (api.TreeResponse, error) {
	if limit < 1 || limit > MaxTreeLimit {
		return api.TreeResponse{}, fmt.Errorf(
			"%w: the limit must be between 1 and %d",
			ErrInvalidTreeLimit, MaxTreeLimit)
	}
	v, err := s.begin(ctx, access, true)
	if err != nil {
		return api.TreeResponse{}, fmt.Errorf("reading tree: %w", err)
	}
	defer v.tx.Rollback()

	if err := v.checkPostExists(ctx, key); err != nil {
		return api.TreeResponse{}, err
	}
	root, err := v.readSubtree(ctx, key)
	if err != nil {
		return api.TreeResponse{}, err
	}
	if !includeDeleted {
		liftDeleted(root)
	}
	if includeSuperseded {
		countSizes(root)
	} else {
		pruneSuperseded(root)
	}
	showLevels(root, limit)

	response := api.TreeResponse{}
	for _, node := range shownDepthFirst(root, nil) {
		post, err := v.getPost(ctx, node.key)
		if err != nil {
			return api.TreeResponse{}, err
		}
		listed, err := v.listPost(ctx, post)
		if err != nil {
			return api.TreeResponse{}, err
		}
		moreBelow, moreUnder := treeCuts(node)
		response.Posts = append(response.Posts, api.TreePost{
			ListedPost: listed,
			MoreBelow:  moreBelow,
			MoreUnder:  moreUnder,
		})
	}
	response.Ancestors, err = v.ancestors(ctx, key)
	if err != nil {
		return api.TreeResponse{}, err
	}
	if err := v.tx.Commit(); err != nil {
		return api.TreeResponse{}, fmt.Errorf("finishing tree read: %w", err)
	}
	return response, nil
}
