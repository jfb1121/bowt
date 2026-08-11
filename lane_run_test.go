package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jfb1121/bowt/internal/agent"
	"github.com/jfb1121/bowt/internal/lock"
	"github.com/jfb1121/bowt/internal/spawn"
	"github.com/jfb1121/bowt/internal/state"
)

// fakeAgent implements agent.Agent so the launcher/supervisor logic runs without
// launching a real CLI. Only Headless is exercised here; headlessFn is the hook.
type fakeAgent struct {
	caps       agent.Capabilities
	headlessFn func(ctx context.Context, prompt string, opts agent.Opts) error
}

func (f fakeAgent) Caps() agent.Capabilities { return f.caps }
func (f fakeAgent) Session(context.Context, string, agent.Opts) error {
	return nil
}
func (f fakeAgent) Oneshot(context.Context, string) (string, error) { return "", nil }
func (f fakeAgent) Headless(ctx context.Context, prompt string, opts agent.Opts) error {
	if f.headlessFn != nil {
		return f.headlessFn(ctx, prompt, opts)
	}
	return nil
}

// fakeSpawner stands in for the real fork: it records the re-exec argv and,
// when publish is set, simulates the supervisor by reading the spec back off
// disk and INSERTing the lane row — exercising the real WriteSpec→ReadSpecAt
// handoff without a process.
type fakeSpawner struct {
	ls      state.LaneStore
	publish bool
	pid     int
	calls   []spawnCall
}

type spawnCall struct {
	exe     string
	args    []string
	dir     string
	logPath string
}

func (f *fakeSpawner) spawn(exe string, args []string, dir, logPath string) (int, error) {
	f.calls = append(f.calls, spawnCall{exe, args, dir, logPath})
	if f.publish {
		spec, err := spawn.ReadSpecAt(args[1]) // args = [_lane-run, specPath]
		if err != nil {
			return 0, err
		}
		if err := f.ls.AddLane(laneFromSpec(spec)); err != nil {
			return 0, err
		}
	}
	return f.pid, nil
}

func sampleSpec(wt string) spawn.LaneSpec {
	return spawn.LaneSpec{
		ID: "lane-t", Repo: "demo", Branch: "slice/x", Worktree: wt,
		Agent: "claude", Model: "opus", Mode: "impl",
		BriefPath: "subagent/PROMPT.md", WritebackDir: spawn.DefaultWritebackDir,
		LogPath: spawn.LaneLogPath(wt, "lane-t"), Prompt: "P",
	}
}

