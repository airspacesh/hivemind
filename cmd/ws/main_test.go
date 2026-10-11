package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/airspacesh/workstream/internal/api"
	"github.com/airspacesh/workstream/internal/version"
)

// The binaries under test, built once by TestMain: the default build,
// and a build under another name, with its three settings.
var wsBinary, forumBinary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "ws-test-")
	if err != nil {
		panic(err)
	}
	wsBinary = filepath.Join(dir, "ws")
	forumBinary = filepath.Join(dir, "forum")
	builds := [][]string{
		{"build", "-o", wsBinary, "."},
		{"build", "-o", forumBinary, "-ldflags",
			"-X main.programName=forum -X main.envPrefix=FORUM " +
				"-X main.defaultConfigPath=~/.config/forum/config.yml", "."},
	}
	for _, args := range builds {
		build := exec.Command("go", args...)
		build.Env = append(os.Environ(), "CGO_ENABLED=0")
		if output, err := build.CombinedOutput(); err != nil {
			panic(fmt.Sprintf("go %v: %v\n%s", args, err, output))
		}
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

type result struct {
	stdout, stderr string
	ok             bool
}

// runBinary runs a built client with only the environment given, so
// no real config, home or settings reach it.
func runBinary(t *testing.T, binary string, env []string, args ...string) result {
	t.Helper()
	command := exec.Command(binary, args...)
	command.Env = append([]string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		"TZ=UTC",
	}, env...)
	var stdout, stderr strings.Builder
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	if _, exited := err.(*exec.ExitError); err != nil && !exited {
		t.Fatal(err)
	}
	return result{stdout.String(), stderr.String(), err == nil}
}

func writeConfig(t *testing.T, text string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(path, []byte(text), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

// ws runs the default build against the config at path.
func ws(t *testing.T, config string, args ...string) result {
	t.Helper()
	return runBinary(t, wsBinary, []string{"WORKSTREAM_CONFIG=" + config},
		args...)
}

type request struct {
	method, path, auth string
}

// fakeServer answers with a handler and records every request.
type fakeServer struct {
	*httptest.Server
	mu       sync.Mutex
	requests []request
}

func newFakeServer(t *testing.T, handler http.HandlerFunc) *fakeServer {
	server := &fakeServer{}
	server.Server = httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			server.mu.Lock()
			server.requests = append(server.requests, request{r.Method,
				r.URL.Path, r.Header.Get("Authorization")})
			server.mu.Unlock()
			handler(w, r)
		}))
	t.Cleanup(server.Close)
	return server
}

func (s *fakeServer) recorded() []request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]request(nil), s.requests...)
}

func reply(w http.ResponseWriter, status int, value any) {
	w.Header().Set(version.APIHeader, strconv.Itoa(version.API))
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func refuse(w http.ResponseWriter, status int, code api.ErrorCode, text string) {
	reply(w, status, api.ErrorResponse{Error: text, Code: code})
}

// hosting answers the workspace listing with the given name=prefix
// pairs, and every other request with next.
func hosting(next http.HandlerFunc, workspaces ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/workspaces" {
			next(w, r)
			return
		}
		listed := api.WorkspacesResponse{Workspaces: []api.Workspace{}}
		for _, pair := range workspaces {
			name, prefix, _ := strings.Cut(pair, "=")
			listed.Workspaces = append(listed.Workspaces,
				api.Workspace{Name: name, Prefix: prefix})
		}
		reply(w, http.StatusOK, listed)
	}
}

func postDetail(ref string) api.PostDetail {
	parsed, _ := api.ParseRef(ref)
	when := time.Date(2026, 10, 9, 5, 20, 0, 0, time.UTC)
	return api.PostDetail{ListedPost: api.ListedPost{Post: api.Post{
		Ref: parsed, Created: when, Modified: when,
		Subject: "Count the ducklings",
	}, Activity: when}}
}

func answering(ref string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/posts/"+ref {
			reply(w, http.StatusOK, postDetail(ref))
			return
		}
		refuse(w, http.StatusNotFound, api.CodeRouteNotFound, "no route")
	}
}

func unexpected(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		refuse(w, http.StatusNotFound, api.CodeRouteNotFound, "no route")
	}
}

// closedServer is a server URL nothing listens on.
func closedServer(t *testing.T) string {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	return "http://" + address
}

func TestConfigRefusesWhatItDoesNotKnow(t *testing.T) {
	tests := map[string]struct{ config, want string }{
		"unknown key": {"workspaces:\n  general:\n    server: http://h:1\n" +
			"    sever: http://h:2\n", "field sever not found"},
		"unknown capture": {"workspaces:\n  general:\n    server: " +
			"http://h:1\n    metadata_capture: [hostname, user]\n",
			`metadata_capture lists "user"`},
		"capture scalar": {"workspaces:\n  general:\n    server: " +
			"http://h:1\n    metadata_capture: hostname\n",
			"must be all, none, or a list"},
		"no scheme": {"workspaces:\n  general:\n    server: h:1\n",
			"must start with http://"},
		"shared prefix": {"workspaces:\n  a:\n    server: http://h:1\n" +
			"    prefix: e\n  b:\n    server: http://h:2\n    prefix: e\n",
			"workspaces a and b both set prefix e"},
		"no workspaces": {"workspaces: {}\n", "at least one workspace"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			got := ws(t, writeConfig(t, test.config, 0o600), "list-workspaces")
			if got.ok || !strings.Contains(got.stderr, test.want) {
				t.Fatalf("got %+v, want an error with %q", got, test.want)
			}
		})
	}
}

func TestConfigHoldingSecretMustBeOwnerOnly(t *testing.T) {
	server := newFakeServer(t, hosting(unexpected(t), "general=e"))
	open := "workspaces:\n  general:\n    server: " + server.URL + "\n"
	secret := open + "    auth:\n      secret: quack\n"

	if got := ws(t, writeConfig(t, open, 0o644), "list-workspaces"); !got.ok {
		t.Fatalf("a readable config without a secret: %+v", got)
	}
	got := ws(t, writeConfig(t, secret, 0o640), "list-workspaces")
	if got.ok || !strings.Contains(got.stderr, "holds a secret") ||
		!strings.Contains(got.stderr, "chmod 600") {
		t.Fatalf("a group-readable config with a secret: %+v", got)
	}
	if len(server.recorded()) != 1 {
		t.Fatalf("a refused config reached the server: %v", server.recorded())
	}
}

