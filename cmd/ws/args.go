package main

import (
	"fmt"
	"maps"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/airspacesh/workstream/internal/api"
)

// flagSpec declares the flags a subcommand accepts. The parser is
// hand-rolled instead of using the flag package so flags can appear
// after positional arguments (`ws add "text" --project x`).
type flagSpec struct {
	strs   []string // flags taking a value
	bools  []string // flags taking no value
	repeat []string // repeatable flags taking a value
	// toggles are pairs of flags taking no value, at most one of each.
	toggles []toggle
	// passUnknown keeps an unknown flag as a positional argument, so
	// free text that starts with - needs no --.
	passUnknown bool
}

// shortForms are the short forms of flags, each accepted wherever its
// flag is.
var shortForms = map[string]string{"-n": "--limit"}

type parsedArgs struct {
	pos     []string
	strs    map[string]string
	lists   map[string][]string
	bools   map[string]bool
	present map[string]bool
}

func (p *parsedArgs) has(flag string) bool {
	return p.present[flag]
}

func parseArgs(args []string, spec flagSpec) (*parsedArgs, error) {
	p := &parsedArgs{
		strs:    map[string]string{},
		lists:   map[string][]string{},
		bools:   map[string]bool{},
		present: map[string]bool{},
	}
	parseFlags := true
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if parseFlags && arg == "--" {
			parseFlags = false
			continue
		}
		if !parseFlags || !strings.HasPrefix(arg, "-") || arg == "-" {
			p.pos = append(p.pos, arg)
			continue
		}
		name := arg
		value := ""
		hasValue := false
		if idx := strings.Index(arg, "="); idx >= 0 {
			name = arg[:idx]
			value = arg[idx+1:]
			hasValue = true
		}
		typed := name
		if long, ok := shortForms[name]; ok {
			name = long
		}
		switch {
		case slices.Contains(spec.bools, name) ||
			slices.ContainsFunc(spec.toggles, func(t toggle) bool {
				return name == t.on || name == t.off
			}):
			if hasValue {
				return nil, fmt.Errorf("%s does not take a value", name)
			}
			if p.present[name] {
				return nil, fmt.Errorf("%s given more than once", name)
			}
			p.bools[name] = true
			p.present[name] = true
		case slices.Contains(spec.strs, name):
			if !hasValue {
				i++
				if i >= len(args) {
					return nil, fmt.Errorf("%s requires a value", name)
				}
				value = args[i]
			}
			if p.present[name] {
				return nil, fmt.Errorf("%s given more than once", name)
			}
			p.strs[name] = value
			p.present[name] = true
		case slices.Contains(spec.repeat, name):
			if !hasValue {
				i++
				if i >= len(args) {
					return nil, fmt.Errorf("%s requires a value", name)
				}
				value = args[i]
			}
			p.lists[name] = append(p.lists[name], value)
			p.present[name] = true
		case spec.passUnknown:
			p.pos = append(p.pos, arg)
		default:
			return nil, fmt.Errorf("unknown flag: %s", typed)
		}
	}
	for _, t := range spec.toggles {
		if p.has(t.on) && p.has(t.off) {
			return nil, fmt.Errorf("give one of %s and %s", t.on, t.off)
		}
	}
	return p, nil
}

// shorthandKeys are metadata keys with a convenience flag on add,
// edit, and search: --jira QUACK-1 is --meta jira=QUACK-1.
var shorthandKeys = []string{"jira", "confluence"}

// filterFlags names the flags of filters, each with its no- form.
func filterFlags(filters ...string) []string {
	var flags []string
	for _, filter := range filters {
		flags = append(flags, "--"+filter, "--no-"+filter)
	}
	return flags
}

// searchFilterFlags are the flags of every search filter, the
// metadata shorthands included.
func searchFilterFlags() []string {
	return filterFlags(append(slices.Clone(api.SearchFields),
		shorthandKeys...)...)
}

// toggle is a pair of flags that turn one thing on and off, like
// --include-chat and --exclude-chat. A command takes at most one of the
// pair, and does as its own default says without either.
type toggle struct {
	on, off string
}

// The inclusion flags. The help documents them together, under
// Inclusion flags.
var (
	chatToggle       = toggle{on: "--include-chat", off: "--exclude-chat"}
	postsToggle      = toggle{on: "--include-posts", off: "--exclude-posts"}
	mineToggle       = toggle{on: "--exclude-mine", off: "--include-mine"}
	deletedToggle    = toggle{on: "--include-deleted", off: "--exclude-deleted"}
	supersededToggle = toggle{on: "--include-superseded",
		off: "--exclude-superseded"}
)

// isOn reads whether a command's toggle is on, given its default.
func (t toggle) isOn(p *parsedArgs, byDefault bool) bool {
	switch {
	case p.has(t.on):
		return true
	case p.has(t.off):
		return false
	default:
		return byDefault
	}
}

// addTo passes the toggle on to a query, as the parameter its on flag
// names, when a command was given that flag. Without it, the server
// leaves the thing out.
func (t toggle) addTo(p *parsedArgs, query url.Values) {
	if p.has(t.on) {
		query.Set(strings.TrimPrefix(t.on, "--"), "true")
	}
}

// arg repeats the flag of the toggle a command was given, after a
// space, so a command it suggests does the same.
func (t toggle) arg(p *parsedArgs) string {
	for _, flag := range []string{t.on, t.off} {
		if p.has(flag) {
			return " " + flag
		}
	}
	return ""
}

// itemToggles are the toggles that choose whether a listing or wait
// holds posts, messages, or both.
var itemToggles = []toggle{postsToggle, chatToggle}

