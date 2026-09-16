package main

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/jfb1121/bowt/internal/agent"
	"github.com/jfb1121/bowt/internal/lock"
	"github.com/jfb1121/bowt/internal/output"
	"github.com/jfb1121/bowt/internal/review"
	"github.com/jfb1121/bowt/internal/spawn"
	"github.com/jfb1121/bowt/internal/state"
)

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
