// Package api defines the JSON types shared between the hivemind CLI and
// the hivemind-server HTTP API.
package api

import (
	"fmt"
	"time"
)

const MaxResponseBytes = 64 << 20

// UserHeader carries the user a request reads as, who can read the
// posts private to them as well as the public ones. The server takes
// the client's word for it.
const UserHeader = "Hivemind-User"

// ProjectKey is reserved as a metadata key, since every post has a
// project field, and a pair of that name would read as a second one.
const ProjectKey = "project"

// Link names with a meaning to the server. Every other name only links.
// A post has at most one parent, in its own workspace, and a superseded
// post is hidden from listings unless they ask for it.
const (
	ParentLink     = "parent"
	SupersedesLink = "supersedes"
)

// Workspace is a named collection of posts on a server, picked in a
// post id by its prefix.
type Workspace struct {
	Name   string `json:"name"`
	Prefix string `json:"prefix"`
}

type WorkspacesResponse struct {
	Workspaces []Workspace `json:"workspaces"`
}

// Link is a named relationship between posts, possibly in different
// workspaces on one server. On a post it names the target. In backlinks
// it names the source.
type Link struct {
	Name string `json:"name"`
	Ref  Ref    `json:"ref"`
}

// Origin records who created a post or message and, as far as the
// client's metadata capture allows, where: the OS user, plus the host,
// working directory and Claude Code session id of the client. Any field
// may be empty.
type Origin struct {
	User          string `json:"user,omitempty"`
	Host          string `json:"host,omitempty"`
	Dir           string `json:"dir,omitempty"`
	ClaudeSession string `json:"claude_session,omitempty"`
}

// Version is one version of a post: its number, counted from 1 when
// the post was added, and who made it and when, by the add or edit that
// changed the post's project, subject, body, metadata or links.
type Version struct {
	Number  int64     `json:"number"`
	Created time.Time `json:"created"`
	Origin  Origin    `json:"origin"`
}

// Post is a post with its own fields. A deleted post has no subject,
// body or metadata, and cannot be edited. Revision is the revision of
// the post's own latest change, which set Modified. Private says the
// post's owner made it private. PrivateTo names the user who alone can
// read the post, the owner of the nearest private post at or above it,
// or is empty when everyone can.
type Post struct {
	Ref       Ref               `json:"ref"`
	Created   time.Time         `json:"created"`
	Modified  time.Time         `json:"modified"`
	Revision  int64             `json:"revision"`
	Project   string            `json:"project"`
	Subject   string            `json:"subject"`
	Body      string            `json:"body,omitempty"`
	Metadata  map[string]string `json:"metadata,omitempty"`
	Origin    Origin            `json:"origin"`
	Links     []Link            `json:"links,omitempty"`
	Deleted   bool              `json:"deleted,omitempty"`
	Private   bool              `json:"private,omitempty"`
	PrivateTo string            `json:"private_to,omitempty"`
	// SupersededBy lists the posts that supersede this one.
	SupersededBy []Ref `json:"superseded_by,omitempty"`
	// Version is the post's current version. Only a read or an edit of
	// the post alone gets it, since listings show no versions.
	Version *Version `json:"version,omitempty"`
}

