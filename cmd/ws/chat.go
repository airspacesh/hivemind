package main

import (
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/airspacesh/workstream/internal/api"
)

const defaultWaitTimeout = 10 * time.Minute

// retryDelays are the waits before each retry of a chat wait's poll
// after a failed connection. The wait gives up when the retry after
// the last delay fails too.
var retryDelays = []time.Duration{
	1 * time.Second, 2 * time.Second, 4 * time.Second,
	8 * time.Second, 16 * time.Second, 30 * time.Second,
}

// pollInterval is how long a chat wait sleeps between polls: a second
// for the first 10 seconds, then 5 seconds.
func pollInterval(elapsed time.Duration) time.Duration {
	if elapsed < 10*time.Second {
		return time.Second
	}
	return 5 * time.Second
}

// retryable reports whether a poll failed for want of a connection,
// rather than because the server refused it.
func retryable(err error) bool {
	var connection *connectionError
	var refused *serverError
	return errors.As(err, &connection) ||
		(errors.As(err, &refused) && refused.code == api.CodeUnavailable)
}

type clock interface {
	Now() time.Time
	Sleep(time.Duration)
}

type systemClock struct{}

func (systemClock) Now() time.Time        { return time.Now() }
func (systemClock) Sleep(d time.Duration) { time.Sleep(d) }

// waitFor polls until a poll finds what it looks for, or the timeout
// passes, when it returns the last poll's answer. A failed connection
// is retried with backoff, and too many failures in a row end the
// wait.
func waitFor[T any](
	poll func() (T, bool, error), timeout time.Duration, clk clock,
) (T, error) {
	start := clk.Now()
	failures := 0
	var none T
	for {
		answer, found, err := poll()
		elapsed := clk.Now().Sub(start)
		var delay time.Duration
		switch {
		case err == nil && found:
			return answer, nil
		case err == nil:
			failures = 0
			delay = pollInterval(elapsed)
		case !retryable(err):
			return none, err
		case failures == len(retryDelays):
			return none, fmt.Errorf("gave up waiting after %d "+
				"retries failed in a row: %w", failures, err)
		default:
			delay = retryDelays[failures]
			failures++
		}

		remaining := timeout - elapsed
		if remaining <= 0 && err != nil {
			return none, fmt.Errorf("the wait timed out while "+
				"the server could not be reached: %w", err)
		}
		if remaining <= 0 {
			return answer, nil
		}
		clk.Sleep(min(delay, remaining))
	}
}

func (c *client) chatPage(
	t target, entry api.Ref, query url.Values,
) (api.ChatPage, error) {
	var page api.ChatPage
	path := entryPath(entry, "/messages?"+query.Encode())
	err := c.do(t, "GET", path, nil, &page)
	return page, err
}

func cmdChatSend(c *client, args []string) error {
	if len(args) != 2 {
		return usageError("chat send <ref> <message>")
	}
	ref, err := api.ParseRef(args[0])
	if err != nil {
		return err
	}
	t, err := c.resolve(ref)
	if err != nil {
		return err
	}
	origin, err := captureOrigin(t.capture)
	if err != nil {
		return err
	}
	request := api.PostMessageRequest{Body: args[1], Origin: origin}
	if err := request.Validate(); err != nil {
		return err
	}

	var message api.Message
	err = c.do(t, "POST", entryPath(ref, "/messages"), request, &message)
	if err != nil {
		return uncertainWrite(err, "The message",
			programName+" chat recent "+ref.String())
	}
	// A send prints no cursor, since others may have sent messages
	// between the reader's last read and this one.
	fmt.Printf("Sent [%s].\n", message.Ref())
	return nil
}

func cmdChatEdit(c *client, args []string) error {
	if len(args) != 2 {
		return usageError("chat edit <ref>-m<n> <message>")
	}
	ref, err := api.ParseMessageRef(args[0])
	if err != nil {
		return err
	}
	request := api.EditMessageRequest{Body: args[1], User: currentUser()}
	if err := request.Validate(); err != nil {
		return err
	}
	t, err := c.resolve(ref.Entry)
	if err != nil {
		return err
	}

	var message api.Message
	path := entryPath(ref.Entry, fmt.Sprintf("/messages/%d", ref.Number))
	if err := c.do(t, "PATCH", path, request, &message); err != nil {
		return uncertainWrite(explainNotOwner(err, ref.Entry), "The edit",
			programName+" chat recent "+ref.Entry.String())
	}
	fmt.Printf("Edited [%s].\n", message.Ref())
	return nil
}

