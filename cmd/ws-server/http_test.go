package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/airspacesh/workstream/internal/api"
	"github.com/airspacesh/workstream/internal/store"
	"github.com/airspacesh/workstream/internal/version"
)

const (
	personalSecret = "personal-secret"
	teamSecret     = "team-secret"
)

// testConfig hosts an open workspace and two protected ones.
func testConfig(t *testing.T) serverConfig {
	return serverConfig{
		Data:    t.TempDir(),
		Timeout: 5 * time.Second,
		Workspaces: map[string]workspaceConfig{
			"default": {Prefix: "e"},
			"personal": {Prefix: "p",
				Auth: &authConfig{Secret: personalSecret}},
			"team": {Prefix: "t", Auth: &authConfig{Secret: teamSecret}},
		},
	}
}

type testServer struct {
	url string
}

// startServer serves the routes on a loopback port, as ws-server does.
func startServer(t *testing.T, config serverConfig) testServer {
	t.Helper()
	stream, err := store.Open(context.Background(),
		filepath.Join(config.Data, "ws.db"), time.Second,
		config.workspaces())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stream.Close() })
	h := &handler{
		store:       stream,
		workspaces:  newServerWorkspaces(stream.Workspaces(), config),
		workTimeout: serverWorkTimeout(config.Timeout),
	}
	server := httptest.NewServer(h.routes())
	t.Cleanup(server.Close)
	return testServer{url: server.URL}
}