// PostVersion is a post's project, subject, body, metadata and links as
// one of its versions had them. A deleted post's versions have no
// subject, body or metadata.
type PostVersion struct {
	Ref      Ref               `json:"ref"`
	Version  Version           `json:"version"`
	Project  string            `json:"project"`
	Subject  string            `json:"subject"`
	Body     string            `json:"body,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
	Links    []Link            `json:"links,omitempty"`
	Deleted  bool              `json:"deleted,omitempty"`
}

// PostRef names a post in a path of ancestors. A post the request
// cannot read carries its ref alone.
type PostRef struct {
	Ref          Ref    `json:"ref"`
	Subject      string `json:"subject,omitempty"`
	Deleted      bool   `json:"deleted,omitempty"`
	PrivateTo    string `json:"private_to,omitempty"`
	SupersededBy []Ref  `json:"superseded_by,omitempty"`
}

// ListedPost is a post with the facts derived from its place in its
// tree.
type ListedPost struct {
	Post
	// Ancestors runs from the root down to the post's parent.
	Ancestors []PostRef `json:"ancestors,omitempty"`
	// Activity is the latest change anywhere in the post's subtree,
	// itself and chat included. Modified stays the post's own edit
	// time.
	Activity time.Time `json:"activity"`
	// Descendants counts the current posts under the post, leaving out
	// superseded and deleted ones.
	Descendants int `json:"descendants"`
}

// PostDetail adds the links that point at a post, how many of its
// children are neither superseded nor deleted, and a summary of the
// messages in its chat that are not deleted.
type PostDetail struct {
	ListedPost
	Backlinks []Link `json:"backlinks,omitempty"`
	Children  int    `json:"children"`
	Messages  int    `json:"messages"`
	// LastMessage is the number of the newest message, or 0.
	LastMessage int64 `json:"last_message"`
}

// TreePost is a post in a tree, with where the tree was cut below it.
// MoreBelow counts the posts under it when none of its children are
// shown. MoreUnder counts the posts under it that are not shown when
// some of its children are.
type TreePost struct {
	ListedPost
	MoreBelow int `json:"more_below,omitempty"`
	MoreUnder int `json:"more_under,omitempty"`
}

// TreeResponse is a post's subtree with the path to it. Posts come
// depth first, each post's children oldest first, and each post's
// parent link places it in the tree.
type TreeResponse struct {
	Ancestors []PostRef  `json:"ancestors,omitempty"`
	Posts     []TreePost `json:"posts"`
}

// Message is one message in a post's chat, numbered from 1 within it in
// the order they were posted. An edit overwrites Body, keeps the
// message's place, and no earlier text is kept. A deleted message has
// no body, and cannot be edited. Revision is the revision of the
// message's latest change, which set Modified.
type Message struct {
	Post     Ref       `json:"post"`
	Number   int64     `json:"number"`
	Created  time.Time `json:"created"`
	Modified time.Time `json:"modified"`
	Revision int64     `json:"revision"`
	Body     string    `json:"body"`
	Origin   Origin    `json:"origin"`
	Deleted  bool      `json:"deleted,omitempty"`
}

func (m Message) Ref() MessageRef {
	return MessageRef{Post: m.Post, Number: m.Number}
}

// MessageDetail is one message with the post whose chat holds it, and
// the path to that post from the root.
type MessageDetail struct {
	Message   Message   `json:"message"`
	Post      Post      `json:"post"`
	Ancestors []PostRef `json:"ancestors,omitempty"`
}

// PostMessageRequest may carry a request id, which the client makes up
// so that the server sends a message at most once for it, in one post's
// chat.
type PostMessageRequest struct {
	Body      string `json:"body"`
	Origin    Origin `json:"origin"`
	RequestID string `json:"request_id,omitempty"`
}

// EditMessageRequest carries the user making the edit, who must be
// the user that posted the message.
type EditMessageRequest struct {
	Body string `json:"body"`
	User string `json:"user"`
}

// DeleteRequest carries the user deleting a post or message, who must
// be the user that posted it.
type DeleteRequest struct {
	User string `json:"user"`
}

// ChatPage holds messages from one chat, oldest first. A read leaves
// deleted messages out unless it asks for them. Cursor is the highest
// message number the server checked, so every message up to it is in
// the page or did not match the request's filters. Remaining counts the
// messages after Cursor that the read would show, filters aside.
// Earlier, for a read of the newest messages, counts those it would
// show from its start up to the first in Messages, which it leaves out.
// Revision is the latest revision when the page was read, so the
// changes after it are the changes since the read.
type ChatPage struct {
	Messages  []Message `json:"messages"`
	Cursor    int64     `json:"cursor"`
	Remaining int       `json:"remaining"`
	Earlier   int       `json:"earlier"`
	Revision  int64     `json:"revision"`
}

// PostChat is the newest messages of one post in a chat overview,
// oldest first. Earlier counts the messages before the first in
// Messages that the overview would show, but leaves out.
type PostChat struct {
	Post     Post      `json:"post"`
	Messages []Message `json:"messages"`
	Earlier  int       `json:"earlier"`
}

// ChatOverview holds the newest messages across a workspace, grouped by
// post. Posts come in order of their newest message, the most recent
// last. Revision is the latest revision when it was read.
type ChatOverview struct {
	Chats    []PostChat `json:"chats"`
	Revision int64      `json:"revision"`
}

// Change is a post added, edited or deleted, or a message posted,
// edited or deleted in a post's chat. Message is nil for a change to
// the post itself. Each carries its current content, so a post or
// message changed twice after a cursor appears once, at its latest
// change.
type Change struct {
	Post    ListedPost `json:"post"`
	Message *Message   `json:"message,omitempty"`
}

// ChangesPage holds a workspace's changes after a revision, oldest
// first. Cursor is the highest revision the server checked, so every
// change up to it is in the page or did not match the request's
// filters. More says whether changes after Cursor are still to be
// checked.
type ChangesPage struct {
	Changes []Change `json:"changes"`
	Cursor  int64    `json:"cursor"`
	More    bool     `json:"more"`
}

// AddPostRequest may carry a request id, which the client makes up so
// that the server adds a post at most once for it, in one workspace.
// Private makes the post private to the user of its origin.
type AddPostRequest struct {
	Project   string            `json:"project"`
	Subject   string            `json:"subject"`
	Body      string            `json:"body,omitempty"`
	Metadata  map[string]string `json:"metadata,omitempty"`
	Origin    Origin            `json:"origin"`
	Links     []Link            `json:"links,omitempty"`
	Private   bool              `json:"private,omitempty"`
	RequestID string            `json:"request_id,omitempty"`
}

// EditPostRequest uses pointers so a field can be updated to the empty
// string (e.g. clearing the body) while absent fields are left
// untouched. A metadata key given an empty value is removed.
//
// Links are removed before they are added. A parent that is added
// replaces the old one, since a post has at most one.
type EditPostRequest struct {
	Project  *string           `json:"project,omitempty"`
	Subject  *string           `json:"subject,omitempty"`
	Body     *string           `json:"body,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
	AddLinks []Link            `json:"add_links,omitempty"`
	// RemoveLinks names links that must exist. RemoveLinkNames
	// removes every link of a name, and is fine when there are none.
	RemoveLinks     []Link   `json:"remove_links,omitempty"`
	RemoveLinkNames []string `json:"remove_link_names,omitempty"`
	// Private makes the post private when true, and public when false.
	Private *bool `json:"private,omitempty"`
	// Origin records who makes the edit, and where, as the version it
	// makes keeps it. Only the user who wrote the post can edit it while
	// it is private, or make it private or public.
	Origin Origin `json:"origin"`
}

