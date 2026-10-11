package store

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

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
	add := func(
		project, subject, body string, metadata map[string]string,
	) api.Entry {
		t.Helper()
		return addTo(t, store, "default", api.AddEntryRequest{
			Project: project, Subject: subject, Body: body,
			Metadata: metadata,
		})
	}
	return searchEntries{
		store: store,
		first: add("duck-pond", "Count the Ducklings", "",
			map[string]string{"jira": "QUACK-1"}),
		second: add("Duck-Nest", "Fix Nest", "Ducklings are safe", nil),
		third: add("duck-pond", "Ducklings restless", "",
			map[string]string{"jira": "QUACK-2"}),
		fourth: add("feed", "Cache feed", "", nil),
		fifth:  add("æther", "Æther", "", nil),
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

func assertEntryRefs(
	t *testing.T, entries []api.MatchedEntry, want ...api.Ref,
) {
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
		Meta: []MetaCond{{Key: "jira", Value: "QUACK-*"}},
	})
	assertEntryRefs(t, result.Entries, entries.third.Ref, entries.first.Ref)

	result = runSearch(t, entries.store, Filter{
		Meta: []MetaCond{{Key: "jira", Value: "QUACK"}},
	})
	assertEntryRefs(t, result.Entries)

	result = runSearch(t, entries.store, Filter{
		Meta: []MetaCond{{Negate: true, Key: "jira", Value: "QUACK-1"}},
	})
	assertEntryRefs(t, result.Entries, entries.fifth.Ref,
		entries.fourth.Ref, entries.third.Ref, entries.second.Ref)
}

