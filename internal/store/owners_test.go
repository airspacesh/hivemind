package store

import (
	"context"
	"errors"
	"testing"

	"github.com/airspacesh/workstream/internal/api"
)

func addAs(
	t *testing.T, store *Store, user, subject string, links ...api.Link,
) api.Entry {
	t.Helper()
	return addTo(t, store, "general", api.AddEntryRequest{
		Subject: subject, Links: links,
		Origin: api.Origin{User: user},
	})
}

func postAs(
	t *testing.T, store *Store, user string, entry api.Ref, body string,
) api.Message {
	t.Helper()
	message, err := store.Post(context.Background(), store.mustKey(entry),
		api.PostMessageRequest{Body: body, Origin: api.Origin{User: user}})
	if err != nil {
		t.Fatalf("posting to %s as %s: %v", entry, user, err)
	}
	return message
}

func assertNotOwner(t *testing.T, err error, want string) {
	t.Helper()
	var notOwner *NotOwnerError
	if !errors.As(err, &notOwner) || err.Error() != want {
		t.Fatalf("change = %v, want a refusal saying %q", err, want)
	}
}

func TestOnlyThePosterCanEditOrDeleteAnEntry(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	topic := addAs(t, store, "alice", "topic")
	other := addAs(t, store, "alice", "other")
	key := store.mustKey(topic.Ref)
	subject := "taken over"
	refusal := "e1 was posted by alice, so only alice can edit or " +
		"delete it"

	for _, req := range []api.EditEntryRequest{
		{Subject: &subject, User: "bob"},
		{Subject: &subject, User: "Alice"},
		{Metadata: map[string]string{"jira": "QUACK-1"}, User: "bob"},
		{AddLinks: []api.Link{link("see", other.Ref)}, User: "bob"},
		{RemoveLinkNames: []string{api.ParentLink}, User: "bob"},
	} {
		_, err := store.Edit(ctx, everyAccess(store), key, req)
		assertNotOwner(t, err, refusal)
	}
	_, err := store.Delete(ctx, everyAccess(store), key,
		api.DeleteRequest{User: "bob"})
	assertNotOwner(t, err, refusal)
	if got := mustGet(t, store, topic.Ref); got.Subject != "topic" ||
		len(got.Metadata) != 0 || len(got.Links) != 0 ||
		!got.Modified.Equal(topic.Modified) {
		t.Fatalf("a refused change changed the entry: %+v", got)
	}

	edited, err := store.Edit(ctx, everyAccess(store), key,
		api.EditEntryRequest{Subject: &subject, User: "alice"})
	if err != nil || edited.Subject != subject {
		t.Fatalf("the poster's own edit = %+v, %v", edited, err)
	}
}

func TestOthersCanStillAddUnderAnEntryAndChatOnIt(t *testing.T) {
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

	// The entry's own poster cannot edit its chat either, even with
	// text that would change nothing.
	for _, body := range []string{"a different answer", "the answer"} {
		_, _, err := store.EditMessage(ctx, key, message.Number,
			api.EditMessageRequest{Body: body, User: "bob"})
		assertNotOwner(t, err, refusal)
	}
	_, err := store.DeleteMessage(ctx, key, message.Number,
		api.DeleteRequest{User: "bob"})
	assertNotOwner(t, err, refusal)
	if got := readChat(t, store, topic.Ref, 0, 20); got.Messages[0].Body !=
		"the answer" {
		t.Fatalf("a refused change changed the message: %+v",
			got.Messages)
	}

	edited, changed, err := store.EditMessage(ctx, key, message.Number,
		api.EditMessageRequest{Body: "Retracted", User: "alice"})
	if err != nil || !changed || edited.Body != "Retracted" {
		t.Fatalf("the poster's own edit = %+v, %v, %v", edited, changed,
			err)
	}
}

func TestNobodyCanEditOrDeleteWhatRecordsNoUser(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	entry, err := store.Add(ctx, everyAccess(store),
		workspaceID(t, store, "general"),
		api.AddEntryRequest{Subject: "anonymous"})
	if err != nil {
		t.Fatal(err)
	}
	message := postAs(t, store, "", entry.Ref, "anonymous")
	key := store.mustKey(entry.Ref)

	subject := "claimed"
	entryRefusal := "e1 records no user, so nobody can edit or delete it"
	messageRefusal := "e1-m1 records no user, so nobody can edit or " +
		"delete it"
	for _, user := range []string{"duck", ""} {
		_, err := store.Edit(ctx, everyAccess(store), key,
			api.EditEntryRequest{Subject: &subject, User: user})
		assertNotOwner(t, err, entryRefusal)
		_, err = store.Delete(ctx, everyAccess(store), key,
			api.DeleteRequest{User: user})
		assertNotOwner(t, err, entryRefusal)
		_, _, err = store.EditMessage(ctx, key, message.Number,
			api.EditMessageRequest{Body: "claimed", User: user})
		assertNotOwner(t, err, messageRefusal)
		_, err = store.DeleteMessage(ctx, key, message.Number,
			api.DeleteRequest{User: user})
		assertNotOwner(t, err, messageRefusal)
	}
}
