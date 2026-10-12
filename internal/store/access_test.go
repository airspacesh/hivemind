package store

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/airspacesh/workstream/internal/api"
)

// linkedWorkspaces holds open posts in general linked every way a link
// may cross workspaces to protected posts in personal, whose text all
// holds SECRET. A parent never crosses workspaces, so trees stay in
// one.
type linkedWorkspaces struct {
	store *Store
	// openRoot has an open child, openChild. secretSee links to
	// openRoot, and openSee links to secretSee.
	openRoot  api.Post
	openChild api.Post
	secretSee api.Post
	openSee   api.Post
	// secretSuperseder supersedes openOld.
	openOld          api.Post
	secretSuperseder api.Post
}

func newLinkedWorkspaces(t *testing.T) linkedWorkspaces {
	t.Helper()
	store := openTestStore(t)
	secret := func(subject string, links ...api.Link) api.Post {
		t.Helper()
		post := addTo(t, store, "personal", api.AddPostRequest{
			Subject: "SECRET " + subject,
			Body:    "SECRET body",
			Project: "SECRET-project",
			Origin:  api.Origin{User: "SECRET-user", Dir: "/SECRET"},
			Links:   links,
		})
		mustPostMessage(t, store, post.Ref, "SECRET message")
		return post
	}
	open := func(subject string, links ...api.Link) api.Post {
		t.Helper()
		return addTo(t, store, "general", api.AddPostRequest{
			Subject: subject, Links: links,
		})
	}

	w := linkedWorkspaces{store: store}
	w.openRoot = open("open root")
	w.openChild = open("open child", link("parent", w.openRoot.Ref))
	w.secretSee = secret("see", link("see", w.openRoot.Ref))
	w.openSee = open("open see", link("see", w.secretSee.Ref))
	w.openOld = open("open old")
	w.secretSuperseder = secret("superseder",
		link(api.SupersedesLink, w.openOld.Ref))
	return w
}

func (w linkedWorkspaces) openAccess(t *testing.T) Access {
	return accessTo(t, w.store, "general")
}

func (w linkedWorkspaces) detail(t *testing.T, ref api.Ref) api.PostDetail {
	t.Helper()
	detail, err := w.store.Detail(context.Background(), w.openAccess(t),
		w.store.mustKey(ref))
	if err != nil {
		t.Fatal(err)
	}
	return detail
}

func assertNoSecret(t *testing.T, what string, value any) {
	t.Helper()
	text, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(text), "SECRET") {
		t.Fatalf("%s shows unreadable text: %s", what, text)
	}
}

func TestAProtectedLinkShowsAsItsBareRef(t *testing.T) {
	w := newLinkedWorkspaces(t)
	detail := w.detail(t, w.openSee.Ref)
	assertNoSecret(t, "a post linking to a protected one", detail)
	if !reflect.DeepEqual(detail.Links,
		[]api.Link{link("see", w.secretSee.Ref)}) {
		t.Fatalf("links = %+v", detail.Links)
	}

	root := w.detail(t, w.openRoot.Ref)
	assertNoSecret(t, "a post a protected one links to", root)
	if !reflect.DeepEqual(root.Backlinks, []api.Link{
		link("parent", w.openChild.Ref), link("see", w.secretSee.Ref),
	}) {
		t.Fatalf("backlinks = %+v, want a bare ref for the protected one",
			root.Backlinks)
	}

	old := w.detail(t, w.openOld.Ref)
	assertNoSecret(t, "a post a protected one supersedes", old)
	if !reflect.DeepEqual(old.SupersededBy,
		[]api.Ref{w.secretSuperseder.Ref}) {
		t.Fatalf("superseded by %+v, want the bare ref", old.SupersededBy)
	}
}

func TestProtectedPostsStayOutOfListings(t *testing.T) {
	ctx := context.Background()
	w := newLinkedWorkspaces(t)
	open := w.openAccess(t)
	root := w.store.mustKey(w.openRoot.Ref)

	tree, err := w.store.Tree(ctx, open, root, true, true, 30)
	if err != nil {
		t.Fatal(err)
	}
	assertNoSecret(t, "a tree", tree)
	if got := treeText(tree); got != "open root, open child" {
		t.Fatalf("tree = %q", got)
	}

	under, err := w.store.Search(ctx, open, root.Workspace,
		Filter{Limit: 20, Under: &root, IncludeSuperseded: true})
	if err != nil {
		t.Fatal(err)
	}
	assertNoSecret(t, "a search under the root", under)
	if refs := matchRefs(under); !reflect.DeepEqual(refs,
		refStrings(w.openChild.Ref, w.openRoot.Ref)) {
		t.Fatalf("search under the root = %v", refs)
	}

	listing, err := w.store.Search(ctx, open, root.Workspace,
		Filter{Limit: 20, Items: api.PostsAndChat})
	if err != nil {
		t.Fatal(err)
	}
	assertNoSecret(t, "a listing", listing)
	for _, match := range listing.Matches {
		if match.Post.Ref == w.openOld.Ref {
			t.Fatalf("search lists %s, which a protected post "+
				"supersedes", match.Post.Ref)
		}
	}

	overview, err := w.store.Overview(ctx, open, root.Workspace, false, 20)
	if err != nil {
		t.Fatal(err)
	}
	assertNoSecret(t, "the chat overview", overview)
}

func TestLinkingToAnUnreadablePostIsRefusedAsMissing(t *testing.T) {
	ctx := context.Background()
	w := newLinkedWorkspaces(t)
	add := func(target api.Ref) error {
		_, _, err := w.store.Add(ctx, w.openAccess(t),
			workspaceID(t, w.store, "general"), api.AddPostRequest{
				Subject: "probe",
				Links:   []api.Link{link("see", target)},
			})
		return err
	}
	unreadable := add(w.secretSee.Ref)
	missing := add(api.Ref{Prefix: "p", ID: 99})
	if !errors.Is(unreadable, ErrLinkTarget) ||
		!errors.Is(missing, ErrLinkTarget) {
		t.Fatalf("errors = %v and %v, want ErrLinkTarget", unreadable,
			missing)
	}
	if strings.Replace(missing.Error(), "p99", w.secretSee.Ref.String(),
		1) != unreadable.Error() {
		t.Fatalf("an unreadable target (%v) differs from a missing one "+
			"(%v)", unreadable, missing)
	}
	if _, err := w.store.Edit(ctx, w.openAccess(t),
		w.store.mustKey(w.openRoot.Ref), api.EditPostRequest{
			AddLinks: []api.Link{link("see", w.secretSee.Ref)},
			User:     testUser,
		}); !errors.Is(err, ErrLinkTarget) {
		t.Fatalf("editing in a link to an unreadable post = %v", err)
	}
}
