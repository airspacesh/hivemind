// Package api defines the JSON types shared between the ws CLI and
// the ws-server HTTP API.
package api

import "time"

const MaxResponseBytes = 64 << 20

// ProjectKey is the metadata key every entry carries, naming the
// project it belongs to within its workspace.
const ProjectKey = "project"

// Link names with a meaning to the server. Every other name only
// links. An entry has at most one parent, and a superseded entry is
// hidden from listings unless they ask for it.
const (
	ParentLink     = "parent"
	SupersedesLink = "supersedes"
)

// Workspace is a named collection of entries on a server, picked in an
// entry id by its prefix.
type Workspace struct {
	Name   string `json:"name"`
	Prefix string `json:"prefix"`
}

type WorkspacesResponse struct {
	Workspaces []Workspace `json:"workspaces"`
}

// Link is a named relationship between entries, possibly in different
// workspaces on one server. On an entry it names the target. In
// backlinks it names the source.
type Link struct {
	Name string `json:"name"`
	Ref  Ref    `json:"ref"`
}

// Origin records who created an entry or message and, as far as the
// client's metadata capture allows, where: the OS user, plus the host,
// working directory and Claude Code session id of the client. Any field
// may be empty.
type Origin struct {
	User          string `json:"user,omitempty"`
	Host          string `json:"host,omitempty"`
	Dir           string `json:"dir,omitempty"`
	ClaudeSession string `json:"claude_session,omitempty"`
}

type Entry struct {
	Ref      Ref               `json:"ref"`
	Created  time.Time         `json:"created"`
	Modified time.Time         `json:"modified"`
	Type     string            `json:"type"`
	Subject  string            `json:"subject"`
	Body     string            `json:"body,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
	Origin   Origin            `json:"origin"`
	Links    []Link            `json:"links,omitempty"`
	// SupersededBy lists the entries that supersede this one.
	SupersededBy []Ref `json:"superseded_by,omitempty"`
}

// EntryRef names an entry in a path of ancestors or a chat overview.
// An entry the request cannot read carries its ref alone.
type EntryRef struct {
	Ref          Ref    `json:"ref"`
	Type         string `json:"type,omitempty"`
	Subject      string `json:"subject,omitempty"`
	SupersededBy []Ref  `json:"superseded_by,omitempty"`
}

// ListedEntry is an entry with the facts derived from its place in its
// tree. The facts count only entries the request can read.
type ListedEntry struct {
	Entry
	// Ancestors runs from the root down to the entry's parent.
	Ancestors []EntryRef `json:"ancestors,omitempty"`
	// Activity is the latest change anywhere in the entry's subtree,
	// itself and chat included. Modified stays the entry's own edit
	// time.
	Activity    time.Time `json:"activity"`
	Descendants int       `json:"descendants"`
}

// EntryDetail adds the links that point at an entry, its child count,
// and a summary of its chat.
type EntryDetail struct {
	ListedEntry
	Backlinks []Link `json:"backlinks,omitempty"`
	Children  int    `json:"children"`
	Messages  int    `json:"messages"`
	// LastMessage is the number of the newest message, or 0.
	LastMessage int64 `json:"last_message"`
}

// TreeEntry is an entry in a tree, with where the tree was cut below
// it. MoreBelow counts the entries under it when none of its children
// are shown. MoreUnder counts the entries under it that are not shown
// when some of its children are.
type TreeEntry struct {
	ListedEntry
	MoreBelow int `json:"more_below,omitempty"`
	MoreUnder int `json:"more_under,omitempty"`
}

// TreeResponse is an entry's subtree with the path to it. Entries come
// depth first, each entry's children oldest first, and each entry's
// parent link places it in the tree.
type TreeResponse struct {
	Ancestors []EntryRef  `json:"ancestors,omitempty"`
	Entries   []TreeEntry `json:"entries"`
}

// Message is one message in an entry's chat, numbered from 1 within
// it in the order they were posted. An edit overwrites Body, keeps
// the message's place, and no earlier text is kept.
type Message struct {
	Entry    Ref       `json:"entry"`
	Number   int64     `json:"number"`
	Created  time.Time `json:"created"`
	Modified time.Time `json:"modified"`
	Body     string    `json:"body"`
	Origin   Origin    `json:"origin"`
}

func (m Message) Ref() MessageRef {
	return MessageRef{Entry: m.Entry, Number: m.Number}
}

type PostMessageRequest struct {
	Body   string `json:"body"`
	Origin Origin `json:"origin"`
}

type EditMessageRequest struct {
	Body string `json:"body"`
}

// ChatPage holds messages from one chat, oldest first. Cursor is the
// number of the last message in the page, or the cursor read from
// when the page is empty. Remaining counts the messages after Cursor.
type ChatPage struct {
	Messages  []Message `json:"messages"`
	Cursor    int64     `json:"cursor"`
	Remaining int       `json:"remaining"`
}

// EntryChat is the newest messages of one entry in a chat overview,
// oldest first.
type EntryChat struct {
	Entry    EntryRef  `json:"entry"`
	Messages []Message `json:"messages"`
}

// ChatOverview holds the newest messages across a workspace, grouped
// by entry. Entries come in order of their newest message, the most
// recent last.
type ChatOverview struct {
	Chats []EntryChat `json:"chats"`
}

type AddEntryRequest struct {
	Type     string            `json:"type"`
	Subject  string            `json:"subject"`
	Body     string            `json:"body,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
	Origin   Origin            `json:"origin"`
	Links    []Link            `json:"links,omitempty"`
}

