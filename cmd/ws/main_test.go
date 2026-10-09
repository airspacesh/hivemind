package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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

func entryDetail(ref string) api.EntryDetail {
	parsed, _ := api.ParseRef(ref)
	when := time.Date(2026, 10, 9, 5, 20, 0, 0, time.UTC)
	return api.EntryDetail{ListedEntry: api.ListedEntry{Entry: api.Entry{
		Ref: parsed, Created: when, Modified: when, Type: "note",
		Subject: "Count the ducklings",
	}, Activity: when}}
}

func answering(ref string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/entries/"+ref {
			reply(w, http.StatusOK, entryDetail(ref))
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
		"unknown key": {"workspaces:\n  default:\n    server: http://h:1\n" +
			"    sever: http://h:2\n", "field sever not found"},
		"unknown capture": {"workspaces:\n  default:\n    server: " +
			"http://h:1\n    metadata_capture: [hostname, user]\n",
			`metadata_capture lists "user"`},
		"capture scalar": {"workspaces:\n  default:\n    server: " +
			"http://h:1\n    metadata_capture: hostname\n",
			"must be all, none, or a list"},
		"no scheme": {"workspaces:\n  default:\n    server: h:1\n",
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
	server := newFakeServer(t, hosting(unexpected(t), "default=e"))
	open := "workspaces:\n  default:\n    server: " + server.URL + "\n"
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
	config := writeConfig(t, fmt.Sprintf("workspaces:\n  default:\n    "+
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
		{env(), nil, "default"},
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

func TestConfiguredPrefixSkipsAskingServers(t *testing.T) {
	first := newFakeServer(t, unexpected(t))
	second := newFakeServer(t, answering("r7"))
	config := writeConfig(t, fmt.Sprintf("workspaces:\n  default:\n    "+
		"server: %s\n  remote:\n    server: %s\n    prefix: r\n",
		first.URL, second.URL), 0o600)
	if got := ws(t, config, "entry", "r7"); !got.ok {
		t.Fatal(got)
	}
	if got := second.recorded(); len(got) != 1 || got[0].path != "/api/entries/r7" {
		t.Fatalf("the prefix's server got %v", got)
	}
}

// TestResolutionMovesOnOnlyForAnAbsentPrefix checks that an id goes to
// the first server listing its prefix, and that only that server's own
// workspace's secret travels, and only with the entry request.
func TestResolutionMovesOnOnlyForAnAbsentPrefix(t *testing.T) {
	first := newFakeServer(t, hosting(unexpected(t), "default=e"))
	second := newFakeServer(t, hosting(answering("p3"), "personal=p"))
	config := writeConfig(t, fmt.Sprintf("workspaces:\n  default:\n    "+
		"server: %s\n    auth:\n      secret: first-secret\n  personal:\n"+
		"    server: %s\n    auth:\n      secret: second-secret\n",
		first.URL, second.URL), 0o600)
	if got := ws(t, config, "entry", "p3"); !got.ok ||
		!strings.Contains(got.stdout, "[p3]") {
		t.Fatal(got)
	}

	want := []request{{"GET", "/api/workspaces", ""}}
	if got := first.recorded(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("first server got %v, want %v", got, want)
	}
	want = []request{
		{"GET", "/api/workspaces", ""},
		{"GET", "/api/entries/p3", "Bearer second-secret"},
	}
	if got := second.recorded(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("second server got %v, want %v", got, want)
	}
}

func TestSecretNeverReachesAnotherServer(t *testing.T) {
	first := newFakeServer(t, hosting(unexpected(t), "default=e"))
	// The second server hosts a workspace of the same name as the one
	// the config gives a secret on the first server.
	second := newFakeServer(t, hosting(answering("p3"), "default=p"))
	config := writeConfig(t, fmt.Sprintf("workspaces:\n  default:\n    "+
		"server: %s\n    auth:\n      secret: first-secret\n  other:\n"+
		"    server: %s\n", first.URL, second.URL), 0o600)
	if got := ws(t, config, "entry", "p3"); !got.ok {
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
				0o600), "entry", "p3")
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
			"entry", "p3")
		if got.ok || !strings.Contains(got.stderr, down) {
			t.Errorf("want an error naming %s: %+v", down, got)
		}
		if requests := second.recorded(); len(requests) != 0 {
			t.Errorf("the next server was asked: %v", requests)
		}
	})
}

