// Package store owns the SQLite database behind ws-server.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"

	sqlite "modernc.org/sqlite"

	"github.com/airspacesh/workstream/internal/api"
)

var (
	ErrPostNotFound    = errors.New("post not found")
	ErrMetadataLimit   = errors.New("metadata pair limit reached")
	ErrInvalidDatabase = errors.New("invalid workstream database")
)

const (
	applicationID = 0x574b5350 // "WKSP"
	// legacyApplicationID marked the databases of releases before
	// workspaces, which this release does not migrate.
	legacyApplicationID = 0x57535452 // "WSTR"
	maxBusyTimeoutMsecs = int64(1<<31 - 1)
	sqliteBusy          = 5
	sqliteLocked        = 6
)

// createWorkspaces defines the workspaces a server hosts. Nothing is
// ever deleted, so an id is never reused. A workspace the config no
// longer names keeps its row, and its prefix is NULL, so a configured
// workspace can take that prefix.
const createWorkspaces = `
CREATE TABLE workspaces (
	id INTEGER PRIMARY KEY,
	name TEXT NOT NULL UNIQUE,
	prefix TEXT UNIQUE
)`

// createRevisionCounter holds the revision of the latest change on the
// server, in its one row. Each change takes the next revision.
const createRevisionCounter = `
CREATE TABLE revision_counter (
	only_row INTEGER PRIMARY KEY CHECK (only_row = 1),
	revision INTEGER NOT NULL
)`

const startRevisionCounter = `
INSERT INTO revision_counter (only_row, revision) VALUES (1, 0)`

// createPosts defines the posts table. A post's id counts from 1 within
// its workspace. Each post records its origin: the OS user, host,
// working directory, and Claude Code session that created it. Every
// searchable text field has a lowered twin so GLOB can match without
// regard to ASCII case. activity_utc is the latest change to the post
// or its chat. Every write to either sets it, so listings read it
// rather than the chat. revision is the revision of the post's own
// latest change, which also set modified_utc, and activity_revision the
// revision of the change that set activity_utc. deleted is 1 once a
// delete has cleared the subject and body, and 0 before.
const createPosts = `
CREATE TABLE posts (
	workspace_id INTEGER NOT NULL REFERENCES workspaces(id),
	id INTEGER NOT NULL,
	created_utc TEXT NOT NULL,
	modified_utc TEXT NOT NULL,
	activity_utc TEXT NOT NULL,
	revision INTEGER NOT NULL,
	activity_revision INTEGER NOT NULL,
	project TEXT NOT NULL,
	project_lower TEXT COLLATE BINARY
		GENERATED ALWAYS AS (lower(project)) STORED,
	subject TEXT NOT NULL,
	subject_lower TEXT COLLATE BINARY
		GENERATED ALWAYS AS (lower(subject)) STORED,
	body TEXT NOT NULL DEFAULT '',
	body_lower TEXT COLLATE BINARY
		GENERATED ALWAYS AS (lower(body)) STORED,
	deleted INTEGER NOT NULL DEFAULT 0,
	origin_user TEXT NOT NULL DEFAULT '',
	origin_user_lower TEXT COLLATE BINARY
		GENERATED ALWAYS AS (lower(origin_user)) STORED,
	origin_host TEXT NOT NULL DEFAULT '',
	origin_host_lower TEXT COLLATE BINARY
		GENERATED ALWAYS AS (lower(origin_host)) STORED,
	origin_dir TEXT NOT NULL DEFAULT '',
	origin_dir_lower TEXT COLLATE BINARY
		GENERATED ALWAYS AS (lower(origin_dir)) STORED,
	origin_claude_session TEXT NOT NULL DEFAULT '',
	origin_claude_session_lower TEXT COLLATE BINARY
		GENERATED ALWAYS AS (lower(origin_claude_session)) STORED,
	PRIMARY KEY (workspace_id, id)
)`

const createMetadata = `
CREATE TABLE post_metadata (
	workspace_id INTEGER NOT NULL,
	post_id INTEGER NOT NULL,
	key TEXT NOT NULL,
	value TEXT NOT NULL,
	value_lower TEXT COLLATE BINARY
		GENERATED ALWAYS AS (lower(value)) STORED,
	PRIMARY KEY (workspace_id, post_id, key),
	FOREIGN KEY (workspace_id, post_id) REFERENCES posts(workspace_id, id)
) WITHOUT ROWID`

