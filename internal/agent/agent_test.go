package agent

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// fakeRunner records invocations and returns canned responses, so provider
// logic is tested without launching a real CLI.
type fakeRunner struct {
	sess    []fakeCall
	head    []fakeCall
	one     []fakeCall
	oneOut  string
	oneErr  error
	sessErr error
	headErr error
}

type fakeCall struct {
	bin   string
	args  []string
	stdin string
	opts  Opts // captured so tests assert opts.Stdout/Stderr are forwarded
}

func (f *fakeRunner) session(_ context.Context, bin string, args []string, opts Opts) error {
	f.sess = append(f.sess, fakeCall{bin: bin, args: append([]string(nil), args...), opts: opts})
	return f.sessErr
}

func (f *fakeRunner) headless(_ context.Context, bin string, args []string, opts Opts) error {
	f.head = append(f.head, fakeCall{bin: bin, args: append([]string(nil), args...), opts: opts})
	return f.headErr
}

func (f *fakeRunner) oneshot(_ context.Context, bin string, args []string, stdin string) (string, error) {
	f.one = append(f.one, fakeCall{bin: bin, args: append([]string(nil), args...), stdin: stdin})
	return f.oneOut, f.oneErr
}

// newFake builds the named provider with a fake runner and a warning recorder.
func newFake(t *testing.T, name string) (Agent, *fakeRunner, *[]string) {
	t.Helper()
	fr := &fakeRunner{}
	var warns []string
	a, err := newWith(name, fr, func(format string, args ...any) {
		warns = append(warns, format)
	})
	if err != nil {
		t.Fatalf("newWith(%q): %v", name, err)
	}
	return a, fr, &warns
}

// claude's Session argv must match bowt's original hardcoded invocation exactly.
func TestClaudeSessionArgsByteIdentical(t *testing.T) {
	tests := []struct {
		name  string
		opts  Opts
		want  []string
		warns int
	}{
		{
			name: "model and effort both set",
			opts: Opts{Model: "claude-opus-4-8", Effort: "high"},
			want: []string{"--dangerously-skip-permissions", "--model", "claude-opus-4-8", "--effort", "high", "--", "THE-PROMPT"},
		},
		{
			name: "neither set (session defaults)",
			opts: Opts{},
			want: []string{"--dangerously-skip-permissions", "--", "THE-PROMPT"},
		},
		{
			name: "model only",
			opts: Opts{Model: "claude-sonnet-5"},
			want: []string{"--dangerously-skip-permissions", "--model", "claude-sonnet-5", "--", "THE-PROMPT"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, fr, warns := newFake(t, "claude")
			if err := a.Session(context.Background(), "THE-PROMPT", tt.opts); err != nil {
				t.Fatalf("Session: %v", err)
			}
			if len(fr.sess) != 1 {
				t.Fatalf("got %d session calls; want 1", len(fr.sess))
			}
			if fr.sess[0].bin != "claude" {
				t.Errorf("bin = %q; want claude", fr.sess[0].bin)
			}
			if strings.Join(fr.sess[0].args, "\x00") != strings.Join(tt.want, "\x00") {
				t.Errorf("args = %v; want %v", fr.sess[0].args, tt.want)
			}
			if len(*warns) != tt.warns {
				t.Errorf("warnings = %v; want %d", *warns, tt.warns)
			}
		})
	}
}

