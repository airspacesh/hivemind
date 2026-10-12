package main

import (
	"crypto/rand"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/user"
	"strings"
	"time"

	"github.com/airspacesh/workstream/internal/api"
)

const claudeSessionEnvironment = "CLAUDE_CODE_SESSION_ID"

// currentUser names the user writing, in the origin of a post and in
// an edit. It falls back to $USER.
func currentUser() string {
	if current, err := user.Current(); err == nil {
		return current.Username
	}
	return os.Getenv("USER")
}

// captureOrigin records who is writing, and where as far as the
// workspace's metadata capture allows. The session is empty outside
// Claude Code.
func captureOrigin(capture metadataCapture) (api.Origin, error) {
	origin := api.Origin{User: currentUser()}
	if capture.hostname {
		host, err := os.Hostname()
		if err != nil {
			return api.Origin{}, fmt.Errorf("reading the hostname: %w", err)
		}
		origin.Host = host
	}
	if capture.directory {
		dir, err := os.Getwd()
		if err != nil {
			return api.Origin{}, fmt.Errorf("reading the working "+
				"directory: %w", err)
		}
		origin.Dir = dir
	}
	if capture.session {
		origin.ClaudeSession = os.Getenv(claudeSessionEnvironment)
	}
	return origin, nil
}

// excludeMine asks the server to leave out the changes this client
// writes, whose origin has the user and Claude Code session it records
// now. With no session recorded, that is only what this user wrote
// outside a session.
func excludeMine(query url.Values, capture metadataCapture) error {
	origin, err := captureOrigin(capture)
	if err != nil {
		return err
	}
	query.Set("exclude-user", origin.User)
	query.Set("exclude-claude-session", origin.ClaudeSession)
	return nil
}

// uncertainWrite reports a failed write. One that may have taken
// effect says how to check, since it is never repeated.
func uncertainWrite(err error, outcome, check string) error {
	if !mayHaveApplied(err) {
		return err
	}
	return fmt.Errorf("%w\n%s may have taken effect anyway, and %s did "+
		"not retry. Check '%s' before trying again", err, outcome,
		programName, check)
}

// uncertainCreate reports a failed add or send. One that may have taken
// effect gives the command that repeats it under its request id, which
// cannot make a second copy.
func uncertainCreate(err error, outcome, repeat string) error {
	if !mayHaveApplied(err) {
		return err
	}
	return fmt.Errorf("%w\n%s may have taken effect anyway. This repeats "+
		"it under the same request id, so it cannot make a second "+
		"copy:\n  %s", err, outcome, repeat)
}

// requestID is the id of the one write a command makes: the id
// --request-id gives, or else a new random one.
func requestID(p *parsedArgs) (string, error) {
	if !p.has("--request-id") {
		return rand.Text(), nil
	}
	if p.strs["--request-id"] == "" {
		return "", errors.New("--request-id needs an id")
	}
	return p.strs["--request-id"], nil
}

// repeatCommand is the command line that repeats a command, given its
// args, under its request id. The id goes first, so that a -- among
// the args cannot turn it into a positional argument.
func repeatCommand(
	command string, args []string, p *parsedArgs, requestID string,
) string {
	text := programName + " " + command
	if !p.has("--request-id") {
		text += " --request-id " + shellQuote(requestID)
	}
	for _, arg := range args {
		text += " " + shellQuote(arg)
	}
	return text
}

// printWhoCanRead says who alone can read a post a write left
// private, and when public says so if asked to.
func printWhoCanRead(post api.Post, public bool) {
	switch {
	case post.Private:
		fmt.Printf("Only %s can read it.\n", post.PrivateTo)
	case post.PrivateTo != "":
		fmt.Printf("Only %s can read it, as a post above it is private.\n",
			post.PrivateTo)
	case public:
		fmt.Println("Everyone can read it.")
	}
}

// explainNotOwner adds the next step to an edit or delete refused
// because another user posted what it changes.
func explainNotOwner(err error, post api.Ref) error {
	var refused *serverError
	if !errors.As(err, &refused) || refused.code != api.CodeNotOwner {
		return err
	}
	return fmt.Errorf("%w. To suggest a change, send a message in its "+
		"chat with '%s chat send %s \"<message>\"'", err, programName,
		post)
}

func postPath(ref api.Ref, rest string) string {
	return "/api/posts/" + ref.String() + rest
}

func workspacePath(t target, rest string) string {
	return "/api/workspaces/" + url.PathEscape(t.workspace) + rest
}

