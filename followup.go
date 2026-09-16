package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/jfb1121/bowt/internal/agent"
	"github.com/jfb1121/bowt/internal/spawn"
	"github.com/jfb1121/bowt/internal/state"
)

// newLaneFollowupCmd registers `bowt lane followup <id>`: feedback → re-spawn.
func newLaneFollowupCmd() *cobra.Command {
	var msg, file string
	c := &cobra.Command{
		Use:   "followup <id> [-m <msg> | -f <file>]",
		Short: "feed a message back to a lane and re-spawn it headless",
		Long: `Write a follow-up message to the lane's worktree (subagent/FOLLOWUP.md, which
the next spawn auto-appends to the brief), bump the lane's attempt, recompute its
provenance, reset its status to the running phase, clear the prior attempt's
comms, and re-spawn the agent headless against the same lane.

The message is either inline (-m) or read from a file (-f); pass exactly one. The
prior writeback files remain on disk as the agent's context for the new pass.`,
		Example: `  bowt lane followup my-lane-ab12 -m "address the review blockers, then re-run gate"
  bowt lane followup my-lane-ab12 -f notes/followup.md`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmdLaneFollowup(args[0], msg, file)
		},
	}
	c.Flags().StringVarP(&msg, "message", "m", "", "inline follow-up message")
	c.Flags().StringVarP(&file, "file", "f", "", "read the follow-up message from a file")
	return c
}

// cmdLaneFollowup is the `bowt lane followup <id>` entrypoint. It resolves the
// message (inline -m or a -f file, exactly one), the lane row, and the agent,
// then hands the real seams to runFollowup. It never launches a real agent
// itself — the re-spawn goes through the same launcher a fresh headless spawn
// uses (the supervisor owns the lock and the agent process).
func cmdLaneFollowup(id, msg, file string) error {
	message, err := resolveFollowupMessage(msg, file)
	if err != nil {
		return err
	}

	ls, err := state.OpenLanes()
	if err != nil {
		return err
	}
	lane, ok, err := ls.GetLane(id)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("lane %q not found", id)
	}
	if fi, err := os.Stat(lane.Worktree); err != nil || !fi.IsDir() {
		return fmt.Errorf("lane %q: worktree %s is gone — cannot re-spawn", id, lane.Worktree)
	}

	// The follow-up re-spawns headless: refuse a provider without the Edit/Write
	// guardrail up front, exactly as `spawn --headless` does.
	ag, err := agent.New(lane.Agent)
	if err != nil {
		return err
	}
	if err := agent.RequireHeadless(ag); err != nil {
		return err
	}

	exe, err := os.Executable()
	if err != nil {
		exe = os.Args[0]
	}
	return runFollowup(followupDeps{sp: osSpawner{}, ls: ls, exe: exe, caps: ag.Caps(), cfg: defaultLaunchConfig}, lane, message)
}

// resolveFollowupMessage returns the follow-up text from exactly one of -m / -f.
// Passing both, neither, or an empty message is a hard error (a follow-up with
// nothing to say would re-spawn the agent with no new guidance).
func resolveFollowupMessage(msg, file string) (string, error) {
	switch {
	case msg != "" && file != "":
		return "", fmt.Errorf("pass exactly one of -m/--message or -f/--file, not both")
	case msg != "":
		return msg, nil
	case file != "":
		b, err := os.ReadFile(file)
		if err != nil {
			return "", fmt.Errorf("read follow-up file %s: %w", file, err)
		}
		if len(strings.TrimSpace(string(b))) == 0 {
			return "", fmt.Errorf("follow-up file %s is empty", file)
		}
		return string(b), nil
	default:
		return "", fmt.Errorf("provide a follow-up message with -m/--message or -f/--file")
	}
}

// followupDeps bundles the seams runFollowup drives so its logic is unit-tested
// with a fake spawner + LaneStore and no real agent (caps is pure data).
type followupDeps struct {
	sp   spawner
	ls   state.LaneStore
	exe  string
	caps agent.Capabilities
	cfg  launchConfig
}

// runFollowup is the testable core of `lane followup`: (1) write FOLLOWUP.md to
// the worktree (ResolveBrief auto-appends it on the next spawn); (2) re-resolve
// the brief (now carrying the follow-up) and re-Assemble to recompute the
// provenance line; (3) bump attempt, update provenance/brief hash, reset status
// to the running phase, clear the prior attempt's terminal comms, and point the
// row at this attempt's log in ONE UpdateLane; (4) re-spawn headless through the
// SAME launcher a fresh spawn uses, reusing the lane id (the supervisor's
// publish reuses the row rather than re-INSERTing it).
func runFollowup(d followupDeps, lane state.Lane, message string) error {
	// (1) FOLLOWUP.md — the handshake input the next spawn appends to the brief.
	subagentDir := filepath.Join(lane.Worktree, "subagent")
	if err := os.MkdirAll(subagentDir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", subagentDir, err)
	}
	if err := os.WriteFile(filepath.Join(subagentDir, "FOLLOWUP.md"), []byte(message), 0o644); err != nil {
		return fmt.Errorf("write FOLLOWUP.md: %w", err)
	}

	// (2) Re-resolve the brief (ResolveBrief auto-appends the FOLLOWUP.md we just
	// wrote) and re-Assemble so a prompt-version bump since the last attempt is
	// reflected in the recomputed provenance.
	mode := spawn.Mode(lane.PromptMode)
	briefPath, brief, err := spawn.ResolveBrief(lane.Worktree, "")
	if err != nil {
		return err
	}
	a, err := spawn.Assemble(mode, d.caps.Name, d.caps.MemoryFile, brief)
	if err != nil {
		return err
	}

	// (3) Bump attempt + provenance, reset status to the running phase, clear the
	// prior attempt's terminal comms, retarget the log — one UpdateLane.
	lane.Attempt++
	lane.PromptVersion = a.Version
	lane.PromptHash = a.Hash
	lane.BriefPath = briefPath
	lane.BriefHash = spawn.BlobHash([]byte(brief))
	lane.Status = runningStatus(lane.PromptMode)
	lane.Escalated = false
	lane.EscalationNote = ""
	lane.PausedOn = ""
	lane.ReviewBlockers, lane.ReviewMajors, lane.ReviewMinors = 0, 0, 0
	lane.LogPath = spawn.LaneLogPathAttempt(lane.Worktree, lane.ID, lane.Attempt)
	if err := d.ls.UpdateLane(lane); err != nil {
		return err
	}

	// (4) Re-spawn headless through the launcher, reusing the lane id + row.
	spec := spawn.LaneSpec{
		ID:            lane.ID,
		Ticket:        lane.Ticket,
		Repo:          lane.Repo,
		Branch:        lane.Branch,
		Worktree:      lane.Worktree,
		Agent:         lane.Agent,
		Model:         lane.Model,
		Effort:        spawn.ResolveEffort("", mode),
		Mode:          lane.PromptMode,
		PromptVersion: a.Version,
		PromptHash:    a.Hash,
		BriefPath:     briefPath,
		BriefHash:     lane.BriefHash,
		WritebackDir:  lane.WritebackDir,
		LogPath:       lane.LogPath,
		Wave:          lane.Wave,
		Deps:          lane.Deps,
		Prompt:        a.Prompt,
	}
	return runHeadlessLaunch(d.sp, d.ls, d.exe, spec, d.cfg)
}
