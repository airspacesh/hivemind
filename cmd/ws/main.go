// ws is the client of ws-server, which hosts workspaces of posts with
// metadata, links, and a chat on each post.
package main

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/airspacesh/workstream/internal/api"
	"github.com/airspacesh/workstream/internal/version"
)

// The build settings. A build under another name sets them with
// -ldflags "-X main.programName=forum -X main.envPrefix=... -X
// main.defaultConfigPath=...". The linker sets only package-level
// string variables with constant initializers, and silently ignores
// -X for anything else, so these must stay that way.
var (
	programName       = "ws"
	envPrefix         = "WORKSTREAM"
	defaultConfigPath = "~/.config/workstream/config.yml"
)

const usage = `{program} - a client for workstream: shared workspaces of posts

Usage:
  {program} add <subject> --project <p> [--body <b>]
         [--meta <key>=<value>]... [--parent <ref>] [--supersedes <ref>]...
         [--link <name>=<ref>]... [--private] [--workspace <name>]
  {program} edit <ref> [<subject>] [--project <p>] [--body <b>]
         [--meta <key>=<value>]... [--parent <ref>|""]
         [--supersedes <ref>]... [--link <name>=<ref>]...
         [--unlink <name>[=<ref>]]... [--private|--public]
  {program} delete <ref>
  {program} post <ref>|<ref>-m<n>
  {program} tree <ref> [--limit 30] [--include-superseded]
         [--include-deleted]
  {program} recent [--include-chat|--exclude-chat]
         [--include-posts|--exclude-posts] [--include-superseded]
         [--include-deleted] [--under <ref>] [search filters]
         [--workspace <name>]
  {program} search [<text>] [--include-chat|--exclude-chat]
         [--include-posts|--exclude-posts] [--include-superseded]
         [--under <ref>] [search filters] [--workspace <name>]
  {program} list-workspaces
  {program} list-projects [--limit 30] [--include-deleted]
         [--workspace <name>]
  {program} chat send <ref> <message> [--after <ref>-m<n>|<time>]
  {program} chat recent [<ref>|--workspace <name>] [--limit 20]
         [--include-deleted]
  {program} chat read <ref> [--after <ref>-m<n>|<time>] [--limit 20]
         [--include-deleted]
  {program} chat wait <ref> [--after <ref>-m<n>|<time>] [--timeout 10m]
         [--content <pattern>]... [--origin-user <pattern>]...
         [--exclude-mine|--include-mine]
  {program} chat edit <ref>-m<n> <message>
  {program} chat delete <ref>-m<n>
  {program} wait [--after @<revision>|<id>] [--timeout 10m]
         [--include-chat|--exclude-chat] [--include-posts|--exclude-posts]
         [--exclude-mine|--include-mine] [--under <ref>] [search filters]
         [--workspace <name>]
  {program} firehose [--after @<revision>|<id>] [--timeout <d>]
         [--include-chat|--exclude-chat] [--include-posts|--exclude-posts]
         [--exclude-mine|--include-mine] [--under <ref>] [search filters]
         [--workspace <name>]
  {program} status
  {program} secret
  {program} help
  {program} --version

Workspaces and posts:
  A workspace is a named collection of posts, hosted on a server. A post
  has a project, a short subject, an optional longer body, and metadata,
  which is key=value pairs such as jira. Every post needs a project. A
  post's id is its workspace's prefix and a number, like e12, and is
  called <ref> above. Keep the subject to a headline and put detail in
  the body. Listings show only the subject, and '{program} post' shows
  the rest.

  A command that takes a <ref> works in that post's workspace. With
  --parent, add works in the parent's, and with --under, recent, search,
  wait and firehose work in that post's, since a tree never leaves its
  workspace. Any other command works in the workspace --workspace names,
  or else in the one the variable {DEFAULT_WORKSPACE} names, or
  else in the one called general. '{program} list-workspaces' shows them
  all.

  Each post and message records the username, and as far as the
  workspace's metadata_capture allows, the host, directory and Claude
  Code session it was written from. '{program} post' shows it, and the
  origin-* filters search it.

  Metadata keys are lowercase slugs: letters, digits, and single
  dashes, starting with a letter (e.g. jira, build-id). For several of
  a kind, number them: jira-1, jira-2. The key project is reserved,
  since the project is a field of its own. Field limits (characters):
  project 256, subject 72 plus an optional suffix of up to 10 letters
  like " (question)", body 2048, chat message 512, key 64, value 256,
  and 16 metadata pairs.

Editing and deleting:
  '{program} edit' changes the project, subject, body, metadata and
  links. --body "" clears the body. Giving a metadata key no value
  removes it, as in --meta jira= or --jira "". The project can change
  but not be removed. A post's id, times and origin cannot change.

  '{program} delete <ref>' deletes a post. Its subject, body and
  metadata are removed for good. Its id, project, links, origin and chat
  stay, and the posts under it stay in place. A message deleted with
  '{program} chat delete <ref>-m<n>' loses its text for good, and keeps
  its number and place. Deleting again changes nothing. A deleted post
  or message cannot be edited.

  Deleted posts and messages drop out of every listing, but the posts
  under a deleted post and the messages in its chat stay. A tree keeps a
  deleted post only above the posts under it. list-projects leaves out
  a project whose posts are all deleted. --include-deleted shows them
  all, as (deleted). '{program} post' shows one by its id, and wait and
  firehose show a delete as a change.

  Only the user who wrote a post or message can edit or delete it. To
  suggest a change to someone else's, send a message in its chat.

Private posts:
  'add --private' adds a post only you can read, and 'edit <ref>
  --private' makes one of yours private. The posts and chat under a
  private post are private to you too. Everyone else gets "post not
  found" for all of it, and no listing, tree, count or wait shows it to
  them. Its lines show [private] to you. 'edit <ref> --public' shows it
  again, with the posts under it that you did not make private yourself,
  and the post moves to the top of recent and wakes waits, as an edit
  does. A post that others have posts or messages under cannot be made
  private or moved under a private post, unless they deleted them. The
  server takes your client's word for who you are, so a private post is
  out of others' way but not secret. Keep secrets out of workstream.

Failed writes:
  add and chat send give their write a random request id, and the server
  makes at most one post for an id in a workspace, and one message for
  an id in a chat. When the request times out or finds the server busy,
  they send it again under the same id, after waits of 1, 2, 4 and then
  8 seconds, for up to a minute. A write that still fails may have taken
  effect, and the error gives the command that repeats it with
  --request-id <id>, which cannot make a second copy. An id already used
  for other content is refused. An edit or delete is never sent again.
  Doing one twice leaves the same result, so a failed one is safe to run
  again.

Links:
  A link is a named relationship from one post to another on the same
  server, possibly in another workspace, such as --link depends-on=e40.
  --parent <ref>      the post this one belongs under, in the same
                      workspace. A post has at most one parent.
                      --parent "" on edit removes it.
  --supersedes <ref>  this post replaces that one, which then drops out
                      of listings unless they pass --include-superseded.
                      '{program} post' still shows it.
  Neither may loop. --unlink depends-on=e40 removes one link, and
  --unlink depends-on every link of that name. Editing only links
  leaves the modified time alone.

Listings:
  '{program} recent' lists the newest posts that match its filters, at
  every level, a line each with who wrote it and the path to it, like
  [e44] 07:45 ines in e1.e34: subject. A post ranks by the latest change
  to it or its chat, and shows how many messages its chat gained since
  the oldest match listed, like (2 new messages).
  With --include-chat, recent lists the newest messages as well, like
  [e44-m3] 07:50 omar in e1.e34.e44: text, and a post ranks by its own
  latest change. The path after "in" always ends at the post a line sits
  in, a post's parent or a message's own post. '{program} search' lists
  the same way, with chat unless --exclude-chat. Posts and messages
  share --limit. A listing says what it ran on stderr. When older
  matches may remain, a 'Next:' line gives the command that lists them,
  which repeats the flags with --before set to the revision of the
  oldest match shown. Recent ends with a 'Cursor:' line giving the
  revision it read at, like 'Cursor: @812345', which a wait takes.
  --under <ref> keeps recent or search to a post and what is beneath
  it.
  '{program} tree <ref>' shows the path to a post, then its subtree, at
  most --limit posts, marking where it was cut. A superseded post shows
  only above the posts under it, unless --include-superseded shows it.
  '{program} list-projects' lists the --limit most recently active
  projects, each with the number of its posts created in the last 90
  days, and their share of the workspace's. When the limit leaves some
  out, a 'Next:' line gives the command that lists them all. Every
  command that takes --limit <n> takes -n <n> as its short form. Times
  show as 13:20 when an output's times all fall on one day, or else as
  23/07/2025 13:20.

Search filters (recent, search, wait and firehose):
  --subject --body --content --project --key --meta <key>=<value>
  --origin-user --origin-host --origin-dir --origin-claude-session
  --jira --confluence (shorthands for --meta <key>=<value>)
  Each takes an ASCII-case-insensitive GLOB pattern: '*', '?' and
  bracket classes. Quote patterns so the shell leaves them alone, and
  use a bracket class like [*] for a literal wildcard. --subject, --body
  and --content (subject or body, also a positional <text>) match any
  part of the text. The others match the whole value, so --project
  duck does not match duck-pond. An empty pattern matches only an
  empty value. Prefix a filter with no- to exclude, as in --no-project.
  Filters AND together.
  In recent and search, --after <bound> keeps to what last changed at or
  after a time, or after a revision, and --before <bound> to what last
  changed before it. A post last changed at the change it ranks by, and
  a message when it was posted. A bound is a revision like @812345, a
  date like 2026-10-09 or a date and time like 2026-10-09T14:30, both in
  local time, a time with its offset like 2026-10-09T14:30:00+08:00, or
  a span back from now like 2d, 6h or 90m, counted to a whole second. A
  listing's 'Next:' line writes --before as a revision, and any time
  bound as a time with its offset.
  '{program} search', and '{program} recent --include-chat', match each
  message in a post's chat as well as the posts. A message matches when
  it meets the text and origin filters itself, excluding ones included,
  and its post meets the others. A post matches when it meets every
  filter itself. So 'search hook --origin-user ines --project build'
  finds the messages ines sent that mention hook in the chats of build
  posts, and the build posts by ines that mention hook, and 'search
  --origin-user ines --after 2d' lists what ines posted in the last 2
  days, and the posts she created that changed in them.
  --limit <n>, -n <n>    matches to show (default 20)
  --id-only              print one post or message id per line, and
                         the 'Next:' and 'Cursor:' lines on stderr

Inclusion flags:
  Each shows something a command leaves out by default, or leaves out
  something it shows. A command takes at most one flag of a pair.
  --include-chat, --exclude-chat
      the messages in recent, search, wait and firehose. recent leaves
      them out by default, and the others show them.
  --include-posts, --exclude-posts
      the posts in recent, search, wait and firehose, which show them by
      default. A command left to show neither posts nor messages refuses
      to run.
  --include-deleted
      deleted posts and messages, as (deleted), in recent, tree, chat
      recent, chat read and list-projects. search never shows them.
  --include-superseded
      superseded posts, in recent, search and tree.
  --exclude-mine, --include-mine
      your own changes and messages in wait, firehose and chat wait.
      The waits leave them out by default, and firehose shows them. Chat
      says which are yours. A lead whose sub-agents share its session
      waits with --include-mine.

Chat:
  Every post has a chat. A message's id is the post id, -m, and a number
  counted from 1 in that post, like e40-m3. Messages stay in the order
  they were sent, and an edit overwrites one in place. Your cursor is
  the id of the last message you read, like e40-m3. Pass it as --after
  to read only later messages, and keep it, since the server keeps no
  read state. --after e40-m0 starts before the first message. --after
  also takes a time as in recent, like 2h, for the messages posted at or
  after it. A read's cursor is the last message it shows, or else its
  last line names it, as 'Cursor: e40-m3' does. A wait takes it too.
  '{program} chat send' sends a message and prints its id. With --after,
  it then shows the messages after --after, yours among them. It shows
  the newest 20 at most, and says how to read any it left out.
  '{program} chat recent <ref>' shows the newest messages, oldest first.
  '{program} chat recent' alone shows the newest messages across the
  workspace, under their posts, and ends with the revision it read at,
  as recent does. Both say how to read the earlier messages they leave
  out, as a send does.
  '{program} post <ref>-m<n>' shows one message under its post's line,
  with when and where it was written, as '{program} post' shows a post.
  '{program} chat read' shows the messages after --after, or from the
  start with no --after, a page at a time, and says how to read the
  next page.
  '{program} chat wait' returns as soon as a message arrives after
  --after that matches its filters and is not yours, and prints only
  the messages that match, then the cursor. With no --after it waits
  for the next message. --content and the origin filters, and their
  no- forms, match each message itself, as in a search. The cursor
  passes every message the wait checked, matching or not, so wait on
  from it.
  A message is yours when both its user and its Claude Code session are
  yours, so a message from another session of your user wakes a wait.
  Outside a session, or when the workspace does not capture the session,
  it is yours when it came from your user outside any session.
  The wait polls every second for 10 seconds and then every 5. It
  retries a failed connection with backoff, up to 6 times in a row.

Changes:
  A change is a post added, edited or deleted, or a message sent, edited
  or deleted. Editing only links is not a change. Each change takes the
  next revision, a number counted across the whole server. A change
  cursor is the revision of the last change read, like @812345. Pass it
  as --after to read only later changes, and keep it, since the server
  keeps no read state. --after @0 starts before every change, and with
  no --after a command starts from now. --after also takes the id of a
  post or message, like e12 or e40-m3, for the changes after it was
  added or sent, in its workspace unless --workspace or --under names
  another. It was there before you saw it, so a wait from it misses
  nothing, but may show changes you have seen. --after takes no time,
  since changes are read by revision. A post or message changed again
  after your cursor shows once, at its latest change, with its current
  text. To start a wait from what you last saw rather than from now,
  pass the last cursor or id your last command printed, like the
  'Cursor: @812345' that recent and wait end with, or the id a write
  prints, as in 'Added post [e12].'.
  '{program} wait' returns as soon as a change after --after matches its
  filters and is not yours, and prints only the changes that match, then
  the cursor. The search filters and --under match a post's change
  against the post, and a message's change as in a search, so the text
  and origin filters match the message itself and the others its post.
  A change is yours as in chat wait, by the origin of the post or
  message. An edit or delete keeps the origin, so it counts as a change
  by the session that added the post or sent the message. The cursor
  passes every change the wait checked, matching or not, so wait on from
  it. It polls as chat wait does.
  '{program} firehose' prints every change that matches its filters as
  it arrives, each batch followed by its cursor, until interrupted or
  --timeout passes.

Config:
  {program} reads the file {CONFIG_ENV} names, or else
  {CONFIG}:

    workspaces:
      general:
        server: http://localhost:7139
        prefix: g                    # optional, saves asking servers
        auth:
          secret: <secret>           # optional, when the server needs it
        metadata_capture: all        # all (default), none, or a list of
                                     # hostname, directory and session
      personal:
        server: http://other-host:7140

  A config holding a secret must be readable by its owner alone. A
  workspace's secret goes only to its own server, for that workspace.
  For an id whose prefix no workspace sets, {program} asks each
  configured server in turn which prefixes it hosts. '{program} status'
  checks every configured workspace, and '{program} secret' generates a
  secret. This variable sets the request timeout (default 5s):
  {TIMEOUT_ENV}.
`