// createMetadataKeyIndex finds the posts that hold a metadata key, or a
// key with a value, so a search by metadata need not read them all.
// Only the search's lookup of candidates reads it. The listings'
// metadata filters check each post's own pairs, which SQLite reads
// through the primary key instead.
const createMetadataKeyIndex = `
CREATE INDEX post_metadata_key
ON post_metadata(workspace_id, key, value_lower)`

// createActivityIndex reads a workspace's posts newest first by the
// latest change to them or their chats, as a listing without chat ranks
// them, with the time a span keeps to at hand.
const createActivityIndex = `
CREATE INDEX posts_activity_order
ON posts(workspace_id, activity_revision, activity_utc)`

// createRevisionIndex reads a workspace's posts changed after a
// revision, in revision order.
const createRevisionIndex = `
CREATE INDEX posts_revision ON posts(workspace_id, revision)`

// createProjectIndex finds a workspace's projects, and each one's
// latest activity, by seeking rather than reading the posts. The
// project filters reach the project indexes only through the search's
// lookup of candidates. A listing reads posts newest first and stops at
// a full page, which an index on project_lower would turn into reading
// and sorting every post of the project.
const createProjectIndex = `
CREATE INDEX posts_project_activity
ON posts(workspace_id, project, activity_utc)`

// createOriginUserIndex finds the posts a user created, so a search by
// user need not read them all. Only the search's lookup of candidates
// reads it. A listing that reads posts newest first and stops at a full
// page would read and sort every post of the user through it instead,
// so the listings' origin filters keep SQLite off it with unary +.
const createOriginUserIndex = `
CREATE INDEX posts_origin_user
ON posts(workspace_id, origin_user_lower, id)`

// createProjectCreatedIndex counts a project's posts created since a
// time by reading only those posts' part of the index. It holds deleted
// so the count can leave deleted posts out without reading the posts.
const createProjectCreatedIndex = `
CREATE INDEX posts_project_created
ON posts(workspace_id, project, created_utc, deleted)`

// createLinks defines named links between posts, which may join
// workspaces. Each link is stored once, on its source, and
// post_links_target derives the reverse direction. A source has at most
// one parent.
const createLinks = `
CREATE TABLE post_links (
	source_workspace_id INTEGER NOT NULL,
	source_id INTEGER NOT NULL,
	name TEXT NOT NULL,
	target_workspace_id INTEGER NOT NULL,
	target_id INTEGER NOT NULL,
	PRIMARY KEY (source_workspace_id, source_id, name,
		target_workspace_id, target_id),
	FOREIGN KEY (source_workspace_id, source_id)
		REFERENCES posts(workspace_id, id),
	FOREIGN KEY (target_workspace_id, target_id)
		REFERENCES posts(workspace_id, id)
) WITHOUT ROWID`

const createLinkTargetIndex = `
CREATE INDEX post_links_target
ON post_links(target_workspace_id, target_id, name)`

const createOneParentIndex = `
CREATE UNIQUE INDEX post_links_one_parent
ON post_links(source_workspace_id, source_id) WHERE name = 'parent'`

// createMessages defines the chat attached to posts. A message's id
// counts from 1 within its post. post_order numbers every message on
// the server, so the message text index and a search's candidates name
// a message by one number. An edit overwrites the body, so no earlier
// text is kept. revision is the revision of the posting or of the
// latest edit, and posted_revision the revision of the posting, which
// edits leave alone. deleted is 1 once a delete has cleared the body,
// and 0 before. origin_user has a lowered twin for
// createMessageOriginUserIndex to hold.
const createMessages = `
CREATE TABLE post_messages (
	post_order INTEGER PRIMARY KEY,
	workspace_id INTEGER NOT NULL,
	post_id INTEGER NOT NULL,
	id INTEGER NOT NULL,
	created_utc TEXT NOT NULL,
	modified_utc TEXT NOT NULL,
	revision INTEGER NOT NULL,
	posted_revision INTEGER NOT NULL,
	body TEXT NOT NULL,
	deleted INTEGER NOT NULL DEFAULT 0,
	origin_user TEXT NOT NULL DEFAULT '',
	origin_user_lower TEXT COLLATE BINARY
		GENERATED ALWAYS AS (lower(origin_user)) STORED,
	origin_host TEXT NOT NULL DEFAULT '',
	origin_dir TEXT NOT NULL DEFAULT '',
	origin_claude_session TEXT NOT NULL DEFAULT '',
	UNIQUE (workspace_id, post_id, id),
	FOREIGN KEY (workspace_id, post_id) REFERENCES posts(workspace_id, id)
)`

