package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeDropin writes name (a *.json filename) into dir with the given body.
func writeDropin(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// recWarnf returns a formatting warn sink plus the slice it appends the finished
// strings to, so a test can assert a file was skipped WITH a warning.
func recWarnf() (func(string, ...any), *[]string) {
	var warns []string
	return func(format string, args ...any) {
		warns = append(warns, fmt.Sprintf(format, args...))
	}, &warns
}

// A well-formed drop-in: session (required) + oneshot, a distinct name.
const foobarDropin = `{
  "schemaVersion": 1,
  "name": "foobar",
  "bin": "foobar",
  "configDir": ".foobar",
  "memoryFile": "FOOBAR.md",
  "hooks": false,
  "model": {"render": ["--model", "{value}"]},
  "invocations": {
    "session": {"prefix": ["run"], "prompt": "arg-after-dashdash"},
    "oneshot": {"prefix": ["-p"]}
  }
}`

// A valid drop-in registers, is reachable via newWith, and carries origin=dropin
// (loader-set to the file path, NOT read from the JSON).
func TestDropinValidRegisters(t *testing.T) {
	restore := swapRegistry(t)
	defer restore()

	dir := t.TempDir()
	writeDropin(t, dir, "foobar.json", foobarDropin)
	warnf, warns := recWarnf()
	loadDropins(dir, warnf)

	if len(*warns) != 0 {
		t.Fatalf("a valid drop-in should warn nothing; got %v", *warns)
	}
	a, err := newWith("foobar", &fakeRunner{}, func(string, ...any) {})
	if err != nil {
		t.Fatalf("newWith(foobar): %v", err)
	}
	da, ok := a.(descriptorAgent)
	if !ok {
		t.Fatalf("newWith(foobar) = %T; want descriptorAgent", a)
	}
	if da.origin.isBuiltin() {
		t.Error("drop-in origin.isBuiltin() = true; want false (fail-closed)")
	}
	if want := filepath.Join(dir, "foobar.json"); da.origin.path != want {
		t.Errorf("drop-in origin.path = %q; want the source file %q", da.origin.path, want)
	}
	if c := a.Caps(); c.Name != "foobar" || !c.SupportsOneshot {
		t.Errorf("caps = %+v; want name=foobar, oneshot=true", c)
	}
}

// THE SECURITY ASSERTION (the whole point of phase 4): a drop-in that DECLARES a
// headless invocation AND hooks:true still reports SupportsHeadless=false and is
// refused by RequireHeadless — because doctor cannot verify a third-party CLI's
// guardrail (RFC §11). Its session/oneshot still work, and Headless itself
// refuses without reaching the runner.
func TestDropinHeadlessRefusedDespiteHooksClaim(t *testing.T) {
	restore := swapRegistry(t)
	defer restore()

	const lying = `{
      "schemaVersion": 1,
      "name": "lyingheadless",
      "bin": "lh",
      "hooks": true,
      "model": {"render": ["--model", "{value}"]},
      "invocations": {
        "session":  {"prefix": ["run"], "prompt": "arg-after-dashdash"},
        "oneshot":  {"prefix": ["-p"]},
        "headless": {"prefix": ["-p", "--dangerously-skip-permissions"], "prompt": "arg-after-dashdash"}
      }
    }`
	dir := t.TempDir()
	writeDropin(t, dir, "lyingheadless.json", lying)
	warnf, _ := recWarnf()
	loadDropins(dir, warnf)

	fr := &fakeRunner{}
	a, err := newWith("lyingheadless", fr, func(string, ...any) {})
	if err != nil {
		t.Fatalf("newWith: %v", err)
	}

	// The claim is hooks:true + a headless block, yet the origin term denies it.
	if a.Caps().SupportsHeadless {
		t.Fatal("drop-in SupportsHeadless = true; want false (unverifiable hooks:true must not unlock headless)")
	}
	if a.Caps().SupportsHooks != true {
		t.Error("SupportsHooks should still reflect the declared flag (true); only headless is gated by origin")
	}
	if err := RequireHeadless(a); err == nil {
		t.Fatal("RequireHeadless must refuse a drop-in")
	}
	if err := a.Headless(context.Background(), "P", Opts{}); err == nil {
		t.Fatal("Headless must refuse a drop-in")
	}
	if len(fr.head) != 0 {
		t.Errorf("refused Headless must not reach the runner; got %d calls", len(fr.head))
	}

	// But interactive/one-shot paths a drop-in DOES get still work.
	if err := a.Session(context.Background(), "P", Opts{}); err != nil {
		t.Fatalf("drop-in Session: %v", err)
	}
	if len(fr.sess) != 1 {
		t.Errorf("drop-in Session should reach the runner; got %d calls", len(fr.sess))
	}
	fr.oneOut = "ok\n"
	if out, err := a.Oneshot(context.Background(), "x"); err != nil || out != "ok\n" {
		t.Errorf("drop-in Oneshot = (%q, %v); want (\"ok\\n\", nil)", out, err)
	}
}

// A drop-in colliding with a built-in name is skipped with a warning; the
// built-in still wins (precedence, not policy).
func TestDropinCollisionWithBuiltinSkipped(t *testing.T) {
	restore := swapRegistry(t)
	defer restore()

	// Simulate the built-in having registered first (init order).
	register("claude", func(r runner, warnf func(string, ...any)) (Agent, error) {
		return stubAgent{run: r, warnf: warnf}, nil
	})

	dir := t.TempDir()
	writeDropin(t, dir, "claude.json", strings.Replace(foobarDropin, `"foobar"`, `"claude"`, 1))
	warnf, warns := recWarnf()
	loadDropins(dir, warnf)

	if len(*warns) != 1 || !strings.Contains((*warns)[0], "claude") || !strings.Contains((*warns)[0], "already registered") {
		t.Fatalf("collision should skip WITH a warning naming the provider; got %v", *warns)
	}
	// The built-in (stub) still answers — the drop-in did not shadow it.
	a, err := newWith("claude", &fakeRunner{}, func(string, ...any) {})
	if err != nil {
		t.Fatalf("newWith(claude): %v", err)
	}
	if _, ok := a.(stubAgent); !ok {
		t.Errorf("built-in should win the name; got %T", a)
	}
}

// The FIRST drop-in of a name wins; a later duplicate is skipped+warned. Proves
// precedence is general (not built-in-only) and deterministic (sorted files).
func TestDropinDuplicateAmongDropinsFirstWins(t *testing.T) {
	restore := swapRegistry(t)
	defer restore()

	dir := t.TempDir()
	// aaa.json sorts before zzz.json; both declare name "dupdrop".
	writeDropin(t, dir, "aaa.json", strings.Replace(foobarDropin, `"foobar"`, `"dupdrop"`, 1))
	dupBinZ := strings.NewReplacer(`"foobar"`, `"dupdrop"`, `"bin": "foobar"`, `"bin": "zbin"`).Replace(foobarDropin)
	writeDropin(t, dir, "zzz.json", dupBinZ)
	warnf, warns := recWarnf()
	loadDropins(dir, warnf)

	if len(*warns) != 1 || !strings.Contains((*warns)[0], "zzz.json") {
		t.Fatalf("the later (zzz) duplicate should be the one skipped+warned; got %v", *warns)
	}
	a, err := newWith("dupdrop", &fakeRunner{}, func(string, ...any) {})
	if err != nil {
		t.Fatalf("newWith(dupdrop): %v", err)
	}
	if bin := a.Caps().Bin; bin != "foobar" {
		t.Errorf("first drop-in (aaa) should win; got bin=%q want foobar", bin)
	}
}

// Malformed / nameless / sessionless files are each skipped with a warning and
// never register — one bad file must not brick bowt, and a good sibling still loads.
func TestDropinRobustnessSkipsBadFiles(t *testing.T) {
	tests := []struct {
		name string
		file string
		body string
		warn string
	}{
		{name: "unparseable", file: "bad.json", body: `{not json`, warn: "parse error"},
		{name: "no name", file: "noname.json", body: `{"schemaVersion":1,"bin":"x","invocations":{"session":{"prefix":["s"],"prompt":"arg"}}}`, warn: `missing "name"`},
		{name: "no session", file: "nosession.json", body: `{"schemaVersion":1,"name":"nosess","bin":"x","invocations":{"oneshot":{"prefix":["-p"]}}}`, warn: "no \"session\""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			restore := swapRegistry(t)
			defer restore()

			dir := t.TempDir()
			writeDropin(t, dir, tt.file, tt.body)
			writeDropin(t, dir, "foobar.json", foobarDropin) // a good sibling
			warnf, warns := recWarnf()
			loadDropins(dir, warnf)

			if len(*warns) != 1 || !strings.Contains((*warns)[0], tt.warn) {
				t.Fatalf("bad file should skip WITH a warning containing %q; got %v", tt.warn, *warns)
			}
			// The bad file registered nothing…
			names := knownAgents()
			if len(names) != 1 || names[0] != "foobar" {
				t.Errorf("only the good sibling should register; got %v", names)
			}
		})
	}
}

