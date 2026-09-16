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
	"github.com/jfb1121/bowt/internal/research"
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
		newResearchCmd(),
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
			// --impl and --orch pick different wrapper roles; they cannot both apply.
			// (--orch --headless IS allowed — a nested orch-of-orch runs one level
			// down; only the human-attended ancestor acts on owner-only classes.)
			if opts.impl && opts.orch {
				return fmt.Errorf("--impl and --orch are mutually exclusive: pick one role (impl, or orchestrator)")
			}
			return cmdSpawn(opts)
		},
	}
	c.Flags().BoolVar(&opts.impl, "impl", false, "implementation pass (default is a plan + writeback pass)")
	c.Flags().BoolVar(&opts.orch, "orch", false, "orchestrator pass: decompose, delegate, gate, escalate (never writes code; interactive by default; --headless permitted for nested orch-of-orch)")
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
	c.AddCommand(newLaneWaitCmd())
	return c
}

// newLaneWaitCmd registers `bowt lane wait <id>...`: block until the named lane(s)
// leave the in-flight phase, then print their settled rows. This is the
// orchestrator notify primitive (dispatch → wait → gate → land).
func newLaneWaitCmd() *cobra.Command {
	var timeout, interval time.Duration
	var asJSON bool
	c := &cobra.Command{
		Use:   "wait <id>... [--timeout <dur>] [--interval <dur>]",
		Short: "block until the named lane(s) settle, then print their rows",
		Long: `Block until every named lane leaves the in-flight phase (planning/impl) — i.e.
until its detached supervisor has written a settled status (plan-review, review,
paused, done, or failed) — then print the settled rows.

Each poll reconciles the lane at read time exactly as 'bowt lanes' does: an
in-flight lane whose worktree lock is free (its supervisor was SIGKILLed before
writing the terminal status) is repaired from its writeback files and self-healed,
so wait can never hang on a dead supervisor. Progress goes to stderr; the JSON
result goes to stdout, so a script can capture the rows and be woken on exit.

--timeout (default 0 = wait indefinitely) elapsing with any lane still in-flight
prints the rows and returns a non-zero error naming the timed-out lane(s). A
human table is printed at a terminal; JSON is emitted otherwise (agent-first) or
with --json.`,
		Example: `  bowt lane wait my-lane-ab12
  bowt lane wait lane-a lane-b --timeout 30m
  bowt spawn --headless … ; bowt lane wait my-lane-ab12 --json`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ls, err := state.OpenLanes()
			if err != nil {
				return err
			}
			deps := laneWaitDeps{ls: ls, probe: lock.Probe, now: time.Now, sleep: time.Sleep}
			return cmdLaneWait(deps, args, timeout, interval, asJSON)
		},
	}
	c.Flags().DurationVar(&timeout, "timeout", 0, "give up after this long, non-zero exit (0 = wait indefinitely)")
	c.Flags().DurationVar(&interval, "interval", 2*time.Second, "poll interval")
	c.Flags().BoolVar(&asJSON, "json", false, "force JSON output")
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
  4. removes the worktree and deletes the merged local branch, then deletes the
     remote branch (origin/<branch>) too — unless --keep, or --no-push (which
     pushes nothing, so there is no remote branch to delete).

The result is JSON: {landed, branch, base, commit, gate_verdict, pushed,
cleaned, remote_deleted, lane_done}. Any refusal exits non-zero.`,
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
			ls, err := state.OpenLanes()
			if err != nil {
				return err
			}
			return cmdLand(st, ls, args[0], opts)
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

func newResearchCmd() *cobra.Command {
	var opts researchOpts
	c := &cobra.Command{
		Use:   "research <brief>",
		Short: "fan out headless research agents over a brief",
		Long: `Fan out headless research agents — the same subscription-powered child-process
path spawn uses (not a metered API key) — over a research task, each doing web
research (WebSearch/WebFetch), citing sources, and writing ONE findings file. It
is spawn for research rather than code: no gate, no commit, no PR — the
deliverable is the written findings under --out.

