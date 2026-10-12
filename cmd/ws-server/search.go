package main

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/airspacesh/workstream/internal/api"
	"github.com/airspacesh/workstream/internal/store"
)

// conditionParameters names the query parameters of conditions on
// fields, each with its no- form.
func conditionParameters(fields []string) map[string]bool {
	parameters := map[string]bool{}
	for _, field := range fields {
		parameters[field] = true
		parameters["no-"+field] = true
	}
	return parameters
}

// searchConditionParameters are the conditions of a search or the
// changes feed.
var searchConditionParameters = conditionParameters(api.SearchFields)

// searchQueryParameters are the parameters of a search, besides its
// conditions.
var searchQueryParameters = map[string]bool{
	"limit":              true,
	"include-superseded": true,
	"include-deleted":    true,
	"items":              true,
	"under":              true,
	"after":              true,
	"before":             true,
}

// changesQueryParameters are the parameters of the changes feed,
// besides its conditions.
var changesQueryParameters = map[string]bool{
	"after":                  true,
	"items":                  true,
	"limit":                  true,
	"under":                  true,
	"exclude-user":           true,
	"exclude-claude-session": true,
}

// messageConditionParameters are the conditions of a chat read, which
// each message must meet.
var messageConditionParameters = conditionParameters(api.MessageFields)

// chatQueryParameters are the parameters of a chat read, besides its
// conditions.
var chatQueryParameters = map[string]bool{
	"after":                  true,
	"latest":                 true,
	"limit":                  true,
	"include-deleted":        true,
	"exclude-user":           true,
	"exclude-claude-session": true,
}

// latestQueryParameters are the parameters a read of a chat's newest
// messages takes.
var latestQueryParameters = map[string]bool{
	"latest":          true,
	"after":           true,
	"include-deleted": true,
}

// overviewQueryParameters are the parameters of a chat overview.
var overviewQueryParameters = map[string]bool{
	"limit":           true,
	"include-deleted": true,
}

// projectsQueryParameters are the parameters of a projects listing.
var projectsQueryParameters = map[string]bool{
	"name":            true,
	"limit":           true,
	"include-deleted": true,
}

// treeQueryParameters are the parameters of a tree.
var treeQueryParameters = map[string]bool{
	"limit":              true,
	"include-superseded": true,
	"include-deleted":    true,
}

// knownQueryParameter reports whether any route takes a parameter, so
// the request log can name it.
func knownQueryParameter(name string) bool {
	for _, parameters := range []map[string]bool{
		searchConditionParameters, searchQueryParameters,
		changesQueryParameters, messageConditionParameters,
		chatQueryParameters, overviewQueryParameters,
		projectsQueryParameters, treeQueryParameters,
	} {
		if parameters[name] {
			return true
		}
	}
	return false
}

func fieldConditions(query url.Values, field string) []store.FieldCond {
	conditions := make([]store.FieldCond, 0,
		len(query[field])+len(query["no-"+field]))
	for _, value := range query[field] {
		conditions = append(conditions, store.FieldCond{Value: value})
	}
	for _, value := range query["no-"+field] {
		conditions = append(conditions,
			store.FieldCond{Negate: true, Value: value})
	}
	return conditions
}

// refuseProjectKey refuses a metadata filter on the key project, which
// would match nothing, since the project is a field of its own.
func refuseProjectKey(pattern string) error {
	if strings.EqualFold(pattern, api.ProjectKey) {
		return fmt.Errorf("%s is not a metadata key, so filter on the "+
			"project field instead", api.ProjectKey)
	}
	return nil
}

func metaConditions(query url.Values) ([]store.MetaCond, error) {
	conditions := make([]store.MetaCond, 0,
		len(query["meta"])+len(query["no-meta"]))
	for _, parameter := range []struct {
		name   string
		negate bool
	}{
		{"meta", false},
		{"no-meta", true},
	} {
		for _, value := range query[parameter.name] {
			key, metadataValue, found := strings.Cut(value, "=")
			if !found {
				return nil, fmt.Errorf(
					"meta filter %q must be key=value", value)
			}
			if err := refuseProjectKey(key); err != nil {
				return nil, err
			}
			conditions = append(conditions, store.MetaCond{
				Negate: parameter.negate,
				Key:    key,
				Value:  metadataValue,
			})
		}
	}
	return conditions, nil
}

