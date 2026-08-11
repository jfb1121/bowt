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

// Head returns HEAD's full and abbreviated commit SHA for the checkout at dir.
// gate records both so a reader can compare .commit to HEAD (stale-verdict
// detection) while still logging the human-friendly short form.
func Head(dir string) (full, short string, err error) {
	full, err = run(dir, "rev-parse", "HEAD")
	if err != nil {
		return "", "", err
	}
	short, err = run(dir, "rev-parse", "--short", "HEAD")
	if err != nil {
		return "", "", err
	}
	return full, short, nil
}

// Dirty reports whether the checkout at dir has uncommitted changes (tracked or
// untracked). gate records this so a verdict on a dirty tree is never mistaken
// for one on a clean commit.
func Dirty(dir string) (bool, error) {
	out, err := run(dir, "status", "--porcelain")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) != "", nil
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

// RevParse resolves ref to its full commit SHA in dir.
func RevParse(dir, ref string) (string, error) {
	return run(dir, "rev-parse", ref)
}

// IsAncestor reports whether commit a is an ancestor of commit b in dir — i.e.
// b can fast-forward from a. It maps `git merge-base --is-ancestor`'s exit
// codes: 0 → true, 1 → false, anything else → a real error. This is the check
// `land` uses to prove a branch fast-forwards onto its base before merging.
func IsAncestor(dir, a, b string) (bool, error) {
	cmd := exec.Command("git", "merge-base", "--is-ancestor", a, b)
	if dir != "" {
		cmd.Dir = dir
	}
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("git merge-base --is-ancestor %s %s: %w", a, b, err)
}

// MergeFFOnly fast-forwards the branch checked out in dir up to ref, refusing
// to create a merge commit (`git merge --ff-only`). It either advances cleanly
// or fails leaving the working tree and ref untouched — never a partial merge.
func MergeFFOnly(dir, ref string) error {
	_, err := run(dir, "merge", "--ff-only", ref)
	return err
}

// UpdateRef moves branch ref to newVal, asserting its current value is oldVal —
// an atomic, guarded pointer move that does not touch any working tree. `land`
// uses it to fast-forward a base branch that isn't the main repo's checkout.
func UpdateRef(dir, ref, newVal, oldVal string) error {
	_, err := run(dir, "update-ref", ref, newVal, oldVal)
	return err
}

// Push pushes branch to remote from dir.
func Push(dir, remote, branch string) error {
	_, err := run(dir, "push", remote, branch)
	return err
}

// DeleteBranch deletes a merged local branch (`git branch -d`, which refuses an
// unmerged branch) in dir.
func DeleteBranch(dir, branch string) error {
	_, err := run(dir, "branch", "-d", branch)
	return err
}
