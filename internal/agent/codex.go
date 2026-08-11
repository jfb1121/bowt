package agent

import (
	"context"
	"fmt"
)

// codexAgent is a deliberately minimal second provider. Its purpose in this
// slice is to give the capability checks a real "no" to fire against:
// SupportsOneshot=false (so RequireOneshot and review can be seen to refuse it)
// and no --effort knob (so unknown-knob degradation has something to drop).
//
// The Session argv is a plausible placeholder, not a validated codex CLI
// contract — bowt never launches codex in tests, and wiring the real flags is
// out of scope until someone runs a lane under it.
type codexAgent struct {
	run   runner
	warnf func(string, ...any)
}

func (codexAgent) Caps() Capabilities {
	return Capabilities{
		Name:            "codex",
		Bin:             "codex",
		ConfigDir:       ".codex",
		MemoryFile:      "AGENTS.md",
		SupportsOneshot: false,
		SupportsHooks:   false,
	}
}

func (a codexAgent) Session(ctx context.Context, prompt string, opts Opts) error {
	args := []string{"exec"}
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	// codex has no reasoning-effort knob: drop it with a warning rather than
	// erroring or silently pretending it took effect (see the RFC).
	if opts.Effort != "" {
		a.warnf("agent codex: dropping --effort=%s (this provider has no reasoning-effort control)", opts.Effort)
	}
	args = append(args, "--", prompt)
	return a.run.session(ctx, "codex", args, opts)
}

func (codexAgent) Oneshot(ctx context.Context, prompt string) (string, error) {
	// Guarded here too, so a caller that skips RequireOneshot still fails
	// loudly instead of running a broken pipeline.
	return "", fmt.Errorf("agent %q does not support one-shot mode (stdin→stdout, no side effects)", "codex")
}
