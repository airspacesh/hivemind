package store

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"reflect"
	"testing"
	"time"

	"github.com/airspacesh/workstream/internal/api"
)

// activityExpr is when entry e last changed by definition: an edit to
// it, or a post or edit in its chat. activity_utc must always equal it.
const activityExpr = `max(e.modified_utc, coalesce((
	SELECT max(m.modified_utc) FROM entry_messages AS m
	WHERE m.workspace_id = e.workspace_id AND m.entry_id = e.id), ''))`

// spanActivityExpr is when entry e last changed within the span from
// ?1 to ?2, which is the latest of its own change and the changes in
// its chat that fall within it, or empty when none do.
const spanActivityExpr = `max(
	coalesce(CASE WHEN e.modified_utc >= ?1 AND e.modified_utc < ?2
		THEN e.modified_utc END, ''),
	coalesce((
		SELECT max(m.modified_utc) FROM entry_messages AS m
		WHERE m.workspace_id = e.workspace_id AND m.entry_id = e.id
			AND m.modified_utc >= ?1 AND m.modified_utc < ?2), ''))`

// referenceTopics is the topics view as one query that works out the
// activity of every tree in the workspace within a span. It is too slow
// for a large workspace, and serves here as the definition the view
// must match.
func referenceTopics(
	t *testing.T,
	store *Store,
	workspace int64,
	project string,
	includeSuperseded bool,
	span Span,
) []int64 {
	t.Helper()
	visible := `(NOT ` + supersededClause + ` OR roots.current_under)`
	if includeSuperseded {
		visible = `1`
	}
	after, before := "", "~"
	if !span.After.IsZero() {
		after = formatTime(span.After)
	}
	if !span.Before.IsZero() {
		before = formatTime(span.Before)
	}
	rows, err := store.db.Query(`
		WITH RECURSIVE tree(workspace_id, entry_id, root_id) AS (
			SELECT e.workspace_id, e.id, e.id FROM entries AS e
			WHERE e.workspace_id = ?3 AND `+topLevelClause+`
			UNION ALL
			SELECT l.source_workspace_id, l.source_id, tree.root_id
			FROM entry_links AS l JOIN tree
				ON l.target_workspace_id = tree.workspace_id
				AND l.target_id = tree.entry_id
			WHERE l.name = 'parent'
		),
		roots(id, activity_utc, current_under) AS (
			SELECT tree.root_id, max(`+spanActivityExpr+`),
				coalesce(max(NOT (e.workspace_id = ?3 AND e.id = tree.root_id)
					AND NOT `+supersededClause+`), 0)
			FROM tree JOIN entries AS e
				ON e.workspace_id = tree.workspace_id
				AND e.id = tree.entry_id
			GROUP BY tree.root_id
		)
		SELECT e.id FROM entries AS e JOIN roots ON roots.id = e.id
		WHERE e.workspace_id = ?3 AND `+visible+`
			AND (?4 = '' OR e.project = ?4) AND roots.activity_utc != ''
		ORDER BY roots.activity_utc DESC, e.id DESC`,
		after, before, workspace, project)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return ids
}

