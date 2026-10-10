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
	// recentWindow is how far back a project's recent entries reach.
	recentWindow    = 90 * 24 * time.Hour
	MaxProjectLimit = 500
)

var ErrInvalidProjectLimit = errors.New("invalid project limit")

// projectNames names a workspace's projects in a CTE, seeking each name
// in an index rather than reading every entry.
const projectNames = `
	WITH RECURSIVE names(project) AS (
		SELECT min(project) FROM entries WHERE workspace_id = ?1
		UNION ALL
		SELECT (SELECT min(project) FROM entries
			WHERE workspace_id = ?1 AND project > names.project)
		FROM names WHERE names.project IS NOT NULL
	)`

// Projects lists a workspace's most recently active projects, at most
// limit of them, with their entries created in the recentWindow before
// now.
func (s *Store) Projects(
	ctx context.Context, workspace int64, limit int, now time.Time,
) (api.ProjectsResponse, error) {
	if limit < 1 || limit > MaxProjectLimit {
		return api.ProjectsResponse{}, fmt.Errorf(
			"%w: the limit must be between 1 and %d",
			ErrInvalidProjectLimit, MaxProjectLimit)
	}
	since := now.Add(-recentWindow)
	rows, err := s.reads.QueryContext(ctx, projectNames+`
		SELECT project, (
			SELECT max(activity_utc) FROM entries
			WHERE workspace_id = ?1 AND project = names.project
		) AS latest, (
			SELECT count(*) FROM entries
			WHERE workspace_id = ?1 AND project = names.project
				AND created_utc >= ?2
		)
		FROM names WHERE project IS NOT NULL
		ORDER BY latest DESC, project`, workspace, formatTime(since),
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
		var latest string
		if err := rows.Scan(
			&project.Name, &latest, &project.RecentEntries,
		); err != nil {
			return api.ProjectsResponse{}, fmt.Errorf(
				"listing projects: %w", err)
		}
		if project.Latest, err = parseTime(latest); err != nil {
			return api.ProjectsResponse{}, err
		}
		response.Count++
		response.RecentEntries += project.RecentEntries
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
// its count of every entry. It reads only that project's entries.
func (s *Store) Project(
	ctx context.Context, workspace int64, name string, now time.Time,
) (api.ProjectsResponse, error) {
	since := now.Add(-recentWindow)
	var latest sql.NullString
	var entries, recent int
	if err := s.reads.QueryRowContext(ctx, `
		SELECT max(activity_utc), count(*), (
			SELECT count(*) FROM entries
			WHERE workspace_id = ?1 AND project = ?2
				AND created_utc >= ?3
		)
		FROM entries WHERE workspace_id = ?1 AND project = ?2`,
		workspace, name, formatTime(since),
	).Scan(&latest, &entries, &recent); err != nil {
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
		Name: name, RecentEntries: recent, Entries: &entries,
	}
	var err error
	if project.Latest, err = parseTime(latest.String); err != nil {
		return api.ProjectsResponse{}, err
	}
	response.Projects = append(response.Projects, project)
	response.Count = 1
	response.RecentEntries = recent
	return response, nil
}
