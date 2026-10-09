package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/airspacesh/workstream/internal/api"
)

const testBusyTimeout = time.Second

// contendedOpenBusyTimeout is how long an open waits while another
// open creates the schema. That holds the write lock across fsyncs,
// which take over a second each on slow shared
// storage, so testBusyTimeout is too short.
const contendedOpenBusyTimeout = 30 * time.Second

var testWorkspaces = []api.Workspace{
	{Name: "default", Prefix: "e"},
	{Name: "personal", Prefix: "p"},
}

func openStoreWith(
	t *testing.T,
	path string,
	busyTimeout time.Duration,
	workspaces []api.Workspace,
) *Store {
	t.Helper()
	store, err := Open(context.Background(), path, busyTimeout, workspaces)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("closing store: %v", err)
		}
	})
	return store
}

func openStoreAt(
	t *testing.T, path string, busyTimeout time.Duration,
) *Store {
	t.Helper()
	return openStoreWith(t, path, busyTimeout, testWorkspaces)
}

func openTestStore(t *testing.T) *Store {
	t.Helper()
	return openStoreAt(
		t, filepath.Join(t.TempDir(), "test.db"), testBusyTimeout,
	)
}

func workspaceID(t *testing.T, store *Store, name string) int64 {
	t.Helper()
	for _, workspace := range store.Workspaces() {
		if workspace.Name == name {
			return workspace.ID
		}
	}
	t.Fatalf("no workspace %s", name)
	return 0
}

// accessTo can read the named workspaces.
func accessTo(t *testing.T, store *Store, names ...string) Access {
	t.Helper()
	var ids []int64
	for _, name := range names {
		ids = append(ids, workspaceID(t, store, name))
	}
	return NewAccess(ids...)
}

func everyAccess(store *Store) Access {
	var ids []int64
	for _, workspace := range store.Workspaces() {
		ids = append(ids, workspace.ID)
	}
	return NewAccess(ids...)
}

func addTo(
	t *testing.T, store *Store, workspace string, req api.AddEntryRequest,
) api.Entry {
	t.Helper()
	entry, err := store.Add(context.Background(), everyAccess(store),
		workspaceID(t, store, workspace), req)
	if err != nil {
		t.Fatalf("adding entry: %v", err)
	}
	return entry
}

// mustAdd adds an entry to the default workspace.
func mustAdd(
	t *testing.T,
	store *Store,
	typ string,
	subject string,
	body string,
	metadata map[string]string,
) api.Entry {
	t.Helper()
	return addTo(t, store, "default", api.AddEntryRequest{
		Type: typ, Subject: subject, Body: body, Metadata: metadata,
	})
}

func link(name string, ref api.Ref) api.Link {
	return api.Link{Name: name, Ref: ref}
}

func addLinks(
	t *testing.T, store *Store, ref api.Ref, links ...api.Link,
) error {
	t.Helper()
	_, err := store.Edit(context.Background(), everyAccess(store),
		store.mustKey(ref), api.EditEntryRequest{AddLinks: links})
	return err
}

func mustGet(t *testing.T, store *Store, ref api.Ref) api.EntryDetail {
	t.Helper()
	detail, err := store.Detail(context.Background(), everyAccess(store),
		store.mustKey(ref))
	if err != nil {
		t.Fatalf("getting %s: %v", ref, err)
	}
	return detail
}

func entryRefsOf(entries []api.ListedEntry) []string {
	refs := make([]string, len(entries))
	for i, entry := range entries {
		refs[i] = entry.Ref.String()
	}
	return refs
}

func refStrings(refs ...api.Ref) []string {
	texts := make([]string, len(refs))
	for i, ref := range refs {
		texts[i] = ref.String()
	}
	return texts
}

func TestEntryIDsCountFromOnePerWorkspace(t *testing.T) {
	store := openTestStore(t)
	var got []string
	for _, workspace := range []string{"default", "personal", "default",
		"default", "personal"} {
		entry := addTo(t, store, workspace,
			api.AddEntryRequest{Type: "note", Subject: "s"})
		got = append(got, entry.Ref.String())
	}
	want := fmt.Sprint([]string{"e1", "p1", "e2", "e3", "p2"})
	if fmt.Sprint(got) != want {
		t.Fatalf("issued %v, want %v", got, want)
	}
}

