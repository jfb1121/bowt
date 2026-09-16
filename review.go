package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/jfb1121/bowt/internal/agent"
	"github.com/jfb1121/bowt/internal/config"
	"github.com/jfb1121/bowt/internal/lock"
	"github.com/jfb1121/bowt/internal/output"
	"github.com/jfb1121/bowt/internal/repo"
	"github.com/jfb1121/bowt/internal/review"
	"github.com/jfb1121/bowt/internal/state"
)

func newReviewCmd() *cobra.Command {
	var opts reviewOpts
	var perspectives []string
	c := &cobra.Command{
		Use:   "review [-p slugs] [--all] [--base ref]",
		Short: "perspective code review of the current diff",
		Long: `Review the current worktree's diff — merge-base(base, HEAD) → working tree, so
local commits AND uncommitted changes are covered — from several independent
perspectives, then refuse to present any report whose VERDICT lies about what
its body contains.

Each selected perspective (a prompt file with 'layers:' front-matter under
<configDir>/review-perspectives/) is run through the agent's one-shot mode,
bounded by max_parallel. Reports land in .bowt-review/<slug>.md; a report whose
verdict/body counts disagree, or a clean 0/0/0 with no evidence of work, is
rejected to <slug>.rejected. Usable reports are aggregated into SUMMARY.md and
synthesized into a decision queue in SYNTHESIS.md. If 0 of N perspectives
produce a usable report, the command exits non-zero — it never reports success
having reviewed nothing.`,
		Example: `  bowt review --all
  bowt review -p arch-reuse,security-tenancy
  bowt review --base origin/main --no-synthesize`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.perspectives = perspectives
			st, err := state.Open()
			if err != nil {
				return err
			}
			return cmdReview(st, opts)
		},
	}
	c.Flags().StringSliceVarP(&perspectives, "perspectives", "p", nil, "explicit perspective slugs (comma-separated); default is all")
	c.Flags().BoolVar(&opts.all, "all", false, "run every discovered perspective (the default when -p is omitted)")
	c.Flags().StringVar(&opts.base, "base", "", "diff base ref (default: the branch's upstream, else origin/main)")
	c.Flags().StringVar(&opts.agent, "agent", "", "agent provider (must support one-shot; default: $BOWT_AGENT or claude)")
	c.Flags().StringVar(&opts.model, "model", "", "model recorded in the report header (per-call model is not yet plumbed through Oneshot — see STATUS)")
	c.Flags().BoolVar(&opts.noSynthesize, "no-synthesize", false, "skip the synthesis/decision-queue stage")
	return c
}

// reviewOpts carries the parsed flags for `bowt review`.
type reviewOpts struct {
	perspectives []string
	all          bool
	base         string
	agent        string
	model        string
	noSynthesize bool
}

func cmdReview(st state.Store, opts reviewOpts) error {
	// The review runs against the worktree you stand in: the diff, the lock, and
	// the .bowt-review output all belong to this checkout.
	top, err := repo.Toplevel("")
	if err != nil {
		return err
	}
	main, err := repo.MainRepo()
	if err != nil {
		return err
	}
	branch := repo.CurrentBranch(top)
	if branch == "" {
		return fmt.Errorf("detached HEAD — check out a branch to review")
	}

	// Select the provider up front and hard-error BEFORE any work if it cannot
	// run one-shot (this whole pipeline is stdin→stdout fan-out).
	ag, err := agent.Select(opts.agent, os.Getenv)
	if err != nil {
		return err
	}
	if err := agent.RequireOneshot(ag); err != nil {
		return err
	}

	// Perspectives live in the repo's config dir.
	configDir := config.Dir(main)
	if configDir == "" {
		return fmt.Errorf("no .bowt/ config dir in %s — cannot find review perspectives", main)
	}
	perspectivesDir := filepath.Join(configDir, review.PerspectivesSubdir)
	all, err := review.Discover(perspectivesDir)
	if err != nil {
		return err
	}
	selected, err := review.Select(all, opts.perspectives)
	if err != nil {
		return err
	}
	if len(selected) == 0 {
		return fmt.Errorf("no perspectives selected (found %d in %s)", len(all), perspectivesDir)
	}

	// Diff scope + preflight facts (in-place: merge-base(base, HEAD) → worktree).
	base := opts.base
	if base == "" {
		base = repo.UpstreamOrDefault(top)
	}
	facts, err := repo.ReviewScope(top, base)
	if err != nil {
		return err
	}
	if len(facts.ChangedFiles) == 0 {
		output.Errf("no diff against %s — nothing to review", base)
		return nil
	}

	pf := review.Preflight{
		BinaryAdded:     facts.BinaryAdded,
		NewFiles:        facts.NewFiles,
		UntrackedSource: facts.UntrackedSource,
		AddedSymbols:    facts.AddedSymbols,
	}
	jobs := make([]review.Job, len(selected))
	for i, p := range selected {
		jobs[i] = review.Job{Slug: p.Slug, Prompt: review.PerspectivePrompt(p, pf, facts.DiffCmd)}
	}

	modelLabel := opts.model
	if modelLabel == "" {
		modelLabel = ag.Caps().Name + "-default"
	}
	output.Errf("in-place review of %s vs %s — %d perspective(s), agent %s", branch, base, len(jobs), ag.Caps().Name)

	// EXCLUSIVE per-worktree lock: review WRITES the tree (.bowt-review/). Held
	// for the whole run; the kernel releases flock on exit so os.Exit never leaks it.
	// Reentrant so a spawned lane can review its own diff (see lock.HeldByAncestor).
	l, err := lock.AcquireReentrant(top)
	if err != nil {
		return err
	}
	defer func() { _ = l.Release() }()

	r := &review.Runner{
		ReviewDir:   filepath.Join(top, review.ReviewDirName),
		MaxParallel: reviewParallel(os.Getenv),
		Oneshot:     ag.Oneshot,
		Synthesize:  !opts.noSynthesize,
		Branch:      branch,
		Base:        base,
		Mode:        "in-place",
		Model:       modelLabel,
		SynthPrompt: func(paths []string) string { return review.SynthesisPrompt(branch, base, paths) },
		Log:         func(format string, args ...any) { fmt.Fprintf(os.Stderr, format+"\n", args...) },
	}
	rep, err := r.Run(context.Background(), jobs)
	if err != nil {
		return err
	}
	if emitErr := output.Emit(rep); emitErr != nil {
		return emitErr
	}
	if rep.RunFailed {
		os.Exit(1)
	}
	return nil
}

// reviewParallel reads the fan-out bound from BOWT_REVIEW_PARALLEL, falling
// back to the default.
func reviewParallel(getenv func(string) string) int {
	if s := strings.TrimSpace(getenv("BOWT_REVIEW_PARALLEL")); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			return n
		}
	}
	return review.DefaultMaxParallel
}
