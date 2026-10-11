package store

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"
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
	// Changed and Revisions keep a listing to the matches whose time and
	// revision fall in spans, those of the change each ranks by.
	Changed   Span
	Revisions RevisionSpan

	// IncludeChat makes a listing match the messages in the chats of
	// entries as well as the entries.
	IncludeChat bool
	// Under limits the search to an entry and its descendants, when set.
	Under             *Key
	IncludeSuperseded bool
	Limit             int
}

// Span is the times from After, inclusive, to Before, exclusive. A
// zero bound leaves that side open.
type Span struct {
	After  time.Time
	Before time.Time
}

// RevisionSpan is the revisions after After and before Before, both
// exclusive. A zero bound leaves that side open.
type RevisionSpan struct {
	After  int64
	Before int64
}

type whereBuilder struct {
	clauses []string
	args    []any
}

// addSpan keeps a time column within a span.
func (builder *whereBuilder) addSpan(column string, span Span) {
	if !span.After.IsZero() {
		builder.clauses = append(builder.clauses, column+` >= ?`)
		builder.args = append(builder.args, formatTime(span.After))
	}
	if !span.Before.IsZero() {
		builder.clauses = append(builder.clauses, column+` < ?`)
		builder.args = append(builder.args, formatTime(span.Before))
	}
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

// fieldFilter is a filter on one column: its name in errors, the
// column, and its conditions.
type fieldFilter struct {
	name       string
	column     string
	conditions []FieldCond
}

func (builder *whereBuilder) addFields(fields []fieldFilter) error {
	for _, field := range fields {
		if err := builder.addField(
			field.name, field.column, field.conditions,
		); err != nil {
			return err
		}
	}
	return nil
}

// The filters split by what they can match. The text, origin and time
// filters match an entry, or a message on its own. The rest match only
// an entry, so a message meets them through the entry it is in.

// originFilters lists the origin filters, each on the column that
// column gives for its field, such as origin_user.
func originFilters(
	filter Filter, column func(field string) string,
) []fieldFilter {
	return []fieldFilter{
		{"origin-user", column("origin_user"), filter.OriginUser},
		{"origin-host", column("origin_host"), filter.OriginHost},
		{"origin-dir", column("origin_dir"), filter.OriginDir},
		{"origin-claude-session", column("origin_claude_session"),
			filter.OriginClaudeSession},
	}
}

// addEntryOnly adds the filters only an entry e can match.
func (builder *whereBuilder) addEntryOnly(filter Filter) error {
	if err := builder.addFields([]fieldFilter{
		{"subject", "e.subject_lower", matchAnywhere(filter.Subject)},
		{"body", "e.body_lower", matchAnywhere(filter.Body)},
		{"project", "e.project_lower", filter.Project},
	}); err != nil {
		return err
	}
	if err := builder.addKeys(filter.Key); err != nil {
		return err
	}
	return builder.addMetadata(filter.Meta)
}

// buildWhere builds the clause an entry e meets when it matches every
// filter itself.
func buildWhere(filter Filter) (whereBuilder, error) {
	var builder whereBuilder
	if err := builder.addEntryOnly(filter); err != nil {
		return whereBuilder{}, err
	}
	if err := builder.addContent(matchAnywhere(filter.Content)); err != nil {
		return whereBuilder{}, err
	}
	// Unary + keeps SQLite off the origin user index, as
	// createOriginUserIndex explains.
	if err := builder.addFields(originFilters(filter,
		func(field string) string { return "+e." + field + "_lower" },
	)); err != nil {
		return whereBuilder{}, err
	}
	return builder, nil
}

// addMessageFilters adds the filters a message m matches on its own.
func (builder *whereBuilder) addMessageFilters(filter Filter) error {
	return builder.addFields(append([]fieldFilter{
		{"content", "lower(m.body)", matchAnywhere(filter.Content)},
	}, originFilters(filter,
		func(field string) string { return "lower(m." + field + ")" },
	)...))
}

// buildMessageWhere builds the where clause a message m in the chat of
// entry e meets when it matches a filter. The message matches the text,
// origin and time filters itself, and its entry matches the rest.
func buildMessageWhere(filter Filter) (whereBuilder, error) {
	var builder whereBuilder
	if err := builder.addEntryOnly(filter); err != nil {
		return whereBuilder{}, err
	}
	if err := builder.addMessageFilters(filter); err != nil {
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

// idsJSON encodes entry ids for json_each, so a query can take any
// number of them as one argument.
func idsJSON(ids []int64) string {
	encoded, err := json.Marshal(ids)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

// checkPage checks the conditions and limit of a listing.
func checkPage(filter Filter) error {
	if conditionCount(filter) > MaxSearchConditions {
		return fmt.Errorf("%w: at most %d search conditions are allowed",
			ErrInvalidFilter, MaxSearchConditions)
	}
	if filter.Limit < 1 || filter.Limit > MaxSearchLimit {
		return fmt.Errorf("%w: limit must be between 1 and %d",
			ErrInvalidFilter, MaxSearchLimit)
	}
	return nil
}

// candidateSet holds the entries, by id, and the messages, by
// post_order, that a search may match, as the indexes or the subtree
// it keeps to found them.
type candidateSet struct {
	entries  []int64
	messages []int64
}

// within keeps the candidates that are also among others, which come
// sorted.
func (c candidateSet) within(others candidateSet) candidateSet {
	keep := func(ids, sorted []int64) []int64 {
		return slices.DeleteFunc(ids, func(id int64) bool {
			_, found := slices.BinarySearch(sorted, id)
			return !found
		})
	}
	return candidateSet{
		entries:  keep(c.entries, others.entries),
		messages: keep(c.messages, others.messages),
	}
}

// eachChat joins the entries read first to the messages m in their
// chats. A time filter on the messages would lead SQLite to read every
// message in the span for each entry instead, so INDEXED BY holds it to
// the index of each entry's chat.
const eachChat = `CROSS JOIN entry_messages AS m
	INDEXED BY sqlite_autoindex_entry_messages_1`

// findOriginCandidates lists the entries of a workspace, and when the
// search covers chat, the messages, that came from a user, by looking
// up the first origin user pattern that starts with literal text in
// the origin user indexes. SQLite reads only the part of an index
// that starts with that text, as its LIKE optimization describes. The
// candidates come sorted. It reports false when no pattern starts
// with literal text, or when a lookup finds more than limit rows.
func (v view) findOriginCandidates(
	ctx context.Context, workspace int64, filter Filter, limit int,
) (candidateSet, bool, error) {
	var pattern string
	for _, condition := range filter.OriginUser {
		value := lowerASCII(condition.Value)
		if condition.Negate || strings.IndexAny(value, "*?[") == 0 ||
			value == "" {
			continue
		}
		pattern = value
		break
	}
	if pattern == "" {
		return candidateSet{}, false, nil
	}

	var found candidateSet
	var ok bool
	var err error
	found.entries, ok, err = v.readCandidates(ctx, `
		SELECT id FROM entries
		WHERE workspace_id = ? AND origin_user_lower GLOB ? LIMIT ?`,
		limit, workspace, pattern)
	if err != nil {
		return candidateSet{}, false, fmt.Errorf(
			"looking up origin users: %w", err)
	}
	if !ok {
		return candidateSet{}, false, nil
	}
	if filter.IncludeChat {
		found.messages, ok, err = v.readCandidates(ctx, `
			SELECT post_order FROM entry_messages
			WHERE workspace_id = ? AND origin_user_lower GLOB ? LIMIT ?`,
			limit, workspace, pattern)
		if err != nil {
			return candidateSet{}, false, fmt.Errorf(
				"looking up origin users: %w", err)
		}
		if !ok {
			return candidateSet{}, false, nil
		}
	}
	slices.Sort(found.entries)
	slices.Sort(found.messages)
	return found, true, nil
}

// candidateReadCost is what reading a candidate entry by key, and the
// messages in its chat, costs a listing, in entries read newest first.
const candidateReadCost = 8

// lookupEntryLimit bounds the entries a listing reads by key when the
// project or metadata filters narrow it to those. A listing that reads
// entries newest first reads about limit*total/n of a workspace's total
// to find a page among n that match, if they are spread evenly, so it
// reads by key only while candidateReadCost*n costs less.
func (v view) lookupEntryLimit(
	ctx context.Context, workspace int64, limit int,
) (int, error) {
	// Entries are never removed and their ids count up from 1 in each
	// workspace, so the largest id is the workspace's entry count.
	var total int
	if err := v.tx.QueryRowContext(ctx, `
		SELECT coalesce(max(id), 0) FROM entries WHERE workspace_id = ?`,
		workspace).Scan(&total); err != nil {
		return 0, fmt.Errorf("counting entries: %w", err)
	}
	return int(math.Sqrt(float64(limit*total) / candidateReadCost)), nil
}

// findProjectCandidates lists the entries of a workspace in the
// projects the project filters match, and when the search covers chat,
// the messages in their chats. It matches the filters against the
// workspace's project names, then finds each matching project's entries
// in the project indexes. The candidates come sorted. It reports false
// when there are no project filters, when a lookup finds more than
// limit rows, or when the projects hold too many entries to read them
// sooner than a page found newest first, as lookupEntryLimit has it.
func (v view) findProjectCandidates(
	ctx context.Context, workspace int64, filter Filter, limit int,
) (candidateSet, bool, error) {
	if len(filter.Project) == 0 {
		return candidateSet{}, false, nil
	}
	var where whereBuilder
	if err := where.addField("project", "lower(names.project)",
		filter.Project); err != nil {
		return candidateSet{}, false, err
	}
	entryLimit, err := v.lookupEntryLimit(ctx, workspace, filter.Limit)
	if err != nil {
		return candidateSet{}, false, err
	}

	var found candidateSet
	var ok bool
	// projectNames takes the workspace as ?1, so the patterns after it
	// take ?2 on.
	found.entries, ok, err = v.readCandidates(ctx, projectNames+`
		SELECT e.id FROM names CROSS JOIN entries AS e
			ON e.workspace_id = ?1 AND e.project = names.project`+
		where.sql()+` LIMIT ?`,
		min(limit, entryLimit), append([]any{workspace}, where.args...)...)
	if err != nil {
		return candidateSet{}, false, fmt.Errorf(
			"looking up projects: %w", err)
	}
	if !ok {
		return candidateSet{}, false, nil
	}
	if filter.IncludeChat {
		found.messages, ok, err = v.readChatCandidates(ctx, workspace,
			found.entries, limit)
		if err != nil {
			return candidateSet{}, false, err
		}
		if !ok {
			return candidateSet{}, false, nil
		}
	}
	slices.Sort(found.entries)
	slices.Sort(found.messages)
	return found, true, nil
}

// findMetadataCandidates lists the entries of a workspace that hold the
// pair the first metadata filter not negated asks for, or failing that,
// the key the first key filter not negated asks for, and when the search
// covers chat, the messages in their chats. It finds them in the
// metadata key index. The candidates come sorted. It reports false when
// there is no such filter, when a lookup finds more than limit rows, or
// when the entries are too many to read them sooner than a page found
// newest first, as lookupEntryLimit has it.
func (v view) findMetadataCandidates(
	ctx context.Context, workspace int64, filter Filter, limit int,
) (candidateSet, bool, error) {
	// A key pattern with no wildcard or class matches that one key, and
	// = rather than GLOB lets SQLite seek to the value pattern's literal
	// start within the key.
	keyMatch := func(pattern string) (string, []any) {
		pattern = lowerASCII(pattern)
		if strings.ContainsAny(pattern, "*?[") {
			return ` AND key GLOB ?`, []any{pattern}
		}
		return ` AND key = ?`, []any{pattern}
	}
	var match string
	var args []any
	pair := slices.IndexFunc(filter.Meta, func(condition MetaCond) bool {
		return !condition.Negate
	})
	key := slices.IndexFunc(filter.Key, func(condition FieldCond) bool {
		return !condition.Negate
	})
	switch {
	case pair >= 0:
		match, args = keyMatch(filter.Meta[pair].Key)
		match += ` AND value_lower GLOB ?`
		args = append(args, lowerASCII(filter.Meta[pair].Value))
	case key >= 0:
		match, args = keyMatch(filter.Key[key].Value)
	default:
		return candidateSet{}, false, nil
	}
	entryLimit, err := v.lookupEntryLimit(ctx, workspace, filter.Limit)
	if err != nil {
		return candidateSet{}, false, err
	}

	var found candidateSet
	var ok bool
	// A key pattern may match several keys of one entry.
	found.entries, ok, err = v.readCandidates(ctx, `
		SELECT DISTINCT entry_id FROM entry_metadata
		WHERE workspace_id = ?`+match+` LIMIT ?`,
		min(limit, entryLimit), append([]any{workspace}, args...)...)
	if err != nil {
		return candidateSet{}, false, fmt.Errorf(
			"looking up metadata: %w", err)
	}
	if !ok {
		return candidateSet{}, false, nil
	}
	if filter.IncludeChat {
		found.messages, ok, err = v.readChatCandidates(ctx, workspace,
			found.entries, limit)
		if err != nil {
			return candidateSet{}, false, err
		}
		if !ok {
			return candidateSet{}, false, nil
		}
	}
	slices.Sort(found.entries)
	slices.Sort(found.messages)
	return found, true, nil
}

// findIndexedCandidates lists the entries and messages of a workspace
// a search may match, as the text and origin user indexes and the
// project and metadata lookups find them. It reports false when none of
// them narrows the search to at most limit rows of each.
func (v view) findIndexedCandidates(
	ctx context.Context, workspace int64, filter Filter, limit int,
) (candidateSet, bool, error) {
	candidates, found, err := v.findTextCandidates(ctx, workspace, filter,
		limit)
	if err != nil {
		return candidateSet{}, false, err
	}
	// These lookups list their candidates sorted, as within needs.
	for _, find := range []func(
		context.Context, int64, Filter, int,
	) (candidateSet, bool, error){
		v.findOriginCandidates, v.findProjectCandidates,
		v.findMetadataCandidates,
	} {
		others, othersFound, err := find(ctx, workspace, filter, limit)
		if err != nil {
			return candidateSet{}, false, err
		}
		switch {
		case !othersFound:
		case !found:
			candidates, found = others, true
		default:
			// Each lookup finds every match, so the matches are among
			// both.
			candidates = candidates.within(others)
		}
	}
	return candidates, found, nil
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
		ORDER BY m.post_order`, idsJSON(postOrders))
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

// A listing shows the newest entries of a workspace that match a
// filter. Without chat, each entry ranks by the latest change to it or
// its chat. With chat, it shows the newest matching messages too, each
// ranked by its post, and each entry ranks by its own latest change.
// Every change takes the next revision on the server, so matches rank
// by revision, which no two share, and a page ends at an exact one.

// candidateLimit bounds the entries, and the messages, a listing reads
// by key when the indexes narrow it to those. Past it, matches are
// common enough that reading entries and messages newest first finds a
// page sooner.
const candidateLimit = 4096

// listingFrom is the FROM clause a listing reads entries e from, which
// holds every entry, or with under, the entries of that subtree.
// Reading the subtree first and sorting it costs time in proportion to
// the subtree, where walking the workspace newest first and testing
// each entry would read every entry to fill a page from a small one.
// CROSS JOIN holds SQLite to that order. Given a time span, it would
// otherwise read the entries in the span and scan the whole subtree
// for each.
func listingFrom(under *Key) (string, []any) {
	if under == nil {
		return `entries AS e`, nil
	}
	return `(WITH RECURSIVE ` + subtreeCTE("under") +
			` SELECT workspace_id, id FROM under) AS under
		CROSS JOIN entries AS e
			ON e.workspace_id = under.workspace_id AND e.id = under.id`,
		[]any{under.Workspace, under.ID}
}

// match is a match of a listing, with the revision and time it ranks
// by. It is an entry, or a message in the entry's chat when postOrder
// is not 0.
type match struct {
	revision  int64
	at        string
	entry     int64
	postOrder int64
}

// readMatches reads matches, the columns of a match a row.
func (v view) readMatches(
	ctx context.Context, query string, args []any,
) ([]match, error) {
	rows, err := v.tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("reading matches: %w", err)
	}
	defer rows.Close()
	var matches []match
	for rows.Next() {
		var m match
		if err := rows.Scan(&m.revision, &m.at, &m.entry,
			&m.postOrder); err != nil {
			return nil, fmt.Errorf("reading matches: %w", err)
		}
		matches = append(matches, m)
	}
	return matches, rows.Err()
}

// addRanks keeps the matches whose revision and time, in the given
// columns, fall within a filter's spans.
func (builder *whereBuilder) addRanks(
	revisionColumn, timeColumn string, filter Filter,
) {
	if filter.Revisions.After != 0 {
		builder.clauses = append(builder.clauses, revisionColumn+` > ?`)
		builder.args = append(builder.args, filter.Revisions.After)
	}
	if filter.Revisions.Before != 0 {
		builder.clauses = append(builder.clauses, revisionColumn+` < ?`)
		builder.args = append(builder.args, filter.Revisions.Before)
	}
	builder.addSpan(timeColumn, filter.Changed)
}

// newestEntries reads the count newest entries of a workspace that
// match a filter, from among the candidates when they are given.
func (v view) newestEntries(
	ctx context.Context,
	workspace int64,
	filter Filter,
	where whereBuilder,
	candidates *candidateSet,
	count int,
) ([]match, error) {
	revision, at := `e.activity_revision`, `e.activity_utc`
	if filter.IncludeChat {
		revision, at = `e.revision`, `e.modified_utc`
	}
	where.addRanks(revision, at, filter)
	where.clauses = append(where.clauses, `e.workspace_id = ?`)
	where.args = append(where.args, workspace)
	if !filter.IncludeSuperseded {
		where.clauses = append(where.clauses, `NOT `+supersededClause)
	}

	from, args := listingFrom(filter.Under)
	if candidates != nil {
		from = `json_each(?) AS candidate CROSS JOIN entries AS e
			ON e.workspace_id = ? AND e.id = candidate.value`
		args = []any{idsJSON(candidates.entries), workspace}
		where.addUnder(filter.Under)
	}
	return v.readMatches(ctx, `
		SELECT `+revision+`, `+at+`, e.id, 0 FROM `+from+where.sql()+`
		ORDER BY `+revision+` DESC LIMIT ?`,
		append(append(args, where.args...), count))
}

// newestMessages reads the count messages of a workspace posted last
// that match a filter, as buildMessageWhere has it, from among the
// candidates when they are given.
func (v view) newestMessages(
	ctx context.Context,
	workspace int64,
	filter Filter,
	where whereBuilder,
	candidates *candidateSet,
	count int,
) ([]match, error) {
	where.addRanks(`m.posted_revision`, `m.created_utc`, filter)
	where.clauses = append(where.clauses, `m.workspace_id = ?`)
	where.args = append(where.args, workspace)
	if !filter.IncludeSuperseded {
		where.clauses = append(where.clauses, `NOT `+supersededClause)
	}

	// Without a subtree, the messages are read newest first, each
	// joined to its entry. A subtree's entries are read first, then each
	// one's chat, as listingFrom explains.
	from := `entry_messages AS m CROSS JOIN entries AS e
		ON e.workspace_id = m.workspace_id AND e.id = m.entry_id`
	var args []any
	switch {
	case candidates != nil:
		from = `json_each(?) AS candidate
			CROSS JOIN entry_messages AS m ON m.post_order = candidate.value
			CROSS JOIN entries AS e
				ON e.workspace_id = m.workspace_id AND e.id = m.entry_id`
		args = []any{idsJSON(candidates.messages)}
		where.addUnder(filter.Under)
	case filter.Under != nil:
		from, args = listingFrom(filter.Under)
		from += ` ` + eachChat + `
			ON m.workspace_id = e.workspace_id AND m.entry_id = e.id`
	}
	return v.readMatches(ctx, `
		SELECT m.posted_revision, m.created_utc, m.entry_id, m.post_order
		FROM `+from+where.sql()+`
		ORDER BY m.posted_revision DESC LIMIT ?`,
		append(append(args, where.args...), count))
}

// messagesSince counts, for each of the given entries in a workspace,
// the messages in its chat posted at or after a revision. It leaves out
// the entries with none.
func (v view) messagesSince(
	ctx context.Context, workspace int64, ids []int64, revision int64,
) (map[int64]int, error) {
	rows, err := v.tx.QueryContext(ctx, `
		SELECT m.entry_id, count(*)
		FROM json_each(?) AS shown `+eachChat+`
			ON m.workspace_id = ? AND m.entry_id = shown.value
		WHERE m.posted_revision >= ? GROUP BY m.entry_id`,
		idsJSON(ids), workspace, revision)
	if err != nil {
		return nil, fmt.Errorf("counting new messages: %w", err)
	}
	defer rows.Close()
	counts := map[int64]int{}
	for rows.Next() {
		var id int64
		var count int
		if err := rows.Scan(&id, &count); err != nil {
			return nil, fmt.Errorf("counting new messages: %w", err)
		}
		counts[id] = count
	}
	return counts, rows.Err()
}

// readPage reads the entries and messages of a page of matches, and the
// paths above them. Without chat, it counts each entry's messages
// posted since the page's oldest match.
func (v view) readPage(
	ctx context.Context,
	workspace int64,
	matches []match,
	includeChat bool,
) (api.MatchPage, error) {
	entries := map[int64]api.Post{}
	paths := map[int64][]api.PostRef{}
	var ids, postOrders []int64
	for _, m := range matches {
		if m.postOrder != 0 {
			postOrders = append(postOrders, m.postOrder)
		}
		if _, read := entries[m.entry]; read {
			continue
		}
		key := Key{workspace, m.entry}
		var err error
		if entries[m.entry], err = v.getEntry(ctx, key); err != nil {
			return api.MatchPage{}, err
		}
		if paths[m.entry], err = v.ancestors(ctx, key); err != nil {
			return api.MatchPage{}, err
		}
		ids = append(ids, m.entry)
	}

	slices.Sort(postOrders)
	messages, err := v.messagesByPostOrder(ctx, postOrders)
	if err != nil {
		return api.MatchPage{}, err
	}
	if len(messages) != len(postOrders) {
		return api.MatchPage{}, fmt.Errorf("read %d of %d matched messages",
			len(messages), len(postOrders))
	}
	byPostOrder := map[int64]*api.Message{}
	for i, postOrder := range postOrders {
		byPostOrder[postOrder] = &messages[i]
	}
	newMessages := map[int64]int{}
	if !includeChat && len(matches) > 0 {
		oldest := matches[len(matches)-1].revision
		if newMessages, err = v.messagesSince(ctx, workspace, ids,
			oldest); err != nil {
			return api.MatchPage{}, err
		}
	}

	page := api.MatchPage{Matches: []api.Match{}}
	for _, m := range matches {
		at, err := parseTime(m.at)
		if err != nil {
			return api.MatchPage{}, err
		}
		page.Matches = append(page.Matches, api.Match{
			Post: entries[m.entry], Ancestors: paths[m.entry],
			Message: byPostOrder[m.postOrder], Revision: m.revision, At: at,
			NewMessages: newMessages[m.entry],
		})
	}
	return page, nil
}

// Search lists a page of the newest entries of a workspace that match
// a filter, and when it includes chat, of the newest messages as well.
// An entry matches when it meets every filter itself, and a message as
// buildMessageWhere has it.
func (s *Store) Search(
	ctx context.Context,
	access Access,
	workspace int64,
	filter Filter,
) (api.MatchPage, error) {
	if err := checkPage(filter); err != nil {
		return api.MatchPage{}, err
	}
	entryWhere, err := buildWhere(filter)
	if err != nil {
		return api.MatchPage{}, err
	}
	messageWhere, err := buildMessageWhere(filter)
	if err != nil {
		return api.MatchPage{}, err
	}

	v, err := s.begin(ctx, access, true)
	if err != nil {
		return api.MatchPage{}, fmt.Errorf("starting search: %w", err)
	}
	defer v.tx.Rollback()
	if filter.Under != nil {
		if err := v.checkEntryExists(ctx, *filter.Under); err != nil {
			return api.MatchPage{}, err
		}
	}
	found, ok, err := v.findIndexedCandidates(ctx, workspace, filter,
		candidateLimit)
	if err != nil {
		return api.MatchPage{}, err
	}
	var candidates *candidateSet
	if ok {
		candidates = &found
	}
	// A page of matches and one more, to tell whether more follow.
	count := filter.Limit + 1
	matches, err := v.newestEntries(ctx, workspace, filter, entryWhere,
		candidates, count)
	if err != nil {
		return api.MatchPage{}, err
	}
	if filter.IncludeChat {
		messages, err := v.newestMessages(ctx, workspace, filter,
			messageWhere, candidates, count)
		if err != nil {
			return api.MatchPage{}, err
		}
		matches = append(matches, messages...)
		slices.SortFunc(matches, func(a, b match) int {
			return cmp.Compare(b.revision, a.revision)
		})
	}

	page, err := v.readPage(ctx, workspace,
		matches[:min(filter.Limit, len(matches))], filter.IncludeChat)
	if err != nil {
		return api.MatchPage{}, err
	}
	page.More = len(matches) > filter.Limit
	if err := v.tx.Commit(); err != nil {
		return api.MatchPage{}, fmt.Errorf("finishing search: %w", err)
	}
	return page, nil
}