// TestFailuresAreNeverAnUnknownPrefix checks that a resolved entry's
// own failures are reported as what they are, from its server alone.
func TestFailuresAreNeverAnUnknownPrefix(t *testing.T) {
	failures := map[string]struct {
		handler http.HandlerFunc
		want    string
	}{
		"missing entry": {func(w http.ResponseWriter, r *http.Request) {
			refuse(w, http.StatusNotFound, api.CodeEntryNotFound,
				"entry not found: p3")
		}, "entry not found: p3"},
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
				0o600), "entry", "p3")
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
	config := writeConfig(t, fmt.Sprintf("workspaces:\n  default:\n    "+
		"server: %s\n    prefix: e\n  remote:\n    server: %s\n    "+
		"prefix: r\n", first.URL, second.URL), 0o600)
	got := ws(t, config, "add", "note", "Linked", "--project", "pond",
		"--link", "see=r4")
	if got.ok || !strings.Contains(got.stderr, "cannot join entries on "+
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
		}, "default=e")
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
			config := writeConfig(t, "workspaces:\n  default:\n    server: "+
				server.URL+"\n", 0o600)
			got := runBinary(t, wsBinary, []string{"WORKSTREAM_CONFIG=" +
				config, "WORKSTREAM_TIMEOUT=300ms"}, "add", "note", "Once",
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
		config := writeConfig(t, "workspaces:\n  default:\n    server: "+
			closedServer(t)+"\n", 0o600)
		got := ws(t, config, "add", "note", "Never sent", "--project", "pond")
		if got.ok || strings.Contains(got.stderr, "may have taken effect") {
			t.Errorf("an unsent write is certain: %+v", got)
		}
	})
	t.Run("well-formed refusal", func(t *testing.T) {
		server := newFakeServer(t, func(w http.ResponseWriter, r *http.Request) {
			refuse(w, http.StatusConflict, api.CodeConflict, "link loop")
		})
		config := writeConfig(t, "workspaces:\n  default:\n    server: "+
			server.URL+"\n", 0o600)
		got := ws(t, config, "add", "note", "Refused", "--project", "pond")
		if got.ok || strings.Contains(got.stderr, "may have taken effect") {
			t.Errorf("a refusal is certain: %+v", got)
		}
	})
}

// listing answers a search with entries changed at the given times.
func listing(times ...time.Time) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		result := api.SearchResult{Total: len(times)}
		for i, when := range times {
			result.Entries = append(result.Entries, api.ListedEntry{
				Entry: api.Entry{Ref: api.Ref{Prefix: "e", ID: int64(i + 1)},
					Created: when, Modified: when, Type: "note",
					Subject: "Duck"},
				Activity: when,
			})
		}
		reply(w, http.StatusOK, result)
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
		}, []string{"[e1] (note) 09:00: Duck", "[e2] (note) 23:59: Duck"}},
		// One UTC day, but the second falls on the next day at UTC+8.
		"two local days": {[]time.Time{
			time.Date(2025, 7, 23, 1, 0, 0, 0, time.UTC),
			time.Date(2025, 7, 23, 16, 0, 0, 0, time.UTC),
		}, []string{"[e1] (note) 23/07/2025 09:00: Duck",
			"[e2] (note) 24/07/2025 00:00: Duck"}},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			server := newFakeServer(t, listing(test.times...))
			config := writeConfig(t, "workspaces:\n  default:\n    server: "+
				server.URL+"\n", 0o600)
			got := runBinary(t, wsBinary, append(utcPlus8,
				"WORKSTREAM_CONFIG="+config), "recent", "--flat")
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
	}, "default=e"))
	wsConfig := writeConfig(t, "workspaces:\n  default:\n    server: "+
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
	got = withConfig("add", "note", "Lost", "--project", "pond")
	if !strings.Contains(got.stderr, "Check 'forum recent") ||
		!strings.Contains(got.stderr, "forum did not retry") {
		t.Errorf("forum's retry advice: %+v", got)
	}
	got = withConfig("entry")
	if !strings.Contains(got.stderr, "usage: forum entry <ref>") {
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
