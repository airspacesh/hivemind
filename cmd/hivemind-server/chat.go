package main

import (
	"log"
	"maps"
	"net/http"
	"slices"
	"strconv"

	"github.com/airspacesh/hivemind/internal/api"
	"github.com/airspacesh/hivemind/internal/store"
)

const (
	defaultChatLimit = 20
	maxChatLimit     = 500
)

func (h *handler) postMessage(w http.ResponseWriter, r *http.Request) {
	if !requireNoQuery(w, r) {
		return
	}
	access, post, ok := h.pathPost(w, r)
	if !ok {
		return
	}
	var request api.PostMessageRequest
	if !decodeBody(w, r, &request) {
		return
	}
	if err := request.Validate(); err != nil {
		writeBadRequest(w, err.Error())
		return
	}
	message, created, err := h.store.PostMessage(r.Context(), access, post,
		request)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if !created {
		log.Printf("repeated %s for request %q", message.Ref(),
			request.RequestID)
		writeJSON(w, http.StatusOK, message)
		return
	}
	log.Printf("issued %s", message.Ref())
	writeJSON(w, http.StatusCreated, message)
}

// pathMessage finds the message the path's ref and number name, in a
// post the request must be able to read. The message may not exist.
func (h *handler) pathMessage(
	w http.ResponseWriter, r *http.Request,
) (store.Access, store.Key, int64, bool) {
	number, err := strconv.ParseInt(r.PathValue("n"), 10, 64)
	if err != nil || number <= 0 {
		writeBadRequest(w, "the message number must be a positive integer")
		return store.Access{}, store.Key{}, 0, false
	}
	access, post, ok := h.pathPost(w, r)
	return access, post, number, ok
}

func (h *handler) getMessage(w http.ResponseWriter, r *http.Request) {
	if !requireNoQuery(w, r) {
		return
	}
	access, post, number, ok := h.pathMessage(w, r)
	if !ok {
		return
	}
	detail, err := h.store.MessageDetail(r.Context(), access, post, number)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

func (h *handler) editMessage(w http.ResponseWriter, r *http.Request) {
	if !requireNoQuery(w, r) {
		return
	}
	access, post, number, ok := h.pathMessage(w, r)
	if !ok {
		return
	}
	var request api.EditMessageRequest
	if !decodeBody(w, r, &request) {
		return
	}
	if err := request.Validate(); err != nil {
		writeBadRequest(w, err.Error())
		return
	}
	message, _, err := h.store.EditMessage(
		r.Context(), access, post, number, request)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, message)
}

func (h *handler) deleteMessage(w http.ResponseWriter, r *http.Request) {
	if !requireNoQuery(w, r) {
		return
	}
	access, post, number, ok := h.pathMessage(w, r)
	if !ok {
		return
	}
	var request api.DeleteRequest
	if !decodeBody(w, r, &request) {
		return
	}
	if err := request.Validate(); err != nil {
		writeBadRequest(w, err.Error())
		return
	}
	message, err := h.store.DeleteMessage(r.Context(), access, post,
		number, request)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, message)
}

// listMessages returns a page of the messages after the query's after
// and before its before that match its conditions, or the newest of
// them, given latest.
func (h *handler) listMessages(w http.ResponseWriter, r *http.Request) {
	query, ok := parseQueryOf(w, r, messageConditionParameters,
		chatQueryParameters)
	if !ok {
		return
	}
	_, hasLatest := query["latest"]
	if hasLatest {
		for _, key := range slices.Sorted(maps.Keys(query)) {
			if !latestQueryParameters[key] &&
				!messageConditionParameters[key] {
				writeBadRequest(w, "latest takes no "+key)
				return
			}
		}
	}
	limit, err := queryInteger(query, "limit", defaultChatLimit,
		1, maxChatLimit)
	if err != nil {
		writeBadRequest(w, err.Error())
		return
	}
	latest, err := queryInteger(query, "latest", 0, 1, maxChatLimit)
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
	includeDeleted, err := queryFlag(query, "include-deleted")
	if err != nil {
		writeBadRequest(w, err.Error())
		return
	}
	filter.IncludeDeleted = includeDeleted
	access, post, ok := h.pathPost(w, r)
	if !ok {
		return
	}
	ref, err := api.ParseRef(r.PathValue("ref"))
	if err != nil {
		writeBadRequest(w, err.Error())
		return
	}
	since, ok := h.readBounds(w, r, access, post.Workspace, query, ref,
		&filter)
	if !ok {
		return
	}

	var page api.ChatPage
	if hasLatest {
		page, err = h.store.LatestMessages(r.Context(), access, post,
			since, filter, int(latest))
	} else {
		page, err = h.store.Messages(r.Context(), access, post, since,
			filter, exclude, int(limit))
	}
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

// overview returns the newest messages across a workspace that match
// the query's conditions, grouped by post.
func (h *handler) overview(w http.ResponseWriter, r *http.Request) {
	query, ok := parseQueryOf(w, r, messageConditionParameters,
		overviewQueryParameters)
	if !ok {
		return
	}
	limit, err := queryInteger(query, "limit", defaultChatLimit,
		1, maxChatLimit)
	if err != nil {
		writeBadRequest(w, err.Error())
		return
	}
	includeDeleted, err := queryFlag(query, "include-deleted")
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
	filter, err := conditionsFilter(query)
	if err != nil {
		writeBadRequest(w, err.Error())
		return
	}
	filter.IncludeDeleted = includeDeleted
	if _, ok := h.readBounds(w, r, access, workspace.ID, query,
		api.Ref{}, &filter); !ok {
		return
	}
	overview, err := h.store.Overview(r.Context(), access, workspace.ID,
		filter, int(limit))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, overview)
}
