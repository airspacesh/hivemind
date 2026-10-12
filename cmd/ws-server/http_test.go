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
			"general": {Prefix: "e"},
			"personal": {Prefix: "p",
				Auth: &authConfig{Secret: personalSecret}},
			"team": {Prefix: "t", Auth: &authConfig{Secret: teamSecret}},
		},
	}
}

type testServer struct {
	url string
	// user is the user requests read as, when it is not empty.
	user string
}

// as is the server read as user.
func (s testServer) as(user string) testServer {
	s.user = user
	return s
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
	if s.user != "" {
		request.Header.Set(api.UserHeader, s.user)
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

func postBody(subject string, links ...string) string {
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
	s.must(t, "POST", "/api/workspaces/personal/posts",
		postBody("private"), personalSecret, http.StatusCreated)
	s.must(t, "POST", "/api/workspaces/team/posts",
		postBody("shared"), teamSecret, http.StatusCreated)

	for _, test := range []struct {
		secret string
		path   string
		status int
	}{
		{"", "/api/workspaces/general/posts", http.StatusOK},
		{"", "/api/workspaces/personal/posts", http.StatusUnauthorized},
		{"", "/api/posts/p1", http.StatusUnauthorized},
		{personalSecret, "/api/workspaces/general/posts", http.StatusOK},
		{personalSecret, "/api/workspaces/personal/posts", http.StatusOK},
		{personalSecret, "/api/posts/p1", http.StatusOK},
		{personalSecret, "/api/workspaces/team/posts",
			http.StatusUnauthorized},
		{personalSecret, "/api/posts/t1", http.StatusUnauthorized},
		{personalSecret, "/api/workspaces/team/messages",
			http.StatusUnauthorized},
		{"", "/api/workspaces/personal/changes?after=@0",
			http.StatusUnauthorized},
		{teamSecret, "/api/workspaces/personal/changes",
			http.StatusUnauthorized},
		{personalSecret, "/api/workspaces/personal/changes?after=@0",
			http.StatusOK},
		{teamSecret, "/api/posts/t1", http.StatusOK},
		{teamSecret, "/api/posts/p1", http.StatusUnauthorized},
		{"wrong", "/api/workspaces/general/posts",
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

	s.must(t, "POST", "/api/workspaces/personal/posts",
		postBody("written with the wrong secret"), teamSecret,
		http.StatusUnauthorized)
	s.must(t, "POST", "/api/posts/p1/messages",
		`{"body":"hi","origin":{}}`, teamSecret, http.StatusUnauthorized)
	s.must(t, "PATCH", "/api/posts/p1", `{"subject":"taken"}`,
		teamSecret, http.StatusUnauthorized)

	request, err := http.NewRequest("GET",
		s.url+"/api/workspaces/general/posts", nil)
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
	want := `{"workspaces":[{"name":"general","prefix":"e"},` +
		`{"name":"personal","prefix":"p"},{"name":"team","prefix":"t"}]}`
	if strings.TrimSpace(workspaces) != want {
		t.Fatalf("workspaces = %s, want %s", workspaces, want)
	}
}

func TestMissingThingsHaveDistinctCodes(t *testing.T) {
	s := startServer(t, testConfig(t))
	s.must(t, "POST", "/api/workspaces/general/posts", postBody("one"),
		"", http.StatusCreated)
	for _, test := range []struct {
		method string
		path   string
		body   string
		status int
		code   api.ErrorCode
	}{
		{"GET", "/api/posts/x1", "", 404, api.CodeUnknownPrefix},
		{"GET", "/api/posts/e9", "", 404, api.CodePostNotFound},
		{"GET", "/api/posts/e1/messages?after=0", "", 200, ""},
		{"GET", "/api/posts/x1/messages", "", 404, api.CodeUnknownPrefix},
		{"GET", "/api/posts/e9/tree", "", 404, api.CodePostNotFound},
		{"PATCH", "/api/posts/e1/messages/3",
			`{"body":"x","user":"duck"}`, 404,
			api.CodeMessageNotFound},
		{"DELETE", "/api/posts/e1/messages/3", `{"user":"duck"}`, 404,
			api.CodeMessageNotFound},
		{"DELETE", "/api/posts/e9", `{"user":"duck"}`, 404,
			api.CodePostNotFound},
		{"GET", "/api/workspaces/nowhere/posts", "", 404,
			api.CodeUnknownWorkspace},
		{"GET", "/api/workspaces/general/posts?under=x1", "", 404,
			api.CodeUnknownPrefix},
		{"GET", "/api/workspaces/general/posts?under=e9", "", 404,
			api.CodePostNotFound},
		{"POST", "/api/workspaces/general/posts",
			postBody("probe", `{"name":"see","ref":"x1"}`), 404,
			api.CodeUnknownPrefix},
		{"POST", "/api/workspaces/general/posts",
			postBody("probe", `{"name":"see","ref":"e9"}`), 400,
			api.CodeLinkTargetNotFound},
		{"GET", "/api/posts/E1", "", 400, api.CodeBadRequest},
		{"GET", "/api/posts/e1/messages?after=1&latest=2", "", 400,
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

// TestUnreadablePostsShowNoTextOverHTTP links open posts every way a
// link may cross workspaces to protected ones holding SECRET, then
// reads every open route without a credential.
func TestUnreadablePostsShowNoTextOverHTTP(t *testing.T) {
	s := startServer(t, testConfig(t))
	addOpen := func(subject string, links ...string) {
		s.must(t, "POST", "/api/workspaces/general/posts",
			postBody(subject, links...), personalSecret, http.StatusCreated)
	}
	addSecret := func(links ...string) {
		body := `{"subject":"SECRET","body":"SECRET",` +
			`"project":"SECRET","origin":{"user":"SECRET"}`
		if len(links) > 0 {
			body += `,"links":[` + strings.Join(links, ",") + `]`
		}
		s.must(t, "POST", "/api/workspaces/personal/posts", body+"}",
			personalSecret, http.StatusCreated)
	}
	addOpen("open root")                                  // e1
	addSecret(`{"name":"see","ref":"e1"}`)                // p1
	addOpen("open see", `{"name":"see","ref":"p1"}`)      // e2
	addOpen("open child", `{"name":"parent","ref":"e1"}`) // e3
	addSecret(`{"name":"supersedes","ref":"e3"}`)         // p2
	for _, post := range []string{"p1", "p2"} {
		s.must(t, "POST", "/api/posts/"+post+"/messages",
			`{"body":"SECRET","origin":{"user":"SECRET"}}`, personalSecret,
			http.StatusCreated)
	}
	s.must(t, "POST", "/api/posts/e1/messages",
		`{"body":"open","origin":{}}`, "", http.StatusCreated)

	var paths []string
	for _, post := range []string{"e1", "e2", "e3"} {
		paths = append(paths, "/api/posts/"+post,
			"/api/posts/"+post+"/tree?include-superseded=true",
			"/api/posts/"+post+"/messages",
			"/api/workspaces/general/posts?include-superseded=true&"+
				"include-chat=true&under="+post)
	}
	paths = append(paths,
		"/api/workspaces/general/posts?include-superseded=true",
		"/api/workspaces/general/posts?include-superseded=true&"+
			"include-chat=true",
		"/api/workspaces/general/projects",
		"/api/workspaces/general/messages")
	for _, path := range paths {
		text := s.must(t, "GET", path, "", "", http.StatusOK)
		if strings.Contains(text, "SECRET") {
			t.Fatalf("GET %s shows unreadable text: %s", path, text)
		}
	}
	if text := s.must(t, "GET", "/api/posts/e3", "", "",
		http.StatusOK); !strings.Contains(text, `"superseded_by":["p2"]`) {
		t.Fatalf("e3 does not show what supersedes it as a bare ref: %s",
			text)
	}
	if text := s.must(t, "GET", "/api/posts/e2", "", "",
		http.StatusOK); !strings.Contains(text,
		`"links":[{"name":"see","ref":"p1"}]`) {
		t.Fatalf("e2 does not show its link as a bare ref: %s", text)
	}

	_, crossParent := s.do(t, "POST", "/api/workspaces/general/posts",
		postBody("child", `{"name":"parent","ref":"p1"}`), personalSecret)
	if !strings.Contains(crossParent, "a parent must be in the same "+
		"workspace") {
		t.Fatalf("a parent in another workspace gave %s", crossParent)
	}

	_, unreadable := s.do(t, "POST", "/api/workspaces/general/posts",
		postBody("probe", `{"name":"see","ref":"p1"}`), "")
	_, missing := s.do(t, "POST", "/api/workspaces/general/posts",
		postBody("probe", `{"name":"see","ref":"p9"}`), "")
	if strings.Replace(missing, "p9", "p1", 1) != unreadable {
		t.Fatalf("linking to an unreadable post gave %s, but to a "+
			"missing one %s", unreadable, missing)
	}
}

func TestRefusedEditsAndDeletesOverHTTP(t *testing.T) {
	s := startServer(t, testConfig(t))
	s.must(t, "POST", "/api/workspaces/general/posts", postBody("mine"),
		"", http.StatusCreated)
	s.must(t, "POST", "/api/posts/e1/messages",
		`{"body":"hi","origin":{"user":"duck"}}`, "", http.StatusCreated)

	for _, test := range []struct {
		method string
		path   string
		body   string
		status int
		code   api.ErrorCode
	}{
		{"PATCH", "/api/posts/e1", `{"subject":"taken","user":"heron"}`,
			403, api.CodeNotOwner},
		{"PATCH", "/api/posts/e1/messages/1",
			`{"body":"taken","user":"heron"}`, 403, api.CodeNotOwner},
		{"DELETE", "/api/posts/e1", `{"user":"heron"}`, 403,
			api.CodeNotOwner},
		{"DELETE", "/api/posts/e1/messages/1", `{"user":"heron"}`, 403,
			api.CodeNotOwner},
		{"PATCH", "/api/posts/e1", `{"subject":"taken"}`, 400,
			api.CodeBadRequest},
		{"PATCH", "/api/posts/e1/messages/1", `{"body":"taken"}`, 400,
			api.CodeBadRequest},
		{"DELETE", "/api/posts/e1", `{}`, 400, api.CodeBadRequest},
		{"DELETE", "/api/posts/e1/messages/1", `{}`, 400,
			api.CodeBadRequest},
		{"PATCH", "/api/posts/e1", `{"subject":"kept","user":"duck"}`,
			200, ""},
		{"PATCH", "/api/posts/e1/messages/1",
			`{"body":"kept","user":"duck"}`, 200, ""},
		{"DELETE", "/api/posts/e1", `{"user":"duck"}`, 200, ""},
		{"DELETE", "/api/posts/e1/messages/1", `{"user":"duck"}`, 200,
			""},
		{"PATCH", "/api/posts/e1", `{"subject":"back","user":"duck"}`,
			409, api.CodeDeleted},
		{"PATCH", "/api/posts/e1",
			`{"remove_link_names":["see"],"user":"duck"}`, 409,
			api.CodeDeleted},
		{"PATCH", "/api/posts/e1/messages/1",
			`{"body":"back","user":"duck"}`, 409, api.CodeDeleted},
		{"PATCH", "/api/posts/e1", `{"subject":"back","user":"heron"}`,
			403, api.CodeNotOwner},
		{"DELETE", "/api/posts/e1", `{"user":"duck"}`, 200, ""},
		{"DELETE", "/api/posts/e1/messages/1", `{"user":"duck"}`, 200,
			""},
	} {
		status, text := s.do(t, test.method, test.path, test.body, "")
		if status != test.status {
			t.Fatalf("%s %s %s: %d %s, want %d", test.method, test.path,
				test.body, status, text, test.status)
		}
		if test.code != "" && errorCode(t, text) != test.code {
			t.Fatalf("%s %s %s: %s, want code %s", test.method, test.path,
				test.body, text, test.code)
		}
	}
}

// No request names an origin or creation time but the one that adds a
// post or posts a message, so they stay as that request recorded them.
func TestNoRequestChangesAnOriginOverHTTP(t *testing.T) {
	s := startServer(t, testConfig(t))
	origin := `{"user":"duck","host":"nest","dir":"/pond",` +
		`"claude_session":"s1"}`
	var post api.Post
	if err := json.Unmarshal([]byte(s.must(t, "POST",
		"/api/workspaces/general/posts",
		`{"subject":"mine","project":"p","origin":`+origin+`}`, "",
		http.StatusCreated)), &post); err != nil {
		t.Fatal(err)
	}
	var message api.Message
	if err := json.Unmarshal([]byte(s.must(t, "POST",
		"/api/posts/e1/messages", `{"body":"hi","origin":`+origin+`}`, "",
		http.StatusCreated)), &message); err != nil {
		t.Fatal(err)
	}

	claims := []string{
		`"origin":{"user":"heron"}`, `"origin_user":"heron"`,
		`"origin_host":"heron"`, `"created":"2000-01-01T00:00:00Z"`,
	}
	for _, claim := range claims {
		for _, test := range []struct {
			method string
			path   string
			body   string
		}{
			{"PATCH", "/api/posts/e1", `"subject":"x","user":"duck"`},
			{"PATCH", "/api/posts/e1/messages/1", `"body":"x","user":"duck"`},
			{"DELETE", "/api/posts/e1", `"user":"duck"`},
			{"DELETE", "/api/posts/e1/messages/1", `"user":"duck"`},
		} {
			body := "{" + test.body + "," + claim + "}"
			status, text := s.do(t, test.method, test.path, body, "")
			if status != http.StatusBadRequest {
				t.Fatalf("%s %s %s: %d %s, want 400", test.method,
					test.path, body, status, text)
			}
		}
	}
	s.must(t, "PATCH", "/api/posts/e1", `{"subject":"x","user":"duck"}`,
		"", http.StatusOK)
	s.must(t, "DELETE", "/api/posts/e1", `{"user":"duck"}`, "",
		http.StatusOK)
	s.must(t, "DELETE", "/api/posts/e1/messages/1", `{"user":"duck"}`, "",
		http.StatusOK)

	var detail api.PostDetail
	if err := json.Unmarshal([]byte(s.must(t, "GET", "/api/posts/e1", "",
		"", http.StatusOK)), &detail); err != nil {
		t.Fatal(err)
	}
	if detail.Origin != post.Origin || !detail.Created.Equal(post.Created) {
		t.Fatalf("post origin %+v created %v, want %+v created %v",
			detail.Origin, detail.Created, post.Origin, post.Created)
	}
	var page api.ChatPage
	if err := json.Unmarshal([]byte(s.must(t, "GET",
		"/api/posts/e1/messages", "", "", http.StatusOK)),
		&page); err != nil {
		t.Fatal(err)
	}
	if got := page.Messages[0]; got.Origin != message.Origin ||
		!got.Created.Equal(message.Created) {
		t.Fatalf("message origin %+v created %v, want %+v created %v",
			got.Origin, got.Created, message.Origin, message.Created)
	}
}

func TestChatReadsOverHTTP(t *testing.T) {
	s := startServer(t, testConfig(t))
	s.must(t, "POST", "/api/workspaces/general/posts", postBody("chat"),
		"", http.StatusCreated)
	for _, body := range []string{"one", "two", "three"} {
		s.must(t, "POST", "/api/posts/e1/messages",
			`{"body":"`+body+`","origin":{"user":"duck"}}`, "",
			http.StatusCreated)
	}
	s.must(t, "PATCH", "/api/posts/e1/messages/1",
		`{"body":"one!","user":"duck"}`, "", http.StatusOK)

	read := func(query string) string {
		t.Helper()
		var page api.ChatPage
		text := s.must(t, "GET", "/api/posts/e1/messages"+query, "", "",
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
		"?after=0&limit=2": "e1-m1:one! e1-m2:two cursor=2 remaining=1",
		"?after=2":         "e1-m3:three cursor=3 remaining=0",
		"?after=2000-01-01T00:00:00Z&limit=1": "e1-m1:one! cursor=1 " +
			"remaining=2",
		"?latest=2": "e1-m2:two e1-m3:three cursor=3 remaining=0",
	} {
		if got := read(query); got != want {
			t.Fatalf("messages%s = %q, want %q", query, got, want)
		}
	}
}

// An empty session on the wire names the changes written outside a
// session, not every change by the user.
func TestExcludedSessionlessKeyOverHTTP(t *testing.T) {
	s := startServer(t, testConfig(t))
	s.must(t, "POST", "/api/workspaces/general/posts", postBody("chat"),
		"", http.StatusCreated)
	for _, session := range []string{"", "lead", ""} {
		s.must(t, "POST", "/api/posts/e1/messages",
			`{"body":"quack","origin":{"user":"duck","claude_session":"`+
				session+`"}}`, "", http.StatusCreated)
	}

	exclude := "exclude-user=duck&exclude-claude-session="
	var chat api.ChatPage
	text := s.must(t, "GET", "/api/posts/e1/messages?after=0&"+exclude,
		"", "", http.StatusOK)
	if err := json.Unmarshal([]byte(text), &chat); err != nil {
		t.Fatal(err)
	}
	if len(chat.Messages) != 1 || chat.Messages[0].Number != 2 ||
		chat.Cursor != 3 {
		t.Fatalf("chat excluding duck with no session = %s", text)
	}

	var changes api.ChangesPage
	text = s.must(t, "GET", "/api/workspaces/general/changes?after=@0&"+
		exclude, "", "", http.StatusOK)
	if err := json.Unmarshal([]byte(text), &changes); err != nil {
		t.Fatal(err)
	}
	if len(changes.Changes) != 1 || changes.Changes[0].Message == nil ||
		changes.Changes[0].Message.Number != 2 || changes.Cursor != 4 {
		t.Fatalf("changes excluding duck with no session = %s", text)
	}
}

// A post or message id as after reads the changes after it was added or
// sent, however it has changed since, and only in its own workspace.
func TestChangesAfterAnIDOverHTTP(t *testing.T) {
	s := startServer(t, testConfig(t))
	s.must(t, "POST", "/api/workspaces/general/posts", postBody("seen"),
		"", http.StatusCreated)
	s.must(t, "POST", "/api/posts/e1/messages",
		`{"body":"hi","origin":{"user":"duck"}}`, "", http.StatusCreated)
	s.must(t, "POST", "/api/workspaces/general/posts", postBody("later"),
		"", http.StatusCreated)
	s.must(t, "PATCH", "/api/posts/e1", `{"subject":"seen, edited",`+
		`"user":"duck"}`, "", http.StatusOK)

	changesAfter := func(after string) string {
		t.Helper()
		text := s.must(t, "GET", "/api/workspaces/general/changes?after="+
			after, "", "", http.StatusOK)
		var page api.ChangesPage
		if err := json.Unmarshal([]byte(text), &page); err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, change := range page.Changes {
			if change.Message != nil {
				ids = append(ids, change.Message.Ref().String())
			} else {
				ids = append(ids, change.Post.Ref.String())
			}
		}
		return strings.Join(ids, " ")
	}
	for after, want := range map[string]string{
		"e1":    "e1-m1 e2 e1",
		"e1-m1": "e2 e1",
		"e2":    "e1",
	} {
		if got := changesAfter(after); got != want {
			t.Errorf("changes after %s = %q, want %q", after, got, want)
		}
	}

	for _, test := range []struct {
		after  string
		status int
		code   api.ErrorCode
	}{
		{"p1", 400, api.CodeBadRequest},
		{"e1-m2", 404, api.CodeMessageNotFound},
		{"e3", 404, api.CodePostNotFound},
		{"e1-m0", 400, api.CodeBadRequest},
	} {
		text := s.must(t, "GET", "/api/workspaces/general/changes?after="+
			test.after, "", "", test.status)
		if code := errorCode(t, text); code != test.code {
			t.Errorf("after %s gave %s, want %s", test.after, code,
				test.code)
		}
	}
}

// Every read of a post private to alice, or of what is under it, is
// refused to bob as if it did not exist, and so is any change.
func TestPrivatePostsAreNotFoundForOthersOverHTTP(t *testing.T) {
	s := startServer(t, testConfig(t))
	alice, bob := s.as("alice"), s.as("bob")
	alice.must(t, "POST", "/api/workspaces/general/posts",
		`{"subject":"draft","project":"p","private":true,`+
			`"origin":{"user":"alice"}}`, "", http.StatusCreated)
	alice.must(t, "POST", "/api/posts/e1/messages",
		`{"body":"note","origin":{"user":"alice"}}`, "",
		http.StatusCreated)
	text := alice.must(t, "GET", "/api/posts/e1", "", "", http.StatusOK)
	if !strings.Contains(text, `"private_to":"alice"`) {
		t.Fatalf("alice reads her private post as %s", text)
	}

	for _, request := range []struct{ method, path, body string }{
		{"GET", "/api/posts/e1", ""},
		{"GET", "/api/posts/e1/tree", ""},
		{"GET", "/api/posts/e1/messages", ""},
		{"GET", "/api/posts/e1/messages?latest=5", ""},
		{"GET", "/api/posts/e1/messages/1", ""},
		{"GET", "/api/workspaces/general/changes?after=e1", ""},
		{"GET", "/api/workspaces/general/changes?after=e1-m1", ""},
		{"GET", "/api/workspaces/general/posts?under=e1", ""},
		{"POST", "/api/posts/e1/messages",
			`{"body":"hi","origin":{"user":"bob"}}`},
		{"PATCH", "/api/posts/e1", `{"private":false,"user":"bob"}`},
		{"DELETE", "/api/posts/e1", `{"user":"bob"}`},
	} {
		text := bob.must(t, request.method, request.path, request.body,
			"", http.StatusNotFound)
		if code := errorCode(t, text); code != api.CodePostNotFound {
			t.Errorf("bob's %s %s gave %s, want %s", request.method,
				request.path, code, api.CodePostNotFound)
		}
	}

	alice.must(t, "POST", "/api/workspaces/general/posts",
		`{"subject":"topic","project":"p","origin":{"user":"alice"}}`, "",
		http.StatusCreated)
	bob.must(t, "POST", "/api/posts/e2/messages",
		`{"body":"hi","origin":{"user":"bob"}}`, "", http.StatusCreated)
	text = alice.must(t, "PATCH", "/api/posts/e2",
		`{"private":true,"user":"alice"}`, "", http.StatusConflict)
	if code := errorCode(t, text); code != api.CodeConflict {
		t.Errorf("making a post bob chatted on private gave %s, want %s",
			code, api.CodeConflict)
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
		{"POST", "/api/workspaces/general/posts", `{"subject":` +
			`"SUBJECTMARK","body":"BODYMARK",` +
			`"project":"PROJECTMARK","origin":{"user":"duck","dir":"/"}}`,
			personalSecret, http.StatusCreated},
		{"PATCH", "/api/posts/e1", `{"subject":"NEWSUBJECTMARK",` +
			`"body":"NEWBODYMARK","user":"duck"}`, "", http.StatusOK},
		{"POST", "/api/posts/e1/messages",
			`{"body":"MESSAGEMARK","origin":{"user":"duck"}}`, "",
			http.StatusCreated},
		{"PATCH", "/api/posts/e1/messages/1",
			`{"body":"EDITMARK","user":"duck"}`, "", http.StatusOK},
		{"POST", "/api/workspaces/general/posts", `{"FIELDMARK":1}`, "",
			http.StatusBadRequest},
		{"GET", "/api/workspaces/general/posts?subject=SEARCHMARK&limit=5",
			"", "", http.StatusOK},
		{"GET", "/api/workspaces/general/posts?PARAMMARK=1", "", "",
			http.StatusBadRequest},
		{"GET", "/api/workspaces/SPACEMARK/posts", "", "",
			http.StatusNotFound},
		{"GET", "/api/posts/markref1", "", "", http.StatusNotFound},
		{"GET", "/api/posts/IDMARK", "", "", http.StatusBadRequest},
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
		"PATCH /api/posts/{ref} ref=e1 -> 200",
		"PATCH /api/posts/{ref}/messages/{n} ref=e1 n=1 -> 200",
		"GET /api/workspaces/{workspace}/posts workspace=general " +
			"query=limit,subject -> 200",
		"GET /api/workspaces/{workspace}/posts workspace=general " +
			"unknown-query=1 -> 400",
		"GET /api/workspaces/{workspace}/posts -> 404",
		"GET /api/posts/{ref} -> 404",
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
			"/api/workspaces/general/posts?"+query, "", "")
		if status != http.StatusBadRequest ||
			!strings.Contains(body, "filter on the project field") {
			t.Errorf("%s: %d %s", query, status, body)
		}
	}
	s.must(t, "GET", "/api/workspaces/general/posts?meta=jira=pond-1",
		"", "", http.StatusOK)
}

// A request repeated under its request id answers 200 with what the
// first request made, and one that asks for something else is refused.
func TestRepeatedRequestsOverHTTP(t *testing.T) {
	s := startServer(t, testConfig(t))
	for _, test := range []struct {
		path, request, repeat, reuse string
	}{
		{"/api/workspaces/general/posts",
			`{"subject":"once","project":"p","request_id":"r1"}`,
			`{"subject":"once","project":"p","request_id":"r1",` +
				`"origin":{"dir":"/elsewhere"}}`,
			`{"subject":"twice","project":"p","request_id":"r1"}`},
		{"/api/posts/e1/messages",
			`{"body":"once","request_id":"r1"}`,
			`{"body":"once","request_id":"r1",` +
				`"origin":{"dir":"/elsewhere"}}`,
			`{"body":"twice","request_id":"r1"}`},
	} {
		first := s.must(t, "POST", test.path, test.request, "",
			http.StatusCreated)
		if repeat := s.must(t, "POST", test.path, test.repeat, "",
			http.StatusOK); repeat != first {
			t.Fatalf("POST %s repeated gave %s, want %s", test.path,
				repeat, first)
		}
		reused := s.must(t, "POST", test.path, test.reuse, "",
			http.StatusConflict)
		if errorCode(t, reused) != api.CodeConflict {
			t.Fatalf("POST %s with a reused id gave %s", test.path, reused)
		}
	}
}
