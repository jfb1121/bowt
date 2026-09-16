package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/jfb1121/bowt/internal/config"
	"github.com/jfb1121/bowt/internal/env"
	"github.com/jfb1121/bowt/internal/lock"
	"github.com/jfb1121/bowt/internal/output"
	"github.com/jfb1121/bowt/internal/repo"
	"github.com/jfb1121/bowt/internal/run"
	"github.com/jfb1121/bowt/internal/state"
	"github.com/jfb1121/bowt/internal/worktree"
)

func newNewCmd() *cobra.Command {
	var base string
	var codeOnly bool
	c := &cobra.Command{
		Use:   "new <branch>",
		Short: "create a worktree and register it",
		Long: `Create a git worktree for <branch> (from base, defaulting to the main repo's
current branch), register it, allocate a port/offset, and run the setup hooks.

--code-only registers a lightweight worktree: BOWT_CODE_ONLY=1 (and the
GWT_CODE_ONLY alias) is exported to the hooks and to 'bowt exec', so a repo's
setup.sh/teardown.sh can skip the heavy per-worktree provisioning. Without it a
worktree is 'full'.`,
		Example: `  bowt new feature/login
  bowt new hotfix -b release/2.0
  bowt new docs --code-only`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := state.Open()
			if err != nil {
				return err
			}
			return cmdNew(st, args[0], base, codeOnly)
		},
	}
	c.Flags().StringVarP(&base, "base", "b", "", "base branch (default: main repo's current branch)")
	c.Flags().BoolVar(&codeOnly, "code-only", false, "lightweight worktree: export BOWT_CODE_ONLY=1 so hooks skip heavy provisioning")
	return c
}

func newLsCmd() *cobra.Command {
	var asJSON bool
	c := &cobra.Command{
		Use:   "ls",
		Short: "list registered worktrees",
		Long: `List the worktrees registered for the current repo.

A human table is printed at an interactive terminal; JSON is emitted when stdout
is not a terminal (agent-first) or when --json is given.`,
		Example: `  bowt ls
  bowt ls --json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := state.Open()
			if err != nil {
				return err
			}
			return cmdLs(st, asJSON)
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "force JSON output")
	return c
}

func newPathCmd() *cobra.Command {
	c := &cobra.Command{
		Use:               "path <branch>",
		Short:             "print a worktree's path",
		Long:              "Print the on-disk path of the worktree registered for <branch>.",
		Example:           "  bowt path feature/login",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeBranchArg,
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := state.Open()
			if err != nil {
				return err
			}
			return cmdPath(st, args[0])
		},
	}
	return c
}

func newRmCmd() *cobra.Command {
	c := &cobra.Command{
		Use:               "rm <branch>",
		Short:             "remove and deregister a worktree",
		Long:              "Run the teardown hook, remove the git worktree, and deregister it.",
		Example:           "  bowt rm feature/login",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeBranchArg,
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := state.Open()
			if err != nil {
				return err
			}
			return cmdRm(st, args[0])
		},
	}
	return c
}

func newExecCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "exec <branch> [--] <cmd> [args...]",
		Short: "run a command inside a worktree",
		Long: `Run <cmd> inside the worktree for <branch>, with the per-worktree environment
(BOWT_*/GWT_* + config vars) injected — the same environment the hooks see.

Use -- to separate bowt from a command that has its own flags:
  bowt exec feature -- pytest -x`,
		Example: `  bowt exec feature/login npm test
  bowt exec feature -- pytest -x`,
		// Pass the command line through untouched (flags belong to the child
		// command, not to bowt) — matching the original hand-rolled dispatch.
		DisableFlagParsing: true,
		ValidArgsFunction:  completeBranchArg,
		RunE: func(cmd *cobra.Command, args []string) error {
			// DisableFlagParsing means -h/--help reach us raw; honor them so
			// `bowt exec --help` still renders (the old dispatch errored here).
			if len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
				return cmd.Help()
			}
			st, err := state.Open()
			if err != nil {
				return err
			}
			return cmdExec(st, args)
		},
	}
	return c
}

func cmdNew(st state.Store, branch, base string, codeOnly bool) error {
	// Writers take the per-worktree lock so two bowt processes can't race the
	// same tree. defer releases it as soon as this command returns.
	l, err := lock.Acquire(branch)
	if err != nil {
		return err
	}
	defer func() { _ = l.Release() }()

	// Hooks stream their stderr live so a slow setup.sh shows progress;
	// stdout stays reserved for bowt's JSON result.
	r := run.Exec{Stderr: os.Stderr}
	wt, err := worktree.New(st, r, branch, base, codeOnly)
	if err != nil {
		return err
	}
	return output.Emit(wt)
}

func cmdLs(st state.Store, asJSON bool) error {
	wts, err := worktree.List(st)
	if err != nil {
		return err
	}
	// A nil slice marshals to JSON `null`; agents expect an array. Coerce to [].
	if wts == nil {
		wts = []state.Worktree{}
	}
	// Agent-first: JSON unless a human is at the terminal (or --json forces it).
	if asJSON || !output.IsTTY() {
		return output.Emit(wts)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "BRANCH\tPORT\tOFFSET\tMODE\tPATH")
	for _, wt := range wts {
		mode := wt.Mode
		if mode == "" {
			mode = state.ModeFull
		}
		fmt.Fprintf(w, "%s\t%d\t%d\t%s\t%s\n", wt.Branch, wt.Port, wt.Offset, mode, wt.Path)
	}
	return w.Flush()
}

func cmdPath(st state.Store, branch string) error {
	p, err := worktree.Path(st, branch)
	if err != nil {
		return err
	}
	fmt.Println(p)
	return nil
}

func cmdRm(st state.Store, branch string) error {
	l, err := lock.Acquire(branch)
	if err != nil {
		return err
	}
	defer func() { _ = l.Release() }()

	r := run.Exec{Stderr: os.Stderr}
	if err := worktree.Remove(st, r, branch); err != nil {
		return err
	}
	// A declared shape (even anonymous) beats an ad-hoc map for agent-facing JSON.
	return output.Emit(struct {
		Removed string `json:"removed"`
	}{Removed: branch})
}

func cmdExec(st state.Store, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: bowt exec <branch> [--] <cmd> [args...]")
	}
	branch, rest := args[0], args[1:]
	if len(rest) > 0 && rest[0] == "--" {
		rest = rest[1:]
	}
	if len(rest) == 0 {
		return fmt.Errorf("usage: bowt exec <branch> [--] <cmd> [args...]")
	}

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

	// Inject the per-worktree env (BOWT_*/GWT_* + config vars) so the command
	// sees the same environment as the hooks. A broken config is a warning, not
	// a hard failure — exec stays usable.
	vars, err := config.Load(run.Exec{Stderr: os.Stderr}, config.Dir(main))
	if err != nil {
		output.Errf("load config: %v — running without config env", err)
		vars = nil
	}
	envKV := env.Build(env.Info{
		Path:     wt.Path,
		Branch:   branch,
		Offset:   wt.Offset,
		Port:     wt.Port,
		MainRepo: main,
		RepoName: name,
		CodeOnly: wt.CodeOnly(),
	}, vars)

	c := exec.Command(rest[0], rest[1:]...)
	c.Dir = wt.Path
	c.Env = append(os.Environ(), envKV...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := c.Run(); err != nil {
		// Propagate the child's exit code rather than masking it as our own.
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			os.Exit(ee.ExitCode())
		}
		return err
	}
	return nil
}
