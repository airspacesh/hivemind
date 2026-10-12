package main

import (
	"net/http"

	"github.com/airspacesh/workstream/internal/api"
)

const (
	defaultChangesLimit = 100
	maxChangesLimit     = 500
)

// changes returns a page of a workspace's changes after the query's
// after that match its conditions, or an empty page whose cursor is the
// latest revision, given no after. After a time, the changes start at
// the first change at or after it, and after a post or message, at the
// first change after the one that posted it.
func (h *handler) changes(w http.ResponseWriter, r *http.Request) {
	query, ok := parseQueryOf(w, r, searchConditionParameters,
		changesQueryParameters)
	if !ok {
		return
	}
	after, err := queryBound(query, "after")
	if err != nil {
		writeBadRequest(w, err.Error())
		return
	}
	limit, err := queryInteger(query, "limit", defaultChangesLimit,
		1, maxChangesLimit)
	if err != nil {
		writeBadRequest(w, err.Error())
		return
	}
	filter, err := conditionsFilter(query)
	if err != nil {
		writeBadRequest(w, err.Error())
		return
	}
	if filter.Items, err = queryItems(query, api.PostsAndChat); err != nil {
		writeBadRequest(w, err.Error())
		return
	}
	exclude, err := excludedOrigin(query)
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

	var since int64
	if after != nil {
		resolved, ok := h.postedBound(w, r, access, *after)
		if !ok {
			return
		}
		since = resolved.Revision
		if !resolved.Time.IsZero() {
			since, err = h.store.LastRevisionBefore(r.Context(), access,
				workspace.ID, resolved.Time)
			if err != nil {
				writeStoreError(w, err)
				return
			}
		}
	}

	var page api.ChangesPage
	if after == nil {
		page.Changes = []api.Change{}
		page.Cursor, err = h.store.LatestRevision(r.Context())
	} else {
		page, err = h.store.Changes(r.Context(), access, workspace.ID,
			since, filter, exclude, int(limit))
	}
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}
