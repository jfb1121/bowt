package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jfb1121/bowt/internal/agent"
	"github.com/jfb1121/bowt/internal/spawn"
	"github.com/jfb1121/bowt/internal/state"
)

// seedLane writes a brief and a terminal lane row into a fresh store + worktree,
// returning the store, the worktree path, and the seeded lane. The lane carries
// stale comms so a followup can be shown to clear them.
func seedLane(t *testing.T) (state.LaneStore, string, state.Lane) {
	t.Helper()
	wt := t.TempDir()
	if err := os.MkdirAll(filepath.Join(wt, "subagent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "subagent", "PROMPT.md"), []byte("original brief body\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ls, err := state.OpenLanesAt(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	lane := state.Lane{
		ID: "lane-fu", Repo: "demo", Branch: "slice/x", Worktree: wt,
		Status: state.StatusReview, Agent: "claude", Model: "opus",
		PromptMode: "impl", BriefPath: "subagent/PROMPT.md", WritebackDir: spawn.DefaultWritebackDir,
		Attempt: 0, Escalated: true, EscalationNote: "stale", PausedOn: "someone",
		ReviewBlockers: 3, ReviewMajors: 2, ReviewMinors: 1,
	}
	if err := ls.AddLane(lane); err != nil {
		t.Fatal(err)
	}
	return ls, wt, lane
}

func followupCaps() agent.Capabilities {
	return agent.Capabilities{Name: "claude", MemoryFile: "CLAUDE.md", SupportsHooks: true, SupportsHeadless: true}
}

// TestFollowupWritesResetsAndRespawns is the end-to-end contract of the
// `lane followup` core over the fake launcher/store seams: FOLLOWUP.md written,
// the row's attempt bumped, provenance recomputed, status reset to running, the
// stale comms cleared, and the re-spawn re-exec'd through the launcher — with NO
// real agent.
func TestFollowupWritesResetsAndRespawns(t *testing.T) {
	ls, wt, lane := seedLane(t)
	// publish=false: the row already exists (followup updated it), so the launcher
	// poll sees it immediately — the real supervisor's publish reuses the row.
	sp := &fakeSpawner{ls: ls, publish: false, pid: 77}

	const msg = "address the review blockers, then re-run gate"
	deps := followupDeps{sp: sp, ls: ls, exe: "/path/to/bowt", caps: followupCaps(), cfg: defaultLaunchConfig}
	if err := runFollowup(deps, lane, msg); err != nil {
		t.Fatalf("runFollowup: %v", err)
	}

	// FOLLOWUP.md written with the message.
	fu, err := os.ReadFile(filepath.Join(wt, "subagent", "FOLLOWUP.md"))
	if err != nil {
		t.Fatalf("FOLLOWUP.md not written: %v", err)
	}
	if string(fu) != msg {
		t.Errorf("FOLLOWUP.md = %q; want %q", fu, msg)
	}

	// The row: attempt bumped, status reset to running (impl), comms cleared,
	// provenance recomputed, log retargeted to this attempt.
	got, ok, _ := ls.GetLane(lane.ID)
	if !ok {
		t.Fatal("lane row vanished")
	}
	if got.Attempt != 1 {
		t.Errorf("Attempt = %d; want 1", got.Attempt)
	}
	if got.Status != state.StatusImpl {
		t.Errorf("Status = %q; want impl", got.Status)
	}
	if got.Escalated || got.EscalationNote != "" || got.PausedOn != "" {
		t.Errorf("stale comms not cleared: escalated=%v note=%q paused=%q", got.Escalated, got.EscalationNote, got.PausedOn)
	}
	if got.ReviewBlockers != 0 || got.ReviewMajors != 0 || got.ReviewMinors != 0 {
		t.Errorf("stale review counts not cleared: %d/%d/%d", got.ReviewBlockers, got.ReviewMajors, got.ReviewMinors)
	}
	if got.PromptVersion == "" || got.PromptHash == "" {
		t.Errorf("provenance not recomputed: version=%q hash=%q", got.PromptVersion, got.PromptHash)
	}
	if want := spawn.LaneLogPathAttempt(wt, lane.ID, 1); got.LogPath != want {
		t.Errorf("LogPath = %q; want %q", got.LogPath, want)
	}

	// The re-spawn went through the launcher exactly once, re-exec'ing the hidden
	// supervisor verb with this lane's spec.
	if len(sp.calls) != 1 {
		t.Fatalf("spawn called %d times; want 1", len(sp.calls))
	}
	c := sp.calls[0]
	if len(c.args) != 2 || c.args[0] != spawn.LaneRunCommand || c.args[1] != spawn.SpecPath(wt, lane.ID) {
		t.Errorf("re-exec argv = %v; want [%s %s]", c.args, spawn.LaneRunCommand, spawn.SpecPath(wt, lane.ID))
	}

	// The assembled prompt in the spec carries the follow-up (ResolveBrief's
	// auto-append flowed through Assemble).
	spec, err := spawn.ReadSpecAt(spawn.SpecPath(wt, lane.ID))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(spec.Prompt, msg) {
		t.Error("spec prompt does not carry the follow-up message")
	}
	if !strings.Contains(spec.Prompt, "original brief body") {
		t.Error("spec prompt lost the original brief")
	}
}

// TestResolveFollowupMessage covers the -m/-f selection: exactly one, never both
// or neither, and a non-empty file.
func TestResolveFollowupMessage(t *testing.T) {
	dir := t.TempDir()
	full := filepath.Join(dir, "fu.md")
	if err := os.WriteFile(full, []byte("from file\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(dir, "empty.md")
	if err := os.WriteFile(empty, []byte("  \n"), 0o644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		msg     string
		file    string
		want    string
		wantErr bool
	}{
		{"inline", "hi", "", "hi", false},
		{"file", "", full, "from file\n", false},
		{"both", "hi", full, "", true},
		{"neither", "", "", "", true},
		{"missing file", "", filepath.Join(dir, "nope.md"), "", true},
		{"empty file", "", empty, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveFollowupMessage(tt.msg, tt.file)
			if tt.wantErr {
				if err == nil {
					t.Fatal("want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %q; want %q", got, tt.want)
			}
		})
	}
}

// TestApplyCommsPrecedence pins the status precedence: a pause promotes a
// successful terminal status to paused but never rescues a failed run; an
// escalation only sets the flag; review counts are always recorded.
func TestApplyCommsPrecedence(t *testing.T) {
	t.Run("pause promotes review to paused", func(t *testing.T) {
		lane := state.Lane{Status: state.StatusReview}
		applyComms(&lane, spawn.Comms{Paused: true, PausedOn: "alice"})
		if lane.Status != state.StatusPaused || lane.PausedOn != "alice" {
			t.Fatalf("status=%q paused=%q; want paused/alice", lane.Status, lane.PausedOn)
		}
	})
	t.Run("pause does not rescue failed", func(t *testing.T) {
		lane := state.Lane{Status: state.StatusFailed}
		applyComms(&lane, spawn.Comms{Paused: true, PausedOn: "alice"})
		if lane.Status != state.StatusFailed {
			t.Fatalf("status=%q; want failed (a crashed run stays failed)", lane.Status)
		}
		if lane.PausedOn != "alice" {
			t.Errorf("PausedOn still recorded even when failed; got %q", lane.PausedOn)
		}
	})
	t.Run("escalate flags but keeps phase", func(t *testing.T) {
		lane := state.Lane{Status: state.StatusPlanReview}
		applyComms(&lane, spawn.Comms{Escalated: true, EscalationNote: "why"})
		if lane.Status != state.StatusPlanReview {
			t.Fatalf("status=%q; want plan-review (escalate must not move the phase)", lane.Status)
		}
		if !lane.Escalated || lane.EscalationNote != "why" {
			t.Errorf("escalation not recorded: %v %q", lane.Escalated, lane.EscalationNote)
		}
	})
	t.Run("review counts recorded", func(t *testing.T) {
		lane := state.Lane{Status: state.StatusReview}
		applyComms(&lane, spawn.Comms{ReviewBlockers: 1, ReviewMajors: 2, ReviewMinors: 3})
		if lane.ReviewBlockers != 1 || lane.ReviewMajors != 2 || lane.ReviewMinors != 3 {
			t.Fatalf("counts = %d/%d/%d; want 1/2/3", lane.ReviewBlockers, lane.ReviewMajors, lane.ReviewMinors)
		}
	})
}