func TestListWorkspacesKeepsConfigOrder(t *testing.T) {
	server := newFakeServer(t, hosting(unexpected(t), "a=e", "m=p", "z=r"))
	config := writeConfig(t, fmt.Sprintf("workspaces:\n  z:\n    server: %s"+
		"\n  a:\n    server: %[1]s\n  m:\n    server: %[1]s\n", server.URL),
		0o600)
	got := ws(t, config, "list-workspaces")
	names := []string{}
	for _, line := range strings.Split(strings.TrimSpace(got.stdout), "\n") {
		names = append(names, strings.Fields(line)[0])
	}
	if strings.Join(names, " ") != "z: a: m:" {
		t.Fatalf("workspaces listed as %v, want config order\n%s", names,
			got.stdout)
	}
}

func TestWorkspaceChoiceOrder(t *testing.T) {
	var asked []string
	var mu sync.Mutex
	server := newFakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked = append(asked, r.URL.Path)
		mu.Unlock()
		reply(w, http.StatusOK, api.ProjectsResponse{})
	})
	config := writeConfig(t, fmt.Sprintf("workspaces:\n  general:\n    "+
		"server: %s\n  other:\n    server: %[1]s\n  third:\n    "+
		"server: %[1]s\n", server.URL), 0o600)
	env := func(pairs ...string) []string {
		return append([]string{"WORKSTREAM_CONFIG=" + config}, pairs...)
	}

	runs := []struct {
		env  []string
		args []string
		want string
	}{
		{env(), nil, "general"},
		{env("WORKSTREAM_DEFAULT_WORKSPACE=other"), nil, "other"},
		{env("WORKSTREAM_DEFAULT_WORKSPACE=other"),
			[]string{"--workspace", "third"}, "third"},
	}
	for _, run := range runs {
		mu.Lock()
		asked = nil
		mu.Unlock()
		args := append([]string{"list-projects"}, run.args...)
		got := runBinary(t, wsBinary, run.env, args...)
		want := "/api/workspaces/" + run.want + "/projects"
		if !got.ok || len(asked) != 1 || asked[0] != want {
			t.Errorf("%v %v asked %v, want %s: %+v", run.env[1:], run.args,
				asked, want, got)
		}
	}

	got := runBinary(t, wsBinary, env("WORKSTREAM_DEFAULT_WORKSPACE=gone"),
		"list-projects")
	if got.ok || !strings.Contains(got.stderr, `"gone"`) {
		t.Errorf("an unconfigured default workspace: %+v", got)
	}
	got = runBinary(t, wsBinary, env(), "list-projects", "--workspace", "gone")
	if got.ok || !strings.Contains(got.stderr, `--workspace names workspace "gone"`) {
		t.Errorf("an unconfigured --workspace: %+v", got)
	}
}

func TestListProjectsShowsSharesOfRecentPosts(t *testing.T) {
	at := func(text string) time.Time {
		moment, err := time.Parse("2006-01-02 15:04", text)
		if err != nil {
			t.Fatal(err)
		}
		return moment
	}
	server := newFakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		reply(w, http.StatusOK, api.ProjectsResponse{
			Projects: []api.Project{
				{Name: "duck-pond", Latest: at("2026-10-10 20:41"),
					RecentPosts: 1204},
				{Name: "heron", Latest: at("2026-10-09 14:10"),
					RecentPosts: 4},
				{Name: "nest", Latest: at("2026-10-08 09:15"),
					RecentPosts: 1},
				{Name: "old-lake", Latest: at("2026-05-01 10:00")},
			},
			Count:       6,
			RecentSince: time.Now().Add(-90 * 24 * time.Hour),
			RecentPosts: 3200,
		})
	})
	config := writeConfig(t, "workspaces:\n  general:\n    server: "+
		server.URL+"\n", 0o600)

	got := ws(t, config, "list-projects", "--limit", "4")
	want := `Projects by latest activity, with posts from the last 90 days:
duck-pond   10/10/2026 20:41   1,204 posts   38%
heron       09/10/2026 14:10       4 posts   <1%
nest        08/10/2026 09:15       1 post    <1%
old-lake    01/05/2026 10:00       0 posts    0%
Next: ws list-projects --limit 6 --workspace general (2 more projects)
`
	if !got.ok || got.stdout != want {
		t.Fatalf("list-projects printed\n%s\nwant\n%s%s", got.stdout, want,
			got.stderr)
	}
}

func TestConfiguredPrefixSkipsAskingServers(t *testing.T) {
	first := newFakeServer(t, unexpected(t))
	second := newFakeServer(t, answering("r7"))
	config := writeConfig(t, fmt.Sprintf("workspaces:\n  general:\n    "+
		"server: %s\n  remote:\n    server: %s\n    prefix: r\n",
		first.URL, second.URL), 0o600)
	if got := ws(t, config, "post", "r7"); !got.ok {
		t.Fatal(got)
	}
	if got := second.recorded(); len(got) != 1 || got[0].path != "/api/posts/r7" {
		t.Fatalf("the prefix's server got %v", got)
	}
}

// A parent is always in its child's workspace, so add puts a new post
// in its parent's workspace unless --workspace names one.
func TestAddTakesItsParentsWorkspace(t *testing.T) {
	server := newFakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" &&
			r.URL.Path == "/api/workspaces/personal/posts":
			reply(w, http.StatusCreated, postDetail("p4").Post)
		case r.URL.Path == "/api/workspaces/personal/projects":
			reply(w, http.StatusOK, api.ProjectsResponse{
				Projects: []api.Project{}})
		default:
			refuse(w, http.StatusNotFound, api.CodeRouteNotFound, "no route")
		}
	})
	config := writeConfig(t, fmt.Sprintf("workspaces:\n  general:\n    "+
		"server: %s\n    prefix: e\n  personal:\n    server: %s\n    "+
		"prefix: p\n", server.URL, server.URL), 0o600)
	got := ws(t, config, "add", "Answer", "--project", "pond",
		"--parent", "p3", "--no-recent")
	if !got.ok || !strings.Contains(got.stdout, "Added post [p4].") {
		t.Fatal(got)
	}
	if posted := server.recorded()[0]; posted.path !=
		"/api/workspaces/personal/posts" {
		t.Fatalf("the add went to %s", posted.path)
	}
}

