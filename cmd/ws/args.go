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
		case slices.Contains(spec.bools, name):
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

// includeDeleted is the flag of every listing that leaves out deleted
// posts and messages, which shows them.
const includeDeleted = "--include-deleted"

// addIncludeDeleted passes on a command's includeDeleted to its query.
func addIncludeDeleted(p *parsedArgs, query url.Values) {
	if p.bools[includeDeleted] {
		query.Set("include-deleted", "true")
	}
}

// includeDeletedArg repeats a command's includeDeleted after a space,
// for a command it suggests.
func includeDeletedArg(p *parsedArgs) string {
	if p.bools[includeDeleted] {
		return " " + includeDeleted
	}
	return ""
}

func searchFlagSpec() flagSpec {
	return flagSpec{
		strs: []string{"--limit", "--under", "--workspace", "--after",
			"--before"},
		bools: []string{"--id-only", "--include-superseded",
			includeDeleted, "--posts-only", "--include-chat"},
		repeat: searchFilterFlags(),
	}
}

// mineFlags choose whether a wait or firehose leaves out your own
// changes.
var mineFlags = []string{"--exclude-mine", "--include-mine"}

// excludesMine reads whether a command leaves out your own changes,
// given what it does by default.
func excludesMine(p *parsedArgs, byDefault bool) (bool, error) {
	switch {
	case p.has("--exclude-mine") && p.has("--include-mine"):
		return false, fmt.Errorf("give one of --exclude-mine and " +
			"--include-mine")
	case p.has("--exclude-mine"):
		return true, nil
	case p.has("--include-mine"):
		return false, nil
	default:
		return byDefault, nil
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
	for _, flag := range mineFlags {
		if p.has(flag) {
			text += " " + flag
		}
	}
	return text
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

// parseBoundFlag reads a flag that bounds a listing: a revision like
// @812345, a span back from now like 2d, 6h or 90m, a date or a date
// and time in local time, or an RFC 3339 time. A span ends on a whole
// second, so a command that repeats the bound as a time stays short.
func parseBoundFlag(flag, value string, now time.Time) (api.Bound, error) {
	if strings.HasPrefix(value, "@") {
		bound, err := api.ParseBound(value)
		if err != nil {
			return api.Bound{}, fmt.Errorf("%s: %w", flag, err)
		}
		return bound, nil
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
		"span back from now like 2d, 6h or 90m, a date like 2026-10-09, "+
		"or a date and time like 2026-10-09T14:30, not %q", flag, value)
}

// parseChangeBoundFlag reads a flag that takes where a read of changes
// starts: a revision like @812345, @0 for the start, or the id of a post
// or message, for what changed after it was posted.
func parseChangeBoundFlag(flag, value string) (api.ChangeBound, error) {
	bound, err := api.ParseChangeBound(value)
	if err == nil {
		return bound, nil
	}
	if strings.HasPrefix(value, "@") {
		return api.ChangeBound{}, fmt.Errorf("%s: %w", flag, err)
	}
	if _, err := parseBoundFlag(flag, value, time.Now()); err == nil {
		return api.ChangeBound{}, fmt.Errorf("%s takes a revision like "+
			"@812345 or an id like e40-m3, not a time, because the server "+
			"reads changes by revision. To list what changed after a "+
			"time, run '%s recent --include-chat %s %s'", flag,
			programName, flag, shellQuote(value))
	}
	return api.ChangeBound{}, fmt.Errorf("%s must be a revision like "+
		"@812345, @0 for the start, or a post or message id like e12 or "+
		"e40-m3, not %q", flag, value)
}
