package store

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/airspacesh/workstream/internal/api"
)

// The topics view lists a workspace's top-level entries by the latest
// activity anywhere in their trees. Working that out for every tree
// costs time in proportion to the workspace, so the view reads
// activity newest first instead, and maps each change to the top-level
// entry above it until it has the page. When few top-level entries
// match, that walk runs long, so after a while the view finds every
// match and ranks their trees directly instead.

const (
	// rowsBeforeRanking and rowsPerEntry bound the rows the walk reads
	// for a page, before ranking directly costs less.
	rowsBeforeRanking = 1 << 16
	rowsPerEntry      = 256
	// firstActivityWindow is how far back the first read of activity
	// reaches. Each later window doubles while windows come back
	// sparse.
	firstActivityWindow = time.Hour
	// activityBatch is the number of changes a window aims to return.
	activityBatch = 256
	// treeMessageCount is how many of the newest messages in its tree
	// the view shows under each top-level entry.
	treeMessageCount = 2
)

// topLevelClause is true for an entry e with no parent.
const topLevelClause = `NOT EXISTS (
	SELECT 1 FROM entry_links AS p
	WHERE p.source_workspace_id = e.workspace_id
		AND p.source_id = e.id AND p.name = 'parent'
)`

// idsJSON encodes entry ids for json_each, so a query can take any
// number of them as one argument.
func idsJSON(ids []int64) string {
	encoded, err := json.Marshal(ids)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

// treeCTE names the trees under the seed entries of a workspace as
// name(workspace_id, entry_id, root_id). It takes the workspace id and
// the seed ids as JSON.
func treeCTE(name string) string {
	return name + `(workspace_id, entry_id, root_id) AS (
	SELECT ?, value, value FROM json_each(?)
	UNION ALL
	SELECT l.source_workspace_id, l.source_id, ` + name + `.root_id
	FROM entry_links AS l JOIN ` + name + `
		ON l.target_workspace_id = ` + name + `.workspace_id
		AND l.target_id = ` + name + `.entry_id
	WHERE l.name = 'parent'
)`
}

// topicsShown lists the entries a where clause selects that the
// topics view shows: those not superseded, and superseded ones with a
// current entry under them, as context.
func (v view) topicsShown(
	ctx context.Context,
	workspace int64,
	from string,
	where whereBuilder,
	includeSuperseded bool,
) ([]int64, error) {
	rows, err := v.tx.QueryContext(ctx, `
		SELECT e.id, `+supersededClause+` FROM `+from+where.sql()+`
		ORDER BY e.id`, where.args...)
	if err != nil {
		return nil, fmt.Errorf("listing top-level entries: %w", err)
	}
	var shown, superseded []int64
	for rows.Next() {
		var id int64
		var isSuperseded bool
		if err := rows.Scan(&id, &isSuperseded); err != nil {
			rows.Close()
			return nil, fmt.Errorf("listing top-level entries: %w", err)
		}
		if isSuperseded && !includeSuperseded {
			superseded = append(superseded, id)
		} else {
			shown = append(shown, id)
		}
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("listing top-level entries: %w", err)
	}
	withCurrent, err := v.currentUnder(ctx, workspace, superseded)
	if err != nil {
		return nil, err
	}
	shown = append(shown, withCurrent...)
	slices.Sort(shown)
	return shown, nil
}

// topicCandidates lists the top-level entries of a workspace that
// match the search and that the view shows.
func (v view) topicCandidates(
	ctx context.Context,
	workspace int64,
	from string,
	where whereBuilder,
	includeSuperseded bool,
) ([]int64, error) {
	where.clauses = append(slices.Clone(where.clauses), topLevelClause)
	return v.topicsShown(ctx, workspace, from, where, includeSuperseded)
}

// shownOf lists which of the given top-level entries of a workspace
// match the search and the view shows.
func (v view) shownOf(
	ctx context.Context,
	workspace int64,
	ids []int64,
	from string,
	where whereBuilder,
	includeSuperseded bool,
) ([]int64, error) {
	where.clauses = append(slices.Clone(where.clauses),
		`e.id IN (SELECT value FROM json_each(?))`)
	where.args = append(slices.Clone(where.args), idsJSON(ids))
	return v.topicsShown(ctx, workspace, from, where, includeSuperseded)
}

// currentUnder lists which of the given entries of a workspace have a
// current entry under them.
func (v view) currentUnder(
	ctx context.Context, workspace int64, ids []int64,
) ([]int64, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := v.tx.QueryContext(ctx, `
		WITH RECURSIVE `+treeCTE("down")+`
		SELECT DISTINCT down.root_id FROM down JOIN entries AS e
			ON e.workspace_id = down.workspace_id AND e.id = down.entry_id
		WHERE NOT (e.workspace_id = ? AND e.id = down.root_id)
			AND NOT `+supersededClause,
		workspace, idsJSON(ids), workspace)
	if err != nil {
		return nil, fmt.Errorf("finding current entries under "+
			"superseded ones: %w", err)
	}
	defer rows.Close()
	var found []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		found = append(found, id)
	}
	return found, rows.Err()
}

