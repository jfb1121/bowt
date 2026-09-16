package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/jfb1121/bowt/internal/agent"
	"github.com/jfb1121/bowt/internal/lock"
	"github.com/jfb1121/bowt/internal/output"
	"github.com/jfb1121/bowt/internal/repo"
	"github.com/jfb1121/bowt/internal/spawn"
	"github.com/jfb1121/bowt/internal/state"
)

func newSpawnCmd() *cobra.Command {
	var opts spawnOpts
	c := &cobra.Command{
		Use:   "spawn [brief]",
		Short: "launch a headless coding agent against a brief",
		Long: `Hand a fresh headless agent a written brief instead of doing the work in your
own session. spawn loads a versioned wrapper prompt (plan by default, --impl for
implementation), substitutes the brief, stamps a provenance line, then runs the
agent as a child process while holding the per-worktree lock for its lifetime.

Brief resolution (first match, unless a path is passed):
  subagent/PROMPT.md  →  subagent/*-prompt.md  →  PROMPT.md
subagent/FOLLOWUP.md is auto-appended when present.`,
		Example: `  bowt spawn                       # plan pass over subagent/PROMPT.md
  bowt spawn --impl                # implementation pass
  bowt spawn brief.md --model opus # explicit brief + model
  bowt spawn --print-prompt        # assemble + print, don't launch`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				opts.brief = args[0]
			}
			// --impl and --orch pick different wrapper roles; they cannot both apply.
			// (--orch --headless IS allowed — a nested orch-of-orch runs one level
			// down; only the human-attended ancestor acts on owner-only classes.)
			if opts.impl && opts.orch {
				return fmt.Errorf("--impl and --orch are mutually exclusive: pick one role (impl, or orchestrator)")
			}
			return cmdSpawn(opts)
		},
	}
	c.Flags().BoolVar(&opts.impl, "impl", false, "implementation pass (default is a plan + writeback pass)")
	c.Flags().BoolVar(&opts.orch, "orch", false, "orchestrator pass: decompose, delegate, gate, escalate (never writes code; interactive by default; --headless permitted for nested orch-of-orch)")
	c.Flags().StringVar(&opts.agent, "agent", "", "agent provider (claude, codex; default: $BOWT_AGENT or claude)")
	c.Flags().StringVar(&opts.model, "model", "", "agent model (alias opus/sonnet/haiku, or a full ID)")
	c.Flags().StringVar(&opts.effort, "effort", "", "agent reasoning effort (e.g. high)")
	c.Flags().BoolVar(&opts.printPrompt, "print-prompt", false, "assemble and print the prompt + provenance, then exit (no agent, no lock)")
	c.Flags().BoolVar(&opts.headless, "headless", false, "run non-interactively in the background: a detached supervisor holds the lock, records a lane, and captures output to a log (returns a lane id immediately)")
	c.Flags().StringVar(&opts.ticket, "ticket", "", "human ticket ref recorded on the lane (also seeds the lane id)")
	c.Flags().IntVar(&opts.wave, "wave", 0, "dispatch wave recorded on the lane (orchestration hint; not scheduled)")
	c.Flags().StringSliceVar(&opts.deps, "deps", nil, "lane ids this lane depends on, recorded on the lane (comma-separated)")
	return c
}

// spawnOpts carries the parsed flags for `bowt spawn`.
type spawnOpts struct {
	brief       string
	impl        bool
	orch        bool
	agent       string
	model       string
	effort      string
	printPrompt bool
	headless    bool
	ticket      string
	wave        int
	deps        []string
}

