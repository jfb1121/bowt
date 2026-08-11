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
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/jfb1121/bowt/internal/agent"
	"github.com/jfb1121/bowt/internal/config"
	"github.com/jfb1121/bowt/internal/env"
	"github.com/jfb1121/bowt/internal/extension"
	"github.com/jfb1121/bowt/internal/gate"
	"github.com/jfb1121/bowt/internal/lock"
	"github.com/jfb1121/bowt/internal/output"
	"github.com/jfb1121/bowt/internal/repo"
	"github.com/jfb1121/bowt/internal/review"
	"github.com/jfb1121/bowt/internal/run"
	"github.com/jfb1121/bowt/internal/shell"
	"github.com/jfb1121/bowt/internal/spawn"
	"github.com/jfb1121/bowt/internal/state"
	"github.com/jfb1121/bowt/internal/worktree"
)

// version is set at build time via -ldflags "-X main.version=…" (see Makefile).
var version = "dev"

func main() {
	root := newRootCmd()
	// Git-style dispatch: if the first word is not a built-in command but the
	// repo ships a matching extension, run it and exit with its code. Built-ins
	// always win; an unknown word with no extension falls through to cobra's
	// normal unknown-command error below.
	if code, handled := tryExtension(root, os.Args[1:]); handled {
		os.Exit(code)
	}
	if err := root.Execute(); err != nil {
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
		newLandCmd(),
		newReviewCmd(),
		newDoctorCmd(),
		newRootPathCmd(),
		newCdCmd(),
		newShellInitCmd(),
		newExtensionsCmd(),
		newLibCmd(),
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
	c.Flags().StringVar(&opts.agent, "agent", "", "agent provider (claude, codex; default: $BOWT_AGENT/$GWT_AGENT or claude)")
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

func newLandCmd() *cobra.Command {
	var opts landOpts
	c := &cobra.Command{
		Use:   "land <branch> [--base <ref>] [--no-push] [--keep] [--no-gate]",
		Short: "gate a branch, fast-forward it onto its base, and clean up",
		Long: `Land a branch the safe way: gate it, fast-forward-merge it onto its base, push,
and remove its worktree — refusing at every step rather than forcing.

land resolves <branch>'s registered worktree and its base (default: the main
repo's current branch / main), then, holding the worktree lock:

  1. runs the repo's gate hook in the worktree and requires overall=pass for
     exactly the branch HEAD (never a stale verdict). No gate.sh is a hard error
     unless --no-gate (which warns loudly and proceeds).
  2. refuses if the worktree is dirty, or if the branch does not fast-forward
     onto base ("rebase first") — the check that stops half-merges.
  3. fast-forwards base to the branch HEAD (git merge --ff-only) and pushes
     (unless --no-push); any conflict aborts cleanly, never a partial state.
  4. removes the worktree and deletes the merged local branch (unless --keep).

The result is JSON: {landed, branch, base, commit, gate_verdict, pushed,
cleaned}. Any refusal exits non-zero. The remote branch is left untouched.`,
		Example: `  bowt land feature/login
  bowt land hotfix --base release/2.0
  bowt land docs --no-push --keep`,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeBranchArg,
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := state.Open()
			if err != nil {
				return err
			}
			return cmdLand(st, args[0], opts)
		},
	}
	c.Flags().StringVar(&opts.base, "base", "", "base branch to land onto (default: main repo's current branch)")
	c.Flags().BoolVar(&opts.noPush, "no-push", false, "land locally only; do not push the base branch")
	c.Flags().BoolVar(&opts.keep, "keep", false, "keep the worktree and local branch after landing")
	c.Flags().BoolVar(&opts.noGate, "no-gate", false, "skip the gate (warns loudly); required when the repo has no gate hook")
	return c
}

func newReviewCmd() *cobra.Command {
	var opts reviewOpts
	var perspectives []string
	c := &cobra.Command{
		Use:   "review [-p slugs] [--all] [--base ref]",
		Short: "perspective code review of the current diff",
		Long: `Review the current worktree's diff — merge-base(base, HEAD) → working tree, so
local commits AND uncommitted changes are covered — from several independent
perspectives, then refuse to present any report whose VERDICT lies about what
its body contains.

Each selected perspective (a prompt file with 'layers:' front-matter under
<configDir>/review-perspectives/) is run through the agent's one-shot mode,
bounded by max_parallel. Reports land in .bowt-review/<slug>.md; a report whose
verdict/body counts disagree, or a clean 0/0/0 with no evidence of work, is
rejected to <slug>.rejected. Usable reports are aggregated into SUMMARY.md and
synthesized into a decision queue in SYNTHESIS.md. If 0 of N perspectives
produce a usable report, the command exits non-zero — it never reports success
having reviewed nothing.`,
		Example: `  bowt review --all
  bowt review -p arch-reuse,security-tenancy
  bowt review --base origin/main --no-synthesize`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.perspectives = perspectives
			st, err := state.Open()
			if err != nil {
				return err
			}
			return cmdReview(st, opts)
		},
	}
	c.Flags().StringSliceVarP(&perspectives, "perspectives", "p", nil, "explicit perspective slugs (comma-separated); default is all")
	c.Flags().BoolVar(&opts.all, "all", false, "run every discovered perspective (the default when -p is omitted)")
	c.Flags().StringVar(&opts.base, "base", "", "diff base ref (default: the branch's upstream, else origin/main)")
	c.Flags().StringVar(&opts.agent, "agent", "", "agent provider (must support one-shot; default: $BOWT_AGENT/$GWT_AGENT or claude)")
	c.Flags().StringVar(&opts.model, "model", "", "model recorded in the report header (per-call model is not yet plumbed through Oneshot — see STATUS)")
	c.Flags().BoolVar(&opts.noSynthesize, "no-synthesize", false, "skip the synthesis/decision-queue stage")
	return c
}

