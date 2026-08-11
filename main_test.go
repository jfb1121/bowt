package main

import (
	"bytes"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/jfb1121/bowt/internal/repo"
	"github.com/jfb1121/bowt/internal/state"
)

// execRoot runs the cobra tree with args, capturing stdout+stderr.
func execRoot(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := newRootCmd()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs(args)
	err := root.Execute()
	return buf.String(), err
}

// fixtureRepo creates a throwaway git repo, chdirs into it, and returns its
// registry name (the basename repo.Name() will report).
func fixtureRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(dir)
	if out, err := exec.Command("git", "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	name, err := repo.Name()
	if err != nil {
		t.Fatalf("repo.Name: %v", err)
	}
	return name
}

// seedStore opens a fresh store at a temp path and registers the given branches
// under repoName.
func seedStore(t *testing.T, repoName string, branches ...string) state.Store {
	t.Helper()
	st, err := state.OpenAt(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("OpenAt: %v", err)
	}
	for i, b := range branches {
		if err := st.Add(state.Worktree{
			Repo:    repoName,
			Branch:  b,
			Offset:  i + 1,
			Port:    3000 + i + 1,
			Path:    filepath.Join(t.TempDir(), b),
			Created: time.Now(),
		}); err != nil {
			t.Fatalf("seed %q: %v", b, err)
		}
	}
	return st
}

// The dynamic-completion payoff: branchNames returns the registered branches for
// the current repo, given a seeded store + a fixture git repo.
func TestBranchNamesFromRegistry(t *testing.T) {
	name := fixtureRepo(t)
	st := seedStore(t, name, "feature/login", "hotfix", "spike")

	got, err := branchNames(st)
	if err != nil {
		t.Fatalf("branchNames: %v", err)
	}
	sort.Strings(got)
	want := []string{"feature/login", "hotfix", "spike"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("branchNames = %v; want %v", got, want)
	}
}

// completeBranchArg past the first positional arg returns nothing and never
// falls back to file completion.
func TestCompleteBranchArgSecondArg(t *testing.T) {
	names, directive := completeBranchArg(nil, []string{"already"}, "")
	if names != nil {
		t.Errorf("names = %v; want nil for a second arg", names)
	}
	if directive != cobra.ShellCompDirectiveNoFileComp {
		t.Errorf("directive = %v; want NoFileComp", directive)
	}
}

// `bowt completion zsh` must emit a non-empty completion script that references
// bowt and cobra's dynamic-completion entrypoint.
func TestCompletionZshEmitsScript(t *testing.T) {
	out, err := execRoot(t, "completion", "zsh")
	if err != nil {
		t.Fatalf("completion zsh: %v", err)
	}
	if len(strings.TrimSpace(out)) == 0 {
		t.Fatal("completion zsh produced an empty script")
	}
	for _, want := range []string{"#compdef bowt", "__complete"} {
		if !strings.Contains(out, want) {
			t.Errorf("completion zsh missing %q", want)
		}
	}
}

func TestCompletionBashEmitsScript(t *testing.T) {
	out, err := execRoot(t, "completion", "bash")
	if err != nil {
		t.Fatalf("completion bash: %v", err)
	}
	if !strings.Contains(out, "__start_bowt") {
		t.Errorf("completion bash missing __start_bowt; got %d bytes", len(out))
	}
}

// Rich help renders for the root and a subcommand, including flags/examples.
func TestHelpRenders(t *testing.T) {
	out, err := execRoot(t, "--help")
	if err != nil {
		t.Fatalf("--help: %v", err)
	}
	for _, want := range []string{"bowt", "new", "ls", "path", "rm", "exec", "completion", "Available Commands"} {
		if !strings.Contains(out, want) {
			t.Errorf("root help missing %q", want)
		}
	}

	newOut, err := execRoot(t, "new", "--help")
	if err != nil {
		t.Fatalf("new --help: %v", err)
	}
	for _, want := range []string{"Create a git worktree", "--base", "Examples:", "bowt new feature/login"} {
		if !strings.Contains(newOut, want) {
			t.Errorf("new help missing %q", want)
		}
	}
}

// `bowt version` prints just the version string (the old behavior).
func TestVersionSubcommand(t *testing.T) {
	// version subcommand writes via fmt.Println to os.Stdout, so we can't
	// capture it through the cobra buffer; assert it at least runs cleanly.
	if _, err := execRoot(t, "version"); err != nil {
		t.Fatalf("version: %v", err)
	}
}