// usageText is the help, naming this program and its settings.
func usageText() string {
	return strings.NewReplacer(
		"{program}", programName,
		"{CONFIG}", defaultConfigPath,
		"{CONFIG_ENV}", configEnvironment(),
		"{DEFAULT_WORKSPACE}", defaultWorkspaceEnvironment(),
		"{TIMEOUT_ENV}", timeoutEnvironment(),
	).Replace(usage)
}

func usageError(synopsis string) error {
	return fmt.Errorf("usage: %s %s", programName, synopsis)
}

func cmdSecret(args []string) error {
	if len(args) != 0 {
		return usageError("secret")
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return fmt.Errorf("generating a secret: %w", err)
	}
	fmt.Println(base64.RawURLEncoding.EncodeToString(secret))
	return nil
}

// cmdListWorkspaces shows the configured workspaces in config order,
// each with the prefix its server gives it.
func cmdListWorkspaces(c *client, args []string) error {
	if len(args) != 0 {
		return usageError("list-workspaces")
	}
	chosen := defaultWorkspace()
	failed := false
	for _, workspace := range c.config.workspaces {
		line := workspace.name
		if workspace.name == chosen {
			line += " (default)"
		}
		listed, err := c.listWorkspaces(workspace.server)
		prefix := workspace.prefix
		if err != nil && prefix == "" {
			prefix = "unknown"
		}
		if err == nil {
			prefix = "not hosted there"
			for _, hosted := range listed {
				if hosted.Name == workspace.name {
					prefix = hosted.Prefix
				}
			}
		}
		line += ": prefix " + prefix + " on " + workspace.server
		if workspace.secret != "" {
			line += ", with a secret"
		}
		if err != nil {
			line += fmt.Sprintf(" (the server cannot be asked: %v)", err)
			failed = true
		}
		fmt.Println(line)
	}
	if failed {
		return errors.New("some servers could not be asked")
	}
	return nil
}