Fan-out: --queries fans one agent per line of the file (overrides --n); else the
single brief is fanned into --n angled agents (default 1). Brief resolution
matches spawn (subagent/PROMPT.md → subagent/*-prompt.md → PROMPT.md) unless a
path is passed.

Bounded concurrency is load-bearing: at most --concurrency agents run at once
(default 2), the next starting only as one finishes — the host OOMs past a
handful of concurrent agents. Each agent writes <out>/<id>.md; --synthesize adds
a final agent that merges the findings into <out>/SYNTHESIS.md.`,
		Example: `  bowt research brief.md --n 3 --concurrency 2
  bowt research --queries topics.txt --out findings/
  bowt research brief.md --n 5 --synthesize
  bowt research brief.md --n 3 --dry-run   # show the plan, launch nothing`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				opts.brief = args[0]
			}
			return cmdResearch(opts)
		},
	}
	c.Flags().StringVar(&opts.queries, "queries", "", "file with one research query per line → one agent per query (overrides --n)")
	c.Flags().IntVar(&opts.n, "n", 1, "fan the single brief into N angled agents (ignored when --queries is set)")
	c.Flags().IntVar(&opts.concurrency, "concurrency", research.DefaultConcurrency, "max agents running at once (OOM guard)")
	c.Flags().StringVar(&opts.out, "out", research.DefaultOutDir, "directory each agent writes its findings into")
	c.Flags().BoolVar(&opts.synthesize, "synthesize", false, "after all finish, one agent merges the findings into SYNTHESIS.md")
	c.Flags().StringVar(&opts.agent, "agent", "", "agent provider (claude, codex; default: $BOWT_AGENT/$GWT_AGENT or claude)")
	c.Flags().StringVar(&opts.model, "model", "", "agent model (alias opus/sonnet/haiku, or a full ID)")
	c.Flags().StringVar(&opts.effort, "effort", "", "agent reasoning effort (e.g. high)")
	c.Flags().BoolVar(&opts.dryRun, "dry-run", false, "assemble the plan (tasks, concurrency, out paths) and print it, then exit — launch no agents")
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
	// LockHeld is the live signal, reported alongside the recorded phase so a
	// reader can catch the one inconsistency the phase alone cannot express:
	// a running status (planning/impl/review) with no lock held means the
	// supervisor is gone and the row has not been healed yet — which is what
	// makes a finished lane read as still working.
	LockHeld bool `json:"lock_held"`
}

func toLaneView(l state.Lane, reconciled bool, lockHeld bool) laneView {
	return laneView{
		ID: l.ID, Ticket: l.Ticket, Status: l.Status, Agent: l.Agent, Model: l.Model,
		Attempt: l.Attempt, Wave: l.Wave, GateVerdict: l.GateVerdict,
		ReviewBlockers: l.ReviewBlockers, ReviewMajors: l.ReviewMajors, ReviewMinors: l.ReviewMinors,
		Escalated: l.Escalated, PausedOn: l.PausedOn, Branch: l.Branch, Worktree: l.Worktree,
		Reconciled: reconciled, LockHeld: lockHeld,
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
func reconcileForRead(lane state.Lane, probe func(string) (bool, error)) (state.Lane, bool, bool, error) {
	held, err := probe(lane.Worktree)
	if err != nil {
		return lane, false, false, err
	}
	if held || !isRunning(lane.Status) {
		return lane, false, held, nil // live lane or already terminal: no file read needed
	}
	comms, err := spawn.ParseComms(
		filepath.Join(lane.Worktree, lane.WritebackDir),
		filepath.Join(lane.Worktree, review.ReviewDirName),
	)
	if err != nil {
		return lane, false, false, err
	}
	fixed, changed, rerr := reconcile(lane, held, comms)
	return fixed, changed, held, rerr
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
		fixed, changed, held, err := reconcileForRead(lane, probe)
		if err != nil {
			return nil, err
		}
		if changed {
			if err := ls.UpdateLane(fixed); err != nil {
				return nil, err
			}
		}
		views = append(views, toLaneView(fixed, changed, held))
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
	return emitLaneViews(views, asJSON)
}

// emitLaneViews renders a set of reconciled lane rows agent-first: JSON off a TTY
// or with --json, else the human cockpit table. Shared by `bowt lanes` and `bowt
// lane wait` so their row output stays identical.
func emitLaneViews(views []laneView, asJSON bool) error {
	// A nil slice marshals to JSON `null`; agents expect an array. Coerce to [].
	if views == nil {
		views = []laneView{}
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

// laneWaitDeps bundles the injectable seams for runLaneWait so the poll loop is
// deterministic and instant under test: the lane store, the lock probe, and a
// clock/sleep pair. The cobra RunE wires the real state.OpenLanes(), lock.Probe,
// time.Now, and time.Sleep; tests fake the store + probe and step a virtual clock.
type laneWaitDeps struct {
	ls    state.LaneStore
	probe func(string) (bool, error)
	now   func() time.Time
	sleep func(time.Duration)
}

// runLaneWait is the testable core of `bowt lane wait`. It resolves each id up
// front (an unknown id is a hard error BEFORE any waiting), then polls every
// interval: each poll re-reads each still-pending lane and reconciles it via the
// shared reconcileForRead (persisting a correction — self-heal — exactly as the
// cockpit does), so an in-flight lane whose supervisor died settles from files and
// the wait can't hang. A lane drops out once isInFlight is false. It returns the
// latest reconciled row for every requested lane (in input order) plus, if a
// non-zero timeout elapsed with lanes still in-flight, an error naming them.
func runLaneWait(deps laneWaitDeps, ids []string, timeout, interval time.Duration) ([]laneView, error) {
	if interval <= 0 {
		return nil, fmt.Errorf("interval must be positive, got %s", interval)
	}
	// Resolve every id first; a genuinely unknown id fails before we ever block.
	pending := make([]string, 0, len(ids))
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if seen[id] {
			continue // dedup repeats so we don't double-report a lane
		}
		if _, ok, err := deps.ls.GetLane(id); err != nil {
			return nil, err
		} else if !ok {
			return nil, fmt.Errorf("unknown lane %q", id)
		}
		seen[id] = true
		pending = append(pending, id)
	}
	order := append([]string(nil), pending...)

	deadline := deps.now().Add(timeout) // only consulted when timeout > 0
	views := make(map[string]laneView, len(order))
	for {
		stillPending := pending[:0]
		for _, id := range pending {
			lane, ok, err := deps.ls.GetLane(id)
			if err != nil {
				return nil, err
			}
			if !ok {
				return nil, fmt.Errorf("unknown lane %q", id)
			}
			fixed, changed, held, err := reconcileForRead(lane, deps.probe)
			if err != nil {
				return nil, err
			}
			if changed {
				if err := deps.ls.UpdateLane(fixed); err != nil {
					return nil, err
				}
			}
			views[id] = toLaneView(fixed, changed, held)
			if isInFlight(fixed.Status) {
				stillPending = append(stillPending, id)
			}
		}
		pending = stillPending
		if len(pending) == 0 {
			break // all settled
		}
		if timeout > 0 && !deps.now().Before(deadline) {
			break // timed out with lanes still in-flight
		}
		output.Errf("waiting on %d lane(s)…", len(pending))
		deps.sleep(interval)
	}

	rows := make([]laneView, 0, len(order))
	for _, id := range order {
		rows = append(rows, views[id])
	}
	if len(pending) > 0 {
		return rows, fmt.Errorf("timed out after %s waiting on lane(s): %s",
			timeout, strings.Join(pending, ", "))
	}
	return rows, nil
}

// cmdLaneWait runs the wait core, then prints the resulting rows (mirroring
// `bowt lanes`) whether it settled or timed out, and finally returns the wait
// error so a timeout exits non-zero for a script to react to.
func cmdLaneWait(deps laneWaitDeps, ids []string, timeout, interval time.Duration, asJSON bool) error {
	rows, waitErr := runLaneWait(deps, ids, timeout, interval)
	if rows != nil {
		if err := emitLaneViews(rows, asJSON); err != nil {
			return err
		}
	}
	return waitErr
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

// spawnOpts carries the parsed flags for `bowt spawn`.
type spawnOpts struct {
	brief       string
	impl        bool
	orch        bool
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
	switch {
	case opts.impl:
		mode = spawn.ModeImpl
	case opts.orch:
		mode = spawn.ModeOrch
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

	model := spawn.ResolveModel(opts.model, mode)
	effort := spawn.ResolveEffort(opts.effort, mode)
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
		Effort:        spawn.ResolveEffort("", mode),
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

	// Tell our descendants the worktree lock is already held on their behalf.
	// The agent we are about to run is a child, so it inherits this — and so do
	// the `bowt test`/`gate`/`review` calls it makes. Without it a lane cannot
	// run its own verification: flock belongs to an open file description, not a
	// process tree, so the agent's acquire would fail fast against its own
	// supervisor and report the worktree busy.
	if err := lock.ExportHeld(spec.Worktree); err != nil {
		output.Errf("could not export %s: %v — the agent's own bowt calls may report the worktree busy", lock.EnvHeld, err)
	}

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
	status, err := spawn.TerminalStatus(spec.Mode, exitCode, writebackDir, lane.Created)
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

// runningStatus is the lane's in-flight status for a spawn mode: impl (and, for
// now, orch) run at StatusImpl, plan at StatusPlanning. Shared by the
// fresh-INSERT and the followup re-spawn so both flip to the same running state.
// orch mirrors impl here to match its terminal mapping (STATUS.md → review);
// an orch-specific in-flight/terminal state is a later scheduler-slice decision.
func runningStatus(mode string) state.Status {
	if mode == string(spawn.ModeImpl) || mode == string(spawn.ModeOrch) {
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

// isInFlight reports whether a lane's agent is still working — the phase `bowt
// lane wait` blocks on. It is DELIBERATELY narrower than isRunning: it excludes
// StatusReview, which for waiting is a SETTLED state (the agent has finished; the
// review fan-out / triage is the orchestrator's job, not something wait blocks
// on). Everything else — plan-review/review/paused/done/failed — is settled, so
// wait returns once a lane leaves {planning, impl}.
func isInFlight(s state.Status) bool {
	return s == state.StatusPlanning || s == state.StatusImpl
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
	base, err := spawn.ArtifactStatus(lane.PromptMode, filepath.Join(lane.Worktree, lane.WritebackDir), lane.Created)
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
	// Reentrant: a spawned lane must be able to gate its own work, and its
	// supervisor already holds this key (see lock.HeldByAncestor).
	l, err := lock.AcquireReentrant(top)
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
	Landed        bool   `json:"landed"`
	Branch        string `json:"branch"`
	Base          string `json:"base"`
	Commit        string `json:"commit"`
	GateVerdict   string `json:"gate_verdict"`
	Pushed        bool   `json:"pushed"`
	Cleaned       bool   `json:"cleaned"`
	RemoteDeleted bool   `json:"remote_deleted"`
	// LaneDone is true when a lane row for this branch existed and was closed to
	// StatusDone. Absent (omitempty) when the branch had no lane — an interactive
	// spawn or a hand-made branch — so a no-lane land reads identically to before.
	LaneDone bool `json:"lane_done,omitempty"`
}

// gateSkipped marks a land that bypassed verification via --no-gate; any other
// verdict value is a real gate.Status ("pass").
const gateSkipped = "skipped"

// cmdLand gates a branch, fast-forwards it onto its base, pushes, and cleans up
// — refusing (never forcing) if the branch is ungated, dirty, or not a
// fast-forward. It encodes "never merge an ungated lane" as a verb.
func cmdLand(st state.Store, ls state.LaneStore, branch string, opts landOpts) error {
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
		// -D, not -d: land advances the base, not the branch's upstream, so `git
		// branch -d`'s merged-check would refuse a branch that is provably merged
		// (we only reach here after a verified FF-merge).
		if err := repo.DeleteBranchForce(main, branch); err != nil {
			_ = output.Emit(result)
			return fmt.Errorf("landed %q and removed its worktree, but deleting local branch failed: %w", branch, err)
		}
		result.Cleaned = true

		// The base was pushed, so the merged feature branch is now dead weight on
		// the remote — delete it too. Tolerates a branch that was never pushed.
		if result.Pushed {
			if err := repo.DeleteRemoteBranch(main, "origin", branch); err != nil {
				_ = output.Emit(result)
				return fmt.Errorf("landed %q and cleaned up locally, but deleting remote branch origin/%s failed: %w", branch, branch, err)
			}
			result.RemoteDeleted = true
		}
	}

	// Close the lane: a landed branch's lane row (if any) is terminal-done. Do
	// this regardless of --keep/--no-push — the branch is landed either way — so a
	// landed lane reads `done`, not the `review`→`failed` that reconcile-on-read
	// would otherwise infer from the (now-absent) worktree. Best-effort: the merge
	// above is already committed and irreversible, so a lane-store error warns but
	// never turns a successful land into a failure. No matching row is a no-op.
	if done, lerr := markLaneDone(ls, name, branch); lerr != nil {
		output.Errf("landed %q onto %s, but marking its lane done failed (the land still succeeded): %v", branch, base, lerr)
	} else {
		result.LaneDone = done
	}

	return output.Emit(result)
}

// markLaneDone sets the active lane row for (repoName, branch) to StatusDone and
// reports whether a row was updated. A branch with no lane row (an interactive
// spawn or a hand-made branch) is a silent no-op. An already-done row is left
// as-is and still counts as closed.
//
// A branch NAME can be reused across spawns (lane ids are random, and land
// deletes the branch afterward), so several lane rows may share one branch —
// older attempts left terminal beside the active row. The land that just
// happened is the newest lane, so close that one, not a stale older row: ignoring
// this would re-strand the active lane at `review` and reconcile-on-read would
// flip it to `failed`, the very bug this closes. ListLanes orders by created
// ascending, so the last match is the newest.
func markLaneDone(ls state.LaneStore, repoName, branch string) (bool, error) {
	lanes, err := ls.ListLanes(repoName)
	if err != nil {
		return false, fmt.Errorf("list lanes for %q: %w", repoName, err)
	}
	target := -1
	for i := range lanes {
		if lanes[i].Branch == branch {
			target = i
		}
	}
	if target < 0 {
		return false, nil
	}
	l := lanes[target]
	if l.Status == state.StatusDone {
		return true, nil
	}
	l.Status = state.StatusDone
	if err := ls.UpdateLane(l); err != nil {
		return false, fmt.Errorf("mark lane %q done: %w", l.ID, err)
	}
	return true, nil
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
	// Reentrant so a spawned lane can review its own diff (see lock.HeldByAncestor).
	l, err := lock.AcquireReentrant(top)
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

// researchOpts carries the parsed flags for `bowt research`.
type researchOpts struct {
	brief       string
	queries     string
	n           int
	concurrency int
	out         string
	synthesize  bool
	agent       string
	model       string
	effort      string
	dryRun      bool
}

// researchTaskPlan is one task in the --dry-run plan (agent-facing shape).
type researchTaskPlan struct {
	ID      string `json:"id"`
	OutPath string `json:"out_path"`
}

// researchPlan is the --dry-run projection: what research WOULD launch, without
// forking a single agent.
type researchPlan struct {
	OutDir      string             `json:"out_dir"`
	Concurrency int                `json:"concurrency"`
	Synthesize  bool               `json:"synthesize"`
	Tasks       []researchTaskPlan `json:"tasks"`
}

// cmdResearch fans out headless research agents over a brief (or per-query),
// each writing ONE findings file under --out. It reuses the spawn agent path
// (subscription-powered, not a metered key) and the versioned research wrapper,
// but takes NO per-worktree lock: a research agent is a transient child that
// only reads the repo and writes under --out, so N run concurrently rather than
// serializing on the one worktree lock a spawn lane holds. The load-bearing
// bound is --concurrency (OOM guard), enforced by research.Runner.
func cmdResearch(opts researchOpts) error {
	// Root the fan-out at the current worktree: the brief and the out dir belong
	// to the checkout you stand in.
	top, err := repo.Toplevel("")
	if err != nil {
		return err
	}

	// Select the provider up front (flag → env → claude) and hard-error BEFORE any
	// work if it cannot run headless — a research agent runs unattended (no TTY),
	// so it needs the same Edit/Write guardrail `spawn --headless` requires.
	ag, err := agent.Select(opts.agent, os.Getenv)
	if err != nil {
		return err
	}
	if err := agent.RequireHeadless(ag); err != nil {
		return err
	}
	caps := ag.Caps()

	// A brief and --queries are conflicting fan-out sources: --queries would
	// silently discard the brief. Refuse rather than drop it.
	if opts.queries != "" && opts.brief != "" {
		return fmt.Errorf("pass either a brief or --queries, not both")
	}

	// Fan-out: --queries (one agent per line) wins over the single brief + --n.
	briefs, source, err := researchBriefs(top, opts)
	if err != nil {
		return err
	}

	// Floor the concurrency to the OOM-guard default up front, so the header, the
	// --dry-run plan, and the runner all report and enforce the SAME bound (a <=0
	// value runs at DefaultConcurrency, not "0").
	concurrency := opts.concurrency
	if concurrency <= 0 {
		concurrency = research.DefaultConcurrency
	}

	model := spawn.ResolveModel(opts.model, spawn.ModeResearch)
	effort := spawn.ResolveEffort(opts.effort, spawn.ModeResearch)

	outDir := opts.out
	if outDir == "" {
		outDir = research.DefaultOutDir
	}
	if !filepath.IsAbs(outDir) {
		outDir = filepath.Join(top, outDir)
	}

	// Assemble one research prompt per brief, substituting the per-agent out path
	// into the wrapper's {{OUT_FILE}} placeholder (Assemble leaves it intact).
	tasks := make([]research.Task, len(briefs))
	for i, b := range briefs {
		a, aerr := spawn.Assemble(spawn.ModeResearch, caps.Name, caps.MemoryFile, b)
		if aerr != nil {
			return aerr
		}
		id := research.TaskID(i, len(briefs))
		outPath := filepath.Join(outDir, id+".md")
		tasks[i] = research.Task{
			ID:      id,
			Prompt:  strings.ReplaceAll(a.Prompt, spawn.OutFilePlaceholder, outPath),
			OutPath: outPath,
			LogPath: filepath.Join(outDir, id+".log"),
		}
	}

	output.Errf("research → %s", top)
	fmt.Fprintf(os.Stderr, "  source: %s   agents: %d   concurrency: %d   agent: %s\n",
		source, len(tasks), concurrency, caps.Name)
	fmt.Fprintf(os.Stderr, "  out: %s   model: %s   effort: %s   synthesize: %t\n",
		outDir, orDefault(model), orDefault(effort), opts.synthesize)

	// Dry-run: print the plan (what would launch) and stop — no agents, no writes.
	if opts.dryRun {
		plan := researchPlan{OutDir: outDir, Concurrency: concurrency, Synthesize: opts.synthesize}
		for _, t := range tasks {
			plan.Tasks = append(plan.Tasks, researchTaskPlan{ID: t.ID, OutPath: t.OutPath})
		}
		return output.Emit(plan)
	}

	r := &research.Runner{
		OutDir:      outDir,
		Concurrency: concurrency,
		Launch:      headlessLauncher(ag, model, effort),
		Synthesize:  opts.synthesize,
		SynthTask: func(paths []string, outPath, logPath string) research.Task {
			return research.Task{ID: "synthesis", Prompt: research.SynthesisPrompt(paths, outPath), OutPath: outPath, LogPath: logPath}
		},
		Log: func(format string, args ...any) { fmt.Fprintf(os.Stderr, format+"\n", args...) },
	}
	res, err := r.Run(context.Background(), tasks)
	if err != nil {
		return err
	}
	return output.Emit(res)
}

// researchBriefs resolves the fan-out into one brief per agent and a label for
// the header: the lines of --queries (one agent each), else the resolved brief
// fanned into --n angled copies.
func researchBriefs(top string, opts researchOpts) (briefs []string, source string, err error) {
	if opts.queries != "" {
		lines, rerr := readQueryLines(opts.queries)
		if rerr != nil {
			return nil, "", rerr
		}
		if len(lines) == 0 {
			return nil, "", fmt.Errorf("queries file %s has no non-empty lines", opts.queries)
		}
		return research.TaskBriefs("", lines, 0), fmt.Sprintf("%s (%d queries)", opts.queries, len(lines)), nil
	}
	briefPath, brief, rerr := spawn.ResolveBrief(top, opts.brief)
	if rerr != nil {
		return nil, "", rerr
	}
	return research.TaskBriefs(brief, nil, opts.n), briefPath, nil
}

// readQueryLines reads a queries file into its non-empty, trimmed lines.
func readQueryLines(path string) ([]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read queries %s: %w", path, err)
	}
	var lines []string
	for _, ln := range strings.Split(string(b), "\n") {
		if s := strings.TrimSpace(ln); s != "" {
			lines = append(lines, s)
		}
	}
	return lines, nil
}

// headlessLauncher builds the research.Launch seam over ag.Headless: it captures
// each agent's stream to the task's log file and takes NO lock (the research
// agent only reads the repo and writes under --out, so concurrent agents in one
// worktree don't contend). This is the one edge that forks a real agent; tests
// substitute a fake Launch instead.
func headlessLauncher(ag agent.Agent, model, effort string) research.Launch {
	return func(ctx context.Context, t research.Task) error {
		// Truncate, don't append: a re-run into the same out dir must give this
		// agent a fresh log, not blend its stream beneath a prior run's output.
		lf, err := os.OpenFile(t.LogPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			return fmt.Errorf("open research log %s: %w", t.LogPath, err)
		}
		defer func() { _ = lf.Close() }()
		opts := agent.Opts{Model: model, Effort: effort, Stdout: lf, Stderr: lf}
		return ag.Headless(ctx, t.Prompt, opts)
	}
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

	// schema (RFC §11.1) — FIRST check: statically validate the descriptor
	// (known schemaVersion, required fields, effort keys ∈ enum, legal prompt
	// deliveries, at-most-one-{value} render lists) so a malformed or
	// lying-at-the-value-level drop-in fails here, not at run time. A failing
	// schema check sets rep.OK=false, and newDoctorCmd's os.Exit(1) then fires.
	// A non-descriptor provider (future-proofing) is skipped, not failed.
	if problems, checked := agent.Validate(ag); !checked {
		add("schema", true, "not a descriptor-backed provider (skipped)")
	} else if len(problems) == 0 {
		add("schema", true, "descriptor valid")
	} else {
		add("schema", false, strings.Join(problems, "; "))
	}

	// origin/provenance (informational, never fails): whether this provider is a
	// trusted embedded built-in or a user drop-in, and for a drop-in the file to
	// edit — the debugging context a doctor run on one's own descriptor wants.
	if builtin, path, ok := agent.Provenance(ag); ok {
		if builtin {
			add("origin", true, "built-in (embedded)")
		} else {
			add("origin", true, "drop-in: "+path)
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
		l, lerr := lock.AcquireReentrant(top)
		if lerr != nil {
			return 1, lerr
		}
		defer func() { _ = l.Release() }()
	case extension.LockShared:
		l, lerr := lock.AcquireSharedReentrant(top)
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