// A subtree is always in its root's workspace, so a search under a post
// looks in that post's workspace, and a --workspace naming another would
// find nothing.
func TestSearchUnderTakesItsPostsWorkspace(t *testing.T) {
	server := newFakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/workspaces/personal/posts":
			reply(w, http.StatusOK, api.MatchPage{})
		default:
			refuse(w, http.StatusNotFound, api.CodeRouteNotFound, "no route")
		}
	})
	config := writeConfig(t, fmt.Sprintf("workspaces:\n  general:\n    "+
		"server: %s\n    prefix: e\n  personal:\n    server: %s\n    "+
		"prefix: p\n", server.URL, server.URL), 0o600)
	for _, command := range []string{"search", "recent"} {
		got := ws(t, config, command, "--under", "p3")
		if !got.ok {
			t.Fatalf("%s --under p3: %+v", command, got)
		}
	}
	for _, command := range []string{"search", "recent", "wait"} {
		got := ws(t, config, command, "--under", "p3", "--workspace",
			"general")
		if got.ok || !strings.Contains(got.stderr, "workspace personal") {
			t.Errorf("%s --under p3 --workspace general: %+v", command, got)
		}
	}
}

// The chat overview marks how many older messages a post has before the
// ones it shows, and how to read them.
func TestOverviewMarksEarlierMessages(t *testing.T) {
	when := time.Date(2026, 10, 9, 5, 20, 0, 0, time.UTC)
	message := func(number int64) api.Message {
		return api.Message{Post: api.Ref{Prefix: "e", ID: 1},
			Number: number, Created: when, Modified: when, Body: "quack",
			Origin: api.Origin{User: "duck"}}
	}
	server := newFakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		reply(w, http.StatusOK, api.ChatOverview{Chats: []api.PostChat{
			{Post: api.PostRef{Ref: api.Ref{Prefix: "e", ID: 1},
				Subject: "Why?"},
				Messages: []api.Message{message(7), message(8)}},
		}})
	})
	config := writeConfig(t, "workspaces:\n  general:\n    server: "+
		server.URL+"\n", 0o600)
	got := ws(t, config, "chat", "recent")
	want := "[e1] Why?\n" +
		"  ... 6 earlier (ws chat read e1 --after e1-m0)\n" +
		"  [e1-m7] 05:20 duck: quack\n  [e1-m8] 05:20 duck: quack\n"
	if !got.ok || got.stdout != want {
		t.Fatalf("overview =\n%s\nwant\n%s (%+v)", got.stdout, want, got)
	}
}

// sendAfterServer serves e1, where a send posts message number sent
// and the chat then holds messages 1 to newest. Heron sent all the
// others.
func sendAfterServer(t *testing.T, sent, newest int64) string {
	when := time.Date(2026, 10, 9, 5, 20, 0, 0, time.UTC)
	message := func(number int64) api.Message {
		user := "heron"
		if number == sent {
			user = "duck"
		}
		return api.Message{Post: api.Ref{Prefix: "e", ID: 1},
			Number: number, Created: when, Modified: when, Body: "quack",
			Origin: api.Origin{User: user}}
	}
	server := newFakeServer(t, hosting(func(w http.ResponseWriter,
		r *http.Request) {
		switch {
		case r.Method == "POST":
			reply(w, http.StatusCreated, message(sent))
		case r.URL.Query().Has("latest"):
			count, _ := strconv.ParseInt(r.URL.Query().Get("latest"), 10, 64)
			page := api.ChatPage{Messages: []api.Message{}, Cursor: newest}
			for n := max(1, newest-count+1); n <= newest; n++ {
				page.Messages = append(page.Messages, message(n))
			}
			reply(w, http.StatusOK, page)
		default:
			refuse(w, http.StatusNotFound, api.CodeRouteNotFound, "no route")
		}
	}, "general=e"))
	return writeConfig(t, "workspaces:\n  general:\n    server: "+
		server.URL+"\n", 0o600)
}

// A message sent after yours, between the send and the read that
// follows it, shows too, and the cursor moves past it, so a wait from
// the cursor neither repeats nor skips a message.
func TestSendAfterShowsMessagesThatRaceIt(t *testing.T) {
	config := sendAfterServer(t, 3, 4)
	got := ws(t, config, "chat", "send", "e1", "quack", "--after", "e1-m1")
	want := "Sent [e1-m3].\n" +
		"[e1-m2] 05:20 heron: quack\n[e1-m3] 05:20 duck: quack\n" +
		"[e1-m4] 05:20 heron: quack\n" +
		"Cursor: e1-m4 (wait for more: ws chat wait e1 --after e1-m4)\n"
	if !got.ok || got.stdout != want {
		t.Fatalf("send =\n%s\nwant\n%s (%+v)", got.stdout, want, got)
	}
}

// When more than 20 messages follow the cursor, a send shows the
// newest 20 and says how to read the ones it left out.
func TestSendAfterLeavesOutOlderMessages(t *testing.T) {
	config := sendAfterServer(t, 24, 25)
	got := ws(t, config, "chat", "send", "e1", "quack", "--after", "e1-m1")
	lines := strings.Split(strings.TrimSuffix(got.stdout, "\n"), "\n")
	if !got.ok || len(lines) != 23 {
		t.Fatalf("send printed %d lines, want 23:\n%s (%+v)",
			len(lines), got.stdout, got)
	}
	want := []string{"Sent [e1-m24].",
		"... 4 earlier (ws chat read e1 --after e1-m1)",
		"[e1-m6] 05:20 heron: quack"}
	if !slices.Equal(lines[:3], want) {
		t.Fatalf("send began\n%s\nwant\n%s", strings.Join(lines[:3], "\n"),
			strings.Join(want, "\n"))
	}
	end := "Cursor: e1-m25 (wait for more: ws chat wait e1 --after e1-m25)"
	if lines[22] != end {
		t.Fatalf("send ended %q, want %q", lines[22], end)
	}
}