// createMessagePostedIndex reads a workspace's messages newest first
// by when they were posted, as the chat overview and a listing with
// chat rank them, with the time a span keeps to at hand.
const createMessagePostedIndex = `
CREATE INDEX post_messages_posted
ON post_messages(workspace_id, posted_revision, created_utc)`

const createMessageRevisionIndex = `
CREATE INDEX post_messages_revision
ON post_messages(workspace_id, revision)`

// createMessageOriginUserIndex finds the messages a user posted, as
// createOriginUserIndex finds the posts they created. The listings
// match messages on lower(origin_user), which SQLite does not read
// through it.
const createMessageOriginUserIndex = `
CREATE INDEX post_messages_origin_user
ON post_messages(workspace_id, origin_user_lower)`

// The text indexes hold the trigrams of the lowered text a search
// matches, so a search finds the posts and messages that hold a
// pattern's literal text without reading them all. They are
// contentless, so they hold no copy of the text, and keep no positions,
// so they cannot tell where in a row its trigrams are. Each row is
// keyed by its workspace id in the high 32 bits and its post id or
// message post_order in the low 32. secure-delete makes FTS5 remove the
// trigrams of overwritten text at once rather than at a later merge, as
// the secure_delete pragma zeroes overwritten text in the tables.
const createPostText = `
CREATE VIRTUAL TABLE post_text USING fts5(
	subject_lower, body_lower,
	content='', columnsize=0, detail=none,
	tokenize='trigram case_sensitive 1'
)`

const createMessageText = `
CREATE VIRTUAL TABLE message_text USING fts5(
	body_lower,
	content='', columnsize=0, detail=none,
	tokenize='trigram case_sensitive 1'
)`

const securePostText = `
INSERT INTO post_text (post_text, rank) VALUES ('secure-delete', 1)`

const secureMessageText = `
INSERT INTO message_text (message_text, rank) VALUES ('secure-delete', 1)`

// A contentless index forgets a row through the FTS5 'delete' command,
// given the text it indexed.
const createPostTextInsert = `
CREATE TRIGGER post_text_insert AFTER INSERT ON posts BEGIN
	INSERT INTO post_text (rowid, subject_lower, body_lower)
	VALUES ((new.workspace_id << 32) | new.id, new.subject_lower,
		new.body_lower);
END`

const createPostTextUpdate = `
CREATE TRIGGER post_text_update AFTER UPDATE OF subject, body ON posts
BEGIN
	INSERT INTO post_text (post_text, rowid, subject_lower, body_lower)
	VALUES ('delete', (old.workspace_id << 32) | old.id, old.subject_lower,
		old.body_lower);
	INSERT INTO post_text (rowid, subject_lower, body_lower)
	VALUES ((new.workspace_id << 32) | new.id, new.subject_lower,
		new.body_lower);
END`

const createMessageTextInsert = `
CREATE TRIGGER message_text_insert AFTER INSERT ON post_messages BEGIN
	INSERT INTO message_text (rowid, body_lower)
	VALUES ((new.workspace_id << 32) | new.post_order, lower(new.body));
END`

const createMessageTextUpdate = `
CREATE TRIGGER message_text_update AFTER UPDATE OF body ON post_messages
BEGIN
	INSERT INTO message_text (message_text, rowid, body_lower)
	VALUES ('delete', (old.workspace_id << 32) | old.post_order,
		lower(old.body));
	INSERT INTO message_text (rowid, body_lower)
	VALUES ((new.workspace_id << 32) | new.post_order, lower(new.body));
END`

type schemaObject struct {
	typ   string
	name  string
	table string
	defn  string
}

