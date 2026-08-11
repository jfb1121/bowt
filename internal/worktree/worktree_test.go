package worktree

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/jfb1121/bowt/internal/state"
)

func TestSlug(t *testing.T) {
	cases := map[string]string{
		"feature/login": "feature-login",
		"x":             "x",
		"a/b/c":         "a-b-c",
	}
	for in, want := range cases {
		if got := slug(in); got != want {
			t.Errorf("slug(%q) = %q; want %q", in, got, want)
		}
	}
}

// End-to-end against a real temp git repo: create → list → path → remove.
func TestNewListPathRemove(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home) // worktrees + ~/.bowt land here, not in real $HOME
	repoDir := filepath.Join(home, "myrepo")

	git := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", repoDir)
	git("-C", repoDir, "config", "user.email", "t@bowt.dev")
	git("-C", repoDir, "config", "user.name", "bowt test")
	git("-C", repoDir, "commit", "--allow-empty", "-m", "init")

	old, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(old) })
	if err := os.Chdir(repoDir); err != nil {
		t.Fatal(err)
	}

	st, err := state.Open() // uses $HOME/.bowt (the temp one)
	if err != nil {
		t.Fatal(err)
	}

	wt, err := New(st, "feature/login", "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if wt.Offset != 1 || wt.Port != 8001 {
		t.Fatalf("offset/port = %d/%d; want 1/8001", wt.Offset, wt.Port)
	}
	if fi, err := os.Stat(wt.Path); err != nil || !fi.IsDir() {
		t.Fatalf("worktree dir missing at %s: %v", wt.Path, err)
	}

	got, err := List(st)
	if err != nil || len(got) != 1 {
		t.Fatalf("List = %v, %v; want exactly 1", got, err)
	}

	p, err := Path(st, "feature/login")
	if err != nil || p != wt.Path {
		t.Fatalf("Path = %q, %v; want %q", p, err, wt.Path)
	}

	if err := Remove(st, "feature/login"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(wt.Path); !os.IsNotExist(err) {
		t.Fatalf("worktree dir should be gone: %v", err)
	}
	if got, _ := List(st); len(got) != 0 {
		t.Fatalf("List after remove = %d; want 0", len(got))
	}
}