// listingServer answers every listing with a page, and records the
// query of each.
func listingServer(
	t *testing.T, page api.MatchPage,
) (string, func() []url.Values) {
	t.Helper()
	var mu sync.Mutex
	var queries []url.Values
	server := newFakeServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/workspaces/general/posts" {
			t.Errorf("request to %s", r.URL.Path)
		}
		mu.Lock()
		queries = append(queries, r.URL.Query())
		mu.Unlock()
		reply(w, http.StatusOK, page)
	})
	config := writeConfig(t, "workspaces:\n  general:\n    server: "+
		server.URL+"\n", 0o600)
	return config, func() []url.Values {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(queries)
	}
}

// A listing shows a match a line, each with its author and the path to
// its post, says on stderr what it ran, and ends with the command that
// lists the older matches, on stderr with --id-only.
func TestListingShowsAMatchALine(t *testing.T) {
	at := func(minute int) time.Time {
		return time.Date(2026, 10, 9, 8, minute, 0, 0, time.UTC)
	}
	ref := func(id int64) api.Ref { return api.Ref{Prefix: "f", ID: id} }
	by := func(user string) api.Origin { return api.Origin{User: user} }
	releases := api.PostRef{Ref: ref(4), Subject: "toolkit releases"}
	release := api.PostRef{Ref: ref(5), Subject: "toolkit 2.4.0"}
	config, queries := listingServer(t, api.MatchPage{
		Matches: []api.Match{
			{Post: api.Post{Ref: ref(21), Subject: "Fixture rules",
				Origin: by("ruth")}, Revision: 90, At: at(59),
				NewMessages: 4},
			{Post: api.Post{Ref: ref(6), Origin: by("lena")},
				Ancestors: []api.PostRef{releases, release},
				Message: &api.Message{Post: ref(6), Number: 4,
					Created: at(58), Modified: at(59), Body: "two\nlines",
					Origin: by("lena")}, Revision: 89, At: at(58)},
			{Post: api.Post{Ref: ref(7), Deleted: true,
				Origin: by("omar")}, Ancestors: []api.PostRef{releases},
				Revision: 85, At: at(40), NewMessages: 1},
			{Post: api.Post{Ref: ref(6), Origin: by("lena")},
				Ancestors: []api.PostRef{releases, release},
				Message: &api.Message{Post: ref(6), Number: 2,
					Created: at(38), Modified: at(38), Deleted: true,
					Origin: by("ines")}, Revision: 81, At: at(38)},
		},
		More: true,
	})

	got := ws(t, config, "recent", "-n", "4", "--project", "ai",
		"--after", "@12")
	want := `[f21] 08:59 ruth: Fixture rules (4 new messages)
[f6-m4] 08:58 lena in f4.f5.f6 (edited): two
    lines
[f7] 08:40 omar in f4 (deleted) (1 new message)
[f6-m2] 08:38 ines in f4.f5.f6 (deleted)
Next: ws recent --before @81 --workspace general --after @12 --limit 4 --project ai (more older matches)
`
	if !got.ok || got.stdout != want {
		t.Fatalf("recent =\n%s\nwant\n%s (%+v)", got.stdout, want, got)
	}
	header := "Newest 4 posts after @12 matching --project ai:\n"
	if got.stderr != header {
		t.Fatalf("recent said %q on stderr, want %q", got.stderr, header)
	}

	got = ws(t, config, "search", "hook", "--id-only")
	if want := "f21\nf6-m4\nf7\nf6-m2\n"; !got.ok || got.stdout != want {
		t.Fatalf("search --id-only =\n%s\nwant\n%s (%+v)", got.stdout,
			want, got)
	}
	stderr := "Newest 20 posts and messages matching hook:\n" +
		"Next: ws search hook --before @81 --workspace general " +
		"(more older matches)\n"
	if got.stderr != stderr {
		t.Fatalf("search --id-only said %q on stderr, want %q",
			got.stderr, stderr)
	}

	sent := queries()
	if sent[0].Has("include-chat") || sent[1].Get("include-chat") != "true" {
		t.Fatalf("recent sent %v and search %v, want chat in search only",
			sent[0], sent[1])
	}

	for command, header := range map[string]string{
		"recent": "Newest 1 post:\n",
		"search": "Newest 1 post or message:\n",
	} {
		got = ws(t, config, command, "-n", "1", "--id-only")
		if !strings.HasPrefix(got.stderr, header) {
			t.Errorf("%s -n 1 said %q on stderr, want %q", command,
				got.stderr, header)
		}
	}
}

// shellWords splits a command as a POSIX shell would, for the words
// and quoting shellQuote writes.
func shellWords(command string) ([]string, error) {
	var words []string
	var word strings.Builder
	inWord, quoted, escaped := false, false, false
	for _, character := range command {
		switch {
		case escaped:
			word.WriteRune(character)
			escaped = false
		case quoted && character != '\'':
			word.WriteRune(character)
		case character == '\'':
			quoted, inWord = !quoted, true
		case character == '\\':
			escaped, inWord = true, true
		case character == ' ':
			if inWord {
				words = append(words, word.String())
				word.Reset()
			}
			inWord = false
		default:
			word.WriteRune(character)
			inWord = true
		}
	}
	if quoted || escaped {
		return nil, fmt.Errorf("unfinished quoting in %q", command)
	}
	if inWord {
		words = append(words, word.String())
	}
	return words, nil
}