func (c *client) search(t target, query url.Values) (api.MatchPage, error) {
	var page api.MatchPage
	err := c.do(t, "GET", workspacePath(t, "/posts?"+query.Encode()), nil,
		&page)
	return page, err
}

func shorthandFlags() []string {
	flags := make([]string, len(shorthandKeys))
	for i, key := range shorthandKeys {
		flags[i] = "--" + key
	}
	return flags
}

// collectMetadata gathers the pairs given by --meta and the shorthand
// flags, which add and edit accept alike.
func collectMetadata(p *parsedArgs) (map[string]string, error) {
	metadata := map[string]string{}
	set := func(key, value string) error {
		if _, exists := metadata[key]; exists {
			return fmt.Errorf("metadata key %q is set more than once. For "+
				"several, use --meta %s-1=... --meta %s-2=...", key, key, key)
		}
		metadata[key] = value
		return nil
	}
	for _, pair := range p.lists["--meta"] {
		key, value, found := strings.Cut(pair, "=")
		if !found {
			return nil, fmt.Errorf("--meta %q needs <key>=<value>", pair)
		}
		if err := set(key, value); err != nil {
			return nil, err
		}
	}
	for _, key := range shorthandKeys {
		for _, value := range p.lists["--"+key] {
			if err := set(key, value); err != nil {
				return nil, err
			}
		}
	}
	return metadata, nil
}

// parseLink reads a link given as <name>=<ref>.
func parseLink(flag, value string) (api.Link, error) {
	name, text, found := strings.Cut(value, "=")
	if !found {
		return api.Link{}, fmt.Errorf("%s needs <name>=<ref>, like "+
			"depends-on=e40", flag)
	}
	ref, err := api.ParseRef(text)
	if err != nil {
		return api.Link{}, err
	}
	return api.Link{Name: name, Ref: ref}, nil
}

// collectLinks gathers the links given by --parent, --supersedes and
// --link, which add and edit accept alike, and checks each target is on
// the post's server. An empty --parent is left to edit, where it
// removes the parent.
func (c *client) collectLinks(t target, p *parsedArgs) ([]api.Link, error) {
	var links []api.Link
	if parent := p.strs["--parent"]; parent != "" {
		ref, err := api.ParseRef(parent)
		if err != nil {
			return nil, err
		}
		links = append(links, api.Link{Name: api.ParentLink, Ref: ref})
	}
	for _, value := range p.lists["--supersedes"] {
		ref, err := api.ParseRef(value)
		if err != nil {
			return nil, err
		}
		links = append(links, api.Link{Name: api.SupersedesLink, Ref: ref})
	}
	for _, value := range p.lists["--link"] {
		link, err := parseLink("--link", value)
		if err != nil {
			return nil, err
		}
		links = append(links, link)
	}

	for _, link := range links {
		home, err := c.resolve(link.Ref)
		if err != nil {
			return nil, err
		}
		if home.server != t.server {
			return nil, fmt.Errorf("%s is on %s, but the post is on %s, "+
				"and a link cannot join posts on different servers",
				link.Ref, home.server, t.server)
		}
	}
	return links, nil
}

func (c *client) projects(
	t target, query url.Values,
) (api.ProjectsResponse, error) {
	var projects api.ProjectsResponse
	err := c.do(t, "GET", workspacePath(t, "/projects?"+query.Encode()),
		nil, &projects)
	return projects, err
}

// project reads one project of a workspace, with its count of every
// post, which costs the server time in proportion to that project, not
// to the workspace.
func (c *client) project(
	t target, name string,
) (api.ProjectsResponse, error) {
	var projects api.ProjectsResponse
	path := workspacePath(t, "/projects?"+url.Values{"name": {name}}.Encode())
	err := c.do(t, "GET", path, nil, &projects)
	return projects, err
}

// noticeNewProject says when a post is the first in its project, which
// catches a mistyped project without refusing a new one.
func (c *client) noticeNewProject(t target, project string) error {
	result, err := c.project(t, project)
	if err != nil {
		return err
	}
	for _, existing := range result.Projects {
		if existing.Name == project && *existing.Posts == 1 {
			fmt.Printf("This is the first post in project %q. Run '%s "+
				"list-projects' if you meant an existing one.\n", project,
				programName)
		}
	}
	return nil
}

