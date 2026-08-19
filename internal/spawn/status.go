package spawn

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/jfb1121/bowt/internal/state"
)

// TerminalStatus maps a finished headless run to the lane's terminal status,
// from files-as-truth: the child's exit code plus which writeback artifact the
// agent actually produced in writebackDir. It is PURE (a filesystem stat is its
// only input beyond the args) so the supervisor's completion logic is table-
// tested without a process.
//
//   - Any non-zero exit → failed. A crashed/errored agent is never landable,
//     regardless of what files it left behind.
//   - exit 0, plan mode → plan-review IF PLAN.md is present (the plan agent's
//     contract), else failed: a clean exit with no plan is a silent no-op, which
//     is a failure, not a pass.
//   - exit 0, impl mode → review IF STATUS.md is present, else failed (same
//     reasoning: success is defined by the writeback artifact, not exit 0 alone).
//
// The ESCALATE/PAUSE scan over the artifact bodies is deferred to G3; this
// function only decides the phase from exit + presence.
func TerminalStatus(mode string, exitCode int, writebackDir string, since time.Time) (state.Status, error) {
	if exitCode != 0 {
		return state.StatusFailed, nil
	}
	return ArtifactStatus(mode, writebackDir, since)
}

// ArtifactStatus is the FILE-PRESENCE half of the completion contract: the
// terminal status the writeback artifacts imply, independent of any exit code.
// It is shared by TerminalStatus (the live path, which first folds in the exit
// code above) and by the G4 reconciler (the SIGKILL path, where the supervisor's
// exit code is gone and files are the only evidence) so a live verdict and a
// reconciled verdict can never disagree.
//
//   - plan mode → plan-review IF PLAN.md is present, else failed: a clean run
//     with no plan is a silent no-op, which is a failure, not a pass.
//   - impl mode → review IF STATUS.md is present, else failed (same reasoning:
//     success is defined by the writeback artifact, never by exit alone).
//
// It is conservative by construction: it never yields `done` or a `pass` verdict
// — the absence of the expected artifact is a `failed`, so a reconciler built on
// it cannot fabricate success for a lane whose agent died mid-run.
func ArtifactStatus(mode string, writebackDir string, since time.Time) (state.Status, error) {
	switch Mode(mode) {
	case ModePlan:
		if freshFile(filepath.Join(writebackDir, "PLAN.md"), since) {
			return state.StatusPlanReview, nil
		}
		return state.StatusFailed, nil
	case ModeImpl, ModeOrch:
		// orch mirrors impl for now: a written STATUS.md → review, else failed.
		// Orch-specific terminal semantics (done-when-children-done, driven by the
		// wave/deps scheduler) is a later scheduler-slice decision; until that lands
		// an orchestrator writeback follows the same file-presence contract as impl.
		if freshFile(filepath.Join(writebackDir, "STATUS.md"), since) {
			return state.StatusReview, nil
		}
		return state.StatusFailed, nil
	default:
		return "", fmt.Errorf("terminal status: unknown mode %q (want %q, %q, or %q)", mode, ModePlan, ModeImpl, ModeOrch)
	}
}

// freshFile reports whether p is an existing regular file THIS lane produced —
// a regular file (not a directory) last modified no earlier than since, which
// callers set to the lane's creation time.
//
// The mtime half is not belt-and-braces, it is the whole point. Worktrees are
// reused, so a writeback directory routinely still holds the previous
// occupant's PLAN.md/STATUS.md. Bare existence therefore reports a lane as
// plan-review/review — a COMPLETED run — even when its agent died in its first
// second having written nothing. That is a fabricated success, and it is
// exactly what this function's callers promise cannot happen. An artifact older
// than the lane belongs to someone else, so it reads as absent and the lane
// lands `failed`, which is the truthful answer.
//
// A zero `since` disables the check, so a caller with no start time keeps the
// old presence-only behaviour rather than silently failing every lane.
func freshFile(p string, since time.Time) bool {
	fi, err := os.Stat(p)
	if err != nil || fi.IsDir() {
		return false
	}
	return since.IsZero() || !fi.ModTime().Before(since)
}