func newDoctorCmd() *cobra.Command {
	var agentName string
	var dryRun bool
	c := &cobra.Command{
		Use:   "doctor --agent <name>",
		Short: "smoke-check an agent provider",
		Long: `Smoke-check a provider descriptor: its binary is on PATH, its config dir is
resolvable, and — when the provider supports one-shot — a trivial prompt
round-trips through it. Emits a machine-readable report; exits non-zero if any
check fails.

(Only the --agent path exists in this slice; a fuller doctor covering deps,
ports, and the registry is a later slice.)`,
		Example: `  bowt doctor --agent claude
  bowt doctor --agent codex
  bowt doctor --agent claude --dry-run   # skip the one-shot launch`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if agentName == "" {
				return fmt.Errorf("doctor currently supports only --agent <name>")
			}
			ag, err := agent.New(agentName)
			if err != nil {
				return err
			}
			rep := doctorAgent(ag, exec.LookPath, os.Getenv("HOME"), dryRun)
			if emitErr := output.Emit(rep); emitErr != nil {
				return emitErr
			}
			if !rep.OK {
				os.Exit(1)
			}
			return nil
		},
	}
	c.Flags().StringVar(&agentName, "agent", "", "agent provider to smoke-check (claude, codex)")
	c.Flags().BoolVar(&dryRun, "dry-run", false, "skip the one-shot round-trip (check bin + config only)")
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

func newExtensionsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "extensions",
		Short: "list the repo's per-repo extensions",
		Long: `List the extensions this repo ships under <configDir>/extensions/*.sh, with
each one's description and lock mode.

An extension is a bash script the repo drops in to add a 'bowt <cmd>' without
patching bowt. When <cmd> is not a built-in and a matching script exists, bowt
runs it as a subprocess with the per-worktree BOWT_*/GWT_* environment injected
and BOWT_LIB pointing at the helper (see 'bowt lib').

A leading comment header configures it:
  # bowt-lock: none|shared|exclusive   (bowt takes the lock around the run)
  # bowt-desc: <one-line description>`,
		Example: `  bowt extensions
  bowt extensions --json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmdExtensions()
		},
	}
}

func newLibCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "lib",
		Short: "print the sourceable bash helper for extensions",
		Long: `Print bowt.lib.sh, the bash helper an extension sources via "$BOWT_LIB". bowt
exports BOWT_LIB into every extension pointing at this content, so a script can:

  source "$BOWT_LIB"
  bowt_log "starting"