// targetBeside is where a command works on the tree of the post refFlag
// names: in the workspace --workspace names, or else in that post's,
// since a tree never leaves its workspace, or else in the default
// workspace.
func (c *client) targetBeside(p *parsedArgs, refFlag string) (target, error) {
	if p.strs["--workspace"] == "" && p.has(refFlag) {
		ref, err := api.ParseRef(p.strs[refFlag])
		if err != nil {
			return target{}, err
		}
		return c.resolve(ref)
	}
	workspace, err := c.config.chooseWorkspace(p.strs["--workspace"])
	if err != nil {
		return target{}, err
	}
	return targetOf(workspace), nil
}

// checkBoundServer checks that a bound naming a post or message names
// one on the target's server, since a bound names a point in its
// server's history and revisions count on each server apart.
func (c *client) checkBoundServer(t target, bound api.Bound) error {
	if bound.Post.ID == 0 {
		return nil
	}
	home, err := c.resolve(bound.Post)
	if err != nil {
		return err
	}
	if home.server != t.server {
		return fmt.Errorf("%s is on %s, but workspace %s is on %s, and "+
			"revisions count on each server apart, so %s cannot bound a "+
			"read of %s", bound.Post, home.server, t.workspace, t.server,
			bound, t.workspace)
	}
	return nil
}

// boundedTarget is where a listing or wait bounded by bounds works: as
// targetBeside has it for --under, except that without --workspace or
// --under it works in the workspace of the first post or message a
// bound names, as a wait from a post's id reads on from it.
func (c *client) boundedTarget(
	p *parsedArgs, bounds ...api.Bound,
) (target, error) {
	t, err := c.targetBeside(p, "--under")
	if err != nil {
		return target{}, err
	}
	if p.strs["--workspace"] == "" && !p.has("--under") {
		for _, bound := range bounds {
			if bound.Post.ID != 0 {
				if t, err = c.resolve(bound.Post); err != nil {
					return target{}, err
				}
				break
			}
		}
	}
	for _, bound := range bounds {
		if err := c.checkBoundServer(t, bound); err != nil {
			return target{}, err
		}
	}
	return t, nil
}

func cmdAdd(c *client, args []string) error {
	p, err := parseArgs(args, flagSpec{
		strs: []string{"--project", "--body", "--parent", "--workspace",
			"--request-id"},
		bools: []string{"--private"},
		repeat: append([]string{"--meta", "--supersedes", "--link"},
			shorthandFlags()...),
	})
	if err != nil {
		return err
	}
	if len(p.pos) != 1 {
		return usageError("add <subject> --project <p> " +
			"[--body <b>] [--meta <key>=<value>]... [--parent <ref>] " +
			"[--supersedes <ref>]... [--link <name>=<ref>]... [--private] " +
			"[--workspace <name>]")
	}
	metadata, err := collectMetadata(p)
	if err != nil {
		return err
	}
	id, err := requestID(p)
	if err != nil {
		return err
	}
	if p.strs["--project"] == "" {
		return fmt.Errorf("add needs --project <name>. Run '%s "+
			"list-projects' to see the projects in the workspace",
			programName)
	}
	if p.has("--parent") && p.strs["--parent"] == "" {
		return fmt.Errorf("--parent needs a post id")
	}
	t, err := c.targetBeside(p, "--parent")
	if err != nil {
		return err
	}
	links, err := c.collectLinks(t, p)
	if err != nil {
		return err
	}
	origin, err := captureOrigin(t.capture)
	if err != nil {
		return err
	}
	request := api.AddPostRequest{
		Project:   p.strs["--project"],
		Subject:   p.pos[0],
		Body:      p.strs["--body"],
		Metadata:  metadata,
		Origin:    origin,
		Links:     links,
		Private:   p.bools["--private"],
		RequestID: id,
	}
	if err := request.Validate(); err != nil {
		return err
	}

	var post api.Post
	err = sendRetrying(func() error {
		return c.do(t, "POST", workspacePath(t, "/posts"), request, &post)
	}, systemClock{})
	if err != nil {
		return uncertainCreate(err, "The post",
			repeatCommand("add", args, p, id))
	}
	fmt.Printf("Added post [%s].\n", post.Ref)
	printWhoCanRead(post, false)
	if err := c.noticeNewProject(t, request.Project); err != nil {
		return fmt.Errorf("the post was added, but listing projects "+
			"failed: %w", err)
	}
	return nil
}