// do sends a request with the API version, and the secret when it is
// not empty, and returns the status and body.
func (s testServer) do(
	t *testing.T, method, path, body, secret string,
) (int, string) {
	t.Helper()
	request, err := http.NewRequest(method, s.url+path,
		strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set(version.APIHeader, strconv.Itoa(version.API))
	if secret != "" {
		request.Header.Set("Authorization", "Bearer "+secret)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	text, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, string(text)
}

func (s testServer) must(
	t *testing.T, method, path, body, secret string, status int,
) string {
	t.Helper()
	got, text := s.do(t, method, path, body, secret)
	if got != status {
		t.Fatalf("%s %s: %d %s, want %d", method, path, got, text, status)
	}
	return text
}

func entryBody(subject string, links ...string) string {
	body := `{"subject":"` + subject + `",` +
		`"project":"p","origin":{"user":"duck"}`
	if len(links) > 0 {
		body += `,"links":[` + strings.Join(links, ",") + `]`
	}
	return body + "}"
}

func errorCode(t *testing.T, text string) api.ErrorCode {
	t.Helper()
	var response api.ErrorResponse
	if err := json.Unmarshal([]byte(text), &response); err != nil {
		t.Fatalf("decoding error %q: %v", text, err)
	}
	return response.Code
}

func TestEachSecretGrantsOnlyItsOwnWorkspace(t *testing.T) {
	s := startServer(t, testConfig(t))
	s.must(t, "POST", "/api/workspaces/personal/entries",
		entryBody("private"), personalSecret, http.StatusCreated)
	s.must(t, "POST", "/api/workspaces/team/entries",
		entryBody("shared"), teamSecret, http.StatusCreated)

	for _, test := range []struct {
		secret string
		path   string
		status int
	}{
		{"", "/api/workspaces/default/entries", http.StatusOK},
		{"", "/api/workspaces/personal/entries", http.StatusUnauthorized},
		{"", "/api/entries/p1", http.StatusUnauthorized},
		{personalSecret, "/api/workspaces/default/entries", http.StatusOK},
		{personalSecret, "/api/workspaces/personal/entries", http.StatusOK},
		{personalSecret, "/api/entries/p1", http.StatusOK},
		{personalSecret, "/api/workspaces/team/entries",
			http.StatusUnauthorized},
		{personalSecret, "/api/entries/t1", http.StatusUnauthorized},
		{personalSecret, "/api/workspaces/team/messages",
			http.StatusUnauthorized},
		{teamSecret, "/api/entries/t1", http.StatusOK},
		{teamSecret, "/api/entries/p1", http.StatusUnauthorized},
		{"wrong", "/api/workspaces/default/entries",
			http.StatusUnauthorized},
	} {
		status, text := s.do(t, "GET", test.path, "", test.secret)
		if status != test.status {
			t.Fatalf("GET %s with %q: %d %s, want %d", test.path,
				test.secret, status, text, test.status)
		}
		if status == http.StatusUnauthorized &&
			errorCode(t, text) != api.CodeUnauthorized {
			t.Fatalf("GET %s: %s, want code unauthorized", test.path, text)
		}
	}

	s.must(t, "POST", "/api/workspaces/personal/entries",
		entryBody("written with the wrong secret"), teamSecret,
		http.StatusUnauthorized)
	s.must(t, "POST", "/api/entries/p1/messages",
		`{"body":"hi","origin":{}}`, teamSecret, http.StatusUnauthorized)
	s.must(t, "PATCH", "/api/entries/p1", `{"subject":"taken"}`,
		teamSecret, http.StatusUnauthorized)

	request, err := http.NewRequest("GET",
		s.url+"/api/workspaces/default/entries", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set(version.APIHeader, strconv.Itoa(version.API))
	request.Header.Add("Authorization", "Bearer "+personalSecret)
	request.Header.Add("Authorization", "Bearer "+teamSecret)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("two credentials: %d, want 400", response.StatusCode)
	}

	workspaces := s.must(t, "GET", "/api/workspaces", "", "", http.StatusOK)
	want := `{"workspaces":[{"name":"default","prefix":"e"},` +
		`{"name":"personal","prefix":"p"},{"name":"team","prefix":"t"}]}`
	if strings.TrimSpace(workspaces) != want {
		t.Fatalf("workspaces = %s, want %s", workspaces, want)
	}
}

func TestMissingThingsHaveDistinctCodes(t *testing.T) {
	s := startServer(t, testConfig(t))
	s.must(t, "POST", "/api/workspaces/default/entries", entryBody("one"),
		"", http.StatusCreated)
	for _, test := range []struct {
		method string
		path   string
		body   string
		status int
		code   api.ErrorCode
	}{
		{"GET", "/api/entries/x1", "", 404, api.CodeUnknownPrefix},
		{"GET", "/api/entries/e9", "", 404, api.CodeEntryNotFound},
		{"GET", "/api/entries/e1/messages?since=0", "", 200, ""},
		{"GET", "/api/entries/x1/messages", "", 404, api.CodeUnknownPrefix},
		{"GET", "/api/entries/e9/tree", "", 404, api.CodeEntryNotFound},
		{"PATCH", "/api/entries/e1/messages/3",
			`{"body":"x","user":"duck"}`, 404,
			api.CodeMessageNotFound},
		{"GET", "/api/workspaces/nowhere/entries", "", 404,
			api.CodeUnknownWorkspace},
		{"GET", "/api/workspaces/default/entries?under=x1", "", 404,
			api.CodeUnknownPrefix},
		{"GET", "/api/workspaces/default/entries?under=e9", "", 404,
			api.CodeEntryNotFound},
		{"POST", "/api/workspaces/default/entries",
			entryBody("probe", `{"name":"see","ref":"x1"}`), 404,
			api.CodeUnknownPrefix},
		{"POST", "/api/workspaces/default/entries",
			entryBody("probe", `{"name":"see","ref":"e9"}`), 400,
			api.CodeLinkTargetNotFound},
		{"GET", "/api/entries/E1", "", 400, api.CodeBadRequest},
		{"GET", "/api/entries/e1/messages?since=1&latest=2", "", 400,
			api.CodeBadRequest},
		{"GET", "/nowhere", "", 404, api.CodeRouteNotFound},
	} {
		status, text := s.do(t, test.method, test.path, test.body, "")
		if status != test.status {
			t.Fatalf("%s %s: %d %s, want %d", test.method, test.path,
				status, text, test.status)
		}
		if test.code != "" && errorCode(t, text) != test.code {
			t.Fatalf("%s %s: %s, want code %s", test.method, test.path,
				text, test.code)
		}
	}

	request, err := http.NewRequest("GET", s.url+"/api/status", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set(version.APIHeader, "1")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	text, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != 400 ||
		errorCode(t, string(text)) != api.CodeAPIVersion ||
		response.Header.Get(version.APIHeader) != strconv.Itoa(version.API) {
		t.Fatalf("an old client got %d %s", response.StatusCode, text)
	}
}

// TestUnreadableEntriesShowNoTextOverHTTP links open entries every way
// a link may cross workspaces to protected ones holding SECRET, then
// reads every open route without a credential.
func TestUnreadableEntriesShowNoTextOverHTTP(t *testing.T) {
	s := startServer(t, testConfig(t))
	addOpen := func(subject string, links ...string) {
		s.must(t, "POST", "/api/workspaces/default/entries",
			entryBody(subject, links...), personalSecret, http.StatusCreated)
	}
	addSecret := func(links ...string) {
		body := `{"subject":"SECRET","body":"SECRET",` +
			`"project":"SECRET","origin":{"user":"SECRET"}`
		if len(links) > 0 {
			body += `,"links":[` + strings.Join(links, ",") + `]`
		}
		s.must(t, "POST", "/api/workspaces/personal/entries", body+"}",
			personalSecret, http.StatusCreated)
	}
	addOpen("open root")                                  // e1
	addSecret(`{"name":"see","ref":"e1"}`)                // p1
	addOpen("open see", `{"name":"see","ref":"p1"}`)      // e2
	addOpen("open child", `{"name":"parent","ref":"e1"}`) // e3
	addSecret(`{"name":"supersedes","ref":"e3"}`)         // p2
	for _, entry := range []string{"p1", "p2"} {
		s.must(t, "POST", "/api/entries/"+entry+"/messages",
			`{"body":"SECRET","origin":{"user":"SECRET"}}`, personalSecret,
			http.StatusCreated)
	}
	s.must(t, "POST", "/api/entries/e1/messages",
		`{"body":"open","origin":{}}`, "", http.StatusCreated)

	var paths []string
	for _, entry := range []string{"e1", "e2", "e3"} {
		paths = append(paths, "/api/entries/"+entry,
			"/api/entries/"+entry+"/tree?include-superseded=true",
			"/api/entries/"+entry+"/messages",
			"/api/workspaces/default/entries?include-superseded=true&under="+
				entry)
	}
	paths = append(paths,
		"/api/workspaces/default/entries?include-superseded=true",
		"/api/workspaces/default/entries?view=topics&include-superseded=true",
		"/api/workspaces/default/projects",
		"/api/workspaces/default/messages")
	for _, path := range paths {
		text := s.must(t, "GET", path, "", "", http.StatusOK)
		if strings.Contains(text, "SECRET") {
			t.Fatalf("GET %s shows unreadable text: %s", path, text)
		}
	}
	if text := s.must(t, "GET", "/api/entries/e3", "", "",
		http.StatusOK); !strings.Contains(text, `"superseded_by":["p2"]`) {
		t.Fatalf("e3 does not show what supersedes it as a bare ref: %s",
			text)
	}
	if text := s.must(t, "GET", "/api/entries/e2", "", "",
		http.StatusOK); !strings.Contains(text,
		`"links":[{"name":"see","ref":"p1"}]`) {
		t.Fatalf("e2 does not show its link as a bare ref: %s", text)
	}

	_, crossParent := s.do(t, "POST", "/api/workspaces/default/entries",
		entryBody("child", `{"name":"parent","ref":"p1"}`), personalSecret)
	if !strings.Contains(crossParent, "a parent must be in the same "+
		"workspace") {
		t.Fatalf("a parent in another workspace gave %s", crossParent)
	}

	_, unreadable := s.do(t, "POST", "/api/workspaces/default/entries",
		entryBody("probe", `{"name":"see","ref":"p1"}`), "")
	_, missing := s.do(t, "POST", "/api/workspaces/default/entries",
		entryBody("probe", `{"name":"see","ref":"p9"}`), "")
	if strings.Replace(missing, "p9", "p1", 1) != unreadable {
		t.Fatalf("linking to an unreadable entry gave %s, but to a "+
			"missing one %s", unreadable, missing)
	}
}

func TestOnlyThePosterCanEditOverHTTP(t *testing.T) {
	s := startServer(t, testConfig(t))
	s.must(t, "POST", "/api/workspaces/default/entries", entryBody("mine"),
		"", http.StatusCreated)
	s.must(t, "POST", "/api/entries/e1/messages",
		`{"body":"hi","origin":{"user":"duck"}}`, "", http.StatusCreated)

	for _, test := range []struct {
		path   string
		body   string
		status int
		code   api.ErrorCode
	}{
		{"/api/entries/e1", `{"subject":"taken","user":"heron"}`, 403,
			api.CodeNotOwner},
		{"/api/entries/e1/messages/1", `{"body":"taken","user":"heron"}`,
			403, api.CodeNotOwner},
		{"/api/entries/e1", `{"subject":"taken"}`, 400, api.CodeBadRequest},
		{"/api/entries/e1/messages/1", `{"body":"taken"}`, 400,
			api.CodeBadRequest},
		{"/api/entries/e1", `{"subject":"kept","user":"duck"}`, 200, ""},
		{"/api/entries/e1/messages/1", `{"body":"kept","user":"duck"}`,
			200, ""},
	} {
		status, text := s.do(t, "PATCH", test.path, test.body, "")
		if status != test.status {
			t.Fatalf("PATCH %s %s: %d %s, want %d", test.path, test.body,
				status, text, test.status)
		}
		if test.code != "" && errorCode(t, text) != test.code {
			t.Fatalf("PATCH %s %s: %s, want code %s", test.path,
				test.body, text, test.code)
		}
	}
}

func TestChatReadsOverHTTP(t *testing.T) {
	s := startServer(t, testConfig(t))
	s.must(t, "POST", "/api/workspaces/default/entries", entryBody("chat"),
		"", http.StatusCreated)
	for _, body := range []string{"one", "two", "three"} {
		s.must(t, "POST", "/api/entries/e1/messages",
			`{"body":"`+body+`","origin":{"user":"duck"}}`, "",
			http.StatusCreated)
	}
	s.must(t, "PATCH", "/api/entries/e1/messages/1",
		`{"body":"one!","user":"duck"}`, "", http.StatusOK)

	read := func(query string) string {
		t.Helper()
		var page api.ChatPage
		text := s.must(t, "GET", "/api/entries/e1/messages"+query, "", "",
			http.StatusOK)
		if err := json.Unmarshal([]byte(text), &page); err != nil {
			t.Fatal(err)
		}
		var bodies []string
		for _, message := range page.Messages {
			bodies = append(bodies, message.Ref().String()+":"+message.Body)
		}
		return strings.Join(bodies, " ") + " cursor=" +
			strconv.FormatInt(page.Cursor, 10) + " remaining=" +
			strconv.Itoa(page.Remaining)
	}
	for query, want := range map[string]string{
		"?since=0&limit=2": "e1-m1:one! e1-m2:two cursor=2 remaining=1",
		"?since=2":         "e1-m3:three cursor=3 remaining=0",
		"?latest=2":        "e1-m2:two e1-m3:three cursor=3 remaining=0",
	} {
		if got := read(query); got != want {
			t.Fatalf("messages%s = %q, want %q", query, got, want)
		}
	}
}

func TestRequestLogKeepsNoClientText(t *testing.T) {
	var logs bytes.Buffer
	oldOutput := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(oldOutput) })
	s := startServer(t, testConfig(t))

	requests := []struct {
		method string
		target string
		body   string
		secret string
		status int
	}{
		{"POST", "/api/workspaces/default/entries", `{"subject":` +
			`"SUBJECTMARK","body":"BODYMARK",` +
			`"project":"PROJECTMARK","origin":{"user":"duck","dir":"/"}}`,
			personalSecret, http.StatusCreated},
		{"PATCH", "/api/entries/e1", `{"subject":"NEWSUBJECTMARK",` +
			`"body":"NEWBODYMARK","user":"duck"}`, "", http.StatusOK},
		{"POST", "/api/entries/e1/messages",
			`{"body":"MESSAGEMARK","origin":{"user":"duck"}}`, "",
			http.StatusCreated},
		{"PATCH", "/api/entries/e1/messages/1",
			`{"body":"EDITMARK","user":"duck"}`, "", http.StatusOK},
		{"POST", "/api/workspaces/default/entries", `{"FIELDMARK":1}`, "",
			http.StatusBadRequest},
		{"GET", "/api/workspaces/default/entries?subject=SEARCHMARK&limit=5",
			"", "", http.StatusOK},
		{"GET", "/api/workspaces/default/entries?PARAMMARK=1", "", "",
			http.StatusBadRequest},
		{"GET", "/api/workspaces/SPACEMARK/entries", "", "",
			http.StatusNotFound},
		{"GET", "/api/entries/markref1", "", "", http.StatusNotFound},
		{"GET", "/api/entries/IDMARK", "", "", http.StatusBadRequest},
		{"GET", "/PATHMARK", "", "", http.StatusNotFound},
		{"METHODMARK", "/api/status", "", "", http.StatusNotFound},
		{"GET", "/api/status", "", "SECRETMARK", http.StatusOK},
	}
	for _, request := range requests {
		s.must(t, request.method, request.target, request.body,
			request.secret, request.status)
	}

	text := logs.String()
	for _, leak := range []string{"MARK", "markref", personalSecret} {
		if strings.Contains(text, leak) {
			t.Fatalf("request log holds client text %q:\n%s", leak, text)
		}
	}
	for _, want := range []string{
		"issued e1\n",
		"issued e1-m1\n",
		"PATCH /api/entries/{ref} ref=e1 -> 200",
		"PATCH /api/entries/{ref}/messages/{n} ref=e1 n=1 -> 200",
		"GET /api/workspaces/{workspace}/entries workspace=default " +
			"query=limit,subject -> 200",
		"GET /api/workspaces/{workspace}/entries workspace=default " +
			"unknown-query=1 -> 400",
		"GET /api/workspaces/{workspace}/entries -> 404",
		"GET /api/entries/{ref} -> 404",
		"GET (no route) -> 404",
		"OTHER (no route) -> 404",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("request log lacks %q:\n%s", want, text)
		}
	}
}

// The project is a field of its own, so a metadata filter on the key
// project would match nothing. It is refused rather than left empty.
func TestMetadataFiltersOnProjectAreRefused(t *testing.T) {
	s := startServer(t, testConfig(t))
	for _, query := range []string{
		"meta=project=pond", "no-meta=Project=pond", "key=project",
		"no-key=PROJECT",
	} {
		status, body := s.do(t, "GET",
			"/api/workspaces/default/entries?"+query, "", "")
		if status != http.StatusBadRequest ||
			!strings.Contains(body, "filter on the project field") {
			t.Errorf("%s: %d %s", query, status, body)
		}
	}
	s.must(t, "GET", "/api/workspaces/default/entries?meta=jira=pond-1",
		"", "", http.StatusOK)
}
