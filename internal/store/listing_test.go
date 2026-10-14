package store

import (
	"cmp"
	"context"
	"fmt"
	"math/rand"
	"path"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/airspacesh/workstream/internal/api"
)

// activityExpr is when post e last changed by definition: an edit to
// it, or a message posted or edited in its chat. activity_utc must
// always equal it.
const activityExpr = `max(e.modified_utc, coalesce((
	SELECT max(m.modified_utc) FROM post_messages AS m
	WHERE m.workspace_id = e.workspace_id AND m.post_id = e.id), ''))`

// forestWriter makes random writes to two workspaces: posts with random
// parents in the same workspace, supersessions across them, origins,
// metadata, messages, and edits and deletes of both.
type forestWriter struct {
	t           *testing.T
	store       *Store
	random      *rand.Rand
	posts       []api.Post
	messages    []api.Message
	inWorkspace map[string][]api.Ref
	deleted     map[string]bool
	writes      int
}

func newForestWriter(
	t *testing.T, store *Store, random *rand.Rand,
) *forestWriter {
	return &forestWriter{t: t, store: store, random: random,
		inWorkspace: map[string][]api.Ref{}, deleted: map[string]bool{}}
}

// add adds a post, in a random place.
func (w *forestWriter) add() {
	workspace := "general"
	if w.random.Intn(3) == 0 {
		workspace = "personal"
	}
	var links []api.Link
	if parents := w.inWorkspace[workspace]; len(parents) > 0 &&
		w.random.Intn(10) < 7 {
		links = append(links, link(api.ParentLink,
			parents[w.random.Intn(len(parents))]))
	}
	if len(w.posts) > 0 && w.random.Intn(7) == 0 {
		links = append(links, link(api.SupersedesLink,
			w.posts[w.random.Intn(len(w.posts))].Ref))
	}
	// Project c is rare, so a listing of it reads its posts by key.
	project := []string{"a", "b"}[w.random.Intn(2)]
	if w.writes%13 == 0 {
		project = "c"
	}
	// Some posts hold jira, and a few jira-2 as well. The values are
	// upper case, so the metadata filters match them regardless of
	// case.
	metadata := map[string]string{}
	if w.writes%3 == 0 {
		metadata["jira"] = fmt.Sprintf("PROJ-%d", w.writes%4)
	}
	if w.writes%9 == 0 {
		metadata["jira-2"] = "PROJ-9"
	}
	post := addTo(w.t, w.store, workspace, api.AddPostRequest{
		Project:  project,
		Subject:  fmt.Sprintf("post %d %s", w.writes, w.word()),
		Metadata: metadata,
		Links:    links,
		Origin:   api.Origin{User: []string{"ines", "omar"}[w.random.Intn(2)]},
	})
	w.posts = append(w.posts, post)
	w.inWorkspace[workspace] = append(w.inWorkspace[workspace], post.Ref)
}

func (w *forestWriter) word() string {
	return []string{"heron", "duck"}[w.random.Intn(2)]
}

// write makes one random write.
func (w *forestWriter) write() {
	w.t.Helper()
	ctx := context.Background()
	w.writes++
	if len(w.posts) < 20 || w.random.Intn(3) == 0 {
		w.add()
		return
	}
	post := w.posts[w.random.Intn(len(w.posts))]
	key := w.store.mustKey(post.Ref)
	switch choice := w.random.Intn(16); {
	case choice < 3 && !w.deleted[post.Ref.String()]:
		subject := fmt.Sprintf("edited %d %s", w.writes, w.word())
		edit := api.EditPostRequest{Subject: &subject,
			User: post.Origin.User}
		switch choice {
		case 0:
			edit.Metadata = map[string]string{"jira": ""}
		case 1:
			edit.Metadata = map[string]string{"jira": "PROJ-1"}
		}
		if _, err := w.store.Edit(ctx, everyAccess(w.store), key,
			edit); err != nil {
			w.t.Fatal(err)
		}
	case choice < 4:
		if _, err := w.store.Delete(ctx, everyAccess(w.store), key,
			api.DeleteRequest{User: post.Origin.User}); err != nil {
			w.t.Fatal(err)
		}
		w.deleted[post.Ref.String()] = true
	case choice < 8 && len(w.messages) > 0:
		message := w.messages[w.random.Intn(len(w.messages))]
		messageKey := w.store.mustKey(message.Post)
		if w.deleted[message.Ref().String()] {
			return
		}
		if choice == 7 {
			if _, err := w.store.DeleteMessage(ctx, everyAccess(w.store),
				messageKey,
				message.Number, api.DeleteRequest{
					User: message.Origin.User}); err != nil {
				w.t.Fatal(err)
			}
			w.deleted[message.Ref().String()] = true
			return
		}
		if _, _, err := w.store.EditMessage(ctx, everyAccess(w.store),
			messageKey,
			message.Number, api.EditMessageRequest{
				Body: fmt.Sprintf("edited %d %s", w.writes, w.word()),
				User: message.Origin.User,
			}); err != nil {
			w.t.Fatal(err)
		}
	default:
		user := []string{"ines", "omar"}[w.random.Intn(2)]
		message, _, err := w.store.PostMessage(ctx, everyAccess(w.store),
			key, api.PostMessageRequest{
				Body: w.word(), Origin: api.Origin{User: user},
			})
		if err != nil {
			w.t.Fatal(err)
		}
		w.messages = append(w.messages, message)
	}
}

