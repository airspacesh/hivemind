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
// since, or an empty page whose cursor is the latest revision, given
// since=now.
func (h *handler) changes(w http.ResponseWriter, r *http.Request) {
	query, ok := parseQueryOf(w, r, "since", "limit")
	if !ok {
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
	access, ok := h.access(w, r)
	if !ok {
		return
	}
	workspace, ok := h.workspace(w, r, access)
	if !ok {
		return
	}

	var page api.ChangesPage
	if now {
		page.Changes = []api.Change{}
		page.Cursor, err = h.store.LatestRevision(r.Context())
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
