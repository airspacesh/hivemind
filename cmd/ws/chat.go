package main

import (
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/airspacesh/workstream/internal/api"
)

const defaultWaitTimeout = 10 * time.Minute

// sendShownLimit is how many of the newest messages a chat send with
// --after shows at most.
const sendShownLimit = 20

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

// pollOutcome says what a poll of a wait found.
type pollOutcome int

const (
	// nothingYet sends the wait to sleep before it polls again.
	nothingYet pollOutcome = iota
	// moreToCheck polls again at once, since the server stopped
	// checking short of the latest change.
	moreToCheck
	// found ends the wait.
	found
)

// waitFor polls until a poll finds what it looks for, or the timeout
// passes, when it returns the last poll's answer. A failed connection
// is retried with backoff, and too many failures in a row end the
// wait.
func waitFor[T any](
	poll func() (T, pollOutcome, error), timeout time.Duration, clk clock,
) (T, error) {
	start := clk.Now()
	failures := 0
	var none T
	for {
		answer, outcome, err := poll()
		elapsed := clk.Now().Sub(start)
		var delay time.Duration
		switch {
		case err == nil && outcome == found:
			return answer, nil
		case err == nil && outcome == moreToCheck:
			failures = 0
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
		if delay > 0 {
			clk.Sleep(min(delay, remaining))
		}
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

// parseChatAfter reads the --after of a command on an entry's chat,
// which is a message of that chat like e40-m3, e40-m0 for its start, or
// a time as a listing's --after reads it.
func parseChatAfter(
	entry api.Ref, value string, now time.Time,
) (api.ChatBound, error) {
	if cursor, err := api.ParseMessageCursor(value); err == nil {
		if cursor.Entry != entry {
			return api.ChatBound{}, fmt.Errorf("--after %s is a message "+
				"of %s, but this is the chat of %s. Pass a message of %s, "+
				"like %s-m3", value, cursor.Entry, entry, entry, entry)
		}
		return api.ChatBound{Number: cursor.Number}, nil
	}
	bound, err := parseBoundFlag("--after", value, now)
	if err == nil && bound.Revision == 0 {
		return api.ChatBound{Time: bound.Time}, nil
	}
	return api.ChatBound{}, fmt.Errorf("--after must be a message of the "+
		"chat like %s-m3, %s-m0 for its start, a span back from now like "+
		"2d, 6h or 90m, a date like 2026-10-09, or a date and time like "+
		"2026-10-09T14:30, not %q", entry, entry, value)
}

// describeChatAfter says where a chat read started, for a reply that
// found nothing.
func describeChatAfter(entry api.Ref, after api.ChatBound) string {
	if !after.Time.IsZero() {
		return "at or after " + after.Time.Local().Format(dateClockLayout)
	}
	return "after " + api.MessageRef{Entry: entry, Number: after.Number}.
		String()
}

// numberBefore is the number of the message a read of an entry's chat
// from after starts after. That is after's own number, or for a time,
// the number of the last message posted before it.
func (c *client) numberBefore(
	t target, entry api.Ref, after api.ChatBound,
) (int64, error) {
	if after.Time.IsZero() {
		return after.Number, nil
	}
	query := url.Values{"after": {after.String()}, "limit": {"1"}}
	page, err := c.chatPage(t, entry, query)
	if err != nil {
		return 0, err
	}
	if len(page.Messages) == 0 {
		return page.Cursor, nil
	}
	return page.Messages[0].Number - 1, nil
}

func cmdChatSend(c *client, args []string) error {
	p, err := parseArgs(args, flagSpec{
		strs: []string{"--after"}, passUnknown: true,
	})
	if err != nil {
		return err
	}
	if len(p.pos) != 2 {
		usage := usageError("chat send <ref> <message> " +
			"[--after <ref>-m<n>|<time>]")
		for _, arg := range p.pos {
			if strings.HasPrefix(arg, "--") {
				return fmt.Errorf("%s looks like a flag, but chat send "+
					"has only --after\n%w", arg, usage)
			}
		}
		return usage
	}
	ref, err := api.ParseRef(p.pos[0])
	if err != nil {
		return err
	}
	var after api.ChatBound
	if p.has("--after") {
		after, err = parseChatAfter(ref, p.strs["--after"], time.Now())
		if err != nil {
			return err
		}
	}
	t, err := c.resolve(ref)
	if err != nil {
		return err
	}
	origin, err := captureOrigin(t.capture)
	if err != nil {
		return err
	}
	request := api.PostMessageRequest{Body: p.pos[1], Origin: origin}
	if err := request.Validate(); err != nil {
		return err
	}

	var message api.Message
	err = c.do(t, "POST", entryPath(ref, "/messages"), request, &message)
	if err != nil {
		return uncertainWrite(err, "The message",
			programName+" chat recent "+ref.String())
	}
	fmt.Printf("Sent [%s].\n", message.Ref())
	// Without --after a send prints no cursor, since others may have
	// sent messages between the reader's last read and this one.
	if !p.has("--after") {
		return nil
	}

	since, err := c.numberBefore(t, ref, after)
	if err != nil {
		return fmt.Errorf("reading the chat after the send: %w", err)
	}
	latest := url.Values{"latest": {fmt.Sprint(sendShownLimit)}}
	page, err := c.chatPage(t, ref, latest)
	if err != nil {
		return fmt.Errorf("reading the chat after the send: %w", err)
	}
	// Message numbers count from 1 in each entry and none are removed,
	// so the first number shown says how many after since are left out.
	cursor := api.MessageRef{Entry: ref, Number: since}
	earlier := page.Messages[0].Number - 1 - since
	if earlier > 0 {
		fmt.Printf("... %d earlier (%s chat read %s --after %s)\n",
			earlier, programName, ref, cursor)
	}
	page.Messages = slices.DeleteFunc(page.Messages, func(m api.Message) bool {
		return m.Number <= since
	})
	printChatPage(ref, page, fmt.Sprintf("No messages after %s.", cursor),
		"")
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

func cmdChatDelete(c *client, args []string) error {
	if len(args) != 1 {
		return usageError("chat delete <ref>-m<n>")
	}
	ref, err := api.ParseMessageRef(args[0])
	if err != nil {
		return err
	}
	request := api.DeleteRequest{User: currentUser()}
	if err := request.Validate(); err != nil {
		return err
	}
	t, err := c.resolve(ref.Entry)
	if err != nil {
		return err
	}

	var message api.Message
	path := entryPath(ref.Entry, fmt.Sprintf("/messages/%d", ref.Number))
	if err := c.do(t, "DELETE", path, request, &message); err != nil {
		return uncertainWrite(explainNotOwner(err, ref.Entry), "The delete",
			programName+" chat recent "+ref.Entry.String())
	}
	fmt.Printf("Deleted [%s].\n", message.Ref())
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
	printChatPage(ref, page, fmt.Sprintf("%s has no messages yet.", ref), "")
	return nil
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
	p, err := parseArgs(args, flagSpec{strs: []string{"--after", "--limit"}})
	if err != nil {
		return err
	}
	if len(p.pos) != 1 {
		return usageError("chat read <ref> [--after <ref>-m<n>|<time>] " +
			"[--limit <n>]")
	}
	ref, err := api.ParseRef(p.pos[0])
	if err != nil {
		return err
	}
	var after api.ChatBound
	if p.has("--after") {
		after, err = parseChatAfter(ref, p.strs["--after"], time.Now())
		if err != nil {
			return err
		}
	}
	query := url.Values{"after": {after.String()}}
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
	empty := fmt.Sprintf("%s has no messages yet.", ref)
	if p.has("--after") {
		empty = fmt.Sprintf("No messages %s.", describeChatAfter(ref, after))
	}
	printChatPage(ref, page, empty, "")
	return nil
}

func cmdChatWait(c *client, args []string) error {
	p, err := parseArgs(args, flagSpec{
		strs:   []string{"--after", "--timeout"},
		bools:  mineFlags,
		repeat: filterFlags(messageFields...),
	})
	if err != nil {
		return err
	}
	if len(p.pos) != 1 {
		return usageError("chat wait <ref> [--after <ref>-m<n>|<time>] " +
			"[--timeout <d>] " +
			"[--content <pattern>]... [--origin-user <pattern>]... " +
			"[--exclude-mine|--include-mine]")
	}
	ref, err := api.ParseRef(p.pos[0])
	if err != nil {
		return err
	}
	var after api.ChatBound
	if p.has("--after") {
		after, err = parseChatAfter(ref, p.strs["--after"], time.Now())
		if err != nil {
			return err
		}
	}
	timeout, err := parseTimeout(p, defaultWaitTimeout)
	if err != nil {
		return err
	}
	exclude, err := excludesMine(p, true)
	if err != nil {
		return err
	}
	query, err := searchQuery(p)
	if err != nil {
		return err
	}

	t, err := c.resolve(ref)
	if err != nil {
		return err
	}
	if exclude {
		if err := excludeMine(query, t.capture); err != nil {
			return err
		}
	}
	if !p.has("--after") {
		newest := url.Values{"latest": {"1"}}
		page, err := c.chatPage(t, ref, newest)
		if err != nil {
			return err
		}
		after = api.ChatBound{Number: page.Cursor}
	}
	cursor := after
	poll := func() (api.ChatPage, pollOutcome, error) {
		query.Set("after", cursor.String())
		page, err := c.chatPage(t, ref, query)
		if err != nil {
			return page, nothingYet, err
		}
		cursor = api.ChatBound{Number: page.Cursor}
		if len(page.Messages) > 0 {
			return page, found, nil
		}
		return page, nothingYet, nil
	}
	page, err := waitFor(poll, timeout, systemClock{})
	if err != nil {
		return err
	}
	printChatPage(ref, page, fmt.Sprintf("No matching messages %s within "+
		"%s.", describeChatAfter(ref, after), timeout), filterArgs(p))
	return nil
}

func cmdChat(c *client, args []string) error {
	if len(args) == 0 {
		return usageError("chat send|recent|read|wait|edit|delete ...")
	}
	commands := map[string]func(*client, []string) error{
		"send":   cmdChatSend,
		"recent": cmdChatRecent,
		"read":   cmdChatRead,
		"wait":   cmdChatWait,
		"edit":   cmdChatEdit,
		"delete": cmdChatDelete,
	}
	command, ok := commands[args[0]]
	if !ok {
		return fmt.Errorf("unknown chat command %q. Run '%s help'", args[0],
			programName)
	}
	return command(c, args[1:])
}
