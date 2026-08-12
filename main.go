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
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

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
	// Discover user drop-in providers (~/.bowt/agents/*.json) once at startup,
	// AFTER package init has registered the built-ins, so a colliding drop-in is
	// skipped (built-ins win) and provider selection sees the full set. Mirrors
	// how the extension loader is wired from main; a missing dir is a no-op.
	agent.LoadDropins(output.Errf)

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
		newLaneCmd(),
		newLanesCmd(),
		newStatusCmd(),
		newLaneRunCmd(),
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
	c.Flags().BoolVar(&opts.headless, "headless", false, "run non-interactively in the background: a detached supervisor holds the lock, records a lane, and captures output to a log (returns a lane id immediately)")
	c.Flags().StringVar(&opts.ticket, "ticket", "", "human ticket ref recorded on the lane (also seeds the lane id)")
	c.Flags().IntVar(&opts.wave, "wave", 0, "dispatch wave recorded on the lane (orchestration hint; not scheduled)")
	c.Flags().StringSliceVar(&opts.deps, "deps", nil, "lane ids this lane depends on, recorded on the lane (comma-separated)")
	return c
}

// newLaneRunCmd registers the hidden supervisor entrypoint. It is not a user
// command — the launcher re-execs it (`bowt _lane-run <spec>`) to become the
// detached process that holds the lock and owns the lane row.
func newLaneRunCmd() *cobra.Command {
	return &cobra.Command{
		Use:    spawn.LaneRunCommand + " <spec>",
		Short:  "internal: detached lane supervisor (re-exec'd by spawn --headless)",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmdLaneRun(args[0])
		},
	}
}

// newLaneCmd is the parent group for lane-scoped verbs. A headless spawn records
// a lane row; these operate on that row. (Only `followup` ships in this slice —
// the read-only `lane show`/`lanes` cockpit is G4.)
func newLaneCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "lane",
		Short: "operate on a headless spawn's lane (the tracked unit of delegated work)",
		Long: `A headless spawn (bowt spawn --headless) records a lane: a tracked unit of
delegated work with a status, provenance, and derived comms. lane subcommands
act on that row by id.`,
	}
	c.AddCommand(newLaneFollowupCmd())
	return c
}

// newLaneFollowupCmd registers `bowt lane followup <id>`: feedback → re-spawn.
func newLaneFollowupCmd() *cobra.Command {
	var msg, file string
	c := &cobra.Command{
		Use:   "followup <id> [-m <msg> | -f <file>]",
		Short: "feed a message back to a lane and re-spawn it headless",
		Long: `Write a follow-up message to the lane's worktree (subagent/FOLLOWUP.md, which
the next spawn auto-appends to the brief), bump the lane's attempt, recompute its
provenance, reset its status to the running phase, clear the prior attempt's
comms, and re-spawn the agent headless against the same lane.

The message is either inline (-m) or read from a file (-f); pass exactly one. The
prior writeback files remain on disk as the agent's context for the new pass.`,
		Example: `  bowt lane followup my-lane-ab12 -m "address the review blockers, then re-run gate"
  bowt lane followup my-lane-ab12 -f notes/followup.md`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmdLaneFollowup(args[0], msg, file)
		},
	}
	c.Flags().StringVarP(&msg, "message", "m", "", "inline follow-up message")
	c.Flags().StringVarP(&file, "file", "f", "", "read the follow-up message from a file")
	return c
}

// newLanesCmd registers `bowt lanes`: the read-only lane list, one row per lane
// with its (reconciled) status + derived scalars.
func newLanesCmd() *cobra.Command {
	var asJSON bool
	var repoName string
	c := &cobra.Command{
		Use:   "lanes [--repo <r>]",
		Short: "list the tracked lanes and their reconciled status",
		Long: `List every headless-spawn lane for the repo — id, status, agent/model,
attempt, wave, gate verdict, review C/S/N, and the escalated/paused flags.

Each lane is reconciled at read time: a lane stuck in a running status
(planning/impl/review) whose worktree lock is free (its detached supervisor died
before writing the terminal status) is repaired from its writeback files before
it is shown, and the repair is persisted (self-heal). A human table is printed at
a terminal; JSON is emitted otherwise (agent-first) or with --json.`,
		Example: `  bowt lanes
  bowt lanes --json
  bowt lanes --repo other-repo`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ls, err := state.OpenLanes()
			if err != nil {
				return err
			}
			if repoName == "" {
				repoName, err = repo.Name()
				if err != nil {
					return err
				}
			}
			return cmdLanes(ls, repoName, lock.Probe, asJSON)
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "force JSON output")
	c.Flags().StringVar(&repoName, "repo", "", "repo to list lanes for (default: current repo)")
	return c
}

