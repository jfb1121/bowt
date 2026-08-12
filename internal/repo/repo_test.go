package repo

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// initRepo makes a temp git repo with one empty commit and returns its path.
func initRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", dir)
	git("-C", dir, "config", "user.email", "t@bowt.dev")
	git("-C", dir, "config", "user.name", "bowt test")
	git("-C", dir, "commit", "--allow-empty", "-m", "init")
	return dir
}

// TestDeleteRemoteBranch: deletes an existing remote branch, and is a no-op on a
// ref that was never pushed. A local bare repo stands in for "origin".
func TestDeleteRemoteBranch(t *testing.T) {
	git := func(dir string, args ...string) {
		t.Helper()
		full := append([]string{"-C", dir}, args...)
		if out, err := exec.Command("git", full...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	origin := t.TempDir()
	if out, err := exec.Command("git", "init", "--bare", origin).CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v\n%s", err, out)
	}

	work := initRepo(t)
	git(work, "remote", "add", "origin", origin)
	git(work, "branch", "feature")
	git(work, "push", "-q", "origin", "feature")

	// Sanity: the remote ref exists before we delete it.
	if out, err := exec.Command("git", "-C", origin, "rev-parse", "--verify", "refs/heads/feature").CombinedOutput(); err != nil {
		t.Fatalf("remote branch not pushed: %v\n%s", err, out)
	}

	// Deleting the existing branch succeeds and removes the remote ref.
	if err := DeleteRemoteBranch(work, "origin", "feature"); err != nil {
		t.Fatalf("DeleteRemoteBranch(existing): %v", err)
	}
	if out, err := exec.Command("git", "-C", origin, "rev-parse", "--verify", "refs/heads/feature").CombinedOutput(); err == nil {
		t.Fatalf("remote branch still present after delete: %s", out)
	}

	// Deleting a ref that never existed is a no-op success, not an error.
	if err := DeleteRemoteBranch(work, "origin", "never-pushed"); err != nil {
		t.Fatalf("DeleteRemoteBranch(missing) = %v; want no-op success", err)
	}
}

func TestResolution(t *testing.T) {
	dir := initRepo(t)

	old, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(old) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}

	main, err := MainRepo()
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(main) {
		t.Fatalf("MainRepo not absolute: %s", main)
	}
	// Compare basenames only: macOS temp dirs live under a /var -> /private/var
	// symlink, so the resolved path differs from dir but the basename does not.
	if filepath.Base(main) != filepath.Base(dir) {
		t.Fatalf("MainRepo base = %s; want %s", filepath.Base(main), filepath.Base(dir))
	}

	name, err := Name()
	if err != nil {
		t.Fatal(err)
	}
	if name != filepath.Base(dir) {
		t.Fatalf("Name = %s; want %s", name, filepath.Base(dir))
	}

	if b := CurrentBranch(dir); b == "" {
		t.Fatal("CurrentBranch empty after a commit")
	}
}