// spreadTimes spreads the times of the changes over a year at random,
// in the order of their revisions, as the clock gives them. When onDays
// is set, every time falls on midnight, so many share it.
func spreadTimes(
	t *testing.T, store *Store, random *rand.Rand, onDays bool,
) {
	t.Helper()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	year := int64(365 * 24 * time.Hour)
	times := make([]time.Time, latestRevision(t, store)+1)
	for i := range times {
		times[i] = start.Add(time.Duration(random.Int63n(year)))
		if onDays {
			times[i] = times[i].Truncate(24 * time.Hour)
		}
	}
	slices.SortFunc(times, time.Time.Compare)
	retime(t, store, func(revision int64) time.Time {
		return times[revision]
	})
}

// forestPost and forestMessage hold what the reference reads of the
// posts and messages of a workspace.
type forestPost struct {
	id                  int64
	revision            int64
	activityRevision    int64
	modified, activity  string
	project, text, user string
	superseded, deleted bool
	parent              int64
	// metadata holds the post's values lowered, by key.
	metadata map[string]string
}

type forestMessage struct {
	postOrder, post, number, revision int64
	created, text, user               string
	deleted                           bool
}

// referenceMatch is a match as the reference ranks it.
type referenceMatch struct {
	revision  int64
	post      int64
	postOrder int64
}

// readForest reads every post and message of a workspace.
func readForest(
	t *testing.T, store *Store, workspace int64,
) (map[int64]forestPost, []forestMessage) {
	t.Helper()
	rows, err := store.db.Query(`
		SELECT e.id, e.revision, e.activity_revision, e.modified_utc,
			e.activity_utc, e.project_lower,
			e.subject_lower || ' ' || e.body_lower, e.origin_user_lower,
			`+supersededClause+`, e.deleted, coalesce((
				SELECT target_id FROM post_links
				WHERE source_workspace_id = e.workspace_id
					AND source_id = e.id AND name = 'parent'), 0)
		FROM posts AS e WHERE e.workspace_id = ?`, "", workspace)
	if err != nil {
		t.Fatal(err)
	}
	posts := map[int64]forestPost{}
	for rows.Next() {
		var e forestPost
		if err := rows.Scan(&e.id, &e.revision, &e.activityRevision,
			&e.modified, &e.activity, &e.project, &e.text, &e.user,
			&e.superseded, &e.deleted, &e.parent); err != nil {
			t.Fatal(err)
		}
		e.metadata = map[string]string{}
		posts[e.id] = e
	}
	rows.Close()

	rows, err = store.db.Query(`
		SELECT post_id, key, value_lower FROM post_metadata
		WHERE workspace_id = ?`, workspace)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id int64
		var key, value string
		if err := rows.Scan(&id, &key, &value); err != nil {
			t.Fatal(err)
		}
		posts[id].metadata[key] = value
	}
	rows.Close()

	rows, err = store.db.Query(`
		SELECT post_order, post_id, id, posted_revision, created_utc,
			lower(body), lower(origin_user), deleted
		FROM post_messages WHERE workspace_id = ?`, workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var messages []forestMessage
	for rows.Next() {
		var m forestMessage
		if err := rows.Scan(&m.postOrder, &m.post, &m.number, &m.revision,
			&m.created, &m.text, &m.user, &m.deleted); err != nil {
			t.Fatal(err)
		}
		messages = append(messages, m)
	}
	return posts, messages
}

