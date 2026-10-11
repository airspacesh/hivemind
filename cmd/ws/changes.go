package main

import (
	"fmt"
	"maps"
	"math"
	"net/url"
	"strconv"
	"time"

	"github.com/airspacesh/workstream/internal/api"
)

// forever is the timeout of a firehose given no --timeout.
const forever = time.Duration(math.MaxInt64)

func (c *client) changesPage(
	t target, since string, filters url.Values,
) (api.ChangesPage, error) {
	query := maps.Clone(filters)
	query.Set("since", since)
	var page api.ChangesPage
	path := workspacePath(t, "/changes?"+query.Encode())
	err := c.do(t, "GET", path, nil, &page)
	return page, err
}

// startRevision is the revision a read of changes from since starts
// after, which is the latest change when since is now.
func (c *client) startRevision(t target, since string) (int64, error) {
	if since != "now" {
		return strconv.ParseInt(since, 10, 64)
	}
	page, err := c.changesPage(t, since, url.Values{})
	return page.Cursor, err
}

// pollChanges polls a workspace's changes that match filters from
// since on, moving since to each page's cursor.
func (c *client) pollChanges(
	t target, since int64, filters url.Values,
) func() (api.ChangesPage, pollOutcome, error) {
	return func() (api.ChangesPage, pollOutcome, error) {
		page, err := c.changesPage(t, fmt.Sprint(since), filters)
		if err != nil {
			return page, nothingYet, err
		}
		since = page.Cursor
		switch {
		case len(page.Changes) > 0:
			return page, found, nil
		case page.More:
			return page, moreToCheck, nil
		default:
			return page, nothingYet, nil
		}
	}
}

// follow waits for changes again and again, handing each page that
// holds some to show, until the timeout passes. It returns the last
// page, which holds none.
func follow(
	poll func() (api.ChangesPage, pollOutcome, error),
	show func(api.ChangesPage),
	timeout time.Duration,
	clk clock,
) (api.ChangesPage, error) {
	start := clk.Now()
	for {
		remaining := timeout - clk.Now().Sub(start)
		page, err := waitFor(poll, remaining, clk)
		if err != nil || len(page.Changes) == 0 {
			return page, err
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
	// filters holds the query parameters that pick the changes shown.
	filters url.Values
	// filterArgs repeats the filter flags for a suggested command.
	filterArgs string
}

// parseChangesArgs reads the arguments of firehose and wait, given
// whether the command leaves out your own changes by default.
func (c *client) parseChangesArgs(
	command string, args []string, timeout time.Duration, mine bool,
) (changesArgs, error) {
	p, err := parseArgs(args, flagSpec{
		strs:   []string{"--since", "--timeout", "--workspace", "--under"},
		bools:  mineFlags,
		repeat: searchFilterFlags(),
	})
	if err != nil {
		return changesArgs{}, err
	}
	if len(p.pos) != 0 {
		return changesArgs{}, usageError(command + " [--since <cursor>] " +
			"[--timeout <d>] [--under <ref>] [search filters] " +
			"[--exclude-mine|--include-mine] [--workspace <name>]")
	}
	parsed := changesArgs{since: "now", filterArgs: filterArgs(p)}
	if p.has("--since") {
		parsed.since = p.strs["--since"]
	}
	revision, err := strconv.ParseInt(parsed.since, 10, 64)
	if parsed.since != "now" && (err != nil || revision < 0) {
		return changesArgs{}, fmt.Errorf("--since must be a cursor, 0 for "+
			"the start, or now, not %q", parsed.since)
	}
	if parsed.timeout, err = parseTimeout(p, timeout); err != nil {
		return changesArgs{}, err
	}
	exclude, err := excludesMine(p, mine)
	if err != nil {
		return changesArgs{}, err
	}
	if parsed.filters, err = searchQuery(p); err != nil {
		return changesArgs{}, err
	}
	if parsed.target, err = c.targetBeside(p, "--under"); err != nil {
		return changesArgs{}, err
	}
	if err := c.addUnder(parsed.target, p, parsed.filters); err != nil {
		return changesArgs{}, err
	}
	if exclude {
		err := excludeMine(parsed.filters, parsed.target.capture)
		if err != nil {
			return changesArgs{}, err
		}
	}
	return parsed, nil
}

// waitCommand is the wait command that reads a workspace's changes
// after a revision, with the filters it was given.
func waitCommand(t target, revision int64, filterArgs string) string {
	return fmt.Sprintf("%s wait --since %d --workspace %s%s", programName,
		revision, t.workspace, filterArgs)
}

func cmdFirehose(c *client, args []string) error {
	parsed, err := c.parseChangesArgs("firehose", args, forever, false)
	if err != nil {
		return err
	}
	t := parsed.target
	since, err := c.startRevision(t, parsed.since)
	if err != nil {
		return err
	}

	fmt.Printf("Following workspace %s after %d.\n", t.workspace, since)
	show := func(page api.ChangesPage) {
		printChanges(page.Changes)
		fmt.Printf("Cursor: %d\n", page.Cursor)
	}
	last, err := follow(c.pollChanges(t, since, parsed.filters), show,
		parsed.timeout, systemClock{})
	if err != nil {
		return err
	}
	fmt.Printf("Stopped at the %s timeout. Cursor: %d\n", parsed.timeout,
		last.Cursor)
	return nil
}

func cmdWait(c *client, args []string) error {
	parsed, err := c.parseChangesArgs("wait", args, defaultWaitTimeout,
		true)
	if err != nil {
		return err
	}
	t := parsed.target
	since, err := c.startRevision(t, parsed.since)
	if err != nil {
		return err
	}
	page, err := waitFor(c.pollChanges(t, since, parsed.filters),
		parsed.timeout, systemClock{})
	if err != nil {
		return err
	}

	if len(page.Changes) == 0 {
		fmt.Printf("No matching changes after %d within %s.\n", since,
			parsed.timeout)
	}
	printChanges(page.Changes)
	next := waitCommand(t, page.Cursor, parsed.filterArgs)
	if page.More {
		fmt.Printf("Next: %s (more changes to check)\n", next)
		return nil
	}
	fmt.Printf("Cursor: %d (wait for more: %s)\n", page.Cursor, next)
	return nil
}
