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
// its metadata as a JSON object, and its links as a JSON array of
// [name, workspace id, post id]. Both run in key order, so the same
// content always reads as the same text.
const versionContent = `e.project, e.subject, e.body,
	(SELECT json_group_object(key, value) FROM (
		SELECT key, value FROM post_metadata
		WHERE workspace_id = e.workspace_id AND post_id = e.id
		ORDER BY key)),
	(SELECT json_group_array(
		json_array(name, target_workspace_id, target_id)) FROM (
		SELECT name, target_workspace_id, target_id FROM post_links
		WHERE source_workspace_id = e.workspace_id AND source_id = e.id
		ORDER BY name, target_workspace_id, target_id))`

// purgedMetadata is the metadata of a version a delete purged, as
// versionContent writes no metadata.
const purgedMetadata = `{}`

// versionRecord is a version's content, as versionContent selects it.
type versionRecord struct {
	project  string
	subject  string
	body     string
	metadata string
	links    string
}

// recordVersion makes a post's content its next version, made by
// origin at now, unless its latest version holds that content already.
func (v view) recordVersion(
	ctx context.Context, key Key, origin api.Origin, now string,
) error {
	var current versionRecord
	if err := v.tx.QueryRowContext(ctx, `
		SELECT `+versionContent+` FROM posts AS e
		WHERE e.workspace_id = ? AND e.id = ?`, key.Workspace, key.ID,
	).Scan(&current.project, &current.subject, &current.body,
		&current.metadata, &current.links); err != nil {
		return fmt.Errorf("reading the content of %s: %w",
			v.store.mustRef(key), err)
	}
	var latest versionRecord
	var number int64
	err := v.tx.QueryRowContext(ctx, `
		SELECT version, project, subject, body, metadata, links
		FROM post_versions WHERE workspace_id = ? AND post_id = ?
		ORDER BY version DESC LIMIT 1`, key.Workspace, key.ID,
	).Scan(&number, &latest.project, &latest.subject, &latest.body,
		&latest.metadata, &latest.links)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("reading the latest version of %s: %w",
			v.store.mustRef(key), err)
	}
	if err == nil && latest == current {
		return nil
	}

	if _, err := v.tx.ExecContext(ctx, `
		INSERT INTO post_versions (workspace_id, post_id, version,
			created_utc, origin_user, origin_host, origin_dir,
			origin_claude_session, project, subject, body, metadata, links)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		key.Workspace, key.ID, number+1, now, origin.User, origin.Host,
		origin.Dir, origin.ClaudeSession, current.project, current.subject,
		current.body, current.metadata, current.links,
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

// Version reads one version of a post the reader can read. Its links
// leave out the posts the reader cannot read now.
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
		SELECT json_extract(l.value, '$[0]'), json_extract(l.value, '$[1]'),
			json_extract(l.value, '$[2]')
		FROM post_versions AS pv, json_each(pv.links) AS l
		WHERE pv.workspace_id = ? AND pv.post_id = ? AND pv.version = ?
		ORDER BY l.key`, key.Workspace, key.ID, number)
	if err != nil {
		return api.PostVersion{}, fmt.Errorf("reading the links of "+
			"version %d of %s: %w", number, post.Ref, err)
	}
	if err := v.tx.Commit(); err != nil {
		return api.PostVersion{}, fmt.Errorf("finishing version read: %w",
			err)
	}
	return post, nil
}
