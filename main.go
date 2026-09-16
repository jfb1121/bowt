// Command bowt manages git worktrees with isolated dev environments.
//
// Agent-first: commands emit structured JSON by default whenever stdout is not
// a terminal, so agents parse output instead of scraping human tables.
//
// The CLI is built on cobra so every command gets real --help, usage, and
// shell completion (see `bowt completion`) — but the command logic still lives
// in the cmd* functions below, called from each command's RunE.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/jfb1121/bowt/internal/agent"
	"github.com/jfb1121/bowt/internal/config"
	"github.com/jfb1121/bowt/internal/env"
	"github.com/jfb1121/bowt/internal/extension"
	"github.com/jfb1121/bowt/internal/lock"
	"github.com/jfb1121/bowt/internal/output"
	"github.com/jfb1121/bowt/internal/repo"
	"github.com/jfb1121/bowt/internal/research"
	"github.com/jfb1121/bowt/internal/run"
	"github.com/jfb1121/bowt/internal/shell"
	"github.com/jfb1121/bowt/internal/spawn"
	"github.com/jfb1121/bowt/internal/state"
)

// version is set at build time via -ldflags "-X main.version=…" (see Makefile).
var version = "dev"

func main() {
	// Discover user drop-in providers (~/.bowt/agents/*.json) once at startup,
	// AFTER package init has registered the built-ins, so a colliding drop-in is
	// skipped (built-ins win) and provider selection sees the full set. Mirrors
	// how the extension loader is wired from main; a missing dir is a no-op.
	agent.LoadDropins(output.Errf)

	root := newRootCmd()
	// Git-style dispatch: if the first word is not a built-in command but the
	// repo ships a matching extension, run it and exit with its code. Built-ins
	// always win; an unknown word with no extension falls through to cobra's
	// normal unknown-command error below.
	if code, handled := tryExtension(root, os.Args[1:]); handled {
		os.Exit(code)
	}
	if err := root.Execute(); err != nil {
		// Preserve bowt's error contract: "bowt: <err>" on stderr, exit 1.
		// Usage/errors are silenced on the commands so cobra doesn't also print
		// its own "Error:" line or dump usage on a runtime failure.
		output.Errf("%v", err)
		os.Exit(1)
	}
}

// newRootCmd builds the full command tree. It's a constructor (not a package
// var) so tests can build a fresh, isolated tree per case.
func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "bowt",
		Short: "git worktrees with isolated environments",
		Long: `bowt — git worktrees with isolated environments.

Every branch gets its own worktree with an allocated port/offset. Commands emit
JSON by default whenever stdout is not a terminal, so agents parse structured
output instead of scraping tables.`,
		Version: version,
		// Own our error/usage output (see main): print "bowt: <err>" ourselves.
		SilenceUsage:  true,
		SilenceErrors: true,
		// No bash-completion command noise for a private tool; we ship our own.
		CompletionOptions: cobra.CompletionOptions{DisableDefaultCmd: true},
	}
	// `bowt --version` prints just the version string (matches the old behavior),
	// not cobra's default "bowt version X.Y".
	root.SetVersionTemplate("{{.Version}}\n")

	root.AddCommand(
		newNewCmd(),
		newLsCmd(),
		newPathCmd(),
		newRmCmd(),
		newExecCmd(),
		newSpawnCmd(),
		newLaneCmd(),
		newLanesCmd(),
		newStatusCmd(),
		newLaneRunCmd(),
		newGateCmd(),
		newLandCmd(),
		newReviewCmd(),
		newResearchCmd(),
		newDoctorCmd(),
		newRootPathCmd(),
		newCdCmd(),
		newShellInitCmd(),
		newExtensionsCmd(),
		newLibCmd(),
		newVersionCmd(),
		newCompletionCmd(),
	)
	return root
}

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
	c.Flags().StringVar(&opts.agent, "agent", "", "agent provider (claude, codex; default: $BOWT_AGENT/$GWT_AGENT or claude)")
	c.Flags().StringVar(&opts.model, "model", "", "agent model (alias opus/sonnet/haiku, or a full ID)")
	c.Flags().StringVar(&opts.effort, "effort", "", "agent reasoning effort (e.g. high)")
	c.Flags().BoolVar(&opts.dryRun, "dry-run", false, "assemble the plan (tasks, concurrency, out paths) and print it, then exit — launch no agents")
	return c
}

