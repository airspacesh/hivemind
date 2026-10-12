package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/airspacesh/workstream/internal/api"
)

// postFrom posts a message with the given origin.
func postFrom(
	t *testing.T, store *Store, post api.Ref, body string, origin api.Origin,
) api.Message {
	t.Helper()
	message, _, err := store.PostMessage(context.Background(),
		store.mustKey(post), api.PostMessageRequest{Body: body, Origin: origin})
	if err != nil {
		t.Fatalf("posting to %s as %s: %v", post, origin.User, err)
	}
	return message
}

func postAs(
	t *testing.T, store *Store, user string, post api.Ref, body string,
) api.Message {
	t.Helper()
	return postFrom(t, store, post, body, api.Origin{User: user})
}

func mustPostMessage(
	t *testing.T, store *Store, post api.Ref, body string,
) api.Message {
	t.Helper()
	return postAs(t, store, testUser, post, body)
}

// ownEdit is an edit to a message by the user mustPostMessage posts as.
func ownEdit(body string) api.EditMessageRequest {
	return api.EditMessageRequest{Body: body, User: testUser}
}

func readChat(
	t *testing.T, store *Store, post api.Ref, since int64, limit int,
) api.ChatPage {
	t.Helper()
	page, err := store.Messages(context.Background(), store.mustKey(post),
		api.ChatBound{Number: since}, Filter{}, nil, limit)
	if err != nil {
		t.Fatal(err)
	}
	return page
}

// chatText shows a page as its message ids and bodies, its cursor and
// how many messages remain.
func chatText(page api.ChatPage) string {
	var text []string
	for _, message := range page.Messages {
		text = append(text, message.Ref().String()+":"+message.Body)
	}
	return fmt.Sprintf("%s cursor=%d remaining=%d",
		strings.Join(text, " "), page.Cursor, page.Remaining)
}

func TestMessageNumbersCountFromOnePerPost(t *testing.T) {
	store := openTestStore(t)
	first := mustAdd(t, store, "first", "", nil)
	second := mustAdd(t, store, "second", "", nil)
	private := addTo(t, store, "personal",
		api.AddPostRequest{Subject: "private"})

	var got []string
	for _, post := range []api.Ref{first.Ref, second.Ref, first.Ref,
		private.Ref, first.Ref} {
		message := mustPostMessage(t, store, post, "hi")
		got = append(got, message.Ref().String())
	}
	want := []string{"e1-m1", "e2-m1", "e1-m2", "p1-m1", "e1-m3"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("issued %v, want %v", got, want)
	}
}

func TestConcurrentPostsGetUniqueNumbersCountingFromOne(t *testing.T) {
	store := openTestStore(t)
	first := mustAdd(t, store, "first", "", nil)
	second := mustAdd(t, store, "second", "", nil)

	const perPost = 10
	var wg sync.WaitGroup
	refs := make(chan string, 2*perPost)
	for i := range 2 * perPost {
		wg.Add(1)
		go func() {
			defer wg.Done()
			post := []api.Ref{first.Ref, second.Ref}[i%2]
			message, _, err := store.PostMessage(context.Background(),
				store.mustKey(post), api.PostMessageRequest{Body: "hi"})
			if err != nil {
				t.Errorf("posting: %v", err)
				return
			}
			refs <- message.Ref().String()
		}()
	}
	wg.Wait()
	close(refs)
	seen := map[string]bool{}
	for ref := range refs {
		if seen[ref] {
			t.Fatalf("%s was issued twice", ref)
		}
		seen[ref] = true
	}
	for _, post := range []string{"e1", "e2"} {
		for n := 1; n <= perPost; n++ {
			if ref := fmt.Sprintf("%s-m%d", post, n); !seen[ref] {
				t.Fatalf("%s was never issued: %v", ref, seen)
			}
		}
	}
}

func TestChatReadsPageAfterTheCursor(t *testing.T) {
	store := openTestStore(t)
	post := mustAdd(t, store, "thread", "", nil)
	if got := chatText(readChat(t, store, post.Ref, 0, 2)); got !=
		" cursor=0 remaining=0" {
		t.Fatalf("empty chat = %q", got)
	}
	for i := 1; i <= 5; i++ {
		mustPostMessage(t, store, post.Ref, fmt.Sprint(i))
	}

	for _, test := range []struct {
		since int64
		limit int
		want  string
	}{
		{0, 2, "e1-m1:1 e1-m2:2 cursor=2 remaining=3"},
		{2, 2, "e1-m3:3 e1-m4:4 cursor=4 remaining=1"},
		{4, 2, "e1-m5:5 cursor=5 remaining=0"},
		{5, 2, " cursor=5 remaining=0"},
		{9, 2, " cursor=9 remaining=0"},
	} {
		got := chatText(readChat(t, store, post.Ref, test.since,
			test.limit))
		if got != test.want {
			t.Fatalf("since %d limit %d = %q, want %q", test.since,
				test.limit, got, test.want)
		}
	}

	latest, err := store.LatestMessages(context.Background(),
		store.mustKey(post.Ref), 3)
	if err != nil {
		t.Fatal(err)
	}
	if got := chatText(latest); got !=
		"e1-m3:3 e1-m4:4 e1-m5:5 cursor=5 remaining=0" {
		t.Fatalf("latest 3 = %q", got)
	}
	if _, err := store.Messages(context.Background(),
		Key{Workspace: workspaceID(t, store, "general"), ID: 99},
		api.ChatBound{}, Filter{}, nil, 2); !errors.Is(err, ErrPostNotFound) {
		t.Fatalf("reading a missing post's chat = %v", err)
	}
}

