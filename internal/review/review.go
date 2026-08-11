package review

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ReviewDirName is the per-worktree directory holding the review output
// contract: <slug>.md reports, <slug>.err / <slug>.rejected on failure,
// SUMMARY.md, and SYNTHESIS.md.
const ReviewDirName = ".bowt-review"

// DefaultMaxParallel is the fan-out concurrency when the config sets none.
const DefaultMaxParallel = 3

// Oneshot is the fan-out primitive: feed a prompt, get captured stdout, no side
// effects. It matches agent.Agent.Oneshot's signature so the command layer
// passes ag.Oneshot directly, and tests pass a fake that returns fixture reports
// with NO real agent process.
type Oneshot func(ctx context.Context, prompt string) (string, error)

// Job is one perspective to run: its slug (report filename) and the fully
// assembled prompt.
type Job struct {
	Slug   string
	Prompt string
}

// Runner drives the fan-out, validation, aggregation, and synthesis. It is
// constructed by the command layer with the resolved output dir, concurrency
// bound, and Oneshot; it holds no git or lock state (those live in the command).
type Runner struct {
	ReviewDir   string  // absolute <worktree>/.bowt-review
	MaxParallel int     // fan-out bound (<=0 means DefaultMaxParallel)
	Oneshot     Oneshot // the fan-out primitive
	Synthesize  bool    // run the synthesis/decision-queue stage

	// Header fields recorded in SUMMARY.md and the synthesis prompt.
	Branch string
	Base   string
	Mode   string
	Model  string

	// SynthPrompt builds the synthesis prompt from the usable report file paths.
	// Injected so tests drive synthesis without the real prompt text.
	SynthPrompt func(reportPaths []string) string

	// Log receives human diagnostics (stderr). nil discards them. It may be
	// called concurrently from the fan-out goroutines, so an implementation that
	// writes to a shared sink must synchronize (the command layer writes to
	// os.Stderr, whose writes are independently flushed).
	Log func(format string, args ...any)

	// Now is the clock seam for the SUMMARY.md timestamp; nil means time.Now.
	Now func() time.Time
}

// Report is the machine-readable outcome. RunFailed is the runner postcondition:
// true when 0 of N perspectives produced a usable report (nothing was reviewed)
// or when synthesis was expected but produced no usable decision queue.
type Report struct {
	Selected    int      `json:"selected"`
	Usable      []string `json:"usable"`
	Failed      []string `json:"failed"`
	Synthesized bool     `json:"synthesized"`
	RunFailed   bool     `json:"run_failed"`
	SummaryPath string   `json:"summary_path"`
	SynthPath   string   `json:"synthesis_path,omitempty"`
}

func (r *Runner) logf(format string, args ...any) {
	if r.Log != nil {
		r.Log(format, args...)
	}
}

// Run fans the jobs out through Oneshot (bounded by MaxParallel), validates each
// report, writes SUMMARY.md, enforces the runner postcondition, and — when
// Synthesize is set and at least one report is usable — synthesizes the decision
// queue into SYNTHESIS.md (archiving any prior one first). A returned error means
// the run could not be performed; a run that completed but reviewed nothing is
// reported via Report.RunFailed, not an error.
func (r *Runner) Run(ctx context.Context, jobs []Job) (*Report, error) {
	if err := os.MkdirAll(r.ReviewDir, 0o755); err != nil {
		return nil, fmt.Errorf("create %s: %w", r.ReviewDir, err)
	}

	// Never destroy a decision queue: archive the previous synthesis (it may
	// carry human AGREE/REJECT decisions) before this run can overwrite it.
	r.archivePriorSynthesis()
	_ = os.Remove(filepath.Join(r.ReviewDir, "SUMMARY.md"))

	max := r.MaxParallel
	if max <= 0 {
		max = DefaultMaxParallel
	}

	// Fan out, bounded by a counting semaphore. Each goroutine writes only its
	// own files; nothing shared is mutated, so the partition below reads results
	// from disk after all goroutines have joined (race-clean).
	sem := make(chan struct{}, max)
	var wg sync.WaitGroup
	for _, j := range jobs {
		wg.Add(1)
		sem <- struct{}{}
		go func(j Job) {
			defer wg.Done()
			defer func() { <-sem }()
			r.runOne(ctx, j)
		}(j)
	}
	wg.Wait()

	// Partition into usable/failed using the SAME tolerant parser the validator
	// uses — never a raw grep. A usable report is one whose .md survived
	// validation (rejected ones were moved to .rejected) and still parses a
	// VERDICT line; this admits a valid-but-backticked verdict exactly as the
	// validator does.
	var usable, failed []string
	for _, j := range jobs {
		if r.reportUsable(j.Slug) {
			usable = append(usable, j.Slug)
		} else {
			failed = append(failed, j.Slug)
		}
	}

	rep := &Report{
		Selected:    len(jobs),
		Usable:      usable,
		Failed:      failed,
		SummaryPath: filepath.Join(r.ReviewDir, "SUMMARY.md"),
	}
	r.writeSummary(rep)

	// Runner postcondition: the counts this pipeline refuses to let a report lie
	// about are the counts the runner must not lie about either. 0 usable of N
	// selected means nothing was reviewed — never report success.
	if len(usable) == 0 {
		r.logf("bowt review: FAILED — 0 of %d selected perspectives produced a usable report; nothing was reviewed", len(jobs))
		rep.RunFailed = true
		if r.Synthesize {
			r.logf("  skip: synthesis (no usable perspective reports)")
		}
		return rep, nil
	}

	if r.Synthesize {
		if r.synthesize(ctx, rep) {
			rep.Synthesized = true
		} else {
			rep.RunFailed = true
		}
	}
	return rep, nil
}

