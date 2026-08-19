package main

import (
	"strings"
	"testing"
	"time"

	"github.com/jfb1121/bowt/internal/spawn"
	"github.com/jfb1121/bowt/internal/state"
)

// waitEnv wires runLaneWait's seams over a real (temp) LaneStore, a keyed lock
// probe, and a virtual clock that only advances when sleep is called — so the
// poll loop is deterministic and instant, with NO real sleeping. onSleep, if set,
// runs on each sleep to advance the world (e.g. flip a lane's status) the way a
// live supervisor would between polls.
type waitEnv struct {
	ls      state.LaneStore
	held    map[string]bool
	clock   time.Time
	sleeps  int
	onSleep func(e *waitEnv)
}

func newWaitEnv(t *testing.T) *waitEnv {
	t.Helper()
	_, ls := openCockpitStores(t)
	return &waitEnv{ls: ls, held: map[string]bool{}}
}

func (e *waitEnv) deps() laneWaitDeps {
	return laneWaitDeps{
		ls:    e.ls,
		probe: func(key string) (bool, error) { return e.held[key], nil },
		now:   func() time.Time { return e.clock },
		sleep: func(d time.Duration) {
			e.sleeps++
			e.clock = e.clock.Add(d)
			if e.onSleep != nil {
				e.onSleep(e)
			}
		},
	}
}

func (e *waitEnv) add(t *testing.T, l state.Lane) {
	t.Helper()
	if err := e.ls.AddLane(l); err != nil {
		t.Fatal(err)
	}
}

func (e *waitEnv) setStatus(t *testing.T, id string, s state.Status) {
	t.Helper()
	l, ok, err := e.ls.GetLane(id)
	if err != nil || !ok {
		t.Fatalf("GetLane %q: ok=%v err=%v", id, ok, err)
	}
	l.Status = s
	if err := e.ls.UpdateLane(l); err != nil {
		t.Fatal(err)
	}
}

func lane(id string, s state.Status) state.Lane {
	return state.Lane{ID: id, Repo: "demo", Branch: id, Worktree: "/wt/" + id,
		Status: s, PromptMode: "impl", WritebackDir: spawn.DefaultWritebackDir, Created: time.Now().Add(-time.Hour)}
}

func viewByID(rows []laneView) map[string]laneView {
	m := make(map[string]laneView, len(rows))
	for _, v := range rows {
		m[v.ID] = v
	}
	return m
}

// (a) a lane that is impl on poll 1 and review (settled) on poll 2 returns the
// settled row. The status flip happens on sleep — as a live supervisor would.
func TestRunLaneWaitReturnsWhenSettled(t *testing.T) {
	e := newWaitEnv(t)
	e.add(t, lane("a", state.StatusImpl))
	e.held["/wt/a"] = true // lock held: no reconcile, purely a DB status change
	e.onSleep = func(e *waitEnv) { e.setStatus(t, "a", state.StatusReview) }

	rows, err := runLaneWait(e.deps(), []string{"a"}, 0, time.Second)
	if err != nil {
		t.Fatalf("runLaneWait: %v", err)
	}
	if e.sleeps != 1 {
		t.Errorf("sleeps = %d; want 1 (in-flight poll 1, settled poll 2)", e.sleeps)
	}
	if v := viewByID(rows)["a"]; v.Status != state.StatusReview {
		t.Errorf("row status = %q; want review", v.Status)
	}
}

// (b) an in-flight lane whose lock probe reports FREE is settled from files by
// reconcileForRead (dead supervisor) → wait returns on poll 1 with no sleep,
// proving it cannot hang on a dead supervisor.
func TestRunLaneWaitDeadSupervisorNoHang(t *testing.T) {
	e := newWaitEnv(t)
	wt := worktreeWithArtifact(t, "STATUS.md") // impl finished; supervisor died
	e.add(t, state.Lane{ID: "dead", Repo: "demo", Branch: "dead", Worktree: wt,
		Status: state.StatusImpl, PromptMode: "impl", WritebackDir: spawn.DefaultWritebackDir, Created: time.Now().Add(-time.Hour)})
	// probe leaves /wt free (not in held map) → reconcileForRead reads files.

	rows, err := runLaneWait(e.deps(), []string{"dead"}, 0, time.Second)
	if err != nil {
		t.Fatalf("runLaneWait: %v", err)
	}
	if e.sleeps != 0 {
		t.Errorf("sleeps = %d; want 0 (settled from files on poll 1)", e.sleeps)
	}
	v := viewByID(rows)["dead"]
	if v.Status != state.StatusReview || !v.Reconciled {
		t.Errorf("row = %q reconciled=%v; want review reconciled=true", v.Status, v.Reconciled)
	}
	// Self-heal: the reconciled status was persisted, not just displayed.
	if got, _, _ := e.ls.GetLane("dead"); got.Status != state.StatusReview {
		t.Errorf("dead lane not self-healed in store: %q", got.Status)
	}
}