// rankTopics orders top-level entries of a workspace by the latest
// activity in their trees, then by id, the newest first.
func (v view) rankTopics(
	ctx context.Context, workspace int64, ids []int64,
) ([]int64, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := v.tx.QueryContext(ctx, `
		WITH RECURSIVE `+treeCTE("tree")+`
		SELECT tree.root_id FROM tree JOIN entries AS e
			ON e.workspace_id = tree.workspace_id AND e.id = tree.entry_id
		GROUP BY tree.root_id
		ORDER BY max(e.activity_utc) DESC, tree.root_id DESC`,
		workspace, idsJSON(ids))
	if err != nil {
		return nil, fmt.Errorf("ranking top-level entries: %w", err)
	}
	defer rows.Close()
	var ranked []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("ranking top-level entries: %w", err)
		}
		ranked = append(ranked, id)
	}
	return ranked, rows.Err()
}

// change is an edit to an entry, or a post or edit in its chat.
type change struct {
	at    string
	entry Key
}

// changesBetween reads the changes in a workspace at or after from
// and, unless to is empty, before to, the newest first, at most limit
// of them.
func (v view) changesBetween(
	ctx context.Context, workspace int64, from, to string, limit int,
) ([]change, error) {
	bound := `workspace_id = ? AND modified_utc >= ?`
	args := []any{workspace, from}
	if to != "" {
		bound += ` AND modified_utc < ?`
		args = append(args, to)
	}
	rows, err := v.tx.QueryContext(ctx, `
		SELECT modified_utc, workspace_id, id FROM entries
		WHERE `+bound+`
		UNION ALL
		SELECT modified_utc, workspace_id, entry_id FROM entry_messages
		WHERE `+bound+`
		ORDER BY 1 DESC LIMIT ?`, append(append(args, args...), limit)...)
	if err != nil {
		return nil, fmt.Errorf("reading recent changes: %w", err)
	}
	defer rows.Close()
	var changes []change
	for rows.Next() {
		var c change
		if err := rows.Scan(&c.at, &c.entry.Workspace,
			&c.entry.ID); err != nil {
			return nil, fmt.Errorf("reading recent changes: %w", err)
		}
		changes = append(changes, c)
	}
	return changes, rows.Err()
}

// changeSpan is when the first and the latest changes in a workspace
// happened. The first entry's creation comes before every change,
// since an entry exists before it is edited or its chat is posted to.
// Both are empty when there are no entries.
func (v view) changeSpan(
	ctx context.Context, workspace int64,
) (string, string, error) {
	var created, modified, posted *string
	if err := v.tx.QueryRowContext(ctx, `
		SELECT (SELECT min(created_utc) FROM entries WHERE workspace_id = ?1),
			(SELECT max(modified_utc) FROM entries WHERE workspace_id = ?1),
			(SELECT max(modified_utc) FROM entry_messages
				WHERE workspace_id = ?1)`, workspace,
	).Scan(&created, &modified, &posted); err != nil {
		return "", "", fmt.Errorf("reading the span of changes: %w", err)
	}
	if created == nil {
		return "", "", nil
	}
	latest := *modified
	if posted != nil && *posted > latest {
		latest = *posted
	}
	return *created, latest, nil
}

