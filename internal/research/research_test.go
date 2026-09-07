package research

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeLauncher stands in for ag.Headless: it optionally writes each task's out
// file (so the collection step sees a finding) and tracks concurrency so a test
// can prove Run never exceeds the bound. NO real agent, no network.
type fakeLauncher struct {
	write  bool // write the out file (simulate a successful research pass)
	errFor map[string]error
	hold   time.Duration // how long each launch stays "in flight"

	mu        sync.Mutex
	inFlight  int
	maxFlight int
	launched  []string
}

func (f *fakeLauncher) run(_ context.Context, t Task) error {
	f.mu.Lock()
	f.inFlight++
	if f.inFlight > f.maxFlight {
		f.maxFlight = f.inFlight
	}
	f.launched = append(f.launched, t.ID)
	f.mu.Unlock()

	if f.hold > 0 {
		time.Sleep(f.hold)
	}

	f.mu.Lock()
	f.inFlight--
	f.mu.Unlock()

	if err := f.errFor[t.ID]; err != nil {
		return err
	}
	if f.write {
		if err := os.WriteFile(t.OutPath, []byte("# findings "+t.ID+"\n"), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func newRunner(t *testing.T, f *fakeLauncher) *Runner {
	t.Helper()
	return &Runner{
		OutDir:      filepath.Join(t.TempDir(), DefaultOutDir),
		Concurrency: 2,
		Launch:      f.run,
		SynthTask: func(paths []string, outPath, logPath string) Task {
			return Task{ID: "synthesis", Prompt: SynthesisPrompt(paths, outPath), OutPath: outPath, LogPath: logPath}
		},
	}
}

// tasks builds n Tasks writing into the runner's out dir.
func tasks(r *Runner, n int) []Task {
	ts := make([]Task, n)
	for i := 0; i < n; i++ {
		id := TaskID(i, n)
		ts[i] = Task{
			ID:      id,
			Prompt:  "research " + id,
			OutPath: filepath.Join(r.OutDir, id+".md"),
			LogPath: filepath.Join(r.OutDir, id+".log"),
		}
	}
	return ts
}

// --queries wins over --n (one agent per query); otherwise --n angled copies;
// n<=1 leaves the brief untouched.
func TestTaskBriefsFanOut(t *testing.T) {
	if got := TaskBriefs("b", nil, 1); len(got) != 1 || got[0] != "b" {
		t.Fatalf("n=1 should pass the brief through unchanged, got %v", got)
	}
	if got := TaskBriefs("b", nil, 3); len(got) != 3 {
		t.Fatalf("n=3 should yield 3 briefs, got %d", len(got))
	} else {
		for i, b := range got {
			if !strings.Contains(b, "ANGLE") || !strings.HasPrefix(b, "b") {
				t.Fatalf("angle %d missing directive or brief: %q", i, b)
			}
		}
	}
	// queries override n entirely: 2 queries + n=5 → 2 agents, verbatim.
	q := []string{"query one", "query two"}
	got := TaskBriefs("b", q, 5)
	if len(got) != 2 || got[0] != "query one" || got[1] != "query two" {
		t.Fatalf("queries must win over n and pass through verbatim, got %v", got)
	}
}

// TaskID zero-pads to the width of the fan-out (min two) so a directory listing
// sorts in launch order at any size.
func TestTaskIDWidth(t *testing.T) {
	if got := TaskID(0, 3); got != "r01" {
		t.Fatalf("TaskID(0,3) = %q, want r01", got)
	}
	if got := TaskID(9, 100); got != "r010" {
		t.Fatalf("TaskID(9,100) = %q, want r010 (sorts before r100)", got)
	}
	if got := TaskID(99, 100); got != "r100" {
		t.Fatalf("TaskID(99,100) = %q, want r100", got)
	}
}

// A stale findings file from a prior run into the same out dir must NOT be
// counted as this run's success when this run's agent writes nothing: runOne
// clears the target first, so existence is a true per-run postcondition.
func TestRunClearsStaleFindings(t *testing.T) {
	f := &fakeLauncher{write: false} // this run's agent writes nothing
	r := newRunner(t, f)
	if err := os.MkdirAll(r.OutDir, 0o755); err != nil {
		t.Fatal(err)
	}
	ts := tasks(r, 1)
	// Seed a leftover findings file at the task's out path.
	if err := os.WriteFile(ts[0].OutPath, []byte("STALE"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := r.Run(context.Background(), ts)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Written) != 0 || len(res.Missing) != 1 {
		t.Fatalf("stale file must not count as written: Written=%v Missing=%v", res.Written, res.Missing)
	}
	if _, statErr := os.Stat(ts[0].OutPath); statErr == nil {
		t.Fatalf("stale findings file should have been cleared")
	}
}

// Run collects exactly the findings files that landed and reports the rest as
// missing, and it launches one agent per task.
func TestRunCollectsWritten(t *testing.T) {
	f := &fakeLauncher{write: true, errFor: map[string]error{"r03": errors.New("boom")}}
	r := newRunner(t, f)
	ts := tasks(r, 4)

	res, err := r.Run(context.Background(), ts)
	if err != nil {
		t.Fatal(err)
	}
	if res.Tasks != 4 {
		t.Fatalf("Tasks = %d, want 4", res.Tasks)
	}
	if len(f.launched) != 4 {
		t.Fatalf("launched %d agents, want 4", len(f.launched))
	}
	if len(res.Written) != 3 {
		t.Fatalf("Written = %v, want 3 files", res.Written)
	}
	if len(res.Missing) != 1 || !strings.HasSuffix(res.Missing[0], "r03.md") {
		t.Fatalf("Missing = %v, want [r03.md]", res.Missing)
	}
}

// The concurrency bound is load-bearing (OOM protection): Run must never have
// more than Concurrency agents in flight at once. Asserted under -race.
func TestRunConcurrencyBound(t *testing.T) {
	f := &fakeLauncher{write: true, hold: 20 * time.Millisecond}
	r := newRunner(t, f)
	r.Concurrency = 2
	ts := tasks(r, 10)

	if _, err := r.Run(context.Background(), ts); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.maxFlight > 2 {
		t.Fatalf("max in-flight = %d; must not exceed Concurrency=2", f.maxFlight)
	}
	if f.maxFlight < 2 {
		t.Fatalf("max in-flight = %d; expected real concurrency (>=2)", f.maxFlight)
	}
}

// A zero/negative Concurrency falls back to the low default floor, never
// unbounded fan-out.
func TestRunDefaultConcurrencyFloor(t *testing.T) {
	f := &fakeLauncher{write: true, hold: 15 * time.Millisecond}
	r := newRunner(t, f)
	r.Concurrency = 0 // → DefaultConcurrency
	ts := tasks(r, 8)

	if _, err := r.Run(context.Background(), ts); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.maxFlight > DefaultConcurrency {
		t.Fatalf("max in-flight = %d; must not exceed DefaultConcurrency=%d", f.maxFlight, DefaultConcurrency)
	}
}

// --synthesize runs one final agent over the written findings and reports the
// synthesis file it wrote.
func TestRunSynthesizeWiring(t *testing.T) {
	f := &fakeLauncher{write: true}
	r := newRunner(t, f)
	r.Synthesize = true
	ts := tasks(r, 3)

	res, err := r.Run(context.Background(), ts)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Synthesized {
		t.Fatalf("expected Synthesized=true")
	}
	if !strings.HasSuffix(res.SynthPath, SynthesisFile) {
		t.Fatalf("SynthPath = %q, want …/%s", res.SynthPath, SynthesisFile)
	}
	if _, err := os.Stat(res.SynthPath); err != nil {
		t.Fatalf("synthesis file not written: %v", err)
	}
	// The synthesis agent is one launch beyond the 3 research tasks.
	if len(f.launched) != 4 {
		t.Fatalf("launched %d agents, want 4 (3 research + 1 synthesis)", len(f.launched))
	}
}

// Synthesis is skipped when no findings landed (nothing to synthesize).
func TestRunSynthesizeSkippedNoFindings(t *testing.T) {
	f := &fakeLauncher{write: false} // no agent writes a findings file
	r := newRunner(t, f)
	r.Synthesize = true
	ts := tasks(r, 2)

	res, err := r.Run(context.Background(), ts)
	if err != nil {
		t.Fatal(err)
	}
	if res.Synthesized || res.SynthPath != "" {
		t.Fatalf("synthesis must be skipped with no findings, got %+v", res)
	}
	if len(f.launched) != 2 {
		t.Fatalf("launched %d agents, want 2 (no synthesis agent)", len(f.launched))
	}
}
