package store

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/airspacesh/workstream/internal/api"
)

type searchEntries struct {
	store  *Store
	first  api.Entry
	second api.Entry
	third  api.Entry
	fourth api.Entry
	fifth  api.Entry
}

func newSearchEntries(t *testing.T) searchEntries {
	t.Helper()
	store := openTestStore(t)
	return searchEntries{
		store: store,
		first: mustAdd(t, store, "TODO", "Count the Ducklings", "",
			map[string]string{"project": "duck-pond", "jira": "QUACK-1"}),
		second: mustAdd(t, store, "todo", "Fix Nest", "Ducklings are safe",
			map[string]string{"project": "duck-nest"}),
		third: mustAdd(t, store, "note", "Ducklings restless", "",
			map[string]string{"project": "duck-pond", "jira": "QUACK-2"}),
		fourth: mustAdd(t, store, "idea", "Cache feed", "", nil),
		fifth:  mustAdd(t, store, "note", "Æther", "", nil),
	}
}

// runSearch searches the default workspace with every workspace
// readable.
func runSearch(t *testing.T, store *Store, filter Filter) api.SearchResult {
	t.Helper()
	if filter.Limit == 0 {
		filter.Limit = 50
	}
	result, err := store.Search(context.Background(), everyAccess(store),
		workspaceID(t, store, "default"), filter)
	if err != nil {
		t.Fatalf("searching: %v", err)
	}
	return result
}

func assertEntryRefs(t *testing.T, entries []api.ListedEntry, want ...api.Ref) {
	t.Helper()
	if got := entryRefsOf(entries); !reflect.DeepEqual(
		got, refStrings(want...)) {
		t.Fatalf("entries = %v, want %v", got, refStrings(want...))
	}
}

func TestSearchUsesRawCaseFoldedGlobPatterns(t *testing.T) {
	entries := newSearchEntries(t)
	tests := []struct {
		name    string
		pattern string
		want    []api.Ref
	}{
		{"whole string", "COUNT THE DUCKLINGS", []api.Ref{entries.first.Ref}},
		{"bare text matches anywhere", "DUCKLINGS", []api.Ref{
			entries.third.Ref, entries.first.Ref,
		}},
		{"empty matches only an empty value", "", nil},
		{"wildcard", "*DUCKLINGS*", []api.Ref{
			entries.third.Ref, entries.first.Ref,
		}},
		{"single character", "fix nes?", []api.Ref{entries.second.Ref}},
		{"character class", "fi[xz] nest", []api.Ref{entries.second.Ref}},
		{"non-ASCII is unchanged", "æther", nil},
		{"matching non-ASCII", "ÆTHER", []api.Ref{entries.fifth.Ref}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := runSearch(t, entries.store, Filter{
				Subject: []FieldCond{{Value: test.pattern}},
			})
			assertEntryRefs(t, result.Entries, test.want...)
		})
	}
}

func TestSearchMatchesTypeWholeValue(t *testing.T) {
	entries := newSearchEntries(t)
	result := runSearch(t, entries.store, Filter{
		Type: []FieldCond{{Value: "not"}},
	})
	assertEntryRefs(t, result.Entries)

	result = runSearch(t, entries.store, Filter{
		Type: []FieldCond{{Value: "not*"}},
	})
	assertEntryRefs(t, result.Entries, entries.fifth.Ref, entries.third.Ref)
}

func TestSearchSetAndPairNegationUsesNotExists(t *testing.T) {
	entries := newSearchEntries(t)
	result := runSearch(t, entries.store, Filter{
		Key: []FieldCond{{Value: "JIRA"}},
	})
	assertEntryRefs(t, result.Entries, entries.third.Ref, entries.first.Ref)

	result = runSearch(t, entries.store, Filter{
		Key: []FieldCond{{Negate: true, Value: "JIRA"}},
	})
	assertEntryRefs(t, result.Entries,
		entries.fifth.Ref, entries.fourth.Ref, entries.second.Ref)

	result = runSearch(t, entries.store, Filter{
		Meta: []MetaCond{{Key: "project", Value: "DUCK-*"}},
	})
	assertEntryRefs(t, result.Entries,
		entries.third.Ref, entries.second.Ref, entries.first.Ref)

	result = runSearch(t, entries.store, Filter{
		Meta: []MetaCond{{Key: "project", Value: "DUCK-1"}},
	})
	assertEntryRefs(t, result.Entries)

	result = runSearch(t, entries.store, Filter{
		Meta: []MetaCond{{Negate: true, Key: "project", Value: "DUCK-POND"}},
	})
	assertEntryRefs(t, result.Entries,
		entries.fifth.Ref, entries.fourth.Ref, entries.second.Ref)
}

func TestSearchContentPagingAndMetadata(t *testing.T) {
	entries := newSearchEntries(t)
	result := runSearch(t, entries.store, Filter{
		Content: []FieldCond{{Value: "*DUCKLINGS*"}},
		Limit:   2,
		Offset:  1,
	})
	if result.More {
		t.Fatal("more after the last of 3 matches")
	}
	assertEntryRefs(t, result.Entries, entries.second.Ref, entries.first.Ref)
	if first := runSearch(t, entries.store, Filter{
		Content: []FieldCond{{Value: "*DUCKLINGS*"}}, Limit: 2,
	}); !first.More {
		t.Fatal("no more after the first 2 of 3 matches")
	}
	if got := result.Entries[1].Metadata["jira"]; got != "QUACK-1" {
		t.Fatalf("paged metadata jira = %q, want QUACK-1", got)
	}
	if result.Entries[0].Metadata == nil {
		t.Fatal("entry metadata map is nil")
	}
}

