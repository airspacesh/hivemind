package main

import (
	"log"
	"net/http"
	"strconv"

	"github.com/airspacesh/workstream/internal/api"
	"github.com/airspacesh/workstream/internal/store"
)

const (
	defaultChatLimit = 20
	maxChatLimit     = 500
)

func (h *handler) postMessage(w http.ResponseWriter, r *http.Request) {
	if !requireNoQuery(w, r) {
		return
	}
	_, post, ok := h.pathPost(w, r)
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
	message, err := h.store.PostMessage(r.Context(), post, request)
	if err != nil {
		writeStoreError(w, err)
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
	_, post, number, ok := h.pathMessage(w, r)
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
		r.Context(), post, number, request)
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
	_, post, number, ok := h.pathMessage(w, r)
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
	message, err := h.store.DeleteMessage(r.Context(), post, number,
		request)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, message)
}

// listMessages returns a page of the messages after the query's after
// that match its conditions, or the newest messages, given latest.
func (h *handler) listMessages(w http.ResponseWriter, r *http.Request) {
	query, ok := parseQueryOf(w, r, messageConditionParameters,
		chatQueryParameters)
	if !ok {
		return
	}
	_, hasLatest := query["latest"]
	if hasLatest && len(query) > 1 {
		writeBadRequest(w, "latest takes no other parameter")
		return
	}
	var after api.ChatBound
	if values := query["after"]; len(values) != 0 {
		var err error
		if after, err = api.ParseChatBound(values[0]); err != nil {
			writeBadRequest(w, "after: "+err.Error())
			return
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
	_, post, ok := h.pathPost(w, r)
	if !ok {
		return
	}

	var page api.ChatPage
	if hasLatest {
		page, err = h.store.LatestMessages(r.Context(), post, int(latest))
	} else {
		page, err = h.store.Messages(r.Context(), post, after, filter,
			exclude, int(limit))
	}
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

// overview returns the newest messages across a workspace, grouped by
// post.
func (h *handler) overview(w http.ResponseWriter, r *http.Request) {
	query, ok := parseQueryOf(w, r, nil, overviewQueryParameters)
	if !ok {
		return
	}
	limit, err := queryInteger(query, "limit", defaultChatLimit,
		1, maxChatLimit)
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
	overview, err := h.store.Overview(r.Context(), workspace.ID, int(limit))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, overview)
}
