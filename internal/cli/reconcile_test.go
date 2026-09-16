package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jfb1121/bowt/internal/spawn"
	"github.com/jfb1121/bowt/internal/state"
)

// TestReconcileDecision is the exhaustive table for the G4 reconcile DECISION:
// {plan,impl} running × {lock held, lock free} × {artifact present/absent} ×
// {PAUSED or not}, plus every terminal status. It is pure — a writeback tempdir
// (artifact presence) + the comms arg are the only inputs — so no process, no
// real supervisor, no real lock is involved.
func TestReconcileDecision(t *testing.T) {
	tests := []struct {
		name        string
		status      state.Status // the stale stored status
		mode        string       // lane.PromptMode
		lockHeld    bool
		artifact    string // file to drop in the writeback dir ("" = none)
		comms       spawn.Comms
		want        state.Status
		wantChanged bool
	}{
		// running + lock FREE = candidate: reconstruct from files.
		{"impl free STATUS.md", state.StatusImpl, "impl", false, "STATUS.md", spawn.Comms{}, state.StatusReview, true},
		{"plan free PLAN.md", state.StatusPlanning, "plan", false, "PLAN.md", spawn.Comms{}, state.StatusPlanReview, true},
		{"impl free no artifact", state.StatusImpl, "impl", false, "", spawn.Comms{}, state.StatusFailed, true},
		{"plan free no artifact", state.StatusPlanning, "plan", false, "", spawn.Comms{}, state.StatusFailed, true},
		// The wrong artifact for the mode does not count (mirrors ArtifactStatus).
		{"impl free PLAN.md only", state.StatusImpl, "impl", false, "PLAN.md", spawn.Comms{}, state.StatusFailed, true},
		{"plan free STATUS.md only", state.StatusPlanning, "plan", false, "STATUS.md", spawn.Comms{}, state.StatusFailed, true},

		// PAUSED precedence: promotes a SUCCESSFUL terminal status to paused, but
		// never rescues a failed run (a died-mid-run agent stays failed).
		{"impl free STATUS.md paused", state.StatusImpl, "impl", false, "STATUS.md", spawn.Comms{Paused: true, PausedOn: "alice"}, state.StatusPaused, true},
		{"impl free no artifact paused", state.StatusImpl, "impl", false, "", spawn.Comms{Paused: true}, state.StatusFailed, true},

		// running + lock HELD = live lane: never touched.
		{"impl held STATUS.md", state.StatusImpl, "impl", true, "STATUS.md", spawn.Comms{}, state.StatusImpl, false},
		{"plan held", state.StatusPlanning, "plan", true, "", spawn.Comms{}, state.StatusPlanning, false},

		// A review lane (running) reconciles idempotently when its artifact is still
		// present (unchanged → no self-heal write) but conservatively to failed if
		// the STATUS.md has since vanished.
		{"review free STATUS.md", state.StatusReview, "impl", false, "STATUS.md", spawn.Comms{}, state.StatusReview, false},
		{"review free no artifact", state.StatusReview, "impl", false, "", spawn.Comms{}, state.StatusFailed, true},

		// Terminal statuses are never candidates, lock free or not.
		{"done free", state.StatusDone, "impl", false, "STATUS.md", spawn.Comms{}, state.StatusDone, false},
		{"failed free", state.StatusFailed, "impl", false, "", spawn.Comms{}, state.StatusFailed, false},
		{"plan-review free", state.StatusPlanReview, "plan", false, "PLAN.md", spawn.Comms{}, state.StatusPlanReview, false},
		{"paused free", state.StatusPaused, "impl", false, "STATUS.md", spawn.Comms{}, state.StatusPaused, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wt := t.TempDir()
			wb := filepath.Join(wt, spawn.DefaultWritebackDir)
			if err := os.MkdirAll(wb, 0o755); err != nil {
				t.Fatal(err)
			}
			if tt.artifact != "" {
				if err := os.WriteFile(filepath.Join(wb, tt.artifact), []byte("x"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			lane := state.Lane{
				ID: "l", Status: tt.status, PromptMode: tt.mode,
				Worktree: wt, WritebackDir: spawn.DefaultWritebackDir,
			}
			got, changed, err := reconcile(lane, tt.lockHeld, tt.comms)
			if err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			if got.Status != tt.want {
				t.Errorf("status = %q; want %q", got.Status, tt.want)
			}
			if changed != tt.wantChanged {
				t.Errorf("changed = %v; want %v", changed, tt.wantChanged)
			}
		})
	}
}

// TestReconcileFoldsComms confirms a reconciled candidate carries the SAME comms
// scalars the live supervisor would have stored (via applyComms), not just the
// repaired status — so the self-heal write retires the whole terminal update.
func TestReconcileFoldsComms(t *testing.T) {
	wt := t.TempDir()
	wb := filepath.Join(wt, spawn.DefaultWritebackDir)
	if err := os.MkdirAll(wb, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wb, "STATUS.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	lane := state.Lane{ID: "l", Status: state.StatusImpl, PromptMode: "impl", Worktree: wt, WritebackDir: spawn.DefaultWritebackDir}
	comms := spawn.Comms{
		Escalated: true, EscalationNote: "needs owner",
		ReviewBlockers: 2, ReviewMajors: 1, ReviewMinors: 3,
	}
	got, changed, err := reconcile(lane, false, comms)
	if err != nil || !changed {
		t.Fatalf("reconcile: changed=%v err=%v", changed, err)
	}
	if got.Status != state.StatusReview {
		t.Errorf("status = %q; want review", got.Status)
	}
	if !got.Escalated || got.EscalationNote != "needs owner" {
		t.Errorf("escalation not folded: %+v", got)
	}
	if got.ReviewBlockers != 2 || got.ReviewMajors != 1 || got.ReviewMinors != 3 {
		t.Errorf("review counts not folded: %d/%d/%d", got.ReviewBlockers, got.ReviewMajors, got.ReviewMinors)
	}
}
