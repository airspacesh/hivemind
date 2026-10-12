package main

import (
	"fmt"
	"net/http"

	"github.com/airspacesh/workstream/internal/api"
	"github.com/airspacesh/workstream/internal/store"
)

const (
	defaultChangesLimit = 100
	maxChangesLimit     = 500
)

// changes returns a page of a workspace's changes after the change
// bound the query's after names that match its conditions, or an empty
// page whose cursor is the latest revision, given no after.
func (h *handler) changes(w http.ResponseWriter, r *http.Request) {
	query, ok := parseQueryOf(w, r, searchConditionParameters,
		changesQueryParameters)
	if !ok {
		return
	}
	_, hasAfter := query["after"]
	var after api.ChangeBound
	if hasAfter {
		var err error
		if after, err = api.ParseChangeBound(query.Get("after")); err != nil {
			writeBadRequest(w, "after: "+err.Error())
			return
		}
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

	since := after.Revision
	if after.Post.ID != 0 {
		if after.Post.Prefix != workspace.Prefix {
			writeBadRequest(w, fmt.Sprintf("after: %s is in another "+
				"workspace than %s, whose ids start with %s", after,
				workspace.Name, workspace.Prefix))
			return
		}
		post := store.Key{Workspace: workspace.ID, ID: after.Post.ID}
		since, err = h.store.PostedRevision(r.Context(), access, post,
			after.Message)
		if err != nil {
			writeStoreError(w, err)
			return
		}
	}

	var page api.ChangesPage
	if !hasAfter {
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