// newStatusCmd registers `bowt status`: the per-worktree cockpit joining the
// registry × lanes × live lock × gate.json.
func newStatusCmd() *cobra.Command {
	var asJSON bool
	c := &cobra.Command{
		Use:   "status",
		Short: "per-worktree cockpit: registry × lanes × lock × gate",
		Long: `Show one row per registered worktree in the current repo — its HEAD commit,
dirty flag, whether its lock is currently held (a live spawn/gate/land), the
worktree's gate verdict (from .bowt/gate.json), and the lane(s) on it with each
lane's reconciled status, gate verdict, review C/S/N, and escalated/paused flags.

A worktree with no lane still appears (from the registry), just without lane
data. Lanes are reconciled at read time exactly as 'bowt lanes' does. This is the
one screen an orchestrator reads instead of grepping. JSON is emitted off a
terminal (agent-first) or with --json; a human table at a terminal.`,
		Example: `  bowt status
  bowt status --json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := state.Open()
			if err != nil {
				return err
			}
			ls, err := state.OpenLanes()
			if err != nil {
				return err
			}
			name, err := repo.Name()
			if err != nil {
				return err
			}
			return cmdStatus(st, ls, name, lock.Probe, realWorktreeFacts, asJSON)
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "force JSON output")
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

// laneView is the read-only cockpit projection of a lane row: a subset of
// state.Lane's scalars an orchestrator reads, plus Reconciled — set when the
// status was repaired from files at THIS read (a SIGKILL-orphaned row self-heal
// has now corrected). A declared shape, per house style, not a map.
type laneView struct {
	ID             string       `json:"id"`
	Ticket         string       `json:"ticket,omitempty"`
	Status         state.Status `json:"status"`
	Agent          string       `json:"agent,omitempty"`
	Model          string       `json:"model,omitempty"`
	Attempt        int          `json:"attempt"`
	Wave           int          `json:"wave"`
	GateVerdict    string       `json:"gate_verdict,omitempty"`
	ReviewBlockers int          `json:"review_blockers"`
	ReviewMajors   int          `json:"review_majors"`
	ReviewMinors   int          `json:"review_minors"`
	Escalated      bool         `json:"escalated"`
	PausedOn       string       `json:"paused_on,omitempty"`
	Branch         string       `json:"branch"`
	Worktree       string       `json:"worktree"`
	Reconciled     bool         `json:"reconciled,omitempty"`
}

func toLaneView(l state.Lane, reconciled bool) laneView {
	return laneView{
		ID: l.ID, Ticket: l.Ticket, Status: l.Status, Agent: l.Agent, Model: l.Model,
		Attempt: l.Attempt, Wave: l.Wave, GateVerdict: l.GateVerdict,
		ReviewBlockers: l.ReviewBlockers, ReviewMajors: l.ReviewMajors, ReviewMinors: l.ReviewMinors,
		Escalated: l.Escalated, PausedOn: l.PausedOn, Branch: l.Branch, Worktree: l.Worktree,
		Reconciled: reconciled,
	}
}

// worktreeView is the per-worktree cockpit row: the registry facts joined with
// the live lock state, the on-disk gate verdict, and the lane(s) on the worktree.
type worktreeView struct {
	Repo        string     `json:"repo"`
	Branch      string     `json:"branch"`
	Path        string     `json:"path"`
	Commit      string     `json:"commit,omitempty"`
	Dirty       bool       `json:"dirty"`
	LockHeld    bool       `json:"lock_held"`
	GateVerdict string     `json:"gate_verdict,omitempty"`
	GateCommit  string     `json:"gate_commit,omitempty"`
	Lanes       []laneView `json:"lanes"`
}

// reconcileForRead applies the G4 reconcile-on-read to one lane: it probes the
// worktree lock (the live-supervisor signal) and, for a running-status lane whose
// lock is FREE, reconstructs the terminal state from files (spawn.ParseComms +
// the pure reconcile decision). It only READS; the caller persists a change
// (self-heal). Returns the (possibly corrected) lane and whether it changed.
func reconcileForRead(lane state.Lane, probe func(string) (bool, error)) (state.Lane, bool, error) {
	held, err := probe(lane.Worktree)
	if err != nil {
		return lane, false, err
	}
	if held || !isRunning(lane.Status) {
		return lane, false, nil // live lane or already terminal: no file read needed
	}
	comms, err := spawn.ParseComms(
		filepath.Join(lane.Worktree, lane.WritebackDir),
		filepath.Join(lane.Worktree, review.ReviewDirName),
	)
	if err != nil {
		return lane, false, err
	}
	return reconcile(lane, held, comms)
}

// reconcileLaneViews reconciles each lane at read time, SELF-HEALS a corrected
// row (RFC option b: a running-status lane whose lock is free is unambiguously
// stale, so persist the reconstructed terminal status to RETIRE the SIGKILL edge
// rather than re-derive it every read — the decision stays the pure reconcile()
// above; only this reader writes), and projects the result. Shared by `lanes`
// and `status` so both reconcile identically.
func reconcileLaneViews(ls state.LaneStore, lanes []state.Lane, probe func(string) (bool, error)) ([]laneView, error) {
	views := make([]laneView, 0, len(lanes))
	for _, lane := range lanes {
		fixed, changed, err := reconcileForRead(lane, probe)
		if err != nil {
			return nil, err
		}
		if changed {
			if err := ls.UpdateLane(fixed); err != nil {
				return nil, err
			}
		}
		views = append(views, toLaneView(fixed, changed))
	}
	return views, nil
}

// cmdLanes lists the repo's lanes as the read-only cockpit projection, each lane
// reconciled (and self-healed) first. Agent-first: JSON off a TTY or with --json.
func cmdLanes(ls state.LaneStore, repoName string, probe func(string) (bool, error), asJSON bool) error {
	lanes, err := ls.ListLanes(repoName)
	if err != nil {
		return err
	}
	views, err := reconcileLaneViews(ls, lanes, probe)
	if err != nil {
		return err
	}
	if asJSON || !output.IsTTY() {
		return output.Emit(views)
	}
	if len(views) == 0 {
		fmt.Println("no lanes")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tSTATUS\tAGENT\tMODEL\tATT\tWAVE\tGATE\tREVIEW\tFLAGS\tBRANCH")
	for _, v := range views {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%d\t%s\t%s\t%s\t%s\n",
			v.ID, v.Status, orDash(v.Agent), orDash(v.Model), v.Attempt, v.Wave,
			orDash(v.GateVerdict), reviewCell(v), laneFlags(v), v.Branch)
	}
	return w.Flush()
}

// cmdStatus is the per-worktree cockpit: the registry joined with each worktree's
// live lock, its on-disk gate verdict, and the reconciled lane(s) on it. facts
// gathers the git/gate IO (injected so the join is testable without git); probe
// reads the live lock. Agent-first: JSON off a TTY or with --json.
func cmdStatus(st state.Store, ls state.LaneStore, repoName string, probe func(string) (bool, error), facts gatherFacts, asJSON bool) error {
	wts, err := st.List(repoName)
	if err != nil {
		return err
	}
	lanes, err := ls.ListLanes(repoName)
	if err != nil {
		return err
	}
	laneViews, err := reconcileLaneViews(ls, lanes, probe)
	if err != nil {
		return err
	}
	// Join lanes to worktrees on branch (worktrees' PK is (repo,branch); both
	// lists are already scoped to repoName).
	byBranch := make(map[string][]laneView, len(laneViews))
	for _, lv := range laneViews {
		byBranch[lv.Branch] = append(byBranch[lv.Branch], lv)
	}

	views := make([]worktreeView, 0, len(wts))
	for _, wt := range wts {
		f, err := facts(wt)
		if err != nil {
			return err
		}
		held, err := probe(wt.Path)
		if err != nil {
			return err
		}
		on := byBranch[wt.Branch]
		if on == nil {
			on = []laneView{} // a no-lane worktree still appears, with [] lanes
		}
		views = append(views, worktreeView{
			Repo: wt.Repo, Branch: wt.Branch, Path: wt.Path,
			Commit: f.Commit, Dirty: f.Dirty, LockHeld: held,
			GateVerdict: f.GateVerdict, GateCommit: f.GateCommit, Lanes: on,
		})
	}

	if asJSON || !output.IsTTY() {
		return output.Emit(views)
	}
	if len(views) == 0 {
		fmt.Println("no worktrees")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "BRANCH\tCOMMIT\tDIRTY\tLOCK\tGATE\tLANES")
	for _, v := range views {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			v.Branch, orDash(v.Commit), yesNo(v.Dirty), lockCell(v.LockHeld),
			orDash(v.GateVerdict), laneSummary(v.Lanes))
	}
	return w.Flush()
}

// gatherFacts is the per-worktree IO the status cockpit needs beyond the
// registry: HEAD, dirty, and the on-disk gate verdict. Injected so the join +
// reconcile projection is tested without git or a real gate.json.
type gatherFacts func(wt state.Worktree) (wtFacts, error)

// wtFacts are the gathered facts for one worktree.
type wtFacts struct {
	Commit      string // abbreviated HEAD
	Dirty       bool
	GateVerdict string // .bowt/gate.json overall ("" when never gated)
	GateCommit  string // the commit that verdict is for (abbreviated)
}

// realWorktreeFacts is the production gatherFacts: git HEAD/dirty (best-effort —
// a worktree whose checkout is gone still lists, just without commit/dirty) plus
// the on-disk gate verdict. A corrupt gate.json is surfaced; a missing one is the
// normal "never gated" case.
func realWorktreeFacts(wt state.Worktree) (wtFacts, error) {
	var f wtFacts
	if _, short, err := repo.Head(wt.Path); err == nil {
		f.Commit = short
	}
	if dirty, err := repo.Dirty(wt.Path); err == nil {
		f.Dirty = dirty
	}
	res, ok, err := gate.ReadResult(wt.Path)
	if err != nil {
		return f, err
	}
	if ok {
		f.GateVerdict = string(res.Overall)
		f.GateCommit = res.CommitShort
	}
	return f, nil
}

// orDash renders an empty scalar as "-" in a human table.
func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func lockCell(held bool) string {
	if held {
		return "held"
	}
	return "free"
}

// reviewCell renders the C/S/N triple, or "-" when all zero.
func reviewCell(v laneView) string {
	if v.ReviewBlockers == 0 && v.ReviewMajors == 0 && v.ReviewMinors == 0 {
		return "-"
	}
	return fmt.Sprintf("%d/%d/%d", v.ReviewBlockers, v.ReviewMajors, v.ReviewMinors)
}

// laneFlags renders the comms/reconcile flags for a lane: E=escalated,
// P(<owner>)=paused, R=reconciled at this read. "-" when none.
func laneFlags(v laneView) string {
	var flags []string
	if v.Escalated {
		flags = append(flags, "E")
	}
	if v.PausedOn != "" {
		flags = append(flags, "P("+v.PausedOn+")")
	}
	if v.Reconciled {
		flags = append(flags, "R")
	}
	if len(flags) == 0 {
		return "-"
	}
	return strings.Join(flags, ",")
}

// laneSummary renders the lanes on a worktree as "id:status" tokens for the
// status table's LANES cell.
func laneSummary(lanes []laneView) string {
	if len(lanes) == 0 {
		return "-"
	}
	toks := make([]string, 0, len(lanes))
	for _, l := range lanes {
		toks = append(toks, l.ID+":"+string(l.Status))
	}
	return strings.Join(toks, " ")
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
	headless    bool
	ticket      string
	wave        int
	deps        []string
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

	if opts.headless {
		// Headless is opt-in and guard-railed: refuse a provider with no
		// Edit/Write hook guardrail up front (a backgrounded skip-perms run with
		// no guardrail is exactly what RequireHeadless forbids).
		if err := agent.RequireHeadless(ag); err != nil {
			return err
		}
		return launchHeadless(top, opts, mode, caps, a, briefPath, brief, model, effort)
	}

	// Interactive (default, unchanged): spawn is a writer — take the EXCLUSIVE
	// per-worktree lock and hold it for the agent's lifetime by running the agent
	// as a child. defer releases on return.
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

// launchHeadless is the LAUNCHER half of a backgrounded spawn. It resolves the
// lane's identity + placement, assembles the handoff spec, and re-execs a
// detached bowt supervisor — then it returns fast, WITHOUT holding the exclusive
// lock (the supervisor is the single lock owner, so there is no double-hold or
// TOCTOU window). It waits only for the supervisor to publish the lane row.
func launchHeadless(top string, opts spawnOpts, mode spawn.Mode, caps agent.Capabilities, a spawn.Assembled, briefPath, brief, model, effort string) error {
	repoName, err := repo.Name()
	if err != nil {
		return err
	}
	id, err := newLaneID(opts.ticket)
	if err != nil {
		return err
	}
	spec := spawn.LaneSpec{
		ID:            id,
		Ticket:        opts.ticket,
		Repo:          repoName,
		Branch:        repo.CurrentBranch(top),
		Worktree:      top,
		Agent:         caps.Name,
		Model:         model,
		Effort:        effort,
		Mode:          string(mode),
		PromptVersion: a.Version,
		PromptHash:    a.Hash,
		BriefPath:     briefPath,
		BriefHash:     spawn.BlobHash([]byte(brief)),
		WritebackDir:  spawn.DefaultWritebackDir,
		LogPath:       spawn.LaneLogPath(top, id),
		Wave:          opts.wave,
		Deps:          opts.deps,
		Prompt:        a.Prompt,
	}

	ls, err := state.OpenLanes()
	if err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		exe = os.Args[0] // best-effort fallback; re-exec still needs an argv0
	}
	return runHeadlessLaunch(osSpawner{}, ls, exe, spec, defaultLaunchConfig)
}

// spawner is the detach/re-exec seam: the one edge that actually forks a
// background process. A Fake lets the launcher's spec-write + readiness-poll
// logic be unit-tested WITHOUT forking (mirrors run.Runner).
type spawner interface {
	// spawn starts exe+args detached from the caller's terminal, in dir, with
	// stdout+stderr appended to logPath, and returns the child pid WITHOUT
	// waiting for it. The child outlives the caller.
	spawn(exe string, args []string, dir, logPath string) (int, error)
}

// osSpawner is the real spawner. Setsid detaches the child into its own session
// (no controlling terminal) on both darwin and linux; stdio goes to the log so
// the supervisor + agent output is captured; stdin is the null device (headless
// has no TTY). We Start but never Wait — the launcher exits and the child is
// reparented to init, which reaps it.
type osSpawner struct{}

func (osSpawner) spawn(exe string, args []string, dir, logPath string) (int, error) {
	lf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return 0, fmt.Errorf("open lane log %s: %w", logPath, err)
	}
	defer func() { _ = lf.Close() }() // the child dups the fd; our copy can close
	cmd := exec.Command(exe, args...)
	cmd.Dir = dir
	cmd.Stdin = nil // /dev/null: a headless supervisor has no interactive stdin
	cmd.Stdout = lf
	cmd.Stderr = lf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("start lane supervisor: %w", err)
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Release() // don't hold the child; it outlives us
	return pid, nil
}

// launchConfig bounds how long the launcher waits for the supervisor to publish
// the lane row before declaring a failed start.
type launchConfig struct {
	attempts int
	interval time.Duration
}

var defaultLaunchConfig = launchConfig{attempts: 100, interval: 50 * time.Millisecond}

// laneStarted is the launcher's stdout payload — agent-parseable (stdout=data).
type laneStarted struct {
	ID      string `json:"id"`
	LogPath string `json:"log_path"`
	Pid     int    `json:"pid"`
}

// runHeadlessLaunch is the testable launcher core: write the spec, fork the
// supervisor, poll for the row it publishes, then emit the lane id + log path.
// It never acquires the exclusive lock — the supervisor owns it.
func runHeadlessLaunch(sp spawner, ls state.LaneStore, exe string, spec spawn.LaneSpec, cfg launchConfig) error {
	specPath, err := spawn.WriteSpec(spec)
	if err != nil {
		return err
	}
	pid, err := sp.spawn(exe, spawn.SupervisorArgs(specPath), spec.Worktree, spec.LogPath)
	if err != nil {
		return err
	}
	// Bounded poll for the supervisor to INSERT the row (status planning|impl).
	for i := 0; i < cfg.attempts; i++ {
		if _, ok, err := ls.GetLane(spec.ID); err != nil {
			return err
		} else if ok {
			output.Errf("lane %s started (pid %d) — tail %s", spec.ID, pid, spec.LogPath)
			return output.Emit(laneStarted{ID: spec.ID, LogPath: spec.LogPath, Pid: pid})
		}
		time.Sleep(cfg.interval)
	}
	return fmt.Errorf("lane %s: supervisor did not publish the lane row within %s (see %s)",
		spec.ID, time.Duration(cfg.attempts)*cfg.interval, spec.LogPath)
}

// cmdLaneFollowup is the `bowt lane followup <id>` entrypoint. It resolves the
// message (inline -m or a -f file, exactly one), the lane row, and the agent,
// then hands the real seams to runFollowup. It never launches a real agent
// itself — the re-spawn goes through the same launcher a fresh headless spawn
// uses (the supervisor owns the lock and the agent process).
func cmdLaneFollowup(id, msg, file string) error {
	message, err := resolveFollowupMessage(msg, file)
	if err != nil {
		return err
	}

	ls, err := state.OpenLanes()
	if err != nil {
		return err
	}
	lane, ok, err := ls.GetLane(id)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("lane %q not found", id)
	}
	if fi, err := os.Stat(lane.Worktree); err != nil || !fi.IsDir() {
		return fmt.Errorf("lane %q: worktree %s is gone — cannot re-spawn", id, lane.Worktree)
	}

	// The follow-up re-spawns headless: refuse a provider without the Edit/Write
	// guardrail up front, exactly as `spawn --headless` does.
	ag, err := agent.New(lane.Agent)
	if err != nil {
		return err
	}
	if err := agent.RequireHeadless(ag); err != nil {
		return err
	}

	exe, err := os.Executable()
	if err != nil {
		exe = os.Args[0]
	}
	return runFollowup(followupDeps{sp: osSpawner{}, ls: ls, exe: exe, caps: ag.Caps(), cfg: defaultLaunchConfig}, lane, message)
}

// resolveFollowupMessage returns the follow-up text from exactly one of -m / -f.
// Passing both, neither, or an empty message is a hard error (a follow-up with
// nothing to say would re-spawn the agent with no new guidance).
func resolveFollowupMessage(msg, file string) (string, error) {
	switch {
	case msg != "" && file != "":
		return "", fmt.Errorf("pass exactly one of -m/--message or -f/--file, not both")
	case msg != "":
		return msg, nil
	case file != "":
		b, err := os.ReadFile(file)
		if err != nil {
			return "", fmt.Errorf("read follow-up file %s: %w", file, err)
		}
		if len(strings.TrimSpace(string(b))) == 0 {
			return "", fmt.Errorf("follow-up file %s is empty", file)
		}
		return string(b), nil
	default:
		return "", fmt.Errorf("provide a follow-up message with -m/--message or -f/--file")
	}
}

// followupDeps bundles the seams runFollowup drives so its logic is unit-tested
// with a fake spawner + LaneStore and no real agent (caps is pure data).
type followupDeps struct {
	sp   spawner
	ls   state.LaneStore
	exe  string
	caps agent.Capabilities
	cfg  launchConfig
}

// runFollowup is the testable core of `lane followup`: (1) write FOLLOWUP.md to
// the worktree (ResolveBrief auto-appends it on the next spawn); (2) re-resolve
// the brief (now carrying the follow-up) and re-Assemble to recompute the
// provenance line; (3) bump attempt, update provenance/brief hash, reset status
// to the running phase, clear the prior attempt's terminal comms, and point the
// row at this attempt's log in ONE UpdateLane; (4) re-spawn headless through the
// SAME launcher a fresh spawn uses, reusing the lane id (the supervisor's
// publish reuses the row rather than re-INSERTing it).
func runFollowup(d followupDeps, lane state.Lane, message string) error {
	// (1) FOLLOWUP.md — the handshake input the next spawn appends to the brief.
	subagentDir := filepath.Join(lane.Worktree, "subagent")
	if err := os.MkdirAll(subagentDir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", subagentDir, err)
	}
	if err := os.WriteFile(filepath.Join(subagentDir, "FOLLOWUP.md"), []byte(message), 0o644); err != nil {
		return fmt.Errorf("write FOLLOWUP.md: %w", err)
	}

	// (2) Re-resolve the brief (ResolveBrief auto-appends the FOLLOWUP.md we just
	// wrote) and re-Assemble so a prompt-version bump since the last attempt is
	// reflected in the recomputed provenance.
	mode := spawn.Mode(lane.PromptMode)
	briefPath, brief, err := spawn.ResolveBrief(lane.Worktree, "")
	if err != nil {
		return err
	}
	a, err := spawn.Assemble(mode, d.caps.Name, d.caps.MemoryFile, brief)
	if err != nil {
		return err
	}

	// (3) Bump attempt + provenance, reset status to the running phase, clear the
	// prior attempt's terminal comms, retarget the log — one UpdateLane.
	lane.Attempt++
	lane.PromptVersion = a.Version
	lane.PromptHash = a.Hash
	lane.BriefPath = briefPath
	lane.BriefHash = spawn.BlobHash([]byte(brief))
	lane.Status = runningStatus(lane.PromptMode)
	lane.Escalated = false
	lane.EscalationNote = ""
	lane.PausedOn = ""
	lane.ReviewBlockers, lane.ReviewMajors, lane.ReviewMinors = 0, 0, 0
	lane.LogPath = spawn.LaneLogPathAttempt(lane.Worktree, lane.ID, lane.Attempt)
	if err := d.ls.UpdateLane(lane); err != nil {
		return err
	}

	// (4) Re-spawn headless through the launcher, reusing the lane id + row.
	spec := spawn.LaneSpec{
		ID:            lane.ID,
		Ticket:        lane.Ticket,
		Repo:          lane.Repo,
		Branch:        lane.Branch,
		Worktree:      lane.Worktree,
		Agent:         lane.Agent,
		Model:         lane.Model,
		Effort:        spawn.ResolveEffort("", mode == spawn.ModeImpl),
		Mode:          lane.PromptMode,
		PromptVersion: a.Version,
		PromptHash:    a.Hash,
		BriefPath:     briefPath,
		BriefHash:     lane.BriefHash,
		WritebackDir:  lane.WritebackDir,
		LogPath:       lane.LogPath,
		Wave:          lane.Wave,
		Deps:          lane.Deps,
		Prompt:        a.Prompt,
	}
	return runHeadlessLaunch(d.sp, d.ls, d.exe, spec, d.cfg)
}

// cmdLaneRun is the SUPERVISOR entrypoint (hidden `bowt _lane-run <spec>`). It
// is the long-lived, detached bowt process that holds the worktree lock for the
// agent's whole lifetime and writes both lane rows (INSERT on start, terminal
// UPDATE on completion).
func cmdLaneRun(specPath string) error {
	spec, err := spawn.ReadSpecAt(specPath)
	if err != nil {
		return err
	}
	ls, err := state.OpenLanes()
	if err != nil {
		return err
	}
	ag, err := agent.New(spec.Agent)
	if err != nil {
		return err
	}
	return runSupervisor(ls, ag, spec, lock.Acquire, os.Stdout, os.Stderr)
}

// runSupervisor is the testable supervisor core. acquire is injected (real
// lock.Acquire in production, a tmp-key acquire in tests); stdout/stderr are the
// agent's captured streams (the lane log in production). It holds the exclusive
// worktree lock for the agent's entire run — the exec.go child-holds-lock
// invariant, moved into the detached process — and is the sole writer of the
// lane row.
func runSupervisor(ls state.LaneStore, ag agent.Agent, spec spawn.LaneSpec, acquire func(string) (*lock.Lock, error), stdout, stderr io.Writer) error {
	// Fail-fast exclusive lock, keyed on the worktree exactly as interactive
	// spawn/gate/land do. Held until this process exits (or dies — the kernel
	// releases flock on exit, incl. SIGKILL), so a second bowt can't act on the
	// worktree under the running agent.
	l, err := acquire(spec.Worktree)
	if err != nil {
		return err
	}
	defer func() { _ = l.Release() }()

	lane, err := publishRunning(ls, spec)
	if err != nil {
		return err
	}

	// Run the agent headless, streaming to the captured log. Capture the exit
	// code the same way interactive spawn does (errors.As on *exec.ExitError).
	opts := agent.Opts{Model: spec.Model, Effort: spec.Effort, Stdout: stdout, Stderr: stderr}
	runErr := ag.Headless(context.Background(), spec.Prompt, opts)
	exitCode := 0
	if runErr != nil {
		var ee *exec.ExitError
		if errors.As(runErr, &ee) {
			exitCode = ee.ExitCode()
		} else {
			exitCode = -1 // a non-exit failure (couldn't launch, signalled, ...)
		}
	}

	// Terminal UPDATE on BOTH normal and non-zero exit. (G4: reconciler — if this
	// supervisor is SIGKILLed BEFORE this write, the row is stranded in
	// planning/impl and the flock is released cleanly; a G4 reconciler-on-read
	// reconstructs the terminal status from files + the now-free lock. Out of
	// scope for G2 — no daemon.)
	writebackDir := filepath.Join(spec.Worktree, spec.WritebackDir)
	status, err := spawn.TerminalStatus(spec.Mode, exitCode, writebackDir)
	if err != nil {
		return err
	}
	lane.Status = status

	// G3: surface the writeback's comms as stored scalars in the SAME terminal
	// update — a single parse pass, no prose stored. Precedence: a "PAUSED ON"
	// marker promotes a *successful* terminal status to paused (a human owns the
	// resume); it never rescues a failed run (a crashed agent stays failed).
	// ESCALATE only sets the flag — the orchestrator triages; the lane keeps its
	// phase. Review C/S/N are recorded regardless of status.
	comms, err := spawn.ParseComms(writebackDir, filepath.Join(spec.Worktree, review.ReviewDirName))
	if err != nil {
		return err
	}
	applyComms(&lane, comms)
	if uerr := ls.UpdateLane(lane); uerr != nil {
		return uerr
	}
	return runErr
}

// applyComms folds a parsed Comms into the lane's stored scalars and applies the
// status precedence (see runSupervisor). It is pure so the precedence is unit-
// tested directly.
func applyComms(lane *state.Lane, comms spawn.Comms) {
	lane.Escalated = comms.Escalated
	lane.EscalationNote = comms.EscalationNote
	lane.PausedOn = comms.PausedOn
	lane.ReviewBlockers = comms.ReviewBlockers
	lane.ReviewMajors = comms.ReviewMajors
	lane.ReviewMinors = comms.ReviewMinors
	if comms.Paused && lane.Status != state.StatusFailed {
		lane.Status = state.StatusPaused
	}
}

// publishRunning writes the lane's running-state row at supervisor start. A
// fresh spawn has no row and is INSERTed; a `lane followup` re-spawn finds the
// row the followup command already updated (bumped attempt/provenance, reset
// status, cleared stale comms) and must NOT re-INSERT it (the id is the PK) — it
// flips that row to the running status and points it at this attempt's log,
// preserving everything followup wrote. Returns the row the terminal update
// mutates.
func publishRunning(ls state.LaneStore, spec spawn.LaneSpec) (state.Lane, error) {
	running := runningStatus(spec.Mode)
	if existing, ok, err := ls.GetLane(spec.ID); err != nil {
		return state.Lane{}, err
	} else if ok {
		existing.Status = running
		existing.LogPath = spec.LogPath
		if uerr := ls.UpdateLane(existing); uerr != nil {
			return state.Lane{}, uerr
		}
		return existing, nil
	}
	lane := laneFromSpec(spec)
	if err := ls.AddLane(lane); err != nil {
		return state.Lane{}, err
	}
	return lane, nil
}

// laneFromSpec builds the initial lane row from the handoff spec: status is the
// mode's running state (planning|impl); provenance/brief/placement are copied
// straight through (the launcher already parsed them once).
func laneFromSpec(spec spawn.LaneSpec) state.Lane {
	return state.Lane{
		ID:            spec.ID,
		Ticket:        spec.Ticket,
		Repo:          spec.Repo,
		Branch:        spec.Branch,
		Worktree:      spec.Worktree,
		Status:        runningStatus(spec.Mode),
		Agent:         spec.Agent,
		Model:         spec.Model,
		PromptMode:    spec.Mode,
		PromptVersion: spec.PromptVersion,
		PromptHash:    spec.PromptHash,
		BriefPath:     spec.BriefPath,
		BriefHash:     spec.BriefHash,
		Wave:          spec.Wave,
		Deps:          spec.Deps,
		WritebackDir:  spec.WritebackDir,
		LogPath:       spec.LogPath,
	}
}

// runningStatus is the lane's in-flight status for a spawn mode: impl runs at
// StatusImpl, plan at StatusPlanning. Shared by the fresh-INSERT and the
// followup re-spawn so both flip to the same running state.
func runningStatus(mode string) state.Status {
	if mode == string(spawn.ModeImpl) {
		return state.StatusImpl
	}
	return state.StatusPlanning
}

// isRunning reports whether a status is an in-flight state — one a live
// supervisor holds the worktree lock during (planning/impl) or leaves as the
// still-open resting state (review). Only a running-state lane is a reconcile
// candidate; a terminal status (plan-review/paused/done/failed) is never touched.
func isRunning(s state.Status) bool {
	return s == state.StatusPlanning || s == state.StatusImpl || s == state.StatusReview
}

// reconcile is the pure G4 reconcile DECISION for a lane read at cockpit time.
// It repairs the one SIGKILL edge G2 left: a detached supervisor killed AFTER
// the agent finished but BEFORE its terminal UpdateLane leaves the row stuck at a
// running status forever, even though the kernel already released the flock.
//
// The rule (the whole decision, as a table):
//   - lockHeld, OR a non-running status  ⇒ unchanged. A running status with the
//     lock still HELD is a LIVE lane (never touch it); a terminal status is done.
//   - a running status with the lock FREE ⇒ a candidate: reconstruct the terminal
//     status FROM FILES ALONE (the exit code died with the supervisor) via
//     spawn.ArtifactStatus (the same file-presence half TerminalStatus uses, so a
//     reconciled verdict can't disagree with a live one), then fold the parsed
//     comms via applyComms (the same precedence the live path applies: a PAUSED
//     marker promotes a successful status to paused; ESCALATE only sets the flag).
//
// Conservative by construction: ArtifactStatus never yields done/pass, so an
// absent writeback artifact (the agent died mid-run, not just the supervisor)
// reconciles to failed, never to a review state. It is PURE — its only inputs are
// the args plus a filesystem stat of the writeback dir (as TerminalStatus is) —
// so the whole table is unit-tested without a process. It returns the corrected
// lane and whether its status changed; only the command persists (self-heal).
func reconcile(lane state.Lane, lockHeld bool, comms spawn.Comms) (state.Lane, bool, error) {
	if lockHeld || !isRunning(lane.Status) {
		return lane, false, nil
	}
	base, err := spawn.ArtifactStatus(lane.PromptMode, filepath.Join(lane.Worktree, lane.WritebackDir))
	if err != nil {
		return lane, false, err
	}
	prev := lane.Status
	lane.Status = base
	applyComms(&lane, comms) // same scalar fold + pause precedence as runSupervisor
	return lane, lane.Status != prev, nil
}

// newLaneID mints a stable, filesystem-safe lane id: a slug of the ticket (when
// given) plus a short random suffix, so it can name the log/spec files and be
// reported before the fork races. Random bytes come from crypto/rand.
func newLaneID(ticket string) (string, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate lane id: %w", err)
	}
	suffix := hex.EncodeToString(b[:])
	slug := slugify(ticket)
	if slug == "" {
		return "lane-" + suffix, nil
	}
	return slug + "-" + suffix, nil
}

// slugify lowercases s and collapses any run of non-alphanumeric bytes to a
// single dash, trimming leading/trailing dashes — a filesystem-safe stem. A
// pending dash is only emitted once a real char follows, so leading and
// trailing separators never survive.
func slugify(s string) string {
	var out []rune
	dash := false
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			if dash && len(out) > 0 {
				out = append(out, '-')
			}
			out = append(out, r)
			dash = false
		} else {
			dash = true
		}
	}
	return string(out)
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
