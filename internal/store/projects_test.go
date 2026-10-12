package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/airspacesh/workstream/internal/api"
)

// describeProjects lists each project as its name and recent posts, and
// its count of every post when the response gives it.
func describeProjects(response api.ProjectsResponse) string {
	var listed []string
	for _, project := range response.Projects {
		described := fmt.Sprintf("%s %d", project.Name,
			project.RecentPosts)
		if project.Posts != nil {
			described += fmt.Sprintf("/%d", *project.Posts)
		}
		listed = append(listed, described)
	}
	return strings.Join(listed, ", ")
}

func listProjects(
	t *testing.T, store *Store, workspace string, limit int, now time.Time,
) api.ProjectsResponse {
	t.Helper()
	response, err := store.Projects(context.Background(), everyAccess(store),
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
	response, err := store.Project(context.Background(), everyAccess(store),
		workspaceID(t, store, "general"), name, now)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func addToProject(
	t *testing.T, store *Store, workspace, project string,
) api.Post {
	t.Helper()
	return addTo(t, store, workspace, api.AddPostRequest{
		Subject: "duck", Project: project})
}

func TestProjectsCountOneWorkspace(t *testing.T) {
	store := openTestStore(t)
	now := time.Now()
	first := addToProject(t, store, "general", "pond")
	addToProject(t, store, "general", "pond")
	addToProject(t, store, "personal", "pond")
	addToProject(t, store, "general", "Pond")
	addToProject(t, store, "general", "nest")

	got := describeProjects(listProjects(t, store, "general", 30, now))
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

	// Chat counts as activity, so a message to an old post brings its
	// project to the top.
	mustPostMessage(t, store, first.Ref, "still here")
	got = describeProjects(listProjects(t, store, "general", 30, now))
	if got != "pond 2, nest 1, Pond 1" {
		t.Fatalf("projects after a message = %q", got)
	}

	addToProject(t, store, "general", "nest")
	got = describeProjects(listProjects(t, store, "general", 30, now))
	if got != "nest 2, pond 2, Pond 1" {
		t.Fatalf("projects after another post = %q", got)
	}
}

func setCreated(t *testing.T, store *Store, post api.Ref, at time.Time) {
	t.Helper()
	key := store.mustKey(post)
	if _, err := store.db.Exec(`
		UPDATE posts SET created_utc = ?
		WHERE workspace_id = ? AND id = ?`,
		formatTime(at), key.Workspace, key.ID); err != nil {
		t.Fatal(err)
	}
}

func TestProjectsCountPostsCreatedInTheWindow(t *testing.T) {
	store := openTestStore(t)
	now := time.Now()
	since := now.Add(-recentWindow)
	setCreated(t, store, addToProject(t, store, "general", "pond").Ref,
		since)
	setCreated(t, store, addToProject(t, store, "general", "pond").Ref,
		since.Add(-time.Nanosecond))
	addToProject(t, store, "general", "pond")
	setCreated(t, store, addToProject(t, store, "general", "lake").Ref,
		since.Add(-time.Hour))

	all := listProjects(t, store, "general", 30, now)
	if got := describeProjects(all); got != "lake 0, pond 2" {
		t.Fatalf("projects = %q", got)
	}
	if !all.RecentSince.Equal(since) || all.RecentPosts != 2 ||
		all.Count != 2 {
		t.Fatalf("since %v with %d posts in %d projects, want since %v "+
			"with 2 in 2", all.RecentSince, all.RecentPosts, all.Count,
			since)
	}
	pond := readProject(t, store, "pond", now)
	if got := describeProjects(pond); got != "pond 2/3" ||
		pond.RecentPosts != 2 {
		t.Fatalf("project pond = %q of %d recent posts", got,
			pond.RecentPosts)
	}
	if got := describeProjects(readProject(t, store, "lake", now)); got !=
		"lake 0/1" {
		t.Fatalf("project lake = %q", got)
	}

	limited := listProjects(t, store, "general", 1, now)
	if got := describeProjects(limited); got != "lake 0" ||
		limited.Count != 2 || limited.RecentPosts != 2 {
		t.Fatalf("one project = %q of %d with %d recent posts", got,
			limited.Count, limited.RecentPosts)
	}
}

// The counts leave deleted posts out, but a delete is activity, so a
// project whose every post is deleted still lists, with none.
func TestProjectsLeaveDeletedPostsOutOfTheCounts(t *testing.T) {
	store := openTestStore(t)
	now := time.Now()
	addToProject(t, store, "general", "pond")
	mustDelete(t, store, addToProject(t, store, "general", "pond").Ref)
	mustDelete(t, store, addToProject(t, store, "general", "nest").Ref)

	all := listProjects(t, store, "general", 30, now)
	if got := describeProjects(all); got != "nest 0, pond 1" ||
		all.RecentPosts != 1 {
		t.Fatalf("projects = %q with %d recent posts", got,
			all.RecentPosts)
	}
	if got := describeProjects(readProject(t, store, "pond", now)); got !=
		"pond 1/1" {
		t.Fatalf("project pond = %q", got)
	}
	if got := describeProjects(readProject(t, store, "nest", now)); got !=
		"nest 0/0" {
		t.Fatalf("project nest = %q", got)
	}
}