// The command a footer names asks for the page before the oldest match
// shown, with the same text, filters and bounds, and gives a time bound
// in local time with its offset.
func TestFooterAsksForTheNextPage(t *testing.T) {
	config, queries := listingServer(t, api.MatchPage{
		Matches: []api.Match{{Post: api.Post{
			Ref: api.Ref{Prefix: "e", ID: 3}, Subject: "Pond"},
			Revision: 812345}},
		More: true,
	})
	env := []string{"WORKSTREAM_CONFIG=" + config, "TZ=Asia/Singapore"}
	for _, after := range []string{"2026-10-09T04:00:00.25Z", "2h"} {
		first := runBinary(t, wsBinary, env, "search", "duck's pond",
			"--posts-only", "--after", after, "--no-project", "x", "-n",
			"1", "--include-superseded")
		lines := strings.Split(strings.TrimSpace(first.stdout), "\n")
		footer, ok := strings.CutPrefix(lines[len(lines)-1], "Next: ws ")
		if !first.ok || !ok {
			t.Fatalf("search printed no footer: %+v", first)
		}
		footer = strings.TrimSuffix(footer, " (more older matches)")
		if !regexp.MustCompile(`--after \S*\+08:00 `).MatchString(footer) {
			t.Fatalf("the footer gave --after %s as %q, want local time",
				after, footer)
		}
		words, err := shellWords(footer)
		if err != nil {
			t.Fatal(err)
		}
		if next := runBinary(t, wsBinary, env, words...); !next.ok {
			t.Fatalf("the footer's command failed: %+v", next)
		}

		all := queries()
		sent := all[len(all)-2:]
		if sent[1].Get("before") != "@812345" {
			t.Fatalf("the next page sent before %q, want @812345",
				sent[1].Get("before"))
		}
		sent[1].Del("before")
		if !reflect.DeepEqual(sent[0], sent[1]) {
			t.Fatalf("the next page sent %v, want %v and before", sent[1],
				sent[0])
		}
	}
}

// TestResolutionMovesOnOnlyForAnAbsentPrefix checks that an id goes to
// the first server listing its prefix, and that only that server's own
// workspace's secret travels, and only with the post request.
func TestResolutionMovesOnOnlyForAnAbsentPrefix(t *testing.T) {
	first := newFakeServer(t, hosting(unexpected(t), "general=e"))
	second := newFakeServer(t, hosting(answering("p3"), "personal=p"))
	config := writeConfig(t, fmt.Sprintf("workspaces:\n  general:\n    "+
		"server: %s\n    auth:\n      secret: first-secret\n  personal:\n"+
		"    server: %s\n    auth:\n      secret: second-secret\n",
		first.URL, second.URL), 0o600)
	if got := ws(t, config, "post", "p3"); !got.ok ||
		!strings.Contains(got.stdout, "[p3]") {
		t.Fatal(got)
	}

	want := []request{{"GET", "/api/workspaces", ""}}
	if got := first.recorded(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("first server got %v, want %v", got, want)
	}
	want = []request{
		{"GET", "/api/workspaces", ""},
		{"GET", "/api/posts/p3", "Bearer second-secret"},
	}
	if got := second.recorded(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("second server got %v, want %v", got, want)
	}
}

func TestSecretNeverReachesAnotherServer(t *testing.T) {
	first := newFakeServer(t, hosting(unexpected(t), "general=e"))
	// The second server hosts a workspace of the same name as the one
	// the config gives a secret on the first server.
	second := newFakeServer(t, hosting(answering("p3"), "general=p"))
	config := writeConfig(t, fmt.Sprintf("workspaces:\n  general:\n    "+
		"server: %s\n    auth:\n      secret: first-secret\n  other:\n"+
		"    server: %s\n", first.URL, second.URL), 0o600)
	if got := ws(t, config, "post", "p3"); !got.ok {
		t.Fatal(got)
	}
	for _, request := range second.recorded() {
		if request.auth != "" {
			t.Errorf("the second server got a credential: %+v", request)
		}
	}
}

// TestResolutionStopsOnAnyOtherFailure checks that only a server that
// answers without the prefix passes the question on.
func TestResolutionStopsOnAnyOtherFailure(t *testing.T) {
	failing := map[string]http.HandlerFunc{
		"server error": func(w http.ResponseWriter, r *http.Request) {
			refuse(w, http.StatusInternalServerError, api.CodeInternal,
				"internal server error")
		},
		"malformed listing": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set(version.APIHeader, strconv.Itoa(version.API))
			fmt.Fprint(w, `{"workspaces": [`)
		},
		"another API version": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set(version.APIHeader, "1")
			fmt.Fprint(w, `{"workspaces": []}`)
		},
	}
	for name, handler := range failing {
		t.Run(name, func(t *testing.T) {
			first := newFakeServer(t, handler)
			second := newFakeServer(t, hosting(answering("p3"), "personal=p"))
			got := ws(t, writeConfig(t, fmt.Sprintf("workspaces:\n  a:\n    "+
				"server: %s\n  b:\n    server: %s\n", first.URL, second.URL),
				0o600), "post", "p3")
			if got.ok || !strings.Contains(got.stderr, first.URL) {
				t.Errorf("want an error naming %s: %+v", first.URL, got)
			}
			if requests := second.recorded(); len(requests) != 0 {
				t.Errorf("the next server was asked: %v", requests)
			}
		})
	}

	t.Run("unreachable", func(t *testing.T) {
		down := closedServer(t)
		second := newFakeServer(t, hosting(answering("p3"), "personal=p"))
		got := ws(t, writeConfig(t, fmt.Sprintf("workspaces:\n  a:\n    "+
			"server: %s\n  b:\n    server: %s\n", down, second.URL), 0o600),
			"post", "p3")
		if got.ok || !strings.Contains(got.stderr, down) {
			t.Errorf("want an error naming %s: %+v", down, got)
		}
		if requests := second.recorded(); len(requests) != 0 {
			t.Errorf("the next server was asked: %v", requests)
		}
	})
}