// checkWorkspace checks one configured workspace: that its server is
// up, hosts it under the configured prefix, and lets this client read
// it.
func (c *client) checkWorkspace(workspace workspaceConfig) (string, error) {
	t := targetOf(workspace)
	var status api.StatusResponse
	if err := c.do(t, "GET", "/api/status", nil, &status); err != nil {
		return "", err
	}
	listed, err := c.listWorkspaces(workspace.server)
	if err != nil {
		return "", err
	}
	prefix := ""
	for _, hosted := range listed {
		if hosted.Name == workspace.name {
			prefix = hosted.Prefix
		}
	}
	if prefix == "" {
		return "", fmt.Errorf("%s hosts no workspace %s", workspace.server,
			workspace.name)
	}
	if workspace.prefix != "" && workspace.prefix != prefix {
		return "", fmt.Errorf("%s gives workspace %s prefix %s, but %s "+
			"sets prefix %s", workspace.server, workspace.name, prefix,
			c.config.path, workspace.prefix)
	}
	// Any read of the workspace proves access. The newest message is
	// the cheapest, since an index finds it.
	var overview api.ChatOverview
	err = c.do(t, "GET", workspacePath(t, "/messages?limit=1"), nil,
		&overview)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("prefix %s on %s (ws-server %s, database %s), "+
		"readable", prefix, workspace.server, status.Version,
		status.Database), nil
}

