package store

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/airspacesh/hivemind/internal/api"
)

func TestRepeatedRequestReturnsThePostItAdded(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	general := workspaceID(t, store, "general")
	add := func(
		workspace int64, req api.AddPostRequest,
	) (api.Post, bool, error) {
		return store.Add(ctx, everyAccess(store), workspace, req)
	}
	req := api.AddPostRequest{
		Project:   "pond",
		Subject:   "Once",
		Body:      "Only once",
		Metadata:  map[string]string{"jira": "PROJ-1"},
		Origin:    api.Origin{User: testUser, Dir: "/pond"},
		RequestID: "r1",
	}
	first, created, err := add(general, req)
	if err != nil || !created {
		t.Fatalf("first add: created %v, %v", created, err)
	}
	revision := latestRevision(t, store)

	// A repeat from another directory is still a repeat.
	repeat := req
	repeat.Origin.Dir = "/elsewhere"
	second, created, err := add(general, repeat)
	if err != nil || created || !reflect.DeepEqual(second, first) {
		t.Fatalf("repeat: created %v, %v, got %+v, want %+v", created, err,
			second, first)
	}
	other := req
	other.Subject = "Twice"
	_, _, err = add(general, other)
	if !errors.Is(err, ErrRequestReused) {
		t.Fatalf("an id reused for other content: got %v", err)
	}
	if latest := latestRevision(t, store); latest != revision {
		t.Fatalf("revision moved from %d to %d", revision, latest)
	}

	// Request ids are per workspace, and an add with no id never
	// repeats.
	personal, created, err := add(workspaceID(t, store, "personal"), req)
	if err != nil || !created || personal.Ref == first.Ref {
		t.Fatalf("the same id in another workspace: %+v, %v, %v", personal,
			created, err)
	}
	req.RequestID = ""
	for range 2 {
		if _, created, err := add(general, req); err != nil || !created {
			t.Fatalf("an add without an id: created %v, %v", created, err)
		}
	}
}

func TestRepeatedRequestReturnsTheMessageItSent(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	pond := mustAdd(t, store, "pond", "", nil)
	lake := mustAdd(t, store, "lake", "", nil)
	send := func(post api.Ref, body, id string) (api.Message, bool, error) {
		return store.PostMessage(ctx, everyAccess(store), store.mustKey(post),
			api.PostMessageRequest{Body: body, RequestID: id,
				Origin: api.Origin{User: testUser}})
	}
	first, created, err := send(pond.Ref, "quack", "r1")
	if err != nil || !created {
		t.Fatalf("first send: created %v, %v", created, err)
	}
	revision := latestRevision(t, store)

	second, created, err := send(pond.Ref, "quack", "r1")
	if err != nil || created || !reflect.DeepEqual(second, first) {
		t.Fatalf("repeat: created %v, %v, got %+v, want %+v", created, err,
			second, first)
	}
	if _, _, err := send(pond.Ref, "honk", "r1"); !errors.Is(err,
		ErrRequestReused) {
		t.Fatalf("an id reused for other text: got %v", err)
	}
	if latest := latestRevision(t, store); latest != revision {
		t.Fatalf("revision moved from %d to %d", revision, latest)
	}

	// A repeat matches the request, not the message's current text.
	edited, _, err := store.EditMessage(ctx, everyAccess(store),
		store.mustKey(pond.Ref),
		first.Number, ownEdit("quack quack"))
	if err != nil {
		t.Fatal(err)
	}
	third, created, err := send(pond.Ref, "quack", "r1")
	if err != nil || created || !reflect.DeepEqual(third, edited) {
		t.Fatalf("repeat after an edit: created %v, %v, got %+v", created,
			err, third)
	}

	// Request ids are per post, and a send with no id never repeats.
	other, created, err := send(lake.Ref, "quack", "r1")
	if err != nil || !created || other.Post != lake.Ref {
		t.Fatalf("the same id in another chat: %+v, %v, %v", other,
			created, err)
	}
	for range 2 {
		if _, created, err := send(pond.Ref, "quack", ""); err != nil ||
			!created {
			t.Fatalf("a send without an id: created %v, %v", created, err)
		}
	}
	if got := chatText(readChat(t, store, pond.Ref, 0, 10)); got !=
		"e1-m1:quack quack e1-m2:quack e1-m3:quack cursor=3 remaining=0" {
		t.Fatalf("chat holds %s", got)
	}
}