// (c) --timeout elapses with a lane still in-flight → non-zero error naming it,
// and the (still in-flight) row is returned for printing.
func TestRunLaneWaitTimeout(t *testing.T) {
	e := newWaitEnv(t)
	e.add(t, lane("stuck", state.StatusImpl))
	e.held["/wt/stuck"] = true // live lock: never reconciles, stays in-flight

	rows, err := runLaneWait(e.deps(), []string{"stuck"}, time.Second, time.Second)
	if err == nil {
		t.Fatal("runLaneWait: want a timeout error, got nil")
	}
	if !strings.Contains(err.Error(), "stuck") || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("error = %q; want it to name the timed-out lane", err)
	}
	if v := viewByID(rows)["stuck"]; v.Status != state.StatusImpl {
		t.Errorf("row status = %q; want the still-in-flight impl row printed", v.Status)
	}
}

// (d) multiple ids: wait blocks until ALL settle and returns every settled row.
// lane a settles on poll 1 (dead-supervisor reconcile), b on poll 2.
func TestRunLaneWaitWaitsForAll(t *testing.T) {
	e := newWaitEnv(t)
	wtA := worktreeWithArtifact(t, "STATUS.md")
	e.add(t, state.Lane{ID: "a", Repo: "demo", Branch: "a", Worktree: wtA,
		Status: state.StatusImpl, PromptMode: "impl", WritebackDir: spawn.DefaultWritebackDir, Created: time.Now().Add(-time.Hour)})
	e.add(t, lane("b", state.StatusImpl))
	e.held["/wt/b"] = true // b's lock held; settles via DB flip on sleep
	e.onSleep = func(e *waitEnv) { e.setStatus(t, "b", state.StatusDone) }

	rows, err := runLaneWait(e.deps(), []string{"a", "b"}, 0, time.Second)
	if err != nil {
		t.Fatalf("runLaneWait: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d; want 2", len(rows))
	}
	by := viewByID(rows)
	if by["a"].Status != state.StatusReview {
		t.Errorf("a = %q; want review", by["a"].Status)
	}
	if by["b"].Status != state.StatusDone {
		t.Errorf("b = %q; want done", by["b"].Status)
	}
	// Order is preserved as requested.
	if rows[0].ID != "a" || rows[1].ID != "b" {
		t.Errorf("row order = %q,%q; want a,b", rows[0].ID, rows[1].ID)
	}
}

// (e) an unknown id is a hard error BEFORE any waiting (no sleep occurs).
func TestRunLaneWaitUnknownID(t *testing.T) {
	e := newWaitEnv(t)
	e.add(t, lane("known", state.StatusImpl))
	e.held["/wt/known"] = true

	_, err := runLaneWait(e.deps(), []string{"known", "ghost"}, 0, time.Second)
	if err == nil {
		t.Fatal("runLaneWait: want an error for the unknown id, got nil")
	}
	if !strings.Contains(err.Error(), "ghost") {
		t.Errorf("error = %q; want it to name the unknown lane", err)
	}
	if e.sleeps != 0 {
		t.Errorf("sleeps = %d; want 0 (fail before waiting)", e.sleeps)
	}
}

// A lane already settled (review) returns immediately with no sleep; review is a
// SETTLED state for waiting even though isRunning still counts it as running.
func TestRunLaneWaitAlreadySettled(t *testing.T) {
	e := newWaitEnv(t)
	e.add(t, lane("r", state.StatusReview))
	e.held["/wt/r"] = true

	rows, err := runLaneWait(e.deps(), []string{"r"}, 0, time.Second)
	if err != nil {
		t.Fatalf("runLaneWait: %v", err)
	}
	if e.sleeps != 0 {
		t.Errorf("sleeps = %d; want 0 (review is settled for wait)", e.sleeps)
	}
	if isInFlight(state.StatusReview) {
		t.Error("isInFlight(review) = true; review must be settled for wait")
	}
	if v := viewByID(rows)["r"]; v.Status != state.StatusReview {
		t.Errorf("row status = %q; want review", v.Status)
	}
}
