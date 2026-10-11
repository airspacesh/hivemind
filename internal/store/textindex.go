package store

import (
	"context"
	"fmt"
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

// findTextCandidates lists the entries of a workspace whose text holds
// the literal text the search requires, and when the search covers
// chat, the messages whose text does, by looking it up in the text
// indexes. It reports false when the search names no text the indexes
// can look up for the entries, or when a lookup finds more than limit
// rows.
func (v view) findTextCandidates(
	ctx context.Context,
	workspace int64,
	filter Filter,
	limit int,
) (candidateSet, bool, error) {
	var entryQuery indexQuery
	entryQuery.add(filter.Subject)
	entryQuery.add(filter.Body)
	entryQuery.add(filter.Content)
	var found candidateSet
	var ok bool
	var err error
	found.entries, ok, err = v.lookUpText(ctx, `
		SELECT rowid & 0xffffffff FROM entry_text
		WHERE entry_text MATCH ? AND rowid BETWEEN ? AND ? LIMIT ?`,
		entryQuery, workspace, limit)
	if err != nil || !ok {
		return candidateSet{}, false, err
	}
	if !searchesChat(filter) {
		return found, true, nil
	}

	var messageQuery indexQuery
	messageQuery.add(filter.Content)
	if len(messageQuery) == 0 {
		// A matching message's entry holds the subject and body text,
		// so with no text for the messages, the messages in the chats
		// of the entries found are the candidates.
		found.messages, ok, err = v.readCandidates(ctx, `
			SELECT m.post_order FROM json_each(?) AS candidate `+eachChat+`
				ON m.workspace_id = ? AND m.entry_id = candidate.value
			LIMIT ?`, limit, idsJSON(found.entries), workspace)
		if err != nil {
			return candidateSet{}, false, fmt.Errorf(
				"reading the chats of search candidates: %w", err)
		}
	} else {
		found.messages, ok, err = v.lookUpText(ctx, `
			SELECT rowid & 0xffffffff FROM message_text
			WHERE message_text MATCH ? AND rowid BETWEEN ? AND ? LIMIT ?`,
			messageQuery, workspace, limit)
		if err != nil {
			return candidateSet{}, false, err
		}
	}
	if !ok {
		return candidateSet{}, false, nil
	}
	return found, true, nil
}

// lookUpText runs a lookup in a text index, restricted to one
// workspace's rows, and lists the ids it selects. It reports false
// when the query is empty or finds more than limit rows.
func (v view) lookUpText(
	ctx context.Context,
	lookup string,
	query indexQuery,
	workspace int64,
	limit int,
) ([]int64, bool, error) {
	if len(query) == 0 {
		return nil, false, nil
	}
	first := workspace << 32
	ids, ok, err := v.readCandidates(ctx, lookup, limit,
		strings.Join(query, " AND "), first, first|0xffffffff)
	if err != nil {
		return nil, false, fmt.Errorf("looking up search text: %w", err)
	}
	return ids, ok, nil
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
