package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"

	"github.com/airspacesh/workstream/internal/api"
)

var ErrUnknownPrefix = errors.New("no workspace has prefix")

// Workspace is a configured workspace with the id the database gave it.
type Workspace struct {
	ID     int64
	Name   string
	Prefix string
}

// Key names a post inside the database by its workspace id and its
// number within that workspace.
type Key struct {
	Workspace int64
	ID        int64
}

// Access is the set of workspaces a request can read, and so write,
// and the user it reads as. Every post outside the workspaces is
// unreadable. It shows as its bare ref in a link or an ancestor path,
// and nowhere else. A post private to another user is unreadable
// everywhere, as if it did not exist. The user is the one the client
// names, unchecked, so a private post is kept out of other users' way
// rather than kept secret.
type Access struct {
	user       string
	workspaces []int64
}

func NewAccess(user string, workspaces ...int64) Access {
	return Access{user: user, workspaces: slices.Clone(workspaces)}
}

func (a Access) CanRead(workspace int64) bool {
	return slices.Contains(a.workspaces, workspace)
}

// syncWorkspaces adds each configured workspace the database lacks and
// moves a changed prefix, in one transaction. It returns the
// configured workspaces with their ids, and the names of the
// workspaces the database holds that the config does not.
func syncWorkspaces(
	ctx context.Context, conn *sql.Conn, configured []api.Workspace,
) ([]Workspace, []string, error) {
	held := map[string]sql.NullString{}
	rows, err := conn.QueryContext(ctx, `SELECT name, prefix FROM workspaces`)
	if err != nil {
		return nil, nil, fmt.Errorf("reading workspaces: %w", err)
	}
	for rows.Next() {
		var name string
		var prefix sql.NullString
		if err := rows.Scan(&name, &prefix); err != nil {
			rows.Close()
			return nil, nil, fmt.Errorf("reading workspaces: %w", err)
		}
		held[name] = prefix
	}
	if err := rows.Close(); err != nil {
		return nil, nil, fmt.Errorf("reading workspaces: %w", err)
	}

	wanted := map[string]string{}
	for _, workspace := range configured {
		wanted[workspace.Name] = workspace.Prefix
	}
	var unconfigured []string
	for name, prefix := range held {
		want, ok := wanted[name]
		if !ok {
			unconfigured = append(unconfigured, name)
		}
		if prefix.Valid && prefix.String != want {
			if _, err := conn.ExecContext(ctx, `
				UPDATE workspaces SET prefix = NULL WHERE name = ?`, name,
			); err != nil {
				return nil, nil, fmt.Errorf("releasing prefix %s: %w",
					prefix.String, err)
			}
		}
	}
	slices.Sort(unconfigured)

	workspaces := make([]Workspace, len(configured))
	for i, workspace := range configured {
		if err := conn.QueryRowContext(ctx, `
			INSERT INTO workspaces (name, prefix) VALUES (?, ?)
			ON CONFLICT (name) DO UPDATE SET prefix = excluded.prefix
			RETURNING id`, workspace.Name, workspace.Prefix,
		).Scan(&workspaces[i].ID); err != nil {
			return nil, nil, fmt.Errorf("adding workspace %s: %w",
				workspace.Name, err)
		}
		workspaces[i].Name = workspace.Name
		workspaces[i].Prefix = workspace.Prefix
	}
	return workspaces, unconfigured, nil
}

// Workspaces returns the configured workspaces, in config order.
func (s *Store) Workspaces() []Workspace {
	return slices.Clone(s.workspaces)
}

// Unconfigured names the workspaces the database holds that the config
// does not. Their posts are unreachable.
func (s *Store) Unconfigured() []string {
	return slices.Clone(s.unconfigured)
}

// Resolve finds the post key a ref names, without checking the post
// exists.
func (s *Store) Resolve(ref api.Ref) (Key, error) {
	for _, workspace := range s.workspaces {
		if workspace.Prefix == ref.Prefix {
			return Key{Workspace: workspace.ID, ID: ref.ID}, nil
		}
	}
	return Key{}, fmt.Errorf("%w %s", ErrUnknownPrefix, ref.Prefix)
}

// ref names a post by its prefixed id. A post in a workspace the config
// does not name has none.
func (s *Store) ref(key Key) (api.Ref, bool) {
	for _, workspace := range s.workspaces {
		if workspace.ID == key.Workspace {
			return api.Ref{Prefix: workspace.Prefix, ID: key.ID}, true
		}
	}
	return api.Ref{}, false
}

// mustRef names a post in a configured workspace, which every post a
// request reaches directly is.
func (s *Store) mustRef(key Key) api.Ref {
	ref, ok := s.ref(key)
	if !ok {
		panic(fmt.Sprintf("post %+v is in an unconfigured workspace", key))
	}
	return ref
}

// mustKey finds the key of a post the store named itself.
func (s *Store) mustKey(ref api.Ref) Key {
	key, err := s.Resolve(ref)
	if err != nil {
		panic(err)
	}
	return key
}
