package agent

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
)

// claudeDescriptor is the hand-built data form of claude.go, used to prove a
// descriptorAgent reproduces claudeAgent byte-for-byte. It mirrors claude.json
// (RFC §4.2).
func claudeDescriptor() Descriptor {
	return Descriptor{
		SchemaVersion: 1,
		Name:          "claude",
		Bin:           "claude",
		ConfigDir:     ".claude",
		MemoryFile:    "CLAUDE.md",
		Hooks:         true,
		Model: ModelSpec{
			Aliases: map[string]string{
				"opus":   "claude-opus-4-8",
				"sonnet": "claude-sonnet-5",
				"haiku":  "claude-haiku-4-5-20251001",
			},
			Strong: "claude-opus-4-8",
			Render: []string{"--model", "{value}"},
		},
		Effort: map[Effort][]string{
			EffortLow:    {"--effort", "low"},
			EffortMedium: {"--effort", "medium"},
			EffortHigh:   {"--effort", "high"},
		},
		Invocations: map[Mode]InvSpec{
			ModeSession:  {Prefix: []string{"--dangerously-skip-permissions"}, Prompt: DeliveryArgAfterDashDash},
			ModeOneshot:  {Prefix: []string{"-p"}},
			ModeHeadless: {Prefix: []string{"-p", "--dangerously-skip-permissions"}, Prompt: DeliveryArgAfterDashDash},
		},
	}
}

// codexDescriptor is the hand-built data form of codex.go (RFC §4.3): no effort
// map (drop+warn), no oneshot block (refuse), no headless block + hooks:false
// (refuse).
func codexDescriptor() Descriptor {
	return Descriptor{
		SchemaVersion: 1,
		Name:          "codex",
		Bin:           "codex",
		ConfigDir:     ".codex",
		MemoryFile:    "AGENTS.md",
		Hooks:         false,
		Model:         ModelSpec{Render: []string{"--model", "{value}"}},
		Invocations: map[Mode]InvSpec{
			ModeSession: {Prefix: []string{"exec"}, Prompt: DeliveryArgAfterDashDash},
		},
	}
}

// newFakeDescriptor builds a descriptorAgent over a fake runner and a warning
// recorder that keeps the FORMATTED warning text (so tests assert the exact
// user-visible string, proving byte-identity with codex.go's warning).
func newFakeDescriptor(desc Descriptor) (descriptorAgent, *fakeRunner, *[]string) {
	fr := &fakeRunner{}
	var warns []string
	a := descriptorAgent{
		desc: desc,
		run:  fr,
		warnf: func(format string, args ...any) {
			warns = append(warns, fmt.Sprintf(format, args...))
		},
	}
	return a, fr, &warns
}

// Mirror of TestClaudeSessionArgsByteIdentical: a claude-shaped descriptor's
// Session argv must equal claudeAgent's for every knob combination.
func TestDescriptorClaudeSessionArgsByteIdentical(t *testing.T) {
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
			a, fr, warns := newFakeDescriptor(claudeDescriptor())
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

// Mirror of TestClaudeHeadlessArgs: a claude-shaped descriptor's Headless argv,
// opts-stream forwarding, and never-a-TTY-stdin all match claudeAgent.
func TestDescriptorClaudeHeadlessArgs(t *testing.T) {
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
			a, fr, _ := newFakeDescriptor(claudeDescriptor())
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
			if got.opts.Stdout != &out || got.opts.Stderr != &errw {
				t.Errorf("opts streams not forwarded to runner")
			}
			if got.opts.Stdin != nil {
				t.Errorf("headless opts.Stdin = %v; want nil (no TTY)", got.opts.Stdin)
			}
		})
	}
}

// Mirror of TestCodexDropsEffortWithWarning: the codex descriptor keeps --model
// but drops --effort with exactly one warning whose FORMATTED text is identical
// to codex.go's.
func TestDescriptorCodexDropsEffortWithWarning(t *testing.T) {
	a, fr, warns := newFakeDescriptor(codexDescriptor())
	if err := a.Session(context.Background(), "P", Opts{Model: "m", Effort: "high"}); err != nil {
		t.Fatalf("Session: %v", err)
	}
	wantArgs := []string{"exec", "--model", "m", "--", "P"}
	if strings.Join(fr.sess[0].args, "\x00") != strings.Join(wantArgs, "\x00") {
		t.Errorf("args = %v; want %v", fr.sess[0].args, wantArgs)
	}
	got := strings.Join(fr.sess[0].args, " ")
	if strings.Contains(got, "effort") || strings.Contains(got, "high") {
		t.Errorf("codex args leaked the dropped effort knob: %q", got)
	}
	const wantWarn = "agent codex: dropping --effort=high (this provider has no reasoning-effort control)"
	if len(*warns) != 1 {
		t.Fatalf("expected exactly one effort-drop warning; got %v", *warns)
	}
	if (*warns)[0] != wantWarn {
		t.Errorf("warning = %q; want %q", (*warns)[0], wantWarn)
	}
}