// parentsOf reads the parents of the given entries of a workspace,
// for those that have one.
func (v view) parentsOf(
	ctx context.Context, workspace int64, ids []int64,
) (map[int64]int64, error) {
	rows, err := v.tx.QueryContext(ctx, `
		SELECT l.source_id, l.target_workspace_id, l.target_id
		FROM json_each(?) AS child CROSS JOIN entry_links AS l
			ON l.source_workspace_id = ? AND l.source_id = child.value
			AND l.name = 'parent'`, idsJSON(ids), workspace)
	if err != nil {
		return nil, fmt.Errorf("reading parents: %w", err)
	}
	defer rows.Close()
	parents := map[int64]int64{}
	for rows.Next() {
		var child, parentWorkspace, parent int64
		if err := rows.Scan(&child, &parentWorkspace, &parent); err != nil {
			return nil, fmt.Errorf("reading parents: %w", err)
		}
		if parentWorkspace != workspace {
			return nil, fmt.Errorf("the parent of %s is in another workspace",
				v.store.mustRef(Key{workspace, child}))
		}
		parents[child] = parent
	}
	return parents, rows.Err()
}

// topicWalk maps changes, newest first, to the top-level entries whose
// trees hold them. The first change that reaches a top-level entry is
// the latest activity in its tree, so the entries come out in the
// view's order.
type topicWalk struct {
	view      view
	workspace int64
	// shownOf lists which of some top-level entries the view shows.
	shownOf func(ctx context.Context, ids []int64) ([]int64, error)
	// topOf maps each entry whose path up has been followed to the
	// top-level entry above it, itself if it has no parent.
	topOf      map[int64]int64
	considered map[int64]bool
	ranked     []int64
	// rowsLeft bounds the rows the walk may read before ranking
	// directly is the cheaper way.
	rowsLeft int
}

// spend takes rows from the walk's budget, and reports false when the
// budget is spent.
func (w *topicWalk) spend(rows int) bool {
	w.rowsLeft -= rows
	return w.rowsLeft >= 0
}

// followUp finds the top-level entry above each of the given entries.
// It reads the parents of every entry not yet followed in one query,
// then theirs, a level at a time, until each path meets a top-level
// entry or one followed before. It reports false when the walk has run
// out of rows to read.
func (w *topicWalk) followUp(ctx context.Context, ids []int64) (bool, error) {
	queued := map[int64]bool{}
	var level []int64
	for _, id := range ids {
		if _, known := w.topOf[id]; !known && !queued[id] {
			queued[id] = true
			level = append(level, id)
		}
	}
	parentOf := map[int64]int64{}
	var followed []int64
	for len(level) > 0 {
		if !w.spend(len(level)) {
			return false, nil
		}
		parents, err := w.view.parentsOf(ctx, w.workspace, level)
		if err != nil {
			return false, err
		}
		var next []int64
		for _, id := range level {
			followed = append(followed, id)
			parent, ok := parents[id]
			if !ok {
				w.topOf[id] = id
				continue
			}
			parentOf[id] = parent
			if _, known := w.topOf[parent]; !known && !queued[parent] {
				queued[parent] = true
				next = append(next, parent)
			}
		}
		level = next
	}

	for _, id := range followed {
		var path []int64
		at := id
		for {
			if _, known := w.topOf[at]; known {
				break
			}
			path = append(path, at)
			at = parentOf[at]
		}
		for _, node := range path {
			w.topOf[node] = w.topOf[at]
		}
	}
	return true, nil
}