// TestFailuresAreNeverAnUnknownPrefix checks that a resolved post's own
// failures are reported as what they are, from its server alone.
func TestFailuresAreNeverAnUnknownPrefix(t *testing.T) {
	failures := map[string]struct {
		handler http.HandlerFunc
		want    string
	}{
		"missing post": {func(w http.ResponseWriter, r *http.Request) {
			refuse(w, http.StatusNotFound, api.CodePostNotFound,
				"post not found: p3")
		}, "post not found: p3"},
		"auth failure": {func(w http.ResponseWriter, r *http.Request) {
			refuse(w, http.StatusUnauthorized, api.CodeUnauthorized,
				"workspace personal needs its secret")
		}, "needs its secret"},
	}
	for name, failure := range failures {
		t.Run(name, func(t *testing.T) {
			first := newFakeServer(t, hosting(failure.handler, "personal=p"))
			second := newFakeServer(t, hosting(answering("p3"), "other=p"))
			got := ws(t, writeConfig(t, fmt.Sprintf("workspaces:\n  a:\n    "+
				"server: %s\n  b:\n    server: %s\n", first.URL, second.URL),
				0o600), "post", "p3")
			if got.ok || !strings.Contains(got.stderr, failure.want) ||
				strings.Contains(got.stderr, "prefix") {
				t.Errorf("want %q and no prefix error: %+v", failure.want, got)
			}
			if requests := second.recorded(); len(requests) != 0 {
				t.Errorf("another server was asked: %v", requests)
			}
		})
	}
}

func TestLinksCannotJoinServers(t *testing.T) {
	first := newFakeServer(t, unexpected(t))
	second := newFakeServer(t, unexpected(t))
	config := writeConfig(t, fmt.Sprintf("workspaces:\n  general:\n    "+
		"server: %s\n    prefix: e\n  remote:\n    server: %s\n    "+
		"prefix: r\n", first.URL, second.URL), 0o600)
	got := ws(t, config, "add", "Linked", "--project", "pond",
		"--link", "see=r4")
	if got.ok || !strings.Contains(got.stderr, "cannot join posts on "+
		"different servers") {
		t.Fatalf("a link to another server: %+v", got)
	}
}