// addRandomForest adds entries to both workspaces with random parents
// in the same workspace, supersessions across them, messages and
// edits, then spreads their times over a year, and sets each entry's
// activity to match. When onDays is set, every time falls on midnight,
// so many trees share their latest activity.
func addRandomForest(
	t *testing.T, store *Store, random *rand.Rand, onDays bool,
) {
	t.Helper()
	ctx := context.Background()
	var refs []api.Ref
	inWorkspace := map[string][]api.Ref{}
	for i := 0; i < 80; i++ {
		workspace := "default"
		if random.Intn(3) == 0 {
			workspace = "personal"
		}
		var links []api.Link
		if parents := inWorkspace[workspace]; len(parents) > 0 &&
			random.Intn(10) < 6 {
			links = append(links, link(api.ParentLink,
				parents[random.Intn(len(parents))]))
		}
		if len(refs) > 0 && random.Intn(7) == 0 {
			links = append(links, link(api.SupersedesLink,
				refs[random.Intn(len(refs))]))
		}
		entry := addTo(t, store, workspace, api.AddEntryRequest{
			Project: []string{"a", "b"}[random.Intn(2)],
			Subject: fmt.Sprintf("entry %d", i), Links: links,
		})
		refs = append(refs, entry.Ref)
		inWorkspace[workspace] = append(inWorkspace[workspace], entry.Ref)
	}
	for i := 0; i < 60; i++ {
		mustPost(t, store, refs[random.Intn(len(refs))], "hello")
	}

	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	at := func(offset time.Duration) string {
		moment := start.Add(offset)
		if onDays {
			moment = moment.Truncate(24 * time.Hour)
		}
		return moment.Format(timeLayout)
	}
	year := int64(365 * 24 * time.Hour)
	created := map[api.Ref]time.Duration{}
	for i, ref := range refs {
		key := store.mustKey(ref)
		offset := time.Duration(int64(i) * year / int64(len(refs)))
		created[ref] = offset
		edited := offset
		if random.Intn(3) == 0 {
			edited += time.Duration(random.Int63n(year / 4))
		}
		if _, err := store.db.ExecContext(ctx, `
			UPDATE entries SET created_utc = ?, modified_utc = ?
			WHERE workspace_id = ? AND id = ?`,
			at(offset), at(edited), key.Workspace, key.ID); err != nil {
			t.Fatal(err)
		}
	}
	for _, ref := range refs {
		key := store.mustKey(ref)
		if _, err := store.db.ExecContext(ctx, `
			UPDATE entry_messages SET created_utc = ?, modified_utc = ?
			WHERE workspace_id = ? AND entry_id = ?`,
			at(created[ref]+time.Duration(random.Int63n(year/8))),
			at(created[ref]+time.Duration(random.Int63n(year/8))),
			key.Workspace, key.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.db.ExecContext(ctx, `
		UPDATE entries AS e SET activity_utc = `+activityExpr); err != nil {
		t.Fatal(err)
	}
}

func idsOf(t *testing.T, store *Store, result api.SearchResult) []int64 {
	t.Helper()
	var ids []int64
	for _, entry := range result.Entries {
		ids = append(ids, store.mustKey(entry.Ref).ID)
	}
	return ids
}

// topicsPage lists a page of the topics view, reading at most maxRows
// rows walking activity before ranking directly.
func topicsPage(
	t *testing.T,
	store *Store,
	workspace int64,
	filter Filter,
	maxRows int,
) api.SearchResult {
	t.Helper()
	ctx := context.Background()
	v, err := store.begin(ctx, everyAccess(store), true)
	if err != nil {
		t.Fatal(err)
	}
	defer v.tx.Rollback()
	entryFilter := filter
	entryFilter.Changed = Span{}
	where, err := buildWhere(entryFilter)
	if err != nil {
		t.Fatal(err)
	}
	result, err := v.searchTopics(ctx, workspace, filter, `entries AS e`,
		nil, where, maxRows)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestTopicsMatchTheReference(t *testing.T) {
	day := func(month time.Month, day int) time.Time {
		return time.Date(2026, month, day, 0, 0, 0, 0, time.UTC)
	}
	spans := []Span{
		{},
		{After: day(7, 1)},
		{Before: day(4, 1)},
		{After: day(3, 1), Before: day(9, 1)},
		{After: day(5, 1), Before: day(5, 1)},
	}
	for seed := int64(1); seed <= 6; seed++ {
		store := openTestStore(t)
		random := rand.New(rand.NewSource(seed))
		addRandomForest(t, store, random, seed%2 == 0)
		defaultID := workspaceID(t, store, "default")

		for _, project := range []string{"", "a"} {
			for _, include := range []bool{false, true} {
				for _, span := range spans {
					want := referenceTopics(t, store, defaultID, project,
						include, span)
					filter := Filter{View: ViewTopics,
						IncludeSuperseded: include, Changed: span}
					if project != "" {
						filter.Project = []FieldCond{{Value: project}}
					}
					label := fmt.Sprintf("seed %d, project %q, include "+
						"superseded %v, span %v", seed, project, include,
						span)
					checkTopicPages(t, store, defaultID, filter, want,
						label)
					checkWalkMatchesRanking(t, store, defaultID, filter,
						label)
				}
			}
		}
	}
}

// checkTopicPages checks pages of the topics view against want, the
// whole view in order, whether it walks activity, gives up walking
// part way, or ranks directly from the start.
func checkTopicPages(
	t *testing.T,
	store *Store,
	workspace int64,
	filter Filter,
	want []int64,
	label string,
) {
	t.Helper()
	for _, page := range []struct{ offset, limit int }{
		{0, 500}, {0, 1}, {2, 3}, {max(0, len(want)-2), 2},
		{len(want), 5},
	} {
		filter.Offset, filter.Limit = page.offset, page.limit
		wantPage := want[min(page.offset, len(want)):min(
			page.offset+page.limit, len(want))]
		wantMore := len(want) > page.offset+page.limit
		for _, maxRows := range []int{topicWalkRows(filter), 30, 3, 0} {
			result := topicsPage(t, store, workspace, filter, maxRows)
			got := idsOf(t, store, result)
			if !reflect.DeepEqual(got, wantPage) &&
				!(len(got) == 0 && len(wantPage) == 0) {
				t.Fatalf("%s: page %+v walking %d = %v, want %v", label,
					page, maxRows, got, wantPage)
			}
			if result.More != wantMore {
				t.Fatalf("%s: page %+v walking %d: more %v, want %v",
					label, page, maxRows, result.More, wantMore)
			}
		}
	}
}

// checkWalkMatchesRanking checks that reading activity newest first
// ranks every prefix of the candidates as ranking their trees does.
func checkWalkMatchesRanking(
	t *testing.T,
	store *Store,
	workspace int64,
	filter Filter,
	label string,
) {
	t.Helper()
	ctx := context.Background()
	v, err := store.begin(ctx, everyAccess(store), true)
	if err != nil {
		t.Fatal(err)
	}
	defer v.tx.Rollback()
	entryFilter := filter
	entryFilter.Changed = Span{}
	where, err := buildWhere(entryFilter)
	if err != nil {
		t.Fatal(err)
	}
	where.clauses = append(where.clauses, `e.workspace_id = ?`)
	where.args = append(where.args, workspace)
	candidates, err := v.topicCandidates(ctx, workspace, `entries AS e`,
		where, filter.IncludeSuperseded)
	if err != nil {
		t.Fatal(err)
	}
	ranked, err := v.rankTopics(ctx, workspace, candidates, filter.Changed)
	if err != nil {
		t.Fatal(err)
	}
	shownOf := func(ctx context.Context, ids []int64) ([]int64, error) {
		return v.shownOf(ctx, workspace, ids, `entries AS e`, where,
			filter.IncludeSuperseded)
	}
	for _, count := range []int{1, 5, len(candidates)} {
		walked, ok, err := v.walkTopics(ctx, workspace, count,
			filter.Changed, 1<<30, shownOf)
		if err != nil || !ok {
			t.Fatalf("%s: walking %d: ok %v, %v", label, count, ok, err)
		}
		want := ranked[:min(count, len(ranked))]
		if len(walked) > len(want) {
			walked = walked[:len(want)]
		}
		if !reflect.DeepEqual(walked, want) && !(len(want) == 0 &&
			len(walked) == 0) {
			t.Fatalf("%s: walking %d = %v, want %v", label, count, walked,
				want)
		}
	}
}

// addBusyTree adds size entries to a workspace in one tree, each under
// a random earlier one and with one message, after older top-level
// entries, so the tree holds the newest activity. It returns the
// tree's root.
func addBusyTree(
	t *testing.T, store *Store, workspace int64, size, older int,
) int64 {
	t.Helper()
	random := rand.New(rand.NewSource(1))
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	at := func(tick int) string {
		return start.Add(time.Duration(tick) * time.Second).Format(timeLayout)
	}
	type row struct {
		ID       int64  `json:"id"`
		Parent   int64  `json:"parent"`
		Created  string `json:"created"`
		Activity string `json:"activity"`
	}
	var rows []row
	root := int64(older + 1)
	for id := int64(1); id <= root+int64(size)-1; id++ {
		r := row{ID: id, Created: at(2 * len(rows)),
			Activity: at(2*len(rows) + 1)}
		if id > root {
			r.Parent = root + random.Int63n(id-root)
		}
		rows = append(rows, r)
	}
	encoded, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}

	for _, statement := range []string{`
		INSERT INTO entries (workspace_id, id, created_utc, modified_utc,
			activity_utc, revision, project, subject)
		SELECT ?1, r.value ->> 'id', r.value ->> 'created',
			r.value ->> 'created', r.value ->> 'activity', 0, 'pond',
			'duck'
		FROM json_each(?2) AS r`, `
		INSERT INTO entry_links
		SELECT ?1, r.value ->> 'id', 'parent', ?1, r.value ->> 'parent'
		FROM json_each(?2) AS r WHERE r.value ->> 'parent' != 0`, `
		INSERT INTO entry_messages (workspace_id, entry_id, id,
			created_utc, modified_utc, revision, body)
		SELECT ?1, r.value ->> 'id', 1, r.value ->> 'activity',
			r.value ->> 'activity', 0, 'quack'
		FROM json_each(?2) AS r`,
	} {
		if _, err := store.db.Exec(statement, workspace,
			string(encoded)); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// A page of the topics view under one busy tree is found by walking
// activity, rather than by ranking every tree in the workspace, since
// the walk follows a tree's paths up a level at a time.
func TestTopicsWalkPastABusyTree(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	workspace := workspaceID(t, store, "default")
	root := addBusyTree(t, store, workspace, 4000, 30)
	filter := Filter{View: ViewTopics, Limit: 20}

	v, err := store.begin(ctx, everyAccess(store), true)
	if err != nil {
		t.Fatal(err)
	}
	defer v.tx.Rollback()
	where := whereBuilder{clauses: []string{`e.workspace_id = ?`},
		args: []any{workspace}}
	shownOf := func(ctx context.Context, ids []int64) ([]int64, error) {
		return v.shownOf(ctx, workspace, ids, `entries AS e`, where, false)
	}
	ranked, ok, err := v.walkTopics(ctx, workspace, filter.Limit+1,
		Span{}, topicWalkRows(filter), shownOf)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("the walk gave up within the busy tree")
	}
	if len(ranked) != filter.Limit+1 || ranked[0] != root ||
		ranked[1] != 30 {
		t.Fatalf("walk ranked %v, want the tree's root %d, then 30 down",
			ranked, root)
	}
}
