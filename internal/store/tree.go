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

// subtreeCTE names an entry and its descendants as
// name(workspace_id, id). It takes the entry's workspace id and id as
// its two arguments. A parent is always in its child's workspace, so
// the subtree is too.
func subtreeCTE(name string) string {
	return name + `(workspace_id, id) AS (
	SELECT ?, ?
	UNION ALL
	SELECT l.source_workspace_id, l.source_id FROM entry_links AS l
	JOIN ` + name + ` ON l.target_workspace_id = ` + name + `.workspace_id
		AND l.target_id = ` + name + `.id
	WHERE l.name = 'parent'
)`
}

type treeFacts struct {
	activity    time.Time
	descendants int
}

// readTreeFacts reads the latest activity anywhere in an entry's
// subtree, and how many current entries are under it. Superseded
// entries count towards activity but not towards descendants.
func (v view) readTreeFacts(ctx context.Context, key Key) (treeFacts, error) {
	var facts treeFacts
	var activity string
	err := v.tx.QueryRowContext(ctx, `
		WITH RECURSIVE `+subtreeCTE("subtree")+`
		SELECT max(e.activity_utc),
			coalesce(sum(NOT (e.workspace_id = ? AND e.id = ?)
				AND NOT `+supersededClause+`), 0)
		FROM subtree JOIN entries AS e
			ON e.workspace_id = subtree.workspace_id
			AND e.id = subtree.id`,
		key.Workspace, key.ID, key.Workspace, key.ID,
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

// listEntries adds each entry's ancestors and subtree facts.
func (v view) listEntries(
	ctx context.Context, entries []api.Post,
) ([]api.ListedPost, error) {
	listed := make([]api.ListedPost, len(entries))
	for i, entry := range entries {
		key := v.store.mustKey(entry.Ref)
		path, err := v.ancestors(ctx, key)
		if err != nil {
			return nil, err
		}
		facts, err := v.readTreeFacts(ctx, key)
		if err != nil {
			return nil, err
		}
		listed[i] = api.ListedPost{
			Post:        entry,
			Ancestors:   path,
			Activity:    facts.activity,
			Descendants: facts.descendants,
		}
	}
	return listed, nil
}

// treeNode is one entry of a subtree while Tree decides what to show.
type treeNode struct {
	key        Key
	created    string
	superseded bool
	children   []*treeNode
	// size counts the node and the visible nodes under it.
	size  int
	shown bool
}

// readSubtree reads the subtree under root, each node's
// children oldest first.
func (v view) readSubtree(ctx context.Context, root Key) (*treeNode, error) {
	rows, err := v.tx.QueryContext(ctx, `
		WITH RECURSIVE `+subtreeCTE("subtree")+`
		SELECT e.workspace_id, e.id, e.created_utc, `+supersededClause+`,
			p.target_workspace_id, p.target_id
		FROM subtree JOIN entries AS e
			ON e.workspace_id = subtree.workspace_id
			AND e.id = subtree.id
		LEFT JOIN entry_links AS p
			ON p.source_workspace_id = e.workspace_id
			AND p.source_id = e.id AND p.name = 'parent'`,
		root.Workspace, root.ID,
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
			&node.created, &node.superseded,
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

// pruneSuperseded drops each superseded node with no current node
// under it, and sets every remaining node's size. It reports whether
// the node holds a current entry, itself included.
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
// every entry near the root is shown before any deeper one.
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

// treeCuts counts the visible entries under a shown node that are not
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

// Tree returns up to limit entries of an entry's subtree, filled level
// by level, with where it was cut. A superseded entry is left out
// unless includeSuperseded is set or a current entry is under it,
// which it then gives context to. The requested entry is always
// included.
func (s *Store) Tree(
	ctx context.Context,
	access Access,
	key Key,
	includeSuperseded bool,
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

	if err := v.checkEntryExists(ctx, key); err != nil {
		return api.TreeResponse{}, err
	}
	root, err := v.readSubtree(ctx, key)
	if err != nil {
		return api.TreeResponse{}, err
	}
	if includeSuperseded {
		countSizes(root)
	} else {
		pruneSuperseded(root)
	}
	showLevels(root, limit)

	response := api.TreeResponse{}
	for _, node := range shownDepthFirst(root, nil) {
		entry, err := v.getEntry(ctx, node.key)
		if err != nil {
			return api.TreeResponse{}, err
		}
		listed, err := v.listEntries(ctx, []api.Post{entry})
		if err != nil {
			return api.TreeResponse{}, err
		}
		moreBelow, moreUnder := treeCuts(node)
		response.Posts = append(response.Posts, api.TreePost{
			ListedPost: listed[0],
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
