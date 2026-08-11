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

	"github.com/jfb1121/bowt/internal/config"
	"github.com/jfb1121/bowt/internal/env"
	"github.com/jfb1121/bowt/internal/hook"
	"github.com/jfb1121/bowt/internal/repo"
	"github.com/jfb1121/bowt/internal/run"
	"github.com/jfb1121/bowt/internal/state"
)

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
// current branch), registers it, runs the pre-setup/setup lifecycle hooks, and
// returns the created record. The port base comes from the repo's config
// (BOWT_PORT_BASE / GWT_PORT_BASE), defaulting to config.DefaultPortBase.
//
// Hook policy matches the brief (and diverges from twig on one point, noted in
// STATUS.md): a failing setup.sh is a warning — the worktree is kept, not
// rolled back — while a failing pre-setup.sh is fatal for `new` (returns an
// error), though the worktree is still kept on disk for the fix-and-retry flow.
func New(st state.Store, r run.Runner, branch, base string) (state.Worktree, error) {
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

	cfgDir := config.Dir(main)
	vars, err := config.Load(r, cfgDir)
	if err != nil {
		return state.Worktree{}, fmt.Errorf("load config: %w", err)
	}

	offset, err := st.NextOffset(name)
	if err != nil {
		return state.Worktree{}, err
	}
	port := vars.PortBase() + offset

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

	// Lifecycle hooks run only when the repo has a config dir with scripts.
	if cfgDir != "" {
		hookEnv := env.Build(env.Info{
			Path:     path,
			Branch:   branch,
			Offset:   offset,
			Port:     port,
			MainRepo: main,
			RepoName: name,
		}, vars)
		hookArgs := hook.Args{Path: path, Branch: branch, Offset: offset, Port: port}

		if _, err := hook.Run(r, cfgDir, hook.PreSetup, hookArgs, hookEnv); err != nil {
			// Fatal for new; the worktree is kept so the user can fix and retry.
			return wt, fmt.Errorf("%w — worktree kept, fix and rerun setup", err)
		}
		if _, err := hook.Run(r, cfgDir, hook.Setup, hookArgs, hookEnv); err != nil {
			// Non-fatal: keep the worktree, warn, let the caller decide.
			fmt.Fprintf(os.Stderr, "bowt: %v — worktree kept for debugging\n", err)
		}
	}
	return wt, nil
}

// Remove runs the teardown hook (best-effort) and then tears down and
// deregisters a worktree. A failing teardown.sh is a warning — removal still
// proceeds — matching twig.
func Remove(st state.Store, r run.Runner, branch string) error {
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

	// Teardown before removal, only while the worktree still exists on disk.
	if cfgDir := config.Dir(main); cfgDir != "" {
		if _, statErr := os.Stat(wt.Path); statErr == nil {
			vars, err := config.Load(r, cfgDir)
			if err != nil {
				fmt.Fprintf(os.Stderr, "bowt: load config: %v — skipping teardown\n", err)
			} else {
				hookEnv := env.Build(env.Info{
					Path:     wt.Path,
					Branch:   branch,
					Offset:   wt.Offset,
					Port:     wt.Port,
					MainRepo: main,
					RepoName: name,
				}, vars)
				hookArgs := hook.Args{Path: wt.Path, Branch: branch, Offset: wt.Offset, Port: wt.Port}
				if _, err := hook.Run(r, cfgDir, hook.Teardown, hookArgs, hookEnv); err != nil {
					fmt.Fprintf(os.Stderr, "bowt: %v — continuing with removal\n", err)
				}
			}
		}
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
