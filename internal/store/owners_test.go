package store

import (
	"context"
	"errors"
	"testing"

	"github.com/airspacesh/hivemind/internal/api"
)

func addAs(
	t *testing.T, store *Store, user, subject string, links ...api.Link,
) api.Post {
	t.Helper()
	return addTo(t, store, "general", api.AddPostRequest{
		Subject: subject, Links: links,
		Origin: api.Origin{User: user},
	})
}

func assertNotOwner(t *testing.T, err error, want string) {
	t.Helper()
	var notOwner *NotOwnerError
	if !errors.As(err, &notOwner) || err.Error() != want {
		t.Fatalf("change = %v, want a refusal saying %q", err, want)
	}
}

func TestAnyoneCanEditAPublicPostButOnlyThePosterCanDeleteIt(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	topic := addAs(t, store, "alice", "topic")
	other := addAs(t, store, "alice", "other")
	key := store.mustKey(topic.Ref)
	subject := "taken over"

	for _, req := range []api.EditPostRequest{
		{Subject: &subject, User: "bob"},
		{Metadata: map[string]string{"jira": "QUACK-1"}, User: "bob"},
		{AddLinks: []api.Link{link("see", other.Ref)}, User: "bob"},
		{AddLinks: []api.Link{link(api.ParentLink, other.Ref)},
			User: "Alice"},
	} {
		if _, err := store.Edit(ctx, everyAccess(store), key,
			req); err != nil {
			t.Fatalf("another user's edit %+v = %v", req, err)
		}
	}
	got := mustGet(t, store, topic.Ref)
	if got.Subject != subject || got.Metadata["jira"] != "QUACK-1" ||
		len(got.Links) != 2 {
		t.Fatalf("other users' edits left %+v", got)
	}

	private := true
	_, err := store.Edit(ctx, everyAccess(store), key,
		api.EditPostRequest{Private: &private, User: "bob"})
	assertNotOwner(t, err,
		"e1 was posted by alice, so only alice can make it private or public")
	_, err = store.Delete(ctx, everyAccess(store), key,
		api.DeleteRequest{User: "bob"})
	assertNotOwner(t, err,
		"e1 was posted by alice, so only alice can delete it")
	if got := mustGet(t, store, topic.Ref); got.Deleted || got.Private {
		t.Fatalf("a refused change changed the post: %+v", got)
	}
}

// Another user cannot read a private post, so the server refuses their
// edit as post not found. An edit that reads as the owner but names
// another editor is refused too.
func TestOnlyThePosterCanEditAPrivatePost(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	private := addAsUnder(t, store, "alice", "private", nil, true)
	child := addAsUnder(t, store, "alice", "child", &private.Ref, false)
	subject := "taken over"

	for _, post := range []api.Post{private, child} {
		_, err := store.Edit(ctx, accessAs(store, "alice"),
			store.mustKey(post.Ref),
			api.EditPostRequest{Subject: &subject, User: "bob"})
		assertNotOwner(t, err, post.Ref.String()+
			" was posted by alice, so only alice can edit it")
		got, err := store.Detail(ctx, accessAs(store, "alice"),
			store.mustKey(post.Ref))
		if err != nil || got.Subject == subject {
			t.Fatalf("a refused edit changed %s: %+v, %v", post.Ref, got,
				err)
		}
	}
}

func TestOthersCanStillAddUnderAPostAndChatOnIt(t *testing.T) {
	store := openTestStore(t)
	topic := addAs(t, store, "alice", "topic")

	child := addAs(t, store, "bob", "reply", link(api.ParentLink, topic.Ref))
	message := postAs(t, store, "bob", topic.Ref, "a question")

	detail := mustGet(t, store, topic.Ref)
	if detail.Children != 1 || detail.Messages != 1 {
		t.Fatalf("topic has %d children and %d messages, want 1 and 1",
			detail.Children, detail.Messages)
	}
	if child.Origin.User != "bob" || message.Origin.User != "bob" {
		t.Fatalf("child by %q and message by %q, want bob",
			child.Origin.User, message.Origin.User)
	}
}

func TestOnlyThePosterCanEditOrDeleteAMessage(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	topic := addAs(t, store, "bob", "topic")
	message := postAs(t, store, "alice", topic.Ref, "the answer")
	key := store.mustKey(topic.Ref)
	refusal := "e1-m1 was posted by alice, so only alice can edit or " +
		"delete it"

	// The post's own poster cannot edit its chat either, even with text
	// that would change nothing.
	for _, body := range []string{"a different answer", "the answer"} {
		_, _, err := store.EditMessage(ctx, everyAccess(store), key,
			message.Number,
			api.EditMessageRequest{Body: body, User: "bob"})
		assertNotOwner(t, err, refusal)
	}
	_, err := store.DeleteMessage(ctx, everyAccess(store), key, message.Number,
		api.DeleteRequest{User: "bob"})
	assertNotOwner(t, err, refusal)
	if got := readChat(t, store, topic.Ref, 0, 20); got.Messages[0].Body !=
		"the answer" {
		t.Fatalf("a refused change changed the message: %+v",
			got.Messages)
	}

	edited, changed, err := store.EditMessage(ctx, everyAccess(store), key,
		message.Number,
		api.EditMessageRequest{Body: "Retracted", User: "alice"})
	if err != nil || !changed || edited.Body != "Retracted" {
		t.Fatalf("the poster's own edit = %+v, %v, %v", edited, changed,
			err)
	}
}

func TestNobodyCanDeleteWhatRecordsNoUser(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	post, _, err := store.Add(ctx, everyAccess(store),
		workspaceID(t, store, "general"),
		api.AddPostRequest{Subject: "anonymous"})
	if err != nil {
		t.Fatal(err)
	}
	message := postAs(t, store, "", post.Ref, "anonymous")
	key := store.mustKey(post.Ref)

	postRefusal := "e1 records no user, so nobody can delete it"
	messageRefusal := "e1-m1 records no user, so nobody can edit or " +
		"delete it"
	for _, user := range []string{"duck", ""} {
		_, err := store.Delete(ctx, everyAccess(store), key,
			api.DeleteRequest{User: user})
		assertNotOwner(t, err, postRefusal)
		_, _, err = store.EditMessage(ctx, everyAccess(store), key,
			message.Number,
			api.EditMessageRequest{Body: "claimed", User: user})
		assertNotOwner(t, err, messageRefusal)
		_, err = store.DeleteMessage(ctx, everyAccess(store), key,
			message.Number,
			api.DeleteRequest{User: user})
		assertNotOwner(t, err, messageRefusal)
	}
}
