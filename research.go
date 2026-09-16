package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/jfb1121/bowt/internal/agent"
	"github.com/jfb1121/bowt/internal/output"
	"github.com/jfb1121/bowt/internal/repo"
	"github.com/jfb1121/bowt/internal/research"
	"github.com/jfb1121/bowt/internal/spawn"
)

func newResearchCmd() *cobra.Command {
	var opts researchOpts
	c := &cobra.Command{
		Use:   "research <brief>",
		Short: "fan out headless research agents over a brief",
		Long: `Fan out headless research agents — the same subscription-powered child-process
path spawn uses (not a metered API key) — over a research task, each doing web
research (WebSearch/WebFetch), citing sources, and writing ONE findings file. It
is spawn for research rather than code: no gate, no commit, no PR — the
deliverable is the written findings under --out.

Fan-out: --queries fans one agent per line of the file (overrides --n); else the
single brief is fanned into --n angled agents (default 1). Brief resolution
matches spawn (subagent/PROMPT.md → subagent/*-prompt.md → PROMPT.md) unless a
path is passed.

Bounded concurrency is load-bearing: at most --concurrency agents run at once
(default 2), the next starting only as one finishes — the host OOMs past a
handful of concurrent agents. Each agent writes <out>/<id>.md; --synthesize adds
a final agent that merges the findings into <out>/SYNTHESIS.md.`,
		Example: `  bowt research brief.md --n 3 --concurrency 2
  bowt research --queries topics.txt --out findings/
  bowt research brief.md --n 5 --synthesize
  bowt research brief.md --n 3 --dry-run   # show the plan, launch nothing`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				opts.brief = args[0]
			}
			return cmdResearch(opts)
		},
	}
	c.Flags().StringVar(&opts.queries, "queries", "", "file with one research query per line → one agent per query (overrides --n)")
	c.Flags().IntVar(&opts.n, "n", 1, "fan the single brief into N angled agents (ignored when --queries is set)")
	c.Flags().IntVar(&opts.concurrency, "concurrency", research.DefaultConcurrency, "max agents running at once (OOM guard)")
	c.Flags().StringVar(&opts.out, "out", research.DefaultOutDir, "directory each agent writes its findings into")
	c.Flags().BoolVar(&opts.synthesize, "synthesize", false, "after all finish, one agent merges the findings into SYNTHESIS.md")
	c.Flags().StringVar(&opts.agent, "agent", "", "agent provider (claude, codex; default: $BOWT_AGENT or claude)")
	c.Flags().StringVar(&opts.model, "model", "", "agent model (alias opus/sonnet/haiku, or a full ID)")
	c.Flags().StringVar(&opts.effort, "effort", "", "agent reasoning effort (e.g. high)")
	c.Flags().BoolVar(&opts.dryRun, "dry-run", false, "assemble the plan (tasks, concurrency, out paths) and print it, then exit — launch no agents")
	return c
}

// researchOpts carries the parsed flags for `bowt research`.
type researchOpts struct {
	brief       string
	queries     string
	n           int
	concurrency int
	out         string
	synthesize  bool
	agent       string
	model       string
	effort      string
	dryRun      bool
}

// researchTaskPlan is one task in the --dry-run plan (agent-facing shape).
type researchTaskPlan struct {
	ID      string `json:"id"`
	OutPath string `json:"out_path"`
}

// researchPlan is the --dry-run projection: what research WOULD launch, without
// forking a single agent.
type researchPlan struct {
	OutDir      string             `json:"out_dir"`
	Concurrency int                `json:"concurrency"`
	Synthesize  bool               `json:"synthesize"`
	Tasks       []researchTaskPlan `json:"tasks"`
}

