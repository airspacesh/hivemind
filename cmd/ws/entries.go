package main

import (
	"fmt"
	"net/url"
	"os"
	"os/user"
	"strings"

	"github.com/airspacesh/workstream/internal/api"
)

const claudeSessionEnvironment = "CLAUDE_CODE_SESSION_ID"

// captureOrigin records who is writing, and where as far as the
// workspace's metadata capture allows. The username falls back to
// $USER, and the session is empty outside Claude Code.
func captureOrigin(capture metadataCapture) (api.Origin, error) {
	origin := api.Origin{User: os.Getenv("USER")}
	if current, err := user.Current(); err == nil {
		origin.User = current.Username
	}
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

func entryPath(ref api.Ref, rest string) string {
	return "/api/entries/" + ref.String() + rest
}

func workspacePath(t target, rest string) string {
	return "/api/workspaces/" + url.PathEscape(t.workspace) + rest
}

func (c *client) search(t target, query url.Values) (api.SearchResult, error) {
	var result api.SearchResult
	err := c.do(t, "GET", workspacePath(t, "/entries?"+query.Encode()), nil,
		&result)
	return result, err
}

const recentAfterChangeCount = 5

// printRecentAfterChange lists the most recently changed entries in a
// workspace other than the one just changed. That entry is dropped
// after the fetch rather than skipped by offset, because another
// agent's write can land in between and take the top slot.
func (c *client) printRecentAfterChange(t target, changed api.Ref) error {
	query := url.Values{}
	query.Set("limit", fmt.Sprint(recentAfterChangeCount+1))
	result, err := c.search(t, query)
	if err != nil {
		return err
	}

	var others []api.ListedEntry
	for _, e := range result.Entries {
		if e.Ref != changed && len(others) < recentAfterChangeCount {
			others = append(others, e.ListedEntry)
		}
	}
	if len(others) == 0 {
		return nil
	}
	fmt.Println()
	fmt.Println("Other recent entries:")
	printEntryLines(others, byModified)
	return nil
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
// --link, which add and edit accept alike, and checks each target is
// on the entry's server. An empty --parent is left to edit, where it
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
			return nil, fmt.Errorf("%s is on %s, but the entry is on %s, "+
				"and a link cannot join entries on different servers",
				link.Ref, home.server, t.server)
		}
	}
	return links, nil
}

func (c *client) projects(
	t target, limit int,
) (api.ProjectsResponse, error) {
	var projects api.ProjectsResponse
	query := url.Values{"limit": {fmt.Sprint(limit)}}.Encode()
	err := c.do(t, "GET", workspacePath(t, "/projects?"+query), nil,
		&projects)
	return projects, err
}

// project reads one project of a workspace, with its count of every
// entry, which costs the server time in proportion to that project, not
// to the workspace.
func (c *client) project(
	t target, name string,
) (api.ProjectsResponse, error) {
	var projects api.ProjectsResponse
	path := workspacePath(t, "/projects?"+url.Values{"name": {name}}.Encode())
	err := c.do(t, "GET", path, nil, &projects)
	return projects, err
}

// noticeNewProject says when an entry is the first in its project,
// which catches a mistyped project without refusing a new one.
func (c *client) noticeNewProject(t target, project string) error {
	result, err := c.project(t, project)
	if err != nil {
		return err
	}
	for _, existing := range result.Projects {
		if existing.Name == project && *existing.Entries == 1 {
			fmt.Printf("This is the first entry in project %q. Run '%s "+
				"list-projects' if you meant an existing one.\n", project,
				programName)
		}
	}
	return nil
}

// addTarget is where add puts a new entry: in the workspace
// --workspace names, or else in its parent's, since a parent is always
// in its child's workspace, or else in the default workspace.
func (c *client) addTarget(p *parsedArgs) (target, error) {
	if p.strs["--workspace"] == "" && p.has("--parent") {
		parent, err := api.ParseRef(p.strs["--parent"])
		if err != nil {
			return target{}, err
		}
		return c.resolve(parent)
	}
	workspace, err := c.config.chooseWorkspace(p.strs["--workspace"])
	if err != nil {
		return target{}, err
	}
	return targetOf(workspace), nil
}