// runOne executes a single perspective: run the prompt, capture stdout to
// <slug>.md and any error to <slug>.err, then apply the postcondition (a
// rejected report is moved to <slug>.rejected with the reason in <slug>.err, so
// the run is left in exactly the shape the contract defines as "this perspective
// failed").
func (r *Runner) runOne(ctx context.Context, j Job) {
	reportPath := filepath.Join(r.ReviewDir, j.Slug+".md")
	errPath := filepath.Join(r.ReviewDir, j.Slug+".err")
	// Clear a stale reject from an earlier run: a leftover <slug>.rejected reads
	// as a current failure when the operator globs *.rejected.
	_ = os.Remove(filepath.Join(r.ReviewDir, j.Slug+".rejected"))

	out, err := r.Oneshot(ctx, j.Prompt)
	if writeErr := os.WriteFile(reportPath, []byte(out), 0o644); writeErr != nil {
		r.appendErr(errPath, fmt.Sprintf("bowt review: %s — could not write report: %v\n", j.Slug, writeErr))
		return
	}
	if err != nil {
		r.appendErr(errPath, fmt.Sprintf("bowt review: %s — agent error: %v\n", j.Slug, err))
	}

	retry := fmt.Sprintf("bowt review -p %s --no-synthesize", j.Slug)
	if r.validateAndReject(j.Slug, reportPath, errPath, retry) {
		r.logf("  done: %s", j.Slug)
	} else {
		r.logf("  FAILED: %s (see %s)", j.Slug, errPath)
	}
}

// validateAndReject enforces the postcondition for one report file. On failure
// it appends the reason to errPath and moves the report to <base>.rejected,
// returning false. On success it returns true, leaving the report in place.
func (r *Runner) validateAndReject(slug, reportPath, errPath, retry string) bool {
	data, _ := os.ReadFile(reportPath)
	reason := ValidateReport(slug, string(data))
	if reason == "" {
		return true
	}
	rej := strings.TrimSuffix(reportPath, ".md") + ".rejected"
	r.appendErr(errPath, fmt.Sprintf("bowt review: %s REJECTED — %s\n", slug, reason)+
		fmt.Sprintf("bowt review: unusable output preserved at %s\n", rej)+
		fmt.Sprintf("bowt review: retry with  %s\n", retry))
	if err := os.Rename(reportPath, rej); err != nil {
		r.appendErr(errPath, fmt.Sprintf("bowt review: %s — could not preserve rejected report: %v\n", slug, err))
	}
	return false
}

// reportUsable reports whether <slug>.md exists and carries a parseable VERDICT
// line — the definition of a usable report, using the validator's own parser.
func (r *Runner) reportUsable(slug string) bool {
	data, err := os.ReadFile(filepath.Join(r.ReviewDir, slug+".md"))
	if err != nil {
		return false
	}
	_, ok := parseVerdict(string(data), slug)
	return ok
}

