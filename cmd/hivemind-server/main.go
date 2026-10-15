// hivemind-server hosts the workspaces of one hivemind database for remote
// hivemind clients.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/airspacesh/hivemind/internal/api"
	"github.com/airspacesh/hivemind/internal/store"
	"github.com/airspacesh/hivemind/internal/version"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		log.Fatal(err)
	}
}

func run(args []string) error {
	flags := flag.NewFlagSet("hivemind-server", flag.ContinueOnError)
	configPath := flags.String("config", defaultConfigPath,
		"server config file, where a leading ~/ names the home directory")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	config, err := loadConfig(*configPath)
	if err != nil {
		return err
	}

	listener, err := net.Listen("tcp", config.Listen)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", config.Listen, err)
	}
	defer listener.Close()
	if err := os.MkdirAll(filepath.Dir(config.DB), 0o700); err != nil {
		return fmt.Errorf("creating the database's directory: %w", err)
	}
	workTimeout := serverWorkTimeout(config.Timeout)
	// The request timeout does not bound opening, since the integrity
	// check reads the whole database and takes time in proportion to
	// it. Waits for locks are bounded by the busy timeout instead.
	stream, err := store.Open(context.Background(), config.DB,
		workTimeout, config.workspaces())
	if err != nil {
		return fmt.Errorf("opening database %s: %w", config.DB, err)
	}
	defer stream.Close()

	h := &handler{
		store:       stream,
		workspaces:  newServerWorkspaces(stream.Workspaces(), config),
		workTimeout: workTimeout,
	}
	server := &http.Server{
		Handler:           h.routes(),
		ReadTimeout:       config.Timeout,
		ReadHeaderTimeout: config.Timeout,
		WriteTimeout:      config.Timeout,
		IdleTimeout:       config.Timeout,
		MaxHeaderBytes:    1 << 20,
	}
	shutdownSignal, stopSignals := signal.NotifyContext(
		context.Background(), os.Interrupt, syscall.SIGTERM,
	)
	defer stopSignals()
	log.Printf("hivemind-server %s listening on %s (db: %s, workspaces: %s)",
		version.Software, listener.Addr(), config.DB,
		describeWorkspaces(h.workspaces))
	for _, warning := range startupWarnings(
		listener.Addr(), h.workspaces, stream.Unconfigured(),
	) {
		log.Print("warning: " + warning)
	}
	return serveUntilShutdown(
		server, listener, shutdownSignal, config.Timeout,
	)
}

func describeWorkspaces(workspaces []serverWorkspace) string {
	var described []string
	for _, workspace := range workspaces {
		access := "open"
		if workspace.credential != nil {
			access = "secret"
		}
		described = append(described, fmt.Sprintf(
			"%s (prefix %s, %s, post_editors %s)", workspace.Name,
			workspace.Prefix, access, workspace.postEditors))
	}
	return strings.Join(described, ", ")
}

// startupWarnings names the open workspaces when the server listens
// beyond loopback, and the workspaces the database holds that the
// config no longer names.
func startupWarnings(
	address net.Addr, workspaces []serverWorkspace, unconfigured []string,
) []string {
	var warnings []string
	var open []string
	for _, workspace := range workspaces {
		if workspace.credential == nil {
			open = append(open, workspace.Name)
		}
	}
	tcp, ok := address.(*net.TCPAddr)
	if len(open) > 0 && !(ok && tcp.IP.IsLoopback()) {
		warnings = append(warnings, fmt.Sprintf("listening on %s, beyond "+
			"loopback, with open workspaces anyone who can reach it can "+
			"read and write: %s", address, strings.Join(open, ", ")))
	}
	for _, name := range unconfigured {
		warnings = append(warnings, fmt.Sprintf("the database holds "+
			"workspace %s, which the config does not name, so its "+
			"posts are unreachable", name))
	}
	return warnings
}

