package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/airspacesh/hivemind/internal/api"
)

func TestOpenCreatesAndReopensCurrentVersion(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "stream.db")
	store := openStoreAt(t, path, testBusyTimeout)
	if stats := store.db.Stats(); stats.MaxOpenConnections != 1 {
		t.Fatalf("max open connections = %d, want 1",
			stats.MaxOpenConnections)
	}
	var application, version int
	if err := store.db.QueryRowContext(
		ctx, `PRAGMA application_id`,
	).Scan(&application); err != nil {
		t.Fatalf("reading application_id: %v", err)
	}
	if err := store.db.QueryRowContext(
		ctx, `PRAGMA user_version`,
	).Scan(&version); err != nil {
		t.Fatalf("reading user_version: %v", err)
	}
	if application != applicationID || version != databaseVersion {
		t.Fatalf("database identity = %x/v%d, want %x/v%d",
			application, version, applicationID, databaseVersion)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("closing first store: %v", err)
	}
	reopened, err := Open(ctx, path, testBusyTimeout, testWorkspaces)
	if err != nil {
		t.Fatalf("reopening store: %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatalf("closing reopened store: %v", err)
	}
}

func TestOpenMapsTheDatabaseOnEveryConnection(t *testing.T) {
	store := openTestStore(t)
	for _, pool := range []struct {
		name string
		db   *sql.DB
	}{{"write", store.db}, {"read", store.reads}} {
		var size int64
		if err := pool.db.QueryRow(`PRAGMA mmap_size`).Scan(&size); err != nil {
			t.Fatalf("reading mmap_size of a %s connection: %v",
				pool.name, err)
		}
		if size != mmapSizeBytes {
			t.Fatalf("%s connection mmap_size = %d, want %d",
				pool.name, size, mmapSizeBytes)
		}
	}
}

func TestOpenEscapesSQLitePath(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "duck?pond")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatalf("creating data directory: %v", err)
	}
	path := filepath.Join(dir, "stream#1%.db")
	store, err := Open(ctx, path, testBusyTimeout, testWorkspaces)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("closing store: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("database was not created at %q: %v", path, err)
	}
}

// TestConcurrentFirstOpensAllSucceed opens one new database from
// several connections at once, many times over, since the race between
// them is narrow.
func TestConcurrentFirstOpensAllSucceed(t *testing.T) {
	const rounds, opens = 20, 4
	for round := range rounds {
		path := filepath.Join(t.TempDir(), fmt.Sprintf("stream%d.db", round))
		start := make(chan struct{})
		results := make(chan error, opens)
		for range opens {
			go func() {
				<-start
				ctx, cancel := context.WithTimeout(
					context.Background(), 2*contendedOpenBusyTimeout,
				)
				defer cancel()
				store, err := Open(ctx, path, contendedOpenBusyTimeout,
					testWorkspaces)
				if err == nil {
					var mode string
					err = store.db.QueryRow(`PRAGMA journal_mode`).Scan(&mode)
					if err == nil && mode != "wal" {
						err = fmt.Errorf("journal mode %q, want wal", mode)
					}
					if closeErr := store.Close(); err == nil {
						err = closeErr
					}
				}
				results <- err
			}()
		}
		close(start)
		for range opens {
			if err := <-results; err != nil {
				t.Fatalf("round %d: concurrent first open: %v", round, err)
			}
		}
	}
}

