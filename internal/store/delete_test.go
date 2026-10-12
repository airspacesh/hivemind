package store

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/airspacesh/workstream/internal/api"
)

func mustDelete(t *testing.T, store *Store, ref api.Ref) api.Post {
	t.Helper()
	post, err := store.Delete(context.Background(), everyAccess(store),
		store.mustKey(ref), api.DeleteRequest{User: testUser})
	if err != nil {
		t.Fatalf("deleting %s: %v", ref, err)
	}
	return post
}

func mustDeleteMessage(
	t *testing.T, store *Store, ref api.MessageRef,
) api.Message {
	t.Helper()
	message, err := store.DeleteMessage(context.Background(), everyAccess(store),
		store.mustKey(ref.Post), ref.Number,
		api.DeleteRequest{User: testUser})
	if err != nil {
		t.Fatalf("deleting %s: %v", ref, err)
	}
	return message
}

func TestDeleteClearsAPostButKeepsItsPlace(t *testing.T) {
	store := openTestStore(t)
	parent := mustAdd(t, store, "parent", "", nil)
	old := mustAdd(t, store, "old", "", nil)
	post := addTo(t, store, "general", api.AddPostRequest{
		Project: "pond", Subject: "secret plan", Body: "the details",
		Metadata: map[string]string{"jira": "QUACK-1", "pr": "12"},
		Origin: api.Origin{User: testUser, Host: "nest", Dir: "/pond",
			ClaudeSession: "s1"},
		Links: []api.Link{link(api.ParentLink, parent.Ref),
			link(api.SupersedesLink, old.Ref), link("see", parent.Ref)},
	})
	child := addChild(t, store, "child", post.Ref)
	message := mustPostMessage(t, store, post.Ref, "hi")
	cursor := readChanges(t, store, 0, 100).Cursor

	deleted := mustDelete(t, store, post.Ref)
	if !deleted.Deleted || deleted.Subject != "" || deleted.Body != "" ||
		len(deleted.Metadata) != 0 {
		t.Fatalf("deleted post keeps its text: %+v", deleted)
	}
	if deleted.Project != post.Project || deleted.Origin != post.Origin ||
		!deleted.Created.Equal(post.Created) ||
		!reflect.DeepEqual(deleted.Links, post.Links) {
		t.Fatalf("delete changed more than the text:\n%+v\nwas\n%+v",
			deleted, post)
	}
	if !deleted.Modified.After(post.Modified) {
		t.Fatalf("delete left the modified time at %v", deleted.Modified)
	}
	detail := mustGet(t, store, post.Ref)
	if detail.Children != 1 || detail.Messages != 1 {
		t.Fatalf("deleted post has %d children and %d messages, want 1 "+
			"and 1", detail.Children, detail.Messages)
	}
	got := readTree(t, store, child.Ref, false, false, 10).Ancestors
	if len(got) != 2 || got[1].Ref != post.Ref || !got[1].Deleted {
		t.Fatalf("child's path is %+v, want it under the deleted post",
			got)
	}
	if got := readChat(t, store, post.Ref, 0, 10); chatText(got) !=
		message.Ref().String()+":hi cursor=1 remaining=0" {
		t.Fatalf("deleted post's chat is %s", chatText(got))
	}

	page := readChanges(t, store, cursor, 100)
	if got := changeIDs(page); !reflect.DeepEqual(got, []string{"e3"}) {
		t.Fatalf("changes after the delete are %v, want e3", got)
	}
	again := mustDelete(t, store, post.Ref)
	if !again.Modified.Equal(deleted.Modified) {
		t.Fatalf("deleting again moved the modified time")
	}
	if got := changeIDs(readChanges(t, store, page.Cursor, 100)); len(got) !=
		0 {
		t.Fatalf("deleting again made changes %v", got)
	}
}

func TestDeleteMessageKeepsItsPlace(t *testing.T) {
	store := openTestStore(t)
	post := mustAdd(t, store, "topic", "", nil)
	mustPostMessage(t, store, post.Ref, "first")
	message, _, err := store.PostMessage(context.Background(), everyAccess(store),
		store.mustKey(post.Ref), api.PostMessageRequest{Body: "secret",
			Origin: api.Origin{User: testUser, Host: "nest", Dir: "/pond",
				ClaudeSession: "s1"}})
	if err != nil {
		t.Fatal(err)
	}
	mustPostMessage(t, store, post.Ref, "third")
	cursor := readChanges(t, store, 0, 100).Cursor

	deleted := mustDeleteMessage(t, store, message.Ref())
	if deleted.Origin != message.Origin ||
		!deleted.Created.Equal(message.Created) {
		t.Fatalf("delete changed more than the text:\n%+v\nwas\n%+v",
			deleted, message)
	}
	if !deleted.Deleted {
		t.Fatalf("deleted message is not marked deleted: %+v", deleted)
	}
	want := "e1-m1:first e1-m2: e1-m3:third cursor=3 remaining=0"
	if got := chatText(readChat(t, store, post.Ref, 0, 10)); got != want {
		t.Fatalf("chat is %s, want %s", got, want)
	}

	page := readChanges(t, store, cursor, 100)
	if got := changeIDs(page); !reflect.DeepEqual(got,
		[]string{"e1-m2"}) {
		t.Fatalf("changes after the delete are %v, want e1-m2", got)
	}
	mustDeleteMessage(t, store, message.Ref())
	if got := changeIDs(readChanges(t, store, page.Cursor, 100)); len(got) !=
		0 {
		t.Fatalf("deleting again made changes %v", got)
	}
}

// An edit of a deleted post or message would bring back what the delete
// removed, so even the poster cannot make one.
func TestNobodyCanEditWhatIsDeleted(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	other := mustAdd(t, store, "other", "", nil)
	post := mustAdd(t, store, "topic", "", nil)
	message := mustPostMessage(t, store, post.Ref, "hi")
	deleted := mustDelete(t, store, post.Ref)
	mustDeleteMessage(t, store, message.Ref())
	key := store.mustKey(post.Ref)

	subject := "back"
	for _, req := range []api.EditPostRequest{
		{Subject: &subject, User: testUser},
		{AddLinks: []api.Link{link("see", other.Ref)}, User: testUser},
	} {
		_, err := store.Edit(ctx, everyAccess(store), key, req)
		var refused *DeletedError
		if !errors.As(err, &refused) {
			t.Fatalf("editing a deleted post = %v, want a refusal", err)
		}
	}
	if got := mustGet(t, store, post.Ref); !got.Deleted ||
		got.Subject != "" || len(got.Links) != 0 ||
		!got.Modified.Equal(deleted.Modified) {
		t.Fatalf("a refused edit changed the deleted post: %+v", got)
	}

	for _, body := range []string{"back", ""} {
		_, _, err := store.EditMessage(ctx, everyAccess(store), key, message.Number,
			api.EditMessageRequest{Body: body, User: testUser})
		var refused *DeletedError
		if !errors.As(err, &refused) {
			t.Fatalf("editing a deleted message to %q = %v, want a "+
				"refusal", body, err)
		}
	}
	if got := readChat(t, store, post.Ref, 0, 10).Messages[0]; !got.Deleted ||
		got.Body != "" {
		t.Fatalf("a refused edit changed the deleted message: %+v", got)
	}
}
