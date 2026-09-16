package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/jfb1121/bowt/internal/config"
	"github.com/jfb1121/bowt/internal/env"
	"github.com/jfb1121/bowt/internal/gate"
	"github.com/jfb1121/bowt/internal/lock"
	"github.com/jfb1121/bowt/internal/output"
	"github.com/jfb1121/bowt/internal/repo"
	"github.com/jfb1121/bowt/internal/run"
	"github.com/jfb1121/bowt/internal/state"
	"github.com/jfb1121/bowt/internal/worktree"
)

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
