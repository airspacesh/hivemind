package store

import (
	"context"
	"strings"
	"unicode/utf8"
)

// trigramRunes is the length of the trigrams the text indexes hold, so
// the shortest literal text they can look up.
const trigramRunes = 3

// literalRuns lists the runs of literal characters in a GLOB pattern,
// between its wildcards and character classes. Every value the pattern
// matches holds each run. An unclosed class matches nothing, so the
// runs before it are enough.
func literalRuns(pattern string) []string {
	if !utf8.ValidString(pattern) {
		return nil
	}
	var runs []string
	var run []rune
	endRun := func() {
		if len(run) > 0 {
			runs = append(runs, string(run))
			run = run[:0]
		}
	}
	characters := []rune(pattern)
	for i := 0; i < len(characters); i++ {
		switch characters[i] {
		case '*', '?':
			endRun()
		case '[':
			endRun()
			// A class may start with ^, and a ] straight after the
			// opening [ or ^ is a member rather than the close.
			end := i + 1
			if end < len(characters) && characters[end] == '^' {
				end++
			}
			if end < len(characters) && characters[end] == ']' {
				end++
			}
			for end < len(characters) && characters[end] != ']' {
				end++
			}
			if end == len(characters) {
				return runs
			}
			i = end
		default:
			run = append(run, characters[i])
		}
	}
	endRun()
	return runs
}

// indexQuery is an FTS5 query for the rows of a text index that hold
// every trigram of the literal text of some search patterns. The
// indexes keep no positions, so a row may hold them apart.
type indexQuery []string

// add asks for the trigrams of the literal text the given patterns
// require. Excluded patterns and runs too short for a trigram narrow
// nothing.
func (q *indexQuery) add(conditions []FieldCond) {
	for _, condition := range conditions {
		if condition.Negate {
			continue
		}
		for _, run := range literalRuns(lowerASCII(condition.Value)) {
			characters := []rune(run)
			for i := 0; i+trigramRunes <= len(characters); i++ {
				trigram := string(characters[i : i+trigramRunes])
				*q = append(*q,
					`"`+strings.ReplaceAll(trigram, `"`, `""`)+`"`)
			}
		}
	}
}

// sql is the query as FTS5 takes it.
func (q indexQuery) sql() string {
	return strings.Join(q, " AND ")
}

// textLookup looks up the posts whose text holds the literal text the
// search requires, and the messages whose text does, in the text
// indexes. Without content filters, the messages that may match are
// those in the chats of the posts found. It reports false when the
// search names no text the indexes can look up for the posts.
func textLookup(workspace int64, filter Filter) (candidateLookup, bool) {
	var postQuery indexQuery
	postQuery.add(filter.Subject)
	postQuery.add(filter.Body)
	postQuery.add(filter.Content)
	if len(postQuery) == 0 {
		return candidateLookup{}, false
	}
	first := workspace << 32
	lookup := candidateLookup{
		what: "search text",
		posts: `
			SELECT rowid & 0xffffffff FROM post_text
			WHERE post_text MATCH ? AND rowid BETWEEN ? AND ? LIMIT ?`,
		postArgs: []any{postQuery.sql(), first, first | 0xffffffff},
	}

	var messageQuery indexQuery
	messageQuery.add(filter.Content)
	if len(messageQuery) > 0 {
		lookup.messages = `
			SELECT rowid & 0xffffffff FROM message_text
			WHERE message_text MATCH ? AND rowid BETWEEN ? AND ? LIMIT ?`
		lookup.messageArgs = []any{messageQuery.sql(), first,
			first | 0xffffffff}
	}
	return lookup, true
}

// readCandidates runs a lookup of candidates for a search, given its
// arguments and then its limit, and lists the ids it selects. It
// reports false when the lookup finds more than limit rows.
func (v view) readCandidates(
	ctx context.Context, lookup string, limit int, args ...any,
) ([]int64, bool, error) {
	rows, err := v.tx.QueryContext(ctx, lookup, append(args, limit+1)...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, false, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if len(ids) > limit {
		return nil, false, nil
	}
	return ids, true, nil
}
