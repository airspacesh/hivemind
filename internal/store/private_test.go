package store

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/airspacesh/hivemind/internal/api"
)

func setPrivate(
	store *Store, user string, ref api.Ref, private bool,
) (api.Post, error) {
	return store.Edit(context.Background(), accessAs(store, user),
		store.mustKey(ref), api.EditPostRequest{Private: &private,
			Origin: api.Origin{User: user}})
}

func mustSetPrivate(
	t *testing.T, store *Store, user string, ref api.Ref, private bool,
) api.Post {
	t.Helper()
	post, err := setPrivate(store, user, ref, private)
	if err != nil {
		t.Fatalf("making %s private=%v as %s: %v", ref, private, user, err)
	}
	return post
}

// addAsUnder adds a post by user, reading as user, under parent when
// it is set.
func addAsUnder(
	t *testing.T, store *Store, user, subject string, parent *api.Ref,
	private bool,
) api.Post {
	t.Helper()
	req := api.AddPostRequest{
		Subject: subject, Private: private, Origin: api.Origin{User: user},
	}
	if parent != nil {
		req.Links = []api.Link{link(api.ParentLink, *parent)}
	}
	post, _, err := store.Add(context.Background(), accessAs(store, user),
		workspaceID(t, store, "general"), req)
	if err != nil {
		t.Fatalf("adding %q as %s: %v", subject, user, err)
	}
	return post
}

// canRead reports whether user gets a post, failing on any error but
// post not found.
func canRead(t *testing.T, store *Store, user string, ref api.Ref) bool {
	t.Helper()
	_, err := store.Detail(context.Background(), accessAs(store, user),
		store.mustKey(ref))
	if errors.Is(err, ErrPostNotFound) {
		return false
	}
	if err != nil {
		t.Fatal(err)
	}
	return true
}

func assertReaders(
	t *testing.T, store *Store, user string, readable, hidden []api.Ref,
) {
	t.Helper()
	for _, ref := range readable {
		if !canRead(t, store, user, ref) {
			t.Errorf("%s cannot read %s", user, ref)
		}
	}
	for _, ref := range hidden {
		if canRead(t, store, user, ref) {
			t.Errorf("%s can read %s", user, ref)
		}
	}
}

