package agent

import (
	"context"
	"fmt"
)

// descriptorAgent implements Agent from a Descriptor — the data-driven successor
// to the hand-written claudeAgent / codexAgent. A claude-shaped descriptor is
// behaviourally identical to claudeAgent (argv, Caps, warnings, refusals), and a
// codex-shaped one to codexAgent; that identity is what lets a later phase swap
// the built-in structs for embedded JSON descriptors with no test churn.
//
// In this phase nothing wires descriptorAgent to the built-ins: New/newWith still
// return the Go structs, and descriptorAgent is reachable only from tests.
type descriptorAgent struct {
	desc  Descriptor
	run   runner
	warnf func(string, ...any)
}

// Caps derives the static Capabilities from the descriptor. SupportsOneshot is
// pure presence of an oneshot invocation; SupportsHooks is the declared hooks
// flag; SupportsHeadless is the Go safety conjunction — a headless invocation
// present AND hooks:true — so a descriptor cannot declare headless-eligibility
// directly (see RequireHeadless / RFC §5). There is deliberately no
// SupportsEffort field on Capabilities: effort support is handled internally via
// the presence of the effort render map (effortArgs), so this stays parity-safe
// with the landed Capabilities struct.
func (a descriptorAgent) Caps() Capabilities {
	_, hasOneshot := a.desc.Invocations[ModeOneshot]
	_, hasHeadless := a.desc.Invocations[ModeHeadless]
	return Capabilities{
		Name:             a.desc.Name,
		Bin:              a.desc.Bin,
		ConfigDir:        a.desc.ConfigDir,
		MemoryFile:       a.desc.MemoryFile,
		SupportsOneshot:  hasOneshot,
		SupportsHooks:    a.desc.Hooks,
		SupportsHeadless: hasHeadless && a.desc.Hooks,
	}
}

// buildArgs is the concatenator (RFC §6): the one genuinely new logic. For a
// given invocation mode it stitches, in a fixed order,
//
//	Prefix ++ render(Model.Render, opts.Model) ++ effortArgs ++ ExtraArgs ++ deliver(Prompt, prompt)
//
// Model renders only when opts.Model is set (verbatim — already resolved
// upstream). effortArgs drops+warns on a gap. ExtraArgs are appended verbatim
// (§7). deliver appends the prompt tail for session/headless.
func (a descriptorAgent) buildArgs(m Mode, prompt string, opts Opts) ([]string, error) {
	inv, ok := a.desc.Invocations[m]
	if !ok {
		return nil, fmt.Errorf("agent %q has no %q invocation", a.desc.Name, m)
	}
	argv := append([]string(nil), inv.Prefix...)
	if opts.Model != "" {
		argv = append(argv, render(a.desc.Model.Render, opts.Model)...)
	}
	argv = append(argv, a.desc.effortArgs(opts.Effort, a.warnf)...)
	argv = append(argv, a.desc.ExtraArgs...)
	tail, err := deliver(inv.Prompt, prompt)
	if err != nil {
		return nil, err
	}
	return append(argv, tail...), nil
}

// Session builds the session argv and runs it as a child with inherited stdio.
func (a descriptorAgent) Session(ctx context.Context, prompt string, opts Opts) error {
	argv, err := a.buildArgs(ModeSession, prompt, opts)
	if err != nil {
		return err
	}
	return a.run.session(ctx, a.desc.Bin, argv, opts)
}

// Oneshot feeds the prompt on stdin (never via deliver — the runner.oneshot
// contract owns stdio) and returns captured stdout. A descriptor with no oneshot
// invocation refuses with codex's verbatim message, so the guard fires loudly
// even for a caller that skipped RequireOneshot.
func (a descriptorAgent) Oneshot(ctx context.Context, prompt string) (string, error) {
	inv, ok := a.desc.Invocations[ModeOneshot]
	if !ok {
		return "", fmt.Errorf("agent %q does not support one-shot mode (stdin→stdout, no side effects)", a.desc.Name)
	}
	return a.run.oneshot(ctx, a.desc.Bin, inv.Prefix, prompt)
}

// Headless builds the headless argv and runs it non-interactively. It refuses
// with codex's verbatim message unless SupportsHeadless holds — i.e. a headless
// invocation is present AND hooks:true. Gating on the full conjunction (not bare
// block presence) is the safety backstop: a descriptor that declares a headless
// block but no hook guardrail must not launch an unattended
// --dangerously-skip-permissions run, even from a caller that skipped
// RequireHeadless.
func (a descriptorAgent) Headless(ctx context.Context, prompt string, opts Opts) error {
	if !a.Caps().SupportsHeadless {
		return fmt.Errorf("agent %q does not support headless mode (no Edit/Write hook guardrail for an unattended run)", a.desc.Name)
	}
	argv, err := a.buildArgs(ModeHeadless, prompt, opts)
	if err != nil {
		return err
	}
	return a.run.headless(ctx, a.desc.Bin, argv, opts)
}
