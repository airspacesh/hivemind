package store

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/airspacesh/workstream/internal/api"
)

const (
	MaxSearchConditions    = 32
	MaxSearchLimit         = 500
	MaxSearchPatternLength = 4096
)

var ErrInvalidFilter = errors.New("invalid search filter")

type FieldCond struct {
	Negate bool
	Value  string
}

type MetaCond struct {
	Negate bool
	Key    string
	Value  string
}

type Filter struct {
	Subject             []FieldCond
	Body                []FieldCond
	Type                []FieldCond
	Project             []FieldCond
	Key                 []FieldCond
	Content             []FieldCond
	Meta                []MetaCond
	OriginUser          []FieldCond
	OriginHost          []FieldCond
	OriginDir           []FieldCond
	OriginClaudeSession []FieldCond

	View View
	// EntriesOnly keeps a flat search for text to entries, leaving out
	// the messages in their chats.
	EntriesOnly bool
	// Under limits the search to an entry and its descendants, when set.
	Under             *Key
	OrderByCreation   bool
	IncludeSuperseded bool
	Limit             int
	Offset            int
}

// View chooses which entries a search lists, and in what order.
type View int

const (
	// ViewFlat lists every matching entry by its own change time.
	ViewFlat View = iota
	// ViewTopics lists the top-level entries of a workspace by the
	// latest change anywhere in their trees. An entry whose parent is
	// in another workspace is top-level in its own. A superseded entry
	// stays while a current entry is under it, as context for it.
	ViewTopics
)

type whereBuilder struct {
	clauses []string
	args    []any
}

func conditionCount(filter Filter) int {
	return len(filter.Subject) + len(filter.Body) + len(filter.Type) +
		len(filter.Project) + len(filter.Key) + len(filter.Content) +
		len(filter.Meta) + len(filter.OriginUser) + len(filter.OriginHost) +
		len(filter.OriginDir) + len(filter.OriginClaudeSession)
}

func (builder *whereBuilder) addField(
	name string,
	column string,
	conditions []FieldCond,
) error {
	for _, condition := range conditions {
		pattern, err := loweredPattern(name, condition.Value)
		if err != nil {
			return err
		}
		clause := column + ` GLOB ?`
		if condition.Negate {
			clause = `NOT (` + clause + `)`
		}
		builder.clauses = append(builder.clauses, clause)
		builder.args = append(builder.args, pattern)
	}
	return nil
}

func (builder *whereBuilder) addKeys(conditions []FieldCond) error {
	for _, condition := range conditions {
		pattern, err := loweredPattern("key", condition.Value)
		if err != nil {
			return err
		}
		clause := `EXISTS (
			SELECT 1 FROM entry_metadata AS key_meta
			WHERE key_meta.workspace_id = e.workspace_id
				AND key_meta.entry_id = e.id AND key_meta.key GLOB ?
		)`
		if condition.Negate {
			clause = `NOT ` + clause
		}
		builder.clauses = append(builder.clauses, clause)
		builder.args = append(builder.args, pattern)
	}
	return nil
}

func (builder *whereBuilder) addContent(
	conditions []FieldCond,
) error {
	for _, condition := range conditions {
		pattern, err := loweredPattern("content", condition.Value)
		if err != nil {
			return err
		}
		clause := `(e.subject_lower GLOB ? OR e.body_lower GLOB ?)`
		if condition.Negate {
			clause = `NOT ` + clause
		}
		builder.clauses = append(builder.clauses, clause)
		builder.args = append(builder.args, pattern, pattern)
	}
	return nil
}

func (builder *whereBuilder) addMetadata(conditions []MetaCond) error {
	for _, condition := range conditions {
		keyPattern, err := loweredPattern("metadata key", condition.Key)
		if err != nil {
			return err
		}
		valuePattern, err := loweredPattern(
			"metadata value", condition.Value,
		)
		if err != nil {
			return err
		}
		clause := `EXISTS (
			SELECT 1 FROM entry_metadata AS pair_meta
			WHERE pair_meta.workspace_id = e.workspace_id
				AND pair_meta.entry_id = e.id
				AND pair_meta.key GLOB ?
				AND pair_meta.value_lower GLOB ?
		)`
		if condition.Negate {
			clause = `NOT ` + clause
		}
		builder.clauses = append(builder.clauses, clause)
		builder.args = append(
			builder.args, keyPattern, valuePattern,
		)
	}
	return nil
}

