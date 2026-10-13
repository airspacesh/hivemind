package main

import (
	"errors"
	"fmt"
	"net/url"
	"os"
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
	t target, post api.Ref, query url.Values,
) (api.ChatPage, error) {
	var page api.ChatPage
	path := postPath(post, "/messages?"+query.Encode())
	err := c.do(t, "GET", path, nil, &page)
	return page, err
}

// chatBound reads the --after or --before of a command on the chat of
// post, as parseBoundFlag does, and checks that a post or message it
// names is on the chat's server. It returns nil when the command was not
// given the flag.
func (c *client) chatBound(
	p *parsedArgs, flag string, t target, post api.Ref,
) (*api.Bound, error) {
	if !p.has(flag) {
		return nil, nil
	}
	bound, err := parseBoundFlag(flag, p.strs[flag], time.Now())
	if err != nil {
		return nil, err
	}
	if bound.Post != post {
		if err := c.checkBoundServer(t, bound); err != nil {
			return nil, err
		}
	}
	return &bound, nil
}

// chatCursor is the bound that a read of a post's chat after the
// message numbered number takes.
func chatCursor(post api.Ref, number int64) api.Bound {
	return api.Bound{Post: post, Message: number}
}

// addChatBounds adds the bounds a read of a chat was given to its query.
func addChatBounds(query url.Values, after, before *api.Bound) {
	if after != nil {
		query.Set("after", after.String())
	}
	if before != nil {
		query.Set("before", before.String())
	}
}

// chatReadArgs repeats the --before, filters and inclusion flags a
// read of a chat was given, each after a space, for the chat read it
// suggests.
func chatReadArgs(p *parsedArgs, before *api.Bound) string {
	text := ""
	if before != nil {
		text = " --before " + boundArg(*before)
	}
	return text + conditionArgs(p) + deletedToggle.arg(p)
}

// emptyChat is what a read of the chat of post from after says when it
// finds no messages.
func emptyChat(p *parsedArgs, post api.Ref, after *api.Bound) string {
	noun := "messages"
	if len(p.lists) > 0 {
		noun = "matching messages"
	}
	if after == nil {
		return fmt.Sprintf("%s has no %s yet.", post, noun)
	}
	return fmt.Sprintf("No %s %s.", noun, describeChatAfter(post, *after))
}

// describeChatAfter says where a read of the chat of post started, for a
// reply that found nothing.
func describeChatAfter(post api.Ref, after api.Bound) string {
	switch {
	case after.Post == post:
		return "after " + api.MessageRef{Post: post, Number: after.Message}.
			String()
	case after.Post.ID == 0 && !after.Time.IsZero():
		return "at or after " + after.Time.Local().Format(lineTimeLayout)
	default:
		return "posted after " + after.String()
	}
}