// textIndexObjects lists a text index and the shadow tables FTS5
// creates for it, with their definitions as FTS5 writes them.
func textIndexObjects(name, definition string) []schemaObject {
	shadow := func(suffix, columns string) schemaObject {
		table := name + suffix
		return schemaObject{"table", table, table,
			"CREATE TABLE '" + table + "'" + columns}
	}
	return []schemaObject{
		{"table", name, name, normalizeSchemaSQL(definition)},
		shadow("_config", "(k PRIMARY KEY, v) WITHOUT ROWID"),
		shadow("_data", "(id INTEGER PRIMARY KEY, block BLOB)"),
		shadow("_idx",
			"(segid, term, pgno, PRIMARY KEY(segid, term)) WITHOUT ROWID"),
	}
}

// schemaStep takes a database from one version to the next: from
// version i to i+1 for schemaSteps[i]. A new database runs every step,
// so a fresh database and a migrated one go through the same path.
type schemaStep struct {
	statements []string
	objects    []schemaObject
}

var schemaSteps = []schemaStep{
	{
		statements: []string{
			createWorkspaces, createRevisionCounter,
			startRevisionCounter, createPosts, createMetadata,
			createMetadataKeyIndex, createActivityIndex,
			createRevisionIndex, createProjectIndex,
			createProjectCreatedIndex, createOriginUserIndex, createLinks,
			createLinkTargetIndex, createOneParentIndex, createMessages,
			createMessagePostedIndex, createMessageRevisionIndex, createMessageOriginUserIndex,
			createPostText, securePostText, createPostTextInsert,
			createPostTextUpdate, createMessageText, secureMessageText,
			createMessageTextInsert, createMessageTextUpdate,
		},
		objects: slices.Concat([]schemaObject{
			{"index", "posts_activity_order", "posts",
				normalizeSchemaSQL(createActivityIndex)},
			{"index", "posts_origin_user", "posts",
				normalizeSchemaSQL(createOriginUserIndex)},
			{"index", "posts_revision", "posts",
				normalizeSchemaSQL(createRevisionIndex)},
			{"index", "posts_project_activity", "posts",
				normalizeSchemaSQL(createProjectIndex)},
			{"index", "posts_project_created", "posts",
				normalizeSchemaSQL(createProjectCreatedIndex)},
			{"index", "post_links_one_parent", "post_links",
				normalizeSchemaSQL(createOneParentIndex)},
			{"index", "post_links_target", "post_links",
				normalizeSchemaSQL(createLinkTargetIndex)},
			{"index", "post_messages_posted", "post_messages",
				normalizeSchemaSQL(createMessagePostedIndex)},
			{"index", "post_messages_origin_user", "post_messages",
				normalizeSchemaSQL(createMessageOriginUserIndex)},
			{"index", "post_messages_revision", "post_messages",
				normalizeSchemaSQL(createMessageRevisionIndex)},
			{"index", "post_metadata_key", "post_metadata",
				normalizeSchemaSQL(createMetadataKeyIndex)},
			{"table", "posts", "posts",
				normalizeSchemaSQL(createPosts)},
			{"table", "post_links", "post_links",
				normalizeSchemaSQL(createLinks)},
			{"table", "post_messages", "post_messages",
				normalizeSchemaSQL(createMessages)},
			{"table", "post_metadata", "post_metadata",
				normalizeSchemaSQL(createMetadata)},
			{"table", "revision_counter", "revision_counter",
				normalizeSchemaSQL(createRevisionCounter)},
			{"table", "workspaces", "workspaces",
				normalizeSchemaSQL(createWorkspaces)},
			{"trigger", "post_text_insert", "posts",
				normalizeSchemaSQL(createPostTextInsert)},
			{"trigger", "post_text_update", "posts",
				normalizeSchemaSQL(createPostTextUpdate)},
			{"trigger", "message_text_insert", "post_messages",
				normalizeSchemaSQL(createMessageTextInsert)},
			{"trigger", "message_text_update", "post_messages",
				normalizeSchemaSQL(createMessageTextUpdate)},
		}, textIndexObjects("post_text", createPostText),
			textIndexObjects("message_text", createMessageText)),
	},
}

var databaseVersion = len(schemaSteps)

// expectedSchema lists the objects a database of the given version
// holds, in the order readSchema returns them.
func expectedSchema(version int) []schemaObject {
	var objects []schemaObject
	for _, step := range schemaSteps[:version] {
		objects = append(objects, step.objects...)
	}
	sort.Slice(objects, func(i, j int) bool {
		if objects[i].typ != objects[j].typ {
			return objects[i].typ < objects[j].typ
		}
		return objects[i].name < objects[j].name
	})
	return objects
}