func TestOpenRejectsUnrecognizedDatabases(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name  string
		setup func(*testing.T, string)
		want  string
	}{
		{
			"non-empty unversioned",
			func(t *testing.T, path string) {
				rawExec(t, path, `CREATE TABLE alien(value TEXT)`)
			},
			"unversioned",
		},
		{
			"empty with header",
			func(t *testing.T, path string) {
				rawExec(t, path, `PRAGMA user_version = 1`)
			},
			"empty database",
		},
		{
			"database from before workspaces",
			func(t *testing.T, path string) {
				rawExec(t, path, fmt.Sprintf(`
					CREATE TABLE posts(id INTEGER PRIMARY KEY);
					PRAGMA application_id = %d;
					PRAGMA user_version = 3`, legacyApplicationID))
			},
			"starts fresh databases only",
		},
		{
			"wrong application",
			func(t *testing.T, path string) {
				rawExec(t, path, `
					CREATE TABLE alien(value TEXT);
					PRAGMA application_id = 1234;
					PRAGMA user_version = 1`)
			},
			"application_id",
		},
		{
			"newer version",
			func(t *testing.T, path string) {
				createValidDatabase(t, path)
				rawExec(t, path, fmt.Sprintf(
					`PRAGMA user_version = %d`, databaseVersion+1))
			},
			"newer",
		},
		{
			"changed schema",
			func(t *testing.T, path string) {
				createValidDatabase(t, path)
				rawExec(t, path, `DROP INDEX posts_revision`)
			},
			"schema does not match",
		},
		{
			"extra object",
			func(t *testing.T, path string) {
				createValidDatabase(t, path)
				rawExec(t, path, `CREATE TABLE alien(value TEXT)`)
			},
			"schema does not match",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "test.db")
			test.setup(t, path)
			store, err := Open(ctx, path, testBusyTimeout, testWorkspaces)
			if store != nil {
				store.Close()
				t.Fatal("Open returned a store for invalid database")
			}
			if !errors.Is(err, ErrInvalidDatabase) {
				t.Fatalf("Open error = %v, want ErrInvalidDatabase", err)
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Open error = %q, want text %q", err, test.want)
			}
		})
	}
}

func TestOpenSyncsWorkspacesWithTheConfig(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "sync.db")
	reopen := func(workspaces ...api.Workspace) *Store {
		t.Helper()
		store, err := Open(ctx, path, testBusyTimeout, workspaces)
		if err != nil {
			t.Fatalf("opening with %v: %v", workspaces, err)
		}
		return store
	}
	first := reopen(api.Workspace{Name: "general", Prefix: "e"},
		api.Workspace{Name: "personal", Prefix: "p"})
	work := addTo(t, first, "general",
		api.AddPostRequest{Subject: "work"})
	private := addTo(t, first, "personal",
		api.AddPostRequest{Subject: "private"})
	ids := map[string]int64{}
	for _, workspace := range first.Workspaces() {
		ids[workspace.Name] = workspace.ID
	}
	first.Close()

	swapped := reopen(api.Workspace{Name: "general", Prefix: "p"},
		api.Workspace{Name: "personal", Prefix: "e"})
	for _, workspace := range swapped.Workspaces() {
		if ids[workspace.Name] != workspace.ID {
			t.Fatalf("workspace %s changed id on reopening",
				workspace.Name)
		}
	}
	got := mustGet(t, swapped, api.Ref{Prefix: "p", ID: work.Ref.ID})
	if got.Subject != "work" {
		t.Fatalf("after swapping prefixes, p1 = %q, want the work post",
			got.Subject)
	}
	swapped.Close()

	renamed := reopen(api.Workspace{Name: "general", Prefix: "e"},
		api.Workspace{Name: "private", Prefix: "p"})
	if got := renamed.Unconfigured(); !reflect.DeepEqual(
		got, []string{"personal"}) {
		t.Fatalf("unconfigured = %v, want personal", got)
	}
	if _, err := renamed.Detail(ctx, everyAccess(renamed), Key{
		Workspace: workspaceID(t, renamed, "private"), ID: private.Ref.ID,
	}); !errors.Is(err, ErrPostNotFound) {
		t.Fatalf("a new workspace reached another's post: %v", err)
	}
	renamed.Close()

	restored := reopen(api.Workspace{Name: "general", Prefix: "e"},
		api.Workspace{Name: "personal", Prefix: "q"})
	defer restored.Close()
	if got := restored.Unconfigured(); !reflect.DeepEqual(
		got, []string{"private"}) {
		t.Fatalf("unconfigured = %v, want private", got)
	}
	if got := mustGet(t, restored, api.Ref{Prefix: "q",
		ID: private.Ref.ID}); got.Subject != "private" {
		t.Fatalf("q1 = %q, want the restored private post", got.Subject)
	}
}

func rawExec(t *testing.T, path, statement string) {
	t.Helper()
	dsn, err := sqliteDSN(path, testBusyTimeout, false)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("opening raw database: %v", err)
	}
	if _, err := db.Exec(statement); err != nil {
		db.Close()
		t.Fatalf("setting up raw database: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("closing raw database: %v", err)
	}
}

func createValidDatabase(t *testing.T, path string) {
	t.Helper()
	store, err := Open(context.Background(), path, testBusyTimeout,
		testWorkspaces)
	if err != nil {
		t.Fatalf("creating valid database: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("closing valid database: %v", err)
	}
}
