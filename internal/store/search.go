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
	// Revisions keeps a listing to the matches whose revision, that of
	// the change each ranks by, falls in a span. LastRevisionBefore
	// turns a time into such a revision.
	Revisions RevisionSpan

	// Items says whether a listing or the changes feed holds posts, the
	// messages in their chats, or both.
	Items api.Items
	// Under limits the search to a post and its descendants, when set.
	Under *Key
	// NotUnder leaves out each of its posts and their descendants.
	NotUnder          []Key
	IncludeSuperseded bool
	// IncludeDeleted keeps deleted posts and messages in a listing.
	// Without it, the live posts under a deleted post and the live
	// messages in its chat still match.
	IncludeDeleted bool
	Limit          int
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
			SELECT 1 FROM post_metadata AS key_meta
			WHERE key_meta.workspace_id = e.workspace_id
				AND key_meta.post_id = e.id AND key_meta.key GLOB ?
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
			SELECT 1 FROM post_metadata AS pair_meta
			WHERE pair_meta.workspace_id = e.workspace_id
				AND pair_meta.post_id = e.id
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

// The filters split by what they can match. The content, origin and
// time filters match a post, or a message on its own. The rest match
// only a post, so a message meets them through the post it is in.

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

// addPostOnly adds the filters only a post e can match.
func (builder *whereBuilder) addPostOnly(filter Filter) error {
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

// buildWhere builds the clause a post e meets when it matches every
// filter itself.
func buildWhere(filter Filter) (whereBuilder, error) {
	var builder whereBuilder
	if err := builder.addPostOnly(filter); err != nil {
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
// post e meets when it matches a filter. The message matches the
// content, origin and time filters itself, and its post matches the
// rest.
func buildMessageWhere(filter Filter) (whereBuilder, error) {
	var builder whereBuilder
	if err := builder.addPostOnly(filter); err != nil {
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

// idsJSON encodes post ids for json_each, so a query can take any
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

// candidateSet holds the posts, by id, and the messages, by post_order,
// that a search may match, as the indexes or the subtree it keeps to
// found them.
type candidateSet struct {
	posts    []int64
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
		posts:    keep(c.posts, others.posts),
		messages: keep(c.messages, others.messages),
	}
}

// eachChat joins the posts read first to the messages m in their chats.
// A time filter on the messages would lead SQLite to read every message
// in the span for each post instead, so INDEXED BY holds it to the
// index of each post's chat.
const eachChat = `CROSS JOIN post_messages AS m
	INDEXED BY sqlite_autoindex_post_messages_1`

// candidateLookup finds the posts and messages of a workspace that a
// search may match, for one kind of filter. posts selects post ids and
// messages selects message post_orders, each taking its args and then a
// limit. Without a messages query, the messages that may match are
// those in the chats of the posts found, as for a filter that only a
// post can match.
type candidateLookup struct {
	// what names what the lookup reads, for its errors.
	what        string
	posts       string
	postArgs    []any
	messages    string
	messageArgs []any
	// costed keeps the posts read by key to those cheaper to read than
	// a page found newest first, as lookupPostLimit has it.
	costed bool
}

// originLookup looks up the posts, and the messages, that came from a
// user, by the first origin user pattern that starts with literal text,
// in the origin user indexes. SQLite reads only the part of an index
// that starts with that text, as its LIKE optimization describes. It
// reports false when no pattern starts with literal text.
func originLookup(workspace int64, filter Filter) (candidateLookup, bool) {
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
		return candidateLookup{}, false
	}
	return candidateLookup{
		what: "origin users",
		posts: `
			SELECT id FROM posts
			WHERE workspace_id = ? AND origin_user_lower GLOB ? LIMIT ?`,
		postArgs: []any{workspace, pattern},
		messages: `
			SELECT post_order FROM post_messages
			WHERE workspace_id = ? AND origin_user_lower GLOB ? LIMIT ?`,
		messageArgs: []any{workspace, pattern},
		costed:      true,
	}, true
}

// candidateReadCost is what reading a candidate post by key, and the
// messages in its chat, costs a listing, in posts read newest first.
const candidateReadCost = 8

// lookupPostLimit bounds the posts a listing reads by key when the
// origin user, project or metadata filters narrow it to those. A
// listing that reads posts newest first reads about limit*total/n of a
// workspace's total to find a page among n that match, if they are
// spread evenly, so it reads by key only while candidateReadCost*n
// costs less.
func (v view) lookupPostLimit(
	ctx context.Context, workspace int64, limit int,
) (int, error) {
	// Posts are never removed and their ids count up from 1 in each
	// workspace, so the largest id is the workspace's post count.
	var total int
	if err := v.tx.QueryRowContext(ctx, `
		SELECT coalesce(max(id), 0) FROM posts WHERE workspace_id = ?`,
		workspace).Scan(&total); err != nil {
		return 0, fmt.Errorf("counting posts: %w", err)
	}
	return int(math.Sqrt(float64(limit*total) / candidateReadCost)), nil
}

// projectLookup looks up the posts in the projects the project filters
// match. It matches the filters against the workspace's project names,
// then finds each matching project's posts in the project indexes. It
// reports false when there are no project filters.
func projectLookup(
	workspace int64, filter Filter,
) (candidateLookup, bool, error) {
	if len(filter.Project) == 0 {
		return candidateLookup{}, false, nil
	}
	var where whereBuilder
	if err := where.addField("project", "lower(names.project)",
		filter.Project); err != nil {
		return candidateLookup{}, false, err
	}
	// projectNames takes the workspace as ?1, so the patterns after it
	// take ?2 on.
	return candidateLookup{
		what: "projects",
		posts: projectNames + `
			SELECT e.id FROM names CROSS JOIN posts AS e
				ON e.workspace_id = ?1 AND e.project = names.project` +
			where.sql() + ` LIMIT ?`,
		postArgs: append([]any{workspace}, where.args...),
		costed:   true,
	}, true, nil
}

// metadataLookup looks up the posts that hold the pair the first
// metadata filter not negated asks for, or failing that, the key the
// first key filter not negated asks for, in the metadata key index. It
// reports false when there is no such filter.
func metadataLookup(
	workspace int64, filter Filter,
) (candidateLookup, bool) {
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
		return candidateLookup{}, false
	}
	// A key pattern may match several keys of one post.
	return candidateLookup{
		what: "metadata",
		posts: `
			SELECT DISTINCT post_id FROM post_metadata
			WHERE workspace_id = ?` + match + ` LIMIT ?`,
		postArgs: append([]any{workspace}, args...),
		costed:   true,
	}, true
}

// readLookup runs a lookup in a workspace, reading at most postLimit
// posts and, when the search covers chat, at most limit messages. The
// candidates come sorted. It reports false when either read finds more.
func (v view) readLookup(
	ctx context.Context,
	workspace int64,
	lookup candidateLookup,
	includeChat bool,
	postLimit int,
	limit int,
) (candidateSet, bool, error) {
	var found candidateSet
	var ok bool
	var err error
	found.posts, ok, err = v.readCandidates(ctx, lookup.posts, postLimit,
		lookup.postArgs...)
	if err != nil {
		return candidateSet{}, false, fmt.Errorf("looking up %s: %w",
			lookup.what, err)
	}
	if !ok {
		return candidateSet{}, false, nil
	}
	if includeChat {
		messages, args := lookup.messages, lookup.messageArgs
		if messages == "" {
			messages = `
				SELECT m.post_order FROM json_each(?) AS candidate ` +
				eachChat + `
					ON m.workspace_id = ? AND m.post_id = candidate.value
				LIMIT ?`
			args = []any{idsJSON(found.posts), workspace}
		}
		found.messages, ok, err = v.readCandidates(ctx, messages, limit,
			args...)
		if err != nil {
			return candidateSet{}, false, fmt.Errorf(
				"looking up the messages of %s: %w", lookup.what, err)
		}
		if !ok {
			return candidateSet{}, false, nil
		}
	}
	slices.Sort(found.posts)
	slices.Sort(found.messages)
	return found, true, nil
}

// findIndexedCandidates lists the posts and messages of a workspace a
// search may match, as the text and origin user indexes and the project
// and metadata lookups find them. It reports false when none of them
// narrows the search to at most limit rows of each.
func (v view) findIndexedCandidates(
	ctx context.Context, workspace int64, filter Filter, limit int,
) (candidateSet, bool, error) {
	var lookups []candidateLookup
	use := func(lookup candidateLookup, ok bool) {
		if ok {
			lookups = append(lookups, lookup)
		}
	}
	use(textLookup(workspace, filter))
	use(originLookup(workspace, filter))
	project, ok, err := projectLookup(workspace, filter)
	if err != nil {
		return candidateSet{}, false, err
	}
	use(project, ok)
	use(metadataLookup(workspace, filter))

	var candidates candidateSet
	found := false
	for _, lookup := range lookups {
		postLimit := limit
		if lookup.costed {
			costLimit, err := v.lookupPostLimit(ctx, workspace,
				filter.Limit)
			if err != nil {
				return candidateSet{}, false, err
			}
			postLimit = min(limit, costLimit)
		}
		others, othersFound, err := v.readLookup(ctx, workspace, lookup,
			filter.Items.HasChat(), postLimit, limit)
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
		FROM json_each(?) AS shown CROSS JOIN post_messages AS m
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

// A listing shows the newest posts of a workspace that match a filter,
// the newest messages, or both. Without chat, each post ranks by the
// latest change to it or its chat. With chat, each message ranks by its
// posting, and each post by its own latest change. Every change takes
// the next revision on the server, so matches rank by revision, which no
// two share, and a page ends at an exact one.

// candidateLimit bounds the posts, and the messages, a listing reads by
// key when the indexes narrow it to those. Past it, matches are common
// enough that reading posts and messages newest first finds a page
// sooner.
const candidateLimit = 4096

// listingFrom is the FROM clause a listing reads posts e from, which
// holds every post, or with under, the posts of that subtree. Reading
// the subtree first and sorting it costs time in proportion to the
// subtree, where walking the workspace newest first and testing each
// post would read every post to fill a page from a small one. CROSS
// JOIN holds SQLite to that order. Given a time span, it would
// otherwise read the posts in the span and scan the whole subtree for
// each.
func listingFrom(under *Key) (string, []any) {
	if under == nil {
		return `posts AS e`, nil
	}
	return `(WITH RECURSIVE ` + subtreeCTE("under") +
			` SELECT workspace_id, id FROM under) AS under
		CROSS JOIN posts AS e
			ON e.workspace_id = under.workspace_id AND e.id = under.id`,
		[]any{under.Workspace, under.ID}
}

// match is a match of a listing, with the revision and time it ranks
// by. It is a post, or a message in the post's chat when postOrder is
// not 0.
type match struct {
	revision  int64
	at        string
	post      int64
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
		if err := rows.Scan(&m.revision, &m.at, &m.post,
			&m.postOrder); err != nil {
			return nil, fmt.Errorf("reading matches: %w", err)
		}
		matches = append(matches, m)
	}
	return matches, rows.Err()
}

// addRanks keeps the matches whose revision, in the given column, falls
// within a filter's span.
func (builder *whereBuilder) addRanks(revisionColumn string, filter Filter) {
	if filter.Revisions.After != 0 {
		builder.clauses = append(builder.clauses, revisionColumn+` > ?`)
		builder.args = append(builder.args, filter.Revisions.After)
	}
	if filter.Revisions.Before != 0 {
		builder.clauses = append(builder.clauses, revisionColumn+` < ?`)
		builder.args = append(builder.args, filter.Revisions.Before)
	}
}

// addVisible keeps a listing to the posts e the reader can read, and
// unless the filter includes superseded posts, to those no post the
// reader can read supersedes.
func (v view) addVisible(where *whereBuilder, filter Filter) {
	where.clauses = append(where.clauses, visibleClause("e"))
	where.args = append(where.args, v.access.user)
	if !filter.IncludeSuperseded {
		where.clauses = append(where.clauses, `NOT `+supersededClause)
		where.args = append(where.args, v.access.user)
	}
}

// newestPosts reads the count newest posts of a workspace that match a
// filter, from among the candidates when they are given.
func (v view) newestPosts(
	ctx context.Context,
	workspace int64,
	filter Filter,
	where whereBuilder,
	candidates *candidateSet,
	count int,
) ([]match, error) {
	revision, at := `e.activity_revision`, `e.activity_utc`
	if filter.Items.HasChat() {
		revision, at = `e.revision`, `e.modified_utc`
	}
	where.addRanks(revision, filter)
	where.addNotUnder(filter.NotUnder)
	where.clauses = append(where.clauses, `e.workspace_id = ?`)
	where.args = append(where.args, workspace)
	v.addVisible(&where, filter)
	if !filter.IncludeDeleted {
		where.clauses = append(where.clauses, `NOT e.deleted`)
	}

	from, args := listingFrom(filter.Under)
	if candidates != nil {
		from = `json_each(?) AS candidate CROSS JOIN posts AS e
			ON e.workspace_id = ? AND e.id = candidate.value`
		args = []any{idsJSON(candidates.posts), workspace}
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
	where.addRanks(`m.posted_revision`, filter)
	where.addNotUnder(filter.NotUnder)
	where.clauses = append(where.clauses, `m.workspace_id = ?`)
	where.args = append(where.args, workspace)
	v.addVisible(&where, filter)
	if !filter.IncludeDeleted {
		where.clauses = append(where.clauses, `NOT m.deleted`)
	}

	// Without a subtree, the messages are read newest first, each
	// joined to its post. A subtree's posts are read first, then each
	// one's chat, as listingFrom explains.
	from := `post_messages AS m CROSS JOIN posts AS e
		ON e.workspace_id = m.workspace_id AND e.id = m.post_id`
	var args []any
	switch {
	case candidates != nil:
		from = `json_each(?) AS candidate
			CROSS JOIN post_messages AS m ON m.post_order = candidate.value
			CROSS JOIN posts AS e
				ON e.workspace_id = m.workspace_id AND e.id = m.post_id`
		args = []any{idsJSON(candidates.messages)}
		where.addUnder(filter.Under)
	case filter.Under != nil:
		from, args = listingFrom(filter.Under)
		from += ` ` + eachChat + `
			ON m.workspace_id = e.workspace_id AND m.post_id = e.id`
	}
	return v.readMatches(ctx, `
		SELECT m.posted_revision, m.created_utc, m.post_id, m.post_order
		FROM `+from+where.sql()+`
		ORDER BY m.posted_revision DESC LIMIT ?`,
		append(append(args, where.args...), count))
}

// messagesSince counts, for each of the given posts in a workspace, the
// messages in its chat posted at or after a revision, deleted ones only
// when includeDeleted is set. It leaves out the posts with none.
func (v view) messagesSince(
	ctx context.Context, workspace int64, ids []int64, revision int64,
	includeDeleted bool,
) (map[int64]int, error) {
	rows, err := v.tx.QueryContext(ctx, `
		SELECT m.post_id, count(*)
		FROM json_each(?) AS shown `+eachChat+`
			ON m.workspace_id = ? AND m.post_id = shown.value
		WHERE m.posted_revision >= ? AND (? OR NOT m.deleted)
		GROUP BY m.post_id`,
		idsJSON(ids), workspace, revision, includeDeleted)
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

// readPage reads the posts and messages of a page of matches, and the
// paths above them. Without chat, it counts each post's messages posted
// since the page's oldest match, as the filter would list them.
func (v view) readPage(
	ctx context.Context,
	workspace int64,
	matches []match,
	filter Filter,
) (api.MatchPage, error) {
	posts := map[int64]api.Post{}
	paths := map[int64][]api.PostRef{}
	var ids, postOrders []int64
	for _, m := range matches {
		if m.postOrder != 0 {
			postOrders = append(postOrders, m.postOrder)
		}
		if _, read := posts[m.post]; read {
			continue
		}
		key := Key{workspace, m.post}
		var err error
		if posts[m.post], err = v.getPost(ctx, key); err != nil {
			return api.MatchPage{}, err
		}
		if paths[m.post], err = v.ancestors(ctx, key); err != nil {
			return api.MatchPage{}, err
		}
		ids = append(ids, m.post)
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
	if !filter.Items.HasChat() && len(matches) > 0 {
		oldest := matches[len(matches)-1].revision
		if newMessages, err = v.messagesSince(ctx, workspace, ids,
			oldest, filter.IncludeDeleted); err != nil {
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
			Post: posts[m.post], Ancestors: paths[m.post],
			Message: byPostOrder[m.postOrder], Revision: m.revision, At: at,
			NewMessages: newMessages[m.post],
		})
	}
	return page, nil
}

// checkSubtreesExist refuses a filter under or not under a post that
// does not exist, or that the reader cannot read.
func (v view) checkSubtreesExist(ctx context.Context, filter Filter) error {
	keys := filter.NotUnder
	if filter.Under != nil {
		keys = append([]Key{*filter.Under}, keys...)
	}
	for _, key := range keys {
		if err := v.checkPostExists(ctx, key); err != nil {
			return err
		}
	}
	return nil
}

// Search lists a page of the newest posts of a workspace that match a
// filter, of the newest messages, or of both, as the filter's Items
// says. A post matches when it meets every filter itself, and a message
// as buildMessageWhere has it.
func (s *Store) Search(
	ctx context.Context,
	access Access,
	workspace int64,
	filter Filter,
) (api.MatchPage, error) {
	if err := checkPage(filter); err != nil {
		return api.MatchPage{}, err
	}
	postWhere, err := buildWhere(filter)
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
	if err := v.checkSubtreesExist(ctx, filter); err != nil {
		return api.MatchPage{}, err
	}
	revision, err := revisionSeen(ctx, v.tx)
	if err != nil {
		return api.MatchPage{}, err
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
	var matches []match
	if filter.Items.HasPosts() {
		posts, err := v.newestPosts(ctx, workspace, filter, postWhere,
			candidates, count)
		if err != nil {
			return api.MatchPage{}, err
		}
		matches = append(matches, posts...)
	}
	if filter.Items.HasChat() {
		messages, err := v.newestMessages(ctx, workspace, filter,
			messageWhere, candidates, count)
		if err != nil {
			return api.MatchPage{}, err
		}
		matches = append(matches, messages...)
	}
	slices.SortFunc(matches, func(a, b match) int {
		return cmp.Compare(b.revision, a.revision)
	})

	page, err := v.readPage(ctx, workspace,
		matches[:min(filter.Limit, len(matches))], filter)
	if err != nil {
		return api.MatchPage{}, err
	}
	page.More = len(matches) > filter.Limit
	page.Revision = revision
	if err := v.tx.Commit(); err != nil {
		return api.MatchPage{}, fmt.Errorf("finishing search: %w", err)
	}
	return page, nil
}
