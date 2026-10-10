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

func topics(t *testing.T, store *Store, filter Filter) api.SearchResult {
	t.Helper()
	filter.View = ViewTopics
	return runSearch(t, store, filter)
}

func addChild(
	t *testing.T, store *Store, subject string, parent api.Ref,
) api.Entry {
	t.Helper()
	return addTo(t, store, "default", api.AddEntryRequest{
		Type: "note", Subject: subject,
		Links: []api.Link{link(api.ParentLink, parent)},
	})
}

func readTree(
	t *testing.T, store *Store, root api.Ref, includeSuperseded bool,
	limit int,
) api.TreeResponse {
	t.Helper()
	tree, err := store.Tree(context.Background(), everyAccess(store),
		store.mustKey(root), includeSuperseded, limit)
	if err != nil {
		t.Fatal(err)
	}
	return tree
}

// treeText shows a tree's entries in order by subject, with its cuts.
func treeText(tree api.TreeResponse) string {
	var lines []string
	for _, entry := range tree.Entries {
		line := entry.Subject
		if entry.MoreBelow > 0 {
			line += fmt.Sprintf(" (%d more below)", entry.MoreBelow)
		}
		if entry.MoreUnder > 0 {
			line += fmt.Sprintf(" (%d more under)", entry.MoreUnder)
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, ", ")
}

func TestTopicsFollowActivityAnywhereInTheTree(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	older := mustAdd(t, store, "idea", "older topic", "", nil)
	newer := mustAdd(t, store, "idea", "newer topic", "", nil)

	result := topics(t, store, Filter{})
	if refs := entryRefsOf(result.Entries); !reflect.DeepEqual(
		refs, refStrings(newer.Ref, older.Ref)) {
		t.Fatalf("topics before any child = %v", refs)
	}

	child := addChild(t, store, "child", older.Ref)
	grandchild := addChild(t, store, "grandchild", child.Ref)
	result = topics(t, store, Filter{})
	if refs := entryRefsOf(result.Entries); !reflect.DeepEqual(
		refs, refStrings(older.Ref, newer.Ref)) {
		t.Fatalf("topics after a grandchild = %v, want the older first", refs)
	}
	top := result.Entries[0]
	if !top.Modified.Equal(older.Modified) {
		t.Fatal("a descendant changed its root's modified time")
	}
	if !top.Activity.Equal(grandchild.Modified) || top.Descendants != 2 {
		t.Fatalf("root activity %v and descendants %d, want %v and 2",
			top.Activity, top.Descendants, grandchild.Modified)
	}

	flat := runSearch(t, store, Filter{})
	if refs := entryRefsOf(flat.Entries); !reflect.DeepEqual(refs,
		refStrings(grandchild.Ref, child.Ref, newer.Ref, older.Ref)) {
		t.Fatalf("flat listing = %v", refs)
	}
	path := flat.Entries[0].Ancestors
	if len(path) != 2 || path[0].Ref != older.Ref ||
		path[1].Ref != child.Ref {
		t.Fatalf("grandchild's path = %+v, want root then child", path)
	}

	subject := "newer topic, edited"
	if _, err := store.Edit(ctx, everyAccess(store),
		store.mustKey(newer.Ref),
		api.EditEntryRequest{Subject: &subject}); err != nil {
		t.Fatal(err)
	}
	if refs := entryRefsOf(topics(t, store, Filter{}).Entries); refs[0] !=
		newer.Ref.String() {
		t.Fatalf("an edited root did not rise: %v", refs)
	}
	mustPost(t, store, grandchild.Ref, "a reply")
	if refs := entryRefsOf(topics(t, store, Filter{}).Entries); refs[0] !=
		older.Ref.String() {
		t.Fatalf("a chat post did not lift its root: %v", refs)
	}
}

// Under each top-level entry, the topics view shows the newest
// messages anywhere in its tree, superseded entries included, and
// counts the rest.
func TestTopicsShowTheNewestMessagesInEachTree(t *testing.T) {
	store := openTestStore(t)
	root := mustAdd(t, store, "idea", "pond", "", nil)
	child := addChild(t, store, "child", root.Ref)
	grandchild := addChild(t, store, "grandchild", child.Ref)
	replaced := addChild(t, store, "replaced", root.Ref)
	successor := addChild(t, store, "successor", root.Ref)
	if err := addLinks(t, store, successor.Ref,
		link(api.SupersedesLink, replaced.Ref)); err != nil {
		t.Fatal(err)
	}
	silent := mustAdd(t, store, "idea", "silent", "", nil)
	other := mustAdd(t, store, "idea", "other", "", nil)
	mustPost(t, store, root.Ref, "first")
	mustPost(t, store, child.Ref, "second")
	newest := []api.Message{
		mustPost(t, store, grandchild.Ref, "third"),
		mustPost(t, store, replaced.Ref, "fourth"),
	}
	elsewhere := mustPost(t, store, other.Ref, "elsewhere")

	result := topics(t, store, Filter{})
	assertEntryRefs(t, result.Entries, other.Ref, root.Ref, silent.Ref)
	if got, want := messageRefs(result.Entries[1].TreeMessages),
		messageRefs(newest); !reflect.DeepEqual(got, want) {
		t.Fatalf("tree messages = %v, want %v", got, want)
	}
	if got := result.Entries[1].EarlierTreeMessages; got != 2 {
		t.Fatalf("earlier tree messages = %d, want 2", got)
	}
	if got, want := messageRefs(result.Entries[0].TreeMessages),
		messageRefs([]api.Message{elsewhere}); !reflect.DeepEqual(
		got, want) || result.Entries[0].EarlierTreeMessages != 0 {
		t.Fatalf("other's tree messages = %v and %d earlier, want %v",
			got, result.Entries[0].EarlierTreeMessages, want)
	}
	if got := result.Entries[2].TreeMessages; len(got) != 0 {
		t.Fatalf("a tree with no chat shows %v", got)
	}

	for _, entry := range runSearch(t, store, Filter{}).Entries {
		if len(entry.TreeMessages) != 0 {
			t.Fatalf("the flat listing shows %s's tree messages", entry.Ref)
		}
	}
}

func TestTopicsKeepASupersededRootOnlyAsContext(t *testing.T) {
	store := openTestStore(t)
	withChild := mustAdd(t, store, "idea", "replaced topic", "", nil)
	child := addChild(t, store, "useful child", withChild.Ref)
	alone := mustAdd(t, store, "idea", "replaced and alone", "", nil)
	successor := mustAdd(t, store, "idea", "successor", "", nil)
	for _, old := range []api.Ref{withChild.Ref, alone.Ref} {
		if err := addLinks(t, store, successor.Ref,
			link(api.SupersedesLink, old)); err != nil {
			t.Fatal(err)
		}
	}

	result := topics(t, store, Filter{})
	if refs := entryRefsOf(result.Entries); !reflect.DeepEqual(
		refs, refStrings(successor.Ref, withChild.Ref)) {
		t.Fatalf("topics = %v, want the successor and the context root",
			refs)
	}
	if !reflect.DeepEqual(result.Entries[1].SupersededBy,
		[]api.Ref{successor.Ref}) {
		t.Fatal("the context root is not labelled as superseded")
	}
	if all := topics(t, store, Filter{IncludeSuperseded: true}); len(
		all.Entries) != 3 {
		t.Fatalf("topics with superseded = %d, want 3", len(all.Entries))
	}
	if got := treeText(readTree(t, store, withChild.Ref, false, 30)); got !=
		"replaced topic, "+child.Subject {
		t.Fatalf("tree = %q", got)
	}
}

func TestTreeHidesSupersededLeavesAndOrdersOldestFirst(t *testing.T) {
	store := openTestStore(t)
	root := mustAdd(t, store, "idea", "root", "", nil)
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

	if got := treeText(readTree(t, store, root.Ref, false, 30)); got !=
		"root, first, middle, under middle, fresh" {
		t.Fatalf("tree = %q, want the stale leaf hidden", got)
	}
	if got := treeText(readTree(t, store, root.Ref, true, 30)); got !=
		"root, first, middle, under middle, stale, fresh" {
		t.Fatalf("tree with superseded = %q", got)
	}
	if got := treeText(readTree(t, store, root.Ref, false, 2)); got !=
		"root (3 more under), first" {
		t.Fatalf("cut tree = %q, want no count of the stale leaf", got)
	}
	subtree := readTree(t, store, middle.Ref, false, 30)
	if len(subtree.Ancestors) != 1 || subtree.Ancestors[0].Ref != root.Ref {
		t.Fatalf("subtree path = %+v, want the root", subtree.Ancestors)
	}
}

func TestTreeFillsLevelByLevelAndSaysWhereItCut(t *testing.T) {
	store := openTestStore(t)
	root := mustAdd(t, store, "idea", "root", "", nil)
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
		got := treeText(readTree(t, store, root.Ref, false, test.limit))
		if got != test.want {
			t.Fatalf("limit %d: tree = %q, want %q", test.limit, got,
				test.want)
		}
	}
	_, err := store.Tree(context.Background(), everyAccess(store),
		store.mustKey(root.Ref), false, 0)
	if !errors.Is(err, ErrInvalidTreeLimit) {
		t.Fatalf("a zero limit = %v", err)
	}
}

