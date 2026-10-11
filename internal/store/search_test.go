package store

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/airspacesh/workstream/internal/api"
)

type searchPosts struct {
	store  *Store
	first  api.Post
	second api.Post
	third  api.Post
	fourth api.Post
	fifth  api.Post
}

func newSearchPosts(t *testing.T) searchPosts {
	t.Helper()
	store := openTestStore(t)
	add := func(
		project, subject, body string, metadata map[string]string,
	) api.Post {
		t.Helper()
		return addTo(t, store, "general", api.AddPostRequest{
			Project: project, Subject: subject, Body: body,
			Metadata: metadata,
		})
	}
	return searchPosts{
		store: store,
		first: add("duck-pond", "Count the Ducklings", "",
			map[string]string{"jira": "QUACK-1"}),
		second: add("Duck-Nest", "Fix Nest", "Ducklings are safe", nil),
		third: add("duck-pond", "Ducklings restless", "",
			map[string]string{"jira": "QUACK-2"}),
		fourth: add("feed", "Cache feed", "", nil),
		fifth:  add("æther", "Æther", "", nil),
	}
}

// runSearch searches the general workspace with every workspace
// readable.
func runSearch(t *testing.T, store *Store, filter Filter) api.MatchPage {
	t.Helper()
	if filter.Limit == 0 {
		filter.Limit = 50
	}
	result, err := store.Search(context.Background(), everyAccess(store),
		workspaceID(t, store, "general"), filter)
	if err != nil {
		t.Fatalf("searching: %v", err)
	}
	return result
}

func assertPostRefs(t *testing.T, page api.MatchPage, want ...api.Ref) {
	t.Helper()
	assertMatches(t, page, refStrings(want...)...)
}

// assertMatches checks a page's matches by their refs, a post's like e1
// and a message's like e1-m2.
func assertMatches(t *testing.T, page api.MatchPage, want ...string) {
	t.Helper()
	if got := matchRefs(page); !slices.Equal(got, want) &&
		!(len(got) == 0 && len(want) == 0) {
		t.Fatalf("matches = %v, want %v", got, want)
	}
}

// messageRef names a message of a post, like e1-m2.
func messageRef(post api.Ref, number int64) string {
	return api.MessageRef{Post: post, Number: number}.String()
}

func TestSearchUsesRawCaseFoldedGlobPatterns(t *testing.T) {
	posts := newSearchPosts(t)
	tests := []struct {
		name    string
		pattern string
		want    []api.Ref
	}{
		{"whole string", "COUNT THE DUCKLINGS", []api.Ref{posts.first.Ref}},
		{"bare text matches anywhere", "DUCKLINGS", []api.Ref{
			posts.third.Ref, posts.first.Ref,
		}},
		{"empty matches only an empty value", "", nil},
		{"wildcard", "*DUCKLINGS*", []api.Ref{
			posts.third.Ref, posts.first.Ref,
		}},
		{"single character", "fix nes?", []api.Ref{posts.second.Ref}},
		{"character class", "fi[xz] nest", []api.Ref{posts.second.Ref}},
		{"non-ASCII is unchanged", "æther", nil},
		{"matching non-ASCII", "ÆTHER", []api.Ref{posts.fifth.Ref}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := runSearch(t, posts.store, Filter{
				Subject: []FieldCond{{Value: test.pattern}},
			})
			assertPostRefs(t, result, test.want...)
		})
	}
}

func TestSearchSetAndPairNegationUsesNotExists(t *testing.T) {
	posts := newSearchPosts(t)
	result := runSearch(t, posts.store, Filter{
		Key: []FieldCond{{Value: "JIRA"}},
	})
	assertPostRefs(t, result, posts.third.Ref, posts.first.Ref)

	result = runSearch(t, posts.store, Filter{
		Key: []FieldCond{{Negate: true, Value: "JIRA"}},
	})
	assertPostRefs(t, result,
		posts.fifth.Ref, posts.fourth.Ref, posts.second.Ref)

	result = runSearch(t, posts.store, Filter{
		Meta: []MetaCond{{Key: "jira", Value: "QUACK-*"}},
	})
	assertPostRefs(t, result, posts.third.Ref, posts.first.Ref)

	result = runSearch(t, posts.store, Filter{
		Meta: []MetaCond{{Key: "jira", Value: "QUACK"}},
	})
	assertPostRefs(t, result)

	result = runSearch(t, posts.store, Filter{
		Meta: []MetaCond{{Negate: true, Key: "jira", Value: "QUACK-1"}},
	})
	assertPostRefs(t, result, posts.fifth.Ref,
		posts.fourth.Ref, posts.third.Ref, posts.second.Ref)
}

