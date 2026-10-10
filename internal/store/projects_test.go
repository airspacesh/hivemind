package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/airspacesh/workstream/internal/api"
)

// describeProjects lists each project as its name and recent entries,
// and its count of every entry when the response gives it.
func describeProjects(response api.ProjectsResponse) string {
	var listed []string
	for _, project := range response.Projects {
		described := fmt.Sprintf("%s %d", project.Name,
			project.RecentEntries)
		if project.Entries != nil {
			described += fmt.Sprintf("/%d", *project.Entries)
		}
		listed = append(listed, described)
	}
	return strings.Join(listed, ", ")
}

func listProjects(
	t *testing.T, store *Store, workspace string, limit int, now time.Time,
) api.ProjectsResponse {
	t.Helper()
	response, err := store.Projects(context.Background(),
		workspaceID(t, store, workspace), limit, now)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func readProject(
	t *testing.T, store *Store, name string, now time.Time,
) api.ProjectsResponse {
	t.Helper()
	response, err := store.Project(context.Background(),
		workspaceID(t, store, "default"), name, now)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func addToProject(
	t *testing.T, store *Store, workspace, project string,
) api.Entry {
	t.Helper()
	return addTo(t, store, workspace, api.AddEntryRequest{
		Subject: "duck", Project: project})
}

func TestProjectsCountOneWorkspace(t *testing.T) {
	store := openTestStore(t)
	now := time.Now()
	first := addToProject(t, store, "default", "pond")
	addToProject(t, store, "default", "pond")
	addToProject(t, store, "personal", "pond")
	addToProject(t, store, "default", "Pond")
	addToProject(t, store, "default", "nest")

	got := describeProjects(listProjects(t, store, "default", 30, now))
	if got != "nest 1, Pond 1, pond 2" {
		t.Fatalf("projects = %q", got)
	}
	got = describeProjects(listProjects(t, store, "personal", 30, now))
	if got != "pond 1" {
		t.Fatalf("personal projects = %q", got)
	}
	if got := describeProjects(readProject(t, store, "pond", now)); got !=
		"pond 2/2" {
		t.Fatalf("projects named pond = %q", got)
	}
	if got := describeProjects(readProject(t, store, "lake", now)); got !=
		"" {
		t.Fatalf("projects named lake = %q", got)
	}

	// Chat counts as activity, so a message to an old entry brings its
	// project to the top.
	mustPost(t, store, first.Ref, "still here")
	got = describeProjects(listProjects(t, store, "default", 30, now))
	if got != "pond 2, nest 1, Pond 1" {
		t.Fatalf("projects after a message = %q", got)
	}

	addToProject(t, store, "default", "nest")
	got = describeProjects(listProjects(t, store, "default", 30, now))
	if got != "nest 2, pond 2, Pond 1" {
		t.Fatalf("projects after another entry = %q", got)
	}
}

func setCreated(t *testing.T, store *Store, entry api.Ref, at time.Time) {
	t.Helper()
	key := store.mustKey(entry)
	if _, err := store.db.Exec(`
		UPDATE entries SET created_utc = ?
		WHERE workspace_id = ? AND id = ?`,
		formatTime(at), key.Workspace, key.ID); err != nil {
		t.Fatal(err)
	}
}

func TestProjectsCountEntriesCreatedInTheWindow(t *testing.T) {
	store := openTestStore(t)
	now := time.Now()
	since := now.Add(-recentWindow)
	setCreated(t, store, addToProject(t, store, "default", "pond").Ref,
		since)
	setCreated(t, store, addToProject(t, store, "default", "pond").Ref,
		since.Add(-time.Nanosecond))
	addToProject(t, store, "default", "pond")
	setCreated(t, store, addToProject(t, store, "default", "lake").Ref,
		since.Add(-time.Hour))

	all := listProjects(t, store, "default", 30, now)
	if got := describeProjects(all); got != "lake 0, pond 2" {
		t.Fatalf("projects = %q", got)
	}
	if !all.RecentSince.Equal(since) || all.RecentEntries != 2 ||
		all.Count != 2 {
		t.Fatalf("since %v with %d entries in %d projects, want since %v "+
			"with 2 in 2", all.RecentSince, all.RecentEntries, all.Count,
			since)
	}
	pond := readProject(t, store, "pond", now)
	if got := describeProjects(pond); got != "pond 2/3" ||
		pond.RecentEntries != 2 {
		t.Fatalf("project pond = %q of %d recent entries", got,
			pond.RecentEntries)
	}
	if got := describeProjects(readProject(t, store, "lake", now)); got !=
		"lake 0/1" {
		t.Fatalf("project lake = %q", got)
	}

	limited := listProjects(t, store, "default", 1, now)
	if got := describeProjects(limited); got != "lake 0" ||
		limited.Count != 2 || limited.RecentEntries != 2 {
		t.Fatalf("one project = %q of %d with %d recent entries", got,
			limited.Count, limited.RecentEntries)
	}
}
