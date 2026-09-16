package main

import (
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
)

func newGateCmd() *cobra.Command {
	var scope string
	c := &cobra.Command{
		Use:   "gate [--scope <full|paths>]",
		Short: "run the repo's verification hook and record a machine-readable verdict",
		Long: `Run the current worktree's gate hook under the exclusive per-worktree lock and
write a machine-readable verdict to <worktree>/.bowt/gate.json.

The repo owns what "gating" means via <configDir>/gate.sh (.bowt/): a
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

	// Build the per-worktree env (BOWT_* + config vars) the hook sees — the
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

// gateSkipped marks a land that bypassed verification via --no-gate; any other
// verdict value is a real gate.Status ("pass").
const gateSkipped = "skipped"