It defines bowt_log / bowt_err (both to stderr). The BOWT_*/GWT_* environment is
already exported into the script, so the helper stays thin.`,
		Example: "  bowt lib",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// WriteString (not fmt.Print) since Lib legitimately contains %s.
			_, err := os.Stdout.WriteString(extension.Lib)
			return err
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
	agent       string
	model       string
	effort      string
	printPrompt bool
}

func cmdSpawn(opts spawnOpts) error {
	// Root the spawn at the current worktree (not the main repo): the brief,
	// the lock, and the writeback all belong to the checkout you stand in.
	top, err := repo.Toplevel("")
	if err != nil {
		return err
	}

	// Select the provider up front (flag → $BOWT_AGENT/$GWT_AGENT → claude); an
	// unknown agent is a hard error before any work.
	ag, err := agent.Select(opts.agent, os.Getenv)
	if err != nil {
		return err
	}
	caps := ag.Caps()

	briefPath, brief, err := spawn.ResolveBrief(top, opts.brief)
	if err != nil {
		return err
	}

	mode := spawn.ModePlan
	if opts.impl {
		mode = spawn.ModeImpl
	}
	// The provider's memory file fills {{MEMORY_FILE}}; its name is stamped into
	// the provenance line so a reader knows which CLI produced the writeback.
	a, err := spawn.Assemble(mode, caps.Name, caps.MemoryFile, brief)
	if err != nil {
		return err
	}

	// Header + provenance are diagnostics (stderr): stdout is either the agent's
	// inherited stream or, under --print-prompt, the assembled prompt itself.
	output.Errf("spawn → %s", top)
	fmt.Fprintf(os.Stderr, "  brief: %s   mode: %s   agent: %s\n", briefPath, mode.Label(), caps.Name)
	fmt.Fprintf(os.Stderr, "  %s\n", a.Provenance)
	if !caps.SupportsHooks {
		// A downgrade the RFC says to surface at spawn, not discover later: this
		// lane runs without Edit/Write guardrail enforcement.
		output.Errf("warning: agent %q has no hook support — this lane runs without Edit/Write guardrails", caps.Name)
	}

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

	// Run the agent as a child with inherited stdio. On a non-zero exit, mirror
	// the child's exit code (matching the prior hardcoded path) rather than
	// masking it as bowt's own error.
	sopts := agent.Opts{Model: model, Effort: effort, Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr}
	if err := ag.Session(context.Background(), a.Prompt, sopts); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			os.Exit(ee.ExitCode())
		}
		return err
	}
	return nil
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

// landOpts carries the parsed flags for `bowt land`.
type landOpts struct {
	base   string
	noPush bool
	keep   bool
	noGate bool
}

// landResult is the agent-facing JSON `bowt land` emits on a successful land.
// A declared shape (house style) beats an ad-hoc map for structured output.
type landResult struct {
	Landed      bool   `json:"landed"`
	Branch      string `json:"branch"`
	Base        string `json:"base"`
	Commit      string `json:"commit"`
	GateVerdict string `json:"gate_verdict"`
	Pushed      bool   `json:"pushed"`
	Cleaned     bool   `json:"cleaned"`
}

// gateSkipped marks a land that bypassed verification via --no-gate; any other
// verdict value is a real gate.Status ("pass").
const gateSkipped = "skipped"

// cmdLand gates a branch, fast-forwards it onto its base, pushes, and cleans up
// — refusing (never forcing) if the branch is ungated, dirty, or not a
// fast-forward. It encodes "never merge an ungated lane" as a verb.
func cmdLand(st state.Store, branch string, opts landOpts) error {
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
		return fmt.Errorf("no worktree registered for %q — nothing to land", branch)
	}

	base := opts.base
	if base == "" {
		base = repo.DefaultBase(main)
	}
	if base == branch {
		return fmt.Errorf("refusing to land %q onto itself (base == branch)", branch)
	}

	// Exclusive lock on the worktree path — the same key `gate` uses — so land
	// and gate can't race the same checkout. The kernel also drops flock on exit,
	// so any os.Exit downstream never leaks it.
	l, err := lock.Acquire(wt.Path)
	if err != nil {
		return err
	}
	defer func() { _ = l.Release() }()

	// The branch HEAD we intend to land; everything downstream is checked against
	// it, so a verdict or FF check for a different commit is caught.
	branchHead, branchShort, err := repo.Head(wt.Path)
	if err != nil {
		return err
	}

	// Precondition: the worktree must be clean — never land uncommitted work.
	dirty, err := repo.Dirty(wt.Path)
	if err != nil {
		return err
	}
	if dirty {
		return fmt.Errorf("refusing to land %q: worktree %s has uncommitted changes — commit or discard first", branch, wt.Path)
	}

	// Gate: run the repo's verification hook in the branch's worktree and land
	// only on a fresh pass for exactly this commit.
	r := run.Exec{Stderr: os.Stderr}
	configDir := config.Dir(main)
	hook := ""
	if configDir != "" {
		hook = filepath.Join(configDir, gate.HookFile)
		if _, statErr := os.Stat(hook); statErr != nil {
			hook = ""
		}
	}

	verdict := gateSkipped
	switch {
	case hook == "" && !opts.noGate:
		return fmt.Errorf("no gate hook (%s) in this repo: add one to define what verification means, or pass --no-gate to land without it",
			filepath.Join(main, ".bowt", gate.HookFile))
	case opts.noGate:
		output.Errf("WARNING: --no-gate: landing %q onto %s WITHOUT verification — you are bypassing the gate", branch, base)
	default:
		vars, cfgErr := config.Load(r, configDir)
		if cfgErr != nil {
			output.Errf("load config: %v — running gate without config env", cfgErr)
			vars = nil
		}
		info := env.Info{Path: wt.Path, Branch: branch, MainRepo: main, RepoName: name,
			Offset: wt.Offset, Port: wt.Port, CodeOnly: wt.CodeOnly()}
		res, gErr := gate.Run(gate.Params{
			Runner:      r,
			ConfigDir:   configDir,
			Worktree:    wt.Path,
			Repo:        name,
			Branch:      branch,
			Commit:      branchHead,
			CommitShort: branchShort,
			Dirty:       dirty,
			Scope:       gate.Scope{Mode: "full"},
			Env:         env.Build(info, vars),
		})
		if gErr != nil {
			return gErr
		}
		if res.Overall != gate.Pass {
			return fmt.Errorf("refusing to land %q: gate did not pass (overall=%s) — see %s",
				branch, res.Overall, filepath.Join(wt.Path, gate.OutputDir, gate.OutputFile))
		}
		// Never land on a stale verdict: the pass must be for the exact HEAD.
		if res.Commit != branchHead {
			return fmt.Errorf("refusing to land %q: gate verdict is stale (verdict %s != HEAD %s) — re-gate", branch, res.CommitShort, branchShort)
		}
		verdict = string(res.Overall)
	}

	// Precondition: the branch must fast-forward onto base (base is an ancestor of
	// the branch HEAD). This is the check that stops half-merges.
	baseHead, err := repo.RevParse(main, base)
	if err != nil {
		return fmt.Errorf("resolve base %q: %w", base, err)
	}
	ff, err := repo.IsAncestor(main, baseHead, branchHead)
	if err != nil {
		return err
	}
	if !ff {
		return fmt.Errorf("refusing to land %q: not a fast-forward onto %s (base has commits %q lacks) — rebase onto %s first", branch, base, branch, base)
	}

	// Land: advance base to the branch HEAD, fast-forward only. When base is the
	// main repo's checkout we merge in its working tree; otherwise we move the ref
	// directly (guarded on its old value). Either advances cleanly or git refuses
	// — never a partial state.
	if base == repo.CurrentBranch(main) {
		if err := repo.MergeFFOnly(main, branchHead); err != nil {
			return fmt.Errorf("land %q onto %s: %w", branch, base, err)
		}
	} else {
		if err := repo.UpdateRef(main, "refs/heads/"+base, branchHead, baseHead); err != nil {
			return fmt.Errorf("land %q onto %s: %w", branch, base, err)
		}
	}

	// From here the local land has happened; report truthfully even if a later
	// step (push, cleanup) fails, then surface the failure as a non-zero exit.
	result := landResult{Landed: true, Branch: branch, Base: base, Commit: branchHead, GateVerdict: verdict}

	if !opts.noPush {
		if err := repo.Push(main, "origin", base); err != nil {
			_ = output.Emit(result)
			return fmt.Errorf("landed %q onto %s locally, but push failed: %w", branch, base, err)
		}
		result.Pushed = true
	}

	if !opts.keep {
		if err := worktree.Remove(st, r, branch); err != nil {
			_ = output.Emit(result)
			return fmt.Errorf("landed %q, but worktree cleanup failed: %w", branch, err)
		}
		if err := repo.DeleteBranch(main, branch); err != nil {
			_ = output.Emit(result)
			return fmt.Errorf("landed %q and removed its worktree, but deleting local branch failed: %w", branch, err)
		}
		result.Cleaned = true
	}

	return output.Emit(result)
}

// reviewOpts carries the parsed flags for `bowt review`.
type reviewOpts struct {
	perspectives []string
	all          bool
	base         string
	agent        string
	model        string
	noSynthesize bool
}

func cmdReview(st state.Store, opts reviewOpts) error {
	// The review runs against the worktree you stand in: the diff, the lock, and
	// the .bowt-review output all belong to this checkout.
	top, err := repo.Toplevel("")
	if err != nil {
		return err
	}
	main, err := repo.MainRepo()
	if err != nil {
		return err
	}
	branch := repo.CurrentBranch(top)
	if branch == "" {
		return fmt.Errorf("detached HEAD — check out a branch to review")
	}

	// Select the provider up front and hard-error BEFORE any work if it cannot
	// run one-shot (this whole pipeline is stdin→stdout fan-out).
	ag, err := agent.Select(opts.agent, os.Getenv)
	if err != nil {
		return err
	}
	if err := agent.RequireOneshot(ag); err != nil {
		return err
	}

	// Perspectives live in the repo's config dir.
	configDir := config.Dir(main)
	if configDir == "" {
		return fmt.Errorf("no config dir (.bowt/ or .twig/) in %s — cannot find review perspectives", main)
	}
	perspectivesDir := filepath.Join(configDir, review.PerspectivesSubdir)
	all, err := review.Discover(perspectivesDir)
	if err != nil {
		return err
	}
	selected, err := review.Select(all, opts.perspectives)
	if err != nil {
		return err
	}
	if len(selected) == 0 {
		return fmt.Errorf("no perspectives selected (found %d in %s)", len(all), perspectivesDir)
	}

	// Diff scope + preflight facts (in-place: merge-base(base, HEAD) → worktree).
	base := opts.base
	if base == "" {
		base = repo.UpstreamOrDefault(top)
	}
	facts, err := repo.ReviewScope(top, base)
	if err != nil {
		return err
	}
	if len(facts.ChangedFiles) == 0 {
		output.Errf("no diff against %s — nothing to review", base)
		return nil
	}

	pf := review.Preflight{
		BinaryAdded:     facts.BinaryAdded,
		NewFiles:        facts.NewFiles,
		UntrackedSource: facts.UntrackedSource,
		AddedSymbols:    facts.AddedSymbols,
	}
	jobs := make([]review.Job, len(selected))
	for i, p := range selected {
		jobs[i] = review.Job{Slug: p.Slug, Prompt: review.PerspectivePrompt(p, pf, facts.DiffCmd)}
	}

	modelLabel := opts.model
	if modelLabel == "" {
		modelLabel = ag.Caps().Name + "-default"
	}
	output.Errf("in-place review of %s vs %s — %d perspective(s), agent %s", branch, base, len(jobs), ag.Caps().Name)

	// EXCLUSIVE per-worktree lock: review WRITES the tree (.bowt-review/). Held
	// for the whole run; the kernel releases flock on exit so os.Exit never leaks it.
	l, err := lock.Acquire(top)
	if err != nil {
		return err
	}
	defer func() { _ = l.Release() }()

	r := &review.Runner{
		ReviewDir:   filepath.Join(top, review.ReviewDirName),
		MaxParallel: reviewParallel(os.Getenv),
		Oneshot:     ag.Oneshot,
		Synthesize:  !opts.noSynthesize,
		Branch:      branch,
		Base:        base,
		Mode:        "in-place",
		Model:       modelLabel,
		SynthPrompt: func(paths []string) string { return review.SynthesisPrompt(branch, base, paths) },
		Log:         func(format string, args ...any) { fmt.Fprintf(os.Stderr, format+"\n", args...) },
	}
	rep, err := r.Run(context.Background(), jobs)
	if err != nil {
		return err
	}
	if emitErr := output.Emit(rep); emitErr != nil {
		return emitErr
	}
	if rep.RunFailed {
		os.Exit(1)
	}
	return nil
}

// reviewParallel reads the fan-out bound from BOWT_REVIEW_PARALLEL (preferred)
// or GWT_REVIEW_PARALLEL (back-compat), falling back to the default.
func reviewParallel(getenv func(string) string) int {
	for _, k := range []string{"BOWT_REVIEW_PARALLEL", "GWT_REVIEW_PARALLEL"} {
		if s := strings.TrimSpace(getenv(k)); s != "" {
			if n, err := strconv.Atoi(s); err == nil && n > 0 {
				return n
			}
		}
	}
	return review.DefaultMaxParallel
}

// doctorCheck is one smoke-check outcome in the agent report.
type doctorCheck struct {
	Name   string `json:"name"`
	Pass   bool   `json:"pass"`
	Detail string `json:"detail,omitempty"`
}

// doctorReport is the agent-facing result of `bowt doctor --agent`.
type doctorReport struct {
	Agent  string        `json:"agent"`
	Checks []doctorCheck `json:"checks"`
	OK     bool          `json:"ok"`
}

// doctorAgent runs the provider smoke-checks. lookPath and home are injected so
// tests exercise it without a real PATH or HOME; dryRun skips the one-shot
// launch so the check is safe to run without launching the CLI. When the
// provider has no one-shot mode, that step is a pass (nothing to check), not a
// failure — SupportsOneshot=false is a legitimate provider shape.
func doctorAgent(ag agent.Agent, lookPath func(string) (string, error), home string, dryRun bool) doctorReport {
	caps := ag.Caps()
	rep := doctorReport{Agent: caps.Name, OK: true}
	add := func(name string, pass bool, detail string) {
		rep.Checks = append(rep.Checks, doctorCheck{Name: name, Pass: pass, Detail: detail})
		if !pass {
			rep.OK = false
		}
	}

	// bin on PATH
	if p, err := lookPath(caps.Bin); err == nil {
		add("bin-on-path", true, p)
	} else {
		add("bin-on-path", false, fmt.Sprintf("%s not found on PATH", caps.Bin))
	}

	// config dir resolvable (path computable from a known HOME; existence is a
	// detail, not a failure — a fresh machine may not have run the agent yet).
	switch {
	case caps.ConfigDir == "":
		add("config-dir", false, "provider declares no config dir")
	case home == "":
		add("config-dir", false, "HOME unset; cannot resolve "+caps.ConfigDir)
	default:
		dir := filepath.Join(home, caps.ConfigDir)
		if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
			add("config-dir", true, dir)
		} else {
			add("config-dir", true, dir+" (absent)")
		}
	}

	// one-shot round-trip (only when supported and not dry-run)
	switch {
	case !caps.SupportsOneshot:
		add("oneshot", true, "not supported by this provider (skipped)")
	case dryRun:
		add("oneshot", true, "skipped (--dry-run)")
	default:
		out, err := ag.Oneshot(context.Background(), "Reply with the single word: ok")
		if err != nil {
			add("oneshot", false, err.Error())
		} else {
			add("oneshot", true, fmt.Sprintf("round-trip ok (%d bytes)", len(strings.TrimSpace(out))))
		}
	}
	return rep
}

func cmdRoot() error {
	main, err := repo.MainRepo()
	if err != nil {
		return err
	}
	fmt.Println(main)
	return nil
}

// tryExtension implements the git-style dispatch fallback. It returns
// handled=true only when args name a per-repo extension that bowt ran (code is
// then the extension's exit code); handled=false means "not an extension — let
// cobra handle it" (a built-in, a flag, or an unknown command with no matching
// extension, which becomes cobra's normal error).
func tryExtension(root *cobra.Command, args []string) (code int, handled bool) {
	// No word, or a flag (e.g. --help/--version): cobra's job, never an extension.
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return 0, false
	}
	// Let cobra resolve the word first: if it maps to any real command (built-in,
	// help, __complete, …), Find returns that command and built-ins always win —
	// an extension can never shadow a built-in. When the word is unknown, Find
	// returns the root command WITH an "unknown command" error; that error is
	// exactly our signal to try an extension, so we key only on cmd != root.
	cmd, _, _ := root.Find(args)
	if cmd != root {
		return 0, false
	}
	// args[0] is unknown to cobra. Only inside a repo with a config dir can an
	// extension exist; otherwise fall through to cobra's unknown-command error.
	main, err := repo.MainRepo()
	if err != nil {
		return 0, false
	}
	ext, found, err := extension.Find(config.Dir(main), args[0])
	if err != nil {
		output.Errf("%v", err)
		return 1, true
	}
	if !found {
		return 0, false
	}
	code, err = cmdExtension(main, ext, args[1:])
	if err != nil {
		output.Errf("%v", err)
		return 1, true
	}
	return code, true
}

// cmdExtension runs a resolved extension against the worktree the caller stands
// in: cwd = that worktree, the per-worktree BOWT_*/GWT_* + config env injected,
// and the manifest's lock taken around the run so the author writes no lock
// code. It returns the child's exit code; err covers only setup/lock failures.
func cmdExtension(main string, ext extension.Extension, args []string) (int, error) {
	top, err := repo.Toplevel("")
	if err != nil {
		return 1, err
	}
	name := filepath.Base(main)
	branch := repo.CurrentBranch(top)

	// Per-worktree env — the same environment exec/gate build. A broken config is
	// a warning, not a hard failure: the extension stays runnable.
	r := run.Exec{Stderr: os.Stderr}
	vars, err := config.Load(r, config.Dir(main))
	if err != nil {
		output.Errf("load config: %v — running %q without config env", err, ext.Cmd)
		vars = nil
	}
	info := env.Info{Path: top, Branch: branch, MainRepo: main, RepoName: name}
	if st, err := state.Open(); err == nil {
		if wt, ok, gerr := st.Get(name, branch); gerr == nil && ok {
			info.Offset, info.Port, info.CodeOnly = wt.Offset, wt.Port, wt.CodeOnly()
		}
	}
	envKV := env.Build(info, vars)

	// The manifest owns locking. Key on the worktree path (as gate/spawn/review
	// do) and fail fast with the busy message if another bowt process holds it.
	// The lock is released by defer before this returns, so main's os.Exit with
	// the propagated code never leaks it (the kernel also frees flock on exit).
	switch ext.Manifest.Lock {
	case extension.LockExclusive:
		l, lerr := lock.Acquire(top)
		if lerr != nil {
			return 1, lerr
		}
		defer func() { _ = l.Release() }()
	case extension.LockShared:
		l, lerr := lock.AcquireShared(top)
		if lerr != nil {
			return 1, lerr
		}
		defer func() { _ = l.Release() }()
	}

	return extension.Run(ext, top, envKV, args, os.Stdin, os.Stdout, os.Stderr)
}

// extInfo is the agent-facing shape for one extension in `bowt extensions`.
type extInfo struct {
	Cmd  string `json:"cmd"`
	Desc string `json:"desc"`
	Lock string `json:"lock"`
}

func cmdExtensions() error {
	main, err := repo.MainRepo()
	if err != nil {
		return err
	}
	exts, err := extension.List(config.Dir(main))
	if err != nil {
		return err
	}
	infos := make([]extInfo, 0, len(exts))
	for _, e := range exts {
		lockMode := e.Manifest.Lock
		if lockMode == "" {
			lockMode = extension.LockNone
		}
		infos = append(infos, extInfo{Cmd: e.Cmd, Desc: e.Manifest.Desc, Lock: string(lockMode)})
	}
	// Agent-first: JSON unless a human is at the terminal.
	if !output.IsTTY() {
		return output.Emit(infos)
	}
	if len(infos) == 0 {
		fmt.Println("no extensions")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "COMMAND\tLOCK\tDESCRIPTION")
	for _, e := range infos {
		fmt.Fprintf(w, "%s\t%s\t%s\n", e.Cmd, e.Lock, e.Desc)
	}
	return w.Flush()
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
