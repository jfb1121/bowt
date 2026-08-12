package agent

import (
	"strings"
	"testing"
)

// TestValidateDescriptorValidBuiltins is the green-path gate: the reference
// claude and codex descriptors (the shapes the built-in JSON parses into) must
// produce ZERO problems, or doctor would flag a valid built-in and TestDoctorAgent
// would break.
func TestValidateDescriptorValidBuiltins(t *testing.T) {
	for _, d := range []struct {
		name string
		desc Descriptor
	}{
		{"claude", claudeDescriptor()},
		{"codex", codexDescriptor()},
	} {
		if problems := validateDescriptor(d.desc); len(problems) != 0 {
			t.Errorf("%s: valid descriptor yielded problems: %v", d.name, problems)
		}
	}
}

// TestValidateDescriptorRules is one failure case per rule (RFC §11.1). Each
// mutates a valid claude descriptor into exactly one violation and asserts a
// problem mentioning the offending token surfaces.
func TestValidateDescriptorRules(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Descriptor)
		wantSub string // substring the surfaced problem must contain
	}{
		{
			name:    "unknown schemaVersion",
			mutate:  func(d *Descriptor) { d.SchemaVersion = 99 },
			wantSub: "schemaVersion 99",
		},
		{
			name:    "zero schemaVersion",
			mutate:  func(d *Descriptor) { d.SchemaVersion = 0 },
			wantSub: "schemaVersion 0",
		},
		{
			name:    "missing name",
			mutate:  func(d *Descriptor) { d.Name = "" },
			wantSub: `"name"`,
		},
		{
			name:    "missing bin",
			mutate:  func(d *Descriptor) { d.Bin = "" },
			wantSub: `"bin"`,
		},
		{
			name:    "missing session invocation",
			mutate:  func(d *Descriptor) { delete(d.Invocations, ModeSession) },
			wantSub: `"session"`,
		},
		{
			name:    "effort key outside enum",
			mutate:  func(d *Descriptor) { d.Effort[Effort("max")] = []string{"--effort", "max"} },
			wantSub: `effort key "max"`,
		},
		{
			name: "illegal prompt delivery",
			mutate: func(d *Descriptor) {
				d.Invocations[ModeSession] = InvSpec{Prefix: []string{"x"}, Prompt: Delivery("stdin")}
			},
			wantSub: `illegal prompt delivery "stdin"`,
		},
		{
			name: "flag delivery with empty name",
			mutate: func(d *Descriptor) {
				d.Invocations[ModeSession] = InvSpec{Prefix: []string{"x"}, Prompt: Delivery("flag:")}
			},
			wantSub: `illegal prompt delivery "flag:"`,
		},
		{
			name:    "model render list with two value tokens",
			mutate:  func(d *Descriptor) { d.Model.Render = []string{"--model", "{value}", "{value}"} },
			wantSub: "model render list has 2",
		},
		{
			name: "effort render list with two value tokens",
			mutate: func(d *Descriptor) {
				d.Effort[EffortHigh] = []string{"--effort", "{value}", "{value}"}
			},
			wantSub: `effort "high" render list has 2`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := claudeDescriptor()
			tt.mutate(&d)
			problems := validateDescriptor(d)
			if !containsSub(problems, tt.wantSub) {
				t.Errorf("problems %v; want one containing %q", problems, tt.wantSub)
			}
		})
	}
}

// TestValidateReachesDescriptor: Validate on a real descriptorAgent reports
// checked=true and validates its descriptor (a valid built-in ⇒ no problems; a
// mutated one ⇒ problems), proving the type assertion reaches the descriptor.
func TestValidateReachesDescriptor(t *testing.T) {
	good, _, _ := newFakeDescriptor(claudeDescriptor())
	if problems, checked := Validate(good); !checked || len(problems) != 0 {
		t.Errorf("Validate(valid) = (%v, %v); want ([], true)", problems, checked)
	}

	bad := claudeDescriptor()
	bad.SchemaVersion = 42
	agBad, _, _ := newFakeDescriptor(bad)
	if problems, checked := Validate(agBad); !checked || len(problems) == 0 {
		t.Errorf("Validate(bad) = (%v, %v); want (non-empty, true)", problems, checked)
	}
}

// TestValidateNonDescriptorSkips: a non-descriptor Agent (registry_test.go's
// stubAgent) yields checked=false so doctor skips schema validation for a
// hypothetical future non-descriptor provider.
func TestValidateNonDescriptorSkips(t *testing.T) {
	if problems, checked := Validate(stubAgent{}); checked || problems != nil {
		t.Errorf("Validate(non-descriptor) = (%v, %v); want (nil, false)", problems, checked)
	}
}

func containsSub(problems []string, sub string) bool {
	for _, p := range problems {
		if strings.Contains(p, sub) {
			return true
		}
	}
	return false
}
