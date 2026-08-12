package agent

import (
	"reflect"
	"testing"
)

// The whole point of phase 3: the descriptor PARSED from the embedded JSON must
// equal the phase-2 hand-built reference descriptor (proven byte-identical to the
// old claudeAgent/codexAgent structs). Deep-equal here proves the JSON is correct
// independent of argv; agent_test.go then proves the wiring end-to-end. builtins
// is populated at init by loadBuiltins().
func TestBuiltinDescriptorsMatchReferences(t *testing.T) {
	tests := []struct {
		name string
		want Descriptor
	}{
		{name: "claude", want: claudeDescriptor()},
		{name: "codex", want: codexDescriptor()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := builtins[tt.name]
			if !ok {
				t.Fatalf("no built-in parsed for %q", tt.name)
			}
			if !reflect.DeepEqual(got.desc, tt.want) {
				t.Errorf("%s.json parsed =\n%+v\nwant\n%+v", tt.name, got.desc, tt.want)
			}
		})
	}
}

// The registered factory must hand out a descriptorAgent carrying the parsed
// built-in descriptor — proving init() wires the JSON through newWith (not an
// empty or wrong descriptor), independent of the argv gate.
func TestBuiltinRegisteredAsDescriptorAgent(t *testing.T) {
	tests := []struct {
		name string
		want Descriptor
	}{
		{name: "claude", want: claudeDescriptor()},
		{name: "codex", want: codexDescriptor()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, err := newWith(tt.name, &fakeRunner{}, func(string, ...any) {})
			if err != nil {
				t.Fatalf("newWith(%q): %v", tt.name, err)
			}
			da, ok := a.(descriptorAgent)
			if !ok {
				t.Fatalf("newWith(%q) = %T; want descriptorAgent", tt.name, a)
			}
			if !reflect.DeepEqual(da.desc, tt.want) {
				t.Errorf("%s descriptor via newWith =\n%+v\nwant\n%+v", tt.name, da.desc, tt.want)
			}
		})
	}
}

// Provenance infrastructure for later phases: VERSION is non-empty and each
// built-in exposes a git blob hash; an unknown name reports absence.
func TestBuiltinProvenanceAccessors(t *testing.T) {
	if BuiltinVersion() == "" {
		t.Error("BuiltinVersion() is empty; want the embedded agents/VERSION scalar")
	}
	for _, name := range []string{"claude", "codex"} {
		h, ok := BuiltinHash(name)
		if !ok {
			t.Errorf("BuiltinHash(%q): ok=false; want a registered built-in", name)
		}
		if h == "" {
			t.Errorf("BuiltinHash(%q) is empty; want a git blob hash", name)
		}
	}
	if _, ok := BuiltinHash("nope"); ok {
		t.Error("BuiltinHash(\"nope\"): ok=true; want false for an unknown name")
	}
}