func newDoctorCmd() *cobra.Command {
	var agentName string
	var dryRun bool
	c := &cobra.Command{
		Use:   "doctor --agent <name>",
		Short: "smoke-check an agent provider",
		Long: `Smoke-check a provider descriptor: its binary is on PATH, its config dir is
resolvable, and — when the provider supports one-shot — a trivial prompt
round-trips through it. Emits a machine-readable report; exits non-zero if any
check fails.

(Only the --agent path exists in this slice; a fuller doctor covering deps,
ports, and the registry is a later slice.)`,
		Example: `  bowt doctor --agent claude
  bowt doctor --agent codex
  bowt doctor --agent claude --dry-run   # skip the one-shot launch`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if agentName == "" {
				return fmt.Errorf("doctor currently supports only --agent <name>")
			}
			ag, err := agent.New(agentName)
			if err != nil {
				return err
			}
			rep := doctorAgent(ag, exec.LookPath, os.Getenv("HOME"), dryRun)
			if emitErr := output.Emit(rep); emitErr != nil {
				return emitErr
			}
			if !rep.OK {
				os.Exit(1)
			}
			return nil
		},
	}
	c.Flags().StringVar(&agentName, "agent", "", "agent provider to smoke-check (claude, codex)")
	c.Flags().BoolVar(&dryRun, "dry-run", false, "skip the one-shot round-trip (check bin + config only)")
	return c
}

func newRootPathCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "root",
		Short:   "print the main repo path",
		Long:    "Print the absolute path of the main repository root (works from inside a worktree).",
		Example: "  bowt root",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmdRoot()
		},
	}
}

func newCdCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "cd <branch>|main",
		Short: "change directory into a worktree (needs shell integration)",
		Long: `Change the caller's directory into a worktree.

Changing the parent shell's cwd is the one thing a child process cannot do, so
cd requires the shell integration:

  eval "$(bowt shell-init zsh)"`,
		Example:           "  bowt cd feature/login",
		ValidArgsFunction: completeBranchArg,
		RunE: func(cmd *cobra.Command, args []string) error {
			return fmt.Errorf(`cd needs the shell integration — run: eval "$(bowt shell-init zsh)"`)
		},
	}
}

func newShellInitCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "shell-init [bash|zsh]",
		Short: "print shell integration to eval",
		Long: `Print the shell integration snippet to eval, e.g.:

  eval "$(bowt shell-init zsh)"

This installs a bowt() wrapper so 'bowt cd' can change your shell's directory,
and loads bowt's shell completion for the chosen shell.`,
		Example: `  eval "$(bowt shell-init zsh)"
  eval "$(bowt shell-init bash)"`,
		Args:      cobra.MaximumNArgs(1),
		ValidArgs: []string{"bash", "zsh"},
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmdShellInit(args)
		},
	}
}

func newExtensionsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "extensions",
		Short: "list the repo's per-repo extensions",
		Long: `List the extensions this repo ships under <configDir>/extensions/*.sh, with
each one's description and lock mode.

An extension is a bash script the repo drops in to add a 'bowt <cmd>' without
patching bowt. When <cmd> is not a built-in and a matching script exists, bowt
runs it as a subprocess with the per-worktree BOWT_*/GWT_* environment injected
and BOWT_LIB pointing at the helper (see 'bowt lib').

A leading comment header configures it:
  # bowt-lock: none|shared|exclusive   (bowt takes the lock around the run)
  # bowt-desc: <one-line description>`,
		Example: `  bowt extensions
  bowt extensions --json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmdExtensions()
		},
	}
}

func newLibCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "lib",
		Short: "print the sourceable bash helper for extensions",
		Long: `Print bowt.lib.sh, the bash helper an extension sources via "$BOWT_LIB". bowt
