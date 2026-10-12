package store

import (
	"context"
	"database/sql"
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
	{Name: "general", Prefix: "e"},
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
	return NewAccess("", ids...)
}

func everyAccess(store *Store) Access {
	var ids []int64
	for _, workspace := range store.Workspaces() {
		ids = append(ids, workspace.ID)
	}
	return NewAccess("", ids...)
}

// accessAs can read every workspace, as user.
func accessAs(store *Store, user string) Access {
	return NewAccess(user, everyAccess(store).workspaces...)
}

// testUser posts the posts and messages of tests that name no other
// user, and makes their edits.
const testUser = "duck"

// addTo adds a post, posted by testUser unless req names a user.
func addTo(
	t *testing.T, store *Store, workspace string, req api.AddPostRequest,
) api.Post {
	t.Helper()
	if req.Origin.User == "" {
		req.Origin.User = testUser
	}
	post, _, err := store.Add(context.Background(),
		accessAs(store, req.Origin.User), workspaceID(t, store, workspace),
		req)
	if err != nil {
		t.Fatalf("adding post: %v", err)
	}
	return post
}

// mustAdd adds a post to the general workspace.
func mustAdd(
	t *testing.T,
	store *Store,
	subject string,
	body string,
	metadata map[string]string,
) api.Post {
	t.Helper()
	return addTo(t, store, "general", api.AddPostRequest{
		Subject: subject, Body: body, Metadata: metadata,
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
		store.mustKey(ref),
		api.EditPostRequest{AddLinks: links, User: testUser})
	return err
}

func mustGet(t *testing.T, store *Store, ref api.Ref) api.PostDetail {
	t.Helper()
	detail, err := store.Detail(context.Background(), everyAccess(store),
		store.mustKey(ref))
	if err != nil {
		t.Fatalf("getting %s: %v", ref, err)
	}
	return detail
}

// matchRefs names a page's matches, a post by its ref and a message by
// its own.
func matchRefs(page api.MatchPage) []string {
	refs := []string{}
	for _, m := range page.Matches {
		if m.Message != nil {
			refs = append(refs, m.Message.Ref().String())
		} else {
			refs = append(refs, m.Post.Ref.String())
		}
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

func TestPostIDsCountFromOnePerWorkspace(t *testing.T) {
	store := openTestStore(t)
	var got []string
	for _, workspace := range []string{"general", "personal", "general",
		"general", "personal"} {
		post := addTo(t, store, workspace,
			api.AddPostRequest{Subject: "s"})
		got = append(got, post.Ref.String())
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
			workspace := []string{"general", "personal"}[i%2]
			post, _, err := store.Add(context.Background(),
				everyAccess(store), workspaceID(t, store, workspace),
				api.AddPostRequest{Subject: "s"})
			if err != nil {
				t.Errorf("adding post: %v", err)
				return
			}
			refs <- post.Ref
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
	post := mustAdd(
		t, store, "COUNT Æ", "BODY Σ",
		map[string]string{"jira": "QUACK Æ"},
	)
	assertFolded := func(
		wantSubject string,
		wantBody string,
		wantValue string,
	) {
		t.Helper()
		var subject, body, value string
		err := store.db.QueryRowContext(ctx, `
			SELECT e.subject_lower, e.body_lower, metadata.value_lower
			FROM posts AS e
			JOIN post_metadata AS metadata
				ON metadata.workspace_id = e.workspace_id
				AND metadata.post_id = e.id
			WHERE e.id = ?`, post.Ref.ID,
		).Scan(&subject, &body, &value)
		if err != nil {
			t.Fatalf("reading folded values: %v", err)
		}
		if subject != wantSubject || body != wantBody ||
			value != wantValue {
			t.Fatalf(
				"folded values = %q, %q, %q; want %q, %q, %q",
				subject, body, value, wantSubject, wantBody, wantValue,
			)
		}
	}
	assertFolded("count Æ", "body Σ", "quack Æ")

	subject := "CHANGED Æ"
	body := "CHANGED Σ"
	if _, err := store.Edit(ctx, everyAccess(store),
		store.mustKey(post.Ref), api.EditPostRequest{
			Subject: &subject, Body: &body,
			Metadata: map[string]string{"jira": "CHANGED Æ"},
			User:     testUser,
		}); err != nil {
		t.Fatalf("editing post: %v", err)
	}
	assertFolded("changed Æ", "changed Σ", "changed Æ")
}

func TestMutationRollsBackWhenResultCannotBeRead(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	_, err := store.db.ExecContext(ctx, `
		CREATE TRIGGER corrupt_new_post AFTER INSERT ON posts
		BEGIN
			UPDATE posts SET created_utc = 'bad'
			WHERE workspace_id = NEW.workspace_id AND id = NEW.id;
		END`)
	if err != nil {
		t.Fatalf("creating trigger: %v", err)
	}
	_, _, err = store.Add(ctx, everyAccess(store),
		workspaceID(t, store, "general"),
		api.AddPostRequest{Subject: "must roll back"})
	if err == nil {
		t.Fatal("add succeeded despite unreadable result")
	}
	var count int
	if err := store.db.QueryRowContext(
		ctx, `SELECT count(*) FROM posts`,
	).Scan(&count); err != nil {
		t.Fatalf("counting posts: %v", err)
	}
	if count != 0 {
		t.Fatalf("failed add committed %d posts", count)
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
			post := mustAdd(t, store, "post", "",
				map[string]string{"jira": "QUACK-1"})
			oldTime := "2000-01-01T00:00:00.000000000Z"
			if _, err := store.db.ExecContext(ctx, `
				UPDATE posts SET modified_utc = ?`, oldTime,
			); err != nil {
				t.Fatalf("setting old modification time: %v", err)
			}
			got, err := store.Edit(ctx, everyAccess(store),
				store.mustKey(post.Ref),
				api.EditPostRequest{Metadata: test.set, User: testUser})
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
	_, _, err := store.Add(ctx, everyAccess(store),
		workspaceID(t, store, "general"),
		api.AddPostRequest{Subject: "canceled"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("add error = %v, want context.Canceled", err)
	}
	var count int
	if err := store.db.QueryRow(`SELECT count(*) FROM posts`).Scan(
		&count,
	); err != nil {
		t.Fatalf("counting posts: %v", err)
	}
	if count != 0 {
		t.Fatalf("canceled add wrote %d posts", count)
	}
}

func TestBusyDatabaseIsRecognized(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "busy.db")
	first := openStoreAt(t, path, testBusyTimeout)
	second := openStoreAt(t, path, 5*time.Millisecond)
	mustAdd(t, first, "locked", "", nil)

	tx, err := first.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("starting lock transaction: %v", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`UPDATE posts SET subject = subject`); err != nil {
		t.Fatalf("locking database: %v", err)
	}
	_, _, err = second.Add(ctx, everyAccess(second),
		workspaceID(t, second, "general"),
		api.AddPostRequest{Subject: "blocked"})
	if !IsBusy(err) {
		t.Fatalf("add error = %v, want SQLite busy/locked", err)
	}
	if !IsBusy(errors.Join(errors.New("wrapper"), err)) {
		t.Fatal("IsBusy did not inspect wrapped error")
	}
}

func TestMetadataLimitStopsPostGrowth(t *testing.T) {
	store := openTestStore(t)
	metadata := make(map[string]string, api.MaxMetadata)
	for index := range api.MaxMetadata {
		metadata[fmt.Sprintf("key-%d", index)] = "value"
	}
	post := mustAdd(t, store, "bounded", "", metadata)
	ctx := context.Background()
	key := store.mustKey(post.Ref)
	_, err := store.Edit(ctx, everyAccess(store), key, api.EditPostRequest{
		Metadata: map[string]string{"overflow": "value"},
		User:     testUser,
	})
	if !errors.Is(err, ErrMetadataLimit) {
		t.Fatalf("Edit error = %v, want ErrMetadataLimit", err)
	}

	edited, err := store.Edit(ctx, everyAccess(store), key,
		api.EditPostRequest{
			Metadata: map[string]string{"key-0": "replaced"},
			User:     testUser,
		})
	if err != nil {
		t.Fatalf("overwriting a metadata value at the limit: %v", err)
	}
	if got := edited.Metadata["key-0"]; got != "replaced" {
		t.Fatalf("key-0 = %q, want replaced", got)
	}
}

// A read holds a read connection, so a write goes ahead beside it, and
// the read connections refuse writes.
func TestWritesProceedWhileAReadIsOpen(t *testing.T) {
	store := openTestStore(t)
	mustAdd(t, store, "before the read", "", nil)
	ctx := context.Background()
	read, err := store.begin(ctx, everyAccess(store), true)
	if err != nil {
		t.Fatal(err)
	}
	defer read.tx.Rollback()
	var count int
	if err := read.tx.QueryRowContext(
		ctx, `SELECT count(*) FROM posts`,
	).Scan(&count); err != nil {
		t.Fatal(err)
	}

	writeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, _, err := store.Add(writeCtx, everyAccess(store),
		workspaceID(t, store, "general"), api.AddPostRequest{
			Subject: "during the read",
		}); err != nil {
		t.Fatalf("adding while a read is open: %v", err)
	}
	if _, err := store.reads.ExecContext(
		ctx, `DELETE FROM post_metadata`,
	); err == nil {
		t.Fatal("a read connection accepted a write")
	}
}

// holdWriter starts a write that holds the write connection and the
// database's write lock until it is rolled back.
func holdWriter(t *testing.T, store *Store) *sql.Tx {
	t.Helper()
	ctx := context.Background()
	write, err := store.begin(ctx, everyAccess(store), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { write.tx.Rollback() })
	if _, err := write.tx.ExecContext(ctx,
		`UPDATE revision_counter SET revision = revision`); err != nil {
		t.Fatal(err)
	}
	return write.tx
}

// The polls of waits and chats, and every other read, answer while a
// write holds the write connection.
func TestReadsProceedWhileAWriteIsOpen(t *testing.T) {
	store := openTestStore(t)
	post := mustAdd(t, store, "polled", "", nil)
	mustPostMessage(t, store, post.Ref, "a message")
	key := store.mustKey(post.Ref)
	access := everyAccess(store)
	exclude := &OriginKey{User: "someone else"}
	holdWriter(t, store)

	reads := map[string]func(context.Context) error{
		"changes": func(ctx context.Context) error {
			_, err := store.Changes(ctx, access, key.Workspace, 0,
				Filter{}, exclude, 10)
			return err
		},
		"latest revision": func(ctx context.Context) error {
			_, err := store.LatestRevision(ctx)
			return err
		},
		"chat": func(ctx context.Context) error {
			_, err := store.Messages(ctx, everyAccess(store), key, api.ChatBound{}, Filter{},
				exclude, 10)
			return err
		},
		"latest chat": func(ctx context.Context) error {
			_, err := store.LatestMessages(ctx, everyAccess(store), key, 10)
			return err
		},
		"chat overview": func(ctx context.Context) error {
			_, err := store.Overview(ctx, everyAccess(store), key.Workspace, 10)
			return err
		},
		"search": func(ctx context.Context) error {
			_, err := store.Search(ctx, access, key.Workspace,
				Filter{Limit: 10, IncludeChat: true})
			return err
		},
		"detail": func(ctx context.Context) error {
			_, err := store.Detail(ctx, access, key)
			return err
		},
		"tree": func(ctx context.Context) error {
			_, err := store.Tree(ctx, access, key, true, true, 10)
			return err
		},
		"projects": func(ctx context.Context) error {
			_, err := store.Projects(ctx, everyAccess(store), key.Workspace, 10, time.Now())
			return err
		},
		"ping": store.Ping,
	}
	for name, read := range reads {
		ctx, cancel := context.WithTimeout(context.Background(),
			2*time.Second)
		if err := read(ctx); err != nil {
			t.Errorf("%s while a write is open: %v", name, err)
		}
		cancel()
	}
}

// A write that waits for the write connection gives up when its
// context ends, and is not left queued to commit once the connection
// frees.
func TestWriteAbandonedWhileWaitingNeverCommits(t *testing.T) {
	store := openTestStore(t)
	post := mustAdd(t, store, "busy", "", nil)
	key := store.mustKey(post.Ref)
	write := holdWriter(t, store)

	ctx, cancel := context.WithTimeout(context.Background(),
		100*time.Millisecond)
	defer cancel()
	_, _, err := store.PostMessage(ctx, everyAccess(store), key,
		api.PostMessageRequest{Body: "abandoned"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("post error = %v, want context.DeadlineExceeded", err)
	}
	if err := write.Rollback(); err != nil {
		t.Fatal(err)
	}
	if got := readChat(t, store, post.Ref, 0, 10); len(got.Messages) != 0 {
		t.Fatalf("the abandoned write committed %d messages",
			len(got.Messages))
	}
}
