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
		Name:            "claude",
		Bin:             "claude",
		ConfigDir:       ".claude",
		MemoryFile:      "CLAUDE.md",
		SupportsOneshot: true,
		SupportsHooks:   true,
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
