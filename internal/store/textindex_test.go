package store

import (
	"context"
	"slices"
	"testing"

	"github.com/airspacesh/workstream/internal/api"
)

// globMatches lists the posts of the general workspace that a condition
// on a post e matches, given a pattern as ?2, read without the text
// indexes.
func globMatches(
	t *testing.T, store *Store, condition, pattern string,
) []int64 {
	t.Helper()
	rows, err := store.db.Query(`
		SELECT e.id FROM posts AS e
		WHERE e.workspace_id = ?1 AND (`+condition+`)
		ORDER BY e.id`,
		workspaceID(t, store, "general"), pattern)
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

// addFiller adds more posts than a search reads by key from the
// indexes, all newer than everything else, so a search that walks
// rather than reading what the indexes find reads them all first. Each
// one's subject is 0 and its body empty.
func addFiller(t *testing.T, store *Store) {
	t.Helper()
	for _, statement := range []string{`
		WITH RECURSIVE n(i) AS (
			SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < ?1
		)
		INSERT INTO posts (
			workspace_id, id, created_utc, modified_utc, activity_utc,
			revision, activity_revision, posted_revision, project, subject
		)
		SELECT ?2, (SELECT max(id) FROM posts WHERE workspace_id = ?2) + i,
			?3, ?3, ?3, revision + i, revision + i, revision + i, 'pond', '0'
		FROM n, revision_counter`, `
		UPDATE revision_counter SET revision = revision + ?1`,
	} {
		if _, err := store.db.Exec(statement, candidateLimit+1,
			workspaceID(t, store, "general"), nowUTC()); err != nil {
			t.Fatalf("adding filler: %v", err)
		}
	}
}

func TestTextIndexKeepsGlobSemantics(t *testing.T) {
	store := openTestStore(t)
	mustAdd(t, store, "Count the Ducklings", "", nil)
	mustAdd(t, store, "Fix Nest", "Ducklings are safe", nil)
	mustAdd(t, store, `Say "quack" ]twice[`, "", nil)
	mustAdd(t, store, "Æther pond", "a\nduck", nil)
	chatty := mustAdd(t, store, "Chat", "", nil)
	mustPostMessage(t, store, chatty.Ref, "The ducklings hid in the reeds")
	mustPostMessage(t, store, chatty.Ref, "fix the nest by noon")
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
				SELECT 1 FROM post_messages AS m
				WHERE m.workspace_id = e.workspace_id
					AND m.post_id = e.id AND lower(m.body) GLOB ?2
			)`, func(conditions []FieldCond) Filter {
			return Filter{Content: conditions, Items: api.PostsAndChat}
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
				for _, match := range result.Matches {
					got = append(got, match.Post.Ref.ID)
				}
				slices.Sort(got)
				got = slices.Compact(got)
				if !slices.Equal(got, want) {
					t.Fatalf("matched posts %v, want %v", got, want)
				}
			})
		}
	}
}

func TestTextIndexFollowsEdits(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	post := mustAdd(t, store, "Count the herons", "In the reeds", nil)
	message := mustPostMessage(t, store, post.Ref, "Saw an egret")
	subject, body := "Count the ducks", "On the pond"
	if _, err := store.Edit(ctx, everyAccess(store),
		store.mustKey(post.Ref), api.EditPostRequest{
			Subject: &subject, Body: &body, User: testUser,
		}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.EditMessage(ctx, everyAccess(store),
		store.mustKey(post.Ref),
		message.Number, ownEdit("Saw a swan")); err != nil {
		t.Fatal(err)
	}
	addFiller(t, store)

	for text, found := range map[string]bool{
		"herons": false, "reeds": false, "egret": false,
		"ducks": true, "pond": true, "swan": true,
	} {
		result := runSearch(t, store, Filter{
			Content: []FieldCond{{Value: text}}, Items: api.PostsAndChat,
		})
		if got := len(result.Matches) == 1; got != found {
			t.Errorf("search for %q found the post: %v, want %v", text,
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
	post := mustAdd(t, store, "Heron "+secret, "", nil)
	message := mustPostMessage(t, store, post.Ref, "Egret "+secret)
	subject := "Heron"
	if _, err := store.Edit(ctx, everyAccess(store),
		store.mustKey(post.Ref), api.EditPostRequest{
			Subject: &subject, User: testUser,
		}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.EditMessage(ctx, everyAccess(store),
		store.mustKey(post.Ref),
		message.Number, ownEdit("Egret")); err != nil {
		t.Fatal(err)
	}

	for _, index := range []string{"post_text", "message_text"} {
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

// A delete removes text for good, so neither a search nor the
// index's blocks may find it after.
func TestTextIndexForgetsDeletedText(t *testing.T) {
	store := openTestStore(t)
	const secret = "ǂǂǂ"
	post := mustAdd(t, store, "Heron "+secret, "Egret "+secret, nil)
	message := mustPostMessage(t, store, post.Ref, "Swan "+secret)
	addFiller(t, store)
	for _, text := range []string{"heron", "egret", "swan", secret} {
		result := runSearch(t, store, Filter{
			Content: []FieldCond{{Value: text}}, Items: api.PostsAndChat,
		})
		if len(result.Matches) == 0 {
			t.Fatalf("before the delete, a search for %q found nothing",
				text)
		}
	}

	mustDelete(t, store, post.Ref)
	mustDeleteMessage(t, store, message.Ref())

	for _, text := range []string{"heron", "egret", "swan", secret} {
		result := runSearch(t, store, Filter{
			Content: []FieldCond{{Value: text}}, Items: api.PostsAndChat,
		})
		if len(result.Matches) != 0 {
			t.Errorf("a search for %q still finds the deleted text", text)
		}
	}
	for _, index := range []string{"post_text", "message_text"} {
		var blocks int
		if err := store.db.QueryRow(`
			SELECT count(*) FROM `+index+`_data
			WHERE instr(block, CAST(? AS BLOB)) > 0`, secret,
		).Scan(&blocks); err != nil {
			t.Fatal(err)
		}
		if blocks != 0 {
			t.Errorf("%d blocks of %s still hold the deleted text",
				blocks, index)
		}
	}
}

// The text indexes find old matches beneath newer posts that do not
// match, page after page.
func TestSearchPagesBackThroughTheIndex(t *testing.T) {
	store := openTestStore(t)
	oldest := mustAdd(t, store, "Heron at dawn", "", nil)
	addFiller(t, store)
	older := mustAdd(t, store, "Heron at noon", "", nil)
	newest := mustAdd(t, store, "Count", "", nil)
	mustPostMessage(t, store, newest.Ref, "A heron at dusk")

	want := []string{messageRef(newest.Ref, 1), older.Ref.String(),
		oldest.Ref.String()}
	general := workspaceID(t, store, "general")
	before := RevisionSpan{}
	for i, ref := range want {
		page := runSearch(t, store, Filter{
			Content: []FieldCond{{Value: "heron"}}, Items: api.PostsAndChat,
			Limit: 1, Revisions: before,
		})
		assertMatches(t, page, ref)
		if page.More != (i < len(want)-1) {
			t.Fatalf("more after %s = %v", ref, page.More)
		}
		before = timeSpan{Before: page.Matches[0].At}.revisions(t, store,
			general, RevisionSpan{})
	}
}