func TestOnlyTheOwnerReadsAPrivatePostAndWhatIsUnderIt(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	general := workspaceID(t, store, "general")
	topic := addAs(t, store, "alice", "topic")
	old := addAs(t, store, "alice", "old plan")
	draft, _, err := store.Add(ctx, accessAs(store, "alice"), general,
		api.AddPostRequest{
			Subject: "SECRET draft", Body: "SECRET body",
			Project: "SECRET-project", Private: true,
			Origin: api.Origin{User: "alice"},
			Links: []api.Link{link(api.ParentLink, topic.Ref),
				link(api.SupersedesLink, old.Ref)},
		})
	if err != nil {
		t.Fatal(err)
	}
	detail := addAsUnder(t, store, "alice", "SECRET detail", &draft.Ref,
		false)
	postAs(t, store, "alice", draft.Ref, "SECRET message")
	if _, err := store.Edit(ctx, accessAs(store, "alice"),
		store.mustKey(topic.Ref), api.EditPostRequest{
			AddLinks: []api.Link{link("see", draft.Ref)},
			Origin:   api.Origin{User: "alice"},
		}); err != nil {
		t.Fatal(err)
	}
	bob := accessAs(store, "bob")

	assertReaders(t, store, "alice",
		[]api.Ref{topic.Ref, draft.Ref, detail.Ref}, nil)
	assertReaders(t, store, "bob",
		[]api.Ref{topic.Ref}, []api.Ref{draft.Ref, detail.Ref})
	if draft.PrivateTo != "alice" || !draft.Private ||
		detail.PrivateTo != "alice" || detail.Private {
		t.Fatalf("draft private %v to %q, detail private %v to %q",
			draft.Private, draft.PrivateTo, detail.Private,
			detail.PrivateTo)
	}

	topicDetail, err := store.Detail(ctx, bob, store.mustKey(topic.Ref))
	if err != nil {
		t.Fatal(err)
	}
	assertNoSecret(t, "the topic", topicDetail)
	if topicDetail.Children != 0 || topicDetail.Descendants != 0 ||
		!topicDetail.Activity.Equal(topicDetail.Modified) {
		t.Fatalf("bob sees the topic with %d children, %d descendants, "+
			"activity %v", topicDetail.Children, topicDetail.Descendants,
			topicDetail.Activity)
	}
	tree, err := store.Tree(ctx, bob, store.mustKey(topic.Ref), true, true,
		30)
	if err != nil {
		t.Fatal(err)
	}
	if got := treeText(tree); got != "topic" {
		t.Fatalf("bob's tree = %q", got)
	}

	for _, filter := range []Filter{
		{Limit: 20, Items: api.PostsAndChat},
		{Limit: 20, Items: api.PostsAndChat,
			Content: []FieldCond{{Value: "secret"}}},
		{Limit: 20, Project: []FieldCond{{Value: "secret-project"}}},
		{Limit: 20, OriginUser: []FieldCond{{Value: "alice"}}},
	} {
		page, err := store.Search(ctx, bob, general, filter)
		if err != nil {
			t.Fatal(err)
		}
		assertNoSecret(t, "a listing", page)
		if filter.Content == nil && filter.Project == nil &&
			!reflect.DeepEqual(matchRefs(page),
				refStrings(old.Ref, topic.Ref)) {
			t.Fatalf("bob lists %v, want the topic and the old plan, "+
				"which only a private post supersedes", matchRefs(page))
		}
	}
	changes, err := store.Changes(ctx, bob, general, 0,
		Filter{Items: api.PostsAndChat}, nil, 50)
	if err != nil {
		t.Fatal(err)
	}
	assertNoSecret(t, "the changes", changes)
	overview, err := store.Overview(ctx, bob, general, Filter{}, 20)
	if err != nil {
		t.Fatal(err)
	}
	assertNoSecret(t, "the chat overview", overview)
	projects, err := store.Projects(ctx, bob, general, false, 20, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	assertNoSecret(t, "the projects", projects)
	project, err := store.Project(ctx, bob, general, "SECRET-project",
		time.Now())
	if err != nil || len(project.Projects) != 0 {
		t.Fatalf("bob reads the private project as %+v, %v", project, err)
	}

	draftKey := store.mustKey(draft.Ref)
	for what, read := range map[string]func() error{
		"chat": func() error {
			_, err := store.Messages(ctx, bob, draftKey, 0, Filter{},
				nil, 20)
			return err
		},
		"latest chat": func() error {
			_, err := store.LatestMessages(ctx, bob, draftKey,
				0, Filter{}, 20)
			return err
		},
		"message": func() error {
			_, err := store.MessageDetail(ctx, bob, draftKey, 1)
			return err
		},
		"search under": func() error {
			_, err := store.Search(ctx, bob, general,
				Filter{Limit: 20, Under: &draftKey})
			return err
		},
		"changes under": func() error {
			_, err := store.Changes(ctx, bob, general, 0,
				Filter{Items: api.PostsAndChat, Under: &draftKey}, nil, 20)
			return err
		},
		"tree": func() error {
			_, err := store.Tree(ctx, bob, draftKey, false, false, 30)
			return err
		},
		"chat send": func() error {
			_, _, err := store.PostMessage(ctx, bob, draftKey,
				api.PostMessageRequest{Body: "hi",
					Origin: api.Origin{User: "bob"}})
			return err
		},
		"chat edit": func() error {
			_, _, err := store.EditMessage(ctx, bob, draftKey, 1,
				api.EditMessageRequest{Body: "hi", User: "bob"})
			return err
		},
		"edit": func() error {
			_, err := setPrivate(store, "bob", draft.Ref, false)
			return err
		},
		"delete": func() error {
			_, err := store.Delete(ctx, bob, draftKey,
				api.DeleteRequest{User: "bob"})
			return err
		},
		"add under": func() error {
			_, _, err := store.Add(ctx, bob, general, api.AddPostRequest{
				Subject: "reply", Origin: api.Origin{User: "bob"},
				Links: []api.Link{link(api.ParentLink, draft.Ref)},
			})
			return err
		},
	} {
		err := read()
		if !errors.Is(err, ErrPostNotFound) &&
			!errors.Is(err, ErrLinkTarget) {
			t.Errorf("bob's %s = %v, want post not found", what, err)
		}
	}
}

// TestShowingAPostShowsWhatItHid follows the design's example: g4 is
// private, g5 sits under it and is not private itself, so it is hidden
// through g4, and showing g4 shows both. g6, made private on its own,
// stays private with g7 under it.
func TestShowingAPostShowsWhatItHid(t *testing.T) {
	store := openTestStore(t)
	root := addAsUnder(t, store, "alice", "root", nil, false)
	g4 := addAsUnder(t, store, "alice", "g4", &root.Ref, false)
	g5 := addAsUnder(t, store, "alice", "g5", &g4.Ref, false)
	g6 := addAsUnder(t, store, "alice", "g6", &g4.Ref, true)
	g7 := addAsUnder(t, store, "alice", "g7", &g6.Ref, false)
	assertReaders(t, store, "bob", []api.Ref{root.Ref, g4.Ref, g5.Ref},
		[]api.Ref{g6.Ref, g7.Ref})

	mustSetPrivate(t, store, "alice", g4.Ref, true)
	assertReaders(t, store, "bob", []api.Ref{root.Ref},
		[]api.Ref{g4.Ref, g5.Ref, g6.Ref, g7.Ref})
	assertReaders(t, store, "alice",
		[]api.Ref{root.Ref, g4.Ref, g5.Ref, g6.Ref, g7.Ref}, nil)

	// Making g6 public changes nothing anyone sees while g4 hides it,
	// but it takes effect once g4 is public.
	shown := mustSetPrivate(t, store, "alice", g6.Ref, false)
	if shown.Private || shown.PrivateTo != "alice" {
		t.Fatalf("g6 under private g4 is private %v to %q, want not "+
			"private itself but private to alice", shown.Private,
			shown.PrivateTo)
	}
	mustSetPrivate(t, store, "alice", g6.Ref, true)
	mustSetPrivate(t, store, "alice", g4.Ref, false)
	assertReaders(t, store, "bob", []api.Ref{root.Ref, g4.Ref, g5.Ref},
		[]api.Ref{g6.Ref, g7.Ref})
	mustSetPrivate(t, store, "alice", g6.Ref, false)
	assertReaders(t, store, "bob",
		[]api.Ref{root.Ref, g4.Ref, g5.Ref, g6.Ref, g7.Ref}, nil)
}

func TestMovingAPostTakesTheVisibilityOfItsNewParent(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	private := addAsUnder(t, store, "alice", "private", nil, true)
	moved := addAsUnder(t, store, "alice", "moved", nil, false)
	child := addAsUnder(t, store, "alice", "child", &moved.Ref, false)
	move := func(parent *api.Ref) (api.Post, error) {
		req := api.EditPostRequest{Origin: api.Origin{User: "alice"}}
		if parent != nil {
			req.AddLinks = []api.Link{link(api.ParentLink, *parent)}
		} else {
			req.RemoveLinkNames = []string{api.ParentLink}
		}
		return store.Edit(ctx, accessAs(store, "alice"),
			store.mustKey(moved.Ref), req)
	}

	if _, err := move(&private.Ref); err != nil {
		t.Fatal(err)
	}
	assertReaders(t, store, "bob", nil, []api.Ref{moved.Ref, child.Ref})
	before := readChanges(t, store, 0, 50)
	out, err := move(nil)
	if err != nil {
		t.Fatal(err)
	}
	assertReaders(t, store, "bob", []api.Ref{moved.Ref, child.Ref}, nil)
	if out.Revision <= before.Cursor {
		t.Fatalf("moving the post out of the private tree left it at "+
			"revision %d, before the cursor %d", out.Revision,
			before.Cursor)
	}

	postAs(t, store, "bob", child.Ref, "a question")
	_, err = move(&private.Ref)
	var shared *SharedError
	if !errors.As(err, &shared) {
		t.Fatalf("moving a post bob chatted under into a private tree "+
			"= %v, want a refusal", err)
	}
	assertReaders(t, store, "bob", []api.Ref{moved.Ref, child.Ref}, nil)
}

func TestAPostOthersPostedUnderCannotBeMadePrivate(t *testing.T) {
	store := openTestStore(t)
	topic := addAsUnder(t, store, "alice", "topic", nil, false)
	mine := addAsUnder(t, store, "alice", "mine", &topic.Ref, false)
	chatted := addAsUnder(t, store, "alice", "chatted", nil, false)
	postAs(t, store, "bob", chatted.Ref, "a question")
	addAsUnder(t, store, "bob", "a reply", &mine.Ref, false)

	for _, ref := range []api.Ref{topic.Ref, mine.Ref, chatted.Ref} {
		_, err := setPrivate(store, "alice", ref, true)
		want := "others have posts or messages under " + ref.String() +
			" that they have not deleted, so it cannot be made private"
		var shared *SharedError
		if !errors.As(err, &shared) || err.Error() != want {
			t.Fatalf("making %s private = %v, want %q", ref, err, want)
		}
	}
	assertReaders(t, store, "bob",
		[]api.Ref{topic.Ref, mine.Ref, chatted.Ref}, nil)

	_, err := setPrivate(store, "bob", topic.Ref, true)
	assertNotOwner(t, err, "e1 was posted by alice, so only alice can "+
		"make it private or public")
}

// What others deleted under a post no longer keeps it public. It
// becomes private with the post, even a post of theirs that they made
// private, and goes back to them when the post is public again.
func TestWhatOthersDeletedDoesNotKeepAPostPublic(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	bob := accessAs(store, "bob")
	topic := addAsUnder(t, store, "alice", "topic", nil, false)
	reply := addAsUnder(t, store, "bob", "reply", &topic.Ref, false)
	draft := addAsUnder(t, store, "bob", "draft", &topic.Ref, true)
	message := postAs(t, store, "bob", topic.Ref, "a question")
	for _, ref := range []api.Ref{reply.Ref, draft.Ref} {
		if _, err := store.Delete(ctx, bob, store.mustKey(ref),
			api.DeleteRequest{User: "bob"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := setPrivate(store, "alice", topic.Ref, true); err == nil {
		t.Fatal("alice made the topic private over bob's message")
	}
	if _, err := store.DeleteMessage(ctx, bob, store.mustKey(topic.Ref),
		message.Number, api.DeleteRequest{User: "bob"}); err != nil {
		t.Fatal(err)
	}

	mustSetPrivate(t, store, "alice", topic.Ref, true)
	assertReaders(t, store, "bob", nil,
		[]api.Ref{topic.Ref, reply.Ref, draft.Ref})
	assertReaders(t, store, "alice",
		[]api.Ref{topic.Ref, reply.Ref, draft.Ref}, nil)
	_, err := store.MessageDetail(ctx, bob, store.mustKey(topic.Ref),
		message.Number)
	if !errors.Is(err, ErrPostNotFound) {
		t.Fatalf("bob's read of his deleted message = %v, want post not "+
			"found", err)
	}

	mustSetPrivate(t, store, "alice", topic.Ref, false)
	assertReaders(t, store, "bob",
		[]api.Ref{topic.Ref, reply.Ref, draft.Ref}, nil)
	assertReaders(t, store, "alice", []api.Ref{topic.Ref, reply.Ref},
		[]api.Ref{draft.Ref})
}

func TestMakingAPostPublicOrPrivateIsAChange(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	general := workspaceID(t, store, "general")
	draft := addAsUnder(t, store, "alice", "draft", nil, true)
	addAsUnder(t, store, "alice", "later news", nil, false)
	changes := func(user string, since int64) api.ChangesPage {
		t.Helper()
		page, err := store.Changes(ctx, accessAs(store, user), general,
			since, Filter{Items: api.PostsAndChat}, nil, 50)
		if err != nil {
			t.Fatal(err)
		}
		return page
	}
	cursor := changes("bob", 0).Cursor

	shown := mustSetPrivate(t, store, "alice", draft.Ref, false)
	if got := changeIDs(changes("bob", cursor)); !reflect.DeepEqual(got,
		refStrings(draft.Ref)) {
		t.Fatalf("bob's changes after the draft was shown = %v", got)
	}
	recent, err := store.Search(ctx, accessAs(store, "bob"), general,
		Filter{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if got := matchRefs(recent); !reflect.DeepEqual(got,
		refStrings(draft.Ref)) || shown.Revision <= draft.Revision {
		t.Fatalf("bob's newest post = %v, and the draft moved from "+
			"revision %d to %d", got, draft.Revision, shown.Revision)
	}

	cursor = changes("bob", 0).Cursor
	mustSetPrivate(t, store, "alice", draft.Ref, true)
	if got := changeIDs(changes("bob", cursor)); len(got) != 0 {
		t.Fatalf("bob's changes after the draft was hidden = %v", got)
	}
	if got := changeIDs(changes("alice", cursor)); !reflect.DeepEqual(got,
		refStrings(draft.Ref)) {
		t.Fatalf("alice's changes after the draft was hidden = %v", got)
	}
}

// readsAs gathers what user gets from each read of the general
// workspace, and of each post in refs, by what was read.
func readsAs(
	t *testing.T, store *Store, user string, refs ...api.Ref,
) map[string]any {
	t.Helper()
	ctx := context.Background()
	access := accessAs(store, user)
	general := workspaceID(t, store, "general")
	reads := map[string]any{}
	read := func(what string, value any, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s's %s: %v", user, what, err)
		}
		reads[what] = value
	}
	for _, ref := range refs {
		key := store.mustKey(ref)
		detail, err := store.Detail(ctx, access, key)
		read("post "+ref.String(), detail, err)
		tree, err := store.Tree(ctx, access, key, true, true, 30)
		read("tree "+ref.String(), tree, err)
		tree, err = store.Tree(ctx, access, key, false, false, 30)
		read("current tree "+ref.String(), tree, err)
		chat, err := store.Messages(ctx, access, key, 0, Filter{}, nil,
			20)
		read("chat "+ref.String(), chat, err)
	}
	for what, filter := range map[string]Filter{
		"recent": {Limit: 50},
		"recent with everything": {Limit: 50, Items: api.PostsAndChat,
			IncludeSuperseded: true, IncludeDeleted: true},
	} {
		page, err := store.Search(ctx, access, general, filter)
		read(what, page, err)
	}
	changes, err := store.Changes(ctx, access, general, 0,
		Filter{Items: api.PostsAndChat}, nil, 100)
	read("changes", changes, err)
	for _, includeDeleted := range []bool{false, true} {
		suffix := ""
		if includeDeleted {
			suffix = " with deleted"
		}
		overview, err := store.Overview(ctx, access, general,
			Filter{IncludeDeleted: includeDeleted}, 20)
		read("chat overview"+suffix, overview, err)
		projects, err := store.Projects(ctx, access, general,
			includeDeleted, 20, time.Now())
		read("projects"+suffix, projects, err)
	}
	return reads
}

// mentions reports whether a read names ref, as a post or by one of
// its messages.
func mentions(t *testing.T, read any, ref api.Ref) bool {
	t.Helper()
	text, err := json.Marshal(read)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Contains(string(text), `"`+ref.String()+`"`)
}

// TestLinksOfAPrivatePostShowOnlyToItsOwner links a private post to
// public ones, and public ones to private ones, every way, and checks
// that no read by anyone else names a private post at all.
func TestLinksOfAPrivatePostShowOnlyToItsOwner(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	topic := addAs(t, store, "alice", "topic")
	old := addAs(t, store, "bob", "old")
	bobs := addAs(t, store, "bob", "bobs")
	draft := addTo(t, store, "general", api.AddPostRequest{
		Subject: "draft", Private: true, Origin: api.Origin{User: "alice"},
		Links: []api.Link{link(api.ParentLink, topic.Ref),
			link(api.SupersedesLink, old.Ref), link("see", bobs.Ref)},
	})
	detail := addAsUnder(t, store, "alice", "detail", &draft.Ref, false)
	summary := addAs(t, store, "alice", "summary", link("see", draft.Ref),
		link(api.SupersedesLink, detail.Ref), link("see", topic.Ref))
	later := addAs(t, store, "alice", "later")
	reply := addAs(t, store, "bob", "reply", link("see", later.Ref),
		link(api.SupersedesLink, later.Ref))
	mustSetPrivate(t, store, "alice", later.Ref, true)
	postAs(t, store, "bob", topic.Ref, "a question")
	postAs(t, store, "alice", draft.Ref, "a note")
	postAs(t, store, "alice", later.Ref, "a note")
	public := []api.Ref{topic.Ref, old.Ref, bobs.Ref, summary.Ref,
		reply.Ref}
	private := []api.Ref{draft.Ref, detail.Ref, later.Ref}

	for what, read := range readsAs(t, store, "bob", public...) {
		for _, ref := range private {
			if mentions(t, read, ref) {
				t.Errorf("bob's %s names %s", what, ref)
			}
		}
	}
	for _, ref := range private {
		named := false
		for _, read := range readsAs(t, store, "alice", public...) {
			named = named || mentions(t, read, ref)
		}
		if !named {
			t.Errorf("no read of alice's names %s", ref)
		}
	}

	bob := accessAs(store, "bob")
	shown, err := store.Detail(ctx, bob, store.mustKey(summary.Ref))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(shown.Links, []api.Link{link("see", topic.Ref)}) {
		t.Fatalf("bob reads the summary's links as %+v", shown.Links)
	}
	shown, err = store.Detail(ctx, bob, store.mustKey(topic.Ref))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(shown.Backlinks,
		[]api.Link{link("see", summary.Ref)}) {
		t.Fatalf("bob reads the topic's backlinks as %+v", shown.Backlinks)
	}
}