func TestSearchUnderASubtreeAndFiltersOnRoots(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	root := addTo(t, store, "default", api.AddEntryRequest{
		Type: "idea", Subject: "duck root", Project: "pond"})
	child := addTo(t, store, "default", api.AddEntryRequest{
		Type: "note", Subject: "duck child", Project: "lake",
		Links: []api.Link{link(api.ParentLink, root.Ref)}})
	other := addTo(t, store, "default", api.AddEntryRequest{
		Type: "idea", Subject: "duck elsewhere", Project: "lake"})

	under := store.mustKey(root.Ref)
	result := runSearch(t, store, Filter{Under: &under,
		Content: []FieldCond{{Value: "duck"}}})
	if refs := entryRefsOf(result.Entries); !reflect.DeepEqual(
		refs, refStrings(child.Ref, root.Ref)) {
		t.Fatalf("search under the root = %v", refs)
	}

	lakeTopics := topics(t, store, Filter{
		Project: []FieldCond{{Value: "lake"}}})
	if refs := entryRefsOf(lakeTopics.Entries); !reflect.DeepEqual(
		refs, refStrings(other.Ref)) {
		t.Fatalf("lake topics = %v, want only the lake root", refs)
	}

	missing := Key{Workspace: under.Workspace, ID: 99}
	_, err := store.Search(ctx, everyAccess(store), under.Workspace,
		Filter{Limit: 20, Under: &missing})
	if !errors.Is(err, ErrEntryNotFound) {
		t.Fatalf("search under a missing entry = %v", err)
	}
	_, err = store.Search(ctx, everyAccess(store), under.Workspace,
		Filter{Limit: 20, View: ViewTopics, OrderByCreation: true})
	if !errors.Is(err, ErrInvalidFilter) {
		t.Fatalf("topics by creation = %v, want ErrInvalidFilter", err)
	}
}

// Counts leave out superseded entries, as listings and trees do.
func TestCountsLeaveOutSupersededEntries(t *testing.T) {
	store := openTestStore(t)
	question := mustAdd(t, store, "question", "Why?", "", nil)
	old := addChild(t, store, "old answer", question.Ref)
	addTo(t, store, "default", api.AddEntryRequest{
		Type: "answer", Subject: "new answer",
		Links: []api.Link{link(api.ParentLink, question.Ref),
			link(api.SupersedesLink, old.Ref)},
	})

	detail := mustGet(t, store, question.Ref)
	if detail.Children != 1 || detail.Descendants != 1 {
		t.Fatalf("children %d, descendants %d, want 1 and 1",
			detail.Children, detail.Descendants)
	}
	listed := topics(t, store, Filter{Limit: 20})
	if len(listed.Entries) != 1 || listed.Entries[0].Descendants != 1 {
		t.Fatalf("topics = %+v, want the question with 1 below",
			listed.Entries)
	}
}