// cmdResearch fans out headless research agents over a brief (or per-query),
// each writing ONE findings file under --out. It reuses the spawn agent path
// (subscription-powered, not a metered key) and the versioned research wrapper,
// but takes NO per-worktree lock: a research agent is a transient child that
// only reads the repo and writes under --out, so N run concurrently rather than
// serializing on the one worktree lock a spawn lane holds. The load-bearing
// bound is --concurrency (OOM guard), enforced by research.Runner.
func cmdResearch(opts researchOpts) error {
	// Root the fan-out at the current worktree: the brief and the out dir belong
	// to the checkout you stand in.
	top, err := repo.Toplevel("")
	if err != nil {
		return err
	}

	// Select the provider up front (flag → env → claude) and hard-error BEFORE any
	// work if it cannot run headless — a research agent runs unattended (no TTY),
	// so it needs the same Edit/Write guardrail `spawn --headless` requires.
	ag, err := agent.Select(opts.agent, os.Getenv)
	if err != nil {
		return err
	}
	if err := agent.RequireHeadless(ag); err != nil {
		return err
	}
	caps := ag.Caps()

	// A brief and --queries are conflicting fan-out sources: --queries would
	// silently discard the brief. Refuse rather than drop it.
	if opts.queries != "" && opts.brief != "" {
		return fmt.Errorf("pass either a brief or --queries, not both")
	}

	// Fan-out: --queries (one agent per line) wins over the single brief + --n.
	briefs, source, err := researchBriefs(top, opts)
	if err != nil {
		return err
	}

	// Floor the concurrency to the OOM-guard default up front, so the header, the
	// --dry-run plan, and the runner all report and enforce the SAME bound (a <=0
	// value runs at DefaultConcurrency, not "0").
	concurrency := opts.concurrency
	if concurrency <= 0 {
		concurrency = research.DefaultConcurrency
	}

	model := spawn.ResolveModel(opts.model, spawn.ModeResearch)
	effort := spawn.ResolveEffort(opts.effort, spawn.ModeResearch)

	outDir := opts.out
	if outDir == "" {
		outDir = research.DefaultOutDir
	}
	if !filepath.IsAbs(outDir) {
		outDir = filepath.Join(top, outDir)
	}

	// Assemble one research prompt per brief, substituting the per-agent out path
	// into the wrapper's {{OUT_FILE}} placeholder (Assemble leaves it intact).
	tasks := make([]research.Task, len(briefs))
	for i, b := range briefs {
		a, aerr := spawn.Assemble(spawn.ModeResearch, caps.Name, caps.MemoryFile, b)
		if aerr != nil {
			return aerr
		}
		id := research.TaskID(i, len(briefs))
		outPath := filepath.Join(outDir, id+".md")
		tasks[i] = research.Task{
			ID:      id,
			Prompt:  strings.ReplaceAll(a.Prompt, spawn.OutFilePlaceholder, outPath),
			OutPath: outPath,
			LogPath: filepath.Join(outDir, id+".log"),
		}
	}

	output.Errf("research → %s", top)
	fmt.Fprintf(os.Stderr, "  source: %s   agents: %d   concurrency: %d   agent: %s\n",
		source, len(tasks), concurrency, caps.Name)
	fmt.Fprintf(os.Stderr, "  out: %s   model: %s   effort: %s   synthesize: %t\n",
		outDir, orDefault(model), orDefault(effort), opts.synthesize)

	// Dry-run: print the plan (what would launch) and stop — no agents, no writes.
	if opts.dryRun {
		plan := researchPlan{OutDir: outDir, Concurrency: concurrency, Synthesize: opts.synthesize}
		for _, t := range tasks {
			plan.Tasks = append(plan.Tasks, researchTaskPlan{ID: t.ID, OutPath: t.OutPath})
		}
		return output.Emit(plan)
	}

	r := &research.Runner{
		OutDir:      outDir,
		Concurrency: concurrency,
		Launch:      headlessLauncher(ag, model, effort),
		Synthesize:  opts.synthesize,
		SynthTask: func(paths []string, outPath, logPath string) research.Task {
			return research.Task{ID: "synthesis", Prompt: research.SynthesisPrompt(paths, outPath), OutPath: outPath, LogPath: logPath}
		},
		Log: func(format string, args ...any) { fmt.Fprintf(os.Stderr, format+"\n", args...) },
	}
	res, err := r.Run(context.Background(), tasks)
	if err != nil {
		return err
	}
	return output.Emit(res)
}

// researchBriefs resolves the fan-out into one brief per agent and a label for
// the header: the lines of --queries (one agent each), else the resolved brief
// fanned into --n angled copies.
func researchBriefs(top string, opts researchOpts) (briefs []string, source string, err error) {
	if opts.queries != "" {
		lines, rerr := readQueryLines(opts.queries)
		if rerr != nil {
			return nil, "", rerr
		}
		if len(lines) == 0 {
			return nil, "", fmt.Errorf("queries file %s has no non-empty lines", opts.queries)
		}
		return research.TaskBriefs("", lines, 0), fmt.Sprintf("%s (%d queries)", opts.queries, len(lines)), nil
	}
	briefPath, brief, rerr := spawn.ResolveBrief(top, opts.brief)
	if rerr != nil {
		return nil, "", rerr
	}
	return research.TaskBriefs(brief, nil, opts.n), briefPath, nil
}

// readQueryLines reads a queries file into its non-empty, trimmed lines.
func readQueryLines(path string) ([]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read queries %s: %w", path, err)
	}
	var lines []string
	for _, ln := range strings.Split(string(b), "\n") {
		if s := strings.TrimSpace(ln); s != "" {
			lines = append(lines, s)
		}
	}
	return lines, nil
}

// headlessLauncher builds the research.Launch seam over ag.Headless: it captures
// each agent's stream to the task's log file and takes NO lock (the research
// agent only reads the repo and writes under --out, so concurrent agents in one
// worktree don't contend). This is the one edge that forks a real agent; tests
// substitute a fake Launch instead.
func headlessLauncher(ag agent.Agent, model, effort string) research.Launch {
	return func(ctx context.Context, t research.Task) error {
		// Truncate, don't append: a re-run into the same out dir must give this
		// agent a fresh log, not blend its stream beneath a prior run's output.
		lf, err := os.OpenFile(t.LogPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			return fmt.Errorf("open research log %s: %w", t.LogPath, err)
		}
		defer func() { _ = lf.Close() }()
		opts := agent.Opts{Model: model, Effort: effort, Stdout: lf, Stderr: lf}
		return ag.Headless(ctx, t.Prompt, opts)
	}
}