// conditionsFilter reads the conditions a query asks for.
func conditionsFilter(query url.Values) (store.Filter, error) {
	metadata, err := metaConditions(query)
	if err != nil {
		return store.Filter{}, err
	}
	for _, name := range []string{"key", "no-key"} {
		for _, pattern := range query[name] {
			if err := refuseProjectKey(pattern); err != nil {
				return store.Filter{}, err
			}
		}
	}
	return store.Filter{
		Subject:             fieldConditions(query, "subject"),
		Body:                fieldConditions(query, "body"),
		Project:             fieldConditions(query, "project"),
		Key:                 fieldConditions(query, "key"),
		Content:             fieldConditions(query, "content"),
		Meta:                metadata,
		OriginUser:          fieldConditions(query, "origin-user"),
		OriginHost:          fieldConditions(query, "origin-host"),
		OriginDir:           fieldConditions(query, "origin-dir"),
		OriginClaudeSession: fieldConditions(query, "origin-claude-session"),
	}, nil
}

// excludedOrigin reads the origin key whose changes a query leaves
// out, or nil when it leaves out none.
func excludedOrigin(query url.Values) (*store.OriginKey, error) {
	user, hasUser := query["exclude-user"]
	session, hasSession := query["exclude-claude-session"]
	if !hasUser && !hasSession {
		return nil, nil
	}
	if !hasUser || !hasSession {
		return nil, fmt.Errorf("exclude-user and exclude-claude-session " +
			"go together, with an empty session for changes written " +
			"outside one")
	}
	return &store.OriginKey{User: user[0], ClaudeSession: session[0]}, nil
}

// searchFilter reads the filter a search query asks for, apart from
// the subtree it may be under.
func searchFilter(query url.Values) (store.Filter, error) {
	filter, err := conditionsFilter(query)
	if err != nil {
		return store.Filter{}, err
	}
	limit, err := queryInteger(query, "limit", 20, 1, store.MaxSearchLimit)
	if err != nil {
		return store.Filter{}, err
	}
	includeSuperseded, err := queryFlag(query, "include-superseded")
	if err != nil {
		return store.Filter{}, err
	}
	includeDeleted, err := queryFlag(query, "include-deleted")
	if err != nil {
		return store.Filter{}, err
	}
	items, err := queryItems(query, api.PostsOnly)
	if err != nil {
		return store.Filter{}, err
	}
	after, err := queryBound(query, "after")
	if err != nil {
		return store.Filter{}, err
	}
	before, err := queryBound(query, "before")
	if err != nil {
		return store.Filter{}, err
	}
	filter.Changed = store.Span{After: after.Time, Before: before.Time}
	filter.Revisions = store.RevisionSpan{After: after.Revision,
		Before: before.Revision}
	filter.IncludeSuperseded = includeSuperseded
	filter.IncludeDeleted = includeDeleted
	filter.Items = items
	filter.Limit = int(limit)
	return filter, nil
}

func (h *handler) search(w http.ResponseWriter, r *http.Request) {
	query, ok := parseQueryOf(w, r, searchConditionParameters,
		searchQueryParameters)
	if !ok {
		return
	}
	filter, err := searchFilter(query)
	if err != nil {
		writeBadRequest(w, err.Error())
		return
	}
	access, ok := h.access(w, r)
	if !ok {
		return
	}
	workspace, ok := h.workspace(w, r, access)
	if !ok {
		return
	}
	if values := query["under"]; len(values) != 0 {
		under, ok := h.postKey(w, access, values[0])
		if !ok {
			return
		}
		filter.Under = &under
	}
	page, err := h.store.Search(r.Context(), access, workspace.ID, filter)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}