func TestFilteredChatCursorPassesWhatDoesNotMatch(t *testing.T) {
	store := openTestStore(t)
	post := mustAdd(t, store, "thread", "", nil)
	lead := api.Origin{User: testUser, ClaudeSession: "lead"}
	postFrom(t, store, post.Ref, "asked", lead)
	postFrom(t, store, post.Ref, "quack", api.Origin{User: "heron"})
	postFrom(t, store, post.Ref, "asked again", lead)
	postFrom(t, store, post.Ref, "quack quack", api.Origin{User: testUser})
	postFrom(t, store, post.Ref, "noted", lead)
	notLead := &OriginKey{User: testUser, ClaudeSession: "lead"}
	quack := Filter{Content: []FieldCond{{Value: "QUACK"}}}

	for _, test := range []struct {
		since   int64
		filter  Filter
		exclude *OriginKey
		limit   int
		want    string
	}{
		{0, Filter{}, notLead, 10,
			"e1-m2:quack e1-m4:quack quack cursor=5 remaining=0"},
		{0, Filter{}, notLead, 1, "e1-m2:quack cursor=2 remaining=3"},
		{2, Filter{}, notLead, 1, "e1-m4:quack quack cursor=4 remaining=1"},
		{4, Filter{}, notLead, 1, " cursor=5 remaining=0"},
		{0, quack, &OriginKey{User: testUser}, 10,
			"e1-m2:quack cursor=5 remaining=0"},
		{9, quack, nil, 10, " cursor=9 remaining=0"},
		{0, Filter{OriginUser: []FieldCond{{Value: "heron"}}}, nil, 10,
			"e1-m2:quack cursor=5 remaining=0"},
	} {
		page, err := store.Messages(context.Background(),
			store.mustKey(post.Ref), api.ChatBound{Number: test.since},
			test.filter, test.exclude, test.limit)
		if err != nil {
			t.Fatal(err)
		}
		if got := chatText(page); got != test.want {
			t.Fatalf("since %d excluding %+v limit %d = %q, want %q",
				test.since, test.exclude, test.limit, got, test.want)
		}
	}
}

// A chat read from a time starts at the first message posted at or
// after it, and its cursor passes every message it checked.
func TestChatReadFromATime(t *testing.T) {
	store := openTestStore(t)
	post := mustAdd(t, store, "thread", "", nil)
	key := store.mustKey(post.Ref)
	hour := func(n int) time.Time {
		return time.Date(2026, 10, 9, n, 0, 0, 0, time.UTC)
	}
	for n, body := range []string{"one", "two", "three"} {
		message := mustPostMessage(t, store, post.Ref, body)
		if _, err := store.db.Exec(`UPDATE post_messages
			SET created_utc = ?1, modified_utc = ?1
			WHERE workspace_id = ?2 AND post_id = ?3 AND id = ?4`,
			formatTime(hour(n)), key.Workspace, key.ID, message.Number,
		); err != nil {
			t.Fatal(err)
		}
	}

	for _, test := range []struct {
		after time.Time
		limit int
		want  string
	}{
		{hour(1), 10, "e1-m2:two e1-m3:three cursor=3 remaining=0"},
		{hour(1), 1, "e1-m2:two cursor=2 remaining=1"},
		{hour(3), 10, " cursor=3 remaining=0"},
	} {
		page, err := store.Messages(context.Background(), key,
			api.ChatBound{Time: test.after}, Filter{}, nil, test.limit)
		if err != nil {
			t.Fatal(err)
		}
		if got := chatText(page); got != test.want {
			t.Fatalf("after %s limit %d = %q, want %q", test.after,
				test.limit, got, test.want)
		}
	}
}

