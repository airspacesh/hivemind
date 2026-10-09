---
name: ws
description: "Record and search the work stream: shared workspaces of typed entries, such as note, todo, decision, idea, and artifact, each with a chat. Use it to get broader context on recent work, such as when planning, to log decisions, useful progress, and artifacts another session may need, and to talk with other agents about an entry. Do not use the ws CLI without loading this skill first."
---

# Work Stream

`ws` is a CLI for shared workspaces of entries. Durable work lives in
your issue tracker, documentation, and pull requests. When you create
one of those artifacts, log an entry that links to it.

Run `ws help` for the full reference, and `ws list-workspaces` to see
the workspaces you can use.

## Workspaces and ids

An entry's id is its workspace's prefix and a number, like `e12` or
`p3`. A command given an id works in that entry's workspace. Any other
command works in `--workspace <name>`, or else the default workspace.

## Commands

### add

```
ws add <type> <subject> --project <p> [--body <b>] [--meta <key>=<value>]... [--jira <k>] [--confluence <u>] [--workspace <name>]
```

Adds an entry. `ws list-projects` lists the projects in the workspace,
and `ws add` says when an entry starts a new one. The username, and as
the workspace's metadata capture allows, the host, directory and
Claude session, are recorded automatically. The subject is a headline
of up to 128 characters. Longer detail goes in `--body`, up to 2048.

`add` and `edit` end by listing the 5 most recently changed other
entries, so you see what others have been doing. Pass `--no-recent`
to leave the listing out.

### links

`--parent <ref>` files an entry under the entry it follows up, and
`--supersedes <ref>` marks an older entry as replaced, which hides it
from listings. `--link <name>=<ref>` records any other relationship,
such as `--link depends-on=e40`. A parent is always in its child's
workspace, so `ws add --parent` puts the entry there. Other links can
reach another workspace on the same server. `ws edit --unlink <name>[=<ref>]` removes links, and
`--parent ""` removes the parent. `ws entry` shows links in both
directions.

### recent

```
ws recent [--flat] [-n <limit>] [--offset <n>] [--order-by-creation | --order-by-modified] [--workspace <name>]
```

Top-level entries, default 20: entries with no parent, ordered by the
latest change anywhere beneath them. `--flat` lists every entry by its
own last change, so an edited entry resurfaces. A listing shows the
subject alone. Open an entry with `ws entry` to see its body, metadata
and links.

### tree

```
ws tree <ref> [--limit 30] [--include-superseded]
```

The path to an entry, then its subtree. A cut tree marks where, as
`... 3 more below e20` or `... 6 more under e12`.

### search

```
ws search [<text>] [--under <ref>] [filters] [--id-only]
```

A bare `<text>` is the same as `--content`, so it matches any part of
the subject or body. `--under <ref>` keeps a search to an entry and
what is beneath it. `--id-only` prints an entry id per line.

### entry and edit

```
ws entry <ref>
ws edit <ref> [<subject>] [--type <t>] [--body <b>] [--meta <key>=<value>]... [--no-recent]
```

`ws entry` shows one entry in full. `ws edit` changes the type,
subject, body, metadata and links. `--body ""` clears the body, and a
metadata key given no value is removed, as in `--jira ""`. Every entry is
kept. To withdraw an entry, edit it to say why, for example
`ws edit e12 "Retracted: sensitive information" --body ""`.

### chat

```
ws chat send <ref> <message>
ws chat recent [<ref>] [--limit 20]
ws chat read <ref> --since <n> [--limit 20]
ws chat wait <ref> --since <n> [--timeout 10m]
ws chat edit <ref>-m<n> <message>
```

Every entry has a chat. Messages have ids like `e40-m3`. Keep your
cursor, the number of the last message you read, and pass it as
`--since` to read only newer messages. Every read, wait and
`chat recent <ref>` ends with the cursor, or with the command that
reads the next page. A send prints only the new message's id and
leaves your cursor alone. Keep reading and waiting from your last
read's cursor after you send, so you see what others sent in between,
and your own message too.

`ws chat wait` returns as soon as a message arrives. Run it in the
background most of the time. While actively talking with another
agent, wait in the foreground with a short timeout, like `--timeout 1m`.
`ws chat recent` with no id gives an overview of the newest messages
across the workspace.

Prefer sending a new message over editing an old one, since an edited
message keeps its place and others may have read it already. A send
whose outcome is uncertain is never retried. Check the chat before
sending again.

### status

```
ws status
```

Checks every configured workspace: its server, prefix and access.

### Search filters

Every filter takes an ASCII-case-insensitive GLOB pattern.

`--subject`, `--body`, and `--content` (subject or body) search prose,
so they match any part of the value: `--subject duck` finds "Count the
Ducklings". An empty pattern matches only an empty value, so
`--body ""` finds the entries with no body.

`--type`, `--key`, `--meta <key>=<value>`, the origin filters
`--origin-user`, `--origin-host`, `--origin-dir`,
`--origin-claude-session`, and the metadata shorthands `--project`,
`--jira`, `--confluence` each name one of a known set, so they match
the whole value: `--type not` does not match `note`, while
`--type 'not*'` does.

Prefix any flag with `--no-` to exclude (e.g., `--no-subject`).
Repeated filters AND together. Patterns use `*`, `?`, and bracket
classes. Quote them so the shell does not expand them. Nothing is
escaped for you, so to match a literal `*`, `?`, or `[`, put it in a
bracket class: `--content '*[*]args*'` to partial match `*args`.
Default limit is 20, maximum 500.

To find what your current Claude session logged:
`ws search --origin-claude-session "$CLAUDE_CODE_SESSION_ID"`.

## Conventions

Adapt these to how you work.

Types: `note`, `todo`, `decision`, `idea`, `artifact`, `status`.

`--project`: the repository or project the work belongs to, or a
representative name when there is no repository. Every entry needs
one. Pick an existing project when one fits.

`--jira`: the issue the entry relates to, e.g. `QUACK-1`.

`--confluence`: a related documentation page.

For any key without a shorthand, use `--meta <key>=<value>`. Number
repeats of a key as `jira-1`, `jira-2`.

Append `(Solved <date>)` to a finished todo's subject.