// Mirror of TestClaudeNoWarnOnEffort: the claude descriptor renders --effort, so
// it warns for nothing.
func TestDescriptorClaudeNoWarnOnEffort(t *testing.T) {
	a, _, warns := newFakeDescriptor(claudeDescriptor())
	if err := a.Session(context.Background(), "P", Opts{Effort: "high"}); err != nil {
		t.Fatalf("Session: %v", err)
	}
	if len(*warns) != 0 {
		t.Errorf("claude should not warn on --effort; got %v", *warns)
	}
}

// Mirror of TestOneshot: the claude descriptor round-trips stdin→stdout with the
// "-p" prefix; the codex descriptor refuses with codex.go's verbatim message.
func TestDescriptorOneshot(t *testing.T) {
	a, fr, _ := newFakeDescriptor(claudeDescriptor())
	fr.oneOut = "ok\n"
	out, err := a.Oneshot(context.Background(), "say ok")
	if err != nil {
		t.Fatalf("claude Oneshot: %v", err)
	}
	if out != "ok\n" {
		t.Errorf("out = %q; want ok", out)
	}
	if len(fr.one) != 1 || fr.one[0].bin != "claude" || fr.one[0].stdin != "say ok" || strings.Join(fr.one[0].args, " ") != "-p" {
		t.Errorf("unexpected oneshot call: %+v", fr.one)
	}

	c, cfr, _ := newFakeDescriptor(codexDescriptor())
	_, err = c.Oneshot(context.Background(), "x")
	if err == nil {
		t.Fatal("codex Oneshot should be a hard error")
	}
	const wantMsg = `agent "codex" does not support one-shot mode (stdin→stdout, no side effects)`
	if err.Error() != wantMsg {
		t.Errorf("codex Oneshot error = %q; want %q", err, wantMsg)
	}
	if len(cfr.one) != 0 {
		t.Errorf("codex Oneshot must not reach the runner; got %d calls", len(cfr.one))
	}
}

// Mirror of TestHeadlessRefusesCodex: the claude descriptor satisfies
// RequireHeadless; the codex descriptor fails it and the method itself, with
// codex.go's verbatim message, never reaching the runner.
func TestDescriptorHeadlessRefusesCodex(t *testing.T) {
	claude, _, _ := newFakeDescriptor(claudeDescriptor())
	if err := RequireHeadless(claude); err != nil {
		t.Errorf("claude descriptor should satisfy RequireHeadless; got %v", err)
	}

	codex, fr, _ := newFakeDescriptor(codexDescriptor())
	err := RequireHeadless(codex)
	if err == nil {
		t.Fatal("codex should fail RequireHeadless")
	}
	if !strings.Contains(err.Error(), "codex") || !strings.Contains(err.Error(), "headless") {
		t.Errorf("error should name the agent and the missing capability; got %q", err)
	}

	methodErr := codex.Headless(context.Background(), "P", Opts{})
	if methodErr == nil {
		t.Fatal("codex.Headless should be a hard error")
	}
	const wantMsg = `agent "codex" does not support headless mode (no Edit/Write hook guardrail for an unattended run)`
	if methodErr.Error() != wantMsg {
		t.Errorf("codex.Headless error = %q; want %q", methodErr, wantMsg)
	}
	if len(fr.head) != 0 {
		t.Errorf("codex.Headless must not reach the runner; got %d calls", len(fr.head))
	}
}

// Mirror of TestRequireOneshot: the claude descriptor satisfies RequireOneshot;
// the codex descriptor fails it, naming the agent and the gap.
func TestDescriptorRequireOneshot(t *testing.T) {
	claude, _, _ := newFakeDescriptor(claudeDescriptor())
	if err := RequireOneshot(claude); err != nil {
		t.Errorf("claude descriptor should satisfy RequireOneshot; got %v", err)
	}
	codex, _, _ := newFakeDescriptor(codexDescriptor())
	err := RequireOneshot(codex)
	if err == nil {
		t.Fatal("codex should fail RequireOneshot")
	}
	if !strings.Contains(err.Error(), "codex") || !strings.Contains(err.Error(), "one-shot") {
		t.Errorf("error should name the agent and the missing capability; got %q", err)
	}
}

// Mirror of TestCapabilities: descriptor-derived Caps must equal claudeAgent's
// and codexAgent's exactly.
func TestDescriptorCapabilities(t *testing.T) {
	claude, _, _ := newFakeDescriptor(claudeDescriptor())
	c := claude.Caps()
	want := Capabilities{
		Name: "claude", Bin: "claude", ConfigDir: ".claude", MemoryFile: "CLAUDE.md",
		SupportsOneshot: true, SupportsHooks: true, SupportsHeadless: true,
	}
	if c != want {
		t.Errorf("claude descriptor caps = %+v; want %+v", c, want)
	}

	codex, _, _ := newFakeDescriptor(codexDescriptor())
	x := codex.Caps()
	wantCodex := Capabilities{
		Name: "codex", Bin: "codex", ConfigDir: ".codex", MemoryFile: "AGENTS.md",
		SupportsOneshot: false, SupportsHooks: false, SupportsHeadless: false,
	}
	if x != wantCodex {
		t.Errorf("codex descriptor caps = %+v; want %+v", x, wantCodex)
	}
}