func TestSearchMatchesProjectWholeValue(t *testing.T) {
	posts := newSearchPosts(t)
	result := runSearch(t, posts.store, Filter{
		Project: []FieldCond{{Value: "DUCK-*"}},
	})
	assertPostRefs(t, result,
		posts.third.Ref, posts.second.Ref, posts.first.Ref)

	result = runSearch(t, posts.store, Filter{
		Project: []FieldCond{{Value: "duck"}},
	})
	assertPostRefs(t, result)

	result = runSearch(t, posts.store, Filter{
		Project: []FieldCond{{Negate: true, Value: "DUCK-POND"}},
	})
	assertPostRefs(t, result,
		posts.fifth.Ref, posts.fourth.Ref, posts.second.Ref)

	result = runSearch(t, posts.store, Filter{
		Project: []FieldCond{{Value: "ÆTHER"}},
	})
	assertPostRefs(t, result)
}

// A page ends at the oldest match it shows, and the next page lists
// the matches before it.
func TestSearchContentPagingAndMetadata(t *testing.T) {
	posts := newSearchPosts(t)
	ducklings := []FieldCond{{Value: "*DUCKLINGS*"}}
	first := runSearch(t, posts.store, Filter{Content: ducklings, Limit: 2})
	assertPostRefs(t, first, posts.third.Ref, posts.second.Ref)
	if !first.More {
		t.Fatal("no more after the first 2 of 3 matches")
	}
	result := runSearch(t, posts.store, Filter{Content: ducklings,
		Limit: 2, Changed: Span{Before: first.Matches[1].At}})
	if result.More {
		t.Fatal("more after the last of 3 matches")
	}
	assertPostRefs(t, result, posts.first.Ref)
	if got := result.Matches[0].Post.Metadata["jira"]; got != "QUACK-1" {
		t.Fatalf("paged metadata jira = %q, want QUACK-1", got)
	}
}

func TestSearchRanksAnEditedPostFirst(t *testing.T) {
	posts := newSearchPosts(t)
	oldTime := "2000-01-01T00:00:00.000000000Z"
	if _, err := posts.store.db.Exec(
		`UPDATE posts SET modified_utc = ?1, activity_utc = ?1`, oldTime,
	); err != nil {
		t.Fatalf("setting modification times: %v", err)
	}
	if _, err := posts.store.Edit(
		context.Background(), everyAccess(posts.store),
		posts.store.mustKey(posts.first.Ref), api.EditPostRequest{
			Metadata: map[string]string{"jira": "QUACK-9"},
			User:     testUser,
		},
	); err != nil {
		t.Fatalf("editing metadata: %v", err)
	}

	result := runSearch(t, posts.store, Filter{})
	if got := result.Matches[0].Post.Ref; got != posts.first.Ref {
		t.Fatalf("first post = %s, want the edited %s", got,
			posts.first.Ref)
	}
}

func TestSearchRejectsInvalidBoundsAndNUL(t *testing.T) {
	store := openTestStore(t)
	tests := []struct {
		name   string
		filter Filter
		want   string
	}{
		{"zero limit", Filter{}, "limit"},
		{"large limit", Filter{Limit: MaxSearchLimit + 1}, "limit"},
		{"NUL pattern", Filter{
			Limit: 1,
			Body:  []FieldCond{{Value: "bad\x00pattern"}},
		}, "NUL"},
		{"long pattern", Filter{
			Limit: 1,
			Body: []FieldCond{{
				Value: strings.Repeat("x", MaxSearchPatternLength+1),
			}},
		}, "exceeds"},
		{"too many conditions", Filter{
			Limit: 1,
			Subject: make(
				[]FieldCond, MaxSearchConditions+1,
			),
		}, "conditions"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := store.Search(context.Background(),
				everyAccess(store), workspaceID(t, store, "general"),
				test.filter)
			if !errors.Is(err, ErrInvalidFilter) ||
				!strings.Contains(err.Error(), test.want) {
				t.Fatalf("Search error = %v, want text %q", err, test.want)
			}
		})
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := store.Search(ctx, everyAccess(store),
		workspaceID(t, store, "general"), Filter{Limit: 1})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Search error = %v, want context.Canceled", err)
	}
}

