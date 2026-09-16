package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/jfb1121/bowt/internal/agent"
	"github.com/jfb1121/bowt/internal/config"
	"github.com/jfb1121/bowt/internal/extension"
	"github.com/jfb1121/bowt/internal/lock"
	"github.com/jfb1121/bowt/internal/repo"
	"github.com/jfb1121/bowt/internal/state"
)

// execRoot runs the cobra tree with args, capturing stdout+stderr.
func execRoot(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := newRootCmd()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs(args)
	err := root.Execute()
	return buf.String(), err
}

// fixtureRepo creates a throwaway git repo, chdirs into it, and returns its
// registry name (the basename repo.Name() will report).
func fixtureRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(dir)
	if out, err := exec.Command("git", "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	name, err := repo.Name()
	if err != nil {
		t.Fatalf("repo.Name: %v", err)
	}
	return name
}

// seedStore opens a fresh store at a temp path and registers the given branches
// under repoName.
func seedStore(t *testing.T, repoName string, branches ...string) state.Store {
	t.Helper()
	st, err := state.OpenAt(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("OpenAt: %v", err)
	}
	for i, b := range branches {
		if err := st.Add(state.Worktree{
			Repo:    repoName,
			Branch:  b,
			Offset:  i + 1,
			Port:    3000 + i + 1,
			Path:    filepath.Join(t.TempDir(), b),
			Created: time.Now(),
		}); err != nil {
			t.Fatalf("seed %q: %v", b, err)
		}
	}
	return st
}

// The dynamic-completion payoff: branchNames returns the registered branches for
// the current repo, given a seeded store + a fixture git repo.
func TestBranchNamesFromRegistry(t *testing.T) {
	name := fixtureRepo(t)
	st := seedStore(t, name, "feature/login", "hotfix", "spike")

	got, err := branchNames(st)
	if err != nil {
		t.Fatalf("branchNames: %v", err)
	}
	sort.Strings(got)
	want := []string{"feature/login", "hotfix", "spike"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("branchNames = %v; want %v", got, want)
	}
}

// completeBranchArg past the first positional arg returns nothing and never
// falls back to file completion.
func TestCompleteBranchArgSecondArg(t *testing.T) {
	names, directive := completeBranchArg(nil, []string{"already"}, "")
	if names != nil {
		t.Errorf("names = %v; want nil for a second arg", names)
	}
	if directive != cobra.ShellCompDirectiveNoFileComp {
		t.Errorf("directive = %v; want NoFileComp", directive)
	}
}

// `bowt completion zsh` must emit a non-empty completion script that references
// bowt and cobra's dynamic-completion entrypoint.
func TestCompletionZshEmitsScript(t *testing.T) {
	out, err := execRoot(t, "completion", "zsh")
	if err != nil {
		t.Fatalf("completion zsh: %v", err)
	}
	if len(strings.TrimSpace(out)) == 0 {
		t.Fatal("completion zsh produced an empty script")
	}
	for _, want := range []string{"#compdef bowt", "__complete"} {
		if !strings.Contains(out, want) {
			t.Errorf("completion zsh missing %q", want)
		}
	}
}

func TestCompletionBashEmitsScript(t *testing.T) {
	out, err := execRoot(t, "completion", "bash")
	if err != nil {
		t.Fatalf("completion bash: %v", err)
	}
	if !strings.Contains(out, "__start_bowt") {
		t.Errorf("completion bash missing __start_bowt; got %d bytes", len(out))
	}
}

// Rich help renders for the root and a subcommand, including flags/examples.
func TestHelpRenders(t *testing.T) {
	out, err := execRoot(t, "--help")
	if err != nil {
		t.Fatalf("--help: %v", err)
	}
	for _, want := range []string{"bowt", "new", "ls", "path", "rm", "exec", "completion", "Available Commands"} {
		if !strings.Contains(out, want) {
			t.Errorf("root help missing %q", want)
		}
	}

	newOut, err := execRoot(t, "new", "--help")
	if err != nil {
		t.Fatalf("new --help: %v", err)
	}
	for _, want := range []string{"Create a git worktree", "--base", "Examples:", "bowt new feature/login"} {
		if !strings.Contains(newOut, want) {
			t.Errorf("new help missing %q", want)
		}
	}
}

