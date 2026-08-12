// Package agent makes bowt's coding-agent invocation provider-neutral. A lane
// can run under a different CLI without forking policy: the prompts, the lock,
// and the writeback contract are all provider-agnostic — the coupling was only
// ever at the invocation boundary (see rfc/agent-adapters.md).
//
// An Agent exposes the two genuinely different ways bowt uses a CLI, and a
// provider may support one and not the other:
//
//   - Session — start a long-running agent with an initial prompt; it works,
//     writes files, and exits. stdio is inherited (this is what `spawn` uses).
//   - Oneshot — pipe a prompt in, capture stdout, no side effects (what `review`
//     will use in a later slice).
//
// Each provider also carries static Capabilities so callers can check, loudly
// and early, whether a lane can run at all under the selected CLI.
package agent

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/jfb1121/bowt/internal/output"
)

// DefaultAgent is the provider used when nothing selects one — today's claude,
// so existing users see no change.
const DefaultAgent = "claude"

// Capabilities is a provider's static descriptor: what it is called, how it is
// invoked, and which of bowt's agent features it can support. The Supports*
// fields let callers refuse (or warn) up front rather than discovering the gap
// mid-lane.
type Capabilities struct {
	// Name is the selector ("claude", "codex").
	Name string
	// Bin is the executable looked up on PATH.
	Bin string
	// ConfigDir is the per-provider config directory (e.g. ".claude"), copied
	// into worktrees and where docs install.
	ConfigDir string
	// MemoryFile is the provider's project-memory filename (e.g. "CLAUDE.md"),
	// substituted into the spawn prompts as {{MEMORY_FILE}}.
	MemoryFile string
	// SupportsOneshot reports whether Oneshot (stdin→stdout, no side effects) works.
	SupportsOneshot bool
	// SupportsHooks reports whether the provider has an Edit/Write guardrail
	// system bowt can install into.
	SupportsHooks bool
	// SupportsHeadless reports whether the provider can run a full agentic pass
	// non-interactively with no TTY (Headless). Because a headless run implies
	// --dangerously-skip-permissions (nothing can answer a prompt), the only
	// safety substitute is the Edit/Write hook guardrail — so a provider without
	// SupportsHooks must NOT claim SupportsHeadless (see RequireHeadless).
	SupportsHeadless bool
}

