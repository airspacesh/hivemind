package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/airspacesh/hivemind/internal/api"
)

func readChanges(
	t *testing.T, store *Store, since int64, limit int,
) api.ChangesPage {
	t.Helper()
	page, err := store.Changes(context.Background(), everyAccess(store),
		workspaceID(t, store, "general"), since,
		Filter{Items: api.PostsAndChat}, nil, limit)
	if err != nil {
		t.Fatal(err)
	}
	return page
}

// changeIDs names each change in a page by the post or message that
// changed.
func changeIDs(page api.ChangesPage) []string {
	ids := []string{}
	for _, change := range page.Changes {
		if change.Message != nil {
			ids = append(ids, change.Message.Ref().String())
		} else {
			ids = append(ids, change.Post.Ref.String())
		}
	}
	return ids
}

// readAllChanges pages through every change after since, limit at a
// time, and returns the changes and the final cursor.
func readAllChanges(
	t *testing.T, store *Store, since int64, limit int,
) ([]string, int64) {
	t.Helper()
	var ids []string
	for {
		page := readChanges(t, store, since, limit)
		ids = append(ids, changeIDs(page)...)
		since = page.Cursor
		if !page.More {
			return ids, since
		}
	}
}

func TestChangesPageOnceEachInTheOrderTheyHappened(t *testing.T) {
	store := openTestStore(t)
	first := mustAdd(t, store, "first", "", nil)
	second := mustAdd(t, store, "second", "", nil)
	mustPostMessage(t, store, second.Ref, "hi")
	mustPostMessage(t, store, first.Ref, "hi")
	mustPostMessage(t, store, first.Ref, "again")
	mustAdd(t, store, "third", "", nil)

	want := []string{"e1", "e2", "e2-m1", "e1-m1", "e1-m2", "e3"}
	for limit := 1; limit <= len(want); limit++ {
		got, _ := readAllChanges(t, store, 0, limit)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("limit %d read %v, want %v", limit, got, want)
		}
	}
}

