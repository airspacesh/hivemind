package store

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/airspacesh/workstream/internal/api"
)

func readChanges(
	t *testing.T, store *Store, since api.ChangeCursor, limit int,
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
	t *testing.T, store *Store, since api.ChangeCursor, limit int,
) ([]string, api.ChangeCursor) {
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

// stampAll gives every entry and message the same modified time.
func stampAll(t *testing.T, store *Store) {
	t.Helper()
	const stamp = "2026-10-10T01:02:03.000000000Z"
	for _, table := range []string{"entries", "entry_messages"} {
		if _, err := store.db.Exec(
			`UPDATE `+table+` SET modified_utc = ?`, stamp,
		); err != nil {
			t.Fatal(err)
		}
	}
}

func TestChangesSharingAStampPageOnceEachInOrder(t *testing.T) {
	store := openTestStore(t)
	first := mustAdd(t, store, "first", "", nil)
	second := mustAdd(t, store, "second", "", nil)
	mustPost(t, store, second.Ref, "hi")
	mustPost(t, store, first.Ref, "hi")
	mustPost(t, store, first.Ref, "again")
	mustAdd(t, store, "third", "", nil)
	stampAll(t, store)

	want := []string{"e1", "e1-m1", "e1-m2", "e2", "e2-m1", "e3"}
	for limit := 1; limit <= len(want); limit++ {
		got, _ := readAllChanges(t, store, api.ChangeCursor{}, limit)
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
	_, cursor := readAllChanges(t, store, api.ChangeCursor{}, 10)

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

func TestLatestChangeSkipsEveryChangeSoFar(t *testing.T) {
	store := openTestStore(t)
	entry := mustAdd(t, store, "first", "", nil)
	mustPost(t, store, entry.Ref, "hi")
	mustAdd(t, store, "second", "", nil)
	stampAll(t, store)
	latest, err := store.LatestChange(context.Background(),
		workspaceID(t, store, "default"))
	if err != nil {
		t.Fatal(err)
	}
	if got := changeIDs(readChanges(t, store, latest, 10)); len(got) != 0 {
		t.Fatalf("after the latest change read %v", got)
	}

	mustPost(t, store, entry.Ref, "later")
	if got := changeIDs(readChanges(t, store, latest, 10)); !reflect.DeepEqual(
		got, []string{"e1-m2"}) {
		t.Fatalf("read %v, want the later message alone", got)
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
		api.ChangeCursor{}, 10)
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
