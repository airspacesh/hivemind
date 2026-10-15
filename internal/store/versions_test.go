package store

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/airspacesh/hivemind/internal/api"
)

func mustVersion(
	t *testing.T, store *Store, ref api.Ref, number int64,
) api.PostVersion {
	t.Helper()
	version, err := store.Version(context.Background(), everyAccess(store),
		store.mustKey(ref), number)
	if err != nil {
		t.Fatalf("reading version %d of %s: %v", number, ref, err)
	}
	return version
}

// Each version reads back as the edit that made it left the post, its
// parent and metadata included and its other links left out, and only
// an edit that changes what a version holds makes one.
func TestAVersionReadsBackAsItWas(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	first := mustAdd(t, store, "first parent", "", nil)
	second := mustAdd(t, store, "second parent", "", nil)
	added := addTo(t, store, "general", api.AddPostRequest{
		Project: "pond", Subject: "plan", Body: "draft",
		Metadata: map[string]string{"status": "draft", "jira": "QUACK-1"},
		Origin:   api.Origin{User: "alice", Host: "nest"},
		Links: []api.Link{link(api.ParentLink, first.Ref),
			link("see", second.Ref)},
	})
	key := store.mustKey(added.Ref)
	edit := func(user string, req api.EditPostRequest) api.Post {
		t.Helper()
		req.Origin = api.Origin{User: user, ClaudeSession: user + "-session"}
		post, err := store.Edit(ctx, everyAccess(store), key, req)
		if err != nil {
			t.Fatalf("editing as %s: %v", user, err)
		}
		return post
	}

	subject, body := "plan, revised", "final"
	edit("bob", api.EditPostRequest{Subject: &subject, Body: &body,
		Metadata: map[string]string{"status": "", "owner": "bob"}})
	moved := edit("carol", api.EditPostRequest{
		AddLinks: []api.Link{link(api.ParentLink, second.Ref)}})
	if moved.Version.Number != 3 ||
		moved.Version.Origin.ClaudeSession != "carol-session" {
		t.Fatalf("a move made version %+v, want 3 by carol",
			moved.Version)
	}
	unchanged := edit("dave", api.EditPostRequest{Subject: &subject})
	if unchanged.Version.Number != 3 {
		t.Fatalf("an edit that changed nothing made version %d",
			unchanged.Version.Number)
	}

	want := []api.PostVersion{
		{Ref: added.Ref, Project: "pond", Subject: "plan", Body: "draft",
			Metadata: map[string]string{"status": "draft",
				"jira": "QUACK-1"},
			Links: []api.Link{link(api.ParentLink, first.Ref)},
			Version: api.Version{Number: 1,
				Origin: api.Origin{User: "alice", Host: "nest"}}},
		{Ref: added.Ref, Project: "pond", Subject: subject, Body: body,
			Metadata: map[string]string{"jira": "QUACK-1",
				"owner": "bob"},
			Links: []api.Link{link(api.ParentLink, first.Ref)},
			Version: api.Version{Number: 2, Origin: api.Origin{
				User: "bob", ClaudeSession: "bob-session"}}},
		{Ref: added.Ref, Project: "pond", Subject: subject, Body: body,
			Metadata: map[string]string{"jira": "QUACK-1",
				"owner": "bob"},
			Links: []api.Link{link(api.ParentLink, second.Ref)},
			Version: api.Version{Number: 3, Origin: api.Origin{
				User: "carol", ClaudeSession: "carol-session"}}},
	}
	for _, wanted := range want {
		got := mustVersion(t, store, added.Ref, wanted.Version.Number)
		wanted.Version.Created = got.Version.Created
		if !reflect.DeepEqual(got, wanted) {
			t.Fatalf("version %d is\n%+v\nwant\n%+v", wanted.Version.Number,
				got, wanted)
		}
	}
	if current := mustGet(t, store, added.Ref).Version; *current !=
		mustVersion(t, store, added.Ref, 3).Version {
		t.Fatalf("the post is at %+v, want version 3", current)
	}
}

