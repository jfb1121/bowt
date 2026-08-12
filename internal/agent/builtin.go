package agent

import (
	"crypto/sha1" //nolint:gosec // git object hashing is defined in terms of SHA-1; not a security use.
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"strings"
)

// builtinFS ships the built-in provider descriptors inside the binary, so an
// installed bowt carries its providers with it (no lookup relative to an install
// dir). This mirrors spawn's embedded prompt files. go:embed can only reach
// files at/below this package dir, so the JSON lives under internal/agent/agents/.
//
//go:embed agents/claude.json agents/codex.json agents/VERSION
var builtinFS embed.FS

// builtinVersionPath is the embedded descriptor-set version file (agents/VERSION),
// the descriptor analogue of spawn's prompts/VERSION.
const builtinVersionPath = "agents/VERSION"

// builtinPaths maps a selector name to its embedded descriptor JSON path. init()
// parses each into a descriptorAgent factory; the map is the single list of
// built-ins (a drop-in loader in a later phase registers additional names).
var builtinPaths = map[string]string{
	"claude": "agents/claude.json",
	"codex":  "agents/codex.json",
}

// builtin is a parsed built-in descriptor plus its provenance scalar, computed
// once at init. hash is the git blob hash of the raw embedded JSON — the value
// `git hash-object` prints for the committed file.
type builtin struct {
	desc Descriptor
	hash string
}

var (
	// builtins holds every parsed built-in descriptor keyed by selector name.
	// Populated by loadBuiltins() from init(); read by the registered factories
	// and by the provenance accessors.
	builtins = map[string]builtin{}
	// builtinVersion is the trimmed agents/VERSION scalar.
	builtinVersion string
)

// loadBuiltins parses every embedded built-in descriptor and records its
// provenance. The built-ins are TRUSTED and must be valid: a parse failure is a
// build/ship bug, exactly like a malformed embedded prompt, so it panics rather
// than degrading — mirroring register()'s duplicate-name panic and spawn's
// hard-error-on-empty-prompt stance. It does NOT statically validate arbitrary
// descriptors (effort keys, prompt-delivery legality, …); that is doctor's job
// in a later phase. Called once, from init(), before any registration.
func loadBuiltins() {
	v, err := fs.ReadFile(builtinFS, builtinVersionPath)
	if err != nil {
		panic(fmt.Sprintf("agent: reading embedded %s: %v", builtinVersionPath, err))
	}
	builtinVersion = strings.TrimSpace(string(v))
	if builtinVersion == "" {
		panic(fmt.Sprintf("agent: embedded %s is empty", builtinVersionPath))
	}
	for name, path := range builtinPaths {
		raw, err := fs.ReadFile(builtinFS, path)
		if err != nil {
			panic(fmt.Sprintf("agent: reading embedded %s: %v", path, err))
		}
		var d Descriptor
		if err := json.Unmarshal(raw, &d); err != nil {
			panic(fmt.Sprintf("agent: parsing embedded %s: %v", path, err))
		}
		// The filename is the selector; a mismatch is a build bug that would let
		// register() key a provider under the wrong name.
		if d.Name != name {
			panic(fmt.Sprintf("agent: embedded %s declares name %q; want %q", path, d.Name, name))
		}
		builtins[name] = builtin{desc: d, hash: gitBlobHash(raw)}
	}
}

// gitBlobHash computes the git object hash of b — the same value `git
// hash-object` prints — so a descriptor's provenance hash matches what git
// reports for the committed file. Computed in-process (not by shelling out to
// git) so provenance works even where the embedded bytes aren't a git blob. This
// mirrors spawn.gitBlobHash; it is duplicated here (rather than imported) to keep
// the agent package free of a spawn dependency.
func gitBlobHash(b []byte) string {
	h := sha1.New() //nolint:gosec // see package import note.
	fmt.Fprintf(h, "blob %d\x00", len(b))
	h.Write(b)
	return hex.EncodeToString(h.Sum(nil))
}

// BuiltinVersion returns the embedded agents/VERSION scalar — the built-in
// descriptor set's version. This is provenance infrastructure for a later phase
// (doctor / a descriptor provenance line); no output consumes it yet, so the
// spawn provenance line format is unchanged.
func BuiltinVersion() string { return builtinVersion }

// BuiltinHash returns the git blob hash of the named built-in descriptor's
// embedded JSON, or ("", false) if name is not a built-in. Same provenance role
// as BuiltinVersion — available for later wiring, unused this phase.
func BuiltinHash(name string) (string, bool) {
	b, ok := builtins[name]
	return b.hash, ok
}
