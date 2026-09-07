// Package research fans out headless research agents over a set of tasks. It is
// spawn for *research* rather than code: each agent does web research
// (WebSearch/WebFetch) and writes ONE findings file to a per-agent out path — no
// gate, no commit, no PR. The deliverable is the written findings.
//
// Unlike a headless spawn lane, a research agent is a transient child that only
// reads the repo and writes under the out dir, so it takes NO per-worktree lock:
// N agents running in the same worktree would otherwise serialize on that one
// lock. The single load-bearing bound is Concurrency — the host OOMs past a
// handful of concurrent agents, so Run never launches more than that many at
// once (a counting semaphore over the task list, mirroring internal/review).
package research

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// DefaultConcurrency is the max agents run at once when the caller sets none. It
// is deliberately low: the host OOMs past ~2-3 concurrent agents, so this is a
// safety floor, not a throughput target.
const DefaultConcurrency = 2

// DefaultOutDir is the out directory (relative to the worktree) when --out is
// omitted: each agent writes <DefaultOutDir>/<id>.md there.
const DefaultOutDir = "research-out"

// SynthesisFile is the findings file the optional synthesis agent writes.
const SynthesisFile = "SYNTHESIS.md"

// Task is one research agent to launch: its id (findings filename stem), the
// fully assembled prompt (with the out path already substituted in), and the
// absolute paths it writes to. LogPath captures the child's stream.
type Task struct {
	ID      string
	Prompt  string
	OutPath string
	LogPath string
}

// Launch runs one research task to completion: a headless agent that does web
// research and writes its findings to Task.OutPath. It matches ag.Headless's
// contract (the subscription child-process path). It is the ONLY seam that
// forks a real agent, so tests fake it — recording concurrency and writing a
// fixture out file — with no real Claude process and no network.
type Launch func(ctx context.Context, t Task) error

// Runner drives the bounded fan-out and the optional synthesis pass. It holds no
// git or lock state (research agents take no lock); the command layer builds it
// with the resolved out dir, concurrency bound, and Launch.
type Runner struct {
	OutDir      string // absolute out directory
	Concurrency int    // max agents at once (<=0 means DefaultConcurrency)
	Launch      Launch // the fan-out primitive
	Synthesize  bool   // run the final synthesis agent over the written findings

	// SynthTask builds the synthesis task from the written findings paths and the
	// synthesis out path. Injected so tests drive synthesis without the real
	// prompt text. Only consulted when Synthesize is set and >=1 finding exists.
	SynthTask func(findingPaths []string, outPath, logPath string) Task

	// Log receives human diagnostics (stderr); nil discards them. It may be called
	// concurrently from the fan-out goroutines, so an implementation writing to a
	// shared sink must synchronize.
	Log func(format string, args ...any)
}

// Result is the machine-readable outcome: which findings files landed, which
// tasks were launched but wrote nothing, and the synthesis outcome.
type Result struct {
	Tasks       int      `json:"tasks"`
	OutDir      string   `json:"out_dir"`
	Written     []string `json:"written"`
	Missing     []string `json:"missing,omitempty"`
	Synthesized bool     `json:"synthesized"`
	SynthPath   string   `json:"synthesis_path,omitempty"`
}

func (r *Runner) logf(format string, args ...any) {
	if r.Log != nil {
		r.Log(format, args...)
	}
}

// concurrency returns the effective bound, applying the default floor.
func (r *Runner) concurrency() int {
	if r.Concurrency <= 0 {
		return DefaultConcurrency
	}
	return r.Concurrency
}

// Run fans the tasks out through Launch bounded by Concurrency, then collects
// the findings files that landed and (when Synthesize is set and at least one
// finding exists) runs one final agent to write SYNTHESIS.md. A returned error
// means the fan-out could not be set up (e.g. the out dir is unwritable); a task
// whose agent failed simply leaves no findings file and is reported in Missing —
// research is best-effort per task, never all-or-nothing.
func (r *Runner) Run(ctx context.Context, tasks []Task) (*Result, error) {
	if err := os.MkdirAll(r.OutDir, 0o755); err != nil {
		return nil, fmt.Errorf("create %s: %w", r.OutDir, err)
	}

	// Fan out, bounded by a counting semaphore. This bound is load-bearing (OOM
	// protection): the buffered channel admits at most `max` goroutines into the
	// Launch call at once, and the next starts only as one finishes. Each agent
	// writes only its own out/log file, so nothing shared is mutated and the
	// collection below reads results from disk after all goroutines join.
	max := r.concurrency()
	sem := make(chan struct{}, max)
	var wg sync.WaitGroup
	for _, t := range tasks {
		wg.Add(1)
		sem <- struct{}{}
		go func(t Task) {
			defer wg.Done()
			defer func() { <-sem }()
			r.runOne(ctx, t)
		}(t)
	}
	wg.Wait()

	res := &Result{Tasks: len(tasks), OutDir: r.OutDir}
	for _, t := range tasks {
		if fileExists(t.OutPath) {
			res.Written = append(res.Written, t.OutPath)
		} else {
			res.Missing = append(res.Missing, t.OutPath)
		}
	}

	if r.Synthesize {
		if len(res.Written) == 0 {
			r.logf("  skip: synthesis (no findings were written)")
		} else if r.synthesize(ctx, res) {
			res.Synthesized = true
		}
	}
	return res, nil
}

// runOne launches a single research task and logs its outcome. A Launch error is
// diagnostic only: the postcondition (did a findings file land?) is checked from
// disk by the caller, so a failed agent is simply a task with no output.
func (r *Runner) runOne(ctx context.Context, t Task) {
	if err := r.Launch(ctx, t); err != nil {
		r.logf("  FAILED: %s — %v (see %s)", t.ID, err, t.LogPath)
		return
	}
	if fileExists(t.OutPath) {
		r.logf("  done: %s → %s", t.ID, t.OutPath)
	} else {
		r.logf("  WARNING: %s exited without writing %s (see %s)", t.ID, t.OutPath, t.LogPath)
	}
}

// synthesize runs one final agent over the written findings to produce
// SYNTHESIS.md. Returns true only when the synthesis file lands.
func (r *Runner) synthesize(ctx context.Context, res *Result) bool {
	r.logf("==> Synthesizing %d finding(s)...", len(res.Written))
	outPath := filepath.Join(r.OutDir, SynthesisFile)
	logPath := filepath.Join(r.OutDir, "synthesis.log")
	t := r.SynthTask(res.Written, outPath, logPath)
	if err := r.Launch(ctx, t); err != nil {
		r.logf("  FAILED: synthesis — %v (see %s)", err, logPath)
		return false
	}
	if !fileExists(outPath) {
		r.logf("  FAILED: synthesis exited without writing %s (see %s)", outPath, logPath)
		return false
	}
	r.logf("  done: synthesis → %s", outPath)
	res.SynthPath = outPath
	return true
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}