// numberBefore is the number of the message a read of a post's chat
// from after starts after. That is after's own number for a message of
// that chat, or else the number of the last message posted before after,
// deleted or not.
func (c *client) numberBefore(
	t target, post api.Ref, after api.Bound,
) (int64, error) {
	if after.Post == post {
		return after.Message, nil
	}
	query := url.Values{"after": {after.String()}, "limit": {"1"},
		"include-deleted": {"true"}}
	page, err := c.chatPage(t, post, query)
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
		strs:    []string{"--after", "--request-id"},
		toggles: []toggle{deletedToggle}, passUnknown: true,
	})
	if err != nil {
		return err
	}
	if len(p.pos) != 2 {
		usage := usageError("chat send <ref> <message> " +
			"[--after <bound>] [--include-deleted|--exclude-deleted]")
		for _, arg := range p.pos {
			if strings.HasPrefix(arg, "--") {
				return fmt.Errorf("%s looks like a flag, but chat send "+
					"has only --after, --include-deleted, "+
					"--exclude-deleted and --request-id\n%w", arg, usage)
			}
		}
		return usage
	}
	id, err := requestID(p)
	if err != nil {
		return err
	}
	ref, err := api.ParseRef(p.pos[0])
	if err != nil {
		return err
	}
	t, err := c.resolve(ref)
	if err != nil {
		return err
	}
	after, err := c.chatBound(p, "--after", t, ref)
	if err != nil {
		return err
	}
	origin, err := captureOrigin(t.capture)
	if err != nil {
		return err
	}
	request := api.PostMessageRequest{Body: p.pos[1], Origin: origin,
		RequestID: id}
	if err := request.Validate(); err != nil {
		return err
	}

	var message api.Message
	err = sendRetrying(func() error {
		return c.do(t, "POST", postPath(ref, "/messages"), request,
			&message)
	}, systemClock{})
	if err != nil {
		return uncertainCreate(err, "The message",
			repeatCommand("chat send", args, p, id))
	}
	fmt.Printf("Sent [%s].\n", message.Ref())
	// Without --after a send prints no cursor, since others may have
	// sent messages between the reader's last read and this one.
	if after == nil {
		return nil
	}

	start, err := c.numberBefore(t, ref, *after)
	if err != nil {
		return fmt.Errorf("reading the chat after the send: %w", err)
	}
	from := chatCursor(ref, start)
	latest := url.Values{"latest": {fmt.Sprint(sendShownLimit)},
		"after": {from.String()}}
	deletedToggle.addTo(p, latest)
	page, err := c.chatPage(t, ref, latest)
	if err != nil {
		return fmt.Errorf("reading the chat after the send: %w", err)
	}
	printEarlier("", ref, from, page.Earlier, deletedToggle.arg(p))
	cursor := api.MessageRef{Post: ref, Number: start}
	printChatPage(ref, page, from,
		fmt.Sprintf("No messages after %s.", cursor), deletedToggle.arg(p))
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
	t, err := c.resolve(ref.Post)
	if err != nil {
		return err
	}

	var message api.Message
	path := postPath(ref.Post, fmt.Sprintf("/messages/%d", ref.Number))
	if err := c.do(t, "PATCH", path, request, &message); err != nil {
		return uncertainWrite(explainNotOwner(err, ref.Post), "The edit",
			programName+" chat recent "+ref.Post.String())
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
	t, err := c.resolve(ref.Post)
	if err != nil {
		return err
	}

	var message api.Message
	path := postPath(ref.Post, fmt.Sprintf("/messages/%d", ref.Number))
	if err := c.do(t, "DELETE", path, request, &message); err != nil {
		return uncertainWrite(explainNotOwner(err, ref.Post), "The delete",
			programName+" chat recent "+ref.Post.String())
	}
	fmt.Printf("Deleted [%s].\n", message.Ref())
	return nil
}