// TestUncertainWritesAreReportedNotRepeated checks that a write that
// may have reached the server is sent once, and reported as possibly
// done, whatever went wrong after it left.
func TestUncertainWritesAreReportedNotRepeated(t *testing.T) {
	answer := func(write func(w http.ResponseWriter)) http.HandlerFunc {
		return hosting(func(w http.ResponseWriter, r *http.Request) {
			write(w)
		}, "general=e")
	}
	uncertain := map[string]http.HandlerFunc{
		"no version header": answer(func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"ref":"e1"}`)
		}),
		"another version": answer(func(w http.ResponseWriter) {
			w.Header().Set(version.APIHeader, "1")
			w.WriteHeader(http.StatusCreated)
		}),
		"truncated body": answer(func(w http.ResponseWriter) {
			w.Header().Set(version.APIHeader, strconv.Itoa(version.API))
			w.Header().Set("Content-Length", "500")
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"ref":"e1",`)
		}),
		"malformed JSON": answer(func(w http.ResponseWriter) {
			w.Header().Set(version.APIHeader, strconv.Itoa(version.API))
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"ref": e1}`)
		}),
		"unreadable refusal": answer(func(w http.ResponseWriter) {
			w.Header().Set(version.APIHeader, strconv.Itoa(version.API))
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `<html>`)
		}),
		"server error": answer(func(w http.ResponseWriter) {
			refuse(w, http.StatusServiceUnavailable, api.CodeUnavailable,
				"server is busy or the request timed out")
		}),
		"timeout": answer(func(w http.ResponseWriter) {
			time.Sleep(time.Second)
		}),
	}
	for name, handler := range uncertain {
		t.Run(name, func(t *testing.T) {
			server := newFakeServer(t, handler)
			config := writeConfig(t, "workspaces:\n  general:\n    server: "+
				server.URL+"\n", 0o600)
			got := runBinary(t, wsBinary, []string{"WORKSTREAM_CONFIG=" +
				config, "WORKSTREAM_TIMEOUT=300ms"}, "add", "Once",
				"--project", "pond")
			if got.ok || !strings.Contains(got.stderr, "may have taken "+
				"effect") || !strings.Contains(got.stderr, "Check 'ws recent") {
				t.Errorf("want a possibly-done report: %+v", got)
			}
			if requests := server.recorded(); len(requests) != 1 {
				t.Errorf("want exactly one request, got %v", requests)
			}
		})
	}

	t.Run("refused connection", func(t *testing.T) {
		config := writeConfig(t, "workspaces:\n  general:\n    server: "+
			closedServer(t)+"\n", 0o600)
		got := ws(t, config, "add", "Never sent", "--project", "pond")
		if got.ok || strings.Contains(got.stderr, "may have taken effect") {
			t.Errorf("an unsent write is certain: %+v", got)
		}
	})
	t.Run("well-formed refusal", func(t *testing.T) {
		server := newFakeServer(t, func(w http.ResponseWriter, r *http.Request) {
			refuse(w, http.StatusConflict, api.CodeConflict, "link loop")
		})
		config := writeConfig(t, "workspaces:\n  general:\n    server: "+
			server.URL+"\n", 0o600)
		got := ws(t, config, "add", "Refused", "--project", "pond")
		if got.ok || strings.Contains(got.stderr, "may have taken effect") {
			t.Errorf("a refusal is certain: %+v", got)
		}
	})
}

// listing answers a search with posts changed at the given times.
func listing(times ...time.Time) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var page api.MatchPage
		for i, when := range times {
			page.Matches = append(page.Matches, api.Match{
				Post: api.Post{
					Ref:     api.Ref{Prefix: "e", ID: int64(i + 1)},
					Created: when, Modified: when, Subject: "Duck"},
				At: when,
			})
		}
		reply(w, http.StatusOK, page)
	}
}

func TestTimesShowDatesOnlyWhenDaysDiffer(t *testing.T) {
	utcPlus8 := []string{"TZ=Asia/Singapore"}
	tests := map[string]struct {
		times []time.Time
		want  []string
	}{
		"one local day": {[]time.Time{
			time.Date(2025, 7, 23, 1, 0, 0, 0, time.UTC),
			time.Date(2025, 7, 23, 15, 59, 0, 0, time.UTC),
		}, []string{"[e1] 09:00: Duck", "[e2] 23:59: Duck"}},
		// One UTC day, but the second falls on the next day at UTC+8.
		"two local days": {[]time.Time{
			time.Date(2025, 7, 23, 1, 0, 0, 0, time.UTC),
			time.Date(2025, 7, 23, 16, 0, 0, 0, time.UTC),
		}, []string{"[e1] 23/07/2025 09:00: Duck",
			"[e2] 24/07/2025 00:00: Duck"}},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			server := newFakeServer(t, listing(test.times...))
			config := writeConfig(t, "workspaces:\n  general:\n    server: "+
				server.URL+"\n", 0o600)
			got := runBinary(t, wsBinary, append(utcPlus8,
				"WORKSTREAM_CONFIG="+config), "search")
			lines := strings.Split(strings.TrimSpace(got.stdout), "\n")
			if strings.Join(lines, "\n") != strings.Join(test.want, "\n") {
				t.Errorf("got\n%s\nwant\n%s", got.stdout,
					strings.Join(test.want, "\n"))
			}
		})
	}
}

func TestForumNamesItselfAndReadsOnlyItsConfig(t *testing.T) {
	home := t.TempDir()
	server := newFakeServer(t, hosting(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(time.Second)
	}, "general=e"))
	wsConfig := writeConfig(t, "workspaces:\n  general:\n    server: "+
		server.URL+"\n", 0o600)
	forum := func(env ...string) func(args ...string) result {
		return func(args ...string) result {
			t.Helper()
			command := exec.Command(forumBinary, args...)
			command.Env = append([]string{"PATH=" + os.Getenv("PATH"),
				"HOME=" + home, "WORKSTREAM_CONFIG=" + wsConfig}, env...)
			var stdout, stderr strings.Builder
			command.Stdout, command.Stderr = &stdout, &stderr
			err := command.Run()
			return result{stdout.String(), stderr.String(), err == nil}
		}
	}

	got := forum()("recent")
	want := filepath.Join(home, ".config/forum/config.yml")
	if got.ok || !strings.Contains(got.stderr, "no config file at "+want) ||
		!strings.Contains(got.stderr, "'forum help'") {
		t.Errorf("forum without its config: %+v", got)
	}

	withConfig := forum("FORUM_CONFIG="+wsConfig,
		"FORUM_TIMEOUT=200ms")
	got = withConfig("add", "Lost", "--project", "pond")
	if !strings.Contains(got.stderr, "Check 'forum recent") ||
		!strings.Contains(got.stderr, "forum did not retry") {
		t.Errorf("forum's retry advice: %+v", got)
	}
	got = withConfig("post")
	if !strings.Contains(got.stderr, "usage: forum post <ref>") {
		t.Errorf("forum's usage error: %+v", got)
	}
	help := withConfig("help").stdout
	for _, line := range strings.Split(help, "\n") {
		if regexp.MustCompile(`\bws\b|WORKSTREAM`).MatchString(line) {
			t.Errorf("forum help names ws: %q", line)
		}
	}
	if !strings.Contains(help, "FORUM_DEFAULT_WORKSPACE") ||
		!strings.Contains(help, "~/.config/forum/config.yml") {
		t.Errorf("forum help misses its settings:\n%s", help)
	}
}

// A wait sends the key of its own changes, the session it would record
// now, which is none when the workspace does not capture it. The
// command it suggests next filters the same way.
func TestWaitsExcludeTheirOwnSession(t *testing.T) {
	var mu sync.Mutex
	var queries []url.Values
	server := newFakeServer(t, hosting(func(w http.ResponseWriter,
		r *http.Request) {
		mu.Lock()
		queries = append(queries, r.URL.Query())
		mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/messages") {
			reply(w, http.StatusOK, api.ChatPage{Messages: []api.Message{},
				Cursor: 7})
			return
		}
		reply(w, http.StatusOK, api.ChangesPage{Changes: []api.Change{},
			Cursor: 9})
	}, "general=e"))
	config := func(capture string) string {
		return writeConfig(t, "workspaces:\n  general:\n    server: "+
			server.URL+"\n    metadata_capture: "+capture+"\n", 0o600)
	}
	tests := []struct {
		name, capture string
		args          []string
		session       []string
		next          string
	}{
		{"wait", "all", []string{"wait", "--after", "@3", "--project",
			"duck pond"}, []string{"lead"},
			"Cursor: @9 (wait for more: ws wait --after @9 --workspace " +
				"general --project 'duck pond')"},
		{"wait without the session", "none", []string{"wait", "--after",
			"@3"}, []string{""}, "--after @9 --workspace general)"},
		{"wait including mine", "all", []string{"wait", "--after", "@3",
			"--include-mine"}, nil,
			"--after @9 --workspace general --include-mine)"},
		{"chat wait", "all", []string{"chat", "wait", "e1", "--after",
			"e1-m3", "--content", "quack"}, []string{"lead"},
			"Cursor: e1-m7 (wait for more: ws chat wait e1 --after e1-m7 " +
				"--content quack)"},
		{"chat wait without the session", "none", []string{"chat", "wait",
			"e1", "--after", "e1-m3"}, []string{""}, "--after e1-m7)"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mu.Lock()
			queries = nil
			mu.Unlock()
			got := runBinary(t, wsBinary, []string{
				"WORKSTREAM_CONFIG=" + config(test.capture),
				"CLAUDE_CODE_SESSION_ID=lead"},
				append(test.args, "--timeout", "1ms")...)
			if !got.ok || !strings.Contains(got.stdout, test.next) {
				t.Fatalf("got %+v, want %q", got, test.next)
			}
			mu.Lock()
			defer mu.Unlock()
			for _, query := range queries {
				if query.Get("after") == "" {
					continue
				}
				if !slices.Equal(query["exclude-claude-session"],
					test.session) || query.Has("exclude-user") !=
					(test.session != nil) {
					t.Fatalf("sent %v, want the session %q", query,
						test.session)
				}
			}
		})
	}
}

// A chat wait that leaves messages unchecked reads on with another wait,
// since a chat read would drop its filters and show your own messages.
func TestChatWaitReadsOnWithItsFilters(t *testing.T) {
	server := newFakeServer(t, hosting(func(w http.ResponseWriter,
		r *http.Request) {
		reply(w, http.StatusOK, api.ChatPage{
			Messages: []api.Message{{Post: api.Ref{Prefix: "e", ID: 1},
				Number: 20, Body: "quack"}},
			Cursor: 20, Remaining: 6})
	}, "general=e"))
	config := writeConfig(t, "workspaces:\n  general:\n    server: "+
		server.URL+"\n", 0o600)
	got := ws(t, config, "chat", "wait", "e1", "--after", "e1-m3",
		"--content", "quack")
	want := "Next: ws chat wait e1 --after e1-m20 --content quack " +
		"(more messages to check)\n"
	if !got.ok || !strings.HasSuffix(got.stdout, want) {
		t.Fatalf("got %+v, want it to end with %q", got, want)
	}
}

// A change shows at the time it was made, so an edited or deleted
// message shows when it was edited or deleted, as a post does.
func TestChangesShowEachAtItsLatestChange(t *testing.T) {
	at := func(minute int) time.Time {
		return time.Date(2026, 10, 9, 5, minute, 0, 0, time.UTC)
	}
	pond := api.Ref{Prefix: "e", ID: 1}
	duck := api.Origin{User: "duck"}
	post := api.ListedPost{Post: api.Post{Ref: pond, Created: at(10),
		Modified: at(30), Subject: "Pond", Origin: duck}}
	server := newFakeServer(t, hosting(func(w http.ResponseWriter,
		r *http.Request) {
		reply(w, http.StatusOK, api.ChangesPage{Changes: []api.Change{
			{Post: post},
			{Post: post, Message: &api.Message{Post: pond, Number: 2,
				Created: at(20), Modified: at(41), Body: "quack",
				Origin: duck}},
			{Post: post, Message: &api.Message{Post: pond, Number: 3,
				Created: at(22), Modified: at(45), Deleted: true,
				Origin: duck}},
		}, Cursor: 9})
	}, "general=e"))
	config := writeConfig(t, "workspaces:\n  general:\n    server: "+
		server.URL+"\n", 0o600)
	got := ws(t, config, "wait", "--after", "@3")
	want := "[e1] 05:30 duck: Pond\n" +
		"[e1-m2] 05:41 duck in e1 (edited): quack\n" +
		"[e1-m3] 05:45 duck in e1 (deleted)\n"
	if !got.ok || !strings.HasPrefix(got.stdout, want) {
		t.Fatalf("wait =\n%s\nwant it to begin\n%s (%+v)", got.stdout,
			want, got)
	}
}

// A date, or a date and time, reads in local time, a span counts back
// from now to a whole second, and @ names a revision.
func TestBoundFlagsReadLocalDatesSpansAndRevisions(t *testing.T) {
	now := time.Date(2026, 10, 11, 9, 30, 0, 0, time.UTC)
	for value, want := range map[string]api.Bound{
		"2d":         {Time: now.Add(-48 * time.Hour)},
		"6h":         {Time: now.Add(-6 * time.Hour)},
		"90m":        {Time: now.Add(-90 * time.Minute)},
		"2026-10-09": {Time: time.Date(2026, 10, 9, 0, 0, 0, 0, time.Local)},
		"2026-10-09T14:30": {Time: time.Date(2026, 10, 9, 14, 30, 0, 0,
			time.Local)},
		"2026-10-09 14:30:05": {Time: time.Date(2026, 10, 9, 14, 30, 5, 0,
			time.Local)},
		"2026-10-09T04:00:00+10:00": {Time: time.Date(2026, 10, 8, 18, 0,
			0, 0, time.UTC)},
		"@812345": {Revision: 812345},
	} {
		got, err := parseBoundFlag("--after", value, now)
		if err != nil || !got.Time.Equal(want.Time) ||
			got.Revision != want.Revision {
			t.Errorf("%q = %v, %v, want %v", value, got, err, want)
		}
	}
	for _, value := range []string{"-2h", "2", "yesterday", "2026-13-01",
		"@0", "@", "@-3", "@12a"} {
		if _, err := parseBoundFlag("--after", value, now); err == nil {
			t.Errorf("%q was accepted", value)
		}
	}
	for _, value := range []string{"2d", "90m"} {
		got, err := parseBoundFlag("--after", value,
			now.Add(999*time.Millisecond))
		if err != nil || got.Time.Nanosecond() != 0 {
			t.Errorf("%q = %v, %v, want a whole second", value, got, err)
		}
	}
}

// A chat's --after names a message of that chat, or its start as -m0,
// or a time. A message of another chat, or a revision, is refused
// rather than read as a number in this one.
func TestChatAfterKeepsToItsOwnChat(t *testing.T) {
	now := time.Date(2026, 10, 11, 9, 30, 0, 0, time.UTC)
	post := api.Ref{Prefix: "e", ID: 40}
	for value, want := range map[string]api.ChatBound{
		"e40-m3": {Number: 3},
		"e40-m0": {},
		"2h":     {Time: now.Add(-2 * time.Hour)},
	} {
		got, err := parseChatAfter(post, value, now)
		if err != nil || !got.Time.Equal(want.Time) ||
			got.Number != want.Number {
			t.Errorf("%q = %v, %v, want %v", value, got, err, want)
		}
	}
	for _, value := range []string{"e41-m3", "e4-m0", "f40-m3", "@3"} {
		if got, err := parseChatAfter(post, value, now); err == nil {
			t.Errorf("%q was accepted as %v", value, got)
		}
	}
}

// A change cursor is a revision, @0 included, and a time is refused
// with the command that lists what changed since it.
func TestChangeCursorIsARevision(t *testing.T) {
	for value, want := range map[string]int64{"@0": 0, "@812345": 812345} {
		got, err := parseRevisionFlag("--after", value)
		if err != nil || got != want {
			t.Errorf("%q = %d, %v, want %d", value, got, err, want)
		}
	}
	_, err := parseRevisionFlag("--after", "2h")
	if err == nil || !strings.Contains(err.Error(),
		"'ws recent --include-chat --after 2h'") {
		t.Errorf("a time gave %v", err)
	}
}
