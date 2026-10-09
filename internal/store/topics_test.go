package store

import (
	"context"
	"fmt"
	"math/rand"
	"reflect"
	"testing"
	"time"

	"github.com/airspacesh/workstream/internal/api"
)

// referenceTopics is the topics view as one query that works out the
// activity of every tree in the workspace. It is too slow for a large
// workspace, and serves here as the definition the view must match.
func referenceTopics(
	t *testing.T,
	store *Store,
	workspace int64,
	typ string,
	includeSuperseded bool,
) []int64 {
	t.Helper()
	visible := `(NOT ` + supersededClause + ` OR roots.current_under)`
	if includeSuperseded {
		visible = `1`
	}
	rows, err := store.db.Query(`
		WITH RECURSIVE tree(workspace_id, entry_id, root_id) AS (
			SELECT e.workspace_id, e.id, e.id FROM entries AS e
			WHERE e.workspace_id = ? AND `+topLevelClause+`
			UNION ALL
			SELECT l.source_workspace_id, l.source_id, tree.root_id
			FROM entry_links AS l JOIN tree
				ON l.target_workspace_id = tree.workspace_id
				AND l.target_id = tree.entry_id
			WHERE l.name = 'parent'
		),
		roots(id, activity_utc, current_under) AS (
			SELECT tree.root_id, max(`+activityExpr+`),
				coalesce(max(NOT (e.workspace_id = ? AND e.id = tree.root_id)
					AND NOT `+supersededClause+`), 0)
			FROM tree JOIN entries AS e
				ON e.workspace_id = tree.workspace_id
				AND e.id = tree.entry_id
			GROUP BY tree.root_id
		)
		SELECT e.id FROM entries AS e JOIN roots ON roots.id = e.id
		WHERE e.workspace_id = ? AND `+visible+`
			AND (? = '' OR e.type = ?)
		ORDER BY roots.activity_utc DESC, e.id DESC`,
		workspace, workspace, workspace, typ, typ)
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
// edits, then spreads their times over a year. When onDays is set, every time falls on midnight, so many
// trees share their latest activity.
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
			Type:    []string{"a", "b"}[random.Intn(2)],
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
}

func idsOf(t *testing.T, store *Store, result api.SearchResult) []int64 {
	t.Helper()
	var ids []int64
	for _, entry := range result.Entries {
		ids = append(ids, store.mustKey(entry.Ref).ID)
	}
	return ids
}

// topicsPage lists a page of the topics view, walking at most maxWalks
// paths up before ranking directly.
func topicsPage(
	t *testing.T,
	store *Store,
	access Access,
	workspace int64,
	filter Filter,
	maxWalks int,
) api.SearchResult {
	t.Helper()
	ctx := context.Background()
	v, err := store.begin(ctx, access, true)
	if err != nil {
		t.Fatal(err)
	}
	defer v.tx.Rollback()
	where, err := buildWhere(filter)
	if err != nil {
		t.Fatal(err)
	}
	where.clauses = append(where.clauses, `e.workspace_id = ?`)
	where.args = append(where.args, workspace)
	result, err := v.searchTopics(ctx, workspace, filter, `entries AS e`,
		where, maxWalks)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestTopicsMatchTheReference(t *testing.T) {
	for seed := int64(1); seed <= 12; seed++ {
		store := openTestStore(t)
		random := rand.New(rand.NewSource(seed))
		addRandomForest(t, store, random, seed%2 == 0)
		defaultID := workspaceID(t, store, "default")

		for _, access := range []Access{
			everyAccess(store), accessTo(t, store, "default"),
		} {
			for _, typ := range []string{"", "a"} {
				for _, include := range []bool{false, true} {
					want := referenceTopics(t, store, defaultID, typ,
						include)
					filter := Filter{View: ViewTopics,
						IncludeSuperseded: include}
					if typ != "" {
						filter.Type = []FieldCond{{Value: typ}}
					}
					label := fmt.Sprintf("seed %d, access %v, type %q, "+
						"include superseded %v", seed, access, typ, include)
					checkTopicPages(t, store, access, defaultID, filter,
						want, label)
					checkWalkMatchesRanking(t, store, access, defaultID,
						filter, label)
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
	access Access,
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
		for _, maxWalks := range []int{topicWalks(filter), 3, 0} {
			result := topicsPage(t, store, access, workspace, filter,
				maxWalks)
			got := idsOf(t, store, result)
			if !reflect.DeepEqual(got, wantPage) &&
				!(len(got) == 0 && len(wantPage) == 0) {
				t.Fatalf("%s: page %+v walking %d = %v, want %v", label,
					page, maxWalks, got, wantPage)
			}
			if result.More != wantMore {
				t.Fatalf("%s: page %+v walking %d: more %v, want %v",
					label, page, maxWalks, result.More, wantMore)
			}
		}
	}
}

// checkWalkMatchesRanking checks that reading activity newest first
// ranks every prefix of the candidates as ranking their trees does.
func checkWalkMatchesRanking(
	t *testing.T,
	store *Store,
	access Access,
	workspace int64,
	filter Filter,
	label string,
) {
	t.Helper()
	ctx := context.Background()
	v, err := store.begin(ctx, access, true)
	if err != nil {
		t.Fatal(err)
	}
	defer v.tx.Rollback()
	where, err := buildWhere(filter)
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
	ranked, err := v.rankTopics(ctx, workspace, candidates)
	if err != nil {
		t.Fatal(err)
	}
	shows := func(ctx context.Context, id int64) (bool, error) {
		return v.topicShown(ctx, workspace, id, `entries AS e`, where,
			filter.IncludeSuperseded)
	}
	for _, count := range []int{1, 5, len(candidates)} {
		walked, ok, err := v.walkTopics(ctx, workspace, count, 1<<30, shows)
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