func TestSearchByOrigin(t *testing.T) {
	store := openTestStore(t)
	add := func(subject string, origin api.Origin) api.Post {
		t.Helper()
		return addTo(t, store, "general", api.AddPostRequest{
			Subject: subject, Origin: origin,
		})
	}
	first := add("on the first host", api.Origin{
		User: "duck", Host: "POND-A",
		Dir: "/work/pond-a/src", ClaudeSession: "abc-123",
	})
	second := add("on the second host", api.Origin{
		User: "duck", Host: "pond-b",
		Dir: "/work/pond-b/envs",
	})

	// Host match folds ASCII case, like the other text fields.
	got := runSearch(t, store, Filter{
		Limit: 50, OriginHost: []FieldCond{{Value: "pond-a"}},
	})
	assertPostRefs(t, got, first.Ref)

	// Directory glob and session match reach only their post.
	got = runSearch(t, store, Filter{
		Limit: 50, OriginDir: []FieldCond{{Value: "*/envs"}},
	})
	assertPostRefs(t, got, second.Ref)
	got = runSearch(t, store, Filter{
		Limit: 50, OriginClaudeSession: []FieldCond{{Value: "abc-*"}},
	})
	assertPostRefs(t, got, first.Ref)

	// Negation excludes the matching origin.
	got = runSearch(t, store, Filter{
		Limit: 50, OriginHost: []FieldCond{{Negate: true, Value: "pond-a"}},
	})
	assertPostRefs(t, got, second.Ref)
}

// Posts share numbers across workspaces, so metadata filters must match
// only the metadata of the post in the searched workspace.
func TestSearchMetadataStaysInItsWorkspace(t *testing.T) {
	store := openTestStore(t)
	inGeneral := mustAdd(t, store, "in general", "", nil)
	addTo(t, store, "personal", api.AddPostRequest{
		Subject: "in personal", Project: "secret",
		Metadata: map[string]string{"jira": "QUACK-9"},
	})

	for _, filter := range []Filter{
		{Project: []FieldCond{{Value: "secret"}}},
		{Meta: []MetaCond{{Key: "jira", Value: "*"}}},
		{Key: []FieldCond{{Value: "jira"}}},
	} {
		result := runSearch(t, store, filter)
		assertPostRefs(t, result)
	}
	result := runSearch(t, store, Filter{
		Meta: []MetaCond{{Negate: true, Key: "jira", Value: "*"}},
	})
	assertPostRefs(t, result, inGeneral.Ref)
}

func messageBodies(messages []api.Message) []string {
	var bodies []string
	for _, message := range messages {
		bodies = append(bodies, message.Body)
	}
	return bodies
}

// Each matching message is a match of its own, ranked by when it was
// posted, beside the matching posts.
func TestSearchMatchesChatMessages(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	pond := mustAdd(t, store, "Pond notes", "", nil)
	sighting := mustAdd(t, store, "Heron sighting", "", nil)
	quiet := mustAdd(t, store, "Count ducks", "", nil)
	for _, body := range []string{"a heron landed", "nothing here",
		"HERON again", "heron thrice", "heron four"} {
		mustPostMessage(t, store, pond.Ref, body)
	}
	mustPostMessage(t, store, quiet.Ref, "no birds")
	heron := []FieldCond{{Value: "heron"}}
	chatMatches := []string{messageRef(pond.Ref, 5), messageRef(pond.Ref, 4),
		messageRef(pond.Ref, 3), messageRef(pond.Ref, 1),
		sighting.Ref.String()}

	result := runSearch(t, store, Filter{Content: heron, IncludeChat: true})
	assertMatches(t, result, chatMatches...)
	before := Span{}
	for i, want := range chatMatches {
		page := runSearch(t, store, Filter{Content: heron, IncludeChat: true,
			Limit: 1, Changed: before})
		assertMatches(t, page, want)
		if page.More != (i < len(chatMatches)-1) {
			t.Fatalf("more after %s = %v", want, page.More)
		}
		before.Before = page.Matches[0].At
	}

	postsOnly := runSearch(t, store, Filter{Content: heron})
	assertPostRefs(t, postsOnly, sighting.Ref)

	// The other filters apply to the post a message is in.
	sightings := runSearch(t, store, Filter{Content: heron,
		Subject: []FieldCond{{Value: "sighting"}}, IncludeChat: true})
	assertPostRefs(t, sightings, sighting.Ref)

	// Excluding text matches every message and post without it. With
	// chat, a post ranks by its own change, not by its chat.
	without := runSearch(t, store, Filter{
		Content:     []FieldCond{{Negate: true, Value: "heron"}},
		IncludeChat: true,
	})
	assertMatches(t, without, messageRef(quiet.Ref, 1),
		messageRef(pond.Ref, 2), quiet.Ref.String(), pond.Ref.String())

	// An edit lifts the post, but the message keeps its place as the
	// first posted.
	if _, _, err := store.EditMessage(ctx, store.mustKey(pond.Ref), 1,
		ownEdit("heron at dawn")); err != nil {
		t.Fatal(err)
	}
	assertMatches(t, runSearch(t, store, Filter{Content: heron,
		IncludeChat: true}), chatMatches...)
}

