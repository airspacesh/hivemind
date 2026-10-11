package main

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/airspacesh/workstream/internal/api"
)

const (
	clockLayout     = "15:04"
	dateClockLayout = "02/01/2006 15:04"
)

// timeLayout is how one output shows its times: the clock alone when
// every time falls on one local day, or else the date on every line.
func timeLayout(times []time.Time) string {
	for _, t := range times {
		if t.Local().Format(time.DateOnly) !=
			times[0].Local().Format(time.DateOnly) {
			return dateClockLayout
		}
	}
	return clockLayout
}

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

// placeLabel shows where a listed post sits, as the ids of its
// ancestors from the top down joined by dots, like " in e1.e34", or
// nothing for a top-level post.
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

// lineHead starts the line of a post or message with its ref, a time,
// who wrote it, and the path to the post, like [e44] 07:45 ines in
// e1.e34.
func lineHead(
	ref fmt.Stringer, at time.Time, layout, user string,
	ancestors []api.PostRef,
) string {
	head := fmt.Sprintf("[%s] %s", ref, at.Local().Format(layout))
	if user != "" {
		head += " " + user
	}
	return head + placeLabel(ancestors)
}

// printPostLine prints a post on one line, with a time, its author, the
// path to it, and a suffix such as a count.
func printPostLine(
	e api.Post, ancestors []api.PostRef, at time.Time, layout string,
	suffix string,
) {
	fmt.Printf("%s: %s%s%s\n",
		lineHead(e.Ref, at, layout, e.Origin.User, ancestors),
		subjectLine(e.Subject, e.Deleted), supersededLabel(e.SupersededBy),
		suffix)
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
// path to each one's post.
func printMatches(matches []api.Match) {
	times := make([]time.Time, len(matches))
	for i, m := range matches {
		times[i] = m.At
	}
	layout := timeLayout(times)
	for _, m := range matches {
		if m.Message != nil {
			printMessage(*m.Message, m.Ancestors, "", layout)
		} else {
			printPostLine(m.Post, m.Ancestors, m.At, layout,
				newMessagesLabel(m.NewMessages))
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

func printPostDetail(d api.PostDetail) {
	e := d.Post
	fmt.Printf("[%s] %s\n", e.Ref, subjectLine(e.Subject, e.Deleted))
	if len(e.SupersededBy) > 0 {
		fmt.Println("Superseded by " + joinRefs(e.SupersededBy))
	}
	fmt.Println()
	fmt.Println("Project: " + e.Project)
	fmt.Println("Created: " + localTimeDetail(e.Created))
	if e.Deleted {
		fmt.Println("Deleted: " + localTimeDetail(e.Modified))
	} else {
		fmt.Println("Modified: " + localTimeDetail(e.Modified))
	}
	if !d.Activity.Equal(e.Modified) {
		fmt.Println("Subtree activity: " + localTimeDetail(d.Activity))
	}
	printOrigin(e.Origin)
	if len(d.Ancestors) > 0 {
		fmt.Println("Path: " + formatPath(d.Ancestors))
	}
	if len(e.Metadata) > 0 {
		fmt.Println("Metadata:")
		for _, key := range sortedMetaKeys(e.Metadata) {
			fmt.Printf("  %s: %s\n", key, e.Metadata[key])
		}
	}
	if len(e.Links) > 0 {
		fmt.Println("Links:")
		for _, link := range e.Links {
			fmt.Printf("  %s %s\n", link.Name, link.Ref)
		}
	}
	printBacklinks(d.Backlinks)
	if d.Children > 0 {
		fmt.Printf("Children: %d (see '%s tree %s')\n", d.Children,
			programName, e.Ref)
	}
	if d.Messages > 0 {
		newest := api.MessageRef{Post: e.Ref, Number: d.LastMessage}
		fmt.Printf("Chat: %d messages, newest %s (see '%s chat recent %s')\n",
			d.Messages, newest, programName, e.Ref)
	}
	if e.Body != "" {
		fmt.Println()
		fmt.Println(e.Body)
	}
}

// printTree prints a post's subtree, each post indented under its
// parent, with the path to the post above it and a marker wherever the
// tree was cut.
func printTree(tree api.TreeResponse, root api.Ref) {
	if len(tree.Ancestors) > 0 {
		fmt.Println("Path: " + formatPath(tree.Ancestors))
	}
	children := map[api.Ref][]api.TreePost{}
	var times []time.Time
	for _, e := range tree.Posts {
		times = append(times, e.Modified)
		for _, link := range e.Links {
			if link.Name == api.ParentLink {
				children[link.Ref] = append(children[link.Ref], e)
			}
		}
	}
	layout := timeLayout(times)

	var printFrom func(e api.TreePost, depth int)
	printFrom = func(e api.TreePost, depth int) {
		indent := strings.Repeat("  ", depth)
		fmt.Printf("%s[%s] %s: %s%s\n", indent, e.Ref,
			e.Modified.Local().Format(layout),
			subjectLine(e.Subject, e.Deleted), supersededLabel(e.SupersededBy))
		if e.MoreBelow > 0 {
			fmt.Printf("%s  ... %d more below %s\n", indent, e.MoreBelow,
				e.Ref)
		}
		for _, child := range children[e.Ref] {
			printFrom(child, depth+1)
		}
		if e.MoreUnder > 0 {
			fmt.Printf("%s  ... %d more under %s\n", indent, e.MoreUnder,
				e.Ref)
		}
	}
	for _, e := range tree.Posts {
		if e.Ref == root {
			printFrom(e, 0)
		}
	}
}

func messageTimes(messages []api.Message) []time.Time {
	var times []time.Time
	for _, m := range messages {
		times = append(times, m.Created)
	}
	return times
}

// printMessage prints a message on one line, with any further lines of
// its text indented beneath. The path, when given, is to the message's
// post. A deleted message shows no text.
func printMessage(
	m api.Message, ancestors []api.PostRef, indent, layout string,
) {
	head := indent + lineHead(m.Ref(), m.Created, layout, m.Origin.User,
		ancestors)
	if m.Deleted {
		fmt.Println(head + " (deleted)")
		return
	}

	edited := ""
	if !m.Modified.Equal(m.Created) {
		edited = " (edited)"
	}
	lines := strings.Split(m.Body, "\n")
	fmt.Printf("%s%s: %s\n", head, edited, lines[0])
	for _, line := range lines[1:] {
		fmt.Printf("%s    %s\n", indent, line)
	}
}

// printCursorLine ends a chat output with the cursor to read on from,
// as the command that uses it. A suggested wait repeats filterArgs.
func printCursorLine(post api.Ref, page api.ChatPage, filterArgs string) {
	cursor := api.MessageRef{Post: post, Number: page.Cursor}
	if page.Remaining > 0 {
		noun := "messages"
		if page.Remaining == 1 {
			noun = "message"
		}
		fmt.Printf("Next: %s chat read %s --after %s (%d more %s)\n",
			programName, post, cursor, page.Remaining, noun)
		return
	}
	fmt.Printf("Cursor: %s (wait for more: %s chat wait %s --after %s%s)\n",
		cursor, programName, post, cursor, filterArgs)
}

// printChatPage prints one chat's messages oldest first, then the
// cursor line.
func printChatPage(
	post api.Ref, page api.ChatPage, empty string, filterArgs string,
) {
	if len(page.Messages) == 0 {
		fmt.Println(empty)
	}
	layout := timeLayout(messageTimes(page.Messages))
	for _, m := range page.Messages {
		printMessage(m, nil, "", layout)
	}
	printCursorLine(post, page, filterArgs)
}

// printChatHeader names the post a run of chat messages belongs to.
func printChatHeader(post api.PostRef) {
	fmt.Printf("[%s] %s%s\n", post.Ref,
		subjectLine(post.Subject, post.Deleted),
		supersededLabel(post.SupersededBy))
}

func printOverview(overview api.ChatOverview, workspace string) {
	if len(overview.Chats) == 0 {
		fmt.Printf("No messages in workspace %s yet.\n", workspace)
		return
	}
	var times []time.Time
	for _, chat := range overview.Chats {
		times = append(times, messageTimes(chat.Messages)...)
	}
	layout := timeLayout(times)
	for _, chat := range overview.Chats {
		printChatHeader(chat.Post)
		// Message numbers count from 1 in each post, and the overview
		// shows each post's newest messages, so the first number shown
		// says how many come before it.
		if earlier := chat.Messages[0].Number - 1; earlier > 0 {
			fmt.Printf("  ... %d earlier\n", earlier)
		}
		for _, m := range chat.Messages {
			printMessage(m, nil, "  ", layout)
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
	var times []time.Time
	nameWidth, countWidth := 0, 0
	for _, project := range result.Projects {
		times = append(times, project.Latest)
		nameWidth = max(nameWidth, len(project.Name))
		countWidth = max(countWidth,
			len(withThousands(project.RecentPosts)))
	}
	layout := timeLayout(times)
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
			project.Latest.Local().Format(layout), countWidth,
			withThousands(project.RecentPosts), noun,
			shareOf(project.RecentPosts, result.RecentPosts))
	}
	more := result.Count - len(result.Projects)
	noun := "projects"
	if more == 1 {
		noun = "project"
	}
	if more > 0 {
		fmt.Printf("--- %d more %s (use --limit %d to view all) ---\n",
			more, noun, result.Count)
	}
}

// printChanges prints changes oldest first, a line each, as a listing
// shows them.
func printChanges(changes []api.Change) {
	var times []time.Time
	for _, change := range changes {
		if change.Message != nil {
			times = append(times, change.Message.Created)
		} else {
			times = append(times, change.Post.Modified)
		}
	}
	layout := timeLayout(times)
	for _, change := range changes {
		e := change.Post
		if change.Message != nil {
			printMessage(*change.Message, e.Ancestors, "", layout)
		} else {
			printPostLine(e.Post, e.Ancestors, e.Modified, layout, "")
		}
	}
}
