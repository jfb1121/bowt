package worktree

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jfb1121/bowt/internal/run"
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

	wt, err := New(st, run.Exec{}, "feature/login", "")
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

	if err := Remove(st, run.Exec{}, "feature/login"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(wt.Path); !os.IsNotExist(err) {
		t.Fatalf("worktree dir should be gone: %v", err)
	}
	if got, _ := List(st); len(got) != 0 {
		t.Fatalf("List after remove = %d; want 0", len(got))
	}
}

// initConfigRepo makes a temp repo, chdirs into it, and returns (mainRepo path,
// its .twig dir) so tests can drop config/hook scripts in.
func initConfigRepo(t *testing.T) (string, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
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

	twig := filepath.Join(repoDir, ".twig")
	if err := os.MkdirAll(twig, 0o755); err != nil {
		t.Fatal(err)
	}
	return repoDir, twig
}

func writeFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

// End-to-end with a real .twig/config + setup.sh: the config's GWT_PORT_BASE
// drives the port, and setup.sh receives the positional args and the exported
// BOWT_*/GWT_* environment.
func TestNewConfigAndHooks(t *testing.T) {
	_, twig := initConfigRepo(t)
	writeFile(t, filepath.Join(twig, "config"), "GWT_PORT_BASE=9000\nFOO=bar\n", 0o644)
	// setup.sh records its args + selected env into $BOWT_PATH/setup.out.
	setup := `#!/usr/bin/env bash
{
  echo "args=$1|$2|$3|$4"
  echo "BOWT_PORT=$BOWT_PORT"
  echo "GWT_PORT=$GWT_PORT"
  echo "BOWT_BRANCH=$BOWT_BRANCH"
  echo "BOWT_REPO_NAME=$BOWT_REPO_NAME"
  echo "TWIG_REPO_NAME=$TWIG_REPO_NAME"
  echo "FOO=$FOO"
  echo "AUTOENV_ASSUME_YES=$AUTOENV_ASSUME_YES"
} > "$1/setup.out"
`
	writeFile(t, filepath.Join(twig, "setup.sh"), setup, 0o755)

	st, err := state.Open()
	if err != nil {
		t.Fatal(err)
	}

	wt, err := New(st, run.Exec{}, "feature/x", "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Port base comes from GWT_PORT_BASE=9000, offset 1 → 9001.
	if wt.Port != 9001 {
		t.Fatalf("port = %d; want 9001 (config port base)", wt.Port)
	}

	out, err := os.ReadFile(filepath.Join(wt.Path, "setup.out"))
	if err != nil {
		t.Fatalf("setup.sh did not run (no setup.out): %v", err)
	}
	got := string(out)
	wants := []string{
		"args=" + wt.Path + "|feature/x|1|9001",
		"BOWT_PORT=9001",
		"GWT_PORT=9001",
		"BOWT_BRANCH=feature/x",
		"BOWT_REPO_NAME=myrepo",
		"TWIG_REPO_NAME=myrepo",
		"FOO=bar",
		"AUTOENV_ASSUME_YES=1",
	}
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("setup.out missing %q\n--- got ---\n%s", w, got)
		}
	}
}

// A failing pre-setup.sh is fatal for New (returns an error) but the worktree
// is kept on disk and in the registry.
func TestNewPreSetupFatal(t *testing.T) {
	_, twig := initConfigRepo(t)
	writeFile(t, filepath.Join(twig, "pre-setup.sh"), "#!/usr/bin/env bash\nexit 3\n", 0o755)

	st, err := state.Open()
	if err != nil {
		t.Fatal(err)
	}

	wt, err := New(st, run.Exec{}, "feature/y", "")
	if err == nil {
		t.Fatal("New: want error from failing pre-setup.sh, got nil")
	}
	// Worktree kept: on disk and registered.
	if fi, statErr := os.Stat(wt.Path); statErr != nil || !fi.IsDir() {
		t.Fatalf("worktree should be kept on disk: %v", statErr)
	}
	if _, ok, _ := st.Get(wt.Repo, "feature/y"); !ok {
		t.Fatal("worktree should stay registered after fatal pre-setup")
	}
}

// A failing setup.sh is only a warning: New succeeds, worktree is kept.
func TestNewSetupNonFatal(t *testing.T) {
	_, twig := initConfigRepo(t)
	writeFile(t, filepath.Join(twig, "setup.sh"), "#!/usr/bin/env bash\nexit 1\n", 0o755)

	st, err := state.Open()
	if err != nil {
		t.Fatal(err)
	}

	wt, err := New(st, run.Exec{}, "feature/z", "")
	if err != nil {
		t.Fatalf("New: setup.sh failure must be non-fatal, got %v", err)
	}
	if fi, statErr := os.Stat(wt.Path); statErr != nil || !fi.IsDir() {
		t.Fatalf("worktree should exist: %v", statErr)
	}
}

// teardown.sh runs before removal and receives the positional args.
func TestRemoveTeardown(t *testing.T) {
	_, twig := initConfigRepo(t)
	marker := filepath.Join(t.TempDir(), "teardown.out")
	teardown := "#!/usr/bin/env bash\necho \"$1|$2|$4\" > " + marker + "\n"
	writeFile(t, filepath.Join(twig, "teardown.sh"), teardown, 0o755)

	st, err := state.Open()
	if err != nil {
		t.Fatal(err)
	}

	wt, err := New(st, run.Exec{}, "feature/rm", "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := Remove(st, run.Exec{}, "feature/rm"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	out, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("teardown.sh did not run: %v", err)
	}
	want := wt.Path + "|feature/rm|8001"
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("teardown args = %q; want %q", strings.TrimSpace(string(out)), want)
	}
}
