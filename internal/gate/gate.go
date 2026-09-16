// Package gate runs a worktree's configured verification hook and records a
// machine-readable verdict an orchestrator can trust without parsing prose.
//
// The repo owns what "gating" means: gate invokes <configDir>/gate.sh (Django
// might put `makemigrations --check` there; a Go repo puts `make check`),
// streaming its progress to stderr while capturing stdout. The hook reports
// per-check results by printing BOWT_CHECK lines on stdout; gate collects them,
// folds in the hook's overall exit code, and writes the verdict atomically to
// <worktree>/.bowt/gate.json. The exclusive per-worktree lock and target
// resolution (commit/branch/dirty) live in the command layer that calls Run.
package gate

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jfb1121/bowt/internal/run"
)

// SchemaVersion is the gate.json schema version. Bump it on any incompatible
// change to the shape below so readers can refuse a version they don't grok.
const SchemaVersion = 1

// HookFile is the repo-provided gate hook, resolved inside the config dir.
const HookFile = "gate.sh"

// OutputDir / OutputFile locate the verdict written into the worktree.
const (
	OutputDir  = ".bowt"
	OutputFile = "gate.json"
)

// checkPrefix marks a per-check line the hook prints on stdout:
//
//	BOWT_CHECK <name> <pass|fail> <exit_code> [detail...]
const checkPrefix = "BOWT_CHECK"

// Status is the pass/fail verdict for a single check and for the run overall.
type Status string

const (
	Pass Status = "pass"
	Fail Status = "fail"
)

// Check is one collected result. Detail is optional free text.
type Check struct {
	Name     string `json:"name"`
	Status   Status `json:"status"`
	ExitCode int    `json:"exit_code"`
	Detail   string `json:"detail,omitempty"`
}

// Scope records what verification was requested, verbatim. Narrowing checks to
// the scope is the hook's job (via BOWT_GATE_SCOPE*); core only records it so a
// verdict is reproducible.
type Scope struct {
	Mode  string `json:"mode"`
	Value string `json:"value"`
}

// Result is the machine-readable verdict written to gate.json and emitted as
// the command's JSON. Field order matches the on-disk shape.
type Result struct {
	SchemaVersion int       `json:"schema_version"`
	Overall       Status    `json:"overall"`
	Repo          string    `json:"repo"`
	Branch        string    `json:"branch"`
	Worktree      string    `json:"worktree"`
	Commit        string    `json:"commit"`
	CommitShort   string    `json:"commit_short"`
	Dirty         bool      `json:"dirty"`
	Scope         Scope     `json:"scope"`
	StartedAt     time.Time `json:"started_at"`
	FinishedAt    time.Time `json:"finished_at"`
	DurationS     float64   `json:"duration_s"`
	Checks        []Check   `json:"checks"`
}

// ExitCode is the process exit code that mirrors the verdict: 0 on pass, 1 on
// fail. The command layer calls os.Exit(r.ExitCode()).
func (r *Result) ExitCode() int {
	if r.Overall == Pass {
		return 0
	}
	return 1
}

// Params are the already-resolved inputs Run needs. The command layer resolves
// the target (commit/branch/dirty), the config dir, and the BOWT_* env, then
// hands them here; Run stays free of git and lock concerns so it is unit
// testable with a real gate.sh and a fake or real Runner.
type Params struct {
	Runner      run.Runner
	ConfigDir   string   // dir holding gate.sh (config.Dir of the main repo)
	Worktree    string   // absolute worktree path: hook cwd + where gate.json lands
	Repo        string   // repo (registry) name
	Branch      string   // checked-out branch ("" if detached)
	Commit      string   // full HEAD SHA
	CommitShort string   // abbreviated HEAD SHA
	Dirty       bool     // uncommitted changes present
	Scope       Scope    // requested scope, recorded verbatim
	Env         []string // BOWT_* env injected into the hook
	// Now is the clock seam; nil means time.Now.
	Now func() time.Time
}

