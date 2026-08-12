package agent

import (
	"fmt"
	"strings"
)

// Descriptor is the data form of a provider: everything claude.go / codex.go
// express as hand-written Go, reduced to a table. bowt never interprets a
// provider flag from it — the concatenator (buildArgs) only stitches the render
// lists together in a fixed order. In a later phase a Descriptor unmarshals from
// embedded built-in JSON or a ~/.bowt/agents/*.json drop-in; for now it is
// hand-built (built-ins stay the Go structs). The json tags are part of the
// schema even though nothing parses JSON in this phase.
type Descriptor struct {
	SchemaVersion int    `json:"schemaVersion"` // must be a known version
	Name          string `json:"name"`          // selector; was the switch case
	Bin           string `json:"bin"`           // PATH lookup (Capabilities.Bin)
	ConfigDir     string `json:"configDir"`     // e.g. ".claude"
	MemoryFile    string `json:"memoryFile"`    // fills {{MEMORY_FILE}}
	Hooks         bool   `json:"hooks"`         // Edit/Write guardrail present?

	Model       ModelSpec           `json:"model"`
	Effort      map[Effort][]string `json:"effort,omitempty"`    // per-member render; nil ⇒ no effort control
	Invocations map[Mode]InvSpec    `json:"invocations"`         // session (required), oneshot?, headless?
	ExtraArgs   []string            `json:"extraArgs,omitempty"` // §7 escape hatch — appended verbatim, unguarded
}

// ModelSpec carries the model surface. Only Render is consumed by the
// concatenator in this phase: opts.Model arrives already resolved (from
// spawn.ResolveModel) and is rendered verbatim, which is what keeps argv
// byte-identical with claude.go today. Aliases/Strong are schema data that feed
// spawn's ResolveModel split in a later phase; the concatenator ignores them.
type ModelSpec struct {
	Aliases map[string]string `json:"aliases,omitempty"` // "opus" → "claude-opus-4-8"
	Strong  string            `json:"strong,omitempty"`  // plan-mode default target
	Render  []string          `json:"render"`            // ["--model","{value}"]
}

// InvSpec is one invocation mode's fixed leading args plus how the prompt is
// delivered. oneshot ignores Prompt (the prompt is always fed on stdin, per the
// runner.oneshot contract in exec.go).
type InvSpec struct {
	Prefix []string `json:"prefix"`           // fixed leading args ("exec", "-p", …)
	Prompt Delivery `json:"prompt,omitempty"` // how the prompt reaches the CLI
}

// Effort is bowt's closed reasoning-effort vocabulary. A descriptor declares how
// each member renders for its provider (claude: high → ["--effort","high"]); a
// provider with no effort map drops the knob with a warning.
type Effort string

const (
	EffortLow    Effort = "low"
	EffortMedium Effort = "medium"
	EffortHigh   Effort = "high"
)

// efforts is the closed set of effort members, in canonical order. It is the
// SINGLE definition of the Effort vocabulary — the doctor validator consults
// isKnownEffort rather than restating {low,medium,high}, so the value check
// cannot drift from the enum. effortArgs already keys off the same Effort type.
var efforts = []Effort{EffortLow, EffortMedium, EffortHigh}

// isKnownEffort reports whether e is a member of the Effort enum.
func isKnownEffort(e Effort) bool {
	for _, k := range efforts {
		if e == k {
			return true
		}
	}
	return false
}

// Delivery is a small closed bowt-owned enum: how the prompt is passed to a
// session/headless invocation.
//
//	"arg-after-dashdash" → append ["--", prompt]   (claude/codex today)
//	"flag:<name>"        → append ["<name>", prompt]
//	"arg"                → append [prompt]
//
// oneshot is ALWAYS stdin (the runner.oneshot contract, exec.go), so its Prompt
// field is ignored.
type Delivery string

const (
	DeliveryArgAfterDashDash Delivery = "arg-after-dashdash"
	DeliveryArg              Delivery = "arg"
	// The flag form is "flag:<name>" — a prefix, matched in deliver().
	deliveryFlagPrefix = "flag:"
)

// Mode is the INVOCATION mode key of Descriptor.Invocations — how bowt drives
// the CLI. It is DISTINCT from spawn.Mode (plan/impl, the lane's intent); there
// is no import cycle or collision, so both keep the name Mode in their own
// package, but they are unrelated vocabularies.
type Mode string

const (
	ModeSession  Mode = "session"
	ModeOneshot  Mode = "oneshot"
	ModeHeadless Mode = "headless"
)

// render returns a copy of list with every "{value}" placeholder replaced by v.
// A list with no placeholder is returned unchanged (a fresh copy), so the caller
// never aliases the descriptor's slice.
func render(list []string, v string) []string {
	out := make([]string, len(list))
	for i, s := range list {
		out[i] = strings.ReplaceAll(s, "{value}", v)
	}
	return out
}

// isLegalDelivery reports whether d is a prompt-delivery form bowt can render:
// one of the two fixed forms, or "flag:<name>" with a non-empty flag name. It is
// the SINGLE source of Delivery legality — both deliver() (runtime) and the
// doctor validator (validateDescriptor) consult it, so a descriptor that passes
// doctor can never fail in deliver(), and vice versa. Do not fork this rule.
func isLegalDelivery(d Delivery) bool {
	switch {
	case d == DeliveryArgAfterDashDash, d == DeliveryArg:
		return true
	case strings.HasPrefix(string(d), deliveryFlagPrefix):
		return strings.TrimPrefix(string(d), deliveryFlagPrefix) != ""
	default:
		return false
	}
}

// deliver renders the prompt-delivery tail for a session/headless invocation.
// An unknown or empty Delivery is a hard error: a session with no way to pass
// the prompt is a malformed descriptor, not a silent no-op. Legality is decided
// by isLegalDelivery (shared with the validator); this only renders the tail.
func deliver(d Delivery, prompt string) ([]string, error) {
	if !isLegalDelivery(d) {
		if strings.HasPrefix(string(d), deliveryFlagPrefix) {
			return nil, fmt.Errorf("agent descriptor: prompt delivery %q has an empty flag name", d)
		}
		return nil, fmt.Errorf("agent descriptor: unknown prompt delivery %q (want %q, %q, or %q)",
			d, DeliveryArgAfterDashDash, DeliveryArg, "flag:<name>")
	}
	switch d {
	case DeliveryArgAfterDashDash:
		return []string{"--", prompt}, nil
	case DeliveryArg:
		return []string{prompt}, nil
	default: // flag:<non-empty>, per isLegalDelivery
		return []string{strings.TrimPrefix(string(d), deliveryFlagPrefix), prompt}, nil
	}
}

// effortArgs renders the effort tail. Nothing when no effort is requested. When
// the descriptor has a render for the requested member, that render list (copied)
// is returned. Otherwise the knob is dropped with a warning — this is the single
// matcher that reproduces both codex (no effort map ⇒ drop+warn) and a
// per-member gap (a member the provider does not render). The warning text and
// name-keying match codex.go verbatim, so the phase-3 gate stays green.
func (d Descriptor) effortArgs(effort string, warnf func(string, ...any)) []string {
	if effort == "" {
		return nil
	}
	if r, ok := d.Effort[Effort(effort)]; ok {
		return append([]string(nil), r...)
	}
	warnf("agent %s: dropping --effort=%s (this provider has no reasoning-effort control)", d.Name, effort)
	return nil
}