func TestSearchMatchesProjectWholeValue(t *testing.T) {
	entries := newSearchEntries(t)
	result := runSearch(t, entries.store, Filter{
		Project: []FieldCond{{Value: "DUCK-*"}},
	})
	assertEntryRefs(t, result.Entries,
		entries.third.Ref, entries.second.Ref, entries.first.Ref)

	result = runSearch(t, entries.store, Filter{
		Project: []FieldCond{{Value: "duck"}},
	})
	assertEntryRefs(t, result.Entries)

	result = runSearch(t, entries.store, Filter{
		Project: []FieldCond{{Negate: true, Value: "DUCK-POND"}},
	})
	assertEntryRefs(t, result.Entries,
		entries.fifth.Ref, entries.fourth.Ref, entries.second.Ref)

	result = runSearch(t, entries.store, Filter{
		Project: []FieldCond{{Value: "ÆTHER"}},
	})
	assertEntryRefs(t, result.Entries)
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
			User:     testUser,
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
			Subject: subject, Origin: origin,
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
	inDefault := mustAdd(t, store, "in default", "", nil)
	addTo(t, store, "personal", api.AddEntryRequest{
		Subject: "in personal", Project: "secret",
		Metadata: map[string]string{"jira": "QUACK-9"},
	})

	for _, filter := range []Filter{
		{Project: []FieldCond{{Value: "secret"}}},
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

func messageBodies(messages []api.Message) []string {
	var bodies []string
	for _, message := range messages {
		bodies = append(bodies, message.Body)
	}
	return bodies
}

func TestSearchMatchesChatMessages(t *testing.T) {
	store := openTestStore(t)
	pond := mustAdd(t, store, "Pond notes", "", nil)
	sighting := mustAdd(t, store, "Heron sighting", "", nil)
	quiet := mustAdd(t, store, "Count ducks", "", nil)
	for _, body := range []string{"a heron landed", "nothing here",
		"HERON again", "heron thrice", "heron four"} {
		mustPost(t, store, pond.Ref, body)
	}
	mustPost(t, store, quiet.Ref, "no birds")
	heron := []FieldCond{{Value: "heron"}}

	result := runSearch(t, store, Filter{Content: heron})
	assertEntryRefs(t, result.Entries, pond.Ref, sighting.Ref)
	if got, want := messageBodies(result.Entries[0].MatchingMessages),
		[]string{"a heron landed", "HERON again", "heron thrice",
			"heron four"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("matching messages = %q, want %q", got, want)
	}
	if got := result.Entries[1].MatchingMessages; len(got) != 0 {
		t.Fatalf("an entry with no matching chat shows messages %v", got)
	}

	byCreation := runSearch(t, store, Filter{
		Content: heron, OrderByCreation: true,
	})
	assertEntryRefs(t, byCreation.Entries, sighting.Ref, pond.Ref)

	for offset, want := range []api.Ref{pond.Ref, sighting.Ref} {
		page := runSearch(t, store, Filter{
			Content: heron, Limit: 1, Offset: offset,
		})
		assertEntryRefs(t, page.Entries, want)
		if page.More != (offset == 0) {
			t.Fatalf("more at offset %d = %v", offset, page.More)
		}
	}

	entriesOnly := runSearch(t, store, Filter{
		Content: heron, EntriesOnly: true,
	})
	assertEntryRefs(t, entriesOnly.Entries, sighting.Ref)

	// The other filters apply to the entry a message is in.
	sightings := runSearch(t, store, Filter{
		Content: heron, Subject: []FieldCond{{Value: "sighting"}},
	})
	assertEntryRefs(t, sightings.Entries, sighting.Ref)

	// Excluding text alone does not search chat, or every entry with
	// one message lacking the text would match.
	without := runSearch(t, store, Filter{
		Content: []FieldCond{{Negate: true, Value: "heron"}},
	})
	assertEntryRefs(t, without.Entries, quiet.Ref, pond.Ref)
	for _, entry := range without.Entries {
		if len(entry.MatchingMessages) != 0 {
			t.Fatalf("%s shows messages %v", entry.Ref,
				entry.MatchingMessages)
		}
	}
}

// A message matches when it matches every text filter, required and
// excluded, on its own.
func TestSearchMatchesEachMessageOnItsOwn(t *testing.T) {
	store := openTestStore(t)
	split := mustAdd(t, store, "Split", "", nil)
	mustPost(t, store, split.Ref, "heron")
	mustPost(t, store, split.Ref, "egret")
	both := mustAdd(t, store, "Both", "", nil)
	mustPost(t, store, both.Ref, "heron and egret")
	mustPost(t, store, both.Ref, "heron alone")

	result := runSearch(t, store, Filter{Content: []FieldCond{
		{Value: "heron"}, {Value: "egret"},
	}})
	assertEntryRefs(t, result.Entries, both.Ref)

	result = runSearch(t, store, Filter{Content: []FieldCond{
		{Value: "heron"}, {Negate: true, Value: "egret"},
	}})
	assertEntryRefs(t, result.Entries, both.Ref, split.Ref)
	got := messageBodies(result.Entries[0].MatchingMessages)
	if !reflect.DeepEqual(got, []string{"heron alone"}) {
		t.Fatalf("matching messages = %q, want only the one without egret",
			got)
	}
}

// A message matches the text and origin filters on its own, and the
// other filters through its entry.
func TestSearchMatchesMessagesByOrigin(t *testing.T) {
	store := openTestStore(t)
	add := func(user, project, subject string) api.Entry {
		t.Helper()
		return addTo(t, store, "default", api.AddEntryRequest{
			Project: project, Subject: subject,
			Origin: api.Origin{User: user},
		})
	}
	design := add("ines", "build", "Hook design")
	release := add("ken", "build", "Release notes")
	cats := add("ines", "pets", "Cats")
	garden := add("ken", "garden", "Garden")
	postAs(t, store, "ken", release.Ref, "hook fails")
	postAs(t, store, "ines", release.Ref, "hook fixed")
	postAs(t, store, "ines", release.Ref, "lunch")
	postAs(t, store, "ken", cats.Ref, "hook question")
	postAs(t, store, "ines", garden.Ref, "hook in the garden")
	hook := []FieldCond{{Value: "hook"}}
	ines := []FieldCond{{Value: "ines"}}
	notInes := []FieldCond{{Negate: true, Value: "ines"}}

	shown := func(result api.SearchResult) map[string][]string {
		bodies := map[string][]string{}
		for _, entry := range result.Entries {
			bodies[entry.Subject] = messageBodies(entry.MatchingMessages)
		}
		return bodies
	}
	tests := []struct {
		name   string
		filter Filter
		want   []api.Ref
		shown  map[string][]string
	}{
		{"text and origin match the same message",
			Filter{Content: hook, OriginUser: ines},
			[]api.Ref{garden.Ref, release.Ref, design.Ref},
			map[string][]string{"Garden": {"hook in the garden"},
				"Release notes": {"hook fixed"}, "Hook design": nil}},
		{"an origin alone searches chat",
			Filter{OriginUser: ines},
			[]api.Ref{garden.Ref, release.Ref, cats.Ref, design.Ref},
			map[string][]string{"Garden": {"hook in the garden"},
				"Release notes": {"hook fixed", "lunch"}, "Cats": nil,
				"Hook design": nil}},
		{"the other filters apply to a message's entry",
			Filter{OriginUser: ines, Project: []FieldCond{{Value: "build"}}},
			[]api.Ref{release.Ref, design.Ref},
			map[string][]string{"Release notes": {"hook fixed", "lunch"},
				"Hook design": nil}},
		{"an excluded origin excludes messages too",
			Filter{Content: hook, OriginUser: notInes},
			[]api.Ref{cats.Ref, release.Ref},
			map[string][]string{"Cats": {"hook question"},
				"Release notes": {"hook fails"}}},
		{"an excluded origin alone does not search chat",
			Filter{OriginUser: notInes},
			[]api.Ref{garden.Ref, release.Ref},
			map[string][]string{"Garden": nil, "Release notes": nil}},
		{"entries only",
			Filter{OriginUser: ines, EntriesOnly: true},
			[]api.Ref{cats.Ref, design.Ref},
			map[string][]string{"Cats": nil, "Hook design": nil}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := runSearch(t, store, test.filter)
			assertEntryRefs(t, result.Entries, test.want...)
			if got := shown(result); !reflect.DeepEqual(got, test.shown) {
				t.Fatalf("matching messages = %q, want %q", got, test.shown)
			}
		})
	}
}

// The text indexes find an old entry by its own text, and its messages
// then match by origin.
func TestSearchMatchesMessagesByOriginInEntriesTheIndexFinds(t *testing.T) {
	store := openTestStore(t)
	heron := mustAdd(t, store, "Heron at dawn", "", nil)
	postAs(t, store, "ines", heron.Ref, "seen twice")
	postAs(t, store, "ken", heron.Ref, "seen once")
	addFiller(t, store)

	result := runSearch(t, store, Filter{
		Subject:    []FieldCond{{Value: "heron"}},
		OriginUser: []FieldCond{{Value: "ines"}},
	})
	assertEntryRefs(t, result.Entries, heron.Ref)
	got := messageBodies(result.Entries[0].MatchingMessages)
	if !reflect.DeepEqual(got, []string{"seen twice"}) {
		t.Fatalf("matching messages = %q, want ines's alone", got)
	}
}

func messageRefs(messages []api.Message) []string {
	var refs []string
	for _, message := range messages {
		refs = append(refs, message.Ref().String())
	}
	return refs
}

// The messages a page shows come from one budget, filled with the
// newest matches across its entries, so a busy entry can take most of
// it and an older one none.
func TestSearchSharesOneMessageBudgetAcrossThePage(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	oldest := mustAdd(t, store, "Oldest", "", nil)
	busy := mustAdd(t, store, "Busy", "", nil)
	quiet := mustAdd(t, store, "Quiet", "", nil)
	mustPost(t, store, oldest.Ref, "heron at dawn")
	for i := range matchingMessageBudget {
		mustPost(t, store, busy.Ref, fmt.Sprintf("heron %d", i+1))
		mustPost(t, store, busy.Ref, "no birds")
	}
	mustPost(t, store, quiet.Ref, "heron once")
	mustPost(t, store, quiet.Ref, "heron twice")
	heron := []FieldCond{{Value: "heron"}}

	result := runSearch(t, store, Filter{Content: heron})
	assertEntryRefs(t, result.Entries, quiet.Ref, busy.Ref, oldest.Ref)
	wantBusy := matchingMessageBudget - 2
	got := result.Entries[1].MatchingMessages
	if len(got) != wantBusy || got[0].Body != "heron 3" ||
		got[len(got)-1].Body != fmt.Sprintf("heron %d",
			matchingMessageBudget) {
		t.Fatalf("busy entry shows %q, want its newest %d oldest first",
			messageBodies(got), wantBusy)
	}
	shown := map[string]int{}
	more := map[string]int{}
	for _, entry := range result.Entries {
		shown[entry.Subject] = len(entry.MatchingMessages)
		more[entry.Subject] = entry.MoreMatchingMessages
	}
	if want := map[string]int{"Quiet": 2, "Busy": wantBusy,
		"Oldest": 0}; !reflect.DeepEqual(shown, want) {
		t.Fatalf("messages shown = %v, want %v", shown, want)
	}
	if want := map[string]int{"Quiet": 0, "Busy": 2,
		"Oldest": 1}; !reflect.DeepEqual(more, want) {
		t.Fatalf("matching messages left out = %v, want %v", more, want)
	}

	// An edit lifts the entry, which ranks by its change time, but the
	// edited message keeps its place as the first posted, so it stays
	// out of the budget.
	if _, _, err := store.EditMessage(ctx, store.mustKey(busy.Ref), 1,
		ownEdit("heron again")); err != nil {
		t.Fatal(err)
	}
	result = runSearch(t, store, Filter{Content: heron})
	assertEntryRefs(t, result.Entries, busy.Ref, quiet.Ref, oldest.Ref)
	got = result.Entries[0].MatchingMessages
	if len(got) != wantBusy || got[0].Body != "heron 3" ||
		got[len(got)-1].Body != fmt.Sprintf("heron %d",
			matchingMessageBudget) {
		t.Fatalf("busy entry shows %q, want heron 3 to %d", messageBodies(got),
			matchingMessageBudget)
	}
	if more := result.Entries[0].MoreMatchingMessages; more != 2 {
		t.Fatalf("busy entry leaves out %d matches, want 2", more)
	}
}

// A time filter keeps to the entries and messages last changed within
// a span, from its start up to but not including its end.
func TestSearchKeepsToASpan(t *testing.T) {
	store := openTestStore(t)
	day := func(month time.Month, day int) time.Time {
		return time.Date(2026, month, day, 0, 0, 0, 0, time.UTC)
	}
	setChanged := func(ref api.Ref, message int64, at time.Time) {
		t.Helper()
		key := store.mustKey(ref)
		query := `UPDATE entries SET created_utc = ?1, modified_utc = ?1,
			activity_utc = ?1 WHERE workspace_id = ?2 AND id = ?3`
		if message != 0 {
			query = `UPDATE entry_messages SET created_utc = ?1,
				modified_utc = ?1 WHERE workspace_id = ?2 AND entry_id = ?3
				AND id = ?4`
		}
		if _, err := store.db.Exec(query, formatTime(at), key.Workspace,
			key.ID, message); err != nil {
			t.Fatal(err)
		}
	}
	old := mustAdd(t, store, "Old", "", nil)
	setChanged(old.Ref, 0, day(1, 1))
	mid := mustAdd(t, store, "Mid", "", nil)
	setChanged(mid.Ref, 0, day(2, 1))
	setChanged(mid.Ref, postAs(t, store, "ines", mid.Ref, "late").Number,
		day(3, 1))
	setChanged(mid.Ref, postAs(t, store, "ken", mid.Ref, "early").Number,
		day(1, 15))
	recent := mustAdd(t, store, "Recent", "", nil)
	setChanged(recent.Ref, 0, day(4, 1))

	shown := func(result api.SearchResult) map[string][]string {
		bodies := map[string][]string{}
		for _, entry := range result.Entries {
			bodies[entry.Subject] = messageBodies(entry.MatchingMessages)
		}
		return bodies
	}
	tests := []struct {
		name   string
		filter Filter
		want   []api.Ref
		shown  map[string][]string
	}{
		{"after",
			Filter{Changed: Span{After: day(2, 15)}},
			[]api.Ref{recent.Ref, mid.Ref},
			map[string][]string{"Recent": nil, "Mid": {"late"}}},
		{"before leaves out its own instant",
			Filter{Changed: Span{Before: day(2, 1)}},
			[]api.Ref{mid.Ref, old.Ref},
			map[string][]string{"Mid": {"early"}, "Old": nil}},
		{"after takes in its own instant",
			Filter{Changed: Span{After: day(2, 1), Before: day(2, 2)}},
			[]api.Ref{mid.Ref},
			map[string][]string{"Mid": nil}},
		{"with an origin",
			Filter{Changed: Span{After: day(1, 10)},
				OriginUser: []FieldCond{{Value: "ken"}}},
			[]api.Ref{mid.Ref},
			map[string][]string{"Mid": {"early"}}},
		{"entries only",
			Filter{Changed: Span{After: day(2, 15)}, EntriesOnly: true},
			[]api.Ref{recent.Ref},
			map[string][]string{"Recent": nil}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := runSearch(t, store, test.filter)
			assertEntryRefs(t, result.Entries, test.want...)
			if got := shown(result); !reflect.DeepEqual(got, test.shown) {
				t.Fatalf("matching messages = %q, want %q", got, test.shown)
			}
		})
	}
}
