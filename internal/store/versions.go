package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/airspacesh/hivemind/internal/api"
)

var ErrVersionNotFound = errors.New("version not found")

// versionContent selects what a version records of a post e, in the
// order of post_versions' last columns: its project, subject and body,
// its metadata as a JSON object in key order, so the same metadata
// always reads as the same text, and the id of its parent, or NULL for
// a top-level post.
const versionContent = `e.project, e.subject, e.body,
	(SELECT json_group_object(key, value) FROM (
		SELECT key, value FROM post_metadata
		WHERE workspace_id = e.workspace_id AND post_id = e.id
		ORDER BY key)),
	(SELECT target_id FROM post_links
		WHERE source_workspace_id = e.workspace_id AND source_id = e.id
			AND name = 'parent')`

// purgedMetadata is the metadata of a version a delete purged, as
// versionContent writes no metadata.
const purgedMetadata = `{}`

// versionRecord is a version's content, as versionContent selects it.
type versionRecord struct {
	project  string
	subject  string
	body     string
	metadata string
	parent   sql.NullInt64
}

// recordVersion makes a post's content its next version, made by
// origin at now in the change that took revision, unless its latest
// version holds that content already.
func (v view) recordVersion(
	ctx context.Context, key Key, origin api.Origin, now string,
	revision int64,
) error {
	var current versionRecord
	if err := v.tx.QueryRowContext(ctx, `
		SELECT `+versionContent+` FROM posts AS e
		WHERE e.workspace_id = ? AND e.id = ?`, key.Workspace, key.ID,
	).Scan(&current.project, &current.subject, &current.body,
		&current.metadata, &current.parent); err != nil {
		return fmt.Errorf("reading the content of %s: %w",
			v.store.mustRef(key), err)
	}
	var latest versionRecord
	var number int64
	err := v.tx.QueryRowContext(ctx, `
		SELECT version, project, subject, body, metadata, parent_id
		FROM post_versions WHERE workspace_id = ? AND post_id = ?
		ORDER BY version DESC LIMIT 1`, key.Workspace, key.ID,
	).Scan(&number, &latest.project, &latest.subject, &latest.body,
		&latest.metadata, &latest.parent)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("reading the latest version of %s: %w",
			v.store.mustRef(key), err)
	}
	if err == nil && latest == current {
		return nil
	}

	if _, err := v.tx.ExecContext(ctx, `
		INSERT INTO post_versions (workspace_id, post_id, version,
			revision, created_utc, origin_user, origin_host, origin_dir,
			origin_claude_session, project, subject, body, metadata,
			parent_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		key.Workspace, key.ID, number+1, revision, now, origin.User,
		origin.Host,
		origin.Dir, origin.ClaudeSession, current.project, current.subject,
		current.body, current.metadata, current.parent,
	); err != nil {
		return fmt.Errorf("recording a version of %s: %w",
			v.store.mustRef(key), err)
	}
	return nil
}

// purgeVersions clears the subject, body and metadata of every version
// of a post, as a delete clears the post's own.
func (v view) purgeVersions(ctx context.Context, key Key) error {
	if _, err := v.tx.ExecContext(ctx, `
		UPDATE post_versions SET subject = '', body = '', metadata = ?
		WHERE workspace_id = ? AND post_id = ?`,
		purgedMetadata, key.Workspace, key.ID,
	); err != nil {
		return fmt.Errorf("purging the versions of %s: %w",
			v.store.mustRef(key), err)
	}
	return nil
}

const versionColumns = `version, created_utc, origin_user, origin_host,
	origin_dir, origin_claude_session`

// currentVersion reads which version a post is at, and who made it
// when.
func (v view) currentVersion(
	ctx context.Context, key Key,
) (*api.Version, error) {
	var version api.Version
	var created string
	if err := v.tx.QueryRowContext(ctx, `
		SELECT `+versionColumns+` FROM post_versions
		WHERE workspace_id = ? AND post_id = ?
		ORDER BY version DESC LIMIT 1`, key.Workspace, key.ID,
	).Scan(&version.Number, &created, &version.Origin.User,
		&version.Origin.Host, &version.Origin.Dir,
		&version.Origin.ClaudeSession); err != nil {
		return nil, fmt.Errorf("reading the version of %s: %w",
			v.store.mustRef(key), err)
	}
	var err error
	if version.Created, err = parseTime(created); err != nil {
		return nil, err
	}
	return &version, nil
}

// Version reads one version of a post the reader can read. Its one
// link is its parent then, left out when the reader cannot read it now.
func (s *Store) Version(
	ctx context.Context, access Access, key Key, number int64,
) (api.PostVersion, error) {
	v, err := s.begin(ctx, access, true)
	if err != nil {
		return api.PostVersion{}, fmt.Errorf("reading version: %w", err)
	}
	defer v.tx.Rollback()

	state, err := v.readPostState(ctx, key)
	if err != nil {
		return api.PostVersion{}, err
	}
	post := api.PostVersion{Ref: s.mustRef(key), Deleted: state.deleted}
	var created, metadata string
	err = v.tx.QueryRowContext(ctx, `
		SELECT `+versionColumns+`, project, subject, body, metadata
		FROM post_versions
		WHERE workspace_id = ? AND post_id = ? AND version = ?`,
		key.Workspace, key.ID, number,
	).Scan(&post.Version.Number, &created, &post.Version.Origin.User,
		&post.Version.Origin.Host, &post.Version.Origin.Dir,
		&post.Version.Origin.ClaudeSession, &post.Project, &post.Subject,
		&post.Body, &metadata)
	if errors.Is(err, sql.ErrNoRows) {
		return api.PostVersion{}, fmt.Errorf("%w: %s has no version %d",
			ErrVersionNotFound, post.Ref, number)
	}
	if err != nil {
		return api.PostVersion{}, fmt.Errorf("reading version %d of %s: %w",
			number, post.Ref, err)
	}
	if post.Version.Created, err = parseTime(created); err != nil {
		return api.PostVersion{}, err
	}
	if err := json.Unmarshal([]byte(metadata), &post.Metadata); err != nil {
		return api.PostVersion{}, fmt.Errorf("reading the metadata of "+
			"version %d of %s: %w", number, post.Ref, err)
	}
	post.Links, err = v.readLinks(ctx, false, `
		SELECT 'parent', workspace_id, parent_id FROM post_versions
		WHERE workspace_id = ? AND post_id = ? AND version = ?
			AND parent_id IS NOT NULL`, key.Workspace, key.ID, number)
	if err != nil {
		return api.PostVersion{}, fmt.Errorf("reading the parent of "+
			"version %d of %s: %w", number, post.Ref, err)
	}
	if err := v.tx.Commit(); err != nil {
		return api.PostVersion{}, fmt.Errorf("finishing version read: %w",
			err)
	}
	return post, nil
}
