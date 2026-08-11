package agent

import (
	"context"
	"strings"
	"testing"
)

// fakeRunner records invocations and returns canned responses, so provider
// logic is tested without launching a real CLI.
type fakeRunner struct {
	sess    []fakeCall
	one     []fakeCall
	oneOut  string
	oneErr  error
	sessErr error
}

type fakeCall struct {
	bin   string
	args  []string
	stdin string
}

func (f *fakeRunner) session(_ context.Context, bin string, args []string, _ Opts) error {
	f.sess = append(f.sess, fakeCall{bin: bin, args: append([]string(nil), args...)})
	return f.sessErr
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

// Selection precedence: flag → BOWT_AGENT → GWT_AGENT → default claude; an
// unknown name from any source is a hard error.
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
		{name: "GWT_AGENT back-compat", env: map[string]string{"GWT_AGENT": "codex"}, want: "codex"},
		{name: "BOWT_AGENT beats GWT_AGENT", env: map[string]string{"BOWT_AGENT": "claude", "GWT_AGENT": "codex"}, want: "claude"},
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
	if c.MemoryFile != "CLAUDE.md" || c.ConfigDir != ".claude" || !c.SupportsOneshot || !c.SupportsHooks {
		t.Errorf("claude caps unexpected: %+v", c)
	}
	codex, _, _ := newFake(t, "codex")
	x := codex.Caps()
	if x.MemoryFile != "AGENTS.md" || x.SupportsOneshot || x.SupportsHooks {
		t.Errorf("codex caps unexpected: %+v", x)
	}
}
