package store

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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
	// walksBeforeRanking and walksPerEntry bound the paths up the walk
	// follows for a page, before ranking directly costs less.
	walksBeforeRanking = 1024
	walksPerEntry      = 32
	// firstActivityWindow is how far back the first read of activity
	// reaches. Each later window doubles while windows come back
	// sparse.
	firstActivityWindow = time.Hour
	// activityBatch is the number of changes a window aims to return.
	activityBatch = 256
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

// topicCandidates lists the top-level entries of a workspace that
// match the search, and that the view shows: those not superseded,
// and superseded ones with a current entry under them, as context.
func (v view) topicCandidates(
	ctx context.Context,
	workspace int64,
	from string,
	where whereBuilder,
	includeSuperseded bool,
) ([]int64, error) {
	where.clauses = append(where.clauses, topLevelClause)
	rows, err := v.tx.QueryContext(ctx, `
		SELECT e.id, `+supersededClause+` FROM `+from+where.sql()+`
		ORDER BY e.id`, where.args...)
	if err != nil {
		return nil, fmt.Errorf("listing top-level entries: %w", err)
	}
	var candidates, superseded []int64
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
			candidates = append(candidates, id)
		}
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("listing top-level entries: %w", err)
	}
	withCurrent, err := v.currentUnder(ctx, workspace, superseded)
	if err != nil {
		return nil, err
	}
	candidates = append(candidates, withCurrent...)
	slices.Sort(candidates)
	return candidates, nil
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

// topicShown reports whether the view shows a top-level entry: whether
// it matches the search, and is current or has a current entry under
// it.
func (v view) topicShown(
	ctx context.Context,
	workspace int64,
	id int64,
	from string,
	where whereBuilder,
	includeSuperseded bool,
) (bool, error) {
	where.clauses = append(slices.Clone(where.clauses), `e.id = ?`)
	where.args = append(slices.Clone(where.args), id)
	var superseded bool
	err := v.tx.QueryRowContext(ctx, `
		SELECT `+supersededClause+` FROM `+from+where.sql(), where.args...,
	).Scan(&superseded)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("matching a top-level entry: %w", err)
	}
	if !superseded || includeSuperseded {
		return true, nil
	}
	withCurrent, err := v.currentUnder(ctx, workspace, []int64{id})
	return len(withCurrent) > 0, err
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
		ORDER BY max(`+activityExpr+`) DESC, tree.root_id DESC`,
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
// and, unless to is empty, before to, the newest first.
func (v view) changesBetween(
	ctx context.Context, workspace int64, from, to string,
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
		ORDER BY 1 DESC`, append(args, args...)...)
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

// topicWalk maps changes, newest first, to the top-level entries whose
// trees hold them. The first change that reaches a top-level entry is
// the latest activity in its tree, so the entries come out in the
// view's order.
type topicWalk struct {
	view view
	// shows reports whether the view shows a top-level entry.
	shows func(ctx context.Context, id int64) (bool, error)
	// walked holds every entry whose path up has been followed. A
	// later, older change to it, or a path up that meets it, reaches a
	// top-level entry already considered.
	walked     map[Key]bool
	considered map[int64]bool
	ranked     []int64
	// walksLeft bounds the paths up the walk may follow before ranking
	// directly is the cheaper way.
	walksLeft int
}

// reach follows a changed entry's path up to its top-level entry, and
// adds that to sameTime if the view shows it and it was not considered
// before. It reports false when the walk has run out of paths to
// follow.
func (w *topicWalk) reach(
	ctx context.Context, entry Key, sameTime *[]int64,
) (bool, error) {
	if w.walked[entry] {
		return true, nil
	}
	if w.walksLeft == 0 {
		return false, nil
	}
	w.walksLeft--
	ancestors, err := w.view.pathUp(ctx, entry)
	if err != nil {
		return false, err
	}

	path := append([]Key{entry}, ancestors...)
	for _, node := range path {
		if w.walked[node] {
			return true, nil
		}
		w.walked[node] = true
	}
	top := path[len(path)-1].ID
	if w.considered[top] {
		return true, nil
	}
	w.considered[top] = true
	shown, err := w.shows(ctx, top)
	if err != nil {
		return false, err
	}
	if shown {
		*sameTime = append(*sameTime, top)
	}
	return true, nil
}

// walkTopics ranks the first count top-level entries the view shows,
// by reading changes newest first. It reports false when it would
// follow more than maxWalks paths up.
func (v view) walkTopics(
	ctx context.Context,
	workspace int64,
	count int,
	maxWalks int,
	shows func(ctx context.Context, id int64) (bool, error),
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
		view: v, shows: shows, walked: map[Key]bool{},
		considered: map[int64]bool{}, walksLeft: maxWalks,
	}

	to := ""
	window := firstActivityWindow
	from := newest.Add(-window)
	for {
		if from.Before(floor) {
			from = floor
		}
		changes, err := v.changesBetween(ctx, workspace,
			from.Format(timeLayout), to)
		if err != nil {
			return nil, false, err
		}
		for start := 0; start < len(changes); {
			end := start
			var sameTime []int64
			for end < len(changes) && changes[end].at == changes[start].at {
				ok, err := w.reach(ctx, changes[end].entry, &sameTime)
				if err != nil || !ok {
					return nil, false, err
				}
				end++
			}
			slices.SortFunc(sameTime, func(a, b int64) int {
				return cmp.Compare(b, a)
			})
			w.ranked = append(w.ranked, sameTime...)
			start = end
			if len(w.ranked) >= count {
				return w.ranked, true, nil
			}
		}
		if !from.After(floor) {
			return w.ranked, true, nil
		}

		to = from.Format(timeLayout)
		if len(changes) < activityBatch {
			window *= 2
		}
		from = from.Add(-window)
	}
}

// searchTopics lists a page of the topics view, and whether more
// follow. It walks activity for at most maxWalks paths up, then ranks
// every match directly.
func (v view) searchTopics(
	ctx context.Context,
	workspace int64,
	filter Filter,
	from string,
	where whereBuilder,
	maxWalks int,
) (api.SearchResult, error) {
	count := filter.Offset + filter.Limit + 1
	shows := func(ctx context.Context, id int64) (bool, error) {
		return v.topicShown(ctx, workspace, id, from, where,
			filter.IncludeSuperseded)
	}
	ranked, walked, err := v.walkTopics(ctx, workspace, count, maxWalks,
		shows)
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

	result := api.SearchResult{
		Entries: []api.ListedEntry{},
		More:    len(ranked) > filter.Offset+filter.Limit,
	}
	if filter.Offset >= len(ranked) {
		return result, nil
	}
	page := ranked[filter.Offset:min(filter.Offset+filter.Limit, len(ranked))]
	entries := make([]api.Entry, len(page))
	for i, id := range page {
		if entries[i], err = v.getEntry(ctx, Key{workspace, id}); err != nil {
			return api.SearchResult{}, err
		}
	}
	if result.Entries, err = v.listEntries(ctx, entries); err != nil {
		return api.SearchResult{}, err
	}
	return result, nil
}

// topicWalks is how many paths up the topics view follows for a page
// before it ranks every match directly.
func topicWalks(filter Filter) int {
	return walksBeforeRanking +
		walksPerEntry*(filter.Offset+filter.Limit+1)
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