func serveUntilShutdown(
	server *http.Server,
	listener net.Listener,
	shutdownSignal context.Context,
	timeout time.Duration,
) error {
	shutdownSignal, cancelShutdown := context.WithCancel(shutdownSignal)
	defer cancelShutdown()
	shutdownComplete := make(chan error, 1)
	go func() {
		<-shutdownSignal.Done()
		shutdownContext, cancel := context.WithTimeout(
			context.Background(), timeout,
		)
		defer cancel()
		shutdownComplete <- server.Shutdown(shutdownContext)
	}()

	serveErr := server.Serve(listener)
	cancelShutdown()
	shutdownErr := <-shutdownComplete
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		return serveErr
	}
	if shutdownErr != nil {
		return fmt.Errorf("shutting down server: %w", shutdownErr)
	}
	return nil
}

// serverWorkTimeout is the share of the request timeout a request's
// database work may take, leaving a margin to write the response.
func serverWorkTimeout(total time.Duration) time.Duration {
	margin := total / 10
	if margin > 500*time.Millisecond {
		margin = 500 * time.Millisecond
	}
	return total - margin
}

type handler struct {
	store      *store.Store
	workspaces []serverWorkspace
	// workTimeout bounds each request's database work.
	workTimeout time.Duration
}

func (h *handler) routes() http.Handler {
	mux := http.NewServeMux()
	route := func(pattern string, f http.HandlerFunc) {
		mux.Handle(pattern, requestTimeout(h.workTimeout, f))
	}
	route("GET /api/status", h.status)
	route("GET /api/workspaces", h.listWorkspaces)
	route("GET /api/workspaces/{workspace}/posts", h.search)
	route("POST /api/workspaces/{workspace}/posts", h.addPost)
	route("GET /api/workspaces/{workspace}/projects", h.projects)
	route("GET /api/workspaces/{workspace}/messages", h.overview)
	route("GET /api/workspaces/{workspace}/changes", h.changes)
	route("GET /api/posts/{ref}", h.getPost)
	route("PATCH /api/posts/{ref}", h.editPost)
	route("DELETE /api/posts/{ref}", h.deletePost)
	route("GET /api/posts/{ref}/versions/{n}", h.getVersion)
	route("GET /api/posts/{ref}/tree", h.tree)
	route("POST /api/posts/{ref}/messages", h.postMessage)
	route("GET /api/posts/{ref}/messages", h.listMessages)
	route("GET /api/posts/{ref}/messages/{n}", h.getMessage)
	route("DELETE /api/posts/{ref}/messages/{n}", h.deleteMessage)
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, api.CodeRouteNotFound,
			"route not found")
	})
	return h.logRequests(requireAPIVersion(limitRequestBody(mux)))
}

func requireNoQuery(w http.ResponseWriter, r *http.Request) bool {
	_, ok := parseQueryOf(w, r, nil, nil)
	return ok
}

func (h *handler) status(w http.ResponseWriter, r *http.Request) {
	if !requireNoQuery(w, r) {
		return
	}
	if err := h.store.Ping(r.Context()); err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, api.StatusResponse{
		Version:  version.Software,
		Database: "ok",
	})
}

func (h *handler) listWorkspaces(w http.ResponseWriter, r *http.Request) {
	if !requireNoQuery(w, r) {
		return
	}
	response := api.WorkspacesResponse{Workspaces: []api.Workspace{}}
	for _, workspace := range h.workspaces {
		response.Workspaces = append(response.Workspaces, api.Workspace{
			Name: workspace.Name, Prefix: workspace.Prefix,
		})
	}
	writeJSON(w, http.StatusOK, response)
}

const defaultProjectLimit = 30

func (h *handler) projects(w http.ResponseWriter, r *http.Request) {
	query, ok := parseQueryOf(w, r, nil, projectsQueryParameters)
	if !ok {
		return
	}
	if query.Has("name") && query.Has("limit") {
		writeBadRequest(w, "give name or limit, not both")
		return
	}
	if query.Has("name") && query.Has("include-deleted") {
		writeBadRequest(w, "name takes no include-deleted")
		return
	}
	limit, err := queryInteger(query, "limit", defaultProjectLimit,
		1, store.MaxProjectLimit)
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

	var projects api.ProjectsResponse
	if query.Has("name") {
		projects, err = h.store.Project(r.Context(), access, workspace.ID,
			query.Get("name"), time.Now())
	} else {
		projects, err = h.store.Projects(r.Context(), access, workspace.ID,
			includeDeleted, int(limit), time.Now())
	}
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, projects)
}