exports BOWT_LIB into every extension pointing at this content, so a script can:

  source "$BOWT_LIB"
  bowt_log "starting"

It defines bowt_log / bowt_err (both to stderr). The BOWT_*/GWT_* environment is
already exported into the script, so the helper stays thin.`,
		Example: "  bowt lib",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// WriteString (not fmt.Print) since Lib legitimately contains %s.
			_, err := os.Stdout.WriteString(extension.Lib)
			return err
		},
	}
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "version",
		Short:   "print build version",
		Example: "  bowt version",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Println(version)
			return nil
		},
	}
}

func newCompletionCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "completion [bash|zsh|fish|powershell]",
		Short: "generate a shell completion script",
		Long: `Generate a shell completion script for bowt.

Usually you don't run this directly — 'bowt shell-init' sources it for you. To
load it manually for the current session:

  source <(bowt completion zsh)     # zsh
  source <(bowt completion bash)    # bash`,
		Example: `  bowt completion zsh
  source <(bowt completion bash)`,
		Args:      cobra.ExactArgs(1),
		ValidArgs: []string{"bash", "zsh", "fish", "powershell"},
		RunE: func(cmd *cobra.Command, args []string) error {
			root := cmd.Root()
			out := cmd.OutOrStdout()
			switch args[0] {
			case "bash":
				return root.GenBashCompletionV2(out, true)
			case "zsh":
				return root.GenZshCompletion(out)
			case "fish":
				return root.GenFishCompletion(out, true)
			case "powershell":
				return root.GenPowerShellCompletionWithDesc(out)
			default:
				return fmt.Errorf("unsupported shell %q (want bash, zsh, fish, or powershell)", args[0])
			}
		},
	}
	return c
}

// orDash renders an empty scalar as "-" in a human table.
func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// newLaneID mints a stable, filesystem-safe lane id: a slug of the ticket (when
// given) plus a short random suffix, so it can name the log/spec files and be
// reported before the fork races. Random bytes come from crypto/rand.
func newLaneID(ticket string) (string, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate lane id: %w", err)
	}
	suffix := hex.EncodeToString(b[:])
	slug := slugify(ticket)
	if slug == "" {
		return "lane-" + suffix, nil
	}
	return slug + "-" + suffix, nil
}

// slugify lowercases s and collapses any run of non-alphanumeric bytes to a
// single dash, trimming leading/trailing dashes — a filesystem-safe stem. A
// pending dash is only emitted once a real char follows, so leading and
// trailing separators never survive.
func slugify(s string) string {
	var out []rune
	dash := false
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			if dash && len(out) > 0 {
				out = append(out, '-')
			}
			out = append(out, r)
			dash = false
		} else {
			dash = true
		}
	}
	return string(out)
}

