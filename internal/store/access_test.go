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

// linkedWorkspaces holds open entries in default linked every way to
// protected entries in personal, whose text all holds SECRET.
type linkedWorkspaces struct {
	store *Store
	// openRoot has a protected child, secretChild, which has an open
	// child, openUnderChild.
	openRoot       api.Entry
	secretChild    api.Entry
	openUnderChild api.Entry
	// secretParent is the parent of openChild, and links to openRoot.
	secretParent api.Entry
	openChild    api.Entry
}

func newLinkedWorkspaces(t *testing.T) linkedWorkspaces {
	t.Helper()
	store := openTestStore(t)
	secret := func(subject string, links ...api.Link) api.Entry {
		t.Helper()
		entry := addTo(t, store, "personal", api.AddEntryRequest{
			Type: "SECRET-type", Subject: "SECRET " + subject,
			Body:     "SECRET body",
			Metadata: map[string]string{"project": "SECRET-project"},
			Origin:   api.Origin{User: "SECRET-user", Dir: "/SECRET"},
			Links:    links,
		})
		mustPost(t, store, entry.Ref, "SECRET message")
		return entry
	}
	open := func(subject string, links ...api.Link) api.Entry {
		t.Helper()
		return addTo(t, store, "default", api.AddEntryRequest{
			Type: "note", Subject: subject, Links: links,
		})
	}

	w := linkedWorkspaces{store: store}
	w.openRoot = open("open root")
	w.secretChild = secret("child", link("parent", w.openRoot.Ref))
	w.openUnderChild = open("open under the secret child",
		link("parent", w.secretChild.Ref))
	w.secretParent = secret("parent", link("see", w.openRoot.Ref))
	w.openChild = open("open child", link("parent", w.secretParent.Ref))
	if err := addLinks(t, store, w.secretParent.Ref,
		link(api.SupersedesLink, w.secretChild.Ref)); err != nil {
		t.Fatal(err)
	}
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

func TestAProtectedParentShowsAsItsBareRef(t *testing.T) {
	w := newLinkedWorkspaces(t)
	detail := w.detail(t, w.openChild.Ref)
	assertNoSecret(t, "an open child", detail)
	want := []api.EntryRef{{Ref: w.secretParent.Ref}}
	if !reflect.DeepEqual(detail.Ancestors, want) {
		t.Fatalf("ancestors = %+v, want the bare ref %s", detail.Ancestors,
			w.secretParent.Ref)
	}
	if !reflect.DeepEqual(detail.Links,
		[]api.Link{link("parent", w.secretParent.Ref)}) {
		t.Fatalf("links = %+v", detail.Links)
	}

	under := w.detail(t, w.openUnderChild.Ref)
	assertNoSecret(t, "an entry under a protected one", under)
	if len(under.Ancestors) != 2 ||
		under.Ancestors[0].Subject != "open root" ||
		!reflect.DeepEqual(under.Ancestors[1],
			api.EntryRef{Ref: w.secretChild.Ref}) {
		t.Fatalf("path = %+v, want the open root then a bare ref",
			under.Ancestors)
	}

	full, err := w.store.Detail(context.Background(), everyAccess(w.store),
		w.store.mustKey(w.openChild.Ref))
	if err != nil {
		t.Fatal(err)
	}
	if full.Ancestors[0].Subject != w.secretParent.Subject {
		t.Fatalf("with access, the parent is %+v", full.Ancestors[0])
	}
}

func TestAProtectedChildIsLeftOutWithNoCount(t *testing.T) {
	ctx := context.Background()
	w := newLinkedWorkspaces(t)
	open := w.openAccess(t)
	root := w.store.mustKey(w.openRoot.Ref)

	detail := w.detail(t, w.openRoot.Ref)
	assertNoSecret(t, "an open root", detail)
	if detail.Children != 0 || detail.Descendants != 0 ||
		!detail.Activity.Equal(w.openRoot.Modified) {
		t.Fatalf("children %d, descendants %d, activity %v; want only "+
			"the root itself", detail.Children, detail.Descendants,
			detail.Activity)
	}
	if !reflect.DeepEqual(detail.Backlinks, []api.Link{
		link("parent", w.secretChild.Ref), link("see", w.secretParent.Ref),
	}) {
		t.Fatalf("backlinks = %+v, want bare refs", detail.Backlinks)
	}

	tree, err := w.store.Tree(ctx, open, root, true, 30)
	if err != nil {
		t.Fatal(err)
	}
	assertNoSecret(t, "a tree", tree)
	if got := treeText(tree); got != "open root" {
		t.Fatalf("tree = %q, want the root alone, with no cut", got)
	}

	under, err := w.store.Search(ctx, open, root.Workspace,
		Filter{Limit: 20, Under: &root, IncludeSuperseded: true})
	if err != nil {
		t.Fatal(err)
	}
	assertNoSecret(t, "a search under the root", under)
	if refs := entryRefsOf(under.Entries); !reflect.DeepEqual(
		refs, refStrings(w.openRoot.Ref)) || under.Total != 1 {
		t.Fatalf("search under the root = %v of %d", refs, under.Total)
	}

	for _, view := range []View{ViewFlat, ViewTopics} {
		listing, err := w.store.Search(ctx, open, root.Workspace,
			Filter{Limit: 20, View: view, IncludeSuperseded: true})
		if err != nil {
			t.Fatal(err)
		}
		assertNoSecret(t, "a listing", listing)
		if listing.Total != 3 {
			t.Fatalf("view %d lists %v, want the three open entries",
				view, entryRefsOf(listing.Entries))
		}
		for _, entry := range listing.Entries {
			if entry.Descendants != 0 {
				t.Fatalf("%s counts %d descendants, want none readable",
					entry.Ref, entry.Descendants)
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
				Type: "note", Subject: "probe",
				Links: []api.Link{link("see", target)},
			})
		return err
	}
	unreadable := add(w.secretParent.Ref)
	missing := add(api.Ref{Prefix: "p", ID: 99})
	if !errors.Is(unreadable, ErrLinkTarget) ||
		!errors.Is(missing, ErrLinkTarget) {
		t.Fatalf("errors = %v and %v, want ErrLinkTarget", unreadable,
			missing)
	}
	if strings.Replace(missing.Error(), "p99", w.secretParent.Ref.String(),
		1) != unreadable.Error() {
		t.Fatalf("an unreadable target (%v) differs from a missing one "+
			"(%v)", unreadable, missing)
	}
	if _, err := w.store.Edit(ctx, w.openAccess(t),
		w.store.mustKey(w.openRoot.Ref), api.EditEntryRequest{
			AddLinks: []api.Link{link("parent", w.secretParent.Ref)},
		}); !errors.Is(err, ErrLinkTarget) {
		t.Fatalf("editing in a link to an unreadable entry = %v", err)
	}
}
