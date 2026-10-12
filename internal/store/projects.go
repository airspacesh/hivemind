package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/airspacesh/workstream/internal/api"
)

const (
	// recentWindow is how far back a project's recent posts reach.
	recentWindow    = 90 * 24 * time.Hour
	MaxProjectLimit = 500
)

var ErrInvalidProjectLimit = errors.New("invalid project limit")

// projectNames names a workspace's projects in a CTE, seeking each name
// in an index rather than reading every post.
const projectNames = `
	WITH RECURSIVE names(project) AS (
		SELECT min(project) FROM posts WHERE workspace_id = ?1
		UNION ALL
		SELECT (SELECT min(project) FROM posts
			WHERE workspace_id = ?1 AND project > names.project)
		FROM names WHERE names.project IS NOT NULL
	)`

// Projects lists a workspace's most recently active projects, at most
// limit of them, with their posts created in the recentWindow before
// now. It counts only the posts the reader can read that are not
// deleted, but a delete is activity. It leaves out a project with no
// post the reader can read, and unless includeDeleted is set, one whose
// posts the reader can read are all deleted. Each count takes away the
// posts private to others, as createPrivateProjectCreatedIndex
// explains.
func (s *Store) Projects(
	ctx context.Context,
	access Access,
	workspace int64,
	includeDeleted bool,
	limit int,
	now time.Time,
) (api.ProjectsResponse, error) {
	if limit < 1 || limit > MaxProjectLimit {
		return api.ProjectsResponse{}, fmt.Errorf(
			"%w: the limit must be between 1 and %d",
			ErrInvalidProjectLimit, MaxProjectLimit)
	}
	since := now.Add(-recentWindow)
	rows, err := s.reads.QueryContext(ctx, projectNames+`
		SELECT project, (
			SELECT max(activity_utc) FROM posts
			WHERE workspace_id = ?1 AND project = names.project
				AND private_to IN ('', ?3)
		) AS latest, (
			SELECT count(*) FROM posts
			WHERE workspace_id = ?1 AND project = names.project
				AND created_utc >= ?2 AND NOT deleted
		) - (
			SELECT count(*) FROM posts
			WHERE workspace_id = ?1 AND project = names.project
				AND created_utc >= ?2 AND NOT deleted
				AND private_to <> '' AND private_to <> ?3
		), ?4 OR EXISTS (
			SELECT 1 FROM posts
			WHERE workspace_id = ?1 AND project = names.project
				AND NOT deleted AND private_to IN ('', ?3)
		)
		FROM names WHERE project IS NOT NULL
		ORDER BY latest DESC, project`, workspace, formatTime(since),
		access.user, includeDeleted,
	)
	if err != nil {
		return api.ProjectsResponse{}, fmt.Errorf("listing projects: %w", err)
	}
	defer rows.Close()

	response := api.ProjectsResponse{
		Projects: []api.Project{}, RecentSince: since,
	}
	for rows.Next() {
		var project api.Project
		var latest sql.NullString
		var listed bool
		if err := rows.Scan(
			&project.Name, &latest, &project.RecentPosts, &listed,
		); err != nil {
			return api.ProjectsResponse{}, fmt.Errorf(
				"listing projects: %w", err)
		}
		// A project has no latest activity when the reader can read none
		// of its posts. Filtering on it in the query would have SQLite
		// read each project's latest activity twice.
		if !latest.Valid || !listed {
			continue
		}
		if project.Latest, err = parseTime(latest.String); err != nil {
			return api.ProjectsResponse{}, err
		}
		response.Count++
		response.RecentPosts += project.RecentPosts
		if len(response.Projects) < limit {
			response.Projects = append(response.Projects, project)
		}
	}
	if err := rows.Err(); err != nil {
		return api.ProjectsResponse{}, fmt.Errorf("listing projects: %w", err)
	}
	return response, nil
}

// Project reads one project of a workspace as Projects lists it, adding
// its count of every post that is not deleted and the reader can read.
// It reads only that project's posts.
func (s *Store) Project(
	ctx context.Context,
	access Access,
	workspace int64,
	name string,
	now time.Time,
) (api.ProjectsResponse, error) {
	since := now.Add(-recentWindow)
	var latest sql.NullString
	var posts, recent int
	if err := s.reads.QueryRowContext(ctx, `
		SELECT (
			SELECT max(activity_utc) FROM posts
			WHERE workspace_id = ?1 AND project = ?2
				AND private_to IN ('', ?4)
		), (
			SELECT count(*) FROM posts
			WHERE workspace_id = ?1 AND project = ?2 AND NOT deleted
		) - (
			SELECT count(*) FROM posts
			WHERE workspace_id = ?1 AND project = ?2 AND NOT deleted
				AND private_to <> '' AND private_to <> ?4
		), (
			SELECT count(*) FROM posts
			WHERE workspace_id = ?1 AND project = ?2
				AND created_utc >= ?3 AND NOT deleted
		) - (
			SELECT count(*) FROM posts
			WHERE workspace_id = ?1 AND project = ?2
				AND created_utc >= ?3 AND NOT deleted
				AND private_to <> '' AND private_to <> ?4
		)`,
		workspace, name, formatTime(since), access.user,
	).Scan(&latest, &posts, &recent); err != nil {
		return api.ProjectsResponse{}, fmt.Errorf(
			"reading project %q: %w", name, err)
	}

	response := api.ProjectsResponse{
		Projects: []api.Project{}, RecentSince: since,
	}
	if !latest.Valid {
		return response, nil
	}
	project := api.Project{
		Name: name, RecentPosts: recent, Posts: &posts,
	}
	var err error
	if project.Latest, err = parseTime(latest.String); err != nil {
		return api.ProjectsResponse{}, err
	}
	response.Projects = append(response.Projects, project)
	response.Count = 1
	response.RecentPosts = recent
	return response, nil
}