func cmdSpawn(opts spawnOpts) error {
	// Root the spawn at the current worktree (not the main repo): the brief,
	// the lock, and the writeback all belong to the checkout you stand in.
	top, err := repo.Toplevel("")
	if err != nil {
		return err
	}

	// Select the provider up front (flag → $BOWT_AGENT → claude); an
	// unknown agent is a hard error before any work.
	ag, err := agent.Select(opts.agent, os.Getenv)
	if err != nil {
		return err
	}
	caps := ag.Caps()

	briefPath, brief, err := spawn.ResolveBrief(top, opts.brief)
	if err != nil {
		return err
	}

	mode := spawn.ModePlan
	switch {
	case opts.impl:
		mode = spawn.ModeImpl
	case opts.orch:
		mode = spawn.ModeOrch
	}
	// The provider's memory file fills {{MEMORY_FILE}}; its name is stamped into
	// the provenance line so a reader knows which CLI produced the writeback.
	a, err := spawn.Assemble(mode, caps.Name, caps.MemoryFile, brief)
	if err != nil {
		return err
	}

	// Header + provenance are diagnostics (stderr): stdout is either the agent's
	// inherited stream or, under --print-prompt, the assembled prompt itself.
	output.Errf("spawn → %s", top)
	fmt.Fprintf(os.Stderr, "  brief: %s   mode: %s   agent: %s\n", briefPath, mode.Label(), caps.Name)
	fmt.Fprintf(os.Stderr, "  %s\n", a.Provenance)
	if !caps.SupportsHooks {
		// A downgrade the RFC says to surface at spawn, not discover later: this
		// lane runs without Edit/Write guardrail enforcement.
		output.Errf("warning: agent %q has no hook support — this lane runs without Edit/Write guardrails", caps.Name)
	}

	model := spawn.ResolveModel(opts.model, mode)
	effort := spawn.ResolveEffort(opts.effort, mode)
	fmt.Fprintf(os.Stderr, "  model: %s   effort: %s\n", orDefault(model), orDefault(effort))

	if opts.printPrompt {
		// The assembled prompt (provenance already prepended) is the data here.
		fmt.Print(a.Prompt)
		return nil
	}

	if opts.headless {
		// Headless is opt-in and guard-railed: refuse a provider with no
		// Edit/Write hook guardrail up front (a backgrounded skip-perms run with
		// no guardrail is exactly what RequireHeadless forbids).
		if err := agent.RequireHeadless(ag); err != nil {
			return err
		}
		return launchHeadless(top, opts, mode, caps, a, briefPath, brief, model, effort)
	}

	// Interactive (default, unchanged): spawn is a writer — take the EXCLUSIVE
	// per-worktree lock and hold it for the agent's lifetime by running the agent
	// as a child. defer releases on return.
	l, err := lock.Acquire(top)
	if err != nil {
		return err
	}
	defer func() { _ = l.Release() }()

	// Run the agent as a child with inherited stdio. On a non-zero exit, mirror
	// the child's exit code (matching the prior hardcoded path) rather than
	// masking it as bowt's own error.
	sopts := agent.Opts{Model: model, Effort: effort, Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr}
	if err := ag.Session(context.Background(), a.Prompt, sopts); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			os.Exit(ee.ExitCode())
		}
		return err
	}
	return nil
}

// launchHeadless is the LAUNCHER half of a backgrounded spawn. It resolves the
// lane's identity + placement, assembles the handoff spec, and re-execs a
// detached bowt supervisor — then it returns fast, WITHOUT holding the exclusive
// lock (the supervisor is the single lock owner, so there is no double-hold or
// TOCTOU window). It waits only for the supervisor to publish the lane row.
func launchHeadless(top string, opts spawnOpts, mode spawn.Mode, caps agent.Capabilities, a spawn.Assembled, briefPath, brief, model, effort string) error {
	repoName, err := repo.Name()
	if err != nil {
		return err
	}
	id, err := newLaneID(opts.ticket)
	if err != nil {
		return err
	}
	spec := spawn.LaneSpec{
		ID:            id,
		Ticket:        opts.ticket,
		Repo:          repoName,
		Branch:        repo.CurrentBranch(top),
		Worktree:      top,
		Agent:         caps.Name,
		Model:         model,
		Effort:        effort,
		Mode:          string(mode),
		PromptVersion: a.Version,
		PromptHash:    a.Hash,
		BriefPath:     briefPath,
		BriefHash:     spawn.BlobHash([]byte(brief)),
		WritebackDir:  spawn.DefaultWritebackDir,
		LogPath:       spawn.LaneLogPath(top, id),
		Wave:          opts.wave,
		Deps:          opts.deps,
		Prompt:        a.Prompt,
	}

	ls, err := state.OpenLanes()
	if err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		exe = os.Args[0] // best-effort fallback; re-exec still needs an argv0
	}
	return runHeadlessLaunch(osSpawner{}, ls, exe, spec, defaultLaunchConfig)
}

