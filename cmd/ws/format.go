package main

import (
	"fmt"
	"sort"
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

// listOrder is the time a listing is ordered by.
type listOrder int

const (
	byModified listOrder = iota
	byCreation
	// byActivity orders by the latest change in each entry's subtree.
	byActivity
)

// listTime is the time a listing shows: the one it is ordered by.
func listTime(e api.ListedEntry, order listOrder) time.Time {
	switch order {
	case byCreation:
		return e.Created
	case byActivity:
		return e.Activity
	default:
		return e.Modified
	}
}

func joinRefs(refs []api.Ref) string {
	names := make([]string, len(refs))
	for i, ref := range refs {
		names[i] = ref.String()
	}
	return strings.Join(names, ", ")
}

// supersededLabel marks an entry that other entries supersede. Every
// successor is named, since none of them is more current than another.
func supersededLabel(supersededBy []api.Ref) string {
	if len(supersededBy) == 0 {
		return ""
	}
	return " (superseded by " + joinRefs(supersededBy) + ")"
}

// formatPath shows ancestors from the root down. An ancestor the
// request cannot read shows as its bare id.
func formatPath(path []api.EntryRef) string {
	parts := make([]string, len(path))
	for i, entry := range path {
		parts[i] = entry.Ref.String()
		if entry.Subject != "" {
			parts[i] += " " + oneLine(entry.Subject) +
				supersededLabel(entry.SupersededBy)
		}
	}
	return strings.Join(parts, " > ")
}

func printEntryLine(e api.ListedEntry, order listOrder, layout string) {
	below := ""
	if order == byActivity && e.Descendants > 0 {
		below = fmt.Sprintf(" (+%d below)", e.Descendants)
	}
	fmt.Printf("[%s] (%s) %s: %s%s%s\n", e.Ref, e.Type,
		listTime(e, order).Local().Format(layout), oneLine(e.Subject),
		supersededLabel(e.SupersededBy), below)
	if len(e.Ancestors) > 0 {
		fmt.Println("    in " + formatPath(e.Ancestors))
	}
}

func printEntryLines(entries []api.ListedEntry, order listOrder) {
	var times []time.Time
	for _, e := range entries {
		times = append(times, listTime(e, order))
	}
	layout := timeLayout(times)
	for _, e := range entries {
		printEntryLine(e, order, layout)
	}
}

func printSearchResult(
	result api.SearchResult, offset, limit int, order listOrder, idOnly bool,
) {
	if idOnly {
		for _, e := range result.Entries {
			fmt.Println(e.Ref)
		}
		return
	}
	if len(result.Entries) == 0 && offset > 0 {
		fmt.Printf("No entries at offset %d.\n", offset)
		return
	}
	if len(result.Entries) == 0 {
		fmt.Println("No matching entries.")
		return
	}
	printEntryLines(result.Entries, order)
	if result.More {
		fmt.Printf("--- more entries (use --offset %d to view older) ---\n",
			offset+limit)
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

// printBacklinks shows the links other entries hold to this one, apart
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
				fmt.Sprintf("%s %s this entry", link.Ref, link.Name))
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

func printEntryDetail(d api.EntryDetail) {
	e := d.Entry
	fmt.Printf("[%s] (%s) %s\n", e.Ref, e.Type, e.Subject)
	if len(e.SupersededBy) > 0 {
		fmt.Println("Superseded by " + joinRefs(e.SupersededBy))
	}
	fmt.Println()
	fmt.Println("Created: " + localTimeDetail(e.Created))
	fmt.Println("Modified: " + localTimeDetail(e.Modified))
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
		newest := api.MessageRef{Entry: e.Ref, Number: d.LastMessage}
		fmt.Printf("Chat: %d messages, newest %s (see '%s chat recent %s')\n",
			d.Messages, newest, programName, e.Ref)
	}
	if e.Body != "" {
		fmt.Println()
		fmt.Println(e.Body)
	}
}

// printTree prints an entry's subtree, each entry indented under its
// parent, with the path to the entry above it and a marker wherever
// the tree was cut.
func printTree(tree api.TreeResponse, root api.Ref) {
	if len(tree.Ancestors) > 0 {
		fmt.Println("Path: " + formatPath(tree.Ancestors))
	}
	children := map[api.Ref][]api.TreeEntry{}
	var times []time.Time
	for _, e := range tree.Entries {
		times = append(times, e.Modified)
		for _, link := range e.Links {
			if link.Name == api.ParentLink {
				children[link.Ref] = append(children[link.Ref], e)
			}
		}
	}
	layout := timeLayout(times)

	var printFrom func(e api.TreeEntry, depth int)
	printFrom = func(e api.TreeEntry, depth int) {
		indent := strings.Repeat("  ", depth)
		fmt.Printf("%s[%s] (%s) %s: %s%s\n", indent, e.Ref, e.Type,
			e.Modified.Local().Format(layout), oneLine(e.Subject),
			supersededLabel(e.SupersededBy))
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
	for _, e := range tree.Entries {
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
// its text indented beneath.
func printMessage(m api.Message, indent, layout string) {
	edited := ""
	if !m.Modified.Equal(m.Created) {
		edited = " (edited)"
	}
	lines := strings.Split(m.Body, "\n")
	fmt.Printf("%s[%s] %s %s%s: %s\n", indent, m.Ref(),
		m.Created.Local().Format(layout), m.Origin.User, edited, lines[0])
	for _, line := range lines[1:] {
		fmt.Printf("%s    %s\n", indent, line)
	}
}

// printCursorLine ends a chat output with the cursor to read on from,
// as the command that uses it.
func printCursorLine(entry api.Ref, page api.ChatPage) {
	if page.Remaining > 0 {
		noun := "messages"
		if page.Remaining == 1 {
			noun = "message"
		}
		fmt.Printf("Next: %s chat read %s --since %d (%d more %s)\n",
			programName, entry, page.Cursor, page.Remaining, noun)
		return
	}
	fmt.Printf("Cursor: %d (wait for more: %s chat wait %s --since %d)\n",
		page.Cursor, programName, entry, page.Cursor)
}

// printChatPage prints one chat's messages oldest first, then the
// cursor line.
func printChatPage(entry api.Ref, page api.ChatPage, empty string) {
	if len(page.Messages) == 0 {
		fmt.Println(empty)
	}
	layout := timeLayout(messageTimes(page.Messages))
	for _, m := range page.Messages {
		printMessage(m, "", layout)
	}
	printCursorLine(entry, page)
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
		fmt.Printf("[%s] (%s) %s%s\n", chat.Entry.Ref, chat.Entry.Type,
			oneLine(chat.Entry.Subject),
			supersededLabel(chat.Entry.SupersededBy))
		for _, m := range chat.Messages {
			printMessage(m, "  ", layout)
		}
	}
}
