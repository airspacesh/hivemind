package store

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/airspacesh/workstream/internal/api"
)

func TestOneNameLinksSeveralTargetsButOneParent(t *testing.T) {
	store := openTestStore(t)
	a := mustAdd(t, store, "a", "", nil)
	b := mustAdd(t, store, "b", "", nil)
	c := mustAdd(t, store, "c", "", nil)

	if err := addLinks(t, store, a.Ref, link("depends-on", b.Ref),
		link("depends-on", c.Ref), link("parent", b.Ref)); err != nil {
		t.Fatal(err)
	}
	if err := addLinks(t, store, a.Ref, link("parent", c.Ref)); err != nil {
		t.Fatal(err)
	}
	want := []api.Link{
		link("depends-on", b.Ref), link("depends-on", c.Ref),
		link("parent", c.Ref),
	}
	if got := mustGet(t, store, a.Ref).Links; !reflect.DeepEqual(got, want) {
		t.Fatalf("links = %v, want %v", got, want)
	}
	if _, err := store.db.Exec(`INSERT INTO entry_links
		(source_workspace_id, source_id, name, target_workspace_id,
			target_id)
		SELECT workspace_id, ?, 'parent', workspace_id, ?
		FROM entries LIMIT 1`, a.Ref.ID, b.Ref.ID); err == nil {
		t.Fatal("the database accepted a second parent")
	}
	if got := mustGet(t, store, b.Ref).Backlinks; !reflect.DeepEqual(
		got, []api.Link{link("depends-on", a.Ref)}) {
		t.Fatalf("backlinks of b = %v", got)
	}
}

func TestLinksJoinWorkspacesOnOneServer(t *testing.T) {
	store := openTestStore(t)
	work := mustAdd(t, store, "work", "", nil)
	private := addTo(t, store, "personal", api.AddEntryRequest{
		Subject: "private",
		Links:   []api.Link{link("see", work.Ref)},
	})
	if err := addLinks(t, store, work.Ref,
		link(api.SupersedesLink, private.Ref)); err != nil {
		t.Fatal(err)
	}

	got := mustGet(t, store, work.Ref)
	if !reflect.DeepEqual(got.Links,
		[]api.Link{link(api.SupersedesLink, private.Ref)}) ||
		!reflect.DeepEqual(got.Backlinks,
			[]api.Link{link("see", private.Ref)}) {
		t.Fatalf("e1 links %v, backlinks %v", got.Links, got.Backlinks)
	}
}

// A parent stays in its child's workspace, so a tree never leaves it.
// The refusal is the same whether the other entry exists or not.
func TestParentsStayInTheirWorkspace(t *testing.T) {
	store := openTestStore(t)
	work := mustAdd(t, store, "work", "", nil)
	private := addTo(t, store, "personal", api.AddEntryRequest{
		Subject: "private",
	})

	_, err := store.Add(context.Background(), everyAccess(store),
		workspaceID(t, store, "personal"), api.AddEntryRequest{
			Subject: "child",
			Links:   []api.Link{link(api.ParentLink, work.Ref)},
		})
	if !errors.Is(err, ErrParentWorkspace) {
		t.Fatalf("adding under a parent in another workspace = %v", err)
	}
	existing := addLinks(t, store, private.Ref,
		link(api.ParentLink, work.Ref))
	missing := addLinks(t, store, private.Ref,
		link(api.ParentLink, api.Ref{Prefix: "e", ID: 99}))
	if !errors.Is(existing, ErrParentWorkspace) ||
		strings.Replace(missing.Error(), "e99", work.Ref.String(), 1) !=
			existing.Error() {
		t.Fatalf("editing in a parent in another workspace = %v, and "+
			"for a missing one %v", existing, missing)
	}
}

func TestParentAndSupersessionLoopsAreRefused(t *testing.T) {
	for _, name := range []string{api.ParentLink, api.SupersedesLink} {
		t.Run(name, func(t *testing.T) {
			store := openTestStore(t)
			a := mustAdd(t, store, "a", "", nil)
			b := mustAdd(t, store, "b", "", nil)
			c := mustAdd(t, store, "c", "", nil)
			if err := addLinks(t, store, b.Ref, link(name, a.Ref)); err != nil {
				t.Fatal(err)
			}
			if err := addLinks(t, store, c.Ref, link(name, b.Ref)); err != nil {
				t.Fatal(err)
			}
			for _, target := range []api.Ref{a.Ref, b.Ref, c.Ref} {
				err := addLinks(t, store, a.Ref, link(name, target))
				if !errors.Is(err, ErrLinkLoop) {
					t.Fatalf("%s %s %s: %v, want ErrLinkLoop",
						a.Ref, name, target, err)
				}
			}
			if links := mustGet(t, store, a.Ref).Links; len(links) != 0 {
				t.Fatalf("a refused loop left links %v", links)
			}
		})
	}
}