// A message matches when it matches every text filter, required and
// excluded, on its own.
func TestSearchMatchesEachMessageOnItsOwn(t *testing.T) {
	store := openTestStore(t)
	split := mustAdd(t, store, "Split", "", nil)
	mustPostMessage(t, store, split.Ref, "heron")
	mustPostMessage(t, store, split.Ref, "egret")
	both := mustAdd(t, store, "Both", "", nil)
	mustPostMessage(t, store, both.Ref, "heron and egret")
	mustPostMessage(t, store, both.Ref, "heron alone")

	result := runSearch(t, store, Filter{Content: []FieldCond{
		{Value: "heron"}, {Value: "egret"},
	}, IncludeChat: true})
	assertMatches(t, result, messageRef(both.Ref, 1))

	result = runSearch(t, store, Filter{Content: []FieldCond{
		{Value: "heron"}, {Negate: true, Value: "egret"},
	}, IncludeChat: true})
	assertMatches(t, result, messageRef(both.Ref, 2),
		messageRef(split.Ref, 1))
}

// A message matches the text and origin filters on its own, and the
// other filters through its post.
func TestSearchMatchesMessagesByOrigin(t *testing.T) {
	store := openTestStore(t)
	add := func(user, project, subject string) api.Post {
		t.Helper()
		return addTo(t, store, "general", api.AddPostRequest{
			Project: project, Subject: subject,
			Origin: api.Origin{User: user},
		})
	}
	design := add("ines", "build", "Hook design")
	release := add("ken", "build", "Release notes")
	cats := add("ines", "pets", "Cats")
	garden := add("ken", "garden", "Garden")
	postAs(t, store, "ken", release.Ref, "hook fails")
	postAs(t, store, "ines", release.Ref, "hook fixed")
	postAs(t, store, "ines", release.Ref, "lunch")
	postAs(t, store, "ken", cats.Ref, "hook question")
	postAs(t, store, "ines", garden.Ref, "hook in the garden")
	hook := []FieldCond{{Value: "hook"}}
	ines := []FieldCond{{Value: "ines"}}
	notInes := []FieldCond{{Negate: true, Value: "ines"}}

	tests := []struct {
		name   string
		filter Filter
		want   []string
	}{
		{"text and origin match the same message",
			Filter{Content: hook, OriginUser: ines, IncludeChat: true},
			[]string{messageRef(garden.Ref, 1), messageRef(release.Ref, 2),
				design.Ref.String()}},
		{"an origin alone",
			Filter{OriginUser: ines, IncludeChat: true},
			[]string{messageRef(garden.Ref, 1), messageRef(release.Ref, 3),
				messageRef(release.Ref, 2), cats.Ref.String(),
				design.Ref.String()}},
		{"the other filters apply to a message's post",
			Filter{OriginUser: ines, Project: []FieldCond{{Value: "build"}},
				IncludeChat: true},
			[]string{messageRef(release.Ref, 3), messageRef(release.Ref, 2),
				design.Ref.String()}},
		{"an excluded origin excludes messages too",
			Filter{Content: hook, OriginUser: notInes, IncludeChat: true},
			[]string{messageRef(cats.Ref, 1), messageRef(release.Ref, 1)}},
		{"an excluded origin alone",
			Filter{OriginUser: notInes, IncludeChat: true},
			[]string{messageRef(cats.Ref, 1), messageRef(release.Ref, 1),
				garden.Ref.String(), release.Ref.String()}},
		{"posts only",
			Filter{OriginUser: ines},
			[]string{cats.Ref.String(), design.Ref.String()}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertMatches(t, runSearch(t, store, test.filter), test.want...)
		})
	}
}

