package gate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jfb1121/bowt/internal/run"
)

// fixture makes a worktree dir with a config dir holding gate.sh (body), and
// returns (worktree, configDir). The real Exec runner runs the real script, so
// exit codes and BOWT_CHECK parsing are exercised end to end.
func fixture(t *testing.T, body string) (worktree, configDir string) {
	t.Helper()
	worktree = t.TempDir()
	configDir = filepath.Join(worktree, ".bowt")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/usr/bin/env bash\n" + body
	if err := os.WriteFile(filepath.Join(configDir, HookFile), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return worktree, configDir
}

// fixedClock returns a Now func advancing 2s per call, so DurationS is stable.
func fixedClock() func() time.Time {
	base := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	n := 0
	return func() time.Time {
		t := base.Add(time.Duration(n) * time.Second)
		n += 2
		return t
	}
}

func baseParams(worktree, configDir string) Params {
	return Params{
		Runner:      run.Exec{}, // capture stderr too (nil Stderr) so tests stay quiet
		ConfigDir:   configDir,
		Worktree:    worktree,
		Repo:        "demo",
		Branch:      "feature/x",
		Commit:      "0123456789abcdef0123456789abcdef01234567",
		CommitShort: "0123456",
		Dirty:       true,
		Scope:       Scope{Mode: "full", Value: ""},
		Env:         []string{"BOWT_BRANCH=feature/x"},
		Now:         fixedClock(),
	}
}

// readVerdict loads and decodes the gate.json the run wrote.
func readVerdict(t *testing.T, worktree string) Result {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(worktree, OutputDir, OutputFile))
	if err != nil {
		t.Fatalf("read gate.json: %v", err)
	}
	var r Result
	if err := json.Unmarshal(data, &r); err != nil {
		t.Fatalf("decode gate.json: %v", err)
	}
	return r
}

// A passing hook that emits two BOWT_CHECK pass lines → overall pass, exit 0,
// and gate.json agrees; the target (commit/worktree/branch/dirty) is recorded.
func TestPassingHookWithChecks(t *testing.T) {
	wt, cfg := fixture(t, `
echo "BOWT_CHECK lint pass 0"
echo "running tests..." 1>&2
echo "BOWT_CHECK test pass 0 42 passed"
exit 0
`)
	p := baseParams(wt, cfg)
	res, err := Run(p)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Overall != Pass {
		t.Fatalf("overall = %q; want pass", res.Overall)
	}
	if res.ExitCode() != 0 {
		t.Fatalf("exit = %d; want 0", res.ExitCode())
	}
	if len(res.Checks) != 2 {
		t.Fatalf("checks = %d; want 2", len(res.Checks))
	}
	if res.Checks[1].Name != "test" || res.Checks[1].Detail != "42 passed" {
		t.Errorf("second check = %+v; want name=test detail=%q", res.Checks[1], "42 passed")
	}
	// Target recorded.
	if res.Commit != p.Commit || res.CommitShort != p.CommitShort {
		t.Errorf("commit = %q/%q; want %q/%q", res.Commit, res.CommitShort, p.Commit, p.CommitShort)
	}
	if res.Worktree != wt || res.Branch != "feature/x" || !res.Dirty {
		t.Errorf("target mismatch: %+v", res)
	}
	if res.SchemaVersion != SchemaVersion {
		t.Errorf("schema_version = %d; want %d", res.SchemaVersion, SchemaVersion)
	}
	// gate.json on disk agrees with the returned Result.
	disk := readVerdict(t, wt)
	if disk.Overall != res.Overall || len(disk.Checks) != len(res.Checks) {
		t.Errorf("gate.json disagrees with result: disk=%+v result=%+v", disk, res)
	}
}

// A hook that emits a failing check (even with exit 0) → overall fail, exit 1.
func TestFailingCheckLine(t *testing.T) {
	wt, cfg := fixture(t, `
echo "BOWT_CHECK lint pass 0"
echo "BOWT_CHECK test fail 2 3 tests failed"
exit 0
`)
	res, err := Run(baseParams(wt, cfg))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Overall != Fail {
		t.Fatalf("overall = %q; want fail", res.Overall)
	}
	if res.ExitCode() != 1 {
		t.Fatalf("exit = %d; want 1", res.ExitCode())
	}
	disk := readVerdict(t, wt)
	if disk.Overall != Fail {
		t.Errorf("gate.json overall = %q; want fail", disk.Overall)
	}
}