func cmdEdit(c *client, args []string) error {
	p, err := parseArgs(args, flagSpec{
		strs:  []string{"--project", "--subject", "--body", "--parent"},
		bools: []string{"--private", "--public"},
		repeat: append(
			[]string{"--meta", "--supersedes", "--link", "--unlink"},
			shorthandFlags()...),
	})
	if err != nil {
		return err
	}
	if len(p.pos) < 1 || len(p.pos) > 2 {
		return usageError("edit <ref> [<subject>] [--project <p>] " +
			"[--body <b>] [--meta <key>=<value>]... " +
			"[--parent <ref>|\"\"] [--supersedes <ref>]... " +
			"[--link <name>=<ref>]... [--unlink <name>[=<ref>]]... " +
			"[--private|--public]")
	}
	ref, err := api.ParseRef(p.pos[0])
	if err != nil {
		return err
	}

	request := api.EditPostRequest{}
	if p.has("--project") {
		project := p.strs["--project"]
		request.Project = &project
	}
	if len(p.pos) == 2 && p.has("--subject") {
		return fmt.Errorf("give the subject once, as <subject> or " +
			"--subject")
	}
	if len(p.pos) == 2 {
		request.Subject = &p.pos[1]
	} else if p.has("--subject") {
		subject := p.strs["--subject"]
		request.Subject = &subject
	}
	if p.has("--body") {
		body := p.strs["--body"]
		request.Body = &body
	}
	if p.bools["--private"] && p.bools["--public"] {
		return fmt.Errorf("give --private or --public, not both")
	}
	if p.bools["--private"] || p.bools["--public"] {
		private := p.bools["--private"]
		request.Private = &private
	}
	metadata, err := collectMetadata(p)
	if err != nil {
		return err
	}
	if len(metadata) > 0 {
		request.Metadata = metadata
	}
	if p.has("--parent") && p.strs["--parent"] == "" {
		request.RemoveLinkNames = append(request.RemoveLinkNames,
			api.ParentLink)
	}
	for _, value := range p.lists["--unlink"] {
		if !strings.Contains(value, "=") {
			request.RemoveLinkNames = append(request.RemoveLinkNames, value)
			continue
		}
		link, err := parseLink("--unlink", value)
		if err != nil {
			return err
		}
		request.RemoveLinks = append(request.RemoveLinks, link)
	}

	t, err := c.resolve(ref)
	if err != nil {
		return err
	}
	if request.AddLinks, err = c.collectLinks(t, p); err != nil {
		return err
	}
	request.User = currentUser()
	if err := request.Validate(); err != nil {
		return err
	}
	var post api.Post
	if err := c.do(t, "PATCH", postPath(ref, ""), request, &post); err != nil {
		return uncertainWrite(explainNotOwner(err, ref), "The edit",
			programName+" post "+ref.String())
	}
	fmt.Printf("Edited post [%s].\n", post.Ref)
	printWhoCanRead(post, request.Private != nil)
	return nil
}

func cmdDelete(c *client, args []string) error {
	if len(args) != 1 {
		return usageError("delete <ref>")
	}
	ref, err := api.ParseRef(args[0])
	if err != nil {
		return err
	}
	request := api.DeleteRequest{User: currentUser()}
	if err := request.Validate(); err != nil {
		return err
	}
	t, err := c.resolve(ref)
	if err != nil {
		return err
	}

	var post api.Post
	if err := c.do(t, "DELETE", postPath(ref, ""), request,
		&post); err != nil {
		return uncertainWrite(explainNotOwner(err, ref), "The delete",
			programName+" post "+ref.String())
	}
	fmt.Printf("Deleted post [%s].\n", post.Ref)
	return nil
}

// showMessage shows one message in full, under its post's line.
func (c *client) showMessage(ref api.MessageRef) error {
	t, err := c.resolve(ref.Post)
	if err != nil {
		return err
	}
	var detail api.MessageDetail
	path := postPath(ref.Post, fmt.Sprintf("/messages/%d", ref.Number))
	if err := c.do(t, "GET", path, nil, &detail); err != nil {
		return err
	}
	printMessageDetail(detail)
	return nil
}

func cmdPost(c *client, args []string) error {
	if len(args) != 1 {
		return usageError("post <ref>|<ref>-m<n>")
	}
	if message, err := api.ParseMessageRef(args[0]); err == nil {
		return c.showMessage(message)
	}
	ref, err := api.ParseRef(args[0])
	if err != nil {
		return err
	}
	t, err := c.resolve(ref)
	if err != nil {
		return err
	}
	var detail api.PostDetail
	if err := c.do(t, "GET", postPath(ref, ""), nil, &detail); err != nil {
		return err
	}
	printPostDetail(detail)
	return nil
}