// An edit expecting an older version is refused, whether it edits the
// post's fields or moves it.
func TestAnEditExpectingAnotherVersionIsRefused(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	post := mustAdd(t, store, "plan", "", nil)
	parent := mustAdd(t, store, "parent", "", nil)
	key := store.mustKey(post.Ref)
	edit := func(req api.EditPostRequest, expected int64) (api.Post, error) {
		req.ExpectedVersion = &expected
		req.Origin = api.Origin{User: "bob"}
		return store.Edit(ctx, everyAccess(store), key, req)
	}
	subject := func(text string) api.EditPostRequest {
		return api.EditPostRequest{Subject: &text}
	}
	if _, err := edit(subject("plan, revised"), 1); err != nil {
		t.Fatal(err)
	}
	before := mustGet(t, store, post.Ref)

	for _, stale := range []api.EditPostRequest{
		subject("plan, from a stale read"),
		{AddLinks: []api.Link{link(api.ParentLink, parent.Ref)}},
	} {
		_, err := edit(stale, 1)
		var conflict *VersionConflictError
		if !errors.As(err, &conflict) ||
			err.Error() != "e1 is at version 2, not 1" {
			t.Fatalf("a stale edit %+v = %v, want a refusal naming "+
				"version 2", stale, err)
		}
		if after := mustGet(t, store, post.Ref); !reflect.DeepEqual(after,
			before) {
			t.Fatalf("a refused edit changed the post:\n%+v\nwas\n%+v",
				after, before)
		}
	}
	if edited, err := edit(subject("plan, from a fresh read"),
		2); err != nil || edited.Version.Number != 3 {
		t.Fatalf("an edit expecting the current version = %+v, %v",
			edited.Version, err)
	}
}

// An edit that writes what the post holds already, its own parent
// included, and removes a metadata key it lacks makes no version and no
// change. It still checks the version it expects.
func TestAnEditThatChangesNothingIsNoChange(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	parent := mustAdd(t, store, "parent", "", nil)
	post := addTo(t, store, "general", api.AddPostRequest{
		Project: "pond", Subject: "plan", Body: "draft",
		Metadata: map[string]string{"status": "draft"},
		Links:    []api.Link{link(api.ParentLink, parent.Ref)},
		Origin:   api.Origin{User: "alice"},
	})
	key := store.mustKey(post.Ref)
	project, subject, body, private := "pond", "plan, revised", "draft",
		false
	if _, err := store.Edit(ctx, everyAccess(store), key,
		api.EditPostRequest{Subject: &subject,
			Origin: api.Origin{User: "bob"}}); err != nil {
		t.Fatal(err)
	}
	before := mustGet(t, store, post.Ref)
	_, cursor := readAllChanges(t, store, 0, 10)
	same := api.EditPostRequest{Project: &project, Subject: &subject,
		Body: &body, Metadata: map[string]string{"status": "draft",
			"jira": ""},
		AddLinks: []api.Link{link(api.ParentLink, parent.Ref)},
		Private:  &private, Origin: api.Origin{User: "alice"},
	}

	edited, err := store.Edit(ctx, everyAccess(store), key, same)
	if err != nil {
		t.Fatal(err)
	}
	if edited.Version.Number != 2 || edited.Revision != before.Revision ||
		!edited.Modified.Equal(before.Modified) {
		t.Fatalf("an edit that changed nothing left version %d, "+
			"revision %d and modified %v, want 2, %d and %v",
			edited.Version.Number, edited.Revision, edited.Modified,
			before.Revision, before.Modified)
	}
	if after := mustGet(t, store, post.Ref); !reflect.DeepEqual(after,
		before) {
		t.Fatalf("an edit that changed nothing changed the post:\n%+v\n"+
			"was\n%+v", after, before)
	}
	if got := changeIDs(readChanges(t, store, cursor, 10)); len(got) != 0 {
		t.Fatalf("an edit that changed nothing showed as changes %v", got)
	}

	stale := int64(1)
	same.ExpectedVersion = &stale
	var conflict *VersionConflictError
	if _, err := store.Edit(ctx, everyAccess(store), key,
		same); !errors.As(err, &conflict) {
		t.Fatalf("an edit that changes nothing, expecting version 1 = %v, "+
			"want a refusal", err)
	}
}