// A hook with NO BOWT_CHECK lines that exits non-zero → one synthesized check
// from the exit code, overall fail, exit 1.
func TestNoCheckLinesUsesExitCode(t *testing.T) {
	wt, cfg := fixture(t, `
echo "make check failed" 1>&2
exit 3
`)
	res, err := Run(baseParams(wt, cfg))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Overall != Fail {
		t.Fatalf("overall = %q; want fail", res.Overall)
	}
	if len(res.Checks) != 1 || res.Checks[0].Name != "gate" || res.Checks[0].ExitCode != 3 {
		t.Fatalf("synthesized check = %+v; want name=gate exit=3", res.Checks)
	}
}

// A hook that passes its checks but exits non-zero → overall fail (both the
// exit code AND the checks must be clean).
func TestHookExitOverridesPassingChecks(t *testing.T) {
	wt, cfg := fixture(t, `
echo "BOWT_CHECK lint pass 0"
exit 1
`)
	res, err := Run(baseParams(wt, cfg))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Overall != Fail {
		t.Fatalf("overall = %q; want fail (hook exit 1 despite passing check)", res.Overall)
	}
}

// A missing gate.sh is a hard error naming the hook — never a silent pass.
func TestMissingHookIsHardError(t *testing.T) {
	wt := t.TempDir()
	cfg := filepath.Join(wt, ".bowt")
	if err := os.MkdirAll(cfg, 0o755); err != nil {
		t.Fatal(err)
	}
	p := baseParams(wt, cfg)
	if _, err := Run(p); err == nil {
		t.Fatal("missing gate.sh should be a hard error")
	}
	// And an empty config dir path is likewise a hard error.
	p.ConfigDir = ""
	if _, err := Run(p); err == nil {
		t.Fatal("empty config dir should be a hard error")
	}
}

// The scope is exposed to the hook (BOWT_GATE_SCOPE*) and recorded verbatim.
func TestScopePassedAndRecorded(t *testing.T) {
	wt, cfg := fixture(t, `
if [ "$BOWT_GATE_SCOPE" = "paths" ] && [ "$BOWT_GATE_SCOPE_VALUE" = "internal/gate" ]; then
  echo "BOWT_CHECK scope pass 0"
else
  echo "BOWT_CHECK scope fail 1 wrong scope env"
fi
`)
	p := baseParams(wt, cfg)
	p.Scope = Scope{Mode: "paths", Value: "internal/gate"}
	res, err := Run(p)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Overall != Pass {
		t.Fatalf("scope env not visible to hook: %+v", res.Checks)
	}
	if res.Scope.Mode != "paths" || res.Scope.Value != "internal/gate" {
		t.Errorf("scope recorded = %+v; want {paths internal/gate}", res.Scope)
	}
}

// exitCode recovers a real child exit code through the Exec runner's %w wrap.
func TestExitCodeThroughRunner(t *testing.T) {
	_, err := run.Exec{}.Run(t.TempDir(), "bash", "-c", "exit 7")
	if got := exitCode(err); got != 7 {
		t.Fatalf("exitCode = %d; want 7", got)
	}
	if got := exitCode(nil); got != 0 {
		t.Fatalf("exitCode(nil) = %d; want 0", got)
	}
}

// TestReadResult round-trips a written verdict, treats a missing file as ok=false
// (never gated), and errors on a corrupt one — the read side the G4 cockpit uses.
func TestReadResult(t *testing.T) {
	wt := t.TempDir()

	// Missing gate.json is the common "never gated" case: ok=false, no error.
	if _, ok, err := ReadResult(wt); err != nil || ok {
		t.Fatalf("missing verdict: ok=%v err=%v; want ok=false nil", ok, err)
	}

	// A written verdict round-trips through writeResult → ReadResult.
	want := &Result{SchemaVersion: SchemaVersion, Overall: Pass, Repo: "demo",
		Branch: "slice/x", Worktree: wt, Commit: "abc123", CommitShort: "abc123"}
	if err := writeResult(wt, want); err != nil {
		t.Fatalf("writeResult: %v", err)
	}
	got, ok, err := ReadResult(wt)
	if err != nil || !ok {
		t.Fatalf("ReadResult: ok=%v err=%v", ok, err)
	}
	if got.Overall != Pass || got.Commit != "abc123" {
		t.Errorf("round-trip mismatch: %+v", got)
	}

	// A corrupt gate.json is a real error, not a silent miss.
	if err := os.WriteFile(filepath.Join(wt, OutputDir, OutputFile), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadResult(wt); err == nil {
		t.Fatal("corrupt verdict should error")
	}
}