func (h *handler) addPost(w http.ResponseWriter, r *http.Request) {
	if !requireNoQuery(w, r) {
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
	var request api.AddPostRequest
	if !decodeBody(w, r, &request) {
		return
	}
	if err := request.Validate(); err != nil {
		writeBadRequest(w, err.Error())
		return
	}
	post, created, err := h.store.Add(r.Context(), access, workspace.ID,
		request)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if !created {
		log.Printf("repeated %s for request %q", post.Ref, request.RequestID)
		writeJSON(w, http.StatusOK, post)
		return
	}
	log.Printf("issued %s", post.Ref)
	writeJSON(w, http.StatusCreated, post)
}

// pathPost finds the post the path's ref names, which the request must
// be able to read.
func (h *handler) pathPost(
	w http.ResponseWriter, r *http.Request,
) (store.Access, store.Key, bool) {
	access, ok := h.access(w, r)
	if !ok {
		return store.Access{}, store.Key{}, false
	}
	key, ok := h.postKey(w, access, r.PathValue("ref"))
	return access, key, ok
}

func (h *handler) getPost(w http.ResponseWriter, r *http.Request) {
	if !requireNoQuery(w, r) {
		return
	}
	access, key, ok := h.pathPost(w, r)
	if !ok {
		return
	}
	detail, err := h.store.Detail(r.Context(), access, key)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

func (h *handler) getVersion(w http.ResponseWriter, r *http.Request) {
	if !requireNoQuery(w, r) {
		return
	}
	number, err := strconv.ParseInt(r.PathValue("n"), 10, 64)
	if err != nil || number <= 0 {
		writeBadRequest(w, "the version number must be a positive integer")
		return
	}
	access, key, ok := h.pathPost(w, r)
	if !ok {
		return
	}
	version, err := h.store.Version(r.Context(), access, key, number)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, version)
}

func (h *handler) editPost(w http.ResponseWriter, r *http.Request) {
	if !requireNoQuery(w, r) {
		return
	}
	access, key, ok := h.pathPost(w, r)
	if !ok {
		return
	}
	var request api.EditPostRequest
	if !decodeBody(w, r, &request) {
		return
	}
	if err := request.Validate(); err != nil {
		writeBadRequest(w, err.Error())
		return
	}
	post, err := h.store.Edit(r.Context(), access, key,
		h.postEditors(key.Workspace), request)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, post)
}

func (h *handler) deletePost(w http.ResponseWriter, r *http.Request) {
	if !requireNoQuery(w, r) {
		return
	}
	access, key, ok := h.pathPost(w, r)
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
	post, err := h.store.Delete(r.Context(), access, key, request)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, post)
}

const defaultTreeLimit = 30

func (h *handler) tree(w http.ResponseWriter, r *http.Request) {
	query, ok := parseQueryOf(w, r, nil, treeQueryParameters)
	if !ok {
		return
	}
	limit, err := queryInteger(query, "limit", defaultTreeLimit,
		1, store.MaxTreeLimit)
	if err != nil {
		writeBadRequest(w, err.Error())
		return
	}
	includeSuperseded, err := queryFlag(query, "include-superseded")
	if err != nil {
		writeBadRequest(w, err.Error())
		return
	}
	includeDeleted, err := queryFlag(query, "include-deleted")
	if err != nil {
		writeBadRequest(w, err.Error())
		return
	}
	access, key, ok := h.pathPost(w, r)
	if !ok {
		return
	}
	tree, err := h.store.Tree(r.Context(), access, key,
		includeSuperseded, includeDeleted, int(limit))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, tree)
}