func TestOtherLinksMayLoop(t *testing.T) {
	store := openTestStore(t)
	a := mustAdd(t, store, "a", "", nil)
	b := mustAdd(t, store, "b", "", nil)
	if err := addLinks(t, store, a.Ref, link("see", b.Ref)); err != nil {
		t.Fatal(err)
	}
	if err := addLinks(t, store, b.Ref, link("see", a.Ref)); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentOppositeParentsLeaveNoLoop(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ws.db")
	first := openStoreAt(t, path, contendedOpenBusyTimeout)
	second := openStoreAt(t, path, contendedOpenBusyTimeout)
	a := mustAdd(t, first, "a", "", nil)
	b := mustAdd(t, first, "b", "", nil)

	for _, stores := range [][2]*Store{{first, first}, {first, second}} {
		start := make(chan struct{})
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for i, pair := range [][2]api.Ref{{a.Ref, b.Ref}, {b.Ref, a.Ref}} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				errs[i] = addLinks(t, stores[i], pair[0],
					link("parent", pair[1]))
			}()
		}
		close(start)
		wg.Wait()

		var count int
		if err := first.db.QueryRow(
			`SELECT count(*) FROM entry_links WHERE name = 'parent'`,
		).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 || (errs[0] == nil) == (errs[1] == nil) {
			t.Fatalf("parent links = %d, errors = %v; want one of each",
				count, errs)
		}
		if _, err := first.db.Exec(`DELETE FROM entry_links`); err != nil {
			t.Fatal(err)
		}
	}
}

func TestIndependentSuccessorsAreAllShown(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	old := mustAdd(t, store, "old", "", nil)
	newer := mustAdd(t, store, "newer", "", nil)
	newest := mustAdd(t, store, "newest", "", nil)
	for _, successor := range []api.Ref{newer.Ref, newest.Ref} {
		if err := addLinks(t, store, successor,
			link("supersedes", old.Ref)); err != nil {
			t.Fatal(err)
		}
	}

	got := mustGet(t, store, old.Ref).SupersededBy
	if !reflect.DeepEqual(got, []api.Ref{newer.Ref, newest.Ref}) {
		t.Fatalf("superseded by = %v, want both successors", got)
	}
	result := runSearch(t, store, Filter{})
	if refs := entryRefsOf(result.Entries); !reflect.DeepEqual(
		refs, refStrings(newest.Ref, newer.Ref)) {
		t.Fatalf("listing = %v, want both successors and not the old", refs)
	}
	result, err := store.Search(ctx, everyAccess(store),
		workspaceID(t, store, "default"),
		Filter{Limit: 10, IncludeSuperseded: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Entries) != 3 || result.More {
		t.Fatalf("listing with superseded holds %d entries and more %v, "+
			"want 3 and no more", len(result.Entries), result.More)
	}
}

func TestChildOfASupersededParentStaysVisible(t *testing.T) {
	store := openTestStore(t)
	parent := mustAdd(t, store, "parent", "", nil)
	child := mustAdd(t, store, "child", "", nil)
	replacement := mustAdd(t, store, "replacement", "", nil)
	if err := addLinks(t, store, child.Ref,
		link("parent", parent.Ref)); err != nil {
		t.Fatal(err)
	}
	if err := addLinks(t, store, replacement.Ref,
		link("supersedes", parent.Ref)); err != nil {
		t.Fatal(err)
	}

	result := runSearch(t, store, Filter{})
	if refs := entryRefsOf(result.Entries); !reflect.DeepEqual(
		refs, refStrings(replacement.Ref, child.Ref)) {
		t.Fatalf("listing = %v, want the replacement and the child", refs)
	}
	path := mustGet(t, store, child.Ref).Ancestors
	if len(path) != 1 || path[0].Ref != parent.Ref ||
		!reflect.DeepEqual(path[0].SupersededBy,
			[]api.Ref{replacement.Ref}) {
		t.Fatalf("child's path = %+v, want the labelled parent", path)
	}
}

func TestLinkEditsAndTheirFailures(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	a := mustAdd(t, store, "a", "", nil)
	b := mustAdd(t, store, "b", "", nil)
	edit := func(req api.EditEntryRequest) error {
		req.User = testUser
		_, err := store.Edit(ctx, everyAccess(store), store.mustKey(a.Ref),
			req)
		return err
	}

	if err := addLinks(t, store, a.Ref, link("see",
		api.Ref{Prefix: "e", ID: 99})); !errors.Is(err, ErrLinkTarget) {
		t.Fatalf("link to a missing entry = %v", err)
	}
	if err := addLinks(t, store, a.Ref, link("see",
		api.Ref{Prefix: "x", ID: 1})); !errors.Is(err, ErrUnknownPrefix) {
		t.Fatalf("link to an unknown prefix = %v", err)
	}
	if err := edit(api.EditEntryRequest{
		RemoveLinks: []api.Link{link("see", b.Ref)},
	}); !errors.Is(err, ErrLinkNotFound) {
		t.Fatalf("removing a missing link = %v", err)
	}
	if err := edit(api.EditEntryRequest{
		RemoveLinkNames: []string{"parent"},
	}); err != nil {
		t.Fatalf("removing every parent of an unparented entry: %v", err)
	}

	for range 2 {
		if err := addLinks(t, store, a.Ref, link("see", b.Ref)); err != nil {
			t.Fatal(err)
		}
	}
	edited := mustGet(t, store, a.Ref)
	if len(edited.Links) != 1 {
		t.Fatalf("a repeated link was stored twice: %v", edited.Links)
	}
	if !edited.Modified.Equal(a.Modified) {
		t.Fatal("a link-only edit changed the modified time")
	}

	var many []api.Link
	for range api.MaxLinks {
		many = append(many,
			link("ref", mustAdd(t, store, "t", "", nil).Ref))
	}
	if err := addLinks(t, store, a.Ref, many...); !errors.Is(
		err, ErrLinkLimit) {
		t.Fatalf("exceeding the link cap = %v", err)
	}
	if links := mustGet(t, store, a.Ref).Links; len(links) != 1 {
		t.Fatalf("a refused edit left %d links", len(links))
	}
}
