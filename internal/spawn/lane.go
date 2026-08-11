package spawn

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// LaneRunCommand is the hidden verb the launcher re-execs to become the
// detached supervisor: `bowt _lane-run <spec-path>`. It is not a user command —
// it is bowt handing work to a copy of itself that outlives the launcher and
// holds the worktree lock for the agent's whole lifetime.
const LaneRunCommand = "_lane-run"

// bowtDir is the per-worktree artifact directory (shared with gate.json).
const bowtDir = ".bowt"

// DefaultWritebackDir is where the agent writes its handshake artifacts
// (PLAN.md / STATUS.md / VALIDATION.md); recorded on the lane, content on disk.
const DefaultWritebackDir = "subagent/writeback"

// LaneSpec is the launcher→supervisor handoff. The two halves are different
// processes: the launcher validates the brief, assembles the prompt, and
// computes provenance (failing fast BEFORE the fork); the supervisor owns the
// lock and BOTH database writes (the INSERT on start and the terminal UPDATE).
// Everything the supervisor needs to do that travels in this struct, written to
// a spec file the supervisor reads back by path — the prompt can be large, so a
// file beats an argv.
type LaneSpec struct {
	ID       string `json:"id"`
	Ticket   string `json:"ticket"`
	Repo     string `json:"repo"`
	Branch   string `json:"branch"`
	Worktree string `json:"worktree"`

	Agent  string `json:"agent"`
	Model  string `json:"model"`
	Effort string `json:"effort"`
	Mode   string `json:"mode"` // "plan" | "impl"

	PromptVersion string `json:"prompt_version"`
	PromptHash    string `json:"prompt_hash"`
	BriefPath     string `json:"brief_path"`
	BriefHash     string `json:"brief_hash"`

	WritebackDir string `json:"writeback_dir"`
	LogPath      string `json:"log_path"`

	Wave int      `json:"wave"`
	Deps []string `json:"deps"`

	// Prompt is the fully assembled message (provenance line prepended) the
	// supervisor hands to the agent. The launcher computes it so a bad brief
	// fails the caller's command, not a detached process the caller can't see.
	Prompt string `json:"prompt"`
}

// LaneLogPath is where a lane's first captured stdout/stderr land:
// <worktree>/.bowt/lane-<id>.log. The orchestrator tails it with `tail -f`.
func LaneLogPath(worktree, id string) string {
	return LaneLogPathAttempt(worktree, id, 0)
}

// LaneLogPathAttempt is LaneLogPath for a specific attempt: attempt 0 keeps the
// base name (so a first spawn's log path is unchanged), and each `lane followup`
// re-spawn (attempt >= 1) gets its own <worktree>/.bowt/lane-<id>.<attempt>.log
// so a re-run's output never overwrites or interleaves with the prior attempt's.
func LaneLogPathAttempt(worktree, id string, attempt int) string {
	name := "lane-" + id + ".log"
	if attempt > 0 {
		name = fmt.Sprintf("lane-%s.%d.log", id, attempt)
	}
	return filepath.Join(worktree, bowtDir, name)
}

// SpecPath is where a lane's handoff spec is written:
// <worktree>/.bowt/lane-<id>.spec.json.
func SpecPath(worktree, id string) string {
	return filepath.Join(worktree, bowtDir, "lane-"+id+".spec.json")
}

// SupervisorArgs is the argv (after the bowt binary) the launcher re-execs to
// start the supervisor. Pure so the re-exec is asserted in a test without
// forking.
func SupervisorArgs(specPath string) []string {
	return []string{LaneRunCommand, specPath}
}

// WriteSpec marshals spec to its SpecPath (creating <worktree>/.bowt), and
// returns the path written. The supervisor reads it back with ReadSpecAt.
func WriteSpec(spec LaneSpec) (string, error) {
	dir := filepath.Join(spec.Worktree, bowtDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create %s: %w", dir, err)
	}
	b, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal lane spec: %w", err)
	}
	path := SpecPath(spec.Worktree, spec.ID)
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return "", fmt.Errorf("write lane spec %s: %w", path, err)
	}
	return path, nil
}

// ReadSpecAt reads and decodes a lane spec written by WriteSpec.
func ReadSpecAt(path string) (LaneSpec, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return LaneSpec{}, fmt.Errorf("read lane spec %s: %w", path, err)
	}
	var spec LaneSpec
	if err := json.Unmarshal(b, &spec); err != nil {
		return LaneSpec{}, fmt.Errorf("decode lane spec %s: %w", path, err)
	}
	return spec, nil
}