func cmdTree(c *client, args []string) error {
	p, err := parseArgs(args, flagSpec{
		strs:    []string{"--limit"},
		toggles: []toggle{supersededToggle, deletedToggle},
	})
	if err != nil {
		return err
	}
	if len(p.pos) != 1 {
		return usageError("tree <ref> [--limit <n>] " +
			"[--include-superseded|--exclude-superseded] " +
			"[--include-deleted|--exclude-deleted]")
	}
	ref, err := api.ParseRef(p.pos[0])
	if err != nil {
		return err
	}
	query := url.Values{}
	if p.has("--limit") {
		limit, err := parsePositive("--limit", p.strs["--limit"])
		if err != nil {
			return err
		}
		query.Set("limit", fmt.Sprint(limit))
	}
	supersededToggle.addTo(p, query)
	deletedToggle.addTo(p, query)

	t, err := c.resolve(ref)
	if err != nil {
		return err
	}
	var tree api.TreeResponse
	path := postPath(ref, "/tree?"+query.Encode())
	if err := c.do(t, "GET", path, nil, &tree); err != nil {
		return err
	}
	printTree(tree, ref)
	return nil
}

// addUnder adds the --under a command was given to its query, which
// must name a post in the target's workspace, since a subtree never
// leaves its workspace.
func (c *client) addUnder(t target, p *parsedArgs, query url.Values) error {
	if !p.has("--under") {
		return nil
	}
	under, err := api.ParseRef(p.strs["--under"])
	if err != nil {
		return err
	}
	home, err := c.resolve(under)
	if err != nil {
		return err
	}
	if home.server != t.server || home.workspace != t.workspace {
		return fmt.Errorf("%s is in workspace %s on %s, not in %s on %s, "+
			"so nothing in %s is under it. Leave out --workspace to work "+
			"in %s", under, home.workspace, home.server, t.workspace,
			t.server, t.workspace, home.workspace)
	}
	query.Set("under", under.String())
	return nil
}

// searchQuery turns the filter flags into the server's query.
func searchQuery(p *parsedArgs) (url.Values, error) {
	query := url.Values{}
	for flag, values := range p.lists {
		if key, negate, ok := shorthandFor(flag); ok {
			parameter := "meta"
			if negate {
				parameter = "no-meta"
			}
			for _, value := range values {
				query.Add(parameter, key+"="+value)
			}
			continue
		}

		parameter := strings.TrimPrefix(flag, "--")
		for _, value := range values {
			if (parameter == "meta" || parameter == "no-meta") &&
				!strings.Contains(value, "=") {
				return nil, fmt.Errorf("%s needs <key>=<value>", flag)
			}
			query.Add(parameter, value)
		}
	}
	return query, nil
}

// listingHeader says what a listing ran, like Newest 20 posts matching
// --project launcher:
func listingHeader(p *parsedArgs, limit int, items api.Items) string {
	nouns := map[api.Items][2]string{
		api.PostsOnly:    {"post", "posts"},
		api.ChatOnly:     {"message", "messages"},
		api.PostsAndChat: {"post or message", "posts and messages"},
	}[items]
	noun := nouns[1]
	if limit == 1 {
		noun = nouns[0]
	}
	text := fmt.Sprintf("Newest %d %s", limit, noun)
	if p.has("--under") {
		text += " under " + p.strs["--under"]
	}
	for _, flag := range []string{"--after", "--before"} {
		if p.has(flag) {
			text += " " + strings.TrimPrefix(flag, "--") + " " + p.strs[flag]
		}
	}
	matching := conditionArgs(p)
	for _, arg := range p.pos {
		matching = " " + shellQuote(arg) + matching
	}
	if matching != "" {
		text += " matching" + matching
	}
	if supersededToggle.isOn(p, false) {
		text += ", superseded included"
	}
	if deletedToggle.isOn(p, false) {
		text += ", deleted included"
	}
	return text + ":"
}

// olderCommand is the command that lists the matches before a revision,
// with the text, flags and bounds a listing was given.
func olderCommand(
	command string, t target, p *parsedArgs, after *api.Bound,
	before int64,
) string {
	text := programName + " " + command
	for _, arg := range p.pos {
		text += " " + shellQuote(arg)
	}
	text += " --before " + api.FormatRevision(before) +
		" --workspace " + shellQuote(t.workspace)
	if after != nil {
		text += " --after " + boundArg(*after)
	}
	if p.has("--limit") {
		text += " --limit " + shellQuote(p.strs["--limit"])
	}
	return text + supersededToggle.arg(p) + deletedToggle.arg(p) +
		filterArgs(p)
}

