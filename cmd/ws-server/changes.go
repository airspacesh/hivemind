package main

import (
	"net/http"

	"github.com/airspacesh/workstream/internal/api"
)

const (
	defaultChangesLimit = 100
	maxChangesLimit     = 500
)

// changes returns a page of a workspace's changes after a cursor,
// given since, or an empty page whose cursor is the latest change,
// given since=now.
func (h *handler) changes(w http.ResponseWriter, r *http.Request) {
	query, ok := parseQueryOf(w, r, "since", "limit")
	if !ok {
		return
	}
	if !query.Has("since") {
		writeBadRequest(w, "since is required. Give a cursor, 0 for "+
			"the start, or now for the latest change")
		return
	}
	text := query.Get("since")
	var since api.ChangeCursor
	if text != "now" {
		var err error
		if since, err = api.ParseChangeCursor(text); err != nil {
			writeBadRequest(w, err.Error())
			return
		}
	}
	limit, err := queryInteger(query, "limit", defaultChangesLimit,
		1, maxChangesLimit)
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

	var page api.ChangesPage
	if text == "now" {
		page.Changes = []api.Change{}
		page.Cursor, err = h.store.LatestChange(r.Context(), workspace.ID)
	} else {
		page, err = h.store.Changes(r.Context(), access, workspace.ID,
			since, int(limit))
	}
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}
