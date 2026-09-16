package main

import (
	"path/filepath"

	"github.com/jfb1121/bowt/internal/review"
	"github.com/jfb1121/bowt/internal/spawn"
	"github.com/jfb1121/bowt/internal/state"
)

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
