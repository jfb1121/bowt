package agent

import "context"

// claudeAgent is the default provider. Its Session reproduces bowt's original
// hardcoded invocation byte-for-byte:
//
//	claude --dangerously-skip-permissions [--model M] [--effort E] -- <prompt>
//
// so switching to the adapter layer changes nothing for existing users.
type claudeAgent struct {
	run   runner
	warnf func(string, ...any)
}

func (claudeAgent) Caps() Capabilities {
	return Capabilities{
		Name:             "claude",
		Bin:              "claude",
		ConfigDir:        ".claude",
		MemoryFile:       "CLAUDE.md",
		SupportsOneshot:  true,
		SupportsHooks:    true,
		SupportsHeadless: true, // has an Edit/Write hook guardrail (see RequireHeadless)
	}
}

func (a claudeAgent) Session(ctx context.Context, prompt string, opts Opts) error {
	args := []string{"--dangerously-skip-permissions"}
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	if opts.Effort != "" {
		args = append(args, "--effort", opts.Effort)
	}
	args = append(args, "--", prompt)
	return a.run.session(ctx, "claude", args, opts)
}

func (a claudeAgent) Oneshot(ctx context.Context, prompt string) (string, error) {
	// -p is claude's non-interactive (print) mode: prompt on stdin, result on
	// stdout, no session side effects.
	return a.run.oneshot(ctx, "claude", []string{"-p"}, prompt)
}

// Headless runs a full agentic pass with no TTY:
//
//	claude -p --dangerously-skip-permissions [--model M] [--effort E] -- <prompt>
//
// -p is the non-interactive print mode; --dangerously-skip-permissions is
// MANDATORY (nothing can answer a permission prompt without a TTY) and already
// appears in the interactive Session argv (claude.go). The Model/Effort knobs
// mirror Session so a plan-mode headless lane keeps its resolved defaults rather
// than silently dropping them. The prompt goes after `--`.
func (a claudeAgent) Headless(ctx context.Context, prompt string, opts Opts) error {
	args := []string{"-p", "--dangerously-skip-permissions"}
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	if opts.Effort != "" {
		args = append(args, "--effort", opts.Effort)
	}
	args = append(args, "--", prompt)
	return a.run.headless(ctx, "claude", args, opts)
}