// render: {value} substitution, multiple occurrences, and a list with no
// placeholder returned unchanged.
func TestRender(t *testing.T) {
	tests := []struct {
		name string
		list []string
		v    string
		want []string
	}{
		{name: "single placeholder", list: []string{"--model", "{value}"}, v: "opus", want: []string{"--model", "opus"}},
		{name: "multiple occurrences in one element", list: []string{"{value}-{value}"}, v: "x", want: []string{"x-x"}},
		{name: "no placeholder untouched", list: []string{"--flag", "literal"}, v: "ignored", want: []string{"--flag", "literal"}},
		{name: "empty list", list: nil, v: "x", want: []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := render(tt.list, tt.v)
			if strings.Join(got, "\x00") != strings.Join(tt.want, "\x00") {
				t.Errorf("render(%v, %q) = %v; want %v", tt.list, tt.v, got, tt.want)
			}
		})
	}

	// render must not alias or mutate the source list.
	src := []string{"--model", "{value}"}
	_ = render(src, "opus")
	if src[1] != "{value}" {
		t.Errorf("render mutated its input: %v", src)
	}
}

// deliver: all three forms plus unknown/empty → error.
func TestDeliver(t *testing.T) {
	tests := []struct {
		name    string
		d       Delivery
		want    []string
		wantErr bool
	}{
		{name: "arg-after-dashdash", d: DeliveryArgAfterDashDash, want: []string{"--", "P"}},
		{name: "arg", d: DeliveryArg, want: []string{"P"}},
		{name: "flag", d: Delivery("flag:--prompt"), want: []string{"--prompt", "P"}},
		{name: "unknown", d: Delivery("mystery"), wantErr: true},
		{name: "empty", d: Delivery(""), wantErr: true},
		{name: "flag with empty name", d: Delivery("flag:"), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := deliver(tt.d, "P")
			if tt.wantErr {
				if err == nil {
					t.Fatalf("deliver(%q) = %v; want error", tt.d, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("deliver(%q): %v", tt.d, err)
			}
			if strings.Join(got, "\x00") != strings.Join(tt.want, "\x00") {
				t.Errorf("deliver(%q) = %v; want %v", tt.d, got, tt.want)
			}
		})
	}
}

// A session/headless invocation whose Prompt delivery is unknown is a hard error
// from the concatenator, not a silent no-op.
func TestSessionUnknownDeliveryErrors(t *testing.T) {
	desc := claudeDescriptor()
	desc.Invocations[ModeSession] = InvSpec{Prefix: []string{"x"}, Prompt: Delivery("bogus")}
	a, fr, _ := newFakeDescriptor(desc)
	if err := a.Session(context.Background(), "P", Opts{}); err == nil {
		t.Fatal("session with unknown delivery should error")
	}
	if len(fr.sess) != 0 {
		t.Errorf("malformed session must not reach the runner; got %d calls", len(fr.sess))
	}
}

// The SupportsHeadless conjunction: headless block AND hooks both required; drop
// either and it is false.
func TestSupportsHeadlessConjunction(t *testing.T) {
	headlessInv := InvSpec{Prefix: []string{"-p"}, Prompt: DeliveryArgAfterDashDash}
	tests := []struct {
		name     string
		hooks    bool
		headless bool
		want     bool
	}{
		{name: "block and hooks", hooks: true, headless: true, want: true},
		{name: "block but no hooks", hooks: false, headless: true, want: false},
		{name: "hooks but no block", hooks: true, headless: false, want: false},
		{name: "neither", hooks: false, headless: false, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			desc := Descriptor{
				Name:  "x",
				Hooks: tt.hooks,
				Invocations: map[Mode]InvSpec{
					ModeSession: {Prefix: []string{"s"}, Prompt: DeliveryArgAfterDashDash},
				},
			}
			if tt.headless {
				desc.Invocations[ModeHeadless] = headlessInv
			}
			a, _, _ := newFakeDescriptor(desc)
			if got := a.Caps().SupportsHeadless; got != tt.want {
				t.Errorf("SupportsHeadless = %v; want %v", got, tt.want)
			}
		})
	}
}

// effortArgs at member granularity: a requested member the descriptor does not
// render is dropped with a warning (the same matcher as a provider with no effort
// map at all), while a rendered member passes through.
func TestEffortArgsPerMemberGap(t *testing.T) {
	desc := claudeDescriptor()
	desc.Effort = map[Effort][]string{EffortLow: {"--effort", "low"}} // only low
	a, fr, warns := newFakeDescriptor(desc)
	if err := a.Session(context.Background(), "P", Opts{Effort: "high"}); err != nil {
		t.Fatalf("Session: %v", err)
	}
	got := strings.Join(fr.sess[0].args, " ")
	if strings.Contains(got, "effort") || strings.Contains(got, "high") {
		t.Errorf("unrendered effort member leaked: %q", got)
	}
	if len(*warns) != 1 || !strings.Contains((*warns)[0], "effort=high") {
		t.Errorf("expected one drop warning for the missing member; got %v", *warns)
	}
}
