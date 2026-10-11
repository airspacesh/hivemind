package store

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/airspacesh/workstream/internal/api"
)

func readChanges(
	t *testing.T, store *Store, since int64, limit int,
) api.ChangesPage {
	t.Helper()
	page, err := store.Changes(context.Background(), everyAccess(store),
		workspaceID(t, store, "general"), since, Filter{}, nil, limit)
	if err != nil {
		t.Fatal(err)
	}
	return page
}

// changeIDs names each change in a page by the entry or message that
// changed.
func changeIDs(page api.ChangesPage) []string {
	ids := []string{}
	for _, change := range page.Changes {
		if change.Message != nil {
			ids = append(ids, change.Message.Ref().String())
		} else {
			ids = append(ids, change.Entry.Ref.String())
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
	mustPost(t, store, second.Ref, "hi")
	mustPost(t, store, first.Ref, "hi")
	mustPost(t, store, first.Ref, "again")
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
	message := mustPost(t, store, seen.Ref, "hi")
	_, cursor := readAllChanges(t, store, 0, 10)

	unseen := mustAdd(t, store, "unseen", "", nil)
	for _, entry := range []api.Entry{unseen, seen, unseen} {
		subject := "edited"
		if _, err := store.Edit(ctx, everyAccess(store),
			store.mustKey(entry.Ref),
			api.EditEntryRequest{Subject: &subject, User: testUser},
		); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := store.EditMessage(ctx, store.mustKey(seen.Ref),
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
		store.mustKey(child.Ref), api.EditEntryRequest{
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
	entry := mustAdd(t, store, "first", "", nil)
	mustPost(t, store, entry.Ref, "hi")
	addTo(t, store, "personal", api.AddEntryRequest{Subject: "private"})
	latest, err := store.LatestRevision(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := changeIDs(readChanges(t, store, latest, 10)); len(got) != 0 {
		t.Fatalf("after the latest revision read %v", got)
	}

	mustPost(t, store, entry.Ref, "later")
	if got := changeIDs(readChanges(t, store, latest, 10)); !reflect.DeepEqual(
		got, []string{"e1-m2"}) {
		t.Fatalf("read %v, want the later message alone", got)
	}
}

func TestRefusedEditsTakeNoRevision(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	entry := mustAdd(t, store, "mine", "", nil)
	message := mustPost(t, store, entry.Ref, "hi")
	before, err := store.LatestRevision(ctx)
	if err != nil {
		t.Fatal(err)
	}

	subject := "taken"
	if _, err := store.Edit(ctx, everyAccess(store),
		store.mustKey(entry.Ref), api.EditEntryRequest{
			Subject: &subject, User: "heron"}); err == nil {
		t.Fatal("another user edited the entry")
	}
	if _, _, err := store.EditMessage(ctx, store.mustKey(entry.Ref),
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
	entry := mustAdd(t, store, "busy", "", nil)
	const posts = 40
	var wg sync.WaitGroup
	for range posts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := store.Post(ctx, store.mustKey(entry.Ref),
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
			t.Fatalf("the posts did not finish in a minute, after %d "+
				"changes read", len(read))
		}
		var ids []string
		ids, cursor = readAllChanges(t, store, cursor, 3)
		for _, id := range ids {
			read[id]++
		}
	}
	if len(read) != posts+1 {
		t.Fatalf("read %d changes, want %d: %v", len(read), posts+1, read)
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
	private := addTo(t, store, "personal", api.AddEntryRequest{
		Subject: "PRIVATEMARK",
		Links:   []api.Link{link("see", open.Ref)},
	})
	mustPost(t, store, private.Ref, "PRIVATEMARK")
	if _, err := store.Edit(context.Background(), everyAccess(store),
		store.mustKey(open.Ref), api.EditEntryRequest{
			AddLinks: []api.Link{link("see", private.Ref)},
			User:     testUser,
		}); err != nil {
		t.Fatal(err)
	}

	page, err := store.Changes(context.Background(),
		accessTo(t, store, "general"), workspaceID(t, store, "general"),
		0, Filter{}, nil, 10)
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

// postFrom posts a message with the given origin.
func postFrom(
	t *testing.T, store *Store, entry api.Ref, body string, origin api.Origin,
) {
	t.Helper()
	if _, err := store.Post(context.Background(), store.mustKey(entry),
		api.PostMessageRequest{Body: body, Origin: origin}); err != nil {
		t.Fatal(err)
	}
}

func TestFilteredChangesShowEachMatchOnceAtAnyLimitAndSpan(t *testing.T) {
	store := openTestStore(t)
	lead := api.Origin{User: testUser, ClaudeSession: "lead"}
	bare := api.Origin{User: testUser}
	heron := api.Origin{User: "heron", ClaudeSession: "lead"}
	pond := addTo(t, store, "general", api.AddEntryRequest{
		Project: "pond", Subject: "pond", Origin: lead})
	addTo(t, store, "personal", api.AddEntryRequest{Subject: "elsewhere"})
	lake := addTo(t, store, "general", api.AddEntryRequest{
		Project: "lake", Subject: "lake", Origin: bare})
	postFrom(t, store, pond.Ref, "quack", lead)
	postFrom(t, store, lake.Ref, "quack", heron)
	postFrom(t, store, pond.Ref, "honk", bare)
	addTo(t, store, "general", api.AddEntryRequest{
		Project: "pond", Subject: "reeds", Origin: heron})
	// An edit stores no origin, so the entry keeps its creator's key.
	subject := "pond, edited"
	if _, err := store.Edit(context.Background(), everyAccess(store),
		store.mustKey(pond.Ref), api.EditEntryRequest{
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
			for limit := 1; limit <= 3; limit++ {
				for span := int64(1); span <= latest+1; span++ {
					got, cursor := readAllMatching(t, store, 0,
						test.filter, test.exclude, limit, span)
					if !reflect.DeepEqual(got, test.want) {
						t.Fatalf("limit %d span %d read %v, want %v",
							limit, span, got, test.want)
					}
					if cursor != latest {
						t.Fatalf("limit %d span %d ended at %d, want the "+
							"latest revision %d", limit, span, cursor, latest)
					}
				}
			}
		})
	}
}

func TestChangesCursorPassesWhatDoesNotMatch(t *testing.T) {
	store := openTestStore(t)
	duck := Filter{Project: []FieldCond{{Value: "duck"}}}
	entry := addTo(t, store, "general", api.AddEntryRequest{
		Project: "heron", Subject: "not a duck"})
	mustPost(t, store, entry.Ref, "still not")
	latest := latestRevision(t, store)

	page, err := store.Changes(context.Background(), everyAccess(store),
		workspaceID(t, store, "general"), 0, duck, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Changes) != 0 || page.Cursor != latest || page.More {
		t.Fatalf("read %+v, want no changes and cursor %d", page, latest)
	}

	addTo(t, store, "general", api.AddEntryRequest{
		Project: "duck", Subject: "a duck"})
	got, _ := readAllMatching(t, store, page.Cursor, duck, nil, 10,
		revisionsPerRead)
	if !reflect.DeepEqual(got, []string{"e2"}) {
		t.Fatalf("read on %v, want e2 alone", got)
	}
}

func TestChangesCursorAtAFullPageIsItsLastMatch(t *testing.T) {
	store := openTestStore(t)
	duck := Filter{Project: []FieldCond{{Value: "duck"}}}
	addTo(t, store, "general", api.AddEntryRequest{
		Project: "duck", Subject: "first"})
	first := latestRevision(t, store)
	addTo(t, store, "general", api.AddEntryRequest{
		Project: "heron", Subject: "between"})
	addTo(t, store, "general", api.AddEntryRequest{
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
	entry := addTo(t, store, "general", api.AddEntryRequest{
		Subject: "chat", Origin: api.Origin{User: "nobody"}})
	for i, origin := range origins {
		postFrom(t, store, entry.Ref, fmt.Sprint(i+1), origin)
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
		got, _ := readAllMatching(t, store, 0, Filter{}, &test.exclude, 10,
			revisionsPerRead)
		if !reflect.DeepEqual(got, test.want) {
			t.Fatalf("excluding %+v read %v, want %v", test.exclude, got,
				test.want)
		}
	}
}

func TestChangesUnderAnEntryKeepToItsSubtree(t *testing.T) {
	store := openTestStore(t)
	root := mustAdd(t, store, "root", "", nil)
	child := addTo(t, store, "general", api.AddEntryRequest{
		Subject: "child", Links: []api.Link{link("parent", root.Ref)}})
	grandchild := addTo(t, store, "general", api.AddEntryRequest{
		Subject: "grandchild", Links: []api.Link{link("parent", child.Ref)}})
	other := mustAdd(t, store, "other", "", nil)
	mustPost(t, store, grandchild.Ref, "deep")
	mustPost(t, store, other.Ref, "aside")

	for _, test := range []struct {
		under api.Ref
		want  []string
	}{
		{root.Ref, []string{"e1", "e2", "e3", "e3-m1"}},
		{child.Ref, []string{"e2", "e3", "e3-m1"}},
		{other.Ref, []string{"e4", "e4-m1"}},
	} {
		under := store.mustKey(test.under)
		got, _ := readAllMatching(t, store, 0, Filter{Under: &under}, nil,
			10, revisionsPerRead)
		if !reflect.DeepEqual(got, test.want) {
			t.Fatalf("under %s read %v, want %v", test.under, got, test.want)
		}
	}
}

// A read that checks a few revisions at a time, while others post,
// still shows every message that matches exactly once.
func TestFilteredChangesReadDuringWritesMissNone(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	entry := mustAdd(t, store, "busy", "", nil)
	mine := api.Origin{User: testUser, ClaudeSession: "mine"}
	theirs := api.Origin{User: testUser, ClaudeSession: "theirs"}
	start := latestRevision(t, store)
	const posts = 40
	var wg sync.WaitGroup
	for i := range posts {
		origin := mine
		if i%2 == 0 {
			origin = theirs
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := store.Post(ctx, store.mustKey(entry.Ref),
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
			t.Fatalf("the posts did not finish in a minute, after %d "+
				"changes read", len(read))
		}
		var ids []string
		ids, cursor = readAllMatching(t, store, cursor, Filter{}, exclude, 2,
			3)
		for _, id := range ids {
			read[id]++
		}
	}

	page, err := store.Messages(ctx, store.mustKey(entry.Ref),
		api.ChatBound{}, Filter{}, exclude, posts)
	if err != nil {
		t.Fatal(err)
	}
	if len(read) != posts/2 || len(page.Messages) != posts/2 {
		t.Fatalf("read %d changes, want %d: %v", len(read), posts/2, read)
	}
	for _, message := range page.Messages {
		if read[message.Ref().String()] != 1 {
			t.Fatalf("read %s %d times", message.Ref(),
				read[message.Ref().String()])
		}
	}
}