// Run executes the gate hook and writes the verdict. A missing gate.sh is a
// hard error (never a silent pass). The returned Result is written to
// <worktree>/.bowt/gate.json atomically before Run returns; a non-nil error
// means the run could not be performed (the verdict was not written).
func Run(p Params) (*Result, error) {
	now := p.Now
	if now == nil {
		now = time.Now
	}

	if p.ConfigDir == "" {
		return nil, fmt.Errorf("gate hook not found: no .bowt/ config dir in the repo — create one with a %s", HookFile)
	}
	hook := filepath.Join(p.ConfigDir, HookFile)
	if _, err := os.Stat(hook); err != nil {
		return nil, fmt.Errorf("gate hook not found: %s (create it to define what gating means for this repo)", hook)
	}

	started := now()

	// Invoke `env KEY=VAL… bash gate.sh` through the Runner, exactly as the
	// lifecycle hooks do: the env travels through the same boundary, stdout is
	// captured (for BOWT_CHECK lines), and the Exec runner streams stderr live.
	// The scope is exposed so the hook can narrow its checks.
	env := append([]string(nil), p.Env...)
	env = append(env,
		"BOWT_GATE_SCOPE="+p.Scope.Mode,
		"BOWT_GATE_SCOPE_VALUE="+p.Scope.Value,
	)
	argv := append(env, "bash", hook)
	stdout, runErr := p.Runner.Run(p.Worktree, "env", argv...)

	hookExit := exitCode(runErr)
	checks := parseChecks(stdout)
	if len(checks) == 0 {
		// No per-check lines: record a single check from the hook's overall exit.
		checks = []Check{{Name: "gate", Status: statusFor(hookExit == 0), ExitCode: hookExit}}
	}

	// overall = hook exited 0 AND every collected check passed.
	overall := Pass
	if hookExit != 0 {
		overall = Fail
	}
	for _, c := range checks {
		if c.Status != Pass {
			overall = Fail
			break
		}
	}

	finished := now()
	res := &Result{
		SchemaVersion: SchemaVersion,
		Overall:       overall,
		Repo:          p.Repo,
		Branch:        p.Branch,
		Worktree:      p.Worktree,
		Commit:        p.Commit,
		CommitShort:   p.CommitShort,
		Dirty:         p.Dirty,
		Scope:         p.Scope,
		StartedAt:     started,
		FinishedAt:    finished,
		DurationS:     finished.Sub(started).Seconds(),
		Checks:        checks,
	}

	if err := writeResult(p.Worktree, res); err != nil {
		return res, err
	}
	return res, nil
}

// parseChecks extracts BOWT_CHECK lines from the hook's stdout. Malformed lines
// (fewer than 4 fields, or an unrecognized status) are skipped rather than
// failing the run — a check the hook could not report is simply not recorded.
func parseChecks(stdout string) []Check {
	var checks []Check
	for _, line := range strings.Split(stdout, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 || fields[0] != checkPrefix {
			continue
		}
		status := Status(fields[2])
		if status != Pass && status != Fail {
			continue
		}
		code, err := strconv.Atoi(fields[3])
		if err != nil {
			continue
		}
		checks = append(checks, Check{
			Name:     fields[1],
			Status:   status,
			ExitCode: code,
			Detail:   strings.Join(fields[4:], " "),
		})
	}
	return checks
}

// exitCode recovers a process exit code from a Runner error. The Exec runner
// wraps the child's *exec.ExitError with %w, so errors.As reaches it; nil means
// success (0), and any non-ExitError failure is a generic 1.
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return 1
}

func statusFor(ok bool) Status {
	if ok {
		return Pass
	}
	return Fail
}

// writeResult writes res to <worktree>/.bowt/gate.json atomically: marshal,
// write a temp file in the same dir, fsync-free rename over the target. A
// reader therefore never observes a half-written verdict.
// ReadResult loads the verdict a prior gate run wrote to
// <worktree>/.bowt/gate.json. It is the read side of writeResult, used by the G4
// cockpit to surface a worktree's gate verdict even when no lane row cached it.
// A missing file is the common case (never gated) and returns ok=false with no
// error; only a present-but-unreadable/corrupt file is an error.
func ReadResult(worktree string) (Result, bool, error) {
	path := filepath.Join(worktree, OutputDir, OutputFile)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Result{}, false, nil
		}
		return Result{}, false, fmt.Errorf("read gate verdict %s: %w", path, err)
	}
	var res Result
	if err := json.Unmarshal(data, &res); err != nil {
		return Result{}, false, fmt.Errorf("decode gate verdict %s: %w", path, err)
	}
	return res, true, nil
}

func writeResult(worktree string, res *Result) error {
	dir := filepath.Join(worktree, OutputDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	data, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal gate result: %w", err)
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(dir, OutputFile+".*.tmp")
	if err != nil {
		return fmt.Errorf("create temp gate file: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("write gate result: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("close temp gate file: %w", err)
	}
	final := filepath.Join(dir, OutputFile)
	if err := os.Rename(tmpName, final); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("finalize %s: %w", final, err)
	}
	return nil
}