// synthesize merges the usable reports into SYNTHESIS.md and applies the same
// postcondition. Returns true only if a usable decision queue was written.
func (r *Runner) synthesize(ctx context.Context, rep *Report) bool {
	r.logf("==> Synthesizing decision queue...")
	paths := make([]string, len(rep.Usable))
	for i, s := range rep.Usable {
		paths[i] = filepath.Join(r.ReviewDir, s+".md")
	}
	prompt := r.SynthPrompt(paths)
	synthPath := filepath.Join(r.ReviewDir, "SYNTHESIS.md")
	errPath := filepath.Join(r.ReviewDir, "synthesis.err")

	out, err := r.Oneshot(ctx, prompt)
	if writeErr := os.WriteFile(synthPath, []byte(out), 0o644); writeErr != nil {
		r.appendErr(errPath, fmt.Sprintf("bowt review: synthesis — could not write decision queue: %v\n", writeErr))
		return false
	}
	if err != nil {
		r.appendErr(errPath, fmt.Sprintf("bowt review: synthesis — agent error: %v\n", err))
	}

	if r.validateAndReject("synthesis", synthPath, errPath, "bowt review --base "+r.Base+"   (re-run the full command)") {
		r.logf("  done: synthesis")
		rep.SynthPath = synthPath
		return true
	}
	r.logf("  FAILED: synthesis (see %s) — retry the full command", errPath)
	r.logf("bowt review: FAILED — synthesis produced no decision queue; %d perspective report(s) are usable but unmerged", len(rep.Usable))
	return false
}

// archivePriorSynthesis renames an existing SYNTHESIS.md to
// SYNTHESIS.<epoch>.md so a prior human-annotated decision queue is never lost.
func (r *Runner) archivePriorSynthesis() {
	prior := filepath.Join(r.ReviewDir, "SYNTHESIS.md")
	if _, err := os.Stat(prior); err != nil {
		return
	}
	now := r.Now
	if now == nil {
		now = time.Now
	}
	archived := filepath.Join(r.ReviewDir, fmt.Sprintf("SYNTHESIS.%d.md", now().Unix()))
	if err := os.Rename(prior, archived); err != nil {
		r.logf("  warning: could not archive prior SYNTHESIS.md: %v", err)
		return
	}
	r.logf("  archived prior decision queue → %s", filepath.Base(archived))
}

// writeSummary writes SUMMARY.md: the header, the usable reports' VERDICT lines,
// a report index, and a failed-perspective list with retry hints. It is built
// from the partitioned slug lists, never a *.md glob (the glob also swept up
// archived SYNTHESIS.<epoch>.md queues and foreign artefacts and fed them back
// in as fresh reports).
func (r *Runner) writeSummary(rep *Report) {
	now := r.Now
	if now == nil {
		now = time.Now
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# Perspective review — %s vs %s (%s)\n", r.Branch, r.Base, r.Mode)
	fmt.Fprintf(&b, "date: %s  model: %s\n\n", now().UTC().Format("2006-01-02T15:04Z"), r.Model)

	b.WriteString("## Verdicts\n")
	var verdicts []string
	for _, s := range rep.Usable {
		if v, ok := r.verdictLine(s); ok {
			verdicts = append(verdicts, v)
		}
	}
	sort.Strings(verdicts)
	for _, v := range verdicts {
		b.WriteString(v + "\n")
	}

	b.WriteString("\n## Reports\n")
	for _, s := range rep.Usable {
		fmt.Fprintf(&b, "- %s/%s.md\n", ReviewDirName, s)
	}

	if len(rep.Failed) > 0 {
		b.WriteString("\n## Failed perspectives\n")
		b.WriteString("No usable report — NOT counted anywhere. Retry each:\n")
		for _, s := range rep.Failed {
			fmt.Fprintf(&b, "- %s — see %s/%s.err   `bowt review -p %s --no-synthesize`\n", s, ReviewDirName, s, s)
		}
	}

	_ = os.WriteFile(filepath.Join(r.ReviewDir, "SUMMARY.md"), []byte(b.String()), 0o644)
}

// verdictLine returns a normalized "VERDICT <slug>: blockers=B majors=M
// minors=N" line for a usable report's SUMMARY headline. It is rebuilt from the
// parsed counts (not grepped raw) so a valid-but-decorated verdict — backticked,
// bold — still contributes a clean line, where review.sh's raw `grep ^VERDICT`
// silently dropped it.
func (r *Runner) verdictLine(slug string) (string, bool) {
	data, err := os.ReadFile(filepath.Join(r.ReviewDir, slug+".md"))
	if err != nil {
		return "", false
	}
	c, ok := parseVerdict(string(data), slug)
	if !ok {
		return "", false
	}
	return fmt.Sprintf("VERDICT %s: blockers=%d majors=%d minors=%d", slug, c.Blockers, c.Majors, c.Minors), true
}

// appendErr appends text to an error file, creating it if absent.
func (r *Runner) appendErr(path, text string) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	_, _ = f.WriteString(text)
}
