package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// dropinSubdir is the user-level directory (under ~/.bowt) that holds drop-in
// provider descriptors. Drop-ins are USER-level, not repo-level: unlike config's
// repo .bowt resolution (config.Dir) or the per-repo extension loader, a
// provider a user adds applies to every repo, so it lives under the home dir.
// The on-disk discovery MECHANISM mirrors the extension loader (extension.List:
// ReadDir + suffix filter), but the location is home.
const dropinSubdir = ".bowt/agents"

// LoadDropins discovers and registers user drop-in providers from
// ~/.bowt/agents/*.json AFTER the built-ins (which package init() has already
// registered). It is the exported wrapper called ONCE at CLI startup (main.go),
// mirroring how the extension loader is invoked from main — NOT from package
// init(), because importing a package must not do disk I/O and init() cannot warn
// cleanly. warnf is the real sink (output.Errf) so a bad drop-in is surfaced to
// the user. A missing home dir or missing ~/.bowt/agents is the normal no-op case.
func LoadDropins(warnf func(string, ...any)) {
	home, err := os.UserHomeDir()
	if err != nil {
		warnf("agent drop-ins: cannot resolve home dir: %v", err)
		return
	}
	loadDropins(filepath.Join(home, dropinSubdir), warnf)
}

// loadDropins reads every *.json in dir and registers each as a drop-in-origin
// provider. dir is an explicit argument (not resolved internally) so the loader
// is table-testable with a temp dir + the swapRegistry helper.
//
// It is deliberately ROBUST, because drop-ins are UNTRUSTED input (unlike the
// built-ins, which panic on any defect): a file that fails to read or parse,
// lacks a "name", or lacks a "session" invocation is SKIPPED WITH A WARNING — one
// bad file must never brick bowt. It is deliberately PRECEDENCE-ORDERED: a name
// already registered (a built-in, or an earlier drop-in) is skipped with a
// warning too, so built-ins always win and there is no policy enum — the origin
// of the winning descriptor is simply whichever registered first. Files are
// processed in sorted order so "earlier drop-in wins" is deterministic.
//
// This does NOT do the rich static validation (effort keys ∈ enum, prompt ∈
// Delivery, render arity) that phase-5 doctor owns; only the minimal name+session
// sanity needed to register a usable provider.
func loadDropins(dir string, warnf func(string, ...any)) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		// No drop-in dir is the normal case; only a real read error is worth a warning.
		if !os.IsNotExist(err) {
			warnf("agent drop-ins: reading %s: %v", dir, err)
		}
		return
	}

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)

	for _, fname := range names {
		path := filepath.Join(dir, fname)
		raw, err := os.ReadFile(path)
		if err != nil {
			warnf("agent drop-in %s: %v — skipping", path, err)
			continue
		}
		var d Descriptor
		if err := json.Unmarshal(raw, &d); err != nil {
			warnf("agent drop-in %s: parse error: %v — skipping", path, err)
			continue
		}
		if d.Name == "" {
			warnf("agent drop-in %s: missing %q — skipping", path, "name")
			continue
		}
		if _, ok := d.Invocations[ModeSession]; !ok {
			warnf("agent drop-in %s (%q): no %q invocation — skipping", path, d.Name, ModeSession)
			continue
		}
		if _, taken := registry[d.Name]; taken {
			// Precedence, not policy: built-ins register first and always win; an
			// earlier drop-in wins over a later one. Skip (never the panicking
			// register()) because a drop-in is untrusted input.
			warnf("agent drop-in %s: provider %q is already registered (built-ins win) — skipping", path, d.Name)
			continue
		}

		// Capture per-iteration so each factory closes over its own descriptor and
		// origin. Origin is set by the LOADER (the drop-in file path), never read
		// from the JSON — a drop-in cannot claim built-in status. isBuiltin() is
		// false for this origin, so SupportsHeadless is false (Caps): a drop-in
		// gets interactive spawn + review, never an unattended headless run.
		desc := d
		origin := dropinOrigin(path)
		registry[d.Name] = func(r runner, warnf func(string, ...any)) (Agent, error) {
			return descriptorAgent{desc: desc, run: r, warnf: warnf, origin: origin}, nil
		}
	}
}
