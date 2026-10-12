package main

import (
	"fmt"
	"io"
	"maps"
	"math"
	"net/url"
	"os"
	"time"

	"github.com/airspacesh/workstream/internal/api"
)

// forever is the timeout of a firehose given no --timeout.
const forever = time.Duration(math.MaxInt64)

func (c *client) changesPage(
	t target, query url.Values,
) (api.ChangesPage, error) {
	var page api.ChangesPage
	path := workspacePath(t, "/changes?"+query.Encode())
	err := c.do(t, "GET", path, nil, &page)
	return page, err
}

// latestRevision is the revision of the latest change on the server.
func (c *client) latestRevision(t target) (int64, error) {
	page, err := c.changesPage(t, url.Values{})
	return page.Cursor, err
}

// pollChanges polls a workspace's changes that match filters after the
// bound after, moving after to each page's cursor.
func (c *client) pollChanges(
	t target, after api.ChangeBound, filters url.Values,
) func() (api.ChangesPage, pollOutcome, error) {
	query := maps.Clone(filters)
	return func() (api.ChangesPage, pollOutcome, error) {
		query.Set("after", after.String())
		page, err := c.changesPage(t, query)
		if err != nil {
			return page, nothingYet, err
		}
		after = api.ChangeBound{Revision: page.Cursor}
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
	// after is where to read changes after, which is the latest change
	// when the command names none.
	after   api.ChangeBound
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
		strs:   []string{"--after", "--timeout", "--workspace", "--under"},
		bools:  mineFlags,
		repeat: searchFilterFlags(),
	})
	if err != nil {
		return changesArgs{}, err
	}
	if len(p.pos) != 0 {
		return changesArgs{}, usageError(command + " " +
			"[--after @<revision>|<id>] [--timeout <d>] [--under <ref>] " +
			"[search filters] " +
			"[--exclude-mine|--include-mine] [--workspace <name>]")
	}
	parsed := changesArgs{filterArgs: filterArgs(p)}
	if p.has("--after") {
		parsed.after, err = parseChangeBoundFlag("--after",
			p.strs["--after"])
		if err != nil {
			return changesArgs{}, err
		}
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
	// A wait after a post or message reads that post's workspace, as a
	// wait under a post does, unless the command names another.
	if p.strs["--workspace"] == "" && !p.has("--under") &&
		parsed.after.Post.ID != 0 {
		parsed.target, err = c.resolve(parsed.after.Post)
	} else {
		parsed.target, err = c.targetBeside(p, "--under")
	}
	if err != nil {
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
	if !p.has("--after") {
		latest, err := c.latestRevision(parsed.target)
		if err != nil {
			return changesArgs{}, err
		}
		parsed.after = api.ChangeBound{Revision: latest}
	}
	return parsed, nil
}

// waitCommand is the wait command that reads a workspace's changes
// after a revision, with the filters it was given.
func waitCommand(t target, revision int64, filterArgs string) string {
	return fmt.Sprintf("%s wait --after %s --workspace %s%s", programName,
		api.FormatRevision(revision), t.workspace, filterArgs)
}

// printChangeCursor ends a read of a workspace with its cursor, the
// revision it read at, from which a wait misses nothing.
func printChangeCursor(w io.Writer, revision int64) {
	fmt.Fprintf(w, "Cursor: %s\n", api.FormatRevision(revision))
}

func cmdFirehose(c *client, args []string) error {
	parsed, err := c.parseChangesArgs("firehose", args, forever, false)
	if err != nil {
		return err
	}
	t := parsed.target

	fmt.Printf("Following workspace %s after %s.\n", t.workspace,
		parsed.after)
	show := func(page api.ChangesPage) {
		printChanges(page.Changes)
		printChangeCursor(os.Stdout, page.Cursor)
	}
	last, err := follow(c.pollChanges(t, parsed.after, parsed.filters),
		show, parsed.timeout, systemClock{})
	if err != nil {
		return err
	}
	fmt.Printf("Stopped at the %s timeout. Cursor: %s\n", parsed.timeout,
		api.FormatRevision(last.Cursor))
	return nil
}

func cmdWait(c *client, args []string) error {
	parsed, err := c.parseChangesArgs("wait", args, defaultWaitTimeout,
		true)
	if err != nil {
		return err
	}
	t := parsed.target
	page, err := waitFor(c.pollChanges(t, parsed.after, parsed.filters),
		parsed.timeout, systemClock{})
	if err != nil {
		return err
	}

	if len(page.Changes) == 0 {
		fmt.Printf("No matching changes after %s within %s.\n",
			parsed.after, parsed.timeout)
	}
	printChanges(page.Changes)
	if page.More {
		fmt.Printf("Next: %s (more changes to check)\n",
			waitCommand(t, page.Cursor, parsed.filterArgs))
		return nil
	}
	printChangeCursor(os.Stdout, page.Cursor)
	return nil
}
