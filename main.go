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
	"github.com/jfb1121/bowt/internal/gate"
	"github.com/jfb1121/bowt/internal/lock"
	"github.com/jfb1121/bowt/internal/output"
	"github.com/jfb1121/bowt/internal/repo"
	"github.com/jfb1121/bowt/internal/run"
	"github.com/jfb1121/bowt/internal/shell"
	"github.com/jfb1121/bowt/internal/spawn"
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
		newSpawnCmd(),
		newGateCmd(),
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

func newSpawnCmd() *cobra.Command {
	var opts spawnOpts
	c := &cobra.Command{
		Use:   "spawn [brief]",
		Short: "launch a headless coding agent against a brief",
		Long: `Hand a fresh headless agent a written brief instead of doing the work in your
own session. spawn loads a versioned wrapper prompt (plan by default, --impl for
implementation), substitutes the brief, stamps a provenance line, then runs the
agent as a child process while holding the per-worktree lock for its lifetime.

Brief resolution (first match, unless a path is passed):
  subagent/PROMPT.md  →  subagent/*-prompt.md  →  PROMPT.md
subagent/FOLLOWUP.md is auto-appended when present.`,
		Example: `  bowt spawn                       # plan pass over subagent/PROMPT.md
  bowt spawn --impl                # implementation pass
  bowt spawn brief.md --model opus # explicit brief + model
  bowt spawn --print-prompt        # assemble + print, don't launch`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				opts.brief = args[0]
			}
			return cmdSpawn(opts)
		},
	}
	c.Flags().BoolVar(&opts.impl, "impl", false, "implementation pass (default is a plan + writeback pass)")
	c.Flags().StringVar(&opts.model, "model", "", "agent model (alias opus/sonnet/haiku, or a full ID)")
	c.Flags().StringVar(&opts.effort, "effort", "", "agent reasoning effort (e.g. high)")
	c.Flags().BoolVar(&opts.printPrompt, "print-prompt", false, "assemble and print the prompt + provenance, then exit (no agent, no lock)")
	return c
}