// TestLauncherPublishesAndReports: with the (faked) supervisor publishing the
// row, the launcher writes the spec, re-execs the hidden verb with the right
// argv/dir/log, and returns cleanly once it sees the row.
func TestLauncherPublishesAndReports(t *testing.T) {
	wt := t.TempDir()
	ls, err := state.OpenLanesAt(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	spec := sampleSpec(wt)
	sp := &fakeSpawner{ls: ls, publish: true, pid: 4321}

	if err := runHeadlessLaunch(sp, ls, "/path/to/bowt", spec, defaultLaunchConfig); err != nil {
		t.Fatalf("runHeadlessLaunch: %v", err)
	}

	if len(sp.calls) != 1 {
		t.Fatalf("spawn called %d times; want 1", len(sp.calls))
	}
	c := sp.calls[0]
	if c.exe != "/path/to/bowt" || c.dir != wt || c.logPath != spec.LogPath {
		t.Errorf("spawn call = %+v", c)
	}
	if len(c.args) != 2 || c.args[0] != spawn.LaneRunCommand || c.args[1] != spawn.SpecPath(wt, spec.ID) {
		t.Errorf("re-exec argv = %v; want [%s %s]", c.args, spawn.LaneRunCommand, spawn.SpecPath(wt, spec.ID))
	}
	// The spec file the supervisor reads must be on disk.
	if _, err := os.Stat(spawn.SpecPath(wt, spec.ID)); err != nil {
		t.Errorf("spec file not written: %v", err)
	}
	// And the row is visible.
	if _, ok, _ := ls.GetLane(spec.ID); !ok {
		t.Error("lane row not published")
	}
}

// TestLauncherHoldsNoExclusiveLock proves the launcher never takes the worktree
// lock across the fork: it succeeds even while another holder owns that lock
// (only the supervisor is meant to hold it).
func TestLauncherHoldsNoExclusiveLock(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // isolate ~/.bowt/locks
	wt := t.TempDir()
	held, err := lock.Acquire(wt)
	if err != nil {
		t.Fatalf("pre-acquire: %v", err)
	}
	defer func() { _ = held.Release() }()

	ls, err := state.OpenLanesAt(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	sp := &fakeSpawner{ls: ls, publish: true, pid: 1}
	if err := runHeadlessLaunch(sp, ls, "bowt", sampleSpec(wt), defaultLaunchConfig); err != nil {
		t.Fatalf("launcher must not contend for the worktree lock; got %v", err)
	}
}

// TestLauncherTimesOutWhenSupervisorSilent: no published row within the bounded
// poll → a clear "did not publish" error that points at the log.
func TestLauncherTimesOutWhenSupervisorSilent(t *testing.T) {
	wt := t.TempDir()
	ls, err := state.OpenLanesAt(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	sp := &fakeSpawner{ls: ls, publish: false, pid: 9}
	cfg := launchConfig{attempts: 3, interval: time.Millisecond}
	err = runHeadlessLaunch(sp, ls, "bowt", sampleSpec(wt), cfg)
	if err == nil {
		t.Fatal("want a timeout error when the supervisor never publishes")
	}
	if !strings.Contains(err.Error(), "did not publish") {
		t.Errorf("error = %q; want a 'did not publish' message", err)
	}
}

// TestSupervisorHoldsLockInsertsThenUpdates drives the supervisor core in-process
// with a real tmp DB + real lock + a fake agent. The fake agent asserts the
// worktree lock is HELD while it runs (a second Acquire is busy); afterwards the
// row is at a terminal status and the lock is released.
func TestSupervisorHoldsLockInsertsThenUpdates(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // isolate ~/.bowt/locks
	wt := t.TempDir()
	ls, err := state.OpenLanesAt(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	// impl mode + a STATUS.md present → the terminal status is review.
	wb := filepath.Join(wt, spawn.DefaultWritebackDir)
	if err := os.MkdirAll(wb, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wb, "STATUS.md"), []byte("done\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	spec := sampleSpec(wt)
	ran := false
	ag := fakeAgent{
		caps: agent.Capabilities{Name: "claude", SupportsHeadless: true},
		headlessFn: func(_ context.Context, _ string, _ agent.Opts) error {
			ran = true
			// The row must already be INSERTed (status impl) by the time the agent runs.
			if l, ok, _ := ls.GetLane(spec.ID); !ok || l.Status != state.StatusImpl {
				t.Errorf("row not INSERTed as impl before the agent ran: ok=%v status=%q", ok, l.Status)
			}
			// The supervisor must hold the worktree lock for the agent's lifetime.
			if _, err := lock.Acquire(wt); err == nil {
				t.Error("worktree lock not held while the agent runs")
			}
			return nil
		},
	}

	if err := runSupervisor(ls, ag, spec, lock.Acquire, os.Stderr, os.Stderr); err != nil {
		t.Fatalf("runSupervisor: %v", err)
	}
	if !ran {
		t.Fatal("agent never ran")
	}
	// Terminal UPDATE landed.
	l, ok, _ := ls.GetLane(spec.ID)
	if !ok || l.Status != state.StatusReview {
		t.Fatalf("after run: ok=%v status=%q; want review", ok, l.Status)
	}
	// Lock released on return.
	if got, err := lock.Acquire(wt); err != nil {
		t.Fatalf("lock should be free after the supervisor returns: %v", err)
	} else {
		_ = got.Release()
	}
}

// TestSupervisorNonZeroExitFails: an agent that exits non-zero → failed, and
// the terminal UPDATE still lands (both the happy and error paths write it).
func TestSupervisorNonZeroExitFails(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	wt := t.TempDir()
	ls, err := state.OpenLanesAt(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	spec := sampleSpec(wt)
	ag := fakeAgent{
		caps: agent.Capabilities{Name: "claude", SupportsHeadless: true},
		headlessFn: func(_ context.Context, _ string, _ agent.Opts) error {
			return exec.Command("false").Run() // *exec.ExitError, code 1
		},
	}
	// runSupervisor returns the agent's error, but must still write the terminal status.
	_ = runSupervisor(ls, ag, spec, lock.Acquire, os.Stderr, os.Stderr)
	if l, ok, _ := ls.GetLane(spec.ID); !ok || l.Status != state.StatusFailed {
		t.Fatalf("after failed run: ok=%v status=%q; want failed", ok, l.Status)
	}
}

// --- ONE integration test that actually forks the hidden `_lane-run` ---------

// TestLaneRunForkHoldsLock forks a real `_lane-run` supervisor (the test binary
// re-execs itself into TestLaneRunHelperProcess, which runs runSupervisor with a
// blocking FAKE agent — never a real CLI). It proves the detached child holds
// the worktree lock while the agent runs and the kernel releases it on exit.
func TestLaneRunForkHoldsLock(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	wt := t.TempDir()

	spec := sampleSpec(wt)
	specPath, err := spawn.WriteSpec(spec)
	if err != nil {
		t.Fatal(err)
	}
	started := filepath.Join(wt, "started")
	release := filepath.Join(wt, "release")
	// The helper needs a STATUS.md present so impl mode ends at 'review'.
	wb := filepath.Join(wt, spawn.DefaultWritebackDir)
	if err := os.MkdirAll(wb, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wb, "STATUS.md"), []byte("ok\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestLaneRunHelperProcess")
	cmd.Env = append(os.Environ(),
		"BOWT_WANT_LANE_HELPER=1",
		"HOME="+home,
		"BOWT_HELPER_SPEC="+specPath,
		"BOWT_HELPER_STARTED="+started,
		"BOWT_HELPER_RELEASE="+release,
	)
	if err := cmd.Start(); err != nil {
		t.Fatalf("fork helper: %v", err)
	}

	waitForFile(t, started, 5*time.Second) // agent is now running inside the child

	// While the child supervises the agent, the worktree lock must be BUSY.
	if l, err := lock.Acquire(wt); err == nil {
		_ = l.Release()
		t.Fatal("worktree lock should be held by the forked supervisor")
	}

	// Let the agent finish; the child writes the terminal UPDATE and exits.
	if err := os.WriteFile(release, []byte("go\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("supervisor exited non-zero: %v", err)
	}

	// Lock released by the kernel on the child's exit.
	l, err := lock.Acquire(wt)
	if err != nil {
		t.Fatalf("lock should be free after the supervisor exits: %v", err)
	}
	_ = l.Release()

	// And the row the forked supervisor wrote is at its terminal status.
	ls, err := state.OpenLanes()
	if err != nil {
		t.Fatal(err)
	}
	if l, ok, _ := ls.GetLane(spec.ID); !ok || l.Status != state.StatusReview {
		t.Fatalf("forked supervisor row: ok=%v status=%q; want review", ok, l.Status)
	}
}

// TestLaneRunHelperProcess is the forked child for TestLaneRunForkHoldsLock. It
// is inert unless BOWT_WANT_LANE_HELPER=1, so it does nothing in a normal run.
func TestLaneRunHelperProcess(t *testing.T) {
	if os.Getenv("BOWT_WANT_LANE_HELPER") != "1" {
		return
	}
	spec, err := spawn.ReadSpecAt(os.Getenv("BOWT_HELPER_SPEC"))
	if err != nil {
		os.Exit(2)
	}
	ls, err := state.OpenLanes()
	if err != nil {
		os.Exit(3)
	}
	startedFile := os.Getenv("BOWT_HELPER_STARTED")
	releaseFile := os.Getenv("BOWT_HELPER_RELEASE")
	ag := fakeAgent{
		caps: agent.Capabilities{Name: "claude", SupportsHeadless: true},
		headlessFn: func(context.Context, string, agent.Opts) error {
			_ = os.WriteFile(startedFile, []byte("1"), 0o644) // signal: lock is held, agent running
			for i := 0; i < 200; i++ {                        // wait for the parent to release us
				if _, err := os.Stat(releaseFile); err == nil {
					return nil
				}
				time.Sleep(25 * time.Millisecond)
			}
			return nil
		},
	}
	if err := runSupervisor(ls, ag, spec, lock.Acquire, os.Stderr, os.Stderr); err != nil {
		os.Exit(4)
	}
	os.Exit(0)
}

func waitForFile(t *testing.T, path string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
}