// listingCase is a listing the reference knows how to filter:
// whole-value project and origin user patterns and a literal content
// pattern, without wildcards, and metadata patterns, whose only
// wildcard is *. key holds a key, noKey does not, and metaKey and
// metaValue hold a pair.
type listingCase struct {
	project, user, content string
	key, noKey             string
	metaKey, metaValue     string
	span                   timeSpan
	revisions              RevisionSpan
	under                  int64
	notUnder               []int64
	includeSuperseded      bool
	includeDeleted         bool
	items                  api.Items
}

func (c listingCase) filter(
	t *testing.T, store *Store, workspace int64,
) Filter {
	t.Helper()
	revisions := c.span.revisions(t, store, workspace, c.revisions)
	filter := Filter{Revisions: revisions,
		IncludeSuperseded: c.includeSuperseded,
		IncludeDeleted:    c.includeDeleted, Items: c.items}
	if c.project != "" {
		filter.Project = []FieldCond{{Value: c.project}}
	}
	if c.user != "" {
		filter.OriginUser = []FieldCond{{Value: c.user}}
	}
	if c.content != "" {
		filter.Content = []FieldCond{{Value: c.content}}
	}
	if c.key != "" {
		filter.Key = append(filter.Key, FieldCond{Value: c.key})
	}
	if c.noKey != "" {
		filter.Key = append(filter.Key,
			FieldCond{Negate: true, Value: c.noKey})
	}
	if c.metaKey != "" {
		filter.Meta = []MetaCond{{Key: c.metaKey, Value: c.metaValue}}
	}
	if c.under != 0 {
		filter.Under = &Key{workspace, c.under}
	}
	for _, id := range c.notUnder {
		filter.NotUnder = append(filter.NotUnder, Key{workspace, id})
	}
	return filter
}

// referenceListing is the listing worked out from every post and
// message: those that match, by the revision each ranks by, newest
// first. A deleted post leaves out only itself, so the posts under it
// and the messages in its chat still match.
func referenceListing(
	posts map[int64]forestPost, messages []forestMessage, c listingCase,
) []referenceMatch {
	inSpans := func(revision int64, at string) bool {
		return (c.revisions.After == 0 || revision > c.revisions.After) &&
			(c.revisions.Before == 0 || revision < c.revisions.Before) &&
			(c.span.After.IsZero() || at >= formatTime(c.span.After)) &&
			(c.span.Before.IsZero() || at < formatTime(c.span.Before))
	}
	under := func(id int64) bool {
		for ; c.under != 0 && id != 0; id = posts[id].parent {
			if id == c.under {
				return true
			}
		}
		return c.under == 0
	}
	notUnder := func(id int64) bool {
		for ; id != 0; id = posts[id].parent {
			if slices.Contains(c.notUnder, id) {
				return false
			}
		}
		return true
	}
	holds := func(e forestPost, keyPattern, valuePattern string) bool {
		for key, value := range e.metadata {
			keyMatched, _ := path.Match(keyPattern, key)
			valueMatched, _ := path.Match(valuePattern, value)
			if keyMatched && valueMatched {
				return true
			}
		}
		return false
	}
	listed := func(e forestPost) bool {
		return (c.project == "" || e.project == c.project) &&
			(c.key == "" || holds(e, c.key, "*")) &&
			(c.noKey == "" || !holds(e, c.noKey, "*")) &&
			(c.metaKey == "" || holds(e, c.metaKey, c.metaValue)) &&
			(c.includeSuperseded || !e.superseded) && under(e.id) &&
			notUnder(e.id)
	}
	matched := func(text, user string) bool {
		return (c.user == "" || user == c.user) &&
			(c.content == "" || strings.Contains(text, c.content))
	}

	var matches []referenceMatch
	for _, e := range posts {
		revision, at := e.activityRevision, e.activity
		if c.items.HasChat() {
			revision, at = e.revision, e.modified
		}
		if c.items.HasPosts() && listed(e) &&
			(c.includeDeleted || !e.deleted) &&
			matched(e.text, e.user) && inSpans(revision, at) {
			matches = append(matches, referenceMatch{revision, e.id, 0})
		}
	}
	for _, m := range messages {
		if c.items.HasChat() && listed(posts[m.post]) &&
			(c.includeDeleted || !m.deleted) &&
			matched(m.text, m.user) && inSpans(m.revision, m.created) {
			matches = append(matches,
				referenceMatch{m.revision, m.post, m.postOrder})
		}
	}
	slices.SortFunc(matches, func(a, b referenceMatch) int {
		return cmp.Compare(b.revision, a.revision)
	})
	return matches
}

