package store

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/airspacesh/hivemind/internal/api"
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
	message, err := store.DeleteMessage(context.Background(),
		everyAccess(store),
		store.mustKey(ref.Post), ref.Number,
		api.DeleteRequest{User: testUser})
	if err != nil {
		t.Fatalf("deleting %s: %v", ref, err)
	}
	return message
}

// readChatWithDeleted reads a post's chat as readChat does, deleted
// messages included.
func readChatWithDeleted(
	t *testing.T, store *Store, post api.Ref, since int64, limit int,
) api.ChatPage {
	t.Helper()
	page, err := store.Messages(context.Background(), everyAccess(store),
		store.mustKey(post), since,
		Filter{IncludeDeleted: true}, nil, limit)
	if err != nil {
		t.Fatal(err)
	}
	return page
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
	body := "more details"
	if _, err := store.Edit(context.Background(), everyAccess(store),
		store.mustKey(post.Ref), api.EditPostRequest{Body: &body,
			Origin: api.Origin{User: "heron"}}); err != nil {
		t.Fatal(err)
	}
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
	for number := int64(1); number <= 2; number++ {
		version := mustVersion(t, store, post.Ref, number)
		if !version.Deleted || version.Subject != "" ||
			version.Body != "" || len(version.Metadata) != 0 ||
			version.Project != post.Project ||
			!reflect.DeepEqual(version.Links, post.Links) {
			t.Fatalf("version %d of the deleted post is %+v", number,
				version)
		}
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
	message, _, err := store.PostMessage(context.Background(),
		everyAccess(store),
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
	if got := chatText(readChatWithDeleted(t, store, post.Ref, 0,
		10)); got != want {
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
		{Subject: &subject, Origin: api.Origin{User: testUser}},
		{AddLinks: []api.Link{link("see", other.Ref)},
			Origin: api.Origin{User: testUser}},
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
		_, _, err := store.EditMessage(ctx, everyAccess(store), key,
			message.Number,
			api.EditMessageRequest{Body: body, User: testUser})
		var refused *DeletedError
		if !errors.As(err, &refused) {
			t.Fatalf("editing a deleted message to %q = %v, want a "+
				"refusal", body, err)
		}
	}
	if got := readChatWithDeleted(t, store, post.Ref, 0,
		10).Messages[0]; !got.Deleted || got.Body != "" {
		t.Fatalf("a refused edit changed the deleted message: %+v", got)
	}
}

// A chat read leaves deleted messages out and counts only the others,
// but its cursor passes the deleted ones, so a read from it moves on.
func TestChatReadsLeaveOutDeletedMessages(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	post := mustAdd(t, store, "topic", "", nil)
	for i := 1; i <= 6; i++ {
		mustPostMessage(t, store, post.Ref, fmt.Sprint(i))
	}
	for _, number := range []int64{1, 4, 6} {
		mustDeleteMessage(t, store, api.MessageRef{Post: post.Ref,
			Number: number})
	}
	key := store.mustKey(post.Ref)

	for _, test := range []struct {
		since int64
		limit int
		want  string
	}{
		{0, 2, "e1-m2:2 e1-m3:3 cursor=3 remaining=1"},
		{3, 2, "e1-m5:5 cursor=6 remaining=0"},
		{3, 1, "e1-m5:5 cursor=5 remaining=0"},
		{5, 2, " cursor=6 remaining=0"},
		{6, 2, " cursor=6 remaining=0"},
	} {
		got := chatText(readChat(t, store, post.Ref, test.since,
			test.limit))
		if got != test.want {
			t.Fatalf("since %d limit %d = %q, want %q", test.since,
				test.limit, got, test.want)
		}
	}
	if got := chatText(readChatWithDeleted(t, store, post.Ref, 0,
		2)); got != "e1-m1: e1-m2:2 cursor=2 remaining=4" {
		t.Fatalf("with deleted = %q", got)
	}

	for _, test := range []struct {
		after          int64
		includeDeleted bool
		want           string
		earlier        int
	}{
		{0, false, "e1-m3:3 e1-m5:5 cursor=6 remaining=0", 1},
		{2, false, "e1-m3:3 e1-m5:5 cursor=6 remaining=0", 0},
		{0, true, "e1-m5:5 e1-m6: cursor=6 remaining=0", 4},
	} {
		latest, err := store.LatestMessages(ctx, everyAccess(store), key,
			test.after, Filter{IncludeDeleted: test.includeDeleted}, 2)
		if err != nil {
			t.Fatal(err)
		}
		if got := chatText(latest); got != test.want ||
			latest.Earlier != test.earlier {
			t.Fatalf("latest 2 after %d with deleted %v = %q and %d "+
				"earlier, want %q and %d", test.after, test.includeDeleted,
				got, latest.Earlier, test.want, test.earlier)
		}
	}

	overview, err := store.Overview(ctx, everyAccess(store), key.Workspace,
		Filter{}, 2)
	if err != nil {
		t.Fatal(err)
	}
	chat := overview.Chats[0]
	if got := chatText(api.ChatPage{Messages: chat.Messages}); got !=
		"e1-m3:3 e1-m5:5 cursor=0 remaining=0" || chat.Earlier != 1 {
		t.Fatalf("overview = %q with %d earlier, want m3 and m5 with 1",
			got, chat.Earlier)
	}
	detail := mustGet(t, store, post.Ref)
	if detail.Messages != 3 || detail.LastMessage != 5 {
		t.Fatalf("post counts %d messages, newest m%d, want 3 and m5",
			detail.Messages, detail.LastMessage)
	}
}
