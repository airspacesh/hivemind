package main

import (
	"net/http"
	"strconv"

	"github.com/airspacesh/workstream/internal/api"
)

const (
	defaultChangesLimit = 100
	maxChangesLimit     = 500
)

// changes returns a page of a workspace's changes after the revision
// since that match the query's conditions, or an empty page whose
// cursor is the latest revision, given since=now.
func (h *handler) changes(w http.ResponseWriter, r *http.Request) {
	query, ok := parseQuery(w, r)
	if !ok || !validateQuery(w, query, repeatedQueryParameters,
		changesQueryParameters) {
		return
	}
	text := query.Get("since")
	now := text == "now"
	since, err := strconv.ParseInt(text, 10, 64)
	if !now && (err != nil || since < 0) {
		writeBadRequest(w, "since must be a revision, 0 for the start, "+
			"or now for the latest change")
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
		under, ok := h.entryKey(w, access, values[0])
		if !ok {
			return
		}
		filter.Under = &under
	}

	var page api.ChangesPage
	if now {
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