func TestSearchOrdersEditedEntryFirstByDefault(t *testing.T) {
	entries := newSearchEntries(t)
	oldTime := "2000-01-01T00:00:00.000000000Z"
	if _, err := entries.store.db.Exec(
		`UPDATE entries SET modified_utc = ?`, oldTime,
	); err != nil {
		t.Fatalf("setting modification times: %v", err)
	}
	if _, err := entries.store.Edit(
		context.Background(), everyAccess(entries.store),
		entries.store.mustKey(entries.first.Ref), api.EditEntryRequest{
			Metadata: map[string]string{"jira": "QUACK-9"},
		},
	); err != nil {
		t.Fatalf("editing metadata: %v", err)
	}

	result := runSearch(t, entries.store, Filter{})
	if result.Entries[0].Ref != entries.first.Ref {
		t.Fatalf("first entry = %s, want the edited %s",
			result.Entries[0].Ref, entries.first.Ref)
	}

	byCreation := runSearch(t, entries.store, Filter{OrderByCreation: true})
	if byCreation.Entries[0].Ref != entries.fifth.Ref {
		t.Fatalf("first entry by creation = %s, want the newest %s",
			byCreation.Entries[0].Ref, entries.fifth.Ref)
	}
}

func TestSearchRejectsInvalidBoundsAndNUL(t *testing.T) {
	store := openTestStore(t)
	tests := []struct {
		name   string
		filter Filter
		want   string
	}{
		{"zero limit", Filter{}, "limit"},
		{"large limit", Filter{Limit: MaxSearchLimit + 1}, "limit"},
		{"negative offset", Filter{Limit: 1, Offset: -1}, "offset"},
		{"NUL pattern", Filter{
			Limit: 1,
			Body:  []FieldCond{{Value: "bad\x00pattern"}},
		}, "NUL"},
		{"long pattern", Filter{
			Limit: 1,
			Body: []FieldCond{{
				Value: strings.Repeat("x", MaxSearchPatternLength+1),
			}},
		}, "exceeds"},
		{"too many conditions", Filter{
			Limit: 1,
			Subject: make(
				[]FieldCond, MaxSearchConditions+1,
			),
		}, "conditions"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := store.Search(context.Background(),
				everyAccess(store), workspaceID(t, store, "default"),
				test.filter)
			if !errors.Is(err, ErrInvalidFilter) ||
				!strings.Contains(err.Error(), test.want) {
				t.Fatalf("Search error = %v, want text %q", err, test.want)
			}
		})
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := store.Search(ctx, everyAccess(store),
		workspaceID(t, store, "default"), Filter{Limit: 1})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Search error = %v, want context.Canceled", err)
	}
}

func TestSearchByOrigin(t *testing.T) {
	store := openTestStore(t)
	add := func(subject string, origin api.Origin) api.Entry {
		t.Helper()
		return addTo(t, store, "default", api.AddEntryRequest{
			Type: "note", Subject: subject, Origin: origin,
		})
	}
	first := add("on the first host", api.Origin{
		User: "duck", Host: "POND-A",
		Dir: "/work/pond-a/src", ClaudeSession: "abc-123",
	})
	second := add("on the second host", api.Origin{
		User: "duck", Host: "pond-b",
		Dir: "/work/pond-b/envs",
	})

	// Host match folds ASCII case, like the other text fields.
	got := runSearch(t, store, Filter{
		Limit: 50, OriginHost: []FieldCond{{Value: "pond-a"}},
	})
	assertEntryRefs(t, got.Entries, first.Ref)

	// Directory glob and session match reach only their entry.
	got = runSearch(t, store, Filter{
		Limit: 50, OriginDir: []FieldCond{{Value: "*/envs"}},
	})
	assertEntryRefs(t, got.Entries, second.Ref)
	got = runSearch(t, store, Filter{
		Limit: 50, OriginClaudeSession: []FieldCond{{Value: "abc-*"}},
	})
	assertEntryRefs(t, got.Entries, first.Ref)

	// Negation excludes the matching origin.
	got = runSearch(t, store, Filter{
		Limit: 50, OriginHost: []FieldCond{{Negate: true, Value: "pond-a"}},
	})
	assertEntryRefs(t, got.Entries, second.Ref)
}

// Entries share numbers across workspaces, so metadata filters must
// match only the metadata of the entry in the searched workspace.
func TestSearchMetadataStaysInItsWorkspace(t *testing.T) {
	store := openTestStore(t)
	inDefault := mustAdd(t, store, "note", "in default", "",
		map[string]string{"project": "pond"})
	addTo(t, store, "personal", api.AddEntryRequest{
		Type: "note", Subject: "in personal",
		Metadata: map[string]string{"project": "secret", "jira": "QUACK-9"},
	})

	for _, filter := range []Filter{
		{Meta: []MetaCond{{Key: "project", Value: "secret"}}},
		{Meta: []MetaCond{{Key: "jira", Value: "*"}}},
		{Key: []FieldCond{{Value: "jira"}}},
	} {
		result := runSearch(t, store, filter)
		assertEntryRefs(t, result.Entries)
	}
	result := runSearch(t, store, Filter{
		Meta: []MetaCond{{Negate: true, Key: "jira", Value: "*"}},
	})
	assertEntryRefs(t, result.Entries, inDefault.Ref)
}