func TestConcurrentAddsGetUniqueIDsCountingFromOne(t *testing.T) {
	store := openTestStore(t)
	const perWorkspace = 15
	var wg sync.WaitGroup
	refs := make(chan api.Ref, 2*perWorkspace)
	for i := range 2 * perWorkspace {
		wg.Add(1)
		go func() {
			defer wg.Done()
			workspace := []string{"default", "personal"}[i%2]
			entry, err := store.Add(context.Background(),
				everyAccess(store), workspaceID(t, store, workspace),
				api.AddEntryRequest{Type: "note", Subject: "s"})
			if err != nil {
				t.Errorf("adding entry: %v", err)
				return
			}
			refs <- entry.Ref
		}()
	}
	wg.Wait()
	close(refs)

	seen := map[api.Ref]bool{}
	for ref := range refs {
		if seen[ref] {
			t.Fatalf("%s was issued twice", ref)
		}
		seen[ref] = true
	}
	for _, prefix := range []string{"e", "p"} {
		for id := int64(1); id <= perWorkspace; id++ {
			if !seen[api.Ref{Prefix: prefix, ID: id}] {
				t.Fatalf("%s%d was never issued: %v", prefix, id, seen)
			}
		}
	}
}

func TestGeneratedColumnsFollowWrites(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	entry := mustAdd(
		t, store, "TODO", "COUNT Æ", "BODY Σ",
		map[string]string{"jira": "QUACK Æ"},
	)
	assertFolded := func(
		wantType string,
		wantSubject string,
		wantBody string,
		wantValue string,
	) {
		t.Helper()
		var typ, subject, body, value string
		err := store.db.QueryRowContext(ctx, `
			SELECT e.type_lower, e.subject_lower, e.body_lower,
				metadata.value_lower
			FROM entries AS e
			JOIN entry_metadata AS metadata
				ON metadata.workspace_id = e.workspace_id
				AND metadata.entry_id = e.id
			WHERE e.id = ?`, entry.Ref.ID,
		).Scan(&typ, &subject, &body, &value)
		if err != nil {
			t.Fatalf("reading folded values: %v", err)
		}
		if typ != wantType || subject != wantSubject ||
			body != wantBody || value != wantValue {
			t.Fatalf(
				"folded values = %q, %q, %q, %q; want %q, %q, %q, %q",
				typ, subject, body, value,
				wantType, wantSubject, wantBody, wantValue,
			)
		}
	}
	assertFolded("todo", "count Æ", "body Σ", "quack Æ")

	entryType := "NOTE"
	subject := "CHANGED Æ"
	body := "CHANGED Σ"
	if _, err := store.Edit(ctx, everyAccess(store),
		store.mustKey(entry.Ref), api.EditEntryRequest{
			Type: &entryType, Subject: &subject, Body: &body,
			Metadata: map[string]string{"jira": "CHANGED Æ"},
		}); err != nil {
		t.Fatalf("editing entry: %v", err)
	}
	assertFolded("note", "changed Æ", "changed Σ", "changed Æ")
}

func TestMutationRollsBackWhenResultCannotBeRead(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	_, err := store.db.ExecContext(ctx, `
		CREATE TRIGGER corrupt_new_entry AFTER INSERT ON entries
		BEGIN
			UPDATE entries SET created_utc = 'bad'
			WHERE workspace_id = NEW.workspace_id AND id = NEW.id;
		END`)
	if err != nil {
		t.Fatalf("creating trigger: %v", err)
	}
	_, err = store.Add(ctx, everyAccess(store),
		workspaceID(t, store, "default"),
		api.AddEntryRequest{Type: "note", Subject: "must roll back"})
	if err == nil {
		t.Fatal("add succeeded despite unreadable result")
	}
	var count int
	if err := store.db.QueryRowContext(
		ctx, `SELECT count(*) FROM entries`,
	).Scan(&count); err != nil {
		t.Fatalf("counting entries: %v", err)
	}
	if count != 0 {
		t.Fatalf("failed add committed %d entries", count)
	}
}

