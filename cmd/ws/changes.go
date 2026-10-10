package main

import (
	"fmt"
	"math"
	"net/url"
	"strconv"
	"time"

	"github.com/airspacesh/workstream/internal/api"
)

// forever is the timeout of a firehose given no --timeout.
const forever = time.Duration(math.MaxInt64)

func (c *client) changesPage(
	t target, since string,
) (api.ChangesPage, error) {
	var page api.ChangesPage
	path := workspacePath(t, "/changes?"+url.Values{"since": {since}}.Encode())
	err := c.do(t, "GET", path, nil, &page)
	return page, err
}

// pollChanges polls a workspace's changes from since on, moving since
// to each page's cursor, which is the latest change when since is now.
func (c *client) pollChanges(
	t target, since string,
) func() (api.ChangesPage, bool, error) {
	return func() (api.ChangesPage, bool, error) {
		page, err := c.changesPage(t, since)
		if err == nil {
			since = fmt.Sprint(page.Cursor)
		}
		return page, len(page.Changes) > 0, err
	}
}

// follow waits for changes again and again, handing each page that
// holds some to show, until the timeout passes.
func follow(
	poll func() (api.ChangesPage, bool, error),
	show func(api.ChangesPage),
	timeout time.Duration,
	clk clock,
) error {
	start := clk.Now()
	for {
		remaining := timeout - clk.Now().Sub(start)
		page, err := waitFor(poll, remaining, clk)
		if err != nil {
			return err
		}
		if len(page.Changes) == 0 {
			return nil
		}
		show(page)
	}
}

type changesArgs struct {
	target target
	// since is a change cursor, 0 for the start, or now for the
	// latest change.
	since   string
	timeout time.Duration
}

// parseChangesArgs reads the arguments of firehose and wait.
func (c *client) parseChangesArgs(
	command string, args []string, timeout time.Duration,
) (changesArgs, error) {
	p, err := parseArgs(args, flagSpec{
		strs: []string{"--since", "--timeout", "--workspace"},
	})
	if err != nil {
		return changesArgs{}, err
	}
	if len(p.pos) != 0 {
		return changesArgs{}, usageError(command + " [--since <cursor>] " +
			"[--timeout <d>] [--workspace <name>]")
	}
	parsed := changesArgs{since: "now", timeout: timeout}
	if p.has("--since") {
		parsed.since = p.strs["--since"]
	}
	revision, err := strconv.ParseInt(parsed.since, 10, 64)
	if parsed.since != "now" && (err != nil || revision < 0) {
		return changesArgs{}, fmt.Errorf("--since must be a cursor, 0 for "+
			"the start, or now, not %q", parsed.since)
	}
	if p.has("--timeout") {
		parsed.timeout, err = time.ParseDuration(p.strs["--timeout"])
		if err != nil || parsed.timeout <= 0 {
			return changesArgs{}, fmt.Errorf("--timeout must be a "+
				"positive duration, like 1m, not %q", p.strs["--timeout"])
		}
	}
	workspace, err := c.config.chooseWorkspace(p.strs["--workspace"])
	if err != nil {
		return changesArgs{}, err
	}
	parsed.target = targetOf(workspace)
	return parsed, nil
}

// changesCommand is the command that waits on in a workspace from a
// revision.
func changesCommand(t target, revision int64) string {
	return fmt.Sprintf("%s wait --since %d --workspace %s", programName,
		revision, t.workspace)
}

func cmdFirehose(c *client, args []string) error {
	parsed, err := c.parseChangesArgs("firehose", args, forever)
	if err != nil {
		return err
	}
	t := parsed.target
	since := parsed.since
	if since == "now" {
		page, err := c.changesPage(t, since)
		if err != nil {
			return err
		}
		since = fmt.Sprint(page.Cursor)
	}

	fmt.Printf("Following workspace %s after %s.\n", t.workspace, since)
	cursor := since
	show := func(page api.ChangesPage) {
		printChanges(page.Changes)
		cursor = fmt.Sprint(page.Cursor)
		fmt.Printf("Cursor: %s\n", cursor)
	}
	err = follow(c.pollChanges(t, since), show, parsed.timeout,
		systemClock{})
	if err != nil {
		return err
	}
	fmt.Printf("Stopped at the %s timeout. Cursor: %s\n", parsed.timeout,
		cursor)
	return nil
}

func cmdWait(c *client, args []string) error {
	parsed, err := c.parseChangesArgs("wait", args, defaultWaitTimeout)
	if err != nil {
		return err
	}
	t := parsed.target
	page, err := waitFor(c.pollChanges(t, parsed.since), parsed.timeout,
		systemClock{})
	if err != nil {
		return err
	}

	if len(page.Changes) == 0 {
		fmt.Printf("No changes after %d within %s.\n", page.Cursor,
			parsed.timeout)
	}
	printChanges(page.Changes)
	if page.More {
		fmt.Printf("Next: %s (more changes follow)\n",
			changesCommand(t, page.Cursor))
		return nil
	}
	fmt.Printf("Cursor: %d (wait for more: %s)\n", page.Cursor,
		changesCommand(t, page.Cursor))
	return nil
}
