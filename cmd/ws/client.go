package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/airspacesh/workstream/internal/api"
	"github.com/airspacesh/workstream/internal/version"
)

const defaultTimeout = 5 * time.Second

// requestTimeout is the timeout <PREFIX>_TIMEOUT sets, or the default.
func requestTimeout() (time.Duration, error) {
	value := os.Getenv(timeoutEnvironment())
	if value == "" {
		return defaultTimeout, nil
	}
	timeout, err := time.ParseDuration(value)
	if err != nil || timeout <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration, like 10s, "+
			"not %q", timeoutEnvironment(), value)
	}
	return timeout, nil
}

// connectionError is a request that got no answer from its server.
// Unless it was never sent, the server may have acted on it.
type connectionError struct {
	server  string
	timeout time.Duration
	err     error
	unsent  bool
}

func (e *connectionError) Error() string {
	var netErr net.Error
	if errors.Is(e.err, context.DeadlineExceeded) ||
		(errors.As(e.err, &netErr) && netErr.Timeout()) {
		return fmt.Sprintf("request to %s timed out after %s. Is the "+
			"server up? %s sets a longer timeout", e.server, e.timeout,
			timeoutEnvironment())
	}
	return fmt.Sprintf("request to %s failed: %v", e.server, e.err)
}

func (e *connectionError) Unwrap() error {
	return e.err
}

// unusableAnswer is a response the client cannot trust or read, so the
// server may have acted on the request whatever the response says.
type unusableAnswer struct {
	server string
	err    error
}

func (e *unusableAnswer) Error() string {
	return fmt.Sprintf("%s gave an unusable answer: %v", e.server, e.err)
}

func (e *unusableAnswer) Unwrap() error {
	return e.err
}

// serverError is a server's well-formed refusal of a request.
type serverError struct {
	server  string
	status  int
	code    api.ErrorCode
	message string
}

func (e *serverError) Error() string {
	return e.server + ": " + e.message
}

// abandonedWrite is a write that sendRetrying stopped repeating after
// a send that may have taken effect.
type abandonedWrite struct {
	sends   int
	elapsed time.Duration
	err     error
}

func (e *abandonedWrite) Error() string {
	return fmt.Sprintf("gave up after %d tries in %s: %v", e.sends,
		e.elapsed.Round(time.Second), e.err)
}

func (e *abandonedWrite) Unwrap() error {
	return e.err
}

// mayHaveApplied reports whether a write that failed with err may
// still have taken effect, so it must not be repeated blindly. Only a
// failure before the request left the client, or a well-formed refusal
// short of a server failure, is certain.
func mayHaveApplied(err error) bool {
	var abandoned *abandonedWrite
	if errors.As(err, &abandoned) {
		return true
	}
	var connection *connectionError
	if errors.As(err, &connection) {
		return !connection.unsent
	}
	var unusable *unusableAnswer
	if errors.As(err, &unusable) {
		return true
	}
	var refused *serverError
	return errors.As(err, &refused) &&
		refused.status >= http.StatusInternalServerError
}

// writeRetryBudget is how long sendRetrying keeps starting sends of a
// write, and maxWriteRetryDelay the longest it waits between them.
const (
	writeRetryBudget   = 60 * time.Second
	maxWriteRetryDelay = 8 * time.Second
)

// sendRetrying sends a write that carries a request id, which the
// server applies at most once. While a send fails for want of a
// connection, it sends again after a wait that doubles from a second,
// until the next send would start after writeRetryBudget. Any other
// failure ends it at once, and so does a first send that never left
// the client. A refusal is certain even after sends that may have
// taken effect, since the server answers a repeat of a request it
// applied with what that request made.
func sendRetrying(send func() error, clk clock) error {
	start := clk.Now()
	delay := time.Second
	for sends := 1; ; sends++ {
		err := send()
		if err == nil || !retryable(err) {
			return err
		}
		if sends == 1 && !mayHaveApplied(err) {
			return err
		}

		elapsed := clk.Now().Sub(start)
		if elapsed+delay > writeRetryBudget {
			return &abandonedWrite{sends: sends, elapsed: elapsed, err: err}
		}
		clk.Sleep(delay)
		delay = min(2*delay, maxWriteRetryDelay)
	}
}

// target is where requests about one workspace go: its server, with
// the secret configured for that workspace there, if any.
type target struct {
	workspace string
	server    string
	secret    string
	// configured says the config names this workspace on this server.
	configured bool
	capture    metadataCapture
}

func targetOf(workspace workspaceConfig) target {
	return target{
		workspace:  workspace.name,
		server:     workspace.server,
		secret:     workspace.secret,
		configured: true,
		capture:    workspace.capture,
	}
}

// targetOn is the target for a workspace a server lists. The config's
// settings and secret apply only when it names that workspace on that
// same server, so a secret never reaches another server.
func (c clientConfig) targetOn(workspace, server string) target {
	configured, ok := c.find(workspace)
	if ok && configured.server == server {
		return targetOf(configured)
	}
	return target{workspace: workspace, server: server, capture: captureAll}
}

type client struct {
	config  clientConfig
	timeout time.Duration
	http    *http.Client
	// listings caches each server's workspaces for one command.
	listings map[string][]api.Workspace
}