func TestMetadataEditsRefreshModifiedAndApply(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name string
		set  map[string]string
		want map[string]string
	}{
		{
			"add",
			map[string]string{"review": "done"},
			map[string]string{"jira": "QUACK-1", "review": "done"},
		},
		{
			"overwrite",
			map[string]string{"jira": "QUACK-2"},
			map[string]string{"jira": "QUACK-2"},
		},
		{
			"remove",
			map[string]string{"jira": ""},
			map[string]string{},
		},
		{
			"remove a key that was never set",
			map[string]string{"review": ""},
			map[string]string{"jira": "QUACK-1"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := openTestStore(t)
			entry := mustAdd(t, store, "note", "entry", "",
				map[string]string{"jira": "QUACK-1"})
			oldTime := "2000-01-01T00:00:00.000000000Z"
			if _, err := store.db.ExecContext(ctx, `
				UPDATE entries SET modified_utc = ?`, oldTime,
			); err != nil {
				t.Fatalf("setting old modification time: %v", err)
			}
			got, err := store.Edit(ctx, everyAccess(store),
				store.mustKey(entry.Ref),
				api.EditEntryRequest{Metadata: test.set})
			if err != nil {
				t.Fatalf("changing metadata: %v", err)
			}
			if got.Modified.Year() == 2000 {
				t.Fatalf("modified was not refreshed: %v", got.Modified)
			}
			if fmt.Sprint(got.Metadata) != fmt.Sprint(test.want) {
				t.Fatalf("metadata = %v, want %v", got.Metadata, test.want)
			}
		})
	}
}

func TestCanceledMutationDoesNotWrite(t *testing.T) {
	store := openTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := store.Add(ctx, everyAccess(store),
		workspaceID(t, store, "default"),
		api.AddEntryRequest{Type: "note", Subject: "canceled"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("add error = %v, want context.Canceled", err)
	}
	var count int
	if err := store.db.QueryRow(`SELECT count(*) FROM entries`).Scan(
		&count,
	); err != nil {
		t.Fatalf("counting entries: %v", err)
	}
	if count != 0 {
		t.Fatalf("canceled add wrote %d entries", count)
	}
}

func TestBusyDatabaseIsRecognized(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "busy.db")
	first := openStoreAt(t, path, testBusyTimeout)
	second := openStoreAt(t, path, 5*time.Millisecond)
	mustAdd(t, first, "note", "locked", "", nil)

	tx, err := first.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("starting lock transaction: %v", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`UPDATE entries SET subject = subject`); err != nil {
		t.Fatalf("locking database: %v", err)
	}
	_, err = second.Add(ctx, everyAccess(second),
		workspaceID(t, second, "default"),
		api.AddEntryRequest{Type: "note", Subject: "blocked"})
	if !IsBusy(err) {
		t.Fatalf("add error = %v, want SQLite busy/locked", err)
	}
	if !IsBusy(errors.Join(errors.New("wrapper"), err)) {
		t.Fatal("IsBusy did not inspect wrapped error")
	}
}

func TestMetadataLimitStopsEntryGrowth(t *testing.T) {
	store := openTestStore(t)
	metadata := make(map[string]string, api.MaxMetadata)
	for index := range api.MaxMetadata {
		metadata[fmt.Sprintf("key-%d", index)] = "value"
	}
	entry := mustAdd(t, store, "note", "bounded", "", metadata)
	ctx := context.Background()
	key := store.mustKey(entry.Ref)
	_, err := store.Edit(ctx, everyAccess(store), key, api.EditEntryRequest{
		Metadata: map[string]string{"overflow": "value"},
	})
	if !errors.Is(err, ErrMetadataLimit) {
		t.Fatalf("Edit error = %v, want ErrMetadataLimit", err)
	}

	edited, err := store.Edit(ctx, everyAccess(store), key,
		api.EditEntryRequest{
			Metadata: map[string]string{"key-0": "replaced"},
		})
	if err != nil {
		t.Fatalf("overwriting a metadata value at the limit: %v", err)
	}
	if got := edited.Metadata["key-0"]; got != "replaced" {
		t.Fatalf("key-0 = %q, want replaced", got)
	}
}

func TestProjectsCountOneWorkspace(t *testing.T) {
	store := openTestStore(t)
	pond := map[string]string{"project": "pond"}
	mustAdd(t, store, "note", "a", "", pond)
	mustAdd(t, store, "note", "b", "", pond)
	addTo(t, store, "personal", api.AddEntryRequest{
		Type: "note", Subject: "c", Metadata: pond})

	projects, err := store.Projects(context.Background(),
		workspaceID(t, store, "default"))
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 1 || projects[0].Entries != 2 {
		t.Fatalf("projects = %+v, want pond with 2 entries", projects)
	}
}
