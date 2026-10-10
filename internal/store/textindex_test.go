package store

import (
	"context"
	"slices"
	"testing"

	"github.com/airspacesh/workstream/internal/api"
)

// globMatches lists the entries of the default workspace that a
// condition on an entry e matches, given a pattern as ?2, read without
// the text indexes.
func globMatches(
	t *testing.T, store *Store, condition, pattern string,
) []int64 {
	t.Helper()
	rows, err := store.db.Query(`
		SELECT e.id FROM entries AS e
		WHERE e.workspace_id = ?1 AND (`+condition+`)
		ORDER BY e.id`,
		workspaceID(t, store, "default"), pattern)
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
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return ids
}

// addFiller adds more entries than a search walks before it looks its
// text up in the text indexes, all newer than everything else, so a
// search for older text goes on to the indexes. Each one's subject is
// 0 and its body empty.
func addFiller(t *testing.T, store *Store) {
	t.Helper()
	if _, err := store.db.Exec(`
		WITH RECURSIVE n(i) AS (
			SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < ?1
		)
		INSERT INTO entries (
			workspace_id, id, created_utc, modified_utc, activity_utc,
			revision, project, subject
		)
		SELECT ?2, (SELECT max(id) FROM entries WHERE workspace_id = ?2) + i,
			?3, ?3, ?3, 0, 'pond', '0'
		FROM n`,
		changesBeforeIndex+changesPerMatch*100,
		workspaceID(t, store, "default"), nowUTC()); err != nil {
		t.Fatalf("adding filler: %v", err)
	}
}

func TestTextIndexKeepsGlobSemantics(t *testing.T) {
	store := openTestStore(t)
	mustAdd(t, store, "Count the Ducklings", "", nil)
	mustAdd(t, store, "Fix Nest", "Ducklings are safe", nil)
	mustAdd(t, store, `Say "quack" ]twice[`, "", nil)
	mustAdd(t, store, "Æther pond", "a\nduck", nil)
	chatty := mustAdd(t, store, "Chat", "", nil)
	mustPost(t, store, chatty.Ref, "The ducklings hid in the reeds")
	mustPost(t, store, chatty.Ref, "fix the nest by noon")
	// Every run of a pattern is in this subject, but in another order.
	mustAdd(t, store, "nest fix", "", nil)
	addFiller(t, store)

	patterns := []string{
		"ducklings", "DUCKLINGS", "duck*safe", "safe*duck", "fix nest",
		"fix nes?", "fi[xz] nest", "[^a]est", "[]]twice", "twice[",
		`"quack"`, "ck d", "a?duck", "ÆTHER", "æther", "æt", "du", "d",
		"nest*fix", "fix*nest", "reeds", "noon", "[a-z][a-z][a-z]", "%",
		"_",
	}
	fields := []struct {
		name      string
		condition string
		filter    func(conditions []FieldCond) Filter
	}{
		{"content", `e.subject_lower GLOB ?2 OR e.body_lower GLOB ?2
			OR EXISTS (
				SELECT 1 FROM entry_messages AS m
				WHERE m.workspace_id = e.workspace_id
					AND m.entry_id = e.id AND lower(m.body) GLOB ?2
			)`, func(conditions []FieldCond) Filter {
			return Filter{Content: conditions}
		}},
		{"subject", `e.subject_lower GLOB ?2`,
			func(conditions []FieldCond) Filter {
				return Filter{Subject: conditions}
			}},
		{"body", `e.body_lower GLOB ?2`,
			func(conditions []FieldCond) Filter {
				return Filter{Body: conditions}
			}},
	}
	for _, field := range fields {
		for _, pattern := range patterns {
			t.Run(field.name+" "+pattern, func(t *testing.T) {
				widened := pattern
				if widened != "" {
					widened = "*" + lowerASCII(pattern) + "*"
				}
				want := globMatches(t, store, field.condition, widened)
				result := runSearch(t, store,
					field.filter([]FieldCond{{Value: pattern}}))
				var got []int64
				for _, entry := range result.Entries {
					got = append(got, entry.Ref.ID)
				}
				slices.Sort(got)
				if !slices.Equal(got, want) {
					t.Fatalf("matched entries %v, want %v", got, want)
				}
			})
		}
	}
}

func TestTextIndexFollowsEdits(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	entry := mustAdd(t, store, "Count the herons", "In the reeds", nil)
	message := mustPost(t, store, entry.Ref, "Saw an egret")
	subject, body := "Count the ducks", "On the pond"
	if _, err := store.Edit(ctx, everyAccess(store),
		store.mustKey(entry.Ref), api.EditEntryRequest{
			Subject: &subject, Body: &body, User: testUser,
		}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.EditMessage(ctx, store.mustKey(entry.Ref),
		message.Number, ownEdit("Saw a swan")); err != nil {
		t.Fatal(err)
	}
	addFiller(t, store)

	for text, found := range map[string]bool{
		"herons": false, "reeds": false, "egret": false,
		"ducks": true, "pond": true, "swan": true,
	} {
		result := runSearch(t, store, Filter{
			Content: []FieldCond{{Value: text}},
		})
		if got := len(result.Entries) == 1; got != found {
			t.Errorf("search for %q found the entry: %v, want %v", text,
				got, found)
		}
	}
}

// An edit overwrites text so it is gone, so the trigrams of the old
// text must leave the index rather than wait in it for a merge.
func TestTextIndexForgetsOverwrittenText(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	// No other text holds this character, so its trigram is stored
	// whole, not as a suffix of another term.
	const secret = "ǂǂǂ"
	entry := mustAdd(t, store, "Heron "+secret, "", nil)
	message := mustPost(t, store, entry.Ref, "Egret "+secret)
	subject := "Heron"
	if _, err := store.Edit(ctx, everyAccess(store),
		store.mustKey(entry.Ref), api.EditEntryRequest{
			Subject: &subject, User: testUser,
		}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.EditMessage(ctx, store.mustKey(entry.Ref),
		message.Number, ownEdit("Egret")); err != nil {
		t.Fatal(err)
	}

	for _, index := range []string{"entry_text", "message_text"} {
		var blocks int
		if err := store.db.QueryRow(`
			SELECT count(*) FROM `+index+`_data
			WHERE instr(block, CAST(? AS BLOB)) > 0`, secret,
		).Scan(&blocks); err != nil {
			t.Fatal(err)
		}
		if blocks != 0 {
			t.Errorf("%d blocks of %s still hold the overwritten text",
				blocks, index)
		}
	}
}

// The newest changes hold the first page here, and the text indexes
// find the older matches the later pages need.
func TestSearchPagesFromTheNewestChangesOnToTheIndex(t *testing.T) {
	store := openTestStore(t)
	oldest := mustAdd(t, store, "Heron at dawn", "", nil)
	addFiller(t, store)
	older := mustAdd(t, store, "Heron at noon", "", nil)
	newest := mustAdd(t, store, "Count", "", nil)
	mustPost(t, store, newest.Ref, "A heron at dusk")

	for offset, want := range []api.Ref{newest.Ref, older.Ref, oldest.Ref} {
		page := runSearch(t, store, Filter{
			Content: []FieldCond{{Value: "heron"}}, Limit: 1, Offset: offset,
		})
		assertEntryRefs(t, page.Entries, want)
		if page.More != (offset < 2) {
			t.Fatalf("more at offset %d = %v", offset, page.More)
		}
	}
}