func newClient(config clientConfig, timeout time.Duration) *client {
	return &client{
		config:  config,
		timeout: timeout,
		http: &http.Client{
			Timeout: timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		listings: map[string][]api.Workspace{},
	}
}

func readResponse(body io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(body, api.MaxResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > api.MaxResponseBytes {
		return nil, fmt.Errorf("the response exceeds %d bytes",
			api.MaxResponseBytes)
	}
	return data, nil
}

func decodeResponse(data []byte, out any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("the response holds more than one JSON value")
	}
	return nil
}

func checkAPIVersion(response *http.Response) error {
	want := strconv.Itoa(version.API)
	values := response.Header.Values(version.APIHeader)
	if len(values) == 1 && values[0] == want {
		return nil
	}
	return fmt.Errorf("it sent %s %q, but this %s needs API %s. Update "+
		"whichever of %s and ws-server is older", version.APIHeader,
		values, programName, want, programName)
}

// refusal reads a server's error response. An auth failure says which
// secret the request carried, since the server cannot know the config.
func (c *client) refusal(t target, response *http.Response) error {
	data, err := readResponse(response.Body)
	var body api.ErrorResponse
	if err == nil {
		err = decodeResponse(data, &body)
	}
	if err == nil && body.Error == "" {
		err = errors.New("the error response gives no error")
	}
	if err != nil {
		return &unusableAnswer{server: t.server, err: fmt.Errorf(
			"%s, with an unreadable error: %w", response.Status, err)}
	}
	refused := &serverError{server: t.server, status: response.StatusCode,
		code: body.Code, message: body.Error}
	if response.StatusCode != http.StatusUnauthorized {
		return refused
	}

	switch {
	case t.secret != "":
		refused.message += fmt.Sprintf(". The request carried the "+
			"auth.secret of workspace %s in %s", t.workspace, c.config.path)
	case t.configured:
		refused.message += fmt.Sprintf(". The request carried no secret, "+
			"since %s sets no auth.secret for workspace %s", c.config.path,
			t.workspace)
	default:
		refused.message += fmt.Sprintf(". The request carried no secret, "+
			"since %s does not configure workspace %s on %s",
			c.config.path, t.workspace, t.server)
	}
	return refused
}

// do sends one request to t's server, with t's secret if it has one,
// and decodes the response into out. It never repeats a request, and
// every failure after the request may have left the client is a
// connectionError, an unusableAnswer, or a serverError.
func (c *client) do(t target, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, t.server+path, reader)
	if err != nil {
		return err
	}
	request.Header.Set(version.APIHeader, strconv.Itoa(version.API))
	if t.secret != "" {
		request.Header.Set("Authorization", "Bearer "+t.secret)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}

	response, err := c.http.Do(request)
	if err != nil {
		var dial *net.OpError
		unsent := errors.As(err, &dial) && dial.Op == "dial"
		return &connectionError{server: t.server, timeout: c.timeout,
			err: err, unsent: unsent}
	}
	defer response.Body.Close()
	if err := checkAPIVersion(response); err != nil {
		return &unusableAnswer{server: t.server, err: err}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return c.refusal(t, response)
	}

	data, err := readResponse(response.Body)
	if err == nil {
		err = decodeResponse(data, out)
	}
	if err != nil {
		return &unusableAnswer{server: t.server,
			err: fmt.Errorf("reading the response: %w", err)}
	}
	return nil
}

// listWorkspaces asks a server which workspaces it hosts. It sends no
// secret, since the server answers anyone, and caches the answer for
// the rest of the command.
func (c *client) listWorkspaces(server string) ([]api.Workspace, error) {
	if listed, ok := c.listings[server]; ok {
		return listed, nil
	}
	var response api.WorkspacesResponse
	err := c.do(target{server: server}, "GET", "/api/workspaces", nil,
		&response)
	if err != nil {
		return nil, err
	}
	c.listings[server] = response.Workspaces
	return response.Workspaces, nil
}

// servers lists the configured servers in config order, each once.
func (c clientConfig) servers() []string {
	var servers []string
	seen := map[string]bool{}
	for _, workspace := range c.workspaces {
		if !seen[workspace.server] {
			seen[workspace.server] = true
			servers = append(servers, workspace.server)
		}
	}
	return servers
}

// resolve finds the workspace a post id belongs to, and so the server
// its requests go to. A prefix the config sets picks its workspace.
// Otherwise each configured server is asked in turn which prefixes it
// hosts, and only a server that answers without the prefix passes the
// question on. Resolving only reads, and sends no secret.
func (c *client) resolve(ref api.Ref) (target, error) {
	for _, workspace := range c.config.workspaces {
		if workspace.prefix == ref.Prefix {
			return targetOf(workspace), nil
		}
	}

	servers := c.config.servers()
	for _, server := range servers {
		listed, err := c.listWorkspaces(server)
		if err != nil {
			return target{}, fmt.Errorf("finding the workspace of %s: %w",
				ref, err)
		}
		for _, workspace := range listed {
			if workspace.Prefix == ref.Prefix {
				return c.config.targetOn(workspace.Name, server), nil
			}
		}
	}
	return target{}, fmt.Errorf("no configured server has a workspace with "+
		"prefix %s, so %s names no post %s can reach (asked %s)",
		ref.Prefix, ref, programName, strings.Join(servers, ", "))
}