func cmdAdd(c *client, args []string) error {
	p, err := parseArgs(args, flagSpec{
		strs:  []string{"--project", "--body", "--parent", "--workspace"},
		bools: []string{"--no-recent"},
		repeat: append([]string{"--meta", "--supersedes", "--link"},
			shorthandFlags()...),
	})
	if err != nil {
		return err
	}
	if len(p.pos) != 2 {
		return usageError("add <type> <subject> --project <p> " +
			"[--body <b>] [--meta <key>=<value>]... [--parent <ref>] " +
			"[--supersedes <ref>]... [--link <name>=<ref>]... [--no-recent] " +
			"[--workspace <name>]")
	}
	metadata, err := collectMetadata(p)
	if err != nil {
		return err
	}
	if p.strs["--project"] == "" {
		return fmt.Errorf("add needs --project <name>. Run '%s "+
			"list-projects' to see the projects in the workspace",
			programName)
	}
	if p.has("--parent") && p.strs["--parent"] == "" {
		return fmt.Errorf("--parent needs an entry id")
	}
	t, err := c.addTarget(p)
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
	request := api.AddEntryRequest{
		Type:     p.pos[0],
		Project:  p.strs["--project"],
		Subject:  p.pos[1],
		Body:     p.strs["--body"],
		Metadata: metadata,
		Origin:   origin,
		Links:    links,
	}
	if err := request.Validate(); err != nil {
		return err
	}

	var entry api.Entry
	err = c.do(t, "POST", workspacePath(t, "/entries"), request, &entry)
	if err != nil {
		return uncertainWrite(err, "The entry",
			programName+" recent --flat --workspace "+t.workspace)
	}
	fmt.Printf("Added entry [%s].\n", entry.Ref)
	if err := c.noticeNewProject(t, request.Project); err != nil {
		return fmt.Errorf("the entry was added, but listing projects "+
			"failed: %w", err)
	}
	if p.bools["--no-recent"] {
		return nil
	}
	if err := c.printRecentAfterChange(t, entry.Ref); err != nil {
		return fmt.Errorf("the entry was added, but listing recent "+
			"entries failed: %w", err)
	}
	return nil
}

func cmdEdit(c *client, args []string) error {
	p, err := parseArgs(args, flagSpec{
		strs: []string{"--type", "--project", "--subject", "--body",
			"--parent"},
		bools: []string{"--no-recent"},
		repeat: append(
			[]string{"--meta", "--supersedes", "--link", "--unlink"},
			shorthandFlags()...),
	})
	if err != nil {
		return err
	}
	if len(p.pos) < 1 || len(p.pos) > 2 {
		return usageError("edit <ref> [<subject>] [--type <t>] " +
			"[--project <p>] [--body <b>] [--meta <key>=<value>]... " +
			"[--parent <ref>|\"\"] [--supersedes <ref>]... " +
			"[--link <name>=<ref>]... [--unlink <name>[=<ref>]]... " +
			"[--no-recent]")
	}
	ref, err := api.ParseRef(p.pos[0])
	if err != nil {
		return err
	}

	request := api.EditEntryRequest{}
	if p.has("--type") {
		entryType := p.strs["--type"]
		request.Type = &entryType
	}
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
	if err := request.Validate(); err != nil {
		return err
	}
	var entry api.Entry
	if err := c.do(t, "PATCH", entryPath(ref, ""), request, &entry); err != nil {
		return uncertainWrite(err, "The edit", programName+" entry "+
			ref.String())
	}
	fmt.Printf("Edited entry [%s].\n", entry.Ref)
	if p.bools["--no-recent"] {
		return nil
	}
	if err := c.printRecentAfterChange(t, entry.Ref); err != nil {
		return fmt.Errorf("the entry was edited, but listing recent "+
			"entries failed: %w", err)
	}
	return nil
}

func cmdEntry(c *client, args []string) error {
	if len(args) != 1 {
		return usageError("entry <ref>")
	}
	ref, err := api.ParseRef(args[0])
	if err != nil {
		return err
	}
	t, err := c.resolve(ref)
	if err != nil {
		return err
	}
	var detail api.EntryDetail
	if err := c.do(t, "GET", entryPath(ref, ""), nil, &detail); err != nil {
		return err
	}
	printEntryDetail(detail)
	return nil
}