// cmdListing lists the newest matches, for recent or search. Search
// takes a <text> and includes chat by default. Recent leaves chat out
// by default.
func cmdListing(c *client, command string, args []string) error {
	p, err := parseArgs(args, searchFlagSpec())
	if err != nil {
		return err
	}
	switch command {
	case "search":
		if len(p.pos) > 1 {
			return fmt.Errorf("give at most one <text> (got %d)",
				len(p.pos))
		}
		if p.has(deletedToggle.on) || p.has(deletedToggle.off) {
			return fmt.Errorf("search never lists deleted posts or "+
				"messages, so it takes no %s or %s", deletedToggle.on,
				deletedToggle.off)
		}
	case "recent":
		if len(p.pos) != 0 {
			return fmt.Errorf("recent takes no <text>. Use '%s search "+
				"<text>'", programName)
		}
	}
	items, err := readItems(p, command, command == "search")
	if err != nil {
		return err
	}

	query, err := searchQuery(p)
	if err != nil {
		return err
	}
	if len(p.pos) == 1 {
		query.Add("content", p.pos[0])
	}

	now := time.Now()
	var after *api.Bound
	var bounds []api.Bound
	for _, flag := range []string{"--after", "--before"} {
		if !p.has(flag) {
			continue
		}
		bound, err := parseBoundFlag(flag, p.strs[flag], now)
		if err != nil {
			return err
		}
		query.Set(strings.TrimPrefix(flag, "--"), bound.String())
		bounds = append(bounds, bound)
		if flag == "--after" {
			after = &bound
		}
	}

	limit := 20
	if p.has("--limit") {
		if limit, err = parsePositive("--limit", p.strs["--limit"]); err != nil {
			return err
		}
	}
	query.Set("limit", fmt.Sprint(limit))
	supersededToggle.addTo(p, query)
	deletedToggle.addTo(p, query)
	query.Set("items", items.String())

	t, err := c.boundedTarget(p, bounds...)
	if err != nil {
		return err
	}
	if err := c.addUnder(t, p, query); err != nil {
		return err
	}
	page, err := c.search(t, query)
	if err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, listingHeader(p, limit, items))
	// With --id-only, stdout holds only ids, so the footer goes to
	// stderr with the header, where a script can still find the next
	// page and the cursor.
	footer := os.Stdout
	if p.bools["--id-only"] {
		printMatchRefs(page)
		footer = os.Stderr
	} else {
		if len(page.Matches) == 0 {
			fmt.Println("No matches.")
		}
		printMatches(page.Matches)
	}
	if page.More {
		oldest := page.Matches[len(page.Matches)-1].Revision
		fmt.Fprintf(footer, "Next: %s (more older matches)\n",
			olderCommand(command, t, p, after, oldest))
	}
	if command == "recent" {
		printChangeCursor(footer, page.Revision)
	}
	return nil
}

func cmdListProjects(c *client, args []string) error {
	p, err := parseArgs(args, flagSpec{
		strs:    []string{"--limit", "--workspace"},
		toggles: []toggle{deletedToggle},
	})
	if err != nil {
		return err
	}
	if len(p.pos) != 0 {
		return usageError("list-projects [--limit 30] " +
			"[--include-deleted|--exclude-deleted] [--workspace <name>]")
	}

	limit := 30
	if p.has("--limit") {
		if limit, err = parsePositive("--limit", p.strs["--limit"]); err != nil {
			return err
		}
	}
	workspace, err := c.config.chooseWorkspace(p.strs["--workspace"])
	if err != nil {
		return err
	}
	query := url.Values{"limit": {fmt.Sprint(limit)}}
	deletedToggle.addTo(p, query)
	result, err := c.projects(targetOf(workspace), query)
	if err != nil {
		return err
	}
	if len(result.Projects) == 0 {
		fmt.Printf("Workspace %s has no projects yet.\n", workspace.name)
		return nil
	}
	printProjects(result)
	more := result.Count - len(result.Projects)
	noun := "projects"
	if more == 1 {
		noun = "project"
	}
	if more > 0 {
		fmt.Printf("Next: %s list-projects --limit %d%s --workspace %s "+
			"(%d more %s)\n", programName, result.Count,
			deletedToggle.arg(p), shellQuote(workspace.name), more, noun)
	}
	return nil
}
