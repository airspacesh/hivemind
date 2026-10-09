package main

import (
	"log"
	"net/http"
	"strconv"

	"github.com/airspacesh/workstream/internal/api"
)

const (
	defaultChatLimit = 20
	maxChatLimit     = 500
)

func (h *handler) postMessage(w http.ResponseWriter, r *http.Request) {
	if !requireNoQuery(w, r) {
		return
	}
	_, entry, ok := h.pathEntry(w, r)
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
	message, err := h.store.Post(r.Context(), entry, request)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	log.Printf("issued %s", message.Ref())
	writeJSON(w, http.StatusCreated, message)
}

func (h *handler) editMessage(w http.ResponseWriter, r *http.Request) {
	if !requireNoQuery(w, r) {
		return
	}
	number, err := strconv.ParseInt(r.PathValue("n"), 10, 64)
	if err != nil || number <= 0 {
		writeBadRequest(w, "the message number must be a positive integer")
		return
	}
	_, entry, ok := h.pathEntry(w, r)
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
		r.Context(), entry, number, request.Body)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, message)
}

// listMessages returns a page of messages after a cursor, given since,
// or the newest messages, given latest.
func (h *handler) listMessages(w http.ResponseWriter, r *http.Request) {
	query, ok := parseQueryOf(w, r, "since", "limit", "latest")
	if !ok {
		return
	}
	_, hasSince := query["since"]
	_, hasLimit := query["limit"]
	_, hasLatest := query["latest"]
	if hasLatest && (hasSince || hasLimit) {
		writeBadRequest(w, "latest takes neither since nor limit")
		return
	}
	since, err := queryInteger(query, "since", 0, 0, 1<<62)
	if err != nil {
		writeBadRequest(w, err.Error())
		return
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
	_, entry, ok := h.pathEntry(w, r)
	if !ok {
		return
	}

	var page api.ChatPage
	if hasLatest {
		page, err = h.store.LatestMessages(r.Context(), entry, int(latest))
	} else {
		page, err = h.store.Messages(r.Context(), entry, since, int(limit))
	}
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

// overview returns the newest messages across a workspace, grouped by
// entry.
func (h *handler) overview(w http.ResponseWriter, r *http.Request) {
	query, ok := parseQueryOf(w, r, "limit")
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