func cmdTree(c *client, args []string) error {
	p, err := parseArgs(args, flagSpec{
		strs:  []string{"--limit"},
		bools: []string{"--include-superseded"},
	})
	if err != nil {
		return err
	}
	if len(p.pos) != 1 {
		return usageError("tree <ref> [--limit <n>] [--include-superseded]")
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
	if p.bools["--include-superseded"] {
		query.Set("include-superseded", "true")
	}

	t, err := c.resolve(ref)
	if err != nil {
		return err
	}
	var tree api.TreeResponse
	path := entryPath(ref, "/tree?"+query.Encode())
	if err := c.do(t, "GET", path, nil, &tree); err != nil {
		return err
	}
	printTree(tree, ref)
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

func cmdSearch(c *client, command string, args []string) error {
	p, err := parseArgs(args, searchFlagSpec())
	if err != nil {
		return err
	}
	if len(p.pos) > 1 {
		return fmt.Errorf("give at most one <text> (got %d)", len(p.pos))
	}
	if command == "recent" && len(p.pos) == 1 {
		return fmt.Errorf("recent takes no <text>. Use '%s search <text>'",
			programName)
	}
	if command == "search" && p.has("--flat") {
		return fmt.Errorf("search always lists every level. --flat is for "+
			"'%s recent'", programName)
	}
	if p.has("--order-by-creation") && p.has("--order-by-modified") {
		return fmt.Errorf("give one of --order-by-creation and " +
			"--order-by-modified")
	}
	query, err := searchQuery(p)
	if err != nil {
		return err
	}
	if len(p.pos) == 1 {
		query.Add("content", p.pos[0])
	}
	limit := 20
	if p.has("--limit") {
		if limit, err = parsePositive("--limit", p.strs["--limit"]); err != nil {
			return err
		}
	}
	offset := 0
	if p.has("--offset") {
		if offset, err = parseOffset(p.strs["--offset"]); err != nil {
			return err
		}
	}
	query.Set("limit", fmt.Sprint(limit))
	query.Set("offset", fmt.Sprint(offset))
	if p.bools["--include-superseded"] {
		query.Set("include-superseded", "true")
	}
	if p.bools["--entries-only"] {
		query.Set("entries-only", "true")
	}
	order := byModified
	if p.bools["--order-by-creation"] {
		query.Set("order-by", "created")
		order = byCreation
	}
	if command == "recent" && !p.has("--flat") {
		if order == byCreation {
			return fmt.Errorf("--order-by-creation needs --flat, since " +
				"top-level entries are ordered by activity")
		}
		if p.bools["--entries-only"] {
			return fmt.Errorf("--entries-only needs --flat, since " +
				"top-level entries are listed without their chats")
		}
		query.Set("view", "topics")
		order = byActivity
	}

	workspace, err := c.config.chooseWorkspace(p.strs["--workspace"])
	if err != nil {
		return err
	}
	t := targetOf(workspace)
	if p.has("--under") {
		under, err := api.ParseRef(p.strs["--under"])
		if err != nil {
			return err
		}
		home, err := c.resolve(under)
		if err != nil {
			return err
		}
		if home.server != t.server {
			return fmt.Errorf("%s is on %s, but workspace %s is on %s. "+
				"Pass --workspace to search a workspace on %s", under,
				home.server, t.workspace, t.server, home.server)
		}
		query.Set("under", under.String())
	}
	result, err := c.search(t, query)
	if err != nil {
		return err
	}
	printSearchResult(result, offset, limit, order, p.bools["--id-only"])
	return nil
}

func cmdListProjects(c *client, args []string) error {
	p, err := parseArgs(args, flagSpec{
		strs: []string{"--limit", "--workspace"},
	})
	if err != nil {
		return err
	}
	if len(p.pos) != 0 {
		return usageError("list-projects [--limit 30] [--workspace <name>]")
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
	result, err := c.projects(targetOf(workspace), limit)
	if err != nil {
		return err
	}
	if len(result.Projects) == 0 {
		fmt.Printf("Workspace %s has no projects yet.\n", workspace.name)
		return nil
	}
	printProjects(result)
	return nil
}