// spawn --print-prompt assembles the prompt end-to-end (brief resolution +
// {{BRIEF}} substitution + provenance) and prints it WITHOUT launching claude
// or taking the lock. This is the seam the tests drive so no real agent runs.
func TestSpawnPrintPrompt(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if out, err := exec.Command("git", "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	if err := os.MkdirAll(filepath.Join(dir, "subagent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "subagent", "PROMPT.md"), []byte("SENTINEL-BRIEF-TEXT"), 0o644); err != nil {
		t.Fatal(err)
	}

	// --print-prompt writes the assembled prompt to os.Stdout (fmt.Print), which
	// the cobra buffer does not capture; redirect the real stdout to a pipe.
	out := captureStdout(t, func() {
		if _, err := execRoot(t, "spawn", "--impl", "--print-prompt"); err != nil {
			t.Fatalf("spawn --print-prompt: %v", err)
		}
	})

	if !strings.Contains(out, "SENTINEL-BRIEF-TEXT") {
		t.Errorf("assembled prompt missing brief body:\n%s", out)
	}
	if strings.Contains(out, "{{BRIEF}}") {
		t.Errorf("placeholder not substituted:\n%s", out)
	}
	if !strings.HasPrefix(out, "agent: claude · prompt: impl.md @ ") {
		t.Errorf("prompt should start with the provenance line; got:\n%s", out[:min(120, len(out))])
	}
}

// Selecting a provider substitutes ITS memory file into the shared prompt and
// stamps ITS name into provenance — one policy, parameterised per provider.
func TestSpawnPrintPromptCodexAgent(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if out, err := exec.Command("git", "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	if err := os.MkdirAll(filepath.Join(dir, "subagent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "subagent", "PROMPT.md"), []byte("BRIEF"), 0o644); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		if _, err := execRoot(t, "spawn", "--impl", "--agent", "codex", "--print-prompt"); err != nil {
			t.Fatalf("spawn --agent codex --print-prompt: %v", err)
		}
	})

	if !strings.HasPrefix(out, "agent: codex · prompt: impl.md @ ") {
		t.Errorf("provenance should name codex; got:\n%s", out[:min(120, len(out))])
	}
	if !strings.Contains(out, "AGENTS.md") {
		t.Errorf("codex lane should carry AGENTS.md; got:\n%s", out)
	}
	if strings.Contains(out, "CLAUDE.md") || strings.Contains(out, "{{MEMORY_FILE}}") {
		t.Errorf("codex lane must not carry CLAUDE.md or an unsubstituted placeholder; got:\n%s", out)
	}
}

// --orch selects the orchestrator wrapper: the assembled prompt is orch.md and
// carries its load-bearing role clauses. Interactive by default; --print-prompt
// is the launch-free seam, same as plan/impl.
func TestSpawnPrintPromptOrch(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if out, err := exec.Command("git", "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	if err := os.MkdirAll(filepath.Join(dir, "subagent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "subagent", "PROMPT.md"), []byte("ORCH-BRIEF-BODY"), 0o644); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		if _, err := execRoot(t, "spawn", "--orch", "--print-prompt"); err != nil {
			t.Fatalf("spawn --orch --print-prompt: %v", err)
		}
	})

	if !strings.HasPrefix(out, "agent: claude · prompt: orch.md @ ") {
		t.Errorf("orch prompt should start with an orch.md provenance line; got:\n%s", out[:min(120, len(out))])
	}
	if !strings.Contains(out, "ORCH-BRIEF-BODY") {
		t.Errorf("assembled orch prompt missing brief body:\n%s", out)
	}
	if !strings.Contains(out, "ORCHESTRATOR") || !strings.Contains(out, "NEVER WRITE PRODUCTION CODE") {
		t.Errorf("orch prompt missing role clauses:\n%s", out)
	}
}