// referenceNewMessages counts, for each post a page without chat
// matched, the messages in its chat posted since the page's oldest
// match that the listing would show.
func referenceNewMessages(
	messages []forestMessage, page []referenceMatch, c listingCase,
) map[int64]int {
	counts := map[int64]int{}
	if c.items.HasChat() || len(page) == 0 {
		return counts
	}
	onPage := map[int64]bool{}
	for _, m := range page {
		onPage[m.post] = true
	}
	for _, m := range messages {
		if onPage[m.post] && m.revision >= page[len(page)-1].revision &&
			(c.includeDeleted || !m.deleted) {
			counts[m.post]++
		}
	}
	return counts
}

// checkListingPage checks a page of a listing against the reference:
// its matches, the path above each, the new messages counted, and
// whether more follow. It returns the page's matches, and whether more
// follow.
func checkListingPage(
	t *testing.T,
	store *Store,
	workspace int64,
	posts map[int64]forestPost,
	messages []forestMessage,
	c listingCase,
	limit int,
) ([]referenceMatch, bool) {
	t.Helper()
	filter := c.filter(t, store, workspace)
	filter.Limit = limit
	page, err := store.Search(context.Background(), everyAccess(store),
		workspace, filter)
	if err != nil {
		t.Fatal(err)
	}
	want := referenceListing(posts, messages, c)
	wantPage := want[:min(limit, len(want))]
	label := fmt.Sprintf("%+v, limit %d", c, limit)

	var got []referenceMatch
	gotNew := map[int64]int{}
	for _, m := range page.Matches {
		key := store.mustKey(m.Post.Ref)
		match := referenceMatch{m.Revision, key.ID, 0}
		if m.Message != nil {
			for _, message := range messages {
				if message.post == key.ID &&
					message.number == m.Message.Number {
					match.postOrder = message.postOrder
				}
			}
		}
		got = append(got, match)
		if m.NewMessages != 0 {
			gotNew[key.ID] = m.NewMessages
		}

		var path []int64
		for id := posts[key.ID].parent; id != 0; id = posts[id].parent {
			path = append([]int64{id}, path...)
		}
		var gotPath []int64
		for _, ancestor := range m.Ancestors {
			gotPath = append(gotPath, store.mustKey(ancestor.Ref).ID)
		}
		if !slices.Equal(gotPath, path) {
			t.Fatalf("%s: the path to e%d = %v, want %v", label, key.ID,
				gotPath, path)
		}
	}
	if !slices.Equal(got, wantPage) {
		t.Fatalf("%s: matches\n%v\nwant\n%v", label, got, wantPage)
	}
	if page.More != (len(want) > limit) {
		t.Fatalf("%s: more %v", label, page.More)
	}
	wantNew := referenceNewMessages(messages, wantPage, c)
	if !reflect.DeepEqual(gotNew, wantNew) {
		t.Fatalf("%s: new messages %v, want %v", label, gotNew, wantNew)
	}
	return got, page.More
}

// checkPagesBack checks every page of a listing, each listing the
// matches before the revision of the oldest on the page before it, as
// its footer suggests, and that together they list every match.
func checkPagesBack(
	t *testing.T,
	store *Store,
	workspace int64,
	posts map[int64]forestPost,
	messages []forestMessage,
	c listingCase,
	limit int,
) {
	t.Helper()
	want := referenceListing(posts, messages, c)
	listed := 0
	for {
		page, more := checkListingPage(t, store, workspace, posts,
			messages, c, limit)
		listed += len(page)
		if !more {
			break
		}
		c.revisions.Before = page[len(page)-1].revision
	}
	if listed != len(want) {
		t.Fatalf("%+v: the pages listed %d matches, want %d", c, listed,
			len(want))
	}
}