// matchAnywhere widens patterns for the fields holding prose, so they
// match any part of the text the way a search box does. Type, project,
// key, origin, and metadata patterns stay whole-value matches, because
// they name one of a known set rather than describe its contents.
//
// An empty pattern is left alone, so it matches only an empty value.
// Widening it would match everything, which is what leaving the filter
// out already does.
func matchAnywhere(conditions []FieldCond) []FieldCond {
	widened := make([]FieldCond, len(conditions))
	for index, condition := range conditions {
		value := condition.Value
		if value != "" {
			value = "*" + value + "*"
		}
		widened[index] = FieldCond{Negate: condition.Negate, Value: value}
	}
	return widened
}

func buildWhere(filter Filter) (whereBuilder, error) {
	var builder whereBuilder
	fields := []struct {
		name       string
		column     string
		conditions []FieldCond
	}{
		{"subject", "e.subject_lower", matchAnywhere(filter.Subject)},
		{"body", "e.body_lower", matchAnywhere(filter.Body)},
		{"type", "e.type_lower", filter.Type},
		{"project", "e.project_lower", filter.Project},
		{"origin-user", "e.origin_user_lower", filter.OriginUser},
		{"origin-host", "e.origin_host_lower", filter.OriginHost},
		{"origin-dir", "e.origin_dir_lower", filter.OriginDir},
		{"origin-claude-session", "e.origin_claude_session_lower", filter.OriginClaudeSession},
	}
	for _, field := range fields {
		if err := builder.addField(
			field.name, field.column, field.conditions,
		); err != nil {
			return whereBuilder{}, err
		}
	}
	if err := builder.addKeys(filter.Key); err != nil {
		return whereBuilder{}, err
	}
	if err := builder.addContent(matchAnywhere(filter.Content)); err != nil {
		return whereBuilder{}, err
	}
	if err := builder.addMetadata(filter.Meta); err != nil {
		return whereBuilder{}, err
	}
	return builder, nil
}

func (builder whereBuilder) sql() string {
	if len(builder.clauses) == 0 {
		return ""
	}
	return " WHERE " + strings.Join(builder.clauses, " AND ")
}

func loweredPattern(name, pattern string) (string, error) {
	if strings.IndexByte(pattern, 0) >= 0 {
		return "", fmt.Errorf("%w: %s pattern contains NUL",
			ErrInvalidFilter, name)
	}
	if utf8.RuneCountInString(pattern) > MaxSearchPatternLength {
		return "", fmt.Errorf(
			"%w: %s pattern exceeds %d characters",
			ErrInvalidFilter, name, MaxSearchPatternLength,
		)
	}
	return lowerASCII(pattern), nil
}

func lowerASCII(value string) string {
	var folded []byte
	for index := 0; index < len(value); index++ {
		character := value[index]
		if character < 'A' || character > 'Z' {
			continue
		}
		if folded == nil {
			folded = []byte(value)
		}
		folded[index] = character + ('a' - 'A')
	}
	if folded == nil {
		return value
	}
	return string(folded)
}