// readItems reads whether a listing or wait holds posts, messages, or
// both, given whether the command holds messages by default. Posts it
// holds by default.
func readItems(
	p *parsedArgs, command string, chatByDefault bool,
) (api.Items, error) {
	posts := postsToggle.isOn(p, true)
	chat := chatToggle.isOn(p, chatByDefault)
	switch {
	case posts && chat:
		return api.PostsAndChat, nil
	case posts:
		return api.PostsOnly, nil
	case chat:
		return api.ChatOnly, nil
	case p.has(chatToggle.off):
		return 0, fmt.Errorf("%s and %s leave nothing to show. Give at "+
			"most one", postsToggle.off, chatToggle.off)
	default:
		return 0, fmt.Errorf("%s leaves nothing to show, since %s leaves "+
			"out chat unless %s. Add %s to show only messages",
			postsToggle.off, command, chatToggle.on, chatToggle.on)
	}
}

// itemArgs repeats the item flags a command was given, each after a
// space, so a command it suggests holds the same.
func itemArgs(p *parsedArgs) string {
	return postsToggle.arg(p) + chatToggle.arg(p)
}

func searchFlagSpec() flagSpec {
	return flagSpec{
		strs: []string{"--limit", "--under", "--workspace", "--after",
			"--before"},
		bools: []string{"--id-only"},
		toggles: append([]toggle{supersededToggle, deletedToggle},
			itemToggles...),
		repeat: searchFilterFlags(),
	}
}

var shellSafe = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

// shellQuote quotes text for a POSIX shell, unless it needs none.
func shellQuote(text string) string {
	if shellSafe.MatchString(text) {
		return text
	}
	return "'" + strings.ReplaceAll(text, "'", `'\''`) + "'"
}

// conditionArgs repeats the filter conditions a command was given,
// each after a space and quoted for the shell.
func conditionArgs(p *parsedArgs) string {
	var text strings.Builder
	for _, flag := range slices.Sorted(maps.Keys(p.lists)) {
		for _, value := range p.lists[flag] {
			text.WriteString(" " + flag + " " + shellQuote(value))
		}
	}
	return text.String()
}

// filterArgs repeats the filters a command was given, each after a
// space and quoted for the shell, so a command it suggests filters the
// same way.
func filterArgs(p *parsedArgs) string {
	text := conditionArgs(p)
	if p.has("--under") {
		text += " --under " + shellQuote(p.strs["--under"])
	}
	return text + itemArgs(p) + mineToggle.arg(p)
}

// shorthandFor reports whether a flag is a metadata-key shorthand
// (--jira, --no-jira, ...) and returns its key and whether it
// negates.
func shorthandFor(flag string) (key string, negate bool, ok bool) {
	name := strings.TrimPrefix(flag, "--")
	negate = strings.HasPrefix(name, "no-")
	name = strings.TrimPrefix(name, "no-")
	for _, key := range shorthandKeys {
		if name == key {
			return key, negate, true
		}
	}
	return "", false, false
}

// parsePositive reads a count flag. The server checks its upper bound.
func parsePositive(flag, value string) (int, error) {
	n, err := strconv.Atoi(value)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("%s must be a positive integer, not %q",
			flag, value)
	}
	return n, nil
}

// localTimeLayouts are the dates, and dates and times, a time flag
// reads in local time.
var localTimeLayouts = []string{
	"2006-01-02", "2006-01-02T15:04", "2006-01-02 15:04",
	"2006-01-02T15:04:05", "2006-01-02 15:04:05",
}

// parseBoundFlag reads an --after or --before, which every command
// that takes one reads alike: a revision like @812345, the id of a post
// or message, like e12 or e40-m3, for when it was posted, a span back
// from now like 2d, 6h or 90m, a date or a date and time in local time,
// or an RFC 3339 time. A span ends on a whole second, so a command that
// repeats the bound as a time stays short.
func parseBoundFlag(flag, value string, now time.Time) (api.Bound, error) {
	if strings.HasPrefix(value, "@") {
		revision, err := api.ParseRevision(value)
		if err != nil {
			return api.Bound{}, fmt.Errorf("%s: %w", flag, err)
		}
		return api.Bound{Revision: revision}, nil
	}
	if message, err := api.ParseMessageCursor(value); err == nil {
		return api.Bound{Post: message.Post, Message: message.Number}, nil
	}
	if post, err := api.ParseRef(value); err == nil {
		return api.Bound{Post: post}, nil
	}
	if days, ok := strings.CutSuffix(value, "d"); ok {
		if n, err := strconv.Atoi(days); err == nil && n >= 0 {
			at := now.Add(-time.Duration(n) * 24 * time.Hour)
			return api.Bound{Time: at.Truncate(time.Second)}, nil
		}
	}
	if span, err := time.ParseDuration(value); err == nil && span >= 0 {
		return api.Bound{Time: now.Add(-span).Truncate(time.Second)}, nil
	}
	for _, layout := range localTimeLayouts {
		at, err := time.ParseInLocation(layout, value, time.Local)
		if err == nil {
			return api.Bound{Time: at}, nil
		}
	}
	if at, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return api.Bound{Time: at}, nil
	}
	return api.Bound{}, fmt.Errorf("%s must be a revision like @812345, a "+
		"post or message id like e12 or e40-m3, a span back from now like "+
		"2d, 6h or 90m, a date like 2026-10-09, or a date and time like "+
		"2026-10-09T14:30, not %q", flag, value)
}

// boundArg writes a bound as parseBoundFlag reads it, a time in local
// time with its offset, to the nanosecond when the time has any.
func boundArg(bound api.Bound) string {
	if bound.Post.ID == 0 && !bound.Time.IsZero() {
		return bound.Time.Local().Format(time.RFC3339Nano)
	}
	return bound.String()
}
