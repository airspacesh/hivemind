package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
	Key                 []FieldCond
	Content             []FieldCond
	Meta                []MetaCond
	OriginUser          []FieldCond
	OriginHost          []FieldCond
	OriginDir           []FieldCond
	OriginClaudeSession []FieldCond

	View View
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
// match any part of the text the way a search box does. Type, key,
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
		{"type", "e.type_lower", filter.Type},
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
	where, err := buildWhere(filter)
	if err != nil {
		return api.SearchResult{}, err
	}
	from, fromArgs := joinedFrom(filter.Under)
	where.args = append(fromArgs, where.args...)
	where.clauses = append(where.clauses, `e.workspace_id = ?`)
	where.args = append(where.args, workspace)

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
		result, err = v.searchTopics(ctx, workspace, filter, from, where,
			topicWalks(filter))
	} else {
		result, err = v.searchFlat(ctx, filter, from, where)
	}
	if err != nil {
		return api.SearchResult{}, err
	}
	if err := v.tx.Commit(); err != nil {
		return api.SearchResult{}, fmt.Errorf("finishing search: %w", err)
	}
	return result, nil
}

// searchFlat lists a page of every matching entry, by its own change
// or creation time, and whether more follow.
func (v view) searchFlat(
	ctx context.Context, filter Filter, from string, where whereBuilder,
) (api.SearchResult, error) {
	if !filter.IncludeSuperseded {
		where.clauses = append(where.clauses, `NOT `+supersededClause)
	}
	orderKey := `e.modified_utc`
	if filter.OrderByCreation {
		orderKey = `e.created_utc`
	}
	pageQuery := fmt.Sprintf(`WITH page AS (
			SELECT %s, %s AS sort_key
			FROM %s%s
			ORDER BY sort_key DESC, e.id DESC
			LIMIT ? OFFSET ?
		)
		SELECT page.*, metadata.key, metadata.value
		FROM page
		LEFT JOIN entry_metadata AS metadata
			ON metadata.workspace_id = page.workspace_id
			AND metadata.entry_id = page.id
		ORDER BY page.sort_key DESC, page.id DESC, metadata.key`,
		entryColumns, orderKey, from, where.sql(),
	)
	pageArgs := append(where.args, filter.Limit+1, filter.Offset)
	rows, err := v.tx.QueryContext(ctx, pageQuery, pageArgs...)
	if err != nil {
		return api.SearchResult{}, fmt.Errorf(
			"reading search results: %w", err)
	}
	entries, err := v.scanSearchRows(rows)
	if err != nil {
		return api.SearchResult{}, err
	}
	more := len(entries) > filter.Limit
	entries = entries[:min(len(entries), filter.Limit)]
	if err := v.attachLinks(ctx, entries); err != nil {
		return api.SearchResult{}, err
	}
	listed, err := v.listEntries(ctx, entries)
	if err != nil {
		return api.SearchResult{}, err
	}
	return api.SearchResult{Entries: listed, More: more}, nil
}

func (v view) scanSearchRows(rows *sql.Rows) ([]api.Entry, error) {
	defer rows.Close()
	var entries []api.Entry
	for rows.Next() {
		var sortKey string
		var key, value sql.NullString
		entry, err := v.scanEntry(rows, &sortKey, &key, &value)
		if err != nil {
			return nil, fmt.Errorf("reading search result: %w", err)
		}
		if len(entries) == 0 || entries[len(entries)-1].Ref != entry.Ref {
			entry.Metadata = map[string]string{}
			entries = append(entries, entry)
		}
		if key.Valid != value.Valid {
			return nil, errors.New("metadata key/value NULL mismatch")
		}
		if key.Valid {
			entries[len(entries)-1].Metadata[key.String] = value.String
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading search results: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("closing search results: %w", err)
	}
	return entries, nil
}
