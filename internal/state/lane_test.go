package state

import (
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// mustOpenLanes gives each test its own throwaway SQLite file as a LaneStore.
func mustOpenLanes(t *testing.T) LaneStore {
	t.Helper()
	ls, err := OpenLanesAt(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	return ls
}

// sampleLane is a fully-populated lane so round-trip tests exercise every column.
func sampleLane(id string) Lane {
	return Lane{
		ID:             id,
		Ticket:         "ENG-1",
		Repo:           "demo",
		Branch:         "slice/x",
		Worktree:       "/wt/x",
		Status:         StatusImpl,
		Agent:          "claude",
		Model:          "claude-opus-4-8",
		PromptMode:     "impl",
		PromptVersion:  "3",
		PromptHash:     "abc1234",
		Attempt:        2,
		BriefPath:      "subagent/PROMPT.md",
		BriefHash:      "def5678",
		Wave:           1,
		Deps:           []string{"lane-a", "lane-b"},
		WritebackDir:   "subagent/writeback",
		LogPath:        "/wt/x/.bowt/lane-x.log",
		GateVerdict:    "pass",
		GateCommit:     "cafef00d",
		GatePath:       "/wt/x/.bowt/gate.json",
		ReviewBlockers: 1,
		ReviewMajors:   2,
		ReviewMinors:   3,
		Escalated:      true,
		EscalationNote: "needs a decision on X",
		PausedOn:       "jayesh",
	}
}

// TestLaneRoundTrip proves every column round-trips through OpenLanesAt: write a
// fully-populated lane, read it back field-for-field.
func TestLaneRoundTrip(t *testing.T) {
	ls := mustOpenLanes(t)
	in := sampleLane("lane-1")
	if err := ls.AddLane(in); err != nil {
		t.Fatal(err)
	}

	got, ok, err := ls.GetLane("lane-1")
	if err != nil || !ok {
		t.Fatalf("GetLane: ok=%v err=%v", ok, err)
	}
	// AddLane stamps Created/Updated when zero; copy them in before comparing.
	if got.Created.IsZero() || got.Updated.IsZero() {
		t.Fatalf("timestamps not stamped: created=%v updated=%v", got.Created, got.Updated)
	}
	in.Created, in.Updated = got.Created, got.Updated
	if !reflect.DeepEqual(in, got) {
		t.Fatalf("round-trip mismatch:\n in=%+v\ngot=%+v", in, got)
	}

	// A missing id is (zero, false, nil), never an error.
	if _, ok, err := ls.GetLane("nope"); err != nil || ok {
		t.Fatalf("GetLane(missing) = ok=%v err=%v; want false, nil", ok, err)
	}
}

// TestLaneDepsJSONRoundTrip pins the JSON TEXT column: a populated list survives,
// and both nil and empty-slice persist as the "" default and read back as nil.
func TestLaneDepsJSONRoundTrip(t *testing.T) {
	ls := mustOpenLanes(t)

	cases := map[string][]string{
		"with-deps":  {"a", "b", "c"},
		"nil-deps":   nil,
		"empty-deps": {},
	}
	for id, deps := range cases {
		l := sampleLane(id)
		l.Deps = deps
		if err := ls.AddLane(l); err != nil {
			t.Fatalf("AddLane(%s): %v", id, err)
		}
	}

	if got, _, _ := ls.GetLane("with-deps"); !reflect.DeepEqual(got.Deps, []string{"a", "b", "c"}) {
		t.Fatalf("with-deps = %v; want [a b c]", got.Deps)
	}
	for _, id := range []string{"nil-deps", "empty-deps"} {
		if got, _, _ := ls.GetLane(id); got.Deps != nil {
			t.Fatalf("%s deps = %v; want nil (empty persists as the '' default)", id, got.Deps)
		}
	}
}

// TestLanePersistsAcrossReopen proves the row hit disk — a map fake would pass
// the round-trip test but fail this one.
func TestLanePersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")

	ls1, err := OpenLanesAt(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := ls1.AddLane(sampleLane("persist")); err != nil {
		t.Fatal(err)
	}

	ls2, err := OpenLanesAt(path) // reopen the same file
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := ls2.GetLane("persist"); err != nil || !ok {
		t.Fatalf("lane did not persist across reopen: ok=%v err=%v", ok, err)
	}
}

// TestOpenLanesIdempotent proves re-Open on an existing DB is a no-op: the lane
// table's CREATE IF NOT EXISTS and the mode ALTER both survive a second Open.
func TestOpenLanesIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	if _, err := OpenLanesAt(path); err != nil {
		t.Fatalf("first open: %v", err)
	}
	// A worktree Store view over the same file must also open clean (shared open).
	if _, err := OpenAt(path); err != nil {
		t.Fatalf("Store view over lane DB: %v", err)
	}
	if _, err := OpenLanesAt(path); err != nil {
		t.Fatalf("second open must be a no-op: %v", err)
	}
}

// TestUpdateLaneTransitions walks a lane through its statuses and proves the
// scalar writeback fields update in place while the row identity is stable.
func TestUpdateLaneTransitions(t *testing.T) {
	ls := mustOpenLanes(t)
	l := sampleLane("trans")
	l.Status = StatusPlanning
	l.Deps = nil
	l.Escalated = false
	if err := ls.AddLane(l); err != nil {
		t.Fatal(err)
	}

	// planning → plan-review, stamping an escalation.
	l.Status = StatusPlanReview
	l.Escalated = true
	l.EscalationNote = "blocked on schema decision"
	if err := ls.UpdateLane(l); err != nil {
		t.Fatal(err)
	}
	got, _, _ := ls.GetLane("trans")
	if got.Status != StatusPlanReview || !got.Escalated || got.EscalationNote != "blocked on schema decision" {
		t.Fatalf("after plan-review: %+v", got)
	}

	// plan-review → impl, bumping attempt and provenance (followup semantics).
	l.Status = StatusImpl
	l.Attempt = 1
	l.PromptHash = "newhash"
	if err := ls.UpdateLane(l); err != nil {
		t.Fatal(err)
	}
	got, _, _ = ls.GetLane("trans")
	if got.Status != StatusImpl || got.Attempt != 1 || got.PromptHash != "newhash" {
		t.Fatalf("after impl: %+v", got)
	}

	// Updated must advance across an update.
	if !got.Updated.After(got.Created) && !got.Updated.Equal(got.Created) {
		t.Fatalf("Updated %v should be >= Created %v", got.Updated, got.Created)
	}
}

// TestUpdateLaneUnknownID refuses to silently insert a row that never existed.
func TestUpdateLaneUnknownID(t *testing.T) {
	ls := mustOpenLanes(t)
	err := ls.UpdateLane(sampleLane("ghost"))
	if !errors.Is(err, ErrLaneNotFound) {
		t.Fatalf("UpdateLane(unknown) = %v; want ErrLaneNotFound", err)
	}
}

// TestListLanesRepoIsolation proves ListLanes scopes by repo and orders by created.
func TestListLanesRepoIsolation(t *testing.T) {
	ls := mustOpenLanes(t)
	base := time.Now().UTC()
	for i, spec := range []struct {
		id, repo string
		created  time.Time
	}{
		{"a1", "a", base.Add(2 * time.Second)},
		{"a2", "a", base.Add(1 * time.Second)},
		{"b1", "b", base},
	} {
		l := sampleLane(spec.id)
		l.Repo = spec.repo
		l.Created = spec.created
		l.Updated = spec.created
		if err := ls.AddLane(l); err != nil {
			t.Fatalf("AddLane[%d]: %v", i, err)
		}
	}

	a, err := ls.ListLanes("a")
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != 2 || a[0].ID != "a2" || a[1].ID != "a1" {
		t.Fatalf("ListLanes(a) = %v; want [a2 a1] ordered by created", ids(a))
	}
	if b, _ := ls.ListLanes("b"); len(b) != 1 || b[0].ID != "b1" {
		t.Fatalf("ListLanes(b) = %v; want [b1]", ids(b))
	}
	if none, _ := ls.ListLanes("missing"); len(none) != 0 {
		t.Fatalf("ListLanes(missing) = %v; want empty", ids(none))
	}
}

func ids(ls []Lane) []string {
	out := make([]string, len(ls))
	for i, l := range ls {
		out[i] = l.ID
	}
	return out
}
