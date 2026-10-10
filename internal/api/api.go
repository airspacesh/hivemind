// Package api defines the JSON types shared between the ws CLI and
// the ws-server HTTP API.
package api

import "time"

const MaxResponseBytes = 64 << 20

// ProjectKey is reserved as a metadata key, since every entry has a
// project field, and a pair of that name would read as a second one.
const ProjectKey = "project"

// Link names with a meaning to the server. Every other name only
// links. An entry has at most one parent, in its own workspace, and a
// superseded entry is hidden from listings unless they ask for it.
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
	Project  string            `json:"project"`
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
	Subject      string `json:"subject,omitempty"`
	SupersededBy []Ref  `json:"superseded_by,omitempty"`
}

// ListedEntry is an entry with the facts derived from its place in its
// tree.
type ListedEntry struct {
	Entry
	// Ancestors runs from the root down to the entry's parent.
	Ancestors []EntryRef `json:"ancestors,omitempty"`
	// Activity is the latest change anywhere in the entry's subtree,
	// itself and chat included. Modified stays the entry's own edit
	// time.
	Activity time.Time `json:"activity"`
	// Descendants counts the current entries under the entry, leaving
	// out superseded ones.
	Descendants int `json:"descendants"`
}

// EntryDetail adds the links that point at an entry, how many current
// entries are its children, and a summary of its chat.
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

// EditMessageRequest carries the user making the edit, who must be
// the user that posted the message.
type EditMessageRequest struct {
	Body string `json:"body"`
	User string `json:"user"`
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

// Change is an entry added or edited, or a message posted or edited
// in an entry's chat. Message is nil for a change to the entry
// itself. Each carries its current content, so an entry or message
// changed twice after a cursor appears once, at its latest change.
type Change struct {
	Entry   ListedEntry `json:"entry"`
	Message *Message    `json:"message,omitempty"`
}

// ChangesPage holds a workspace's changes after a revision, oldest
// first. Cursor is the revision of the last change in the page, or the
// revision read from when the page is empty. More says whether changes
// follow it.
type ChangesPage struct {
	Changes []Change `json:"changes"`
	Cursor  int64    `json:"cursor"`
	More    bool     `json:"more"`
}

type AddEntryRequest struct {
	Project  string            `json:"project"`
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
	Project  *string           `json:"project,omitempty"`
	Subject  *string           `json:"subject,omitempty"`
	Body     *string           `json:"body,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
	AddLinks []Link            `json:"add_links,omitempty"`
	// RemoveLinks names links that must exist. RemoveLinkNames
	// removes every link of a name, and is fine when there are none.
	RemoveLinks     []Link   `json:"remove_links,omitempty"`
	RemoveLinkNames []string `json:"remove_link_names,omitempty"`
	// User is the user making the edit, who must be the user that
	// posted the entry.
	User string `json:"user"`
}

// ChangesContent reports whether the edit changes the entry itself,
// rather than only its links.
func (r EditEntryRequest) ChangesContent() bool {
	return r.Project != nil || r.Subject != nil || r.Body != nil ||
		len(r.Metadata) > 0
}

// MatchedEntry is an entry a listing shows. Messages come in the order
// they were posted, and an edit does not move a message.
//
// In a search for text, MatchingMessages holds the messages in the
// entry's chat that matched and that the page's shared budget of
// messages had room for. MoreMatchingMessages counts the entry's other
// matching messages.
//
// In the topics view, TreeMessages holds the newest messages anywhere
// in the entry's tree, and EarlierTreeMessages counts the tree's other
// messages.
type MatchedEntry struct {
	ListedEntry
	MatchingMessages     []Message `json:"matching_messages,omitempty"`
	MoreMatchingMessages int       `json:"more_matching_messages,omitempty"`
	TreeMessages         []Message `json:"tree_messages,omitempty"`
	EarlierTreeMessages  int       `json:"earlier_tree_messages,omitempty"`
}

// SearchResult is a page of a listing. More says whether entries
// follow the page. Listings do not count their matches, since a count
// takes time in proportion to the workspace.
type SearchResult struct {
	Entries []MatchedEntry `json:"entries"`
	More    bool           `json:"more"`
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
	CodeNotOwner           ErrorCode = "not_owner"
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
	Name string `json:"name"`
	// Latest is the latest change to an entry of the project or to
	// its chat.
	Latest time.Time `json:"latest"`
	// RecentEntries counts the project's entries created since the
	// response's RecentSince.
	RecentEntries int `json:"recent_entries"`
	// Entries counts every entry of the project. Only a request that
	// names the project gets it, since counting every project's entries
	// takes time in proportion to the workspace.
	Entries *int `json:"entries,omitempty"`
}

// ProjectsResponse lists projects, the most recently active first.
type ProjectsResponse struct {
	Projects []Project `json:"projects"`
	// Count is the number of projects the request matched, including
	// any the limit left out.
	Count       int       `json:"count"`
	RecentSince time.Time `json:"recent_since"`
	// RecentEntries sums the RecentEntries of every project the request
	// matched, including any the limit left out.
	RecentEntries int `json:"recent_entries"`
}

type StatusResponse struct {
	Version  string `json:"version"`
	Database string `json:"database"`
}