// ChangesContent reports whether the edit changes the post itself,
// rather than only its links.
func (r EditPostRequest) ChangesContent() bool {
	return r.Project != nil || r.Subject != nil || r.Body != nil ||
		len(r.Metadata) > 0
}

// Match is a match on a page of a listing. It is a post, or a message
// in the post's chat when Message is set. Ancestors runs from the root
// down to the post's parent. Revision and At are the revision and time
// of the change the match ranks by. That is the posting of the
// message, or the post's own latest change in a listing with chat, or
// else the latest change to the post or its chat. NewMessages counts,
// in a listing without chat, the messages posted in the post's chat at
// or after the revision of the page's oldest match.
type Match struct {
	Post        Post      `json:"post"`
	Ancestors   []PostRef `json:"ancestors,omitempty"`
	Message     *Message  `json:"message,omitempty"`
	Revision    int64     `json:"revision"`
	At          time.Time `json:"at"`
	NewMessages int       `json:"new_messages,omitempty"`
}

// MatchPage is a page of a listing, newest first. More says whether
// older matches follow the page. Revision is the latest revision when
// the page was read, so the changes after it are the changes since the
// read.
type MatchPage struct {
	Matches  []Match `json:"matches"`
	More     bool    `json:"more"`
	Revision int64   `json:"revision"`
}

// Items says what a listing or the changes feed holds: posts, the
// messages in their chats, or both. Its text is the value of the items
// query parameter.
type Items int

const (
	PostsOnly Items = iota
	ChatOnly
	PostsAndChat
)

// itemsText is the text of each Items.
var itemsText = map[Items]string{
	PostsOnly:    "posts",
	ChatOnly:     "chat",
	PostsAndChat: "posts-and-chat",
}

func (i Items) String() string {
	return itemsText[i]
}

// ParseItems reads Items as String writes them.
func ParseItems(text string) (Items, error) {
	for items, written := range itemsText {
		if written == text {
			return items, nil
		}
	}
	return 0, fmt.Errorf("items must be posts, chat or posts-and-chat, "+
		"not %q", text)
}

// HasPosts reports whether the items hold posts.
func (i Items) HasPosts() bool {
	return i != ChatOnly
}

// HasChat reports whether the items hold messages.
func (i Items) HasChat() bool {
	return i != PostsOnly
}

// SearchFields name what a search condition can match. Each is a query
// parameter of a listing and of the changes feed, and with no- before
// it, one that leaves out what it matches.
var SearchFields = []string{
	"subject", "body", "content", "project", "key", "meta",
	"origin-user", "origin-host", "origin-dir", "origin-claude-session",
}

// MessageFields are the SearchFields a chat message matches on its own,
// and the conditions a chat read takes.
var MessageFields = []string{
	"content",
	"origin-user", "origin-host", "origin-dir", "origin-claude-session",
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
	CodePostNotFound       ErrorCode = "post_not_found"
	CodeMessageNotFound    ErrorCode = "message_not_found"
	CodeVersionNotFound    ErrorCode = "version_not_found"
	CodeLinkNotFound       ErrorCode = "link_not_found"
	CodeLinkTargetNotFound ErrorCode = "link_target_not_found"
	CodeConflict           ErrorCode = "conflict"
	CodeNotOwner           ErrorCode = "not_owner"
	CodeDeleted            ErrorCode = "deleted"
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
	// Latest is the latest change to a post of the project or to its
	// chat.
	Latest time.Time `json:"latest"`
	// RecentPosts counts the project's posts created since the
	// response's RecentSince.
	RecentPosts int `json:"recent_posts"`
	// Posts counts every post of the project. Only a request that names
	// the project gets it, since counting every project's posts takes
	// time in proportion to the workspace.
	Posts *int `json:"posts,omitempty"`
}

// ProjectsResponse lists projects, the most recently active first.
type ProjectsResponse struct {
	Projects []Project `json:"projects"`
	// Count is the number of projects the request matched, including
	// any the limit left out.
	Count       int       `json:"count"`
	RecentSince time.Time `json:"recent_since"`
	// RecentPosts sums the RecentPosts of every project the request
	// matched, including any the limit left out.
	RecentPosts int `json:"recent_posts"`
}

type StatusResponse struct {
	Version  string `json:"version"`
	Database string `json:"database"`
}
