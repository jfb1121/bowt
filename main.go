// Command bowt manages git worktrees with isolated dev environments.
//
// Agent-first: commands emit structured JSON by default whenever stdout is not
// a terminal, so agents parse output instead of scraping human tables.
//
// The CLI is built on cobra so every command gets real --help, usage, and
// shell completion (see `bowt completion`) — but the command logic still lives
// in the cmd* functions below, called from each command's RunE.
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
	"github.com/jfb1121/bowt/internal/shell"
	"github.com/jfb1121/bowt/internal/state"
	"github.com/jfb1121/bowt/internal/worktree"
)

// version is set at build time via -ldflags "-X main.version=…" (see Makefile).
var version = "dev"

func main() {
	if err := newRootCmd().Execute(); err != nil {
		// Preserve bowt's error contract: "bowt: <err>" on stderr, exit 1.
		// Usage/errors are silenced on the commands so cobra doesn't also print
		// its own "Error:" line or dump usage on a runtime failure.
		output.Errf("%v", err)
		os.Exit(1)
	}
}

// newRootCmd builds the full command tree. It's a constructor (not a package
// var) so tests can build a fresh, isolated tree per case.
func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "bowt",
		Short: "git worktrees with isolated environments",
		Long: `bowt — git worktrees with isolated environments.

Every branch gets its own worktree with an allocated port/offset. Commands emit
JSON by default whenever stdout is not a terminal, so agents parse structured
output instead of scraping tables.`,
		Version: version,
		// Own our error/usage output (see main): print "bowt: <err>" ourselves.
		SilenceUsage:  true,
		SilenceErrors: true,
		// No bash-completion command noise for a private tool; we ship our own.
		CompletionOptions: cobra.CompletionOptions{DisableDefaultCmd: true},
	}
	// `bowt --version` prints just the version string (matches the old behavior),
	// not cobra's default "bowt version X.Y".
	root.SetVersionTemplate("{{.Version}}\n")

	root.AddCommand(
		newNewCmd(),
		newLsCmd(),
		newPathCmd(),
		newRmCmd(),
		newExecCmd(),
		newRootPathCmd(),
		newCdCmd(),
		newShellInitCmd(),
		newVersionCmd(),
		newCompletionCmd(),
	)
	return root
}

func newNewCmd() *cobra.Command {
	var base string
	c := &cobra.Command{
		Use:   "new <branch>",
		Short: "create a worktree and register it",
		Long: `Create a git worktree for <branch> (from base, defaulting to the main repo's
current branch), register it, allocate a port/offset, and run the setup hooks.`,
		Example: `  bowt new feature/login
  bowt new hotfix -b release/2.0`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := state.Open()
			if err != nil {
				return err
			}
			return cmdNew(st, args[0], base)
		},
	}
	c.Flags().StringVarP(&base, "base", "b", "", "base branch (default: main repo's current branch)")
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

func newRootPathCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "root",
		Short:   "print the main repo path",
		Long:    "Print the absolute path of the main repository root (works from inside a worktree).",
		Example: "  bowt root",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmdRoot()
		},
	}
}

func newCdCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "cd <branch>|main",
		Short: "change directory into a worktree (needs shell integration)",
		Long: `Change the caller's directory into a worktree.

Changing the parent shell's cwd is the one thing a child process cannot do, so
cd requires the shell integration:

  eval "$(bowt shell-init zsh)"`,
		Example:           "  bowt cd feature/login",
		ValidArgsFunction: completeBranchArg,
		RunE: func(cmd *cobra.Command, args []string) error {
			return fmt.Errorf(`cd needs the shell integration — run: eval "$(bowt shell-init zsh)"`)
		},
	}
}

func newShellInitCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "shell-init [bash|zsh]",
		Short: "print shell integration to eval",
		Long: `Print the shell integration snippet to eval, e.g.:

  eval "$(bowt shell-init zsh)"

This installs a bowt() wrapper so 'bowt cd' can change your shell's directory,
and loads bowt's shell completion for the chosen shell.`,
		Example: `  eval "$(bowt shell-init zsh)"
  eval "$(bowt shell-init bash)"`,
		Args:      cobra.MaximumNArgs(1),
		ValidArgs: []string{"bash", "zsh"},
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmdShellInit(args)
		},
	}
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "version",
		Short:   "print build version",
		Example: "  bowt version",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Println(version)
			return nil
		},
	}
}

func newCompletionCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "completion [bash|zsh|fish|powershell]",
		Short: "generate a shell completion script",
		Long: `Generate a shell completion script for bowt.

Usually you don't run this directly — 'bowt shell-init' sources it for you. To
load it manually for the current session:

  source <(bowt completion zsh)     # zsh
  source <(bowt completion bash)    # bash`,
		Example: `  bowt completion zsh
  source <(bowt completion bash)`,
		Args:      cobra.ExactArgs(1),
		ValidArgs: []string{"bash", "zsh", "fish", "powershell"},
		RunE: func(cmd *cobra.Command, args []string) error {
			root := cmd.Root()
			out := cmd.OutOrStdout()
			switch args[0] {
			case "bash":
				return root.GenBashCompletionV2(out, true)
			case "zsh":
				return root.GenZshCompletion(out)
			case "fish":
				return root.GenFishCompletion(out, true)
			case "powershell":
				return root.GenPowerShellCompletionWithDesc(out)
			default:
				return fmt.Errorf("unsupported shell %q (want bash, zsh, fish, or powershell)", args[0])
			}
		},
	}
	return c
}

func cmdNew(st state.Store, branch, base string) error {
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
	wt, err := worktree.New(st, r, branch, base)
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
	fmt.Fprintln(w, "BRANCH\tPORT\tOFFSET\tPATH")
	for _, wt := range wts {
		fmt.Fprintf(w, "%s\t%d\t%d\t%s\n", wt.Branch, wt.Port, wt.Offset, wt.Path)
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

func cmdRoot() error {
	main, err := repo.MainRepo()
	if err != nil {
		return err
	}
	fmt.Println(main)
	return nil
}

func cmdShellInit(args []string) error {
	sh := "zsh"
	if len(args) > 0 {
		sh = args[0]
	}
	out, err := shell.Init(sh)
	if err != nil {
		return err
	}
	fmt.Print(out)
	return nil
}