// A missing drop-in dir is a silent no-op (the normal case: most users have none).
func TestDropinMissingDirNoOp(t *testing.T) {
	restore := swapRegistry(t)
	defer restore()

	warnf, warns := recWarnf()
	loadDropins(filepath.Join(t.TempDir(), "does-not-exist"), warnf)
	if len(*warns) != 0 {
		t.Errorf("missing dir should be a silent no-op; got %v", *warns)
	}
	if len(knownAgents()) != 0 {
		t.Errorf("nothing should register from a missing dir; got %v", knownAgents())
	}
}

// The exported LoadDropins wrapper resolves ~/.bowt/agents from the home dir and
// registers a drop-in placed there — proving the thin wrapper's path resolution.
func TestLoadDropinsResolvesHome(t *testing.T) {
	restore := swapRegistry(t)
	defer restore()

	home := t.TempDir()
	t.Setenv("HOME", home)
	agentsDir := filepath.Join(home, ".bowt", "agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeDropin(t, agentsDir, "foobar.json", foobarDropin)

	warnf, warns := recWarnf()
	LoadDropins(warnf)
	if len(*warns) != 0 {
		t.Fatalf("clean load should warn nothing; got %v", *warns)
	}
	if _, err := newWith("foobar", &fakeRunner{}, func(string, ...any) {}); err != nil {
		t.Errorf("LoadDropins should register the home drop-in: %v", err)
	}
}
