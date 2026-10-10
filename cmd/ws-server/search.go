package main

import (
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/airspacesh/workstream/internal/api"
	"github.com/airspacesh/workstream/internal/store"
)

var repeatedQueryParameters = map[string]bool{
	"subject":    true,
	"no-subject": true,
	"body":       true,
	"no-body":    true,
	"project":    true,
	"no-project": true,
	"key":        true,
	"no-key":     true,
	"content":    true,
	"no-content": true,
	"meta":       true,
	"no-meta":    true,

	"origin-user":              true,
	"no-origin-user":           true,
	"origin-host":              true,
	"no-origin-host":           true,
	"origin-dir":               true,
	"no-origin-dir":            true,
	"origin-claude-session":    true,
	"no-origin-claude-session": true,
}

var singleQueryParameters = map[string]bool{
	"limit":              true,
	"offset":             true,
	"order-by":           true,
	"include-superseded": true,
	"entries-only":       true,
	"view":               true,
	"under":              true,
}

// chatQueryParameters are the parameters of a chat read, and of the
// changes feed.
var chatQueryParameters = map[string]bool{
	"since":  true,
	"latest": true,
	"limit":  true,
}

// projectsQueryParameters are the parameters of a projects listing.
var projectsQueryParameters = map[string]bool{
	"name": true,
}

func knownQueryParameter(name string) bool {
	return repeatedQueryParameters[name] || singleQueryParameters[name] ||
		chatQueryParameters[name] || projectsQueryParameters[name]
}

func validateSearchQuery(w http.ResponseWriter, query url.Values) bool {
	keys := make([]string, 0, len(query))
	conditionCount := 0
	for key := range query {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		values := query[key]
		switch {
		case repeatedQueryParameters[key]:
			conditionCount += len(values)
		case singleQueryParameters[key]:
			if len(values) != 1 {
				writeBadRequest(w, key+" must be given once")
				return false
			}
		default:
			writeBadRequest(w,
				fmt.Sprintf("unknown query parameter %q", key))
			return false
		}
	}
	if conditionCount > store.MaxSearchConditions {
		writeBadRequest(w, fmt.Sprintf(
			"at most %d search conditions are allowed",
			store.MaxSearchConditions,
		))
		return false
	}
	return true
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

// searchFilter reads the filter a search query asks for, apart from
// the subtree it may be under.
func searchFilter(query url.Values) (store.Filter, error) {
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
	limit, err := queryInteger(query, "limit", 20, 1, store.MaxSearchLimit)
	if err != nil {
		return store.Filter{}, err
	}
	offset, err := queryInteger(query, "offset", 0, 0, 1<<31-1)
	if err != nil {
		return store.Filter{}, err
	}
	includeSuperseded, err := queryFlag(query, "include-superseded")
	if err != nil {
		return store.Filter{}, err
	}
	entriesOnly, err := queryFlag(query, "entries-only")
	if err != nil {
		return store.Filter{}, err
	}
	filter := store.Filter{
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
		IncludeSuperseded:   includeSuperseded,
		EntriesOnly:         entriesOnly,
		Limit:               int(limit),
		Offset:              int(offset),
	}
	if values := query["view"]; len(values) != 0 {
		switch values[0] {
		case "flat":
		case "topics":
			filter.View = store.ViewTopics
		default:
			return store.Filter{}, fmt.Errorf(
				"view must be 'flat' or 'topics'")
		}
	}
	if values := query["order-by"]; len(values) != 0 {
		switch values[0] {
		case "modified":
		case "created":
			filter.OrderByCreation = true
		default:
			return store.Filter{}, fmt.Errorf(
				"order-by must be 'created' or 'modified'")
		}
	}
	return filter, nil
}

func (h *handler) search(w http.ResponseWriter, r *http.Request) {
	query, ok := parseQuery(w, r)
	if !ok || !validateSearchQuery(w, query) {
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
		under, ok := h.entryKey(w, access, values[0])
		if !ok {
			return
		}
		filter.Under = &under
	}
	result, err := h.store.Search(r.Context(), access, workspace.ID, filter)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