func TestEditsKeepTheirPlaceAndAreNotReadAgain(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	post := mustAdd(t, store, "thread", "", nil)
	question := mustPostMessage(t, store, post.Ref, "question")
	mustPostMessage(t, store, post.Ref, "answer")
	cursor := readChat(t, store, post.Ref, 0, 20).Cursor

	edited, changed, err := store.EditMessage(ctx, store.mustKey(post.Ref),
		question.Number, ownEdit("question, edited"))
	if err != nil || !changed {
		t.Fatalf("edit = %v, %v", changed, err)
	}
	if !edited.Created.Equal(question.Created) ||
		!edited.Modified.After(question.Modified) {
		t.Fatalf("edited times = %v, %v", edited.Created, edited.Modified)
	}
	if got := chatText(readChat(t, store, post.Ref, cursor, 20)); got !=
		" cursor=2 remaining=0" {
		t.Fatalf("read after an edit = %q, want nothing new", got)
	}
	if got := chatText(readChat(t, store, post.Ref, 0, 20)); got !=
		"e1-m1:question, edited e1-m2:answer cursor=2 remaining=0" {
		t.Fatalf("full read = %q, want the edit in its place", got)
	}

	if _, changed, err := store.EditMessage(ctx, store.mustKey(post.Ref),
		question.Number, ownEdit("question, edited")); err != nil || changed {
		t.Fatalf("repeating an edit = %v, %v; want no change", changed, err)
	}
	if _, _, err := store.EditMessage(ctx, store.mustKey(post.Ref), 9,
		ownEdit("text")); !errors.Is(err, ErrMessageNotFound) {
		t.Fatalf("editing a missing message = %v", err)
	}
}

// Each write sets the activity time itself, so after every kind of
// write it must still be the latest change to the post or its chat.
func TestEveryWriteKeepsActivityCurrent(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	checkActivity := func(after string) {
		t.Helper()
		var stale int
		if err := store.db.QueryRowContext(ctx, `
			SELECT count(*) FROM posts AS e
			WHERE e.activity_utc <> `+activityExpr,
		).Scan(&stale); err != nil {
			t.Fatal(err)
		}
		if stale != 0 {
			t.Fatalf("after %s, %d posts have stale activity", after,
				stale)
		}
	}
	editMessage := func(post api.Ref, body string) {
		t.Helper()
		if _, _, err := store.EditMessage(ctx, store.mustKey(post), 1,
			ownEdit(body)); err != nil {
			t.Fatal(err)
		}
	}
	pond := mustAdd(t, store, "pond", "", nil)
	nest := mustAdd(t, store, "nest", "", nil)
	checkActivity("adding")

	mustPostMessage(t, store, pond.Ref, "first")
	mustPostMessage(t, store, nest.Ref, "second")
	checkActivity("posting")
	editMessage(pond.Ref, "first, edited")
	checkActivity("editing a message")
	editMessage(pond.Ref, "first, edited")
	checkActivity("repeating a message edit")

	project := "lake"
	if _, err := store.Edit(ctx, everyAccess(store), store.mustKey(nest.Ref),
		api.EditPostRequest{Project: &project, User: testUser}); err != nil {
		t.Fatal(err)
	}
	checkActivity("editing a post")
	if err := addLinks(t, store, pond.Ref,
		link("see", nest.Ref)); err != nil {
		t.Fatal(err)
	}
	checkActivity("linking")
}

func TestOverviewGroupsTheNewestMessagesByPost(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	a := mustAdd(t, store, "a", "", nil)
	b := mustAdd(t, store, "b", "", nil)
	c := mustAdd(t, store, "c", "", nil)
	private := addTo(t, store, "personal",
		api.AddPostRequest{Subject: "private"})
	first := mustPostMessage(t, store, a.Ref, "a1")
	mustPostMessage(t, store, b.Ref, "b1")
	mustPostMessage(t, store, a.Ref, "a2")
	mustPostMessage(t, store, c.Ref, "c1")
	mustPostMessage(t, store, private.Ref, "private")
	mustPostMessage(t, store, b.Ref, "b2")
	if _, _, err := store.EditMessage(ctx, store.mustKey(a.Ref),
		first.Number, ownEdit("a1 edited")); err != nil {
		t.Fatal(err)
	}

	overview, err := store.Overview(ctx, workspaceID(t, store, "general"), 4)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, chat := range overview.Chats {
		line := chat.Post.Ref.String() + " " + chat.Post.Subject + ":"
		for _, message := range chat.Messages {
			line += " " + message.Body
		}
		got = append(got, line)
	}
	want := []string{"e1 a: a2", "e3 c: c1", "e2 b: b1 b2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("overview = %q, want %q", got, want)
	}
}

func TestOverwrittenTextLeavesNoCopyInTheDatabase(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store := openStoreAt(t, filepath.Join(dir, "ws.db"), testBusyTimeout)
	post := mustAdd(t, store, "SUBJECTMARK "+
		strings.Repeat("x", 100), "", nil)
	message := mustPostMessage(t, store, post.Ref, "EDITEDAWAYMARK "+
		strings.Repeat("x", 200))
	subject := "short"
	if _, err := store.Edit(ctx, everyAccess(store),
		store.mustKey(post.Ref),
		api.EditPostRequest{Subject: &subject, User: testUser}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.EditMessage(ctx, store.mustKey(post.Ref),
		message.Number, ownEdit("short")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"ws.db", "ws.db-wal"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		for _, mark := range []string{"SUBJECTMARK", "EDITEDAWAYMARK"} {
			if bytes.Contains(data, []byte(mark)) {
				t.Fatalf("%s still holds overwritten text %s", name, mark)
			}
		}
	}
}