// claude's Headless argv is the contract the supervisor relies on: -p +
// mandatory --dangerously-skip-permissions, then the model/effort knobs, then
// the prompt after `--`. It also proves opts.Stdout/Stderr are forwarded to the
// runner unchanged (the supervisor points them at the lane log) and that stdin
// is NEVER wired to a terminal.
func TestClaudeHeadlessArgs(t *testing.T) {
	var out, errw bytes.Buffer
	tests := []struct {
		name string
		opts Opts
		want []string
	}{
		{
			name: "model and effort set",
			opts: Opts{Model: "claude-opus-4-8", Effort: "high", Stdout: &out, Stderr: &errw},
			want: []string{"-p", "--dangerously-skip-permissions", "--model", "claude-opus-4-8", "--effort", "high", "--", "P"},
		},
		{
			name: "neither set",
			opts: Opts{Stdout: &out, Stderr: &errw},
			want: []string{"-p", "--dangerously-skip-permissions", "--", "P"},
		},
		{
			name: "model only",
			opts: Opts{Model: "claude-sonnet-5", Stdout: &out, Stderr: &errw},
			want: []string{"-p", "--dangerously-skip-permissions", "--model", "claude-sonnet-5", "--", "P"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, fr, _ := newFake(t, "claude")
			if err := a.Headless(context.Background(), "P", tt.opts); err != nil {
				t.Fatalf("Headless: %v", err)
			}
			if len(fr.head) != 1 || len(fr.sess) != 0 {
				t.Fatalf("headless routed wrong: head=%d sess=%d", len(fr.head), len(fr.sess))
			}
			got := fr.head[0]
			if got.bin != "claude" {
				t.Errorf("bin = %q; want claude", got.bin)
			}
			if strings.Join(got.args, "\x00") != strings.Join(tt.want, "\x00") {
				t.Errorf("args = %v; want %v", got.args, tt.want)
			}
			// opts.Stdout/Stderr must be forwarded to the runner (log capture).
			if got.opts.Stdout != &out || got.opts.Stderr != &errw {
				t.Errorf("opts streams not forwarded to runner")
			}
			// stdin is never wired to the terminal for a headless run.
			if got.opts.Stdin != nil {
				t.Errorf("headless opts.Stdin = %v; want nil (no TTY)", got.opts.Stdin)
			}
		})
	}
}

// codex refuses headless loudly whether via RequireHeadless (up-front) or the
// method guard (a caller that skipped the check) — mirroring the Oneshot refusal.
func TestHeadlessRefusesCodex(t *testing.T) {
	claude, _, _ := newFake(t, "claude")
	if err := RequireHeadless(claude); err != nil {
		t.Errorf("claude should satisfy RequireHeadless; got %v", err)
	}

	codex, fr, _ := newFake(t, "codex")
	err := RequireHeadless(codex)
	if err == nil {
		t.Fatal("codex should fail RequireHeadless")
	}
	if !strings.Contains(err.Error(), "codex") || !strings.Contains(err.Error(), "headless") {
		t.Errorf("error should name the agent and the missing capability; got %q", err)
	}

	// The method itself also guards, and never reaches the runner.
	if err := codex.Headless(context.Background(), "P", Opts{}); err == nil {
		t.Fatal("codex.Headless should be a hard error")
	}
	if len(fr.head) != 0 {
		t.Errorf("codex.Headless must not reach the runner; got %d calls", len(fr.head))
	}
}

// codex can express --model but not --effort: effort is dropped with a warning,
// never errored and never silently passed as an arg.
func TestCodexDropsEffortWithWarning(t *testing.T) {
	a, fr, warns := newFake(t, "codex")
	if err := a.Session(context.Background(), "P", Opts{Model: "m", Effort: "high"}); err != nil {
		t.Fatalf("Session: %v", err)
	}
	got := strings.Join(fr.sess[0].args, " ")
	if strings.Contains(got, "effort") || strings.Contains(got, "high") {
		t.Errorf("codex args leaked the dropped effort knob: %q", got)
	}
	if !strings.Contains(got, "--model m") {
		t.Errorf("codex should still pass --model; got %q", got)
	}
	if len(*warns) != 1 || !strings.Contains((*warns)[0], "effort") {
		t.Errorf("expected exactly one effort-drop warning; got %v", *warns)
	}
}

