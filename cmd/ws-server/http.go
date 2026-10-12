package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/airspacesh/workstream/internal/api"
	"github.com/airspacesh/workstream/internal/store"
	"github.com/airspacesh/workstream/internal/version"
)

const maxRequestBytes = int64(1 << 20)

func writeJSON(w http.ResponseWriter, status int, value any) {
	body, err := json.Marshal(value)
	if err != nil {
		log.Printf("encoding response: %v", err)
		status = http.StatusInternalServerError
		body = []byte(`{"error":"encoding response","code":"internal"}`)
	}
	if len(body) > api.MaxResponseBytes {
		log.Printf("response exceeds %d bytes", api.MaxResponseBytes)
		status = http.StatusInternalServerError
		body = []byte(`{"error":"response is too large","code":"internal"}`)
	}
	body = append(body, '\n')
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func writeError(
	w http.ResponseWriter, status int, code api.ErrorCode, message string,
) {
	writeJSON(w, status, api.ErrorResponse{Error: message, Code: code})
}

func writeBadRequest(w http.ResponseWriter, message string) {
	writeError(w, http.StatusBadRequest, api.CodeBadRequest, message)
}

func writeUnauthorized(w http.ResponseWriter, message string) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="workstream"`)
	writeError(w, http.StatusUnauthorized, api.CodeUnauthorized, message)
}

func writeStoreError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	code := api.CodeInternal
	message := "internal server error"
	var notOwner *store.NotOwnerError
	var deleted *store.DeletedError
	var shared *store.SharedError
	switch {
	case errors.Is(err, store.ErrInvalidFilter),
		errors.Is(err, store.ErrInvalidTreeLimit),
		errors.Is(err, store.ErrInvalidProjectLimit):
		status, code, message = http.StatusBadRequest,
			api.CodeBadRequest, err.Error()
	case errors.Is(err, store.ErrUnknownPrefix):
		status, code, message = http.StatusNotFound,
			api.CodeUnknownPrefix, err.Error()
	case errors.Is(err, store.ErrPostNotFound):
		status, code, message = http.StatusNotFound,
			api.CodePostNotFound, err.Error()
	case errors.Is(err, store.ErrMessageNotFound):
		status, code, message = http.StatusNotFound,
			api.CodeMessageNotFound, err.Error()
	case errors.Is(err, store.ErrLinkNotFound):
		status, code, message = http.StatusNotFound,
			api.CodeLinkNotFound, err.Error()
	case errors.Is(err, store.ErrParentWorkspace):
		status, code, message = http.StatusBadRequest,
			api.CodeBadRequest, err.Error()
	case errors.Is(err, store.ErrLinkTarget):
		status, code, message = http.StatusBadRequest,
			api.CodeLinkTargetNotFound, err.Error()
	case errors.Is(err, store.ErrMetadataLimit),
		errors.Is(err, store.ErrLinkLoop),
		errors.Is(err, store.ErrLinkLimit),
		errors.Is(err, store.ErrRequestReused):
		status, code, message = http.StatusConflict,
			api.CodeConflict, err.Error()
	case errors.As(err, &notOwner):
		status, code, message = http.StatusForbidden,
			api.CodeNotOwner, err.Error()
	case errors.As(err, &deleted):
		status, code, message = http.StatusConflict,
			api.CodeDeleted, err.Error()
	case errors.As(err, &shared):
		status, code, message = http.StatusConflict,
			api.CodeConflict, err.Error()
	case errors.Is(err, context.DeadlineExceeded), store.IsBusy(err):
		status, code, message = http.StatusServiceUnavailable,
			api.CodeUnavailable, "server is busy or the request timed out"
	case errors.Is(err, context.Canceled):
		status, code, message = http.StatusServiceUnavailable,
			api.CodeUnavailable, "request canceled"
	default:
		log.Printf("store request failed: %v", err)
	}
	writeError(w, status, code, message)
}

func requireAPIVersion(next http.Handler) http.Handler {
	serverVersion := strconv.Itoa(version.API)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(version.APIHeader, serverVersion)
		values := r.Header.Values(version.APIHeader)
		message := ""
		switch {
		case len(values) == 0:
			message = "missing " + version.APIHeader + " header"
		case len(values) != 1:
			message = version.APIHeader + " must be given once"
		case values[0] != serverVersion:
			message = fmt.Sprintf("the client uses API version %q, "+
				"but the server uses %d", values[0], version.API)
		}
		if message != "" {
			writeError(w, http.StatusBadRequest, api.CodeAPIVersion, message)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func requestTimeout(timeout time.Duration, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func limitRequestBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
		}
		next.ServeHTTP(w, r)
	})
}

// serverWorkspace is a workspace this server hosts, with the hash of
// the Authorization header that grants access to it, or nil when it is
// open.
type serverWorkspace struct {
	store.Workspace
	credential *[sha256.Size]byte
}

func newServerWorkspaces(
	workspaces []store.Workspace, config serverConfig,
) []serverWorkspace {
	hosted := make([]serverWorkspace, len(workspaces))
	for i, workspace := range workspaces {
		hosted[i].Workspace = workspace
		auth := config.Workspaces[workspace.Name].Auth
		if auth != nil {
			credential := sha256.Sum256([]byte("Bearer " + auth.Secret))
			hosted[i].credential = &credential
		}
	}
	return hosted
}

// access finds the workspaces a request can read, which are the open
// ones and any whose secret its one credential presents, and the user
// it reads as. A credential that matches no workspace is refused, since
// it is surely a mistake.
func (h *handler) access(
	w http.ResponseWriter, r *http.Request,
) (store.Access, bool) {
	users := r.Header.Values(api.UserHeader)
	if len(users) > 1 {
		writeBadRequest(w, api.UserHeader+" must be given at most once")
		return store.Access{}, false
	}
	user := ""
	if len(users) == 1 {
		user = users[0]
	}
	if err := api.ValidateUser(user); err != nil {
		writeBadRequest(w, err.Error())
		return store.Access{}, false
	}
	values := r.Header.Values("Authorization")
	if len(values) > 1 {
		writeBadRequest(w, "a request carries at most one credential")
		return store.Access{}, false
	}
	var presented [sha256.Size]byte
	if len(values) == 1 {
		presented = sha256.Sum256([]byte(values[0]))
	}

	var readable []int64
	matched := false
	for _, workspace := range h.workspaces {
		if workspace.credential == nil {
			readable = append(readable, workspace.ID)
			continue
		}
		if len(values) == 1 && subtle.ConstantTimeCompare(
			presented[:], workspace.credential[:]) == 1 {
			readable = append(readable, workspace.ID)
			matched = true
		}
	}
	if len(values) == 1 && !matched {
		writeUnauthorized(w, "the credential matches no workspace "+
			"on this server")
		return store.Access{}, false
	}
	return store.NewAccess(user, readable...), true
}

func writeNeedsSecret(w http.ResponseWriter, workspace string) {
	writeUnauthorized(w, fmt.Sprintf("workspace %s needs its secret, "+
		"sent as Authorization: Bearer <secret>", workspace))
}

// workspace finds the workspace a request's path names, which the
// request must be able to read.
func (h *handler) workspace(
	w http.ResponseWriter, r *http.Request, access store.Access,
) (store.Workspace, bool) {
	name := r.PathValue("workspace")
	for _, workspace := range h.workspaces {
		if workspace.Name != name {
			continue
		}
		if !access.CanRead(workspace.ID) {
			writeNeedsSecret(w, name)
			return store.Workspace{}, false
		}
		return workspace.Workspace, true
	}
	writeError(w, http.StatusNotFound, api.CodeUnknownWorkspace,
		fmt.Sprintf("this server has no workspace %q", name))
	return store.Workspace{}, false
}

// postKey finds the post a ref names, in a workspace the request must
// be able to read. The post may not exist.
func (h *handler) postKey(
	w http.ResponseWriter, access store.Access, text string,
) (store.Key, bool) {
	ref, err := api.ParseRef(text)
	if err != nil {
		writeBadRequest(w, err.Error())
		return store.Key{}, false
	}
	key, err := h.store.Resolve(ref)
	if err != nil {
		writeStoreError(w, err)
		return store.Key{}, false
	}
	for _, workspace := range h.workspaces {
		if workspace.ID == key.Workspace && !access.CanRead(key.Workspace) {
			writeNeedsSecret(w, workspace.Name)
			return store.Key{}, false
		}
	}
	return key, true
}

// logRequests logs one line per request from facts the server
// controls: the method, the matched route, a configured workspace, a
// ref with a configured prefix, a message number, the names of known
// query parameters, the status, and the sizes. It never logs a body, a
// query value, a raw path, or an error message, because each can carry
// text a client sent, and an edit must leave no copy of what it
// overwrote.
func (h *handler) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := &countingReader{ReadCloser: r.Body}
		r.Body = body
		rw := &recordingWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rw, r)
		log.Printf("%s -> %d (%d B in, %d B out)",
			h.describeRequest(r), rw.status, body.count, rw.count)
	})
}

var messageNumber = regexp.MustCompile(`^[1-9][0-9]{0,17}$`)

// describeRequest names a request by its route, after the mux has
// matched it. A request refused before routing, or matching no route,
// is named by its method alone.
func (h *handler) describeRequest(r *http.Request) string {
	route := r.Pattern
	if route == "" || route == "/" {
		route = loggedMethod(r.Method) + " (no route)"
	}
	name := r.PathValue("workspace")
	for _, workspace := range h.workspaces {
		if workspace.Name == name {
			route += " workspace=" + name
		}
	}
	if ref, err := api.ParseRef(r.PathValue("ref")); err == nil {
		if _, err := h.store.Resolve(ref); err == nil {
			route += " ref=" + ref.String()
		}
	}
	if n := r.PathValue("n"); messageNumber.MatchString(n) {
		route += " n=" + n
	}

	var known []string
	unknown := 0
	for name := range r.URL.Query() {
		if knownQueryParameter(name) {
			known = append(known, name)
		} else {
			unknown++
		}
	}
	sort.Strings(known)
	if len(known) > 0 {
		route += " query=" + strings.Join(known, ",")
	}
	if unknown > 0 {
		route += fmt.Sprintf(" unknown-query=%d", unknown)
	}
	return route
}

// loggedMethod names a request method for the log. A client can send
// any token as its method, so an unknown one is logged as OTHER.
func loggedMethod(method string) string {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
		http.MethodPatch, http.MethodDelete, http.MethodConnect,
		http.MethodOptions, http.MethodTrace:
		return method
	default:
		return "OTHER"
	}
}

type countingReader struct {
	io.ReadCloser
	count int64
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	r.count += int64(n)
	return n, err
}

type recordingWriter struct {
	http.ResponseWriter
	status    int
	wroteHead bool
	count     int64
}

func (w *recordingWriter) WriteHeader(status int) {
	if w.wroteHead {
		return
	}
	w.wroteHead = true
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *recordingWriter) Write(body []byte) (int, error) {
	if !w.wroteHead {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(body)
	w.count += int64(n)
	return n, err
}

func decodeBody(w http.ResponseWriter, r *http.Request, value any) bool {
	body, err := io.ReadAll(r.Body)
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeError(w, http.StatusRequestEntityTooLarge, api.CodeTooLarge,
			"request body exceeds 1 MiB")
		return false
	}
	if err != nil {
		writeBadRequest(w, "reading request body: "+err.Error())
		return false
	}
	if !utf8.Valid(body) {
		writeBadRequest(w, "request body is not valid UTF-8")
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		writeBadRequest(w, "invalid request body: "+err.Error())
		return false
	}
	var trailing any
	err = decoder.Decode(&trailing)
	if !errors.Is(err, io.EOF) {
		if err == nil {
			writeBadRequest(w, "request body must contain one JSON value")
		} else {
			writeBadRequest(w, "invalid request body: "+err.Error())
		}
		return false
	}
	return true
}

// parseQueryOf parses a query that may hold only the given parameters,
// each single one at most once, and the repeated ones, which are search
// conditions, at most store.MaxSearchConditions times in all.
func parseQueryOf(
	w http.ResponseWriter, r *http.Request, repeated, single map[string]bool,
) (url.Values, bool) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeBadRequest(w, "invalid query: "+err.Error())
		return nil, false
	}

	conditionCount := 0
	for _, key := range slices.Sorted(maps.Keys(query)) {
		values := query[key]
		switch {
		case repeated[key]:
			conditionCount += len(values)
		case single[key]:
			if len(values) != 1 {
				writeBadRequest(w, key+" must be given once")
				return nil, false
			}
		default:
			writeBadRequest(w,
				fmt.Sprintf("unknown query parameter %q", key))
			return nil, false
		}
	}
	if conditionCount > store.MaxSearchConditions {
		writeBadRequest(w, fmt.Sprintf(
			"at most %d search conditions are allowed",
			store.MaxSearchConditions,
		))
		return nil, false
	}
	return query, true
}

// queryInteger reads an integer parameter from minimum to maximum, or
// fallback when it is absent.
func queryInteger(
	query url.Values, key string, fallback, minimum, maximum int64,
) (int64, error) {
	values := query[key]
	if len(values) == 0 {
		return fallback, nil
	}
	value, err := strconv.ParseInt(values[0], 10, 64)
	if err != nil || value < minimum || value > maximum {
		return 0, fmt.Errorf("%s must be an integer from %d to %d",
			key, minimum, maximum)
	}
	return value, nil
}

// queryFlag reads a parameter that is either absent or true.
func queryFlag(query url.Values, key string) (bool, error) {
	values := query[key]
	if len(values) == 0 {
		return false, nil
	}
	if values[0] != "true" {
		return false, fmt.Errorf("%s must be 'true'", key)
	}
	return true, nil
}

// queryBound reads a bound as api.ParseBound does, or the zero bound
// when the parameter is absent.
func queryBound(query url.Values, key string) (api.Bound, error) {
	values := query[key]
	if len(values) == 0 {
		return api.Bound{}, nil
	}
	bound, err := api.ParseBound(values[0])
	if err != nil {
		return api.Bound{}, fmt.Errorf("%s: %w", key, err)
	}
	return bound, nil
}
