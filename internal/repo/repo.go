// Package repo wraps the git commands bowt needs. Every git interaction goes
// through here, so the rest of the code never shells out to git directly.
package repo

import (
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// run executes a git command and returns trimmed stdout. On failure it returns
// an error that includes git's own stderr — so callers see "fatal: not a git
// repository", not a bare "exit status 128".
func run(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.Output() // captures stdout; stderr lands in ExitError.Stderr
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(string(ee.Stderr)))
		}
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out)), nil
}

// MainRepo returns the absolute path of the main repository root, working even
// from inside a worktree.
func MainRepo() (string, error) {
	common, err := run("", "rev-parse", "--git-common-dir")
	if err != nil {
		return "", err
	}
	if common == ".git" {
		return run("", "rev-parse", "--show-toplevel")
	}
	abs, err := filepath.Abs(common)
	if err != nil {
		return "", err
	}
	return filepath.Dir(abs), nil
}

// Toplevel returns the absolute path of the current worktree's root (the
// checkout you are standing in), working from anywhere inside it. Unlike
// MainRepo this stays inside the worktree, so it uniquely identifies the
// worktree bowt is operating on.
func Toplevel(dir string) (string, error) {
	return run(dir, "rev-parse", "--show-toplevel")
}

// Name returns the repo's basename — the key we register worktrees under.
func Name() (string, error) {
	main, err := MainRepo()
	if err != nil {
		return "", err
	}
	return filepath.Base(main), nil
}

// CurrentBranch returns the checked-out branch of dir, or "" if detached.
func CurrentBranch(dir string) string {
	b, _ := run(dir, "symbolic-ref", "--short", "HEAD")
	return b
}

// BranchExists reports whether a local branch ref exists.
func BranchExists(mainRepo, branch string) bool {
	_, err := run(mainRepo, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	return err == nil
}

// AddWorktree creates a worktree at path for branch. If the branch does not
// exist yet, it is created from base.
func AddWorktree(mainRepo, path, branch, base string) error {
	var err error
	if BranchExists(mainRepo, branch) {
		_, err = run(mainRepo, "worktree", "add", path, branch)
	} else {
		_, err = run(mainRepo, "worktree", "add", "-b", branch, path, base)
	}
	return err
}

// RemoveWorktree force-removes the worktree at path.
func RemoveWorktree(mainRepo, path string) error {
	_, err := run(mainRepo, "worktree", "remove", "--force", path)
	return err
}

// DefaultBase returns the main repo's current branch, falling back to "main".
func DefaultBase(mainRepo string) string {
	if b := CurrentBranch(mainRepo); b != "" {
		return b
	}
	return "main"
}
