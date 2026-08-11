package review

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeOneshot returns a canned report per prompt (the test uses the slug as the
// prompt), so the whole pipeline runs with NO real agent process. It optionally
// tracks concurrency to prove max_parallel bounds the fan-out.
type fakeOneshot struct {
	responses map[string]string
	errs      map[string]error

	mu        sync.Mutex
	inFlight  int
	maxFlight int
	hold      time.Duration // how long each call stays "in flight"
}

func (f *fakeOneshot) run(_ context.Context, prompt string) (string, error) {
	f.mu.Lock()
	f.inFlight++
	if f.inFlight > f.maxFlight {
		f.maxFlight = f.inFlight
	}
	f.mu.Unlock()

	if f.hold > 0 {
		time.Sleep(f.hold)
	}

	f.mu.Lock()
	f.inFlight--
	f.mu.Unlock()

	if err, ok := f.errs[prompt]; ok {
		return "", err
	}
	return f.responses[prompt], nil
}

// The four fixture classes the brief names, keyed by slug (== prompt in tests).
const (
	fxValid = "### [MAJOR] real (a.go:1)\n- evidence\n- why\n- fix\n\nVERDICT valid: blockers=0 majors=1 minors=0"
	fxLiar  = "### [MAJOR] one block (a.go:1)\n- e\n\nVERDICT liar: blockers=0 majors=2 minors=0"
	fxLazy  = "VERDICT lazy: blockers=0 majors=0 minors=0"
	fxFancy = "### [MINOR] tiny (b.go:2)\n- e\n\n`VERDICT fancy: blockers=0 majors=0 minors=1`"
)

func newRunner(t *testing.T, f *fakeOneshot) *Runner {
	t.Helper()
	return &Runner{
		ReviewDir:   filepath.Join(t.TempDir(), ReviewDirName),
		MaxParallel: 3,
		Oneshot:     f.run,
		Branch:      "feature",
		Base:        "origin/main",
		Mode:        "in-place",
		Model:       "test",
		SynthPrompt: func(paths []string) string { return "SYNTH:" + strings.Join(paths, ",") },
		Now:         func() time.Time { return time.Unix(1700000000, 0) },
	}
}

// The pipeline accepts the valid and backticked reports and rejects the mismatch
// and clean-but-empty ones, and the partition/SUMMARY reflect exactly that.
func TestRunPartitionsReports(t *testing.T) {
	f := &fakeOneshot{responses: map[string]string{
		"valid": fxValid, "liar": fxLiar, "lazy": fxLazy, "fancy": fxFancy,
	}}
	r := newRunner(t, f)
	jobs := []Job{{"valid", "valid"}, {"liar", "liar"}, {"lazy", "lazy"}, {"fancy", "fancy"}}

	rep, err := r.Run(context.Background(), jobs)
	if err != nil {
		t.Fatal(err)
	}

	if got := strings.Join(rep.Usable, ","); got != "valid,fancy" {
		t.Fatalf("usable = %q; want valid,fancy", got)
	}
	if got := strings.Join(rep.Failed, ","); got != "liar,lazy" {
		t.Fatalf("failed = %q; want liar,lazy", got)
	}
	if rep.RunFailed {
		t.Fatalf("RunFailed should be false with usable reports")
	}

	// Usable reports keep their .md; rejected ones are moved to .rejected with a
	// reason in .err.
	mustExist(t, r.ReviewDir, "valid.md", "fancy.md", "liar.rejected", "lazy.rejected")
	mustAbsent(t, r.ReviewDir, "liar.md", "lazy.md")
	if reason := readFile(t, r.ReviewDir, "liar.err"); !strings.Contains(reason, "verdict/body mismatch") {
		t.Fatalf("liar.err missing mismatch reason: %q", reason)
	}
	if reason := readFile(t, r.ReviewDir, "lazy.err"); !strings.Contains(reason, "no evidence of work") {
		t.Fatalf("lazy.err missing floor reason: %q", reason)
	}

	// SUMMARY.md carries only the usable verdicts and a failed list.
	summary := readFile(t, r.ReviewDir, "SUMMARY.md")
	for _, want := range []string{"VERDICT valid:", "VERDICT fancy:", "## Failed perspectives", "liar", "lazy"} {
		if !strings.Contains(summary, want) {
			t.Fatalf("SUMMARY.md missing %q:\n%s", want, summary)
		}
	}
	if strings.Contains(summary, "VERDICT liar") {
		t.Fatalf("SUMMARY.md must not carry a rejected report's verdict")
	}
}

// An agent error becomes a failed perspective (empty report), never a usable one.
func TestRunAgentErrorFails(t *testing.T) {
	okReport := "### [MAJOR] real (a.go:1)\n- e\n\nVERDICT ok: blockers=0 majors=1 minors=0"
	f := &fakeOneshot{
		responses: map[string]string{"ok": okReport},
		errs:      map[string]error{"boom": errors.New("provider exploded")},
	}
	r := newRunner(t, f)
	rep, err := r.Run(context.Background(), []Job{{"ok", "ok"}, {"boom", "boom"}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(rep.Usable, ",") != "ok" || strings.Join(rep.Failed, ",") != "boom" {
		t.Fatalf("usable=%v failed=%v; want ok / boom", rep.Usable, rep.Failed)
	}
	if e := readFile(t, r.ReviewDir, "boom.err"); !strings.Contains(e, "provider exploded") {
		t.Fatalf("boom.err missing agent error: %q", e)
	}
}

// Runner postcondition: 0 usable of N selected exits non-zero (RunFailed) and
// says nothing was reviewed — never report success having reviewed nothing.
func TestRunnerPostconditionZeroUsable(t *testing.T) {
	f := &fakeOneshot{responses: map[string]string{"liar": fxLiar, "lazy": fxLazy}}
	r := newRunner(t, f)
	// Log may be called concurrently from the fan-out goroutines, so the sink
	// must be safe under -race.
	var logMu sync.Mutex
	var logs []string
	r.Log = func(format string, args ...any) {
		logMu.Lock()
		defer logMu.Unlock()
		logs = append(logs, fmt.Sprintf(format, args...))
	}

	rep, err := r.Run(context.Background(), []Job{{"liar", "liar"}, {"lazy", "lazy"}})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.RunFailed {
		t.Fatalf("RunFailed must be true when 0 of N are usable")
	}
	if len(rep.Usable) != 0 {
		t.Fatalf("usable must be empty, got %v", rep.Usable)
	}
	if !anyContains(logs, "0 of 2 selected perspectives produced a usable report") {
		t.Fatalf("log missing nothing-reviewed reason: %v", logs)
	}
}

// max_parallel bounds the fan-out concurrency (asserted under -race).
func TestRunMaxParallelBounds(t *testing.T) {
	responses := map[string]string{}
	var jobs []Job
	for i := 0; i < 12; i++ {
		slug := "p" + string(rune('a'+i))
		responses[slug] = strings.Replace(fxValid, "VERDICT valid:", "VERDICT "+slug+":", 1)
		jobs = append(jobs, Job{slug, slug})
	}
	f := &fakeOneshot{responses: responses, hold: 20 * time.Millisecond}
	r := newRunner(t, f)
	r.MaxParallel = 3

	rep, err := r.Run(context.Background(), jobs)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Usable) != 12 {
		t.Fatalf("expected 12 usable, got %d", len(rep.Usable))
	}
	if got := atomicMax(f); got > 3 {
		t.Fatalf("max in-flight = %d; must not exceed MaxParallel=3", got)
	}
	if got := atomicMax(f); got < 2 {
		t.Fatalf("max in-flight = %d; expected real concurrency (>=2)", got)
	}
}

// A prior SYNTHESIS.md (possibly carrying human AGREE/REJECT decisions) is
// archived before a new run overwrites it, and a valid synthesis is written.
func TestSynthesisArchivesPrior(t *testing.T) {
	synth := "### F1 [MAJOR] merged (`a.go:1`)\n- perspectives: valid\n- decision: PENDING\n\nVERDICT synthesis: blockers=0 majors=1 minors=0 (deduped from 1)"
	f := &fakeOneshot{responses: map[string]string{"valid": fxValid}}
	r := newRunner(t, f)
	r.Synthesize = true
	// SynthPrompt yields a fixed token the fake maps to a valid synthesis report.
	r.SynthPrompt = func(paths []string) string { return "SYNTH" }
	f.responses["SYNTH"] = synth

	// Seed a prior decision queue.
	if err := os.MkdirAll(r.ReviewDir, 0o755); err != nil {
		t.Fatal(err)
	}
	prior := filepath.Join(r.ReviewDir, "SYNTHESIS.md")
	if err := os.WriteFile(prior, []byte("OLD QUEUE decision: AGREE"), 0o644); err != nil {
		t.Fatal(err)
	}

	rep, err := r.Run(context.Background(), []Job{{"valid", "valid"}})
	if err != nil {
		t.Fatal(err)
	}
	if rep.RunFailed || !rep.Synthesized {
		t.Fatalf("expected synthesized run, got RunFailed=%v Synthesized=%v", rep.RunFailed, rep.Synthesized)
	}

	// New SYNTHESIS.md is the fresh queue; the old one survives as SYNTHESIS.<epoch>.md.
	if got := readFile(t, r.ReviewDir, "SYNTHESIS.md"); !strings.Contains(got, "VERDICT synthesis:") {
		t.Fatalf("SYNTHESIS.md is not the new queue: %q", got)
	}
	archived := filepath.Join(r.ReviewDir, "SYNTHESIS.1700000000.md")
	if b, err := os.ReadFile(archived); err != nil || !strings.Contains(string(b), "OLD QUEUE") {
		t.Fatalf("prior decision queue was not archived to %s (err=%v)", archived, err)
	}
}

// A synthesis output that fails the SAME postcondition is rejected and marks the
// run failed even though the perspective reports were usable.
func TestSynthesisRejectedFailsRun(t *testing.T) {
	badSynth := "### F1 [MAJOR] a\n### F2 [MAJOR] b\n\nVERDICT synthesis: blockers=0 majors=1 minors=0" // claims 1, body has 2
	f := &fakeOneshot{responses: map[string]string{"valid": fxValid, "SYNTH": badSynth}}
	r := newRunner(t, f)
	r.Synthesize = true
	r.SynthPrompt = func(paths []string) string { return "SYNTH" }

	rep, err := r.Run(context.Background(), []Job{{"valid", "valid"}})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Synthesized {
		t.Fatalf("synthesis should have been rejected")
	}
	if !rep.RunFailed {
		t.Fatalf("a rejected synthesis over usable reports must fail the run")
	}
	mustExist(t, r.ReviewDir, "SYNTHESIS.rejected")
	mustAbsent(t, r.ReviewDir, "SYNTHESIS.md")
}

// --- helpers ---

func atomicMax(f *fakeOneshot) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.maxFlight
}

func mustExist(t *testing.T, dir string, names ...string) {
	t.Helper()
	for _, n := range names {
		if _, err := os.Stat(filepath.Join(dir, n)); err != nil {
			t.Fatalf("expected %s to exist: %v", n, err)
		}
	}
}

func mustAbsent(t *testing.T, dir string, names ...string) {
	t.Helper()
	for _, n := range names {
		if _, err := os.Stat(filepath.Join(dir, n)); err == nil {
			t.Fatalf("expected %s to be absent", n)
		}
	}
}

func readFile(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

func anyContains(ss []string, sub string) bool {
	for _, s := range ss {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
