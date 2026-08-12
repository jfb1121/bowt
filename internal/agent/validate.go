package agent

import (
	"fmt"
	"sort"
	"strings"
)

// supportedSchemaVersions is the set of descriptor schemaVersion values bowt
// understands. Today only 1 exists; an unknown (or zero) version is rejected —
// matching the "empty prompt is a hard error, never silent" stance (RFC §14.3):
// a descriptor written against a schema this binary predates must fail loudly at
// doctor time, not be silently best-effort-parsed.
var supportedSchemaVersions = map[int]bool{1: true}

// valueToken is the sole placeholder a render list may contain (see render() in
// descriptor.go). More than one in a single list is a descriptor bug: render()
// replaces EVERY occurrence with the same resolved scalar, so two tokens can
// never denote two distinct values.
const valueToken = "{value}"

// Validate runs the static schema checks (RFC §11.1) on ag's descriptor and
// returns one problem string per violation (empty ⇒ valid). checked is false
// when ag is not a descriptor-backed provider — a future non-descriptor Agent
// then skips schema validation rather than spuriously failing.
//
// It type-asserts to the concrete descriptorAgent internally, so the Descriptor
// stays ENCAPSULATED in package agent: callers (doctor) get []string, never the
// raw Descriptor. Post-phase-3 every built-in and drop-in provider is a
// descriptorAgent, so checked is true for any real --agent today.
func Validate(ag Agent) (problems []string, checked bool) {
	da, ok := ag.(descriptorAgent)
	if !ok {
		return nil, false
	}
	return validateDescriptor(da.desc), true
}

// Provenance reports WHERE ag's descriptor came from: builtin=true for a trusted
// embedded built-in, or builtin=false with the drop-in file path for a
// ~/.bowt/agents/*.json drop-in. ok is false for a non-descriptor provider. Like
// Validate, it type-asserts internally so Origin (and Descriptor) stay
// encapsulated in package agent — callers get scalars, never the raw types. This
// is doctor-facing provenance only; the headless trust boundary still lives in
// Caps (SupportsHeadless), NOT here.
func Provenance(ag Agent) (builtin bool, path string, ok bool) {
	da, isDesc := ag.(descriptorAgent)
	if !isDesc {
		return false, "", false
	}
	return da.origin.isBuiltin(), da.origin.path, true
}

// validateDescriptor is the pure rule set (RFC §11.1), factored out of Validate
// so it is table-tested without constructing an Agent. Each violation is a
// distinct, human-readable problem string; the slice is empty for a valid
// descriptor (so a valid built-in produces zero problems and keeps doctor
// green). The rules deliberately SHARE the runtime's vocabulary — isLegalDelivery
// and isKnownEffort (descriptor.go) — rather than restating it, so the validator
// can never disagree with deliver()/effortArgs. This is a fixed, non-Turing
// check set (RFC §10): no new schema features.
func validateDescriptor(d Descriptor) []string {
	var problems []string

	// 1. schemaVersion is a version bowt understands.
	if !supportedSchemaVersions[d.SchemaVersion] {
		problems = append(problems, fmt.Sprintf(
			"schemaVersion %d is not supported (known: %s)", d.SchemaVersion, knownSchemaVersionList()))
	}

	// 2. required fields present.
	if d.Name == "" {
		problems = append(problems, `missing required field "name"`)
	}
	if d.Bin == "" {
		problems = append(problems, `missing required field "bin"`)
	}
	if _, ok := d.Invocations[ModeSession]; !ok {
		problems = append(problems, fmt.Sprintf("missing required %q invocation", ModeSession))
	}

	// 3. every effort map key is a member of the Effort enum.
	for _, k := range sortedEffortKeys(d.Effort) {
		if !isKnownEffort(k) {
			problems = append(problems, fmt.Sprintf(
				"effort key %q is not a known effort (want %s)", k, effortEnumList()))
		}
	}

	// 4. every session/headless prompt delivery is a legal Delivery. oneshot is
	// skipped: its prompt is fed on stdin by contract (exec.go), so InvSpec.Prompt
	// is ignored there and need not be a legal delivery.
	for _, m := range []Mode{ModeSession, ModeHeadless} {
		inv, ok := d.Invocations[m]
		if !ok {
			continue
		}
		if !isLegalDelivery(inv.Prompt) {
			problems = append(problems, fmt.Sprintf(
				"%q invocation has an illegal prompt delivery %q (want %q, %q, or %q)",
				m, inv.Prompt, DeliveryArgAfterDashDash, DeliveryArg, "flag:<name>"))
		}
	}

	// 5. every render list carries at most one {value} token: Model.Render and
	// each effort render list.
	if n := countValueTokens(d.Model.Render); n > 1 {
		problems = append(problems, fmt.Sprintf(
			"model render list has %d %s tokens (at most one allowed)", n, valueToken))
	}
	for _, k := range sortedEffortKeys(d.Effort) {
		if n := countValueTokens(d.Effort[k]); n > 1 {
			problems = append(problems, fmt.Sprintf(
				"effort %q render list has %d %s tokens (at most one allowed)", k, n, valueToken))
		}
	}

	return problems
}

// countValueTokens totals the {value} placeholders across a render list's
// strings (a single string may hold more than one).
func countValueTokens(list []string) int {
	n := 0
	for _, s := range list {
		n += strings.Count(s, valueToken)
	}
	return n
}

// sortedEffortKeys returns the effort map's keys in a stable order so problem
// strings (and thus doctor output) are deterministic across runs — map
// iteration order is randomized.
func sortedEffortKeys(m map[Effort][]string) []Effort {
	keys := make([]Effort, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	return keys
}

// knownSchemaVersionList renders the supported schemaVersion set as a sorted,
// comma-joined string for the unknown-version problem message.
func knownSchemaVersionList() string {
	vs := make([]int, 0, len(supportedSchemaVersions))
	for v := range supportedSchemaVersions {
		vs = append(vs, v)
	}
	sort.Ints(vs)
	parts := make([]string, len(vs))
	for i, v := range vs {
		parts[i] = fmt.Sprintf("%d", v)
	}
	return strings.Join(parts, ", ")
}

// effortEnumList renders the Effort enum as a comma-joined string for the
// unknown-effort-key problem message. It reads the same efforts slice the
// runtime keys off of, so the message can never list a stale vocabulary.
func effortEnumList() string {
	parts := make([]string, len(efforts))
	for i, e := range efforts {
		parts[i] = string(e)
	}
	return strings.Join(parts, ", ")
}