func cmdStatus(c *client, args []string) error {
	if len(args) != 0 {
		return usageError("status")
	}
	fmt.Println("config: " + c.config.path)
	fmt.Printf("timeout: %s\n", c.timeout)
	failed := false
	for _, workspace := range c.config.workspaces {
		summary, err := c.checkWorkspace(workspace)
		if err != nil {
			summary = "FAILED: " + err.Error()
			failed = true
		}
		fmt.Printf("%s: %s\n", workspace.name, summary)
	}
	if failed {
		return errors.New("some workspaces failed their check")
	}
	return nil
}

// run runs one command line, apart from the program name.
func run(args []string) error {
	if len(args) == 0 {
		fmt.Print(usageText())
		return nil
	}
	command, args := args[0], args[1:]
	switch command {
	case "help", "--help", "-h":
		fmt.Print(usageText())
		return nil
	case "--version":
		if len(args) != 0 {
			return usageError("--version")
		}
		fmt.Printf("%s %s\n", programName, version.Software)
		return nil
	case "secret":
		return cmdSecret(args)
	}

	path, err := configPath()
	if err != nil {
		return err
	}
	config, err := loadConfig(path)
	if errors.Is(err, errNoConfig) {
		return errors.New(missingConfigHelp(path))
	}
	if err != nil {
		return err
	}
	timeout, err := requestTimeout()
	if err != nil {
		return err
	}
	c := newClient(config, timeout)

	switch command {
	case "add":
		return cmdAdd(c, args)
	case "edit":
		return cmdEdit(c, args)
	case "delete":
		return cmdDelete(c, args)
	case "post":
		return cmdPost(c, args)
	case "tree":
		return cmdTree(c, args)
	case "recent", "search":
		return cmdListing(c, command, args)
	case "list-workspaces":
		return cmdListWorkspaces(c, args)
	case "list-projects":
		return cmdListProjects(c, args)
	case "chat":
		return cmdChat(c, args)
	case "wait":
		return cmdWait(c, args)
	case "firehose":
		return cmdFirehose(c, args)
	case "status":
		return cmdStatus(c, args)
	default:
		return fmt.Errorf("unknown command %q. Run '%s help'", command,
			programName)
	}
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", programName, err)
		os.Exit(1)
	}
}
