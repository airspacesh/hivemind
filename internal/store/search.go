package store

import (
	"cmp"
	"context"
	"database/sql"
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
	return len(filter.Subject) + len(filter.Body) + len(filter.Project) +
		len(filter.Key) + len(filter.Content) + len(filter.Meta) +
		len(filter.OriginUser) + len(filter.OriginHost) +
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
// match any part of the text the way a search box does. Project, key,
// origin, and metadata patterns stay whole-value matches, because they
// name one of a known set rather than describe its contents.
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
			topicWalkRows(filter))
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

const (
	// changesBeforeIndex and changesPerMatch bound the newest changes
	// a flat search walks for a page before it looks its text up in
	// the text indexes. Common text fills a page from the newest
	// changes sooner than the indexes can list where it is.
	changesBeforeIndex = 2048
	changesPerMatch    = 32
	// candidatesPerChange bounds the candidates a search reads from
	// the text indexes, per change it walked first, before it walks
	// every change instead.
	candidatesPerChange = 8
	// matchingMessageBudget is how many matching messages a page of a
	// search shows, shared by its entries and filled newest first.
	matchingMessageBudget = 30
)

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

// matchScope narrows the matches a flat search reads.
type matchScope struct {
	// since, unless empty, keeps to the matches at or after a time.
	since string
	// indexed keeps to the candidates, the entries the text indexes
	// found, and their chats.
	indexed    bool
	candidates []int64
}

// matchQuery selects a flat search's matches as entry ids, each with
// the time it ranks by, newest first. A match is an entry the search
// matches, or when it searches chat, a message the search's text
// matches in the chat of an entry the rest of the search matches.
// Unless it keeps to candidates, it walks the entries and messages
// newest first, so a reader can stop once it has a page.
func matchQuery(
	workspace int64,
	filter Filter,
	from string,
	fromArgs []any,
	where whereBuilder,
	scope matchScope,
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
	if scope.indexed {
		// Reading the candidates first, and each one's chat within,
		// keeps SQLite from walking every entry to keep them in order.
		from = `json_each(?) AS candidate CROSS JOIN ` + from
		fromArgs = append([]any{idsJSON(scope.candidates)}, fromArgs...)
		where.clauses = append(where.clauses, `e.id = candidate.value`)
		join = `CROSS JOIN`
	}
	if scope.since != "" {
		where.clauses = append(where.clauses, entryKey+` >= ?`)
		where.args = append(where.args, scope.since)
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
	if scope.indexed {
		messageWhere.clauses = append(messageWhere.clauses,
			`e.id = candidate.value`)
	}
	if scope.since != "" {
		messageWhere.clauses = append(messageWhere.clauses,
			messageKey+` >= ?`)
		messageWhere.args = append(messageWhere.args, scope.since)
	}
	query += ` UNION ALL SELECT e.id, ` + messageKey + ` AS at FROM ` +
		from + ` ` + join + ` entry_messages AS m
			ON m.workspace_id = e.workspace_id AND m.entry_id = e.id` +
		messageWhere.sql() + ` ORDER BY at DESC`
	args = append(append(args, fromArgs...), messageWhere.args...)
	return query, args, nil
}

// walkStart is the time of the n-th newest change a flat search ranks
// matches by, or empty when there are fewer.
func (v view) walkStart(
	ctx context.Context, workspace int64, filter Filter, n int,
) (string, error) {
	changes := `SELECT modified_utc AS at FROM entries
		WHERE workspace_id = ?1`
	if filter.OrderByCreation {
		changes = `SELECT created_utc AS at FROM entries
			WHERE workspace_id = ?1`
	} else if searchesChat(filter) {
		changes += ` UNION ALL SELECT modified_utc FROM entry_messages
			WHERE workspace_id = ?1`
	}
	var start string
	err := v.tx.QueryRowContext(ctx, changes+` ORDER BY at DESC
		LIMIT 1 OFFSET ?2`, workspace, n-1).Scan(&start)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("finding where a search starts: %w", err)
	}
	return start, nil
}

// match is an entry a search matched, and the time of its newest
// match.
type match struct {
	id int64
	at string
}