type Store struct {
	// db holds the one connection every write goes through, so writes
	// take turns and no two take the same post or message number.
	db *sql.DB
	// reads holds the connections reads go through. In WAL mode they
	// read beside the writer, so a slow listing holds up no write.
	reads *sql.DB
	// workspaces are the configured workspaces, in config order.
	workspaces []Workspace
	// unconfigured names the workspaces the database holds but the
	// config does not, whose posts are unreachable.
	unconfigured []string
}

// Open opens the database at path, creating it if it is new, and
// brings its workspaces in line with the configured ones.
func Open(
	ctx context.Context,
	path string,
	busyTimeout time.Duration,
	configured []api.Workspace,
) (_ *Store, retErr error) {
	dsn, err := sqliteDSN(path, busyTimeout, false)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening database: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	defer func() {
		if retErr != nil {
			db.Close()
		}
	}()

	if err := db.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("opening database: %w", err)
	}
	workspaces, unconfigured, err := prepareDatabase(ctx, db, configured)
	if err != nil {
		return nil, err
	}
	if err := enableWAL(ctx, db, busyTimeout); err != nil {
		return nil, err
	}
	var foreignKeys int
	if err := db.QueryRowContext(
		ctx, `PRAGMA foreign_keys`,
	).Scan(&foreignKeys); err != nil {
		return nil, fmt.Errorf("checking foreign keys: %w", err)
	}
	if foreignKeys != 1 {
		return nil, errors.New("SQLite foreign keys are disabled")
	}
	if err := checkIntegrity(ctx, db); err != nil {
		return nil, err
	}

	readDSN, err := sqliteDSN(path, busyTimeout, true)
	if err != nil {
		return nil, err
	}
	reads, err := sql.Open("sqlite", readDSN)
	if err != nil {
		return nil, fmt.Errorf("opening database for reads: %w", err)
	}
	reads.SetMaxOpenConns(maxReadConnections)
	reads.SetMaxIdleConns(maxReadConnections)
	stream := &Store{
		db: db, reads: reads,
		workspaces: workspaces, unconfigured: unconfigured,
	}
	if err := stream.Ping(ctx); err != nil {
		reads.Close()
		return nil, fmt.Errorf("opening database for reads: %w", err)
	}
	return stream, nil
}

// maxReadConnections bounds how many reads run at once.
const maxReadConnections = 8

// walRetryInterval is how long enableWAL waits between attempts.
const walRetryInterval = 10 * time.Millisecond

// enableWAL switches the database to WAL. SQLite does not call the busy
// handler for a journal mode change, so while another connection is
// opening the same new database the change fails at once with
// SQLITE_BUSY. It retries until busyTimeout, the wait every other lock
// gets.
func enableWAL(
	ctx context.Context, db *sql.DB, busyTimeout time.Duration,
) error {
	deadline := time.Now().Add(busyTimeout)
	for {
		var mode string
		err := db.QueryRowContext(
			ctx, `PRAGMA journal_mode = WAL`,
		).Scan(&mode)
		if err == nil && strings.EqualFold(mode, "wal") {
			return nil
		}
		if err == nil {
			return fmt.Errorf("enabling WAL: got mode %q", mode)
		}
		if !IsBusy(err) || time.Now().After(deadline) {
			return fmt.Errorf("enabling WAL: %w", err)
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("enabling WAL: %w", ctx.Err())
		case <-time.After(walRetryInterval):
		}
	}
}

// mmapSizeBytes is SQLITE_MAX_MMAP_SIZE in modernc's build of SQLite,
// the most of the database file a connection can map. SQLite lowers a
// larger setting to it. Connections read mapped pages straight from the
// OS page cache, which they all share, instead of copying each page
// into a page cache of their own. A disk error on a mapped page raises
// SIGBUS, which crashes the server instead of failing one request.
const mmapSizeBytes = 2147418112