// rank maps a window of changes, newest first, to the top-level
// entries above them, and ranks those the view shows that no newer
// change reached. It reports whether the walk has count entries, and
// false when it has run out of rows to read.
func (w *topicWalk) rank(
	ctx context.Context, changes []change, count int,
) (bool, bool, error) {
	ids := make([]int64, len(changes))
	for i, c := range changes {
		ids[i] = c.entry.ID
	}
	if ok, err := w.followUp(ctx, ids); err != nil || !ok {
		return false, ok, err
	}
	var reached []int64
	for _, id := range ids {
		if top := w.topOf[id]; !w.considered[top] {
			w.considered[top] = true
			reached = append(reached, top)
		}
	}
	if !w.spend(len(reached)) {
		return false, false, nil
	}
	shown, err := w.shownOf(ctx, reached)
	if err != nil {
		return false, false, err
	}

	unranked := map[int64]bool{}
	for _, id := range shown {
		unranked[id] = true
	}
	for start := 0; start < len(changes); {
		end := start
		var sameTime []int64
		for end < len(changes) && changes[end].at == changes[start].at {
			if top := w.topOf[changes[end].entry.ID]; unranked[top] {
				delete(unranked, top)
				sameTime = append(sameTime, top)
			}
			end++
		}
		slices.SortFunc(sameTime, func(a, b int64) int {
			return cmp.Compare(b, a)
		})
		w.ranked = append(w.ranked, sameTime...)
		start = end
		if len(w.ranked) >= count {
			return true, true, nil
		}
	}
	return false, true, nil
}

// walkTopics ranks the first count top-level entries the view shows,
// by reading changes newest first. It reports false when it would
// read more than maxRows rows.
func (v view) walkTopics(
	ctx context.Context,
	workspace int64,
	count int,
	maxRows int,
	shownOf func(ctx context.Context, ids []int64) ([]int64, error),
) ([]int64, bool, error) {
	earliest, latest, err := v.changeSpan(ctx, workspace)
	if err != nil {
		return nil, false, err
	}
	if earliest == "" {
		return nil, true, nil
	}
	floor, err := parseTime(earliest)
	if err != nil {
		return nil, false, err
	}
	newest, err := parseTime(latest)
	if err != nil {
		return nil, false, err
	}
	w := topicWalk{
		view: v, workspace: workspace, shownOf: shownOf,
		topOf: map[int64]int64{}, considered: map[int64]bool{},
		rowsLeft: maxRows,
	}

	to := ""
	window := firstActivityWindow
	from := newest.Add(-window)
	for {
		if from.Before(floor) {
			from = floor
		}
		changes, err := v.changesBetween(ctx, workspace,
			from.Format(timeLayout), to, w.rowsLeft+1)
		if err != nil {
			return nil, false, err
		}
		if !w.spend(len(changes)) {
			return nil, false, nil
		}
		full, ok, err := w.rank(ctx, changes, count)
		if err != nil || !ok {
			return nil, false, err
		}
		if full || !from.After(floor) {
			return w.ranked, true, nil
		}

		to = from.Format(timeLayout)
		if len(changes) < activityBatch {
			window *= 2
		}
		from = from.Add(-window)
	}
}

// treeMessagesFrom joins an entry's subtree, from subtreeCTE, to the
// messages in it. It takes the entry's workspace, which holds the
// whole subtree. Left to choose, SQLite may build an automatic index
// over every message on the server to join a large subtree, which
// takes seconds, so INDEXED BY holds it to the index of each entry's
// chat.
const treeMessagesFrom = `subtree CROSS JOIN entry_messages AS m
	INDEXED BY sqlite_autoindex_entry_messages_1
	ON m.workspace_id = ? AND m.entry_id = subtree.id`

// newestTreeMessages reads the treeMessageCount messages posted last
// anywhere in an entry's tree, oldest first, and counts the tree's
// other messages.
func (v view) newestTreeMessages(
	ctx context.Context, key Key,
) ([]api.Message, int, error) {
	var total int
	if err := v.tx.QueryRowContext(ctx, `
		WITH RECURSIVE `+subtreeCTE("subtree")+`
		SELECT count(*) FROM `+treeMessagesFrom,
		key.Workspace, key.ID, key.Workspace,
	).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("counting the messages under %s: %w",
			v.store.mustRef(key), err)
	}

	rows, err := v.tx.QueryContext(ctx, `
		WITH RECURSIVE `+subtreeCTE("subtree")+`
		SELECT `+messageColumns+` FROM `+treeMessagesFrom+`
		ORDER BY m.post_order DESC LIMIT ?`,
		key.Workspace, key.ID, key.Workspace, treeMessageCount)
	if err != nil {
		return nil, 0, fmt.Errorf("reading the messages under %s: %w",
			v.store.mustRef(key), err)
	}
	defer rows.Close()
	var messages []api.Message
	for rows.Next() {
		message, err := v.store.scanMessage(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("reading the messages under %s: %w",
				v.store.mustRef(key), err)
		}
		messages = append(messages, message)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("reading the messages under %s: %w",
			v.store.mustRef(key), err)
	}
	slices.Reverse(messages)
	return messages, total - len(messages), nil
}