// rankMatches reads a flat search's matches within a scope, newest
// first, until it has count entries, and returns the entries by their
// newest match, then by id, the newest first.
func (v view) rankMatches(
	ctx context.Context,
	workspace int64,
	filter Filter,
	from string,
	fromArgs []any,
	where whereBuilder,
	scope matchScope,
	count int,
) ([]match, error) {
	query, args, err := matchQuery(workspace, filter, from, fromArgs, where,
		scope)
	if err != nil {
		return nil, err
	}
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

// showMatchingMessages gives the entries of a page of a search the
// messages in their chats that the search's text matches, newest first
// until the page's budget is spent, and counts each entry's matches
// left out.
func (v view) showMatchingMessages(
	ctx context.Context,
	workspace int64,
	page []api.MatchedEntry,
	content []FieldCond,
) error {
	ids := make([]int64, len(page))
	for i, entry := range page {
		ids[i] = entry.Ref.ID
	}
	where := whereBuilder{args: []any{idsJSON(ids)}}
	if err := where.addField("content", "lower(m.body)",
		matchAnywhere(content)); err != nil {
		return err
	}
	where.clauses = append(where.clauses, `m.workspace_id = ?`,
		`m.entry_id = page.value`)
	where.args = append(where.args, workspace)
	rows, err := v.tx.QueryContext(ctx, `
		SELECT m.entry_id, m.post_order
		FROM json_each(?) AS page CROSS JOIN entry_messages AS m`+
		where.sql()+`
		ORDER BY m.modified_utc DESC, m.post_order DESC`, where.args...)
	if err != nil {
		return fmt.Errorf("reading matching messages: %w", err)
	}
	matches := map[int64]int{}
	shown := make([]int64, 0, matchingMessageBudget)
	for rows.Next() {
		var entryID, postOrder int64
		if err := rows.Scan(&entryID, &postOrder); err != nil {
			rows.Close()
			return fmt.Errorf("reading matching messages: %w", err)
		}
		matches[entryID]++
		if len(shown) < matchingMessageBudget {
			shown = append(shown, postOrder)
		}
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("reading matching messages: %w", err)
	}

	messages, err := v.messagesByPostOrder(ctx, shown)
	if err != nil {
		return err
	}
	for i := range page {
		entry := &page[i]
		for _, message := range messages {
			if message.Entry == entry.Ref {
				entry.MatchingMessages = append(entry.MatchingMessages,
					message)
			}
		}
		entry.MoreMatchingMessages = matches[entry.Ref.ID] -
			len(entry.MatchingMessages)
	}
	return nil
}

// messagesByPostOrder reads the messages with the given post orders,
// oldest first.
func (v view) messagesByPostOrder(
	ctx context.Context, postOrders []int64,
) ([]api.Message, error) {
	rows, err := v.tx.QueryContext(ctx, `
		SELECT `+messageColumns+`
		FROM json_each(?) AS shown CROSS JOIN entry_messages AS m
			ON m.post_order = shown.value
		ORDER BY m.modified_utc, m.post_order`, idsJSON(postOrders))
	if err != nil {
		return nil, fmt.Errorf("reading messages: %w", err)
	}
	defer rows.Close()
	var messages []api.Message
	for rows.Next() {
		message, err := v.store.scanMessage(rows)
		if err != nil {
			return nil, fmt.Errorf("reading messages: %w", err)
		}
		messages = append(messages, message)
	}
	return messages, rows.Err()
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
	count := filter.Offset + filter.Limit + 1
	walked := changesBeforeIndex + changesPerMatch*count
	since, err := v.walkStart(ctx, workspace, filter, walked)
	if err != nil {
		return api.SearchResult{}, err
	}
	ranked, err := v.rankMatches(ctx, workspace, filter, from, fromArgs, where,
		matchScope{since: since}, count)
	if err != nil {
		return api.SearchResult{}, err
	}
	// The newest changes held too few matches for a page, so the text
	// indexes find the rest, or else the walk goes on to the oldest.
	if len(ranked) < count && since != "" {
		scope := matchScope{}
		scope.candidates, scope.indexed, err = v.findTextCandidates(ctx,
			workspace, filter, searchesChat(filter),
			candidatesPerChange*walked)
		if err != nil {
			return api.SearchResult{}, err
		}
		ranked, err = v.rankMatches(ctx, workspace, filter, from, fromArgs,
			where, scope, count)
		if err != nil {
			return api.SearchResult{}, err
		}
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
		err := v.showMatchingMessages(ctx, workspace, listed, filter.Content)
		if err != nil {
			return api.SearchResult{}, err
		}
	}
	result := api.SearchResult{
		Entries: listed,
		More:    len(ranked) > filter.Offset+filter.Limit,
	}
	return result, nil
}
