// ws is the client of ws-server, which hosts workspaces of entries
// with metadata, links, and a chat on each entry.
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

const usage = `{program} - a client for workstream: shared workspaces of entries

Usage:
  {program} add <subject> --project <p> [--body <b>]
         [--meta <key>=<value>]... [--parent <ref>] [--supersedes <ref>]...
         [--link <name>=<ref>]... [--no-recent] [--workspace <name>]
  {program} edit <ref> [<subject>] [--project <p>] [--body <b>]
         [--meta <key>=<value>]... [--parent <ref>|""]
         [--supersedes <ref>]... [--link <name>=<ref>]...
         [--unlink <name>[=<ref>]]... [--no-recent]
  {program} entry <ref>
  {program} tree <ref> [--limit 30] [--include-superseded]
  {program} recent [--flat] [search filters] [--workspace <name>]
  {program} search [<text>] [--under <ref>] [search filters] [--workspace <name>]
  {program} list-workspaces
  {program} list-projects [--limit 30] [--workspace <name>]
  {program} chat send <ref> <message> [--since <n>]
  {program} chat recent [<ref>] [--limit 20] [--workspace <name>]
  {program} chat read <ref> --since <n> [--limit 20]
  {program} chat wait <ref> --since <n> [--timeout 10m]
         [--content <pattern>]... [--exclude-mine|--include-mine]
  {program} chat edit <ref>-m<n> <message>
  {program} wait [--since <cursor>] [--timeout 10m] [--under <ref>]
         [search filters] [--exclude-mine|--include-mine]
         [--workspace <name>]
  {program} firehose [--since <cursor>] [--timeout <d>] [--under <ref>]
         [search filters] [--exclude-mine|--include-mine]
         [--workspace <name>]
  {program} status
  {program} secret
  {program} help
  {program} --version

Workspaces and entries:
  A workspace is a named collection of entries, hosted on a server. An
  entry has a project, a short subject, an optional longer body, and
  metadata: key=value pairs such as jira. Every entry needs a project.
  An entry's id is its workspace's prefix and a number, like e12, and
  is called <ref> above. Keep the subject to a headline and put detail
  in the body. Listings show only the subject, and '{program} entry'
  shows the rest.

  A command that takes a <ref> works in that entry's workspace. With
  --parent, add works in the parent's, and with --under, recent and
  search work in that entry's, unless --workspace names another. Any
  other command works in the workspace --workspace names, or else in
  the one the variable {DEFAULT_WORKSPACE} names, or else in the one
  called default. '{program} list-workspaces' shows them all.

  Each entry and message records the username, and as far as the
  workspace's metadata_capture allows, the host, directory and Claude
  Code session it was written from. '{program} entry' shows it, and
  the origin-* filters search it.

  Metadata keys are lowercase slugs: letters, digits, and single
  dashes, starting with a letter (e.g. jira, build-id). For several of
  a kind, number them: jira-1, jira-2. The key project is reserved,
  since the project is a field of its own. Field limits (characters):
  project 256, subject 128, body 2048, key 64, value 256, and 16
  metadata pairs. Every entry is kept. To withdraw one, edit it, for
  example to "Retracted: sensitive information".

Editing:
  '{program} edit' changes the project, subject, body, metadata and
  links. --body "" clears the body. Giving a metadata key no value
  removes it, as in --meta jira= or --jira "". The project can change
  but not be removed. An entry's id, times and origin cannot change.
  add and edit end by listing the 5 most recently changed other
  entries in the workspace, so you see what others are doing.
  --no-recent leaves the listing out.

  Only the user who posted an entry or message can edit it. To suggest
  a change to someone else's, send a message in its chat.

Links:
  A link is a named relationship from one entry to another on the same
  server, possibly in another workspace, such as --link depends-on=e40.
  --parent <ref>      the entry this one belongs under, in the same
                      workspace. An entry has at most one parent.
                      --parent "" on edit removes it.
  --supersedes <ref>  this entry replaces that one, which then drops out
                      of listings unless they pass --include-superseded.
                      '{program} entry' still shows it.
  Neither may loop. --unlink depends-on=e40 removes one link, and
  --unlink depends-on every link of that name. Editing only links
  leaves the modified time alone.

Listings:
  '{program} recent' lists top-level entries, those with no parent,
  ordered by the latest change anywhere beneath them, with a count of
  what is below and the 2 most recently sent messages anywhere in the
  tree. '{program} recent --flat' lists every entry by its own last
  change. '{program} search' lists every level, with the path to each
  match. --under <ref> keeps a search to an entry and what is beneath
  it. '{program} tree <ref>' shows the path to an entry, then its
  subtree, at most --limit entries, marking where it was cut.
  '{program} list-projects' lists the --limit most recently active
  projects, each with the number of its entries created in the last 90
  days, and their share of the workspace's. Times show as 13:20 when an
  output's times all fall on one day, or else as 23/07/2025 13:20.

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
  '{program} search' and '{program} recent --flat' also match <text> and
  --content against each message in an entry's chat, while the other
  filters still apply to the entry. An entry ranks by its newest match.
  A page shows the 30 most recently sent matching messages beneath their
  entries, and counts each entry's other matching messages.
  --limit <n>, -n <n>    entries to show (default 20)
  --offset <n>           skip the first n matches
  --order-by-modified    newest first by modification time (default)
  --order-by-creation    newest first by creation time (with --flat)
  --id-only              print one entry id per line
  --include-superseded   include superseded entries
  --entries-only         leave chats out of a search for text

Chat:
  Every entry has a chat. A message's id is the entry id, -m, and a
  number counted from 1 in that entry, like e40-m3. Messages stay in
  the order they were sent, and an edit overwrites one in place. Your
  cursor is the number of the last message you read. Keep it, since
  the server keeps no read state.
  '{program} chat send' sends a message and prints its id. With --since,
  it then shows the messages after --since, yours among them, and the
  cursor. It shows the newest 20 at most, and says how to read any it
  left out.
  '{program} chat recent <ref>' shows the newest messages, oldest first,
  and the cursor. '{program} chat recent' alone shows the newest messages
  across the workspace, under their entries.
  '{program} chat read' shows the messages after --since a page at a
  time, and says how to read the next page.
  '{program} chat wait' returns as soon as a message arrives after
  --since that matches its filters and is not yours, and prints only
  the messages that match, then the cursor. --content and --no-content
  match each message's text, as in a search. The cursor passes every
  message the wait checked, matching or not, so wait on from it.
  A message is yours when it came from your user in your current Claude
  Code session. Outside a session, or when the workspace does not
  capture the session, it is yours when it came from your user outside
  any session. --include-mine wakes on yours too, as a lead whose
  sub-agents share its session needs.
  The wait polls every second for 10 seconds and then every 5. It
  retries a failed connection with backoff, up to 6 times in a row.
  A send or edit whose outcome is uncertain, like one that timed out,
  is never retried. Check the chat before sending again.

Changes:
  A change is an entry added or edited, or a message sent or edited.
  Editing only links is not a change. Each change takes the next
  revision, a number counted across the whole server. A change cursor
  is the revision of the last change read. Pass it as --since to read
  only later changes, and keep it, since the server keeps no read
  state. --since 0 starts before every change, and with no --since a
  command starts from now. An entry or message changed again after your
  cursor shows once, at its latest change, with its current text.
  '{program} wait' returns as soon as a change after --since matches its
  filters and is not yours, and prints only the changes that match,
  then the cursor. The search filters and --under match an entry's
  change against the entry, and a message's change against its entry,
  except that --content matches the message's own text. A change is
  yours as in chat wait, by the origin of the entry or message. An edit
  keeps the origin, so it counts as a change by the session that added
  the entry or sent the message. The cursor passes every change the
  wait checked, matching or not, so wait on from it. It polls as chat
  wait does.
  '{program} firehose' prints every change that matches its filters as
  it arrives, each batch followed by its cursor, until interrupted or
  --timeout passes. It shows your own changes unless --exclude-mine
  leaves them out.

Config:
  {program} reads the file {CONFIG_ENV} names, or else
  {CONFIG}:

    workspaces:
      default:
        server: http://localhost:7139
        prefix: e                    # optional, saves asking servers
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
	case "entry":
		return cmdEntry(c, args)
	case "tree":
		return cmdTree(c, args)
	case "recent", "search":
		return cmdSearch(c, command, args)
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
