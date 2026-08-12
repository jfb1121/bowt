package agent

import (
	"context"
	"strings"
	"testing"
)

// stubAgent is a throwaway Agent for the registry mechanism tests. register /
// newWith / knownAgents only care that a factory returns a non-nil Agent, not
// what it does, so these tests need no real provider. It replaces the former
// claudeAgent{} factory bodies now that the built-in structs are gone (built-ins
// are descriptorAgents parsed from embedded JSON); its behaviour is covered by
// descriptor_test.go and the argv gate in agent_test.go, not here.
type stubAgent struct {
	run   runner
	warnf func(string, ...any)
}

func (stubAgent) Caps() Capabilities                              { return Capabilities{} }
func (stubAgent) Session(context.Context, string, Opts) error     { return nil }
func (stubAgent) Oneshot(context.Context, string) (string, error) { return "", nil }
func (stubAgent) Headless(context.Context, string, Opts) error    { return nil }

// register + newWith lookup: a freshly registered provider is reachable through
// newWith and its factory receives the injected runner and warnf.
func TestRegisterAndLookup(t *testing.T) {
	restore := swapRegistry(t)
	defer restore()

	fr := &fakeRunner{}
	var warns []string
	register("stub", func(r runner, warnf func(string, ...any)) (Agent, error) {
		if r != fr {
			t.Errorf("factory got a different runner than injected")
		}
		warnf("touched") // prove the sink is wired through
		return stubAgent{run: r, warnf: warnf}, nil
	})

	a, err := newWith("stub", fr, func(format string, args ...any) {
		warns = append(warns, format)
	})
	if err != nil {
		t.Fatalf("newWith(stub): %v", err)
	}
	if a == nil {
		t.Fatal("newWith(stub) returned nil agent")
	}
	if len(warns) != 1 || warns[0] != "touched" {
		t.Errorf("warnf not forwarded to factory; got %v", warns)
	}
}

// register panics on a duplicate name, so a drop-in colliding with a built-in
// fails loudly rather than silently shadowing it.
func TestRegisterDuplicatePanics(t *testing.T) {
	restore := swapRegistry(t)
	defer restore()

	register("dup", func(r runner, warnf func(string, ...any)) (Agent, error) {
		return stubAgent{run: r, warnf: warnf}, nil
	})

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("register called twice should panic")
		}
		if msg, ok := r.(string); !ok || !strings.Contains(msg, "dup") {
			t.Errorf("panic should name the duplicated provider; got %v", r)
		}
	}()
	register("dup", func(r runner, warnf func(string, ...any)) (Agent, error) {
		return stubAgent{run: r, warnf: warnf}, nil
	})
}

// The unknown-name error lists the registered providers sorted (deterministic),
// so it reads "claude, codex" today regardless of map iteration order.
func TestUnknownNameListsSortedKnown(t *testing.T) {
	_, err := newWith("bogus", &fakeRunner{}, func(string, ...any) {})
	if err == nil {
		t.Fatal("unknown name should be a hard error")
	}
	if got := err.Error(); got != `unknown agent "bogus" (known: claude, codex)` {
		t.Errorf("unknown-name error = %q; want the sorted known-list form", got)
	}
}

// knownAgents is sorted regardless of registration order.
func TestKnownAgentsSorted(t *testing.T) {
	restore := swapRegistry(t)
	defer restore()

	register("zeta", func(r runner, warnf func(string, ...any)) (Agent, error) {
		return stubAgent{run: r, warnf: warnf}, nil
	})
	register("alpha", func(r runner, warnf func(string, ...any)) (Agent, error) {
		return stubAgent{run: r, warnf: warnf}, nil
	})
	got := strings.Join(knownAgents(), ",")
	if got != "alpha,zeta" {
		t.Errorf("knownAgents = %q; want alpha,zeta", got)
	}
}

// swapRegistry replaces the package registry with an empty one for the duration
// of a test that mutates it (register panics on duplicates), then restores the
// real one so other tests keep seeing claude/codex.
func swapRegistry(t *testing.T) func() {
	t.Helper()
	saved := registry
	registry = map[string]factory{}
	return func() { registry = saved }
}
