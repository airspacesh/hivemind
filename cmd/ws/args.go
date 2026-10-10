package main

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// flagSpec declares the flags a subcommand accepts. The parser is
// hand-rolled instead of using the flag package so flags can appear
// after positional arguments (`ws add todo "text" --project x`).
type flagSpec struct {
	strs   []string          // flags taking a value
	bools  []string          // flags taking no value
	repeat []string          // repeatable flags taking a value
	alias  map[string]string // short form -> canonical flag
}

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
		if canonical, ok := spec.alias[name]; ok {
			name = canonical
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
		default:
			return nil, fmt.Errorf("unknown flag: %s", name)
		}
	}
	return p, nil
}

var searchFields = []string{
	"subject", "body", "content", "type", "project", "key", "meta",
	"origin-user", "origin-host", "origin-dir", "origin-claude-session",
}

// shorthandKeys are metadata keys with a convenience flag on add,
// edit, and search: --jira QUACK-1 is --meta jira=QUACK-1.
var shorthandKeys = []string{"jira", "confluence"}

func searchFlagSpec() flagSpec {
	spec := flagSpec{
		strs: []string{"--limit", "--offset", "--under", "--workspace"},
		bools: []string{"--order-by-creation", "--order-by-modified",
			"--id-only", "--include-superseded", "--flat"},
		alias: map[string]string{"-n": "--limit"},
	}
	for _, field := range searchFields {
		spec.repeat = append(spec.repeat, "--"+field, "--no-"+field)
	}
	for _, key := range shorthandKeys {
		spec.repeat = append(spec.repeat, "--"+key, "--no-"+key)
	}
	return spec
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

func parseOffset(value string) (int, error) {
	n, err := strconv.Atoi(value)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("--offset must be a non-negative integer")
	}
	return n, nil
}