func TestChangesListAChangeAgainOnlyWhenItChangesAfterTheCursor(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	seen := mustAdd(t, store, "seen", "", nil)
	message := mustPostMessage(t, store, seen.Ref, "hi")
	_, cursor := readAllChanges(t, store, 0, 10)

	unseen := mustAdd(t, store, "unseen", "", nil)
	for _, post := range []api.Post{unseen, seen, unseen} {
		subject := "edited"
		if _, err := store.Edit(ctx, everyAccess(store),
			store.mustKey(post.Ref),
			api.EditPostRequest{Subject: &subject, User: testUser},
		); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := store.EditMessage(ctx, everyAccess(store),
		store.mustKey(seen.Ref),
		message.Number, ownEdit("edited")); err != nil {
		t.Fatal(err)
	}

	got, _ := readAllChanges(t, store, cursor, 10)
	want := []string{"e1", "e2", "e1-m1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("read %v, want %v", got, want)
	}
}

func TestChangesLeaveOutEditsToLinksAlone(t *testing.T) {
	store := openTestStore(t)
	parent := mustAdd(t, store, "parent", "", nil)
	child := mustAdd(t, store, "child", "", nil)
	_, cursor := readAllChanges(t, store, 0, 10)

	if _, err := store.Edit(context.Background(), everyAccess(store),
		store.mustKey(child.Ref), api.EditPostRequest{
			AddLinks: []api.Link{link("parent", parent.Ref)},
			User:     testUser,
		}); err != nil {
		t.Fatal(err)
	}
	if got := changeIDs(readChanges(t, store, cursor, 10)); len(got) != 0 {
		t.Fatalf("a link edit showed as changes %v", got)
	}
}

func TestLatestRevisionSkipsEveryChangeSoFar(t *testing.T) {
	store := openTestStore(t)
	post := mustAdd(t, store, "first", "", nil)
	mustPostMessage(t, store, post.Ref, "hi")
	addTo(t, store, "personal", api.AddPostRequest{Subject: "private"})
	latest, err := store.LatestRevision(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := changeIDs(readChanges(t, store, latest, 10)); len(got) != 0 {
		t.Fatalf("after the latest revision read %v", got)
	}

	mustPostMessage(t, store, post.Ref, "later")
	if got := changeIDs(readChanges(t, store, latest, 10)); !reflect.DeepEqual(
		got, []string{"e1-m2"}) {
		t.Fatalf("read %v, want the later message alone", got)
	}
}

func TestRefusedEditsTakeNoRevision(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	post := mustAdd(t, store, "mine", "", nil)
	message := mustPostMessage(t, store, post.Ref, "hi")
	before, err := store.LatestRevision(ctx)
	if err != nil {
		t.Fatal(err)
	}

	subject := "taken"
	if _, err := store.Edit(ctx, everyAccess(store),
		store.mustKey(post.Ref), api.EditPostRequest{
			Subject: &subject, User: "heron"}); err == nil {
		t.Fatal("another user edited the post")
	}
	if _, _, err := store.EditMessage(ctx, everyAccess(store),
		store.mustKey(post.Ref),
		message.Number, api.EditMessageRequest{
			Body: "taken", User: "heron"}); err == nil {
		t.Fatal("another user edited the message")
	}
	after, err := store.LatestRevision(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("refused edits moved the latest revision from %d to %d",
			before, after)
	}
}

func TestChangesReadDuringWritesMissNone(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	post := mustAdd(t, store, "busy", "", nil)
	const messages = 40
	var wg sync.WaitGroup
	for range messages {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := store.PostMessage(ctx, everyAccess(store),
				store.mustKey(post.Ref),
				api.PostMessageRequest{Body: "hi"}); err != nil {
				t.Errorf("posting: %v", err)
			}
		}()
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	read := map[string]int{}
	var cursor int64
	deadline := time.Now().Add(time.Minute)
	for finished := false; !finished; {
		select {
		case <-done:
			finished = true
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("the messages were not all sent in a minute, "+
				"after %d changes read", len(read))
		}
		var ids []string
		ids, cursor = readAllChanges(t, store, cursor, 3)
		for _, id := range ids {
			read[id]++
		}
	}
	if len(read) != messages+1 {
		t.Fatalf("read %d changes, want %d: %v", len(read), messages+1, read)
	}
	for id, count := range read {
		if count != 1 {
			t.Fatalf("read %s %d times", id, count)
		}
	}
}

func TestChangesStayInTheirWorkspace(t *testing.T) {
	store := openTestStore(t)
	open := mustAdd(t, store, "open", "", nil)
	private := addTo(t, store, "personal", api.AddPostRequest{
		Subject: "PRIVATEMARK",
		Links:   []api.Link{link("see", open.Ref)},
	})
	mustPostMessage(t, store, private.Ref, "PRIVATEMARK")
	if _, err := store.Edit(context.Background(), everyAccess(store),
		store.mustKey(open.Ref), api.EditPostRequest{
			AddLinks: []api.Link{link("see", private.Ref)},
			User:     testUser,
		}); err != nil {
		t.Fatal(err)
	}

	page, err := store.Changes(context.Background(),
		accessTo(t, store, "general"), workspaceID(t, store, "general"),
		0, Filter{Items: api.PostsAndChat}, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if got := changeIDs(page); !reflect.DeepEqual(got, []string{"e1"}) {
		t.Fatalf("read %v, want e1 alone", got)
	}
	if text := fmt.Sprintf("%+v", page); strings.Contains(text,
		"PRIVATEMARK") {
		t.Fatalf("the open workspace's changes show %s", text)
	}
}

// readAllMatching pages through every change after since that matches
// a filter, leaving out exclude's, limit at a time and checking span
// revisions a read, and returns the changes and the final cursor.
func readAllMatching(
	t *testing.T,
	store *Store,
	since int64,
	filter Filter,
	exclude *OriginKey,
	limit int,
	span int64,
) ([]string, int64) {
	t.Helper()
	ids := []string{}
	for {
		page, err := store.changes(context.Background(), everyAccess(store),
			workspaceID(t, store, "general"), since, filter, exclude, limit,
			span)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, changeIDs(page)...)
		since = page.Cursor
		if !page.More {
			return ids, since
		}
	}
}

func latestRevision(t *testing.T, store *Store) int64 {
	t.Helper()
	latest, err := store.LatestRevision(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return latest
}

// retime sets the time of every change to at of its revision, in every
// column that holds one, so a test places changes in time while the
// times follow the revisions, as the clock gives them.
func retime(t *testing.T, store *Store, at func(revision int64) time.Time) {
	t.Helper()
	times := make([]string, latestRevision(t, store)+1)
	for revision := range times {
		times[revision] = formatTime(at(int64(revision)))
	}
	encoded, err := json.Marshal(times)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{`
		UPDATE posts SET
			created_utc = json_extract(?1, '$[' || posted_revision || ']'),
			modified_utc = json_extract(?1, '$[' || revision || ']'),
			activity_utc = json_extract(?1, '$[' || activity_revision || ']')`,
		`
		UPDATE post_messages SET
			created_utc = json_extract(?1, '$[' || posted_revision || ']'),
			modified_utc = json_extract(?1, '$[' || revision || ']')`,
	} {
		if _, err := store.db.Exec(statement, string(encoded)); err != nil {
			t.Fatal(err)
		}
	}
}

// timeSpan is the times from After, inclusive, to Before, exclusive. A
// zero time leaves that side open.
type timeSpan struct {
	After  time.Time
	Before time.Time
}

// revisions narrows a span of revisions to what a read of a workspace
// ranks within a span of times, as the server resolves a time bound.
func (s timeSpan) revisions(
	t *testing.T, store *Store, workspace int64, span RevisionSpan,
) RevisionSpan {
	t.Helper()
	lastBefore := func(at time.Time) int64 {
		revision, err := store.LastRevisionBefore(context.Background(),
			everyAccess(store), workspace, at)
		if err != nil {
			t.Fatal(err)
		}
		return revision
	}
	if !s.After.IsZero() {
		span.After = max(span.After, lastBefore(s.After))
	}
	if !s.Before.IsZero() {
		before := lastBefore(s.Before) + 1
		if span.Before == 0 || before < span.Before {
			span.Before = before
		}
	}
	return span
}

func TestFilteredChangesShowEachMatchOnceAtAnyLimitAndSpan(t *testing.T) {
	store := openTestStore(t)
	lead := api.Origin{User: testUser, ClaudeSession: "lead"}
	bare := api.Origin{User: testUser}
	heron := api.Origin{User: "heron", ClaudeSession: "lead"}
	pond := addTo(t, store, "general", api.AddPostRequest{
		Project: "pond", Subject: "pond", Origin: lead})
	addTo(t, store, "personal", api.AddPostRequest{Subject: "elsewhere"})
	lake := addTo(t, store, "general", api.AddPostRequest{
		Project: "lake", Subject: "lake", Origin: bare})
	postFrom(t, store, pond.Ref, "quack", lead)
	postFrom(t, store, lake.Ref, "quack", heron)
	postFrom(t, store, pond.Ref, "honk", bare)
	addTo(t, store, "general", api.AddPostRequest{
		Project: "pond", Subject: "reeds", Origin: heron})
	// An edit stores no origin, so the post keeps its creator's key.
	subject := "pond, edited"
	if _, err := store.Edit(context.Background(), everyAccess(store),
		store.mustKey(pond.Ref), api.EditPostRequest{
			Subject: &subject, User: testUser}); err != nil {
		t.Fatal(err)
	}
	latest := latestRevision(t, store)

	for _, test := range []struct {
		name    string
		filter  Filter
		exclude *OriginKey
		want    []string
	}{
		{"everything", Filter{}, nil,
			[]string{"e2", "e1-m1", "e2-m1", "e1-m2", "e3", "e1"}},
		{"a project", Filter{Project: []FieldCond{{Value: "pond"}}}, nil,
			[]string{"e1-m1", "e1-m2", "e3", "e1"}},
		{"text, which a message matches by its own", Filter{
			Content: []FieldCond{{Value: "QUACK"}}}, nil,
			[]string{"e1-m1", "e2-m1"}},
		{"an origin, which a message matches by its own", Filter{
			OriginUser: []FieldCond{{Value: "heron"}}}, nil,
			[]string{"e2-m1", "e3"}},
		{"text and origin, which must match the same message", Filter{
			Content:    []FieldCond{{Value: "QUACK"}},
			OriginUser: []FieldCond{{Value: testUser}}}, nil,
			[]string{"e1-m1"}},
		{"the lead session's", Filter{},
			&OriginKey{User: testUser, ClaudeSession: "lead"},
			[]string{"e2", "e2-m1", "e1-m2", "e3"}},
		{"no session's", Filter{}, &OriginKey{User: testUser},
			[]string{"e1-m1", "e2-m1", "e3", "e1"}},
		{"nothing", Filter{Project: []FieldCond{{Value: "sea"}}}, nil,
			[]string{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, items := range []api.Items{api.PostsAndChat,
				api.PostsOnly, api.ChatOnly} {
				want := slices.DeleteFunc(slices.Clone(test.want),
					func(id string) bool {
						isMessage := strings.Contains(id, "-m")
						return isMessage && !items.HasChat() ||
							!isMessage && !items.HasPosts()
					})
				filter := test.filter
				filter.Items = items
				for limit := 1; limit <= 3; limit++ {
					for span := int64(1); span <= latest+1; span++ {
						got, cursor := readAllMatching(t, store, 0,
							filter, test.exclude, limit, span)
						if !reflect.DeepEqual(got, want) {
							t.Fatalf("%v, limit %d span %d read %v, "+
								"want %v", items, limit, span, got, want)
						}
						if cursor != latest {
							t.Fatalf("%v, limit %d span %d ended at %d, "+
								"want the latest revision %d", items, limit,
								span, cursor, latest)
						}
					}
				}
			}
		})
	}
}

func TestChangesCursorPassesWhatDoesNotMatch(t *testing.T) {
	store := openTestStore(t)
	duck := Filter{Items: api.PostsAndChat,
		Project: []FieldCond{{Value: "duck"}}}
	post := addTo(t, store, "general", api.AddPostRequest{
		Project: "heron", Subject: "not a duck"})
	mustPostMessage(t, store, post.Ref, "still not")
	latest := latestRevision(t, store)

	page, err := store.Changes(context.Background(), everyAccess(store),
		workspaceID(t, store, "general"), 0, duck, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Changes) != 0 || page.Cursor != latest || page.More {
		t.Fatalf("read %+v, want no changes and cursor %d", page, latest)
	}

	addTo(t, store, "general", api.AddPostRequest{
		Project: "duck", Subject: "a duck"})
	got, _ := readAllMatching(t, store, page.Cursor, duck, nil, 10,
		revisionsPerRead)
	if !reflect.DeepEqual(got, []string{"e2"}) {
		t.Fatalf("read on %v, want e2 alone", got)
	}
}

func TestChangesCursorAtAFullPageIsItsLastMatch(t *testing.T) {
	store := openTestStore(t)
	duck := Filter{Items: api.PostsAndChat,
		Project: []FieldCond{{Value: "duck"}}}
	addTo(t, store, "general", api.AddPostRequest{
		Project: "duck", Subject: "first"})
	first := latestRevision(t, store)
	addTo(t, store, "general", api.AddPostRequest{
		Project: "heron", Subject: "between"})
	addTo(t, store, "general", api.AddPostRequest{
		Project: "duck", Subject: "second"})

	page, err := store.Changes(context.Background(), everyAccess(store),
		workspaceID(t, store, "general"), 0, duck, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got := changeIDs(page); !reflect.DeepEqual(got, []string{"e1"}) ||
		page.Cursor != first || !page.More {
		t.Fatalf("read %v to %d, more %t, want e1 to %d with more", got,
			page.Cursor, page.More, first)
	}
}

func TestExcludedOriginKeysMatchExactly(t *testing.T) {
	store := openTestStore(t)
	origins := []api.Origin{
		{User: testUser, ClaudeSession: "lead"},
		{User: testUser},
		{User: "heron"},
		{User: "Duck", ClaudeSession: "lead"},
		{User: testUser, ClaudeSession: "Lead"},
	}
	post := addTo(t, store, "general", api.AddPostRequest{
		Subject: "chat", Origin: api.Origin{User: "nobody"}})
	for i, origin := range origins {
		postFrom(t, store, post.Ref, fmt.Sprint(i+1), origin)
	}

	for _, test := range []struct {
		exclude OriginKey
		want    []string
	}{
		{OriginKey{User: testUser, ClaudeSession: "lead"},
			[]string{"e1", "e1-m2", "e1-m3", "e1-m4", "e1-m5"}},
		{OriginKey{User: testUser},
			[]string{"e1", "e1-m1", "e1-m3", "e1-m4", "e1-m5"}},
		{OriginKey{User: "heron", ClaudeSession: "lead"},
			[]string{"e1", "e1-m1", "e1-m2", "e1-m3", "e1-m4", "e1-m5"}},
		{OriginKey{},
			[]string{"e1", "e1-m1", "e1-m2", "e1-m3", "e1-m4", "e1-m5"}},
	} {
		got, _ := readAllMatching(t, store, 0,
			Filter{Items: api.PostsAndChat}, &test.exclude, 10,
			revisionsPerRead)
		if !reflect.DeepEqual(got, test.want) {
			t.Fatalf("excluding %+v read %v, want %v", test.exclude, got,
				test.want)
		}
	}
}

// Changes under a post keep to its subtree, and changes not under a
// post leave out its subtree and the chats in it.
func TestChangesUnderAPostKeepToItsSubtree(t *testing.T) {
	store := openTestStore(t)
	root := mustAdd(t, store, "root", "", nil)
	child := addTo(t, store, "general", api.AddPostRequest{
		Subject: "child", Links: []api.Link{link("parent", root.Ref)}})
	grandchild := addTo(t, store, "general", api.AddPostRequest{
		Subject: "grandchild", Links: []api.Link{link("parent", child.Ref)}})
	other := mustAdd(t, store, "other", "", nil)
	mustPostMessage(t, store, grandchild.Ref, "deep")
	mustPostMessage(t, store, other.Ref, "aside")

	for _, test := range []struct {
		under    api.Ref
		notUnder []api.Ref
		want     []string
	}{
		{root.Ref, nil, []string{"e1", "e2", "e3", "e3-m1"}},
		{child.Ref, nil, []string{"e2", "e3", "e3-m1"}},
		{other.Ref, nil, []string{"e4", "e4-m1"}},
		{api.Ref{}, []api.Ref{child.Ref}, []string{"e1", "e4", "e4-m1"}},
		{api.Ref{}, []api.Ref{child.Ref, other.Ref}, []string{"e1"}},
		{root.Ref, []api.Ref{child.Ref}, []string{"e1"}},
		{root.Ref, []api.Ref{grandchild.Ref}, []string{"e1", "e2"}},
	} {
		filter := Filter{Items: api.PostsAndChat}
		if test.under.ID != 0 {
			under := store.mustKey(test.under)
			filter.Under = &under
		}
		for _, ref := range test.notUnder {
			filter.NotUnder = append(filter.NotUnder, store.mustKey(ref))
		}
		got, _ := readAllMatching(t, store, 0, filter, nil, 10,
			revisionsPerRead)
		if !reflect.DeepEqual(got, test.want) {
			t.Fatalf("under %s and not under %v read %v, want %v",
				test.under, test.notUnder, got, test.want)
		}
	}
}

// A read that checks a few revisions at a time, while others send,
// still shows every message that matches exactly once.
func TestFilteredChangesReadDuringWritesMissNone(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	post := mustAdd(t, store, "busy", "", nil)
	mine := api.Origin{User: testUser, ClaudeSession: "mine"}
	theirs := api.Origin{User: testUser, ClaudeSession: "theirs"}
	start := latestRevision(t, store)
	const messages = 40
	var wg sync.WaitGroup
	for i := range messages {
		origin := mine
		if i%2 == 0 {
			origin = theirs
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := store.PostMessage(ctx, everyAccess(store),
				store.mustKey(post.Ref),
				api.PostMessageRequest{Body: "hi", Origin: origin}); err != nil {
				t.Errorf("posting: %v", err)
			}
		}()
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	exclude := &OriginKey{User: mine.User, ClaudeSession: mine.ClaudeSession}
	read := map[string]int{}
	cursor := start
	deadline := time.Now().Add(time.Minute)
	for finished := false; !finished; {
		select {
		case <-done:
			finished = true
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("the messages were not all sent in a minute, "+
				"after %d changes read", len(read))
		}
		var ids []string
		ids, cursor = readAllMatching(t, store, cursor,
			Filter{Items: api.PostsAndChat}, exclude, 2, 3)
		for _, id := range ids {
			read[id]++
		}
	}

	page, err := store.Messages(ctx, everyAccess(store),
		store.mustKey(post.Ref),
		0, Filter{}, exclude, messages)
	if err != nil {
		t.Fatal(err)
	}
	if len(read) != messages/2 || len(page.Messages) != messages/2 {
		t.Fatalf("read %d changes, want %d: %v", len(read), messages/2, read)
	}
	for _, message := range page.Messages {
		if read[message.Ref().String()] != 1 {
			t.Fatalf("read %s %d times", message.Ref(),
				read[message.Ref().String()])
		}
	}
}

// A read gives the latest revision it saw, and a write the revision of
// its change, so the changes after either are exactly those that came
// after it.
func TestReadsAndWritesGiveARevisionToReadChangesAfter(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	pond := mustAdd(t, store, "pond", "", nil)
	first := mustPostMessage(t, store, pond.Ref, "hi")
	if pond.Revision != 1 || first.Revision != 2 {
		t.Fatalf("the writes gave revisions %d and %d, want 1 and 2",
			pond.Revision, first.Revision)
	}

	general := workspaceID(t, store, "general")
	key := store.mustKey(pond.Ref)
	listing, err := store.Search(ctx, everyAccess(store), general,
		Filter{Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	chat, err := store.Messages(ctx, everyAccess(store), key, 0, Filter{},
		nil, 20)
	if err != nil {
		t.Fatal(err)
	}
	latest, err := store.LatestMessages(ctx, everyAccess(store), key,
		0, Filter{}, 20)
	if err != nil {
		t.Fatal(err)
	}
	overview, err := store.Overview(ctx, everyAccess(store), general, Filter{},
		20)
	if err != nil {
		t.Fatal(err)
	}
	for name, revision := range map[string]int64{
		"a listing": listing.Revision, "a chat read": chat.Revision,
		"the newest messages": latest.Revision,
		"an overview":         overview.Revision,
	} {
		if revision != 2 {
			t.Errorf("%s gave revision %d, want 2", name, revision)
		}
	}

	second := mustPostMessage(t, store, pond.Ref, "again")
	got := changeIDs(readChanges(t, store, listing.Revision, 10))
	if want := []string{second.Ref().String()}; !reflect.DeepEqual(got,
		want) {
		t.Fatalf("the changes after the reads are %v, want %v", got, want)
	}
}

// postedRevision reads the revision that added a post, or sent its
// message number when that is not 0.
func postedRevision(
	t *testing.T, store *Store, post api.Ref, number int64,
) int64 {
	t.Helper()
	revision, err := store.PostedRevision(context.Background(),
		everyAccess(store), store.mustKey(post), number)
	if err != nil {
		t.Fatal(err)
	}
	return revision
}

// A reader who saw a post or message in any state saw its adding, so
// reading changes from there shows every change after it, those to
// what came before it included, even once it has been edited or
// deleted.
func TestChangesAfterAPostedRevisionMissNothingAfterIt(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	earlier := mustAdd(t, store, "earlier", "", nil)
	old := mustPostMessage(t, store, earlier.Ref, "old")
	post := mustAdd(t, store, "seen", "", nil)
	message := mustPostMessage(t, store, post.Ref, "seen")

	other := mustAdd(t, store, "other", "", nil)
	mustPostMessage(t, store, other.Ref, "elsewhere")
	if _, _, err := store.EditMessage(ctx, everyAccess(store),
		store.mustKey(earlier.Ref),
		old.Number, ownEdit("old, edited")); err != nil {
		t.Fatal(err)
	}
	mustDeleteMessage(t, store, message.Ref())
	mustDelete(t, store, post.Ref)

	if got := postedRevision(t, store, post.Ref, 0); got != post.Revision {
		t.Fatalf("e2 was added at @%d, but reads as @%d", post.Revision, got)
	}
	got, _ := readAllChanges(t, store,
		postedRevision(t, store, post.Ref, message.Number), 10)
	want := []string{"e3", "e3-m1", "e1-m1", "e2-m1", "e2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("changes after e2-m1 = %v, want %v", got, want)
	}
}

func TestPostedRevisionOfAMissingMessageIsNotFound(t *testing.T) {
	store := openTestStore(t)
	post := mustAdd(t, store, "post", "", nil)
	_, err := store.PostedRevision(context.Background(), everyAccess(store),
		store.mustKey(post.Ref), 1)
	if !errors.Is(err, ErrMessageNotFound) {
		t.Fatalf("e1-m1 gave %v", err)
	}
	_, err = store.PostedRevision(context.Background(), everyAccess(store),
		Key{Workspace: workspaceID(t, store, "general"), ID: 2}, 0)
	if !errors.Is(err, ErrPostNotFound) {
		t.Fatalf("e2 gave %v", err)
	}
}

// The last revision before a time is the last in the workspace that a
// read ranks by made before that time, whatever the other workspace
// changed between them, and when several changes share a time. A read
// ranks by the changes to posts and messages, the latest activity in a
// post, or the posting of a message.
func TestLastRevisionBeforeIsTheLastRankedRevisionBeforeATime(
	t *testing.T,
) {
	store := openTestStore(t)
	writer := newForestWriter(t, store, rand.New(rand.NewSource(1)))
	for range 300 {
		writer.write()
	}
	// Three revisions share each minute, so the times follow the
	// revisions as the clock gives them, ties included.
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	at := func(revision int64) time.Time {
		return start.Add(time.Duration(revision/3) * time.Minute)
	}
	retime(t, store, at)

	general := workspaceID(t, store, "general")
	var changed []int64
	rows, err := store.db.Query(`
		SELECT revision FROM posts WHERE workspace_id = ?1
		UNION ALL
		SELECT activity_revision FROM posts WHERE workspace_id = ?1
		UNION ALL
		SELECT revision FROM post_messages WHERE workspace_id = ?1
		UNION ALL
		SELECT posted_revision FROM post_messages WHERE workspace_id = ?1`,
		general)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var revision int64
		if err := rows.Scan(&revision); err != nil {
			t.Fatal(err)
		}
		changed = append(changed, revision)
	}
	rows.Close()

	latest := latestRevision(t, store)
	for revision := int64(0); revision <= latest+3; revision++ {
		for _, when := range []time.Time{at(revision),
			at(revision).Add(30 * time.Second)} {
			want := int64(0)
			for _, other := range changed {
				if at(other).Before(when) {
					want = max(want, other)
				}
			}
			got, err := store.LastRevisionBefore(context.Background(),
				everyAccess(store), general, when)
			if err != nil || got != want {
				t.Fatalf("the last revision before %v = %d, %v, want %d",
					when, got, err, want)
			}
		}
	}
}
