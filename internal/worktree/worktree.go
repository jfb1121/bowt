// Package worktree ties git operations and the state store together: creating a
// worktree allocates an offset and a port, registers it, and creates the git
// worktree; removing does the reverse.
package worktree

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jfb1121/bowt/internal/repo"
	"github.com/jfb1121/bowt/internal/state"
)

// portBase is the first port; offset N gets portBase+N. (Config comes later.)
const portBase = 8000

// slug turns a branch name into a directory-safe form (feature/x -> feature-x).
func slug(branch string) string {
	return strings.ReplaceAll(branch, "/", "-")
}

// dir returns the worktree path for a branch: ~/<repo>-worktrees/<slug>.
func dir(repoName, branch string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, repoName+"-worktrees", slug(branch)), nil
}

// New creates a worktree for branch (from base, defaulting to the main repo's
// current branch), registers it, and returns the created record.
func New(st state.Store, branch, base string) (state.Worktree, error) {
	main, err := repo.MainRepo()
	if err != nil {
		return state.Worktree{}, err
	}
	name := filepath.Base(main)

	if _, ok, err := st.Get(name, branch); err != nil {
		return state.Worktree{}, err
	} else if ok {
		return state.Worktree{}, fmt.Errorf("worktree for %q already exists (bowt cd %s)", branch, branch)
	}

	path, err := dir(name, branch)
	if err != nil {
		return state.Worktree{}, err
	}
	if base == "" {
		base = repo.DefaultBase(main)
	}

	offset, err := st.NextOffset(name)
	if err != nil {
		return state.Worktree{}, err
	}
	port := portBase + offset

	if err := repo.AddWorktree(main, path, branch, base); err != nil {
		return state.Worktree{}, err
	}

	wt := state.Worktree{
		Repo:    name,
		Branch:  branch,
		Offset:  offset,
		Port:    port,
		Path:    path,
		Created: time.Now(),
	}
	if err := st.Add(wt); err != nil {
		// Roll back the git worktree so disk and registry stay in agreement.
		_ = repo.RemoveWorktree(main, path)
		return state.Worktree{}, err
	}
	return wt, nil
}

// Remove tears down and deregisters a worktree.
func Remove(st state.Store, branch string) error {
	main, err := repo.MainRepo()
	if err != nil {
		return err
	}
	name := filepath.Base(main)

	wt, ok, err := st.Get(name, branch)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("no worktree registered for %q", branch)
	}
	if err := repo.RemoveWorktree(main, wt.Path); err != nil {
		return err
	}
	return st.Remove(name, branch)
}

// Path returns the on-disk path for a branch, so an agent (or the shell shim)
// can cd into it.
func Path(st state.Store, branch string) (string, error) {
	name, err := repo.Name()
	if err != nil {
		return "", err
	}
	wt, ok, err := st.Get(name, branch)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("no worktree registered for %q", branch)
	}
	return wt.Path, nil
}

// List returns all worktrees registered for the current repo.
func List(st state.Store) ([]state.Worktree, error) {
	name, err := repo.Name()
	if err != nil {
		return nil, err
	}
	return st.List(name)
}