func cmdChatRecent(c *client, args []string) error {
	p, err := parseArgs(args, flagSpec{
		strs: []string{"--limit", "--workspace"},
	})
	if err != nil {
		return err
	}
	if len(p.pos) > 1 || (len(p.pos) == 1 && p.has("--workspace")) {
		return usageError("chat recent [<ref>] [--limit <n>], or " +
			"chat recent --workspace <name> [--limit <n>]")
	}
	limit := 20
	if p.has("--limit") {
		if limit, err = parsePositive("--limit", p.strs["--limit"]); err != nil {
			return err
		}
	}

	if len(p.pos) == 0 {
		workspace, err := c.config.chooseWorkspace(p.strs["--workspace"])
		if err != nil {
			return err
		}
		t := targetOf(workspace)
		var overview api.ChatOverview
		path := workspacePath(t, fmt.Sprintf("/messages?limit=%d", limit))
		if err := c.do(t, "GET", path, nil, &overview); err != nil {
			return err
		}
		printOverview(overview, t.workspace)
		return nil
	}

	ref, err := api.ParseRef(p.pos[0])
	if err != nil {
		return err
	}
	t, err := c.resolve(ref)
	if err != nil {
		return err
	}
	page, err := c.chatPage(t, ref, url.Values{"latest": {fmt.Sprint(limit)}})
	if err != nil {
		return err
	}
	printChatPage(ref, page, fmt.Sprintf("%s has no messages yet.", ref))
	return nil
}

// parseCursor reads the --since a chat read or wait needs.
func parseCursor(p *parsedArgs) (int64, error) {
	if !p.has("--since") {
		return 0, fmt.Errorf("--since <n> is required: the number of the "+
			"last message read, or 0 for the start. '%s chat recent <ref>' "+
			"shows the newest messages and the cursor after them",
			programName)
	}
	value := p.strs["--since"]
	var since int64
	if _, err := fmt.Sscan(value, &since); err != nil || since < 0 ||
		fmt.Sprint(since) != value {
		return 0, fmt.Errorf("--since must be a message number, not %q",
			value)
	}
	return since, nil
}

// parseTimeout reads the --timeout of a wait, or fallback when it is
// absent.
func parseTimeout(
	p *parsedArgs, fallback time.Duration,
) (time.Duration, error) {
	if !p.has("--timeout") {
		return fallback, nil
	}
	timeout, err := time.ParseDuration(p.strs["--timeout"])
	if err != nil || timeout <= 0 {
		return 0, fmt.Errorf("--timeout must be a positive duration, "+
			"like 1m, not %q", p.strs["--timeout"])
	}
	return timeout, nil
}

func cmdChatRead(c *client, args []string) error {
	p, err := parseArgs(args, flagSpec{strs: []string{"--since", "--limit"}})
	if err != nil {
		return err
	}
	if len(p.pos) != 1 {
		return usageError("chat read <ref> --since <n> [--limit <n>]")
	}
	ref, err := api.ParseRef(p.pos[0])
	if err != nil {
		return err
	}
	since, err := parseCursor(p)
	if err != nil {
		return err
	}
	query := url.Values{"since": {fmt.Sprint(since)}}
	if p.has("--limit") {
		limit, err := parsePositive("--limit", p.strs["--limit"])
		if err != nil {
			return err
		}
		query.Set("limit", fmt.Sprint(limit))
	}

	t, err := c.resolve(ref)
	if err != nil {
		return err
	}
	page, err := c.chatPage(t, ref, query)
	if err != nil {
		return err
	}
	printChatPage(ref, page, fmt.Sprintf("No messages after %d.", since))
	return nil
}

func cmdChatWait(c *client, args []string) error {
	p, err := parseArgs(args, flagSpec{strs: []string{"--since", "--timeout"}})
	if err != nil {
		return err
	}
	if len(p.pos) != 1 {
		return usageError("chat wait <ref> --since <n> [--timeout <d>]")
	}
	ref, err := api.ParseRef(p.pos[0])
	if err != nil {
		return err
	}
	since, err := parseCursor(p)
	if err != nil {
		return err
	}
	timeout, err := parseTimeout(p, defaultWaitTimeout)
	if err != nil {
		return err
	}

	t, err := c.resolve(ref)
	if err != nil {
		return err
	}
	query := url.Values{"since": {fmt.Sprint(since)}}
	poll := func() (api.ChatPage, bool, error) {
		page, err := c.chatPage(t, ref, query)
		return page, len(page.Messages) > 0, err
	}
	page, err := waitFor(poll, timeout, systemClock{})
	if err != nil {
		return err
	}
	printChatPage(ref, page, fmt.Sprintf("No messages after %d within %s.",
		since, timeout))
	return nil
}

func cmdChat(c *client, args []string) error {
	if len(args) == 0 {
		return usageError("chat send|recent|read|wait|edit ...")
	}
	commands := map[string]func(*client, []string) error{
		"send":   cmdChatSend,
		"recent": cmdChatRecent,
		"read":   cmdChatRead,
		"wait":   cmdChatWait,
		"edit":   cmdChatEdit,
	}
	command, ok := commands[args[0]]
	if !ok {
		return fmt.Errorf("unknown chat command %q. Run '%s help'", args[0],
			programName)
	}
	return command(c, args[1:])
}