// The text indexes find an old post by its own text, and its messages
// then match by origin.
func TestSearchMatchesMessagesByOriginInPostsTheIndexFinds(t *testing.T) {
	store := openTestStore(t)
	heron := mustAdd(t, store, "Heron at dawn", "", nil)
	postAs(t, store, "ines", heron.Ref, "seen twice")
	postAs(t, store, "ken", heron.Ref, "seen once")
	addFiller(t, store)

	result := runSearch(t, store, Filter{
		Subject:     []FieldCond{{Value: "heron"}},
		OriginUser:  []FieldCond{{Value: "ines"}},
		IncludeChat: true,
	})
	assertMatches(t, result, messageRef(heron.Ref, 1))
}

// The origin user indexes find old posts, by their own origin or a
// message's, and keep the GLOB semantics of the walk.
func TestOriginUserIndexKeepsGlobSemantics(t *testing.T) {
	store := openTestStore(t)
	add := func(user string) api.Post {
		t.Helper()
		return addTo(t, store, "general", api.AddPostRequest{
			Project: "pond", Subject: "Duck", Origin: api.Origin{User: user},
		})
	}
	add("Ines")
	add("inesa")
	add("ken")
	chat := add("ken")
	postAs(t, store, "INES", chat.Ref, "quack")
	postAs(t, store, "inez", chat.Ref, "honk")
	addFiller(t, store)

	matches := `e.origin_user_lower GLOB ?2 OR EXISTS (
		SELECT 1 FROM post_messages AS m
		WHERE m.workspace_id = e.workspace_id AND m.post_id = e.id
			AND lower(m.origin_user) GLOB ?2
	)`
	for _, pattern := range []string{
		"ines", "INES", "ines*", "ine?", "ine[sz]", "inez", "in", "i*",
		"*nes", "ken", "nobody",
	} {
		t.Run(pattern, func(t *testing.T) {
			want := globMatches(t, store, matches, lowerASCII(pattern))
			result := runSearch(t, store, Filter{
				OriginUser: []FieldCond{{Value: pattern}}, IncludeChat: true,
			})
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

	// A message must match the text and the origin, so ken's chat has no
	// match from ken, though both indexes find it.
	result := runSearch(t, store, Filter{
		Content:     []FieldCond{{Value: "quack"}},
		OriginUser:  []FieldCond{{Value: "ines"}},
		IncludeChat: true,
	})
	assertMatches(t, result, messageRef(chat.Ref, 1))
	result = runSearch(t, store, Filter{
		Content:     []FieldCond{{Value: "quack"}},
		OriginUser:  []FieldCond{{Value: "ken"}},
		IncludeChat: true,
	})
	assertMatches(t, result)
}

func messageRefs(messages []api.Message) []string {
	var refs []string
	for _, message := range messages {
		refs = append(refs, message.Ref().String())
	}
	return refs
}

// A time filter keeps to the matches whose change they rank by falls
// within a span, from its start up to but not including its end. That
// is a post's latest change to it or its chat without chat, or to it
// alone with chat, and a message's posting.
func TestSearchKeepsToASpan(t *testing.T) {
	store := openTestStore(t)
	day := func(month time.Month, day int) time.Time {
		return time.Date(2026, month, day, 0, 0, 0, 0, time.UTC)
	}
	setChanged := func(ref api.Ref, message int64, at time.Time) {
		t.Helper()
		key := store.mustKey(ref)
		query := `UPDATE posts SET created_utc = ?1, modified_utc = ?1
			WHERE workspace_id = ?2 AND id = ?3`
		if message != 0 {
			query = `UPDATE post_messages SET created_utc = ?1,
				modified_utc = ?1 WHERE workspace_id = ?2 AND post_id = ?3
				AND id = ?4`
		}
		if _, err := store.db.Exec(query, formatTime(at), key.Workspace,
			key.ID, message); err != nil {
			t.Fatal(err)
		}
	}
	old := mustAdd(t, store, "Old", "", nil)
	setChanged(old.Ref, 0, day(1, 1))
	mid := mustAdd(t, store, "Mid", "", nil)
	setChanged(mid.Ref, 0, day(2, 1))
	setChanged(mid.Ref, postAs(t, store, "ines", mid.Ref, "late").Number,
		day(3, 1))
	setChanged(mid.Ref, postAs(t, store, "ken", mid.Ref, "early").Number,
		day(1, 15))
	recent := mustAdd(t, store, "Recent", "", nil)
	setChanged(recent.Ref, 0, day(4, 1))
	if _, err := store.db.Exec(`
		UPDATE posts AS e SET activity_utc = ` + activityExpr); err != nil {
		t.Fatal(err)
	}
	late, early := messageRef(mid.Ref, 1), messageRef(mid.Ref, 2)

	tests := []struct {
		name   string
		filter Filter
		want   []string
	}{
		{"after",
			Filter{Changed: Span{After: day(2, 15)}, IncludeChat: true},
			[]string{recent.Ref.String(), late}},
		{"before leaves out its own instant",
			Filter{Changed: Span{Before: day(3, 1)}, IncludeChat: true},
			[]string{early, mid.Ref.String(), old.Ref.String()}},
		{"after takes in its own instant",
			Filter{Changed: Span{After: day(3, 1), Before: day(3, 2)},
				IncludeChat: true},
			[]string{late}},
		{"with an origin",
			Filter{Changed: Span{After: day(1, 10)},
				OriginUser: []FieldCond{{Value: "ken"}}, IncludeChat: true},
			[]string{early}},
		{"posts only",
			Filter{Changed: Span{After: day(2, 15)}},
			[]string{recent.Ref.String(), mid.Ref.String()}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertMatches(t, runSearch(t, store, test.filter), test.want...)
		})
	}
}

// A search under a post keeps to its subtree in a big workspace,
// whether the indexes find its matches or it reads the subtree newest
// first.
func TestSearchUnderASubtreeInABigWorkspace(t *testing.T) {
	store := openTestStore(t)
	root := mustAdd(t, store, "Heron nest", "", nil)
	child := addTo(t, store, "general", api.AddPostRequest{
		Project: "pond", Subject: "Reeds",
		Links: []api.Link{link(api.ParentLink, root.Ref)}})
	grandchild := addTo(t, store, "general", api.AddPostRequest{
		Project: "pond", Subject: "Heron eggs",
		Links: []api.Link{link(api.ParentLink, child.Ref)}})
	postAs(t, store, "ines", child.Ref, "a heron landed")
	postAs(t, store, "ken", root.Ref, "no birds today")
	outside := mustAdd(t, store, "Heron elsewhere", "", nil)
	postAs(t, store, "ines", outside.Ref, "a heron again")
	addFiller(t, store)

	under := store.mustKey(root.Ref)
	heron := []FieldCond{{Value: "heron"}}
	tests := []struct {
		name   string
		filter Filter
		want   []string
	}{
		{"text",
			Filter{Under: &under, Content: heron, IncludeChat: true},
			[]string{messageRef(child.Ref, 1), grandchild.Ref.String(),
				root.Ref.String()}},
		{"posts only",
			Filter{Under: &under, Content: heron},
			[]string{root.Ref.String(), grandchild.Ref.String()}},
		{"origin",
			Filter{Under: &under, OriginUser: []FieldCond{{Value: "ines"}},
				IncludeChat: true},
			[]string{messageRef(child.Ref, 1)}},
		{"nothing matches",
			Filter{Under: &under, OriginHost: []FieldCond{{Value: "h"}},
				IncludeChat: true},
			nil},
	}
	run := func(t *testing.T) {
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				assertMatches(t, runSearch(t, store, test.filter),
					test.want...)
			})
		}
	}
	t.Run("small subtree", run)

	if _, err := store.db.Exec(`
		INSERT INTO post_links (source_workspace_id, source_id, name,
			target_workspace_id, target_id)
		SELECT workspace_id, id, 'parent', workspace_id, ?1 FROM posts
		WHERE workspace_id = ?2 AND subject = '0'`,
		under.ID, under.Workspace); err != nil {
		t.Fatalf("moving the filler under the root: %v", err)
	}
	t.Run("large subtree", run)
}