// --impl and --orch are mutually exclusive: they pick different wrapper roles.
// (--orch --headless is NOT blocked — that is the nested orch-of-orch case.)
func TestSpawnImplAndOrchConflict(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if out, err := exec.Command("git", "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	_, err := execRoot(t, "spawn", "--impl", "--orch", "--print-prompt")
	if err == nil {
		t.Fatal("spawn --impl --orch should error, but did not")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("error should explain the conflict; got: %v", err)
	}
}

// An unknown --agent is a hard error before any work.
func TestSpawnUnknownAgent(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if out, err := exec.Command("git", "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	_, err := execRoot(t, "spawn", "--agent", "bogus", "--print-prompt")
	if err == nil {
		t.Fatal("want error for an unknown agent")
	}
	if !strings.Contains(err.Error(), "unknown agent") {
		t.Errorf("error should name the problem; got %q", err)
	}
}

// doctorAgent's checks run behind injected lookPath/HOME and dry-run, so no CLI
// is launched: codex skips one-shot (unsupported), claude skips it under
// --dry-run, and a missing binary fails the bin check.
func TestDoctorAgent(t *testing.T) {
	found := func(string) (string, error) { return "/usr/local/bin/x", nil }
	missing := func(string) (string, error) { return "", exec.ErrNotFound }
	home := t.TempDir()

	claude, err := agent.New("claude")
	if err != nil {
		t.Fatal(err)
	}
	codex, err := agent.New("codex")
	if err != nil {
		t.Fatal(err)
	}

	// claude, bin present, dry-run: every check passes without launching claude.
	rep := doctorAgent(claude, found, home, true)
	if !rep.OK {
		t.Errorf("claude dry-run doctor should pass: %+v", rep.Checks)
	}
	if got := checkDetail(rep, "oneshot"); !strings.Contains(got, "dry-run") {
		t.Errorf("oneshot should be skipped under dry-run; got %q", got)
	}

	// codex: one-shot unsupported → skipped as a pass, no launch attempted.
	rep = doctorAgent(codex, found, home, false)
	if !rep.OK {
		t.Errorf("codex doctor should pass (oneshot skipped): %+v", rep.Checks)
	}
	if got := checkDetail(rep, "oneshot"); !strings.Contains(got, "not supported") {
		t.Errorf("codex oneshot should report unsupported; got %q", got)
	}

	// Missing binary fails the bin check and the overall report.
	rep = doctorAgent(claude, missing, home, true)
	if rep.OK {
		t.Error("doctor should fail when the binary is missing")
	}
	if got := checkPass(rep, "bin-on-path"); got {
		t.Error("bin-on-path should fail when LookPath errors")
	}
}

// TestDoctorAgentSchema covers the phase-5 schema check: a valid built-in passes
// the schema check (so the overall report stays OK), and a drop-in that registers
// (valid name + session block) but LIES at the value level — an unknown
// schemaVersion the load-time sanity does not catch — fails the schema check and
// so the whole report, which is what drives newDoctorCmd's os.Exit(1).
func TestDoctorAgentSchema(t *testing.T) {
	found := func(string) (string, error) { return "/usr/local/bin/x", nil }
	home := t.TempDir()

	// Valid built-in: the schema check is present and passes.
	claude, err := agent.New("claude")
	if err != nil {
		t.Fatal(err)
	}
	rep := doctorAgent(claude, found, home, true)
	if !checkPass(rep, "schema") {
		t.Errorf("valid built-in should pass the schema check: %+v", rep.Checks)
	}
	if !strings.Contains(checkDetail(rep, "schema"), "valid") {
		t.Errorf("schema detail should report valid; got %q", checkDetail(rep, "schema"))
	}
	if got := checkDetail(rep, "origin"); !strings.Contains(got, "built-in") {
		t.Errorf("built-in provider should report a built-in origin; got %q", got)
	}

	// A lying drop-in: registers (has name + session) but its schemaVersion is
	// unknown, which only doctor's static validation catches.
	dropHome := t.TempDir()
	agentsDir := filepath.Join(dropHome, ".bowt", "agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	const badDropin = `{
  "schemaVersion": 99,
  "name": "doctorbadschema", "bin": "doctorbadschema",
  "configDir": ".doctorbadschema", "memoryFile": "DOCTORBAD.md",
  "invocations": { "session": { "prefix": ["run"], "prompt": "arg-after-dashdash" } }
}`
	if err := os.WriteFile(filepath.Join(agentsDir, "doctorbadschema.json"), []byte(badDropin), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", dropHome)
	agent.LoadDropins(func(string, ...any) {}) // register the drop-in silently
	bad, err := agent.New("doctorbadschema")
	if err != nil {
		t.Fatalf("drop-in should register (valid name+session): %v", err)
	}
	rep = doctorAgent(bad, found, home, true)
	if rep.OK {
		t.Errorf("doctor should fail on a lying drop-in: %+v", rep.Checks)
	}
	if checkPass(rep, "schema") {
		t.Errorf("schema check should fail for an unknown schemaVersion: %+v", rep.Checks)
	}
	if got := checkDetail(rep, "origin"); !strings.Contains(got, "drop-in") {
		t.Errorf("drop-in provider should report a drop-in origin; got %q", got)
	}
}

func checkDetail(rep doctorReport, name string) string {
	for _, c := range rep.Checks {
		if c.Name == name {
			return c.Detail
		}
	}
	return ""
}

func checkPass(rep doctorReport, name string) bool {
	for _, c := range rep.Checks {
		if c.Name == name {
			return c.Pass
		}
	}
	return false
}

// captureStdout redirects os.Stdout across fn and returns what was written.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(r)
		done <- buf.String()
	}()
	fn()
	_ = w.Close()
	os.Stdout = orig
	return <-done
}

// gate fails fast with the busy message when the per-worktree lock is already
// held. We pre-acquire the lock on the worktree path (keyed as cmdGate keys
// it), then run `bowt gate` and assert it errors before touching the hook. This
// exercises only the busy path, which returns an error normally — the pass/fail
// verdict path (which os.Exit's) is covered by the gate package tests.
func TestGateLockFailFast(t *testing.T) {
	// Redirect ~/.bowt/locks into a temp HOME so we don't touch the real one.
	t.Setenv("HOME", t.TempDir())

	dir := t.TempDir()
	t.Chdir(dir)
	if out, err := exec.Command("git", "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	// A gate.sh must exist so that, absent the lock, gate would proceed — proving
	// it's the lock (not a missing hook) that stops us.
	cfg := filepath.Join(dir, ".bowt")
	if err := os.MkdirAll(cfg, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg, "gate.sh"), []byte("#!/usr/bin/env bash\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	// The worktree toplevel is what cmdGate keys the lock on.
	top, err := repo.Toplevel("")
	if err != nil {
		t.Fatalf("Toplevel: %v", err)
	}
	held, err := lock.Acquire(top)
	if err != nil {
		t.Fatalf("pre-acquire: %v", err)
	}
	defer func() { _ = held.Release() }()

	if _, err := execRoot(t, "gate"); err == nil {
		t.Fatal("gate should fail fast while the worktree lock is held")
	} else if !strings.Contains(err.Error(), "busy") {
		t.Errorf("error = %q; want a busy message", err.Error())
	}
}

// writeExtension drops <cwd>/.bowt/extensions/<cmd>.sh in the fixture repo.
func writeExtension(t *testing.T, cmd, body string) {
	t.Helper()
	dir := filepath.Join(".bowt", "extensions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, cmd+".sh"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// A per-repo extension runs when its name isn't a built-in: it sees the
// BOWT_* env + its args + cwd=worktree, and its exit code is propagated.
func TestExtensionRunsEndToEnd(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // keep state.Open / locks off the real ~/.bowt
	fixtureRepo(t)
	writeExtension(t, "greet", `#!/usr/bin/env bash
# bowt-lock: none
# bowt-desc: greet the world
source "$BOWT_LIB"
echo "main=$BOWT_MAIN_REPO args=$*"
bowt_log "ran"
exit 3
`)

	root := newRootCmd()
	var code int
	var handled bool
	out := captureStdout(t, func() {
		code, handled = tryExtension(root, []string{"greet", "x", "y"})
	})

	if !handled {
		t.Fatal("greet should be handled as an extension")
	}
	if code != 3 {
		t.Errorf("exit code = %d; want 3 (propagated)", code)
	}
	if !strings.Contains(out, "args=x y") {
		t.Errorf("extension did not see its args; got:\n%s", out)
	}
	if !strings.Contains(out, "main=") {
		t.Errorf("extension did not see BOWT_MAIN_REPO; got:\n%s", out)
	}
}

// A built-in is never shadowed by a same-named extension: 'ls' resolves to the
// built-in, so tryExtension declines to handle it.
func TestExtensionNeverShadowsBuiltin(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	fixtureRepo(t)
	// This script would clobber `bowt ls` if extensions could shadow built-ins.
	writeExtension(t, "ls", "#!/usr/bin/env bash\necho SHOULD-NOT-RUN\nexit 1\n")

	root := newRootCmd()
	if _, handled := tryExtension(root, []string{"ls"}); handled {
		t.Fatal("built-in ls must win over an ls.sh extension")
	}
}

// An unknown command with no matching extension is not handled here and becomes
// cobra's normal, clean unknown-command error.
func TestUnknownCommandCleanError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	fixtureRepo(t)

	root := newRootCmd()
	if _, handled := tryExtension(root, []string{"nope"}); handled {
		t.Fatal("an unknown command with no extension must not be handled")
	}
	if _, err := execRoot(t, "nope"); err == nil {
		t.Fatal("cobra should error on an unknown command")
	} else if !strings.Contains(err.Error(), "unknown command") {
		t.Errorf("error = %q; want a clean unknown-command error", err.Error())
	}
}

// A `# bowt-lock: exclusive` extension fails fast when the per-worktree lock is
// already held — bowt owns the locking, the author writes none.
func TestExtensionExclusiveLockFailFast(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	fixtureRepo(t)
	writeExtension(t, "mutate", "#!/usr/bin/env bash\n# bowt-lock: exclusive\necho SHOULD-NOT-RUN\n")

	main, err := repo.MainRepo()
	if err != nil {
		t.Fatalf("MainRepo: %v", err)
	}
	ext, found, err := extension.Find(config.Dir(main), "mutate")
	if err != nil || !found {
		t.Fatalf("resolve extension: found %v err %v", found, err)
	}

	// Pre-hold the worktree lock (keyed on the toplevel, as cmdExtension keys it).
	top, err := repo.Toplevel("")
	if err != nil {
		t.Fatalf("Toplevel: %v", err)
	}
	held, err := lock.Acquire(top)
	if err != nil {
		t.Fatalf("pre-acquire: %v", err)
	}
	defer func() { _ = held.Release() }()

	if _, err := cmdExtension(main, ext, nil); err == nil {
		t.Fatal("exclusive extension should fail fast while the lock is held")
	} else if !strings.Contains(err.Error(), "busy") {
		t.Errorf("error = %q; want a busy message", err.Error())
	}
}

// `bowt lib` prints the sourceable helper defining bowt_log / bowt_err.
func TestLibCommand(t *testing.T) {
	out := captureStdout(t, func() {
		if _, err := execRoot(t, "lib"); err != nil {
			t.Fatalf("lib: %v", err)
		}
	})
	for _, want := range []string{"bowt_log()", "bowt_err()", "BOWT_LIB"} {
		if !strings.Contains(out, want) {
			t.Errorf("bowt lib missing %q; got:\n%s", want, out)
		}
	}
}

// `bowt extensions` lists the repo's extensions with their descriptions.
func TestExtensionsListing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	fixtureRepo(t)
	writeExtension(t, "greet", "# bowt-lock: exclusive\n# bowt-desc: greet the world\necho hi\n")

	out := captureStdout(t, func() {
		if _, err := execRoot(t, "extensions"); err != nil {
			t.Fatalf("extensions: %v", err)
		}
	})
	for _, want := range []string{"greet", "greet the world", "exclusive"} {
		if !strings.Contains(out, want) {
			t.Errorf("extensions listing missing %q; got:\n%s", want, out)
		}
	}
}

// `bowt version` prints just the version string (the old behavior).
func TestVersionSubcommand(t *testing.T) {
	// version subcommand writes via fmt.Println to os.Stdout, so we can't
	// capture it through the cobra buffer; assert it at least runs cleanly.
	if _, err := execRoot(t, "version"); err != nil {
		t.Fatalf("version: %v", err)
	}
}