// EditEntryRequest uses pointers so a field can be updated to the
// empty string (e.g. clearing the body) while absent fields are left
// untouched. A metadata key given an empty value is removed.
//
// Links are removed before they are added. A parent that is added
// replaces the old one, since an entry has at most one.
type EditEntryRequest struct {
	Type     *string           `json:"type,omitempty"`
	Subject  *string           `json:"subject,omitempty"`
	Body     *string           `json:"body,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
	AddLinks []Link            `json:"add_links,omitempty"`
	// RemoveLinks names links that must exist. RemoveLinkNames
	// removes every link of a name, and is fine when there are none.
	RemoveLinks     []Link   `json:"remove_links,omitempty"`
	RemoveLinkNames []string `json:"remove_link_names,omitempty"`
}

// ChangesContent reports whether the edit changes the entry itself,
// rather than only its links.
func (r EditEntryRequest) ChangesContent() bool {
	return r.Type != nil || r.Subject != nil || r.Body != nil ||
		len(r.Metadata) > 0
}

type SearchResult struct {
	Entries []ListedEntry `json:"entries"`
	Total   int           `json:"total"`
}

// ErrorCode says what kind of failure an error response reports, so a
// client can tell them apart without reading the message.
type ErrorCode string

const (
	CodeBadRequest         ErrorCode = "bad_request"
	CodeAPIVersion         ErrorCode = "api_version"
	CodeUnauthorized       ErrorCode = "unauthorized"
	CodeUnknownWorkspace   ErrorCode = "unknown_workspace"
	CodeUnknownPrefix      ErrorCode = "unknown_prefix"
	CodeEntryNotFound      ErrorCode = "entry_not_found"
	CodeMessageNotFound    ErrorCode = "message_not_found"
	CodeLinkNotFound       ErrorCode = "link_not_found"
	CodeLinkTargetNotFound ErrorCode = "link_target_not_found"
	CodeConflict           ErrorCode = "conflict"
	CodeTooLarge           ErrorCode = "too_large"
	CodeRouteNotFound      ErrorCode = "route_not_found"
	CodeUnavailable        ErrorCode = "unavailable"
	CodeInternal           ErrorCode = "internal"
)

type ErrorResponse struct {
	Error string    `json:"error"`
	Code  ErrorCode `json:"code"`
}

type Project struct {
	Name    string `json:"name"`
	Entries int    `json:"entries"`
	// Latest is the latest change to an entry of the project or to
	// its chat.
	Latest time.Time `json:"latest"`
}

type ProjectsResponse struct {
	Projects []Project `json:"projects"`
}

type StatusResponse struct {
	Version  string `json:"version"`
	Database string `json:"database"`
}