// orDefault labels an empty model/effort as the agent's session default for the
// human-facing header.
func orDefault(s string) string {
	if s == "" {
		return "session-default"
	}
	return s
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

// doctorCheck is one smoke-check outcome in the agent report.
type doctorCheck struct {
	Name   string `json:"name"`
	Pass   bool   `json:"pass"`
	Detail string `json:"detail,omitempty"`
}

// doctorReport is the agent-facing result of `bowt doctor --agent`.
type doctorReport struct {
	Agent  string        `json:"agent"`
	Checks []doctorCheck `json:"checks"`
	OK     bool          `json:"ok"`
}

// doctorAgent runs the provider smoke-checks. lookPath and home are injected so
// tests exercise it without a real PATH or HOME; dryRun skips the one-shot
// launch so the check is safe to run without launching the CLI. When the
// provider has no one-shot mode, that step is a pass (nothing to check), not a
// failure — SupportsOneshot=false is a legitimate provider shape.
func doctorAgent(ag agent.Agent, lookPath func(string) (string, error), home string, dryRun bool) doctorReport {
	caps := ag.Caps()
	rep := doctorReport{Agent: caps.Name, OK: true}
	add := func(name string, pass bool, detail string) {
		rep.Checks = append(rep.Checks, doctorCheck{Name: name, Pass: pass, Detail: detail})
		if !pass {
			rep.OK = false
		}
	}

	// schema (RFC §11.1) — FIRST check: statically validate the descriptor
	// (known schemaVersion, required fields, effort keys ∈ enum, legal prompt
	// deliveries, at-most-one-{value} render lists) so a malformed or
	// lying-at-the-value-level drop-in fails here, not at run time. A failing
	// schema check sets rep.OK=false, and newDoctorCmd's os.Exit(1) then fires.
	// A non-descriptor provider (future-proofing) is skipped, not failed.
	if problems, checked := agent.Validate(ag); !checked {
		add("schema", true, "not a descriptor-backed provider (skipped)")
	} else if len(problems) == 0 {
		add("schema", true, "descriptor valid")
	} else {
		add("schema", false, strings.Join(problems, "; "))
	}

	// origin/provenance (informational, never fails): whether this provider is a
	// trusted embedded built-in or a user drop-in, and for a drop-in the file to
	// edit — the debugging context a doctor run on one's own descriptor wants.
	if builtin, path, ok := agent.Provenance(ag); ok {
		if builtin {
			add("origin", true, "built-in (embedded)")
		} else {
			add("origin", true, "drop-in: "+path)
		}
	}

	// bin on PATH
	if p, err := lookPath(caps.Bin); err == nil {
		add("bin-on-path", true, p)
	} else {
		add("bin-on-path", false, fmt.Sprintf("%s not found on PATH", caps.Bin))
	}

	// config dir resolvable (path computable from a known HOME; existence is a
	// detail, not a failure — a fresh machine may not have run the agent yet).
	switch {
	case caps.ConfigDir == "":
		add("config-dir", false, "provider declares no config dir")
	case home == "":
		add("config-dir", false, "HOME unset; cannot resolve "+caps.ConfigDir)
	default:
		dir := filepath.Join(home, caps.ConfigDir)
		if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
			add("config-dir", true, dir)
		} else {
			add("config-dir", true, dir+" (absent)")
		}
	}

	// one-shot round-trip (only when supported and not dry-run)
	switch {
	case !caps.SupportsOneshot:
		add("oneshot", true, "not supported by this provider (skipped)")
	case dryRun:
		add("oneshot", true, "skipped (--dry-run)")
	default:
		out, err := ag.Oneshot(context.Background(), "Reply with the single word: ok")
		if err != nil {
			add("oneshot", false, err.Error())
		} else {
			add("oneshot", true, fmt.Sprintf("round-trip ok (%d bytes)", len(strings.TrimSpace(out))))
		}
	}
	return rep
}

func cmdRoot() error {
	main, err := repo.MainRepo()
	if err != nil {
		return err
	}
	fmt.Println(main)
	return nil
}

// tryExtension implements the git-style dispatch fallback. It returns
// handled=true only when args name a per-repo extension that bowt ran (code is
// then the extension's exit code); handled=false means "not an extension — let
// cobra handle it" (a built-in, a flag, or an unknown command with no matching
// extension, which becomes cobra's normal error).
func tryExtension(root *cobra.Command, args []string) (code int, handled bool) {
	// No word, or a flag (e.g. --help/--version): cobra's job, never an extension.
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return 0, false
	}
	// Let cobra resolve the word first: if it maps to any real command (built-in,
	// help, __complete, …), Find returns that command and built-ins always win —
	// an extension can never shadow a built-in. When the word is unknown, Find
	// returns the root command WITH an "unknown command" error; that error is
	// exactly our signal to try an extension, so we key only on cmd != root.
	cmd, _, _ := root.Find(args)
	if cmd != root {
		return 0, false
	}
	// args[0] is unknown to cobra. Only inside a repo with a config dir can an
	// extension exist; otherwise fall through to cobra's unknown-command error.
	main, err := repo.MainRepo()
	if err != nil {
		return 0, false
	}
	ext, found, err := extension.Find(config.Dir(main), args[0])
	if err != nil {
		output.Errf("%v", err)
		return 1, true
	}
	if !found {
		return 0, false
	}
	code, err = cmdExtension(main, ext, args[1:])
	if err != nil {
		output.Errf("%v", err)
		return 1, true
	}
	return code, true
}

