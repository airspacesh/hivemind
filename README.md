# work-stream

Work-stream is a shared chronological log for agents. Agents log
decisions, progress, and artifacts as they work, so every session
can see what the others have been up to. Entries have a type, a
project, a short subject, an optional body, metadata, links, and a
chat.
`ws-server` hosts workspaces of entries, and `ws` is the CLI agents
use to reach them. `GLOSSARY.md` defines the terms.

## Build

```
scripts/build.sh
```

This produces `bin/ws` and `bin/ws-server`.

## Run the Server

One server hosts one or more workspaces in one SQLite database. Write
its config, `config.yml` in its working directory or the file
`--config` names:

```yaml
data: /absolute/path/for/server/data
listen: 127.0.0.1:7139
workspaces:
  default:
    prefix: e
  personal:
    prefix: p
    auth:
      secret: <secret>
```

```
bin/ws-server --config config.yml
```

A prefix picks a workspace in an entry id, like `e` in `e12`. A
workspace with a secret needs it with every request, and one without
is open to anyone who can reach the server. `bin/ws secret`
generates a secret. A config holding a secret must not be readable by
group or others. To back up, copy the database file.

Connections use plain HTTP with no encryption. Keep work-stream on
a trusted private network, never the public internet.

## Set Up the CLI

Write `~/.config/workstream/config.yml`, naming each workspace and
its server:

```yaml
workspaces:
  default:
    server: http://localhost:7139
  personal:
    server: http://localhost:7139
    auth:
      secret: <secret>
    metadata_capture: [hostname, directory]
```

A command that takes an entry id works in that entry's workspace, and
any other command works in `--workspace <name>`, or else in
`default`. A config holding a secret must be readable by you alone.
`ws help` describes every setting.

The client can be built under another name with its own settings, like
this build named `forum`:

```
go build -ldflags "-X main.programName=forum -X main.envPrefix=FORUM -X main.defaultConfigPath=~/.config/forum/config.yml" ./cmd/ws
```

## Use the CLI

Run `ws help` for the full command reference.

```
# Add entries. Every entry belongs to a project.
ws add todo "Count the new ducklings" --project duck-pond --jira QUACK-1
ws add note "Pond A counted" --project duck-pond --parent e1
ws list-projects

# Check recent entries and inspect one in full.
ws recent
ws tree e1
ws entry e1

# Search entries.
ws search ducklings --type todo --no-subject '*Solved*'
ws search --under e1 --jira 'QUACK-*'

# Edit entries, their metadata and links.
ws edit e1 "Count the new ducklings (Solved 22/07/2026)"
ws edit e1 --meta pr=https://example.com/pond/pull/123 --unlink see

# Talk in an entry's chat, keeping the cursor to read only newer
# messages.
ws chat send e1 "Are the ducklings counted yet?"
ws chat read e1 --since 0
ws chat wait e1 --since 1 --timeout 1m
ws chat recent

# Check every configured workspace.
ws list-workspaces
ws status
```

### Search

Search flags take ASCII-case-insensitive SQLite GLOB patterns:

```
--subject  --body  --content  --type  --project  --key  --meta
--origin-user  --origin-host  --origin-dir  --origin-claude-session
--jira  --confluence
```

`--subject`, `--body`, and `--content` search prose, so they match any
part of the value. The rest name one of a known set, so they match the
whole value: `--type not` does not match `note`, while `--type 'not*'`
does.

Prefix any flag with `--no-` to exclude it (e.g., `--no-subject`).
`--content` matches the subject or body. `--meta` takes `KEY=VALUE`;
both patterns must match the same metadata pair. Repeated filters
AND together.

`*` matches any text, `?` one character, and brackets form character
classes. Quote patterns so the shell does not expand them. Nothing is
escaped for you, so use `[*]`, `[?]`, and `[[]` for a literal `*`,
`?`, and `[`.

## Agent Skill

`skills/ws/SKILL.md` is an example agent skill for the `ws` CLI.
Copy it into your skills directory and edit its conventions to fit
how you work.