// sqliteDSN names the database at path for the driver. A readOnly
// connection refuses every write, through query_only.
func sqliteDSN(
	path string, busyTimeout time.Duration, readOnly bool,
) (string, error) {
	if busyTimeout < 0 {
		return "", errors.New("busy timeout must not be negative")
	}
	msecs := busyTimeout / time.Millisecond
	if busyTimeout%time.Millisecond != 0 {
		msecs++
	}
	if int64(msecs) > maxBusyTimeoutMsecs {
		return "", errors.New("busy timeout is too large")
	}

	fileURL := url.URL{Scheme: "file", Path: path}
	query := url.Values{}
	query.Set("mode", "rwc")
	query.Add("_pragma", "foreign_keys(1)")
	// secure_delete zeroes freed space, so text an edit overwrites is
	// not left behind in the database file's free pages.
	query.Add("_pragma", "secure_delete(1)")
	query.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", msecs))
	query.Add("_pragma", fmt.Sprintf("mmap_size(%d)", mmapSizeBytes))
	// synchronous NORMAL syncs the WAL to disk at checkpoints rather
	// than at every commit. A commit survives a crash of the server,
	// but the latest commits can be lost to a power loss or an OS
	// crash. A sync at every commit makes each write wait on the disk,
	// which takes seconds at a time on busy shared
	// storage, while the write holds the one write connection.
	query.Add("_pragma", "synchronous(1)")
	if readOnly {
		query.Add("_pragma", "query_only(1)")
	}
	fileURL.RawQuery = query.Encode()
	return fileURL.String(), nil
}

// prepareDatabase creates or checks the schema and brings the
// workspaces in line with the configured ones, all under one write
// lock, so connections opening the same database at once take turns.
func prepareDatabase(
	ctx context.Context, db *sql.DB, configured []api.Workspace,
) ([]Workspace, []string, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("checking database: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return nil, nil, fmt.Errorf("locking database schema: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()

	var application, version, objectCount int
	if err := conn.QueryRowContext(
		ctx, `PRAGMA application_id`,
	).Scan(&application); err != nil {
		return nil, nil, fmt.Errorf("reading application_id: %w", err)
	}
	if err := conn.QueryRowContext(
		ctx, `PRAGMA user_version`,
	).Scan(&version); err != nil {
		return nil, nil, fmt.Errorf("reading user_version: %w", err)
	}
	if err := conn.QueryRowContext(
		ctx, `SELECT count(*) FROM sqlite_schema`,
	).Scan(&objectCount); err != nil {
		return nil, nil, fmt.Errorf("reading database schema: %w", err)
	}

	if objectCount == 0 && application == 0 && version == 0 {
		if _, err := conn.ExecContext(ctx, fmt.Sprintf(
			`PRAGMA application_id = %d`, applicationID,
		)); err != nil {
			return nil, nil, fmt.Errorf("setting application_id: %w", err)
		}
	} else if err := validateDatabase(
		ctx, conn, application, version, objectCount,
	); err != nil {
		return nil, nil, err
	}
	if err := migrateDatabase(ctx, conn, version); err != nil {
		return nil, nil, err
	}
	workspaces, unconfigured, err := syncWorkspaces(ctx, conn, configured)
	if err != nil {
		return nil, nil, err
	}

	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return nil, nil, fmt.Errorf("committing database schema: %w", err)
	}
	committed = true
	return workspaces, unconfigured, nil
}

// migrateDatabase runs every schema step after the given version, then
// checks that the result matches a fresh database exactly.
func migrateDatabase(ctx context.Context, conn *sql.Conn, version int) error {
	if version == databaseVersion {
		return nil
	}
	for _, step := range schemaSteps[version:] {
		for _, statement := range step.statements {
			if _, err := conn.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("migrating database from v%d: %w",
					version, err)
			}
		}
	}
	if _, err := conn.ExecContext(
		ctx, fmt.Sprintf(`PRAGMA user_version = %d`, databaseVersion),
	); err != nil {
		return fmt.Errorf("setting user_version: %w", err)
	}
	objects, err := readSchema(ctx, conn)
	if err != nil {
		return err
	}
	if !sameSchema(objects, expectedSchema(databaseVersion)) {
		return fmt.Errorf("%w: schema after migrating from v%d "+
			"does not match v%d", ErrInvalidDatabase, version,
			databaseVersion)
	}
	return nil
}