// Opts are the tunable knobs for a Session. Model/Effort are best-effort: a
// provider that cannot express a knob drops it with a warning rather than
// failing or silently pretending (see the RFC). Stdin/Stdout/Stderr wire the
// child's streams; a nil stream falls back to the process's own.
type Opts struct {
	Model  string
	Effort string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// Agent is a provider adapter. It is defined here (not at a single consumer)
// because it has two real implementations (claude, codex) plus test fakes, and
// three consumers (spawn, doctor, and later review) share it.
type Agent interface {
	// Caps returns the provider's static descriptor.
	Caps() Capabilities
	// Session runs the agent as a child with the prompt and inherited stdio,
	// blocking until it exits. This is what interactive spawn uses.
	Session(ctx context.Context, prompt string, opts Opts) error
	// Oneshot feeds prompt on stdin and returns captured stdout with no side
	// effects. Providers with SupportsOneshot=false return a hard error.
	Oneshot(ctx context.Context, prompt string) (string, error)
	// Headless runs a full agentic pass with NO interactive stdin, streaming
	// stdout/stderr through opts (the supervisor points them at the lane log).
	// It blocks until the child exits — like Session, but non-interactive, so it
	// implies --dangerously-skip-permissions. Providers with
	// SupportsHeadless=false return a hard error.
	Headless(ctx context.Context, prompt string, opts Opts) error
}

// New returns the named provider wired to real process execution. An unknown
// name is a hard error (never a silent fallback to the default).
func New(name string) (Agent, error) {
	return newWith(name, osRunner{}, output.Errf)
}

// factory builds a provider adapter with an injected process runner and warning
// sink. Each provider registers one under its selector name; newWith looks it up
// instead of switching on a hardcoded name.
type factory func(r runner, warnf func(string, ...any)) (Agent, error)

// registry maps a provider selector ("claude", "codex") to its factory. It is
// populated by register() at package-init time — the database/sql driver
// pattern — so adding a provider is a registration, not a new switch arm.
var registry = map[string]factory{}

// register adds a provider factory under name. It panics on a duplicate name
// (as database/sql.Register does), so a drop-in provider colliding with a
// built-in fails loudly at init rather than silently shadowing it.
func register(name string, f factory) {
	if _, dup := registry[name]; dup {
		panic(fmt.Sprintf("agent: register called twice for %q", name))
	}
	registry[name] = f
}

// init parses the embedded built-in descriptors and registers a descriptorAgent
// factory for each. The built-ins are now DATA (agents/*.json), not hand-written
// Go structs: newWith("claude") returns a descriptorAgent parsed from claude.json
// that is behaviourally byte-identical to the former claudeAgent (proven in
// descriptor_test.go and builtin_test.go). loadBuiltins panics on a malformed
// built-in — a build/ship bug — before any registration runs.
func init() {
	loadBuiltins()
	for name := range builtinPaths {
		desc := builtins[name].desc
		register(name, func(r runner, warnf func(string, ...any)) (Agent, error) {
			// Built-ins are trusted: stamp origin=builtin so newWith("claude")
			// keeps SupportsHeadless=true (the origin term in Caps).
			return descriptorAgent{desc: desc, run: r, warnf: warnf, origin: builtinOrigin()}, nil
		})
	}
}

// knownAgents returns the registered selector names sorted, so the unknown-name
// error lists them deterministically — map iteration order is randomized, and
// the sorted list reads "claude, codex" today.
func knownAgents() []string {
	names := make([]string, 0, len(registry))
	for n := range registry {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// newWith builds a provider with an injected process runner and warning sink,
// so tests exercise selection, argument construction, and knob-degradation
// without launching a real CLI. An unknown name is a hard error listing the
// registered providers (never a silent fallback to the default).
func newWith(name string, r runner, warnf func(string, ...any)) (Agent, error) {
	f, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("unknown agent %q (known: %s)", name, strings.Join(knownAgents(), ", "))
	}
	return f(r, warnf)
}

// Select resolves the provider by precedence: an explicit --agent flag, then
// BOWT_AGENT, then GWT_AGENT (back-compat) from getenv, then DefaultAgent.
// getenv is injected (pass os.Getenv) so precedence is testable. An unknown
// name — from any source — is a hard error.
func Select(flag string, getenv func(string) string) (Agent, error) {
	name := flag
	if name == "" {
		name = firstNonEmpty(getenv("BOWT_AGENT"), getenv("GWT_AGENT"))
	}
	if name == "" {
		name = DefaultAgent
	}
	return New(name)
}

// RequireOneshot returns a hard error naming the missing capability when a
// provider cannot run one-shot — call it up front (before any work) in a
// command that needs stdin→stdout, so the lane fails loudly rather than
// discovering the gap mid-flight.
func RequireOneshot(a Agent) error {
	c := a.Caps()
	if !c.SupportsOneshot {
		return fmt.Errorf("agent %q does not support one-shot mode (stdin→stdout, no side effects), which this command requires", c.Name)
	}
	return nil
}

// RequireHeadless returns a hard error when a provider cannot run headless —
// call it up front in `bowt spawn --headless`, so a lane refuses loudly rather
// than launching an unattended, guardrail-free run. The gate is deliberately
// the Edit/Write hook guardrail: a headless run implies skip-perms, so a
// provider without hooks (SupportsHeadless=false) would run with zero
// protection. Mirrors RequireOneshot.
func RequireHeadless(a Agent) error {
	c := a.Caps()
	if !c.SupportsHeadless {
		return fmt.Errorf("agent %q cannot run headless: an unattended run needs the Edit/Write hook guardrail (--dangerously-skip-permissions is mandatory with no TTY), which this provider lacks", c.Name)
	}
	return nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