func TestListingMatchesTheReference(t *testing.T) {
	day := func(month time.Month, day int) time.Time {
		return time.Date(2026, month, day, 0, 0, 0, 0, time.UTC)
	}
	spans := []timeSpan{
		{},
		{After: day(7, 1)},
		{Before: day(4, 1)},
		{After: day(3, 1), Before: day(9, 1)},
	}
	for seed := int64(1); seed <= 4; seed++ {
		store := openTestStore(t)
		random := rand.New(rand.NewSource(seed))
		writer := newForestWriter(t, store, random)
		for range 200 {
			writer.write()
		}
		spreadTimes(t, store, random, seed%2 == 0)
		workspace := workspaceID(t, store, "general")
		posts, messages := readForest(t, store, workspace)
		// A listing under a top-level post, and under a deleted post
		// with posts under it, which it still lists.
		type cut struct {
			under    int64
			notUnder []int64
		}
		cuts := []cut{{}}
		for id, e := range posts {
			if e.parent == 0 {
				cuts = append(cuts, cut{under: id})
				break
			}
		}
		var deleted int64
		for _, e := range posts {
			if e.parent != 0 && posts[e.parent].deleted {
				deleted = e.parent
				cuts = append(cuts, cut{under: deleted})
				break
			}
		}
		// Listings that leave out a post with a grandchild, inside its
		// top-level post and alongside the deleted post.
		for _, e := range posts {
			parent := posts[e.parent]
			if e.parent == 0 || parent.parent == 0 {
				continue
			}
			top := parent.parent
			for posts[top].parent != 0 {
				top = posts[top].parent
			}
			cuts = append(cuts,
				cut{under: top, notUnder: []int64{e.parent}},
				cut{notUnder: []int64{e.parent, deleted}})
			break
		}
		if len(cuts) != 5 {
			t.Fatalf("seed %d made too few subtrees to list", seed)
		}
		latest := latestRevision(t, store)
		middle := RevisionSpan{After: latest / 4, Before: latest * 3 / 4}

		for i, base := range []listingCase{
			{}, {project: "a"}, {user: "ines"}, {content: "heron"},
			{project: "b", user: "omar"}, {project: "c"},
			{project: "c", content: "duck"}, {project: "nothing"},
			{key: "jira"}, {key: "nothing"}, {key: "jira*"},
			{noKey: "jira"}, {metaKey: "jira", metaValue: "proj-1"},
			{metaKey: "jira*", metaValue: "proj-9"},
			{project: "c", metaKey: "jira", metaValue: "proj-*"},
		} {
			for _, k := range cuts {
				// One span covers the cuts that leave a subtree out, to
				// keep the test short.
				cutSpans := spans
				if len(k.notUnder) != 0 {
					cutSpans = spans[:1]
				}
				for _, span := range cutSpans {
					for flags := range 6 {
						c := base
						c.under, c.notUnder, c.span = k.under,
							k.notUnder, span
						c.includeSuperseded = flags%2 != 0
						c.items = []api.Items{api.PostsOnly,
							api.ChatOnly, api.PostsAndChat}[flags/2]
						c.includeDeleted = i%2 != 0
						checkListingPage(t, store, workspace, posts,
							messages, c, 500)
						checkListingPage(t, store, workspace, posts,
							messages, c, 1)
						checkPagesBack(t, store, workspace, posts,
							messages, c, 3)
						c.revisions = middle
						checkListingPage(t, store, workspace, posts,
							messages, c, 500)
					}
				}
			}
		}
	}
}