// spawner is the detach/re-exec seam: the one edge that actually forks a
// background process. A Fake lets the launcher's spec-write + readiness-poll
// logic be unit-tested WITHOUT forking (mirrors run.Runner).
type spawner interface {
	// spawn starts exe+args detached from the caller's terminal, in dir, with
	// stdout+stderr appended to logPath, and returns the child pid WITHOUT
	// waiting for it. The child outlives the caller.
	spawn(exe string, args []string, dir, logPath string) (int, error)
}

// osSpawner is the real spawner. Setsid detaches the child into its own session
// (no controlling terminal) on both darwin and linux; stdio goes to the log so
// the supervisor + agent output is captured; stdin is the null device (headless
// has no TTY). We Start but never Wait — the launcher exits and the child is
// reparented to init, which reaps it.
type osSpawner struct{}

func (osSpawner) spawn(exe string, args []string, dir, logPath string) (int, error) {
	lf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return 0, fmt.Errorf("open lane log %s: %w", logPath, err)
	}
	defer func() { _ = lf.Close() }() // the child dups the fd; our copy can close
	cmd := exec.Command(exe, args...)
	cmd.Dir = dir
	cmd.Stdin = nil // /dev/null: a headless supervisor has no interactive stdin
	cmd.Stdout = lf
	cmd.Stderr = lf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("start lane supervisor: %w", err)
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Release() // don't hold the child; it outlives us
	return pid, nil
}

// launchConfig bounds how long the launcher waits for the supervisor to publish
// the lane row before declaring a failed start.
type launchConfig struct {
	attempts int
	interval time.Duration
}

var defaultLaunchConfig = launchConfig{attempts: 100, interval: 50 * time.Millisecond}

// laneStarted is the launcher's stdout payload — agent-parseable (stdout=data).
type laneStarted struct {
	ID      string `json:"id"`
	LogPath string `json:"log_path"`
	Pid     int    `json:"pid"`
}

// runHeadlessLaunch is the testable launcher core: write the spec, fork the
// supervisor, poll for the row it publishes, then emit the lane id + log path.
// It never acquires the exclusive lock — the supervisor owns it.
func runHeadlessLaunch(sp spawner, ls state.LaneStore, exe string, spec spawn.LaneSpec, cfg launchConfig) error {
	specPath, err := spawn.WriteSpec(spec)
	if err != nil {
		return err
	}
	pid, err := sp.spawn(exe, spawn.SupervisorArgs(specPath), spec.Worktree, spec.LogPath)
	if err != nil {
		return err
	}
	// Bounded poll for the supervisor to INSERT the row (status planning|impl).
	for i := 0; i < cfg.attempts; i++ {
		if _, ok, err := ls.GetLane(spec.ID); err != nil {
			return err
		} else if ok {
			output.Errf("lane %s started (pid %d) — tail %s", spec.ID, pid, spec.LogPath)
			return output.Emit(laneStarted{ID: spec.ID, LogPath: spec.LogPath, Pid: pid})
		}
		time.Sleep(cfg.interval)
	}
	return fmt.Errorf("lane %s: supervisor did not publish the lane row within %s (see %s)",
		spec.ID, time.Duration(cfg.attempts)*cfg.interval, spec.LogPath)
}
