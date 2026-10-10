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
		workspaceID(t, store, "default"), since, limit)
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
		accessTo(t, store, "default"), workspaceID(t, store, "default"),
		0, 10)
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