// Search lists the entries of a workspace that match a filter.
func (s *Store) Search(
	ctx context.Context,
	access Access,
	workspace int64,
	filter Filter,
) (api.SearchResult, error) {
	if conditionCount(filter) > MaxSearchConditions {
		return api.SearchResult{}, fmt.Errorf(
			"%w: at most %d search conditions are allowed",
			ErrInvalidFilter, MaxSearchConditions,
		)
	}
	if filter.Limit < 1 || filter.Limit > MaxSearchLimit {
		return api.SearchResult{}, fmt.Errorf(
			"%w: limit must be between 1 and %d",
			ErrInvalidFilter, MaxSearchLimit,
		)
	}
	if filter.Offset < 0 {
		return api.SearchResult{}, fmt.Errorf(
			"%w: offset must not be negative", ErrInvalidFilter,
		)
	}
	if filter.View == ViewTopics && filter.OrderByCreation {
		return api.SearchResult{}, fmt.Errorf(
			"%w: ordering by creation applies to the flat view only",
			ErrInvalidFilter,
		)
	}
	if filter.View == ViewTopics && filter.EntriesOnly {
		return api.SearchResult{}, fmt.Errorf(
			"%w: the topics view searches entries only already",
			ErrInvalidFilter,
		)
	}
	where, err := buildWhere(filter)
	if err != nil {
		return api.SearchResult{}, err
	}
	from, fromArgs := joinedFrom(filter.Under)

	v, err := s.begin(ctx, access, true)
	if err != nil {
		return api.SearchResult{}, fmt.Errorf("starting search: %w", err)
	}
	defer v.tx.Rollback()
	if filter.Under != nil {
		if err := v.checkEntryExists(ctx, *filter.Under); err != nil {
			return api.SearchResult{}, err
		}
	}
	var result api.SearchResult
	if filter.View == ViewTopics {
		where.args = append(fromArgs, where.args...)
		where.clauses = append(where.clauses, `e.workspace_id = ?`)
		where.args = append(where.args, workspace)
		result, err = v.searchTopics(ctx, workspace, filter, from, where,
			topicWalks(filter))
	} else {
		result, err = v.searchFlat(ctx, workspace, filter, from, fromArgs,
			where)
	}
	if err != nil {
		return api.SearchResult{}, err
	}
	if err := v.tx.Commit(); err != nil {
		return api.SearchResult{}, fmt.Errorf("finishing search: %w", err)
	}
	return result, nil
}

// messagesPerMatch is how many matching messages a search shows under
// each entry.
const messagesPerMatch = 3

// searchesChat reports whether a flat search matches messages as well
// as entries, which it does when it requires some text, unless it
// keeps to entries.
func searchesChat(filter Filter) bool {
	if filter.EntriesOnly {
		return false
	}
	for _, condition := range filter.Content {
		if !condition.Negate {
			return true
		}
	}
	return false
}

// keepTo limits a where clause on entries e to the entries of a
// workspace the search shows.
func (builder *whereBuilder) keepTo(workspace int64, filter Filter) {
	builder.clauses = append(builder.clauses, `e.workspace_id = ?`)
	builder.args = append(builder.args, workspace)
	if !filter.IncludeSuperseded {
		builder.clauses = append(builder.clauses, `NOT `+supersededClause)
	}
}

// matchQuery selects a flat search's matches as entry ids, each with
// the time it ranks by, newest first. A match is an entry the search
// matches, or when it searches chat, a message the search's text
// matches in the chat of an entry the rest of the search matches. It
// walks the entries and messages newest first, so a reader can stop
// once it has a page.
func matchQuery(
	workspace int64,
	filter Filter,
	from string,
	fromArgs []any,
	where whereBuilder,
) (string, []any, error) {
	entryKey, messageKey, join := `e.modified_utc`, `m.modified_utc`, `JOIN`
	if filter.OrderByCreation {
		// A message then ranks by its entry's creation, so the walk
		// reads the entries newest first and each one's chat within.
		// CROSS JOIN keeps SQLite from reading every message and
		// sorting them instead.
		entryKey, messageKey = `e.created_utc`, `e.created_utc`
		join = `CROSS JOIN`
	}
	where.keepTo(workspace, filter)
	query := `SELECT e.id, ` + entryKey + ` AS at FROM ` + from + where.sql()
	args := append(slices.Clone(fromArgs), where.args...)
	if !searchesChat(filter) {
		return query + ` ORDER BY at DESC`, args, nil
	}

	entryFilter := filter
	entryFilter.Content = nil
	messageWhere, err := buildWhere(entryFilter)
	if err != nil {
		return "", nil, err
	}
	if err := messageWhere.addField("content", "lower(m.body)",
		matchAnywhere(filter.Content)); err != nil {
		return "", nil, err
	}
	messageWhere.keepTo(workspace, filter)
	query += ` UNION ALL SELECT e.id, ` + messageKey + ` AS at FROM ` +
		from + ` ` + join + ` entry_messages AS m
			ON m.workspace_id = e.workspace_id AND m.entry_id = e.id` +
		messageWhere.sql() + ` ORDER BY at DESC`
	args = append(append(args, fromArgs...), messageWhere.args...)
	return query, args, nil
}

