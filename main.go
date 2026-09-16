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