func validateDatabase(
	ctx context.Context,
	conn *sql.Conn,
	application int,
	version int,
	objectCount int,
) error {
	if objectCount == 0 {
		return fmt.Errorf(
			"%w: empty database has application_id %d and version %d",
			ErrInvalidDatabase, application, version,
		)
	}
	if application == 0 && version == 0 {
		return fmt.Errorf("%w: non-empty database is unversioned",
			ErrInvalidDatabase)
	}
	if application == legacyApplicationID {
		return fmt.Errorf("%w: it was made by a workstream release "+
			"before workspaces, and this release starts fresh databases "+
			"only. Point data at an empty directory", ErrInvalidDatabase)
	}
	if application != applicationID {
		return fmt.Errorf("%w: application_id is %d, want %d",
			ErrInvalidDatabase, application, applicationID)
	}
	if version == 0 {
		return fmt.Errorf("%w: database is unversioned", ErrInvalidDatabase)
	}
	if version > databaseVersion {
		return fmt.Errorf("%w: database version %d is newer than %d",
			ErrInvalidDatabase, version, databaseVersion)
	}
	objects, err := readSchema(ctx, conn)
	if err != nil {
		return err
	}
	if !sameSchema(objects, expectedSchema(version)) {
		return fmt.Errorf("%w: schema does not match database v%d",
			ErrInvalidDatabase, version)
	}
	return nil
}

func readSchema(ctx context.Context, conn *sql.Conn) ([]schemaObject, error) {
	rows, err := conn.QueryContext(ctx, `
		SELECT type, name, tbl_name, sql
		FROM sqlite_schema
		WHERE substr(name, 1, 7) <> 'sqlite_'
		ORDER BY type, name`)
	if err != nil {
		return nil, fmt.Errorf("reading database schema: %w", err)
	}
	defer rows.Close()
	var objects []schemaObject
	for rows.Next() {
		var object schemaObject
		var definition sql.NullString
		if err := rows.Scan(
			&object.typ, &object.name, &object.table, &definition,
		); err != nil {
			return nil, fmt.Errorf("reading database schema: %w", err)
		}
		object.defn = normalizeSchemaSQL(definition.String)
		objects = append(objects, object)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading database schema: %w", err)
	}
	return objects, nil
}

func normalizeSchemaSQL(definition string) string {
	definition = strings.TrimSpace(definition)
	definition = strings.TrimSuffix(definition, ";")
	return strings.Join(strings.Fields(definition), " ")
}

func sameSchema(got, want []schemaObject) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func (s *Store) Close() error {
	return errors.Join(s.reads.Close(), s.db.Close())
}

// Ping checks that the database answers a read.
func (s *Store) Ping(ctx context.Context) error {
	var count int
	if err := s.reads.QueryRowContext(
		ctx, `SELECT count(*) FROM workspaces`,
	).Scan(&count); err != nil {
		return fmt.Errorf("reading the database: %w", err)
	}
	return nil
}

// checkIntegrity reads the whole database to check its structure and
// its foreign keys. It takes time in proportion to the database, so it
// runs once, when the server opens it.
func checkIntegrity(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return fmt.Errorf("starting database integrity check: %w", err)
	}
	defer tx.Rollback()

	var result string
	if err := tx.QueryRowContext(
		ctx, `PRAGMA quick_check(1)`,
	).Scan(&result); err != nil {
		return fmt.Errorf("checking database integrity: %w", err)
	}
	if result != "ok" {
		return fmt.Errorf("database integrity check failed: %s", result)
	}
	rows, err := tx.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return fmt.Errorf("checking database foreign keys: %w", err)
	}
	if rows.Next() {
		rows.Close()
		return errors.New("database foreign key check failed")
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("checking database foreign keys: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("closing database integrity check: %w", err)
	}
	return tx.Commit()
}

func IsBusy(err error) bool {
	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) {
		return false
	}
	switch sqliteErr.Code() & 0xff {
	case sqliteBusy, sqliteLocked:
		return true
	default:
		return false
	}
}

const timeLayout = "2006-01-02T15:04:05.000000000Z07:00"

func parseTime(value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("parsing time %q: %w", value, err)
	}
	return parsed, nil
}

func formatTime(t time.Time) string {
	return t.UTC().Format(timeLayout)
}

func nowUTC() string {
	return formatTime(time.Now())
}