func cmdChatRecent(c *client, args []string) error {
	p, err := parseArgs(args, flagSpec{
		strs: []string{"--limit", "--workspace", "--after",
			"--before"},
		toggles: []toggle{deletedToggle},
		repeat:  filterFlags(api.MessageFields...),
	})
	if err != nil {
		return err
	}
	if len(p.pos) > 1 || (len(p.pos) == 1 && p.has("--workspace")) {
		return usageError("chat recent [<ref>|--workspace <name>] " +
			"[--after <bound>] [--before <bound>] [--limit <n>] " +
			"[--include-deleted|--exclude-deleted] " +
			"[--content <pattern>]... [--origin-user <pattern>]...")
	}
	limit := 20
	if p.has("--limit") {
		if limit, err = parsePositive("--limit", p.strs["--limit"]); err != nil {
			return err
		}
	}
	query, err := searchQuery(p)
	if err != nil {
		return err
	}

	if len(p.pos) == 0 {
		bounds := map[string]*api.Bound{}
		var named []api.Bound
		for _, flag := range []string{"--after", "--before"} {
			if !p.has(flag) {
				continue
			}
			bound, err := parseBoundFlag(flag, p.strs[flag], time.Now())
			if err != nil {
				return err
			}
			bounds[flag] = &bound
			named = append(named, bound)
		}
		t, err := c.boundedTarget(p, named...)
		if err != nil {
			return err
		}
		query.Set("limit", fmt.Sprint(limit))
		addChatBounds(query, bounds["--after"], bounds["--before"])
		deletedToggle.addTo(p, query)
		var overview api.ChatOverview
		path := workspacePath(t, "/messages?"+query.Encode())
		if err := c.do(t, "GET", path, nil, &overview); err != nil {
			return err
		}
		empty := fmt.Sprintf("No messages in workspace %s yet.", t.workspace)
		if len(p.lists) > 0 || len(bounds) > 0 {
			empty = fmt.Sprintf("No matching messages in workspace %s.",
				t.workspace)
		}
		printOverview(overview, empty, bounds["--after"],
			chatReadArgs(p, bounds["--before"]))
		printChangeCursor(os.Stdout, overview.Revision)
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
	after, err := c.chatBound(p, "--after", t, ref)
	if err != nil {
		return err
	}
	before, err := c.chatBound(p, "--before", t, ref)
	if err != nil {
		return err
	}
	query.Set("latest", fmt.Sprint(limit))
	addChatBounds(query, after, before)
	deletedToggle.addTo(p, query)
	page, err := c.chatPage(t, ref, query)
	if err != nil {
		return err
	}
	from := chatCursor(ref, 0)
	if after != nil {
		from = *after
	}
	readArgs := chatReadArgs(p, before)
	printEarlier("", ref, from, page.Earlier, readArgs)
	printChatPage(ref, page, from, emptyChat(p, ref, after), readArgs)
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
	p, err := parseArgs(args, flagSpec{
		strs:    []string{"--after", "--before", "--limit"},
		toggles: []toggle{deletedToggle},
		repeat:  filterFlags(api.MessageFields...),
	})
	if err != nil {
		return err
	}
	if len(p.pos) != 1 {
		return usageError("chat read <ref> [--after <bound>] " +
			"[--before <bound>] [--limit <n>] " +
			"[--include-deleted|--exclude-deleted] " +
			"[--content <pattern>]... [--origin-user <pattern>]...")
	}
	ref, err := api.ParseRef(p.pos[0])
	if err != nil {
		return err
	}
	t, err := c.resolve(ref)
	if err != nil {
		return err
	}
	after, err := c.chatBound(p, "--after", t, ref)
	if err != nil {
		return err
	}
	before, err := c.chatBound(p, "--before", t, ref)
	if err != nil {
		return err
	}
	query, err := searchQuery(p)
	if err != nil {
		return err
	}
	addChatBounds(query, after, before)
	if p.has("--limit") {
		limit, err := parsePositive("--limit", p.strs["--limit"])
		if err != nil {
			return err
		}
		query.Set("limit", fmt.Sprint(limit))
	}
	deletedToggle.addTo(p, query)

	page, err := c.chatPage(t, ref, query)
	if err != nil {
		return err
	}
	from := chatCursor(ref, 0)
	if after != nil {
		from = *after
	}
	printChatPage(ref, page, from, emptyChat(p, ref, after),
		chatReadArgs(p, before))
	return nil
}

func cmdChatWait(c *client, args []string) error {
	p, err := parseArgs(args, flagSpec{
		strs:    []string{"--after", "--timeout"},
		toggles: []toggle{mineToggle, deletedToggle},
		repeat:  filterFlags(api.MessageFields...),
	})
	if err != nil {
		return err
	}
	if len(p.pos) != 1 {
		return usageError("chat wait <ref> [--after <bound>] " +
			"[--timeout <d>] [--include-deleted|--exclude-deleted] " +
			"[--content <pattern>]... [--origin-user <pattern>]... " +
			"[--exclude-mine|--include-mine]")
	}
	ref, err := api.ParseRef(p.pos[0])
	if err != nil {
		return err
	}
	timeout, err := parseTimeout(p, defaultWaitTimeout)
	if err != nil {
		return err
	}
	exclude := mineToggle.isOn(p, true)
	query, err := searchQuery(p)
	if err != nil {
		return err
	}
	deletedToggle.addTo(p, query)

	t, err := c.resolve(ref)
	if err != nil {
		return err
	}
	given, err := c.chatBound(p, "--after", t, ref)
	if err != nil {
		return err
	}
	if exclude {
		if err := excludeMine(query, t.capture); err != nil {
			return err
		}
	}
	var after api.Bound
	if given != nil {
		after = *given
	} else {
		newest := url.Values{"latest": {"1"}}
		page, err := c.chatPage(t, ref, newest)
		if err != nil {
			return err
		}
		after = chatCursor(ref, page.Cursor)
	}
	cursor := after
	poll := func() (api.ChatPage, pollOutcome, error) {
		query.Set("after", cursor.String())
		page, err := c.chatPage(t, ref, query)
		if err != nil {
			return page, nothingYet, err
		}
		cursor = chatCursor(ref, page.Cursor)
		if len(page.Messages) > 0 {
			return page, found, nil
		}
		return page, nothingYet, nil
	}
	page, err := waitFor(poll, timeout, systemClock{})
	if err != nil {
		return err
	}
	printChatMessages(page, fmt.Sprintf("No matching messages %s within "+
		"%s.", describeChatAfter(ref, after), timeout))

	// Remaining counts the messages the filters leave out too, so a wait
	// with more to check reads on with another wait, not a count.
	if page.Remaining > 0 {
		last := api.MessageRef{Post: ref, Number: page.Cursor}
		fmt.Printf("Next: %s (more messages to check)\n",
			chatWaitCommand(last, filterArgs(p)+deletedToggle.arg(p)))
		return nil
	}
	printChatCursor(ref, page, after)
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