// match is an entry a search matched, and the time of its newest
// match.
type match struct {
	id int64
	at string
}

// rankMatches reads matches newest first until it has count entries,
// and returns the entries by their newest match, then by id, the
// newest first.
func (v view) rankMatches(
	ctx context.Context, query string, args []any, count int,
) ([]match, error) {
	rows, err := v.tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("reading search results: %w", err)
	}
	defer rows.Close()
	var ranked []match
	seen := map[int64]bool{}
	for rows.Next() {
		var m match
		if err := rows.Scan(&m.id, &m.at); err != nil {
			return nil, fmt.Errorf("reading search results: %w", err)
		}
		// Matches at the time of the last entry ranked may rank above
		// it by id, so the walk stops only once time moves on.
		if len(ranked) >= count && m.at != ranked[len(ranked)-1].at {
			break
		}
		if seen[m.id] {
			continue
		}
		seen[m.id] = true
		ranked = append(ranked, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading search results: %w", err)
	}
	slices.SortStableFunc(ranked, func(a, b match) int {
		return cmp.Or(strings.Compare(b.at, a.at), cmp.Compare(b.id, a.id))
	})
	return ranked, nil
}

// matchingMessages reads the newest messages in an entry's chat that a
// search's text matches, oldest first, and counts the matching
// messages before them.
func (v view) matchingMessages(
	ctx context.Context, key Key, content []FieldCond,
) ([]api.Message, int, error) {
	var where whereBuilder
	if err := where.addField("content", "lower(m.body)",
		matchAnywhere(content)); err != nil {
		return nil, 0, err
	}
	where.clauses = append(where.clauses, `m.workspace_id = ?`,
		`m.entry_id = ?`)
	where.args = append(where.args, key.Workspace, key.ID, messagesPerMatch)
	rows, err := v.tx.QueryContext(ctx, `
		SELECT `+messageColumns+`, count(*) OVER ()
		FROM entry_messages AS m`+where.sql()+`
		ORDER BY m.id DESC LIMIT ?`, where.args...)
	if err != nil {
		return nil, 0, fmt.Errorf("reading matching messages: %w", err)
	}
	defer rows.Close()
	var messages []api.Message
	var total int
	for rows.Next() {
		message, err := v.store.scanMessage(rows, &total)
		if err != nil {
			return nil, 0, fmt.Errorf("reading matching messages: %w", err)
		}
		messages = append(messages, message)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("reading matching messages: %w", err)
	}
	slices.Reverse(messages)
	return messages, total - len(messages), nil
}

// searchFlat lists a page of the entries a search matches, and whether
// more follow. Each entry ranks by its newest match, which is its own
// change or creation time, or the change time of a matching message in
// its chat. By creation, an entry matched in its chat ranks by its own
// creation.
func (v view) searchFlat(
	ctx context.Context,
	workspace int64,
	filter Filter,
	from string,
	fromArgs []any,
	where whereBuilder,
) (api.SearchResult, error) {
	query, args, err := matchQuery(workspace, filter, from, fromArgs, where)
	if err != nil {
		return api.SearchResult{}, err
	}
	ranked, err := v.rankMatches(ctx, query, args,
		filter.Offset+filter.Limit+1)
	if err != nil {
		return api.SearchResult{}, err
	}

	ids := make([]int64, 0, len(ranked))
	for _, m := range ranked[min(filter.Offset, len(ranked)):min(
		filter.Offset+filter.Limit, len(ranked))] {
		ids = append(ids, m.id)
	}
	listed, err := v.matchedEntries(ctx, workspace, ids)
	if err != nil {
		return api.SearchResult{}, err
	}
	if searchesChat(filter) {
		for i := range listed {
			key := Key{workspace, listed[i].Ref.ID}
			listed[i].Messages, listed[i].EarlierMessages, err =
				v.matchingMessages(ctx, key, filter.Content)
			if err != nil {
				return api.SearchResult{}, err
			}
		}
	}
	result := api.SearchResult{
		Entries: listed,
		More:    len(ranked) > filter.Offset+filter.Limit,
	}
	return result, nil
}
