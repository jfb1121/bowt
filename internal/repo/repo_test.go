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