// claude with effort set warns for nothing (it can express the knob).
func TestClaudeNoWarnOnEffort(t *testing.T) {
	a, _, warns := newFake(t, "claude")
	if err := a.Session(context.Background(), "P", Opts{Effort: "high"}); err != nil {
		t.Fatalf("Session: %v", err)
	}
	if len(*warns) != 0 {
		t.Errorf("claude should not warn on --effort; got %v", *warns)
	}
}

// Oneshot: claude round-trips stdin→stdout; codex is a hard error naming the gap.
func TestOneshot(t *testing.T) {
	a, fr, _ := newFake(t, "claude")
	fr.oneOut = "ok\n"
	out, err := a.Oneshot(context.Background(), "say ok")
	if err != nil {
		t.Fatalf("claude Oneshot: %v", err)
	}
	if out != "ok\n" {
		t.Errorf("out = %q; want ok", out)
	}
	if len(fr.one) != 1 || fr.one[0].stdin != "say ok" || strings.Join(fr.one[0].args, " ") != "-p" {
		t.Errorf("unexpected oneshot call: %+v", fr.one)
	}

	c, _, _ := newFake(t, "codex")
	if _, err := c.Oneshot(context.Background(), "x"); err == nil {
		t.Fatal("codex Oneshot should be a hard error")
	} else if !strings.Contains(err.Error(), "one-shot") {
		t.Errorf("codex Oneshot error should name the gap; got %q", err)
	}
}

// RequireOneshot passes for a one-shot provider and names the gap for one without.
func TestRequireOneshot(t *testing.T) {
	claude, _, _ := newFake(t, "claude")
	if err := RequireOneshot(claude); err != nil {
		t.Errorf("claude should satisfy RequireOneshot; got %v", err)
	}
	codex, _, _ := newFake(t, "codex")
	err := RequireOneshot(codex)
	if err == nil {
		t.Fatal("codex should fail RequireOneshot")
	}
	if !strings.Contains(err.Error(), "codex") || !strings.Contains(err.Error(), "one-shot") {
		t.Errorf("error should name the agent and the missing capability; got %q", err)
	}
}

// Selection precedence: flag → BOWT_AGENT → default claude; an unknown name
// from any source is a hard error.
func TestSelect(t *testing.T) {
	env := func(m map[string]string) func(string) string {
		return func(k string) string { return m[k] }
	}
	tests := []struct {
		name    string
		flag    string
		env     map[string]string
		want    string
		wantErr bool
	}{
		{name: "flag wins over env", flag: "codex", env: map[string]string{"BOWT_AGENT": "claude"}, want: "codex"},
		{name: "BOWT_AGENT when no flag", env: map[string]string{"BOWT_AGENT": "codex"}, want: "codex"},
		{name: "default claude", want: "claude"},
		{name: "unknown flag errors", flag: "bogus", wantErr: true},
		{name: "unknown env errors", env: map[string]string{"BOWT_AGENT": "bogus"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, err := Select(tt.flag, env(tt.env))
			if tt.wantErr {
				if err == nil {
					t.Fatal("want error")
				}
				if !strings.Contains(err.Error(), "unknown agent") {
					t.Errorf("error should name the problem; got %q", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Select: %v", err)
			}
			if a.Caps().Name != tt.want {
				t.Errorf("selected %q; want %q", a.Caps().Name, tt.want)
			}
		})
	}
}

// The two providers' capability descriptors are what the checks and prompts key
// off — pin the load-bearing fields.
func TestCapabilities(t *testing.T) {
	claude, _, _ := newFake(t, "claude")
	c := claude.Caps()
	if c.MemoryFile != "CLAUDE.md" || c.ConfigDir != ".claude" || !c.SupportsOneshot || !c.SupportsHooks || !c.SupportsHeadless {
		t.Errorf("claude caps unexpected: %+v", c)
	}
	codex, _, _ := newFake(t, "codex")
	x := codex.Caps()
	if x.MemoryFile != "AGENTS.md" || x.SupportsOneshot || x.SupportsHooks || x.SupportsHeadless {
		t.Errorf("codex caps unexpected: %+v", x)
	}
}