// cmdExtension runs a resolved extension against the worktree the caller stands
// in: cwd = that worktree, the per-worktree BOWT_*/GWT_* + config env injected,
// and the manifest's lock taken around the run so the author writes no lock
// code. It returns the child's exit code; err covers only setup/lock failures.
func cmdExtension(main string, ext extension.Extension, args []string) (int, error) {
	top, err := repo.Toplevel("")
	if err != nil {
		return 1, err
	}
	name := filepath.Base(main)
	branch := repo.CurrentBranch(top)

	// Per-worktree env — the same environment exec/gate build. A broken config is
	// a warning, not a hard failure: the extension stays runnable.
	r := run.Exec{Stderr: os.Stderr}
	vars, err := config.Load(r, config.Dir(main))
	if err != nil {
		output.Errf("load config: %v — running %q without config env", err, ext.Cmd)
		vars = nil
	}
	info := env.Info{Path: top, Branch: branch, MainRepo: main, RepoName: name}
	if st, err := state.Open(); err == nil {
		if wt, ok, gerr := st.Get(name, branch); gerr == nil && ok {
			info.Offset, info.Port, info.CodeOnly = wt.Offset, wt.Port, wt.CodeOnly()
		}
	}
	envKV := env.Build(info, vars)

	// The manifest owns locking. Key on the worktree path (as gate/spawn/review
	// do) and fail fast with the busy message if another bowt process holds it.
	// The lock is released by defer before this returns, so main's os.Exit with
	// the propagated code never leaks it (the kernel also frees flock on exit).
	switch ext.Manifest.Lock {
	case extension.LockExclusive:
		l, lerr := lock.AcquireReentrant(top)
		if lerr != nil {
			return 1, lerr
		}
		defer func() { _ = l.Release() }()
	case extension.LockShared:
		l, lerr := lock.AcquireSharedReentrant(top)
		if lerr != nil {
			return 1, lerr
		}
		defer func() { _ = l.Release() }()
	}

	return extension.Run(ext, top, envKV, args, os.Stdin, os.Stdout, os.Stderr)
}

// extInfo is the agent-facing shape for one extension in `bowt extensions`.
type extInfo struct {
	Cmd  string `json:"cmd"`
	Desc string `json:"desc"`
	Lock string `json:"lock"`
}

func cmdExtensions() error {
	main, err := repo.MainRepo()
	if err != nil {
		return err
	}
	exts, err := extension.List(config.Dir(main))
	if err != nil {
		return err
	}
	infos := make([]extInfo, 0, len(exts))
	for _, e := range exts {
		lockMode := e.Manifest.Lock
		if lockMode == "" {
			lockMode = extension.LockNone
		}
		infos = append(infos, extInfo{Cmd: e.Cmd, Desc: e.Manifest.Desc, Lock: string(lockMode)})
	}
	// Agent-first: JSON unless a human is at the terminal.
	if !output.IsTTY() {
		return output.Emit(infos)
	}
	if len(infos) == 0 {
		fmt.Println("no extensions")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "COMMAND\tLOCK\tDESCRIPTION")
	for _, e := range infos {
		fmt.Fprintf(w, "%s\t%s\t%s\n", e.Cmd, e.Lock, e.Desc)
	}
	return w.Flush()
}

func cmdShellInit(args []string) error {
	sh := "zsh"
	if len(args) > 0 {
		sh = args[0]
	}
	out, err := shell.Init(sh)
	if err != nil {
		return err
	}
	fmt.Print(out)
	return nil
}