// Paging back as a listing's footer says, from a time to start and then
// by the revision of each page's oldest match, lists each match once
// while writes land between the pages. A write lifts what it changes
// above the pages still to come, so the matches the walk must list are
// those that kept their rank throughout.
func TestPagingBackListsEachMatchOnceWhileWritesLand(t *testing.T) {
	for seed := int64(1); seed <= 4; seed++ {
		for _, items := range []api.Items{api.PostsOnly, api.ChatOnly,
			api.PostsAndChat} {
			store := openTestStore(t)
			writer := newForestWriter(t, store,
				rand.New(rand.NewSource(seed)))
			for range 120 {
				writer.write()
			}
			before := time.Now()
			for range 40 {
				writer.write()
			}
			workspace := workspaceID(t, store, "general")
			first := listingCase{items: items,
				span: timeSpan{Before: before}}
			if seed%2 == 0 {
				first.project = "a"
			}

			posts, messages := readForest(t, store, workspace)
			atStart := referenceListing(posts, messages, first)
			listed := map[referenceMatch]bool{}
			c := first
			for {
				page, more := checkListingPage(t, store, workspace,
					posts, messages, c, 3)
				for _, m := range page {
					m.revision = 0
					if listed[m] {
						t.Fatalf("%+v: e%d message %d listed twice", c,
							m.post, m.postOrder)
					}
					listed[m] = true
				}
				if !more {
					break
				}
				c.span.Before = time.Time{}
				c.revisions.Before = page[len(page)-1].revision
				for range 3 {
					writer.write()
				}
				posts, messages = readForest(t, store, workspace)
			}

			atEnd := referenceListing(posts, messages, first)
			for _, m := range atStart {
				kept := slices.Contains(atEnd, m)
				m.revision = 0
				if kept && !listed[m] {
					t.Fatalf("seed %d, %v: e%d message %d was skipped",
						seed, items, m.post, m.postOrder)
				}
			}
		}
	}
}

// Without chat, a post ranks by the latest change to it or its chat
// alone, so a message deep in a tree lifts only the post it is in, and
// a filter finds a post at any level.
func TestListingRanksEachPostByItsOwnActivity(t *testing.T) {
	store := openTestStore(t)
	root := addTo(t, store, "general", api.AddPostRequest{
		Subject: "root", Origin: api.Origin{User: "omar"}})
	child := addTo(t, store, "general", api.AddPostRequest{
		Subject: "child", Origin: api.Origin{User: "ines"},
		Links: []api.Link{link(api.ParentLink, root.Ref)}})
	grandchild := addTo(t, store, "general", api.AddPostRequest{
		Subject: "grandchild", Origin: api.Origin{User: "omar"},
		Links: []api.Link{link(api.ParentLink, child.Ref)}})
	other := mustAdd(t, store, "other", "", nil)
	mustPostMessage(t, store, grandchild.Ref, "quack")

	page := runSearch(t, store, Filter{Limit: 2})
	assertPostRefs(t, page, grandchild.Ref, other.Ref)
	if got := page.Matches[0].NewMessages; got != 1 {
		t.Fatalf("new messages = %d, want 1", got)
	}
	if page.Matches[0].Ancestors[1].Ref != child.Ref {
		t.Fatalf("path = %+v, want the root and the child",
			page.Matches[0].Ancestors)
	}

	// With chat, the message is a match of its own, and the post ranks
	// by its own change.
	withChat := runSearch(t, store, Filter{Limit: 2, Items: api.PostsAndChat})
	assertMatches(t, withChat, messageRef(grandchild.Ref, 1),
		other.Ref.String())

	byInes := runSearch(t, store, Filter{
		OriginUser: []FieldCond{{Value: "ines"}}, Items: api.PostsAndChat})
	assertPostRefs(t, byInes, child.Ref)
}

func TestListingLeavesOutSupersededPosts(t *testing.T) {
	store := openTestStore(t)
	old := mustAdd(t, store, "old", "", nil)
	child := addChild(t, store, "child", old.Ref)
	successor := mustAdd(t, store, "successor", "", nil)
	if err := addLinks(t, store, successor.Ref,
		link(api.SupersedesLink, old.Ref)); err != nil {
		t.Fatal(err)
	}

	page := runSearch(t, store, Filter{})
	assertPostRefs(t, page, successor.Ref, child.Ref)
	if path := page.Matches[1].Ancestors; len(path) != 1 ||
		!reflect.DeepEqual(path[0].SupersededBy, []api.Ref{successor.Ref}) {
		t.Fatalf("the child's path = %+v, want its superseded parent", path)
	}
	all := runSearch(t, store, Filter{IncludeSuperseded: true})
	if got := len(all.Matches); got != 3 {
		t.Fatalf("listing with superseded lists %d, want 3", got)
	}
}
