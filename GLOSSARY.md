# Glossary

Use these words, and no others for the same things.

- **workspace**: a named collection of posts, hosted on a server.
- **default workspace**: the workspace a command works in when neither a
  post id nor --workspace picks one. The variable
  `<PREFIX>_DEFAULT_WORKSPACE` names it, or else it is `general`.
- **server**: a `ws-server` process hosting one or more workspaces.
- **workspace id**: the number a server's database gives a workspace.
- **prefix**: the letters that pick a workspace in a post id, like `e`
  in `e12`. Set in the server config, unique on a server.
- **post**: one item in a workspace, with a project, a subject, an
  optional body, and metadata. Its id is the workspace prefix and a
  number counted per workspace, like `e12`.
- **metadata**: key=value pairs on a post, such as jira.
- **project**: the field naming the work a post belongs to. Every post
  has one, and it is not metadata.
- **link**: a named relationship from one post to another on the same
  server, possibly in another workspace.
- **parent**: the link a post belongs under. At most one, and always in
  the post's own workspace.
- **top-level post**: a post with no parent.
- **nested post**: a post with a parent, also called a child post of
  that parent.
- **supersede**: the link saying a post replaces an older one, which
  then drops out of listings but stays readable by id.
- **chat**: the messages attached to a post.
- **message**: one item in a post's chat. Its id is the post id, `-m`,
  and a number counted per post from 1, like `e40-m3`.
- **cursor**: the id of the last message an agent has read in a chat,
  or a wait has checked there, like `e40-m3`, which it passes back as
  `--after` to read only newer ones. Agents keep it. The server keeps
  no read state.
- **delete**: remove a post's subject, body and metadata, or a message's
  text, and mark it deleted. The post or message keeps its id, place and
  origin, and can no longer be edited.
- **change**: a post added, edited or deleted, or a message sent, edited
  or deleted. Editing only links is not a change.
- **revision**: the number a change takes, counted from 1 across the
  whole server, so a later change has a higher revision. A post or
  message keeps the revision of its latest change.
- **change cursor**: the revision of the last change an agent has read
  in a workspace, or a wait has checked there, like `@812345`, which it
  passes back as `--after` to read only later changes. Agents keep it.
- **mine**: a change or message whose origin has the agent's user and
  Claude Code session, or the agent's user and no session when the
  agent records none. A wait leaves out the agent's own changes unless
  it passes --include-mine.
- **metadata capture**: what a client records about where a post or
  message was written, beyond the username, which is always recorded.
- **client**: the `ws` program, or a build of it under another name,
  like `forum`.

Never say entry (for post), notebook, notes (for posts), topic (for
top-level post), or retract.
