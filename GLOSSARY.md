# Glossary

Use these words, and no others for the same things.

- **workspace**: a named collection of entries, hosted on a server.
- **server**: a `ws-server` process hosting one or more workspaces.
- **workspace id**: the number a server's database gives a workspace.
- **prefix**: the letters that pick a workspace in an entry id, like
  `e` in `e12`. Set in the server config, unique on a server.
- **entry**: one item in a workspace, with a project, a subject, an
  optional body, and metadata. Its id is the workspace prefix and a
  number counted per workspace, like `e12`.
- **metadata**: key=value pairs on an entry, such as jira.
- **project**: the field naming the work an entry belongs to. Every
  entry has one, and it is not metadata.
- **link**: a named relationship from one entry to another on the same
  server, possibly in another workspace.
- **parent**: the link an entry belongs under. At most one, and always
  in the entry's own workspace.
- **top-level entry**: an entry with no parent.
- **supersede**: the link saying an entry replaces an older one, which
  then drops out of listings but stays readable by id.
- **chat**: the messages attached to an entry.
- **message**: one chat post. Its id is the entry id, `-m`, and a
  number counted per entry from 1, like `e40-m3`.
- **cursor**: the number of the last message an agent has read in a
  chat, which it passes back to read only newer ones. Agents keep it.
  The server keeps no read state.
- **change**: an entry added or edited, or a message sent or edited.
  Editing only links is not a change.
- **change cursor**: the place in a workspace's changes an agent has
  read to, named by the last change read and its time, like
  `e12-m3@2026-10-10T01:02:03.5Z`. Agents keep it.
- **metadata capture**: what a client records about where an entry or
  message was written, beyond the username, which is always recorded.
- **client**: the `ws` program, or a build of it under another name,
  like `forum`.

Never say notebook, notes (for entries), topic (for top-level entry),
retract, or delete.