// searchTopics lists a page of the topics view, and whether more
// follow. It walks activity reading at most maxRows rows, then ranks
// every match directly.
func (v view) searchTopics(
	ctx context.Context,
	workspace int64,
	filter Filter,
	from string,
	fromArgs []any,
	where whereBuilder,
	maxRows int,
) (api.SearchResult, error) {
	where.args = append(slices.Clone(fromArgs), where.args...)
	where.clauses = append(slices.Clone(where.clauses), `e.workspace_id = ?`)
	where.args = append(where.args, workspace)

	count := filter.Offset + filter.Limit + 1
	shownOf := func(ctx context.Context, ids []int64) ([]int64, error) {
		return v.shownOf(ctx, workspace, ids, from, where,
			filter.IncludeSuperseded)
	}
	ranked, walked, err := v.walkTopics(ctx, workspace, count, maxRows,
		shownOf)
	if err != nil {
		return api.SearchResult{}, err
	}
	if !walked {
		candidates, err := v.topicCandidates(ctx, workspace, from, where,
			filter.IncludeSuperseded)
		if err != nil {
			return api.SearchResult{}, err
		}
		if ranked, err = v.rankTopics(ctx, workspace, candidates); err != nil {
			return api.SearchResult{}, err
		}
	}

	page := ranked[min(filter.Offset, len(ranked)):min(
		filter.Offset+filter.Limit, len(ranked))]
	listed, err := v.matchedEntries(ctx, workspace, page)
	if err != nil {
		return api.SearchResult{}, err
	}
	for i := range listed {
		key := Key{workspace, listed[i].Ref.ID}
		listed[i].TreeMessages, listed[i].EarlierTreeMessages, err =
			v.newestTreeMessages(ctx, key)
		if err != nil {
			return api.SearchResult{}, err
		}
	}
	return api.SearchResult{
		Entries: listed,
		More:    len(ranked) > filter.Offset+filter.Limit,
	}, nil
}

// matchedEntries reads the entries of a workspace a listing shows, in
// the given order.
func (v view) matchedEntries(
	ctx context.Context, workspace int64, ids []int64,
) ([]api.MatchedEntry, error) {
	entries := make([]api.Entry, len(ids))
	for i, id := range ids {
		var err error
		if entries[i], err = v.getEntry(ctx, Key{workspace, id}); err != nil {
			return nil, err
		}
	}
	listed, err := v.listEntries(ctx, entries)
	if err != nil {
		return nil, err
	}
	matched := make([]api.MatchedEntry, len(listed))
	for i, entry := range listed {
		matched[i] = api.MatchedEntry{ListedEntry: entry}
	}
	return matched, nil
}

// topicWalkRows is how many rows the topics view reads walking
// activity for a page before it ranks every match directly.
func topicWalkRows(filter Filter) int {
	return rowsBeforeRanking + rowsPerEntry*(filter.Offset+filter.Limit+1)
}

// joinedFrom is the FROM clause of a search: every entry, or with
// under, the entries of that subtree.
func joinedFrom(under *Key) (string, []any) {
	if under == nil {
		return `entries AS e`, nil
	}
	return `(WITH RECURSIVE ` + subtreeCTE("under") +
			` SELECT workspace_id, id FROM under) AS under
		JOIN entries AS e
			ON e.workspace_id = under.workspace_id AND e.id = under.id`,
		[]any{under.Workspace, under.ID}
}
