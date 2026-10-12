package store

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/airspacesh/workstream/internal/api"
)

func addChild(
	t *testing.T, store *Store, subject string, parent api.Ref,
) api.Post {
	t.Helper()
	return addTo(t, store, "general", api.AddPostRequest{
		Subject: subject,
		Links:   []api.Link{link(api.ParentLink, parent)},
	})
}

func readTree(
	t *testing.T, store *Store, root api.Ref, includeSuperseded bool,
	includeDeleted bool, limit int,
) api.TreeResponse {
	t.Helper()
	tree, err := store.Tree(context.Background(), everyAccess(store),
		store.mustKey(root), includeSuperseded, includeDeleted, limit)
	if err != nil {
		t.Fatal(err)
	}
	return tree
}

// treeText shows a tree's posts in order by subject, with its cuts.
func treeText(tree api.TreeResponse) string {
	var lines []string
	for _, post := range tree.Posts {
		line := post.Subject
		if post.Deleted {
			line = "(deleted)"
		}
		if post.MoreBelow > 0 {
			line += fmt.Sprintf(" (%d more below)", post.MoreBelow)
		}
		if post.MoreUnder > 0 {
			line += fmt.Sprintf(" (%d more under)", post.MoreUnder)
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, ", ")
}

func TestPostsShowThePathAndTheSubtreeFacts(t *testing.T) {
	store := openTestStore(t)
	older := mustAdd(t, store, "older topic", "", nil)
	newer := mustAdd(t, store, "newer topic", "", nil)
	child := addChild(t, store, "child", older.Ref)
	grandchild := addChild(t, store, "grandchild", child.Ref)

	listing := runSearch(t, store, Filter{})
	if refs := matchRefs(listing); !reflect.DeepEqual(refs,
		refStrings(grandchild.Ref, child.Ref, newer.Ref, older.Ref)) {
		t.Fatalf("listing = %v", refs)
	}
	path := listing.Matches[0].Ancestors
	if len(path) != 2 || path[0].Ref != older.Ref ||
		path[1].Ref != child.Ref {
		t.Fatalf("grandchild's path = %+v, want root then child", path)
	}
	top := mustGet(t, store, older.Ref)
	if !top.Modified.Equal(older.Modified) {
		t.Fatal("a descendant changed its root's modified time")
	}
	if !top.Activity.Equal(grandchild.Modified) || top.Descendants != 2 {
		t.Fatalf("root activity %v and descendants %d, want %v and 2",
			top.Activity, top.Descendants, grandchild.Modified)
	}
}

func TestTreeHidesSupersededLeavesAndOrdersOldestFirst(t *testing.T) {
	store := openTestStore(t)
	root := mustAdd(t, store, "root", "", nil)
	addChild(t, store, "first", root.Ref)
	middle := addChild(t, store, "middle", root.Ref)
	addChild(t, store, "under middle", middle.Ref)
	stale := addChild(t, store, "stale", root.Ref)
	fresh := addChild(t, store, "fresh", root.Ref)
	for _, old := range []api.Ref{stale.Ref, middle.Ref} {
		if err := addLinks(t, store, fresh.Ref,
			link(api.SupersedesLink, old)); err != nil {
			t.Fatal(err)
		}
	}

	if got := treeText(readTree(t, store, root.Ref, false, false, 30)); got !=
		"root, first, middle, under middle, fresh" {
		t.Fatalf("tree = %q, want the stale leaf hidden", got)
	}
	if got := treeText(readTree(t, store, root.Ref, true, false, 30)); got !=
		"root, first, middle, under middle, stale, fresh" {
		t.Fatalf("tree with superseded = %q", got)
	}
	if got := treeText(readTree(t, store, root.Ref, false, false, 2)); got !=
		"root (3 more under), first" {
		t.Fatalf("cut tree = %q, want no count of the stale leaf", got)
	}
	subtree := readTree(t, store, middle.Ref, false, false, 30)
	if len(subtree.Ancestors) != 1 || subtree.Ancestors[0].Ref != root.Ref {
		t.Fatalf("subtree path = %+v, want the root", subtree.Ancestors)
	}
}

func TestTreeKeepsADeletedPostOnlyAboveLivePosts(t *testing.T) {
	store := openTestStore(t)
	root := mustAdd(t, store, "root", "", nil)
	addChild(t, store, "first", root.Ref)
	gone := addChild(t, store, "gone", root.Ref)
	addChild(t, store, "under gone", gone.Ref)
	deeper := addChild(t, store, "deeper", gone.Ref)
	addChild(t, store, "under deeper", deeper.Ref)
	empty := addChild(t, store, "empty", gone.Ref)
	addChild(t, store, "last", root.Ref)
	leaf := addChild(t, store, "leaf", root.Ref)
	for _, ref := range []api.Ref{gone.Ref, deeper.Ref, empty.Ref,
		leaf.Ref} {
		mustDelete(t, store, ref)
	}

	if got := treeText(readTree(t, store, root.Ref, false, false, 30)); got !=
		"root, first, (deleted), under gone, (deleted), under deeper, last" {
		t.Fatalf("tree = %q, want the deleted posts above live ones "+
			"and no others", got)
	}
	if got := treeText(readTree(t, store, root.Ref, false, true, 30)); got !=
		"root, first, (deleted), under gone, (deleted), under deeper, "+
			"(deleted), last, (deleted)" {
		t.Fatalf("tree with deleted = %q", got)
	}
	if got := treeText(readTree(t, store, root.Ref, false, false, 3)); got !=
		"root (1 more under), first, (deleted) (3 more below)" {
		t.Fatalf("cut tree = %q, want no count of the deleted leaves", got)
	}
	if got := treeText(readTree(t, store, empty.Ref, false, false, 30)); got !=
		"(deleted)" {
		t.Fatalf("tree of a deleted leaf = %q, want it alone", got)
	}
}

func TestTreeFillsLevelByLevelAndSaysWhereItCut(t *testing.T) {
	store := openTestStore(t)
	root := mustAdd(t, store, "root", "", nil)
	a := addChild(t, store, "a", root.Ref)
	addChild(t, store, "b", root.Ref)
	c := addChild(t, store, "c", root.Ref)
	a1 := addChild(t, store, "a1", a.Ref)
	addChild(t, store, "a2", a.Ref)
	addChild(t, store, "a1x", a1.Ref)
	addChild(t, store, "c1", c.Ref)

	for _, test := range []struct {
		limit int
		want  string
	}{
		{1, "root (7 more below)"},
		{3, "root (2 more under), a (3 more below), b"},
		{4, "root, a (3 more below), b, c (1 more below)"},
		{6, "root, a, a1 (1 more below), a2, b, c (1 more below)"},
		{7, "root, a, a1 (1 more below), a2, b, c, c1"},
		{8, "root, a, a1, a1x, a2, b, c, c1"},
		{30, "root, a, a1, a1x, a2, b, c, c1"},
	} {
		tree := readTree(t, store, root.Ref, false, false, test.limit)
		if got := treeText(tree); got != test.want {
			t.Fatalf("limit %d: tree = %q, want %q", test.limit, got,
				test.want)
		}
	}
	_, err := store.Tree(context.Background(), everyAccess(store),
		store.mustKey(root.Ref), false, false, 0)
	if !errors.Is(err, ErrInvalidTreeLimit) {
		t.Fatalf("a zero limit = %v", err)
	}
}

func TestSearchUnderASubtree(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	root := addTo(t, store, "general", api.AddPostRequest{
		Subject: "duck root", Project: "pond"})
	child := addTo(t, store, "general", api.AddPostRequest{
		Subject: "duck child", Project: "lake",
		Links: []api.Link{link(api.ParentLink, root.Ref)}})
	addTo(t, store, "general", api.AddPostRequest{
		Subject: "duck elsewhere", Project: "lake"})

	under := store.mustKey(root.Ref)
	result := runSearch(t, store, Filter{Under: &under,
		Content: []FieldCond{{Value: "duck"}}})
	if refs := matchRefs(result); !reflect.DeepEqual(
		refs, refStrings(child.Ref, root.Ref)) {
		t.Fatalf("search under the root = %v", refs)
	}

	missing := Key{Workspace: under.Workspace, ID: 99}
	_, err := store.Search(ctx, everyAccess(store), under.Workspace,
		Filter{Limit: 20, Under: &missing})
	if !errors.Is(err, ErrPostNotFound) {
		t.Fatalf("search under a missing post = %v", err)
	}
}

// Counts leave out superseded and deleted posts, as listings and trees
// do, but count the posts under a deleted one.
func TestCountsLeaveOutSupersededAndDeletedPosts(t *testing.T) {
	store := openTestStore(t)
	question := mustAdd(t, store, "Why?", "", nil)
	old := addChild(t, store, "old answer", question.Ref)
	addTo(t, store, "general", api.AddPostRequest{
		Subject: "new answer",
		Links: []api.Link{link(api.ParentLink, question.Ref),
			link(api.SupersedesLink, old.Ref)},
	})
	wrong := addChild(t, store, "wrong answer", question.Ref)
	addChild(t, store, "follow-up", wrong.Ref)
	mustDelete(t, store, wrong.Ref)

	detail := mustGet(t, store, question.Ref)
	if detail.Children != 1 || detail.Descendants != 2 {
		t.Fatalf("children %d, descendants %d, want 1 and 2",
			detail.Children, detail.Descendants)
	}
}
