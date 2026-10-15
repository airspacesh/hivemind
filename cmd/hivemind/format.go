package main

import (
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/airspacesh/hivemind/internal/api"
)

// lineTimeLayout is how a post or message line shows its time, in a
// form --after and --before accept.
const lineTimeLayout = "2006-01-02 15:04"

func localTimeDetail(t time.Time) string {
	return t.Local().Format("2006-01-02 15:04:05 MST")
}

func sortedMetaKeys(md map[string]string) []string {
	keys := make([]string, 0, len(md))
	for key := range md {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// oneLine escapes newlines so a subject stays on its line.
func oneLine(text string) string {
	return strings.ReplaceAll(text, "\n", "\\n")
}

// subjectLine shows a post's subject on one line, or (deleted) for a
// deleted post, which has none.
func subjectLine(subject string, deleted bool) string {
	if deleted {
		return "(deleted)"
	}
	return oneLine(subject)
}

// headedSubject follows the head of a post's line with its subject
// after a colon, or with (deleted) for a deleted post, as a deleted
// message's line ends.
func headedSubject(subject string, deleted bool) string {
	if deleted {
		return " (deleted)"
	}
	return ": " + oneLine(subject)
}

// privateLabel marks a post that only one user can read, its owner or
// the owner of a private post above it, who is the reader.
func privateLabel(privateTo string) string {
	if privateTo == "" {
		return ""
	}
	return " [private]"
}

func joinRefs(refs []api.Ref) string {
	names := make([]string, len(refs))
	for i, ref := range refs {
		names[i] = ref.String()
	}
	return strings.Join(names, ", ")
}

// supersededLabel marks a post that other posts supersede. Every
// successor is named, since none of them is more current than another.
func supersededLabel(supersededBy []api.Ref) string {
	if len(supersededBy) == 0 {
		return ""
	}
	return " (superseded by " + joinRefs(supersededBy) + ")"
}

// formatPath shows ancestors from the root down. An ancestor the
// request cannot read shows as its bare id.
func formatPath(path []api.PostRef) string {
	parts := make([]string, len(path))
	for i, post := range path {
		parts[i] = post.Ref.String()
		subject := subjectLine(post.Subject, post.Deleted)
		if subject != "" {
			parts[i] += " " + subject + supersededLabel(post.SupersededBy)
		}
	}
	return strings.Join(parts, " > ")
}

// placeLabel shows where a listed post or message sits, as the ids of
// the posts above it from the top down joined by dots, like
// " in e1.e34", or nothing for a top-level post.
func placeLabel(ancestors []api.PostRef) string {
	if len(ancestors) == 0 {
		return ""
	}
	ids := make([]string, len(ancestors))
	for i, post := range ancestors {
		ids[i] = post.Ref.String()
	}
	return " in " + strings.Join(ids, ".")
}

// pathTo is the path down to a post from the root, for a line about
// something that sits in it, like a message in its chat.
func pathTo(ancestors []api.PostRef, post api.Ref) []api.PostRef {
	return append(slices.Clone(ancestors), api.PostRef{Ref: post})
}

// lineHead starts the line of a post or message with its ref, a time,
// who wrote it, and the path to the post, like [e44] 2026-10-13 07:45
// ines in e1.e34.
func lineHead(
	ref fmt.Stringer, at time.Time, user string, ancestors []api.PostRef,
) string {
	head := fmt.Sprintf("[%s] %s", ref, at.Local().Format(lineTimeLayout))
	if user != "" {
		head += " " + user
	}
	return head + placeLabel(ancestors)
}

// printPostLine prints a post on one line, with a time, its author, the
// path to it, whether it is private, and a suffix such as a count.
func printPostLine(
	post api.Post, ancestors []api.PostRef, at time.Time,
	indent, suffix string,
) {
	fmt.Printf("%s%s%s%s%s%s\n", indent,
		lineHead(post.Ref, at, post.Origin.User, ancestors),
		privateLabel(post.PrivateTo),
		headedSubject(post.Subject, post.Deleted),
		supersededLabel(post.SupersededBy), suffix)
}

// metadataLabel shows the value of each of keys a post has, in order,
// like " [outdated]".
func metadataLabel(metadata map[string]string, keys []string) string {
	label := ""
	for _, key := range keys {
		if value, ok := metadata[key]; ok {
			label += " [" + value + "]"
		}
	}
	return label
}

func newMessagesLabel(count int) string {
	switch count {
	case 0:
		return ""
	case 1:
		return " (1 new message)"
	default:
		return fmt.Sprintf(" (%d new messages)", count)
	}
}

func printMatchRefs(page api.MatchPage) {
	for _, m := range page.Matches {
		if m.Message != nil {
			fmt.Println(m.Message.Ref())
		} else {
			fmt.Println(m.Post.Ref)
		}
	}
}

// printMatches prints the matches of a listing a line each, with the
// path to each one's post, and each post's values of metadataKeys.
func printMatches(matches []api.Match, metadataKeys []string) {
	for _, m := range matches {
		if m.Message != nil {
			printMessage(*m.Message, pathTo(m.Ancestors, m.Post.Ref), m.At,
				"")
		} else {
			printPostLine(m.Post, m.Ancestors, m.At, "",
				newMessagesLabel(m.NewMessages)+
					metadataLabel(m.Post.Metadata, metadataKeys))
		}
	}
}

// author names who wrote something: the user, and the host when the
// writer's metadata capture recorded it.
func author(o api.Origin) string {
	switch {
	case o.Host == "":
		return o.User
	case o.User == "":
		return o.Host
	default:
		return o.User + "@" + o.Host
	}
}

func printOrigin(o api.Origin) {
	location := author(o)
	if o.Dir != "" {
		location += ":" + o.Dir
	}
	if location != "" {
		fmt.Println("From: " + location)
	}
	if o.ClaudeSession != "" {
		fmt.Println("  claude session: " + o.ClaudeSession)
	}
}

// printBacklinks shows the links other posts hold to this one, apart
// from children, which the child count covers.
func printBacklinks(backlinks []api.Link) {
	var lines []string
	for _, link := range backlinks {
		switch link.Name {
		case api.ParentLink:
		case api.SupersedesLink:
			lines = append(lines, "superseded by "+link.Ref.String())
		default:
			lines = append(lines,
				fmt.Sprintf("%s %s this post", link.Ref, link.Name))
		}
	}
	if len(lines) == 0 {
		return
	}
	fmt.Println("Linked from:")
	for _, line := range lines {
		fmt.Println("  " + line)
	}
}

// versionLine says which version of a post a read shows, who made it,
// and when.
func versionLine(v api.Version) string {
	line := fmt.Sprintf("Version: %d,", v.Number)
	if maker := author(v.Origin); maker != "" {
		line += " by " + maker
	}
	return line + " at " + localTimeDetail(v.Created)
}

func printMetadataAndLinks(metadata map[string]string, links []api.Link) {
	if len(metadata) > 0 {
		fmt.Println("Metadata:")
		for _, key := range sortedMetaKeys(metadata) {
			fmt.Printf("  %s: %s\n", key, metadata[key])
		}
	}
	if len(links) > 0 {
		fmt.Println("Links:")
		for _, link := range links {
			fmt.Printf("  %s %s\n", link.Name, link.Ref)
		}
	}
}

func printPostDetail(d api.PostDetail) {
	post := d.Post
	printPostLine(post, nil, post.Modified, "", "")
	fmt.Println()
	fmt.Println("Project: " + post.Project)
	fmt.Println("Created: " + localTimeDetail(post.Created))
	if post.Deleted {
		fmt.Println("Deleted: " + localTimeDetail(post.Modified))
	} else {
		fmt.Println("Modified: " + localTimeDetail(post.Modified))
	}
	fmt.Println(versionLine(*post.Version))
	if !d.Activity.Equal(post.Modified) {
		fmt.Println("Subtree activity: " + localTimeDetail(d.Activity))
	}
	switch {
	case post.Private:
		fmt.Printf("Private: only %s can read it\n", post.PrivateTo)
	case post.PrivateTo != "":
		fmt.Printf("Private: only %s can read it, as a post above it is "+
			"private\n", post.PrivateTo)
	}
	printOrigin(post.Origin)
	if len(d.Ancestors) > 0 {
		fmt.Println("Path: " + formatPath(d.Ancestors))
	}
	printMetadataAndLinks(post.Metadata, post.Links)
	printBacklinks(d.Backlinks)
	if d.Children > 0 {
		fmt.Printf("Children: %d (see '%s tree %s')\n", d.Children,
			programName, post.Ref)
	}
	if d.Messages > 0 {
		noun := "messages"
		if d.Messages == 1 {
			noun = "message"
		}
		newest := api.MessageRef{Post: post.Ref, Number: d.LastMessage}
		fmt.Printf("Chat: %d %s, newest %s (see '%s chat recent %s')\n",
			d.Messages, noun, newest, programName, post.Ref)
	}
	if post.Body != "" {
		fmt.Println()
		fmt.Println(post.Body)
	}
}

// printPostVersion prints one version of a post as printPostDetail
// prints the post, with only what a version records.
func printPostVersion(v api.PostVersion) {
	fmt.Printf("%s%s\n", lineHead(v.Ref, v.Version.Created,
		v.Version.Origin.User, nil), headedSubject(v.Subject, v.Deleted))
	fmt.Println()
	fmt.Println(versionLine(v.Version))
	fmt.Println("Project: " + v.Project)
	printMetadataAndLinks(v.Metadata, v.Links)
	if v.Body != "" {
		fmt.Println()
		fmt.Println(v.Body)
	}
}

// printMessageDetail prints a message under the line of its post, then
// when and where it was written.
func printMessageDetail(d api.MessageDetail) {
	m := d.Message
	printChatHeader(d.Post)
	printMessage(m, nil, m.Created, "  ")
	fmt.Println()
	fmt.Println("Created: " + localTimeDetail(m.Created))
	if m.Deleted {
		fmt.Println("Deleted: " + localTimeDetail(m.Modified))
	}
	printOrigin(m.Origin)
	if len(d.Ancestors) > 0 {
		fmt.Println("Path: " + formatPath(d.Ancestors))
	}
}

// printTree prints a post's subtree, each post indented under its
// parent, with the path to the post above it and a marker wherever the
// tree was cut. Each post's line ends with its values of metadataKeys.
func printTree(tree api.TreeResponse, root api.Ref, metadataKeys []string) {
	if len(tree.Ancestors) > 0 {
		fmt.Println("Path: " + formatPath(tree.Ancestors))
	}
	children := map[api.Ref][]api.TreePost{}
	for _, post := range tree.Posts {
		for _, link := range post.Links {
			if link.Name == api.ParentLink {
				children[link.Ref] = append(children[link.Ref], post)
			}
		}
	}

	var printFrom func(post api.TreePost, depth int)
	printFrom = func(post api.TreePost, depth int) {
		indent := strings.Repeat("  ", depth)
		printPostLine(post.Post, nil, post.Modified, indent,
			metadataLabel(post.Metadata, metadataKeys))
		if post.MoreBelow > 0 {
			fmt.Printf("%s  ... %d more below %s\n", indent, post.MoreBelow,
				post.Ref)
		}
		for _, child := range children[post.Ref] {
			printFrom(child, depth+1)
		}
		if post.MoreUnder > 0 {
			fmt.Printf("%s  ... %d more under %s\n", indent, post.MoreUnder,
				post.Ref)
		}
	}
	for _, post := range tree.Posts {
		if post.Ref == root {
			printFrom(post, 0)
		}
	}
}

// printMessage prints a message on one line, with a time, its author,
// and any further lines of its text indented beneath. The path, when
// given, runs down to the message's post. A deleted message shows no
// text.
func printMessage(
	m api.Message, ancestors []api.PostRef, at time.Time, indent string,
) {
	head := indent + lineHead(m.Ref(), at, m.Origin.User, ancestors)
	if m.Deleted {
		fmt.Println(head + " (deleted)")
		return
	}

	lines := strings.Split(m.Body, "\n")
	fmt.Printf("%s: %s\n", head, lines[0])
	for _, line := range lines[1:] {
		fmt.Printf("%s    %s\n", indent, line)
	}
}

// chatWaitCommand is the chat wait that reads a chat after cursor, with
// the filters it was given.
func chatWaitCommand(cursor api.MessageRef, filterArgs string) string {
	return fmt.Sprintf("%s chat wait %s --after %s%s", programName,
		cursor.Post, cursor, filterArgs)
}

// printChatCursor ends the output of a chat read from after with its
// cursor, unless the output already names it last, as the last message
// shown, or when it shows none, as the message the read started after.
func printChatCursor(post api.Ref, page api.ChatPage, after api.Bound) {
	var named int64
	switch {
	case len(page.Messages) > 0:
		named = page.Messages[len(page.Messages)-1].Number
	case after.Post == post:
		named = after.Message
	}
	if page.Cursor != named {
		fmt.Printf("Cursor: %s\n",
			api.MessageRef{Post: post, Number: page.Cursor})
	}
}

// printCursorLine ends a chat read from after with the read of its next
// page, with the flags readFlags repeats, or else its cursor.
func printCursorLine(
	post api.Ref, page api.ChatPage, after api.Bound, readFlags string,
) {
	if page.Remaining > 0 {
		noun := "messages"
		if page.Remaining == 1 {
			noun = "message"
		}
		cursor := api.MessageRef{Post: post, Number: page.Cursor}
		fmt.Printf("Next: %s chat read %s --after %s%s (%d more %s)\n",
			programName, post, cursor, readFlags, page.Remaining, noun)
		return
	}
	printChatCursor(post, page, after)
}

// printEarlier marks how many messages after the bound from a chat
// output leaves out before the first it shows, with the read that shows
// them, which repeats the flags readFlags holds.
func printEarlier(
	indent string, post api.Ref, from api.Bound, earlier int,
	readFlags string,
) {
	if earlier == 0 {
		return
	}
	start := boundArg(from)
	if from.Post == post && from.Message == 0 {
		start = api.MessageRef{Post: post}.String()
	}
	fmt.Printf("%s... %d earlier (%s chat read %s --after %s%s)\n", indent,
		earlier, programName, post, start, readFlags)
}

// printChatMessages prints one chat's messages oldest first, or empty
// when there are none.
func printChatMessages(page api.ChatPage, empty string) {
	if len(page.Messages) == 0 {
		fmt.Println(empty)
	}
	for _, m := range page.Messages {
		printMessage(m, nil, m.Created, "")
	}
}

// printChatPage prints one chat's messages after after oldest first,
// then the cursor line, whose read repeats the flags readFlags holds.
func printChatPage(
	post api.Ref, page api.ChatPage, after api.Bound, empty string,
	readFlags string,
) {
	printChatMessages(page, empty)
	printCursorLine(post, page, after, readFlags)
}

// printChatHeader names the post a run of chat messages belongs to.
func printChatHeader(post api.Post) {
	printPostLine(post, nil, post.Modified, "", "")
}

// printOverview prints the newest messages across a workspace under
// their posts, each chat's earlier ones since after counted with the
// read that shows them, which repeats the flags readFlags holds, or
// empty when there are none.
func printOverview(
	overview api.ChatOverview, empty string, after *api.Bound,
	readFlags string,
) {
	if len(overview.Chats) == 0 {
		fmt.Println(empty)
		return
	}
	for _, chat := range overview.Chats {
		printChatHeader(chat.Post)
		from := chatCursor(chat.Post.Ref, 0)
		if after != nil {
			from = *after
		}
		printEarlier("  ", chat.Post.Ref, from, chat.Earlier, readFlags)
		for _, m := range chat.Messages {
			printMessage(m, nil, m.Created, "  ")
		}
	}
}

// withThousands writes a count with commas between groups of three
// digits, like 1,204.
func withThousands(n int) string {
	digits := strconv.Itoa(n)
	for i := len(digits) - 3; i > 0; i -= 3 {
		digits = digits[:i] + "," + digits[i:]
	}
	return digits
}

// shareOf shows part as a whole percentage of total, or <1% for a part
// too small to round to 1%.
func shareOf(part, total int) string {
	if part == 0 {
		return "0%"
	}
	percent := float64(part) * 100 / float64(total)
	if percent < 0.5 {
		return "<1%"
	}
	return fmt.Sprintf("%.0f%%", percent)
}

// printProjects prints one project a line, in columns, with its share
// of the workspace's recent posts.
func printProjects(result api.ProjectsResponse) {
	nameWidth, countWidth := 0, 0
	for _, project := range result.Projects {
		nameWidth = max(nameWidth, len(project.Name))
		countWidth = max(countWidth,
			len(withThousands(project.RecentPosts)))
	}
	days := int(time.Since(result.RecentSince).Round(24*time.Hour) /
		(24 * time.Hour))
	fmt.Printf("Projects by latest activity, with posts from the last "+
		"%d days:\n", days)
	for _, project := range result.Projects {
		noun := "posts"
		if project.RecentPosts == 1 {
			noun = "post"
		}
		fmt.Printf("%-*s   %s   %*s %-5s  %4s\n", nameWidth, project.Name,
			project.Latest.Local().Format(lineTimeLayout), countWidth,
			withThousands(project.RecentPosts), noun,
			shareOf(project.RecentPosts, result.RecentPosts))
	}
}

// printChanges prints changes oldest first, a line each, as a listing
// shows them, each at the time of its latest change.
func printChanges(changes []api.Change) {
	for _, change := range changes {
		post := change.Post
		if change.Message != nil {
			printMessage(*change.Message, pathTo(post.Ancestors, post.Ref),
				change.Message.Modified, "")
		} else {
			printPostLine(post.Post, post.Ancestors, post.Modified, "", "")
		}
	}
}
