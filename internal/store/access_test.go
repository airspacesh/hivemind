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

// linkedWorkspaces holds open entries in default linked every way a
// link may cross workspaces to protected entries in personal, whose
// text all holds SECRET. A parent never crosses workspaces, so trees
// stay in one.
type linkedWorkspaces struct {
	store *Store
	// openRoot has an open child, openChild. secretSee links to
	// openRoot, and openSee links to secretSee.
	openRoot  api.Entry
	openChild api.Entry
	secretSee api.Entry
	openSee   api.Entry
	// secretSuperseder supersedes openOld.
	openOld          api.Entry
	secretSuperseder api.Entry
}

func newLinkedWorkspaces(t *testing.T) linkedWorkspaces {
	t.Helper()
	store := openTestStore(t)
	secret := func(subject string, links ...api.Link) api.Entry {
		t.Helper()
		entry := addTo(t, store, "personal", api.AddEntryRequest{
			Subject: "SECRET " + subject,
			Body:    "SECRET body",
			Project: "SECRET-project",
			Origin:  api.Origin{User: "SECRET-user", Dir: "/SECRET"},
			Links:   links,
		})
		mustPost(t, store, entry.Ref, "SECRET message")
		return entry
	}
	open := func(subject string, links ...api.Link) api.Entry {
		t.Helper()
		return addTo(t, store, "default", api.AddEntryRequest{
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
	return accessTo(t, w.store, "default")
}

func (w linkedWorkspaces) detail(t *testing.T, ref api.Ref) api.EntryDetail {
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
	assertNoSecret(t, "an entry linking to a protected one", detail)
	if !reflect.DeepEqual(detail.Links,
		[]api.Link{link("see", w.secretSee.Ref)}) {
		t.Fatalf("links = %+v", detail.Links)
	}

	root := w.detail(t, w.openRoot.Ref)
	assertNoSecret(t, "an entry a protected one links to", root)
	if !reflect.DeepEqual(root.Backlinks, []api.Link{
		link("parent", w.openChild.Ref), link("see", w.secretSee.Ref),
	}) {
		t.Fatalf("backlinks = %+v, want a bare ref for the protected one",
			root.Backlinks)
	}

	old := w.detail(t, w.openOld.Ref)
	assertNoSecret(t, "an entry a protected one supersedes", old)
	if !reflect.DeepEqual(old.SupersededBy,
		[]api.Ref{w.secretSuperseder.Ref}) {
		t.Fatalf("superseded by %+v, want the bare ref", old.SupersededBy)
	}
}

func TestProtectedEntriesStayOutOfListings(t *testing.T) {
	ctx := context.Background()
	w := newLinkedWorkspaces(t)
	open := w.openAccess(t)
	root := w.store.mustKey(w.openRoot.Ref)

	tree, err := w.store.Tree(ctx, open, root, true, 30)
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
	if refs := entryRefsOf(under.Entries); !reflect.DeepEqual(refs,
		refStrings(w.openChild.Ref, w.openRoot.Ref)) {
		t.Fatalf("search under the root = %v", refs)
	}

	for _, view := range []View{ViewFlat, ViewTopics} {
		listing, err := w.store.Search(ctx, open, root.Workspace,
			Filter{Limit: 20, View: view})
		if err != nil {
			t.Fatal(err)
		}
		assertNoSecret(t, "a listing", listing)
		for _, entry := range listing.Entries {
			if entry.Ref == w.openOld.Ref {
				t.Fatalf("view %d lists %s, which a protected entry "+
					"supersedes", view, entry.Ref)
			}
		}
	}

	overview, err := w.store.Overview(ctx, root.Workspace, 20)
	if err != nil {
		t.Fatal(err)
	}
	assertNoSecret(t, "the chat overview", overview)
}

func TestLinkingToAnUnreadableEntryIsRefusedAsMissing(t *testing.T) {
	ctx := context.Background()
	w := newLinkedWorkspaces(t)
	add := func(target api.Ref) error {
		_, err := w.store.Add(ctx, w.openAccess(t),
			workspaceID(t, w.store, "default"), api.AddEntryRequest{
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
		w.store.mustKey(w.openRoot.Ref), api.EditEntryRequest{
			AddLinks: []api.Link{link("see", w.secretSee.Ref)},
			User:     testUser,
		}); !errors.Is(err, ErrLinkTarget) {
		t.Fatalf("editing in a link to an unreadable entry = %v", err)
	}
}