func newGateCmd() *cobra.Command {
	var scope string
	c := &cobra.Command{
		Use:   "gate [--scope <full|paths>]",
		Short: "run the repo's verification hook and record a machine-readable verdict",
		Long: `Run the current worktree's gate hook under the exclusive per-worktree lock and
write a machine-readable verdict to <worktree>/.bowt/gate.json.

The repo owns what "gating" means via <configDir>/gate.sh (.bowt/ or .twig/): a
Go repo puts 'make check' there, a Django repo 'makemigrations --check'. The
hook reports per-check results by printing lines on stdout:

  BOWT_CHECK <name> <pass|fail> <exit_code> [detail...]

gate records the exact target (worktree path, commit SHA, branch, dirty flag)
so a reader can compare .commit to HEAD and detect a stale verdict. The verdict
is 'pass' only if the hook exits 0 AND every collected check passed; the process
exit code mirrors it (0 pass / 1 fail).`,
		Example: `  bowt gate
  bowt gate --scope paths
  jq -e '.overall=="pass"' .bowt/gate.json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := state.Open()
			if err != nil {
				return err
			}
			return cmdGate(st, scope)
		},
	}
	c.Flags().StringVar(&scope, "scope", "full", "verification scope recorded in the verdict: full, or a paths request the hook narrows to")
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

// spawnOpts carries the parsed flags for `bowt spawn`.
type spawnOpts struct {
	brief       string
	impl        bool
	model       string
	effort      string
	printPrompt bool
}

// runAgent is the seam the future agent-adapter slice slots into. For now it is
// hardcoded to `claude --dangerously-skip-permissions`; model/effort are passed
// as flags only when set. It is a package var so tests can substitute a fake,
// though the primary test path is --print-prompt (which never reaches here).
var runAgent = runClaude

// runClaude launches claude as a CHILD process with inherited stdio. Running it
// as a child (not syscall.Exec) is deliberate: Go opens the flock fd O_CLOEXEC,
// so an exec-replace would drop the per-worktree lock. As a child, the lock is
// held for the agent's whole lifetime and released cleanly when it returns.
func runClaude(prompt, model, effort string) error {
	args := []string{"--dangerously-skip-permissions"}
	if model != "" {
		args = append(args, "--model", model)
	}
	if effort != "" {
		args = append(args, "--effort", effort)
	}
	args = append(args, "--", prompt)

	c := exec.Command("claude", args...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := c.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			os.Exit(ee.ExitCode())
		}
		return fmt.Errorf("run claude: %w", err)
	}
	return nil
}

func cmdSpawn(opts spawnOpts) error {
	// Root the spawn at the current worktree (not the main repo): the brief,
	// the lock, and the writeback all belong to the checkout you stand in.
	top, err := repo.Toplevel("")
	if err != nil {
		return err
	}

	briefPath, brief, err := spawn.ResolveBrief(top, opts.brief)
	if err != nil {
		return err
	}

	mode := spawn.ModePlan
	if opts.impl {
		mode = spawn.ModeImpl
	}
	a, err := spawn.Assemble(mode, brief)
	if err != nil {
		return err
	}

	// Header + provenance are diagnostics (stderr): stdout is either the agent's
	// inherited stream or, under --print-prompt, the assembled prompt itself.
	output.Errf("spawn → %s", top)
	fmt.Fprintf(os.Stderr, "  brief: %s   mode: %s\n", briefPath, mode.Label())
	fmt.Fprintf(os.Stderr, "  %s\n", a.Provenance)

	model := spawn.ResolveModel(opts.model, opts.impl)
	effort := spawn.ResolveEffort(opts.effort, opts.impl)
	fmt.Fprintf(os.Stderr, "  model: %s   effort: %s\n", orDefault(model), orDefault(effort))

	if opts.printPrompt {
		// The assembled prompt (provenance already prepended) is the data here.
		fmt.Print(a.Prompt)
		return nil
	}

	// spawn is a writer: take the EXCLUSIVE per-worktree lock and hold it for the
	// agent's lifetime by running the agent as a child. defer releases on return.
	l, err := lock.Acquire(top)
	if err != nil {
		return err
	}
	defer func() { _ = l.Release() }()

	return runAgent(a.Prompt, model, effort)
}

// orDefault labels an empty model/effort as the agent's session default for the
// human-facing header.
func orDefault(s string) string {
	if s == "" {
		return "session-default"
	}
	return s
}

func cmdGate(st state.Store, scope string) error {
	// gate runs against the worktree you stand in (not the main repo): the lock,
	// the target, and the gate.json all belong to this checkout.
	top, err := repo.Toplevel("")
	if err != nil {
		return err
	}

	// EXCLUSIVE per-worktree lock, keyed on the worktree path (as spawn does),
	// held for the whole run. Fail fast with the busy message if another bowt
	// process holds it. The kernel also releases flock on process exit, so the
	// os.Exit below (mirroring the verdict) never leaks the lock.
	l, err := lock.Acquire(top)
	if err != nil {
		return err
	}
	defer func() { _ = l.Release() }()

	main, err := repo.MainRepo()
	if err != nil {
		return err
	}
	name := filepath.Base(main)
	branch := repo.CurrentBranch(top)

	commit, commitShort, err := repo.Head(top)
	if err != nil {
		return err
	}
	dirty, err := repo.Dirty(top)
	if err != nil {
		return err
	}

	// Build the per-worktree env (BOWT_*/GWT_* + config vars) the hook sees — the
	// same environment as exec/hooks. Port/offset come from the registry when the
	// worktree is registered; a broken config is a warning, not a hard failure.
	r := run.Exec{Stderr: os.Stderr}
	configDir := config.Dir(main)
	vars, err := config.Load(r, configDir)
	if err != nil {
		output.Errf("load config: %v — running gate without config env", err)
		vars = nil
	}
	info := env.Info{Path: top, Branch: branch, MainRepo: main, RepoName: name}
	if wt, ok, err := st.Get(name, branch); err == nil && ok {
		info.Offset, info.Port, info.CodeOnly = wt.Offset, wt.Port, wt.CodeOnly()
	}
	envKV := env.Build(info, vars)

	res, err := gate.Run(gate.Params{
		Runner:      r,
		ConfigDir:   configDir,
		Worktree:    top,
		Repo:        name,
		Branch:      branch,
		Commit:      commit,
		CommitShort: commitShort,
		Dirty:       dirty,
		Scope:       parseScope(scope),
		Env:         envKV,
	})
	if err != nil {
		return err
	}

	// The verdict is the data (stdout JSON); the process exit code mirrors it so
	// `bowt gate && …` works. Emit first, then exit non-zero on a fail verdict.
	if emitErr := output.Emit(res); emitErr != nil {
		return emitErr
	}
	if code := res.ExitCode(); code != 0 {
		os.Exit(code)
	}
	return nil
}

// parseScope maps the --scope flag onto the recorded scope. "full" (the
// default) records mode=full with no value; anything else is a paths request
// whose verbatim string the hook narrows to (exposed as BOWT_GATE_SCOPE_VALUE).
func parseScope(s string) gate.Scope {
	if s == "" || s == "full" {
		return gate.Scope{Mode: "full", Value: ""}
	}
	return gate.Scope{Mode: "paths", Value: s}
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
