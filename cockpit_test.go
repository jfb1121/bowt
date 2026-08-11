package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/jfb1121/bowt/internal/spawn"
	"github.com/jfb1121/bowt/internal/state"
)

// openCockpitStores opens the worktree Store and LaneStore views over one temp
// DB file (both views share open(), so a single file backs both tables).
func openCockpitStores(t *testing.T) (state.Store, state.LaneStore) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	st, err := state.OpenAt(path)
	if err != nil {
		t.Fatal(err)
	}
	ls, err := state.OpenLanesAt(path)
	if err != nil {
		t.Fatal(err)
	}
	return st, ls
}

// worktreeWithArtifact makes a worktree dir with <wt>/subagent/writeback/<file>.
func worktreeWithArtifact(t *testing.T, file string) string {
	t.Helper()
	wt := t.TempDir()
	wb := filepath.Join(wt, spawn.DefaultWritebackDir)
	if err := os.MkdirAll(wb, 0o755); err != nil {
		t.Fatal(err)
	}
	if file != "" {
		if err := os.WriteFile(filepath.Join(wb, file), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return wt
}

// fakeProbe returns a probe func keyed by worktree path; unlisted paths are free.
func fakeProbe(held map[string]bool) func(string) (bool, error) {
	return func(key string) (bool, error) { return held[key], nil }
}

// TestReconcileLaneViewsSelfHeals exercises the shared cockpit projection over a
// real (temp) LaneStore + a fake lock: a running lane whose lock is free is
// reconciled from files AND the correction is persisted (self-heal); a running
// lane whose lock is held is left live; a terminal lane is untouched.
func TestReconcileLaneViewsSelfHeals(t *testing.T) {
	_, ls := openCockpitStores(t)

	orphan := worktreeWithArtifact(t, "STATUS.md") // impl finished, supervisor died
	live := worktreeWithArtifact(t, "STATUS.md")   // impl still running (lock held)

	mk := func(id, wt string, s state.Status) state.Lane {
		return state.Lane{ID: id, Repo: "demo", Branch: id, Worktree: wt,
			Status: s, PromptMode: "impl", WritebackDir: spawn.DefaultWritebackDir}
	}
	for _, l := range []state.Lane{
		mk("orphan", orphan, state.StatusImpl),
		mk("live", live, state.StatusImpl),
		mk("done", t.TempDir(), state.StatusDone),
	} {
		if err := ls.AddLane(l); err != nil {
			t.Fatal(err)
		}
	}

	lanes, err := ls.ListLanes("demo")
	if err != nil {
		t.Fatal(err)
	}
	views, err := reconcileLaneViews(ls, lanes, fakeProbe(map[string]bool{live: true}))
	if err != nil {
		t.Fatalf("reconcileLaneViews: %v", err)
	}

	byID := map[string]laneView{}
	for _, v := range views {
		byID[v.ID] = v
	}
	if v := byID["orphan"]; v.Status != state.StatusReview || !v.Reconciled {
		t.Errorf("orphan view = %q reconciled=%v; want review reconciled=true", v.Status, v.Reconciled)
	}
	if v := byID["live"]; v.Status != state.StatusImpl || v.Reconciled {
		t.Errorf("live view = %q reconciled=%v; want impl reconciled=false", v.Status, v.Reconciled)
	}
	if v := byID["done"]; v.Status != state.StatusDone || v.Reconciled {
		t.Errorf("done view = %q reconciled=%v; want done unchanged", v.Status, v.Reconciled)
	}

	// Self-heal: the orphan's corrected status is PERSISTED, not just displayed.
	got, ok, err := ls.GetLane("orphan")
	if err != nil || !ok {
		t.Fatalf("GetLane orphan: ok=%v err=%v", ok, err)
	}
	if got.Status != state.StatusReview {
		t.Errorf("orphan not self-healed in store: %q", got.Status)
	}
	// The live lane was NOT written (still impl).
	if got, _, _ := ls.GetLane("live"); got.Status != state.StatusImpl {
		t.Errorf("live lane should be untouched in store: %q", got.Status)
	}
}

// TestCmdStatusJoinsAndProjects asserts the status cockpit's join + JSON shape
// over fakes: a worktree with a lane carries the reconciled lane + gate verdict +
// lock state; a no-lane worktree still appears with an empty lanes array.
func TestCmdStatusJoinsAndProjects(t *testing.T) {
	st, ls := openCockpitStores(t)

	laned := worktreeWithArtifact(t, "STATUS.md")
	bare := t.TempDir()
	for _, wt := range []state.Worktree{
		{Repo: "demo", Branch: "slice/laned", Path: laned},
		{Repo: "demo", Branch: "slice/bare", Path: bare},
	} {
		if err := st.Add(wt); err != nil {
			t.Fatal(err)
		}
	}
	// A stranded impl lane on the laned worktree (lock will probe free).
	if err := ls.AddLane(state.Lane{
		ID: "lane-1", Repo: "demo", Branch: "slice/laned", Worktree: laned,
		Status: state.StatusImpl, PromptMode: "impl", WritebackDir: spawn.DefaultWritebackDir,
	}); err != nil {
		t.Fatal(err)
	}

	facts := func(wt state.Worktree) (wtFacts, error) {
		if wt.Branch == "slice/laned" {
			return wtFacts{Commit: "abc1234", Dirty: false, GateVerdict: "pass", GateCommit: "abc1234"}, nil
		}
		return wtFacts{Commit: "def5678", Dirty: true}, nil
	}
	probe := fakeProbe(map[string]bool{bare: true}) // bare's lock held; laned free

	out := captureStdout(t, func() {
		if err := cmdStatus(st, ls, "demo", probe, facts, true); err != nil {
			t.Fatalf("cmdStatus: %v", err)
		}
	})

	var views []worktreeView
	if err := json.Unmarshal([]byte(out), &views); err != nil {
		t.Fatalf("unmarshal status JSON: %v\n%s", err, out)
	}
	if len(views) != 2 {
		t.Fatalf("want 2 worktree rows, got %d", len(views))
	}
	byBranch := map[string]worktreeView{}
	for _, v := range views {
		byBranch[v.Branch] = v
	}

	laneWv := byBranch["slice/laned"]
	if laneWv.Commit != "abc1234" || laneWv.GateVerdict != "pass" || laneWv.LockHeld {
		t.Errorf("laned row facts/lock wrong: %+v", laneWv)
	}
	if len(laneWv.Lanes) != 1 {
		t.Fatalf("laned worktree should carry 1 lane, got %d", len(laneWv.Lanes))
	}
	// The lane was reconciled at read time (impl → review) and marked reconciled.
	if l := laneWv.Lanes[0]; l.Status != state.StatusReview || !l.Reconciled {
		t.Errorf("joined lane not reconciled: %q reconciled=%v", l.Status, l.Reconciled)
	}

	bareWv := byBranch["slice/bare"]
	if !bareWv.Dirty || !bareWv.LockHeld {
		t.Errorf("bare row facts/lock wrong: %+v", bareWv)
	}
	if bareWv.Lanes == nil || len(bareWv.Lanes) != 0 {
		t.Errorf("no-lane worktree must project an empty (non-null) lanes array, got %v", bareWv.Lanes)
	}
}
