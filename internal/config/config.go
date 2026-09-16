// Package config resolves a repo's config directory and loads its bash config
// file. The config is a bash script we source, so a repo's .bowt/config (and
// its exported vars) is applied as-is. We source it in a subshell through the
// run.Runner seam and diff the resulting environment against a clean bash
// baseline to recover exactly the variables the config defined.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/jfb1121/bowt/internal/run"
)

// DefaultPortBase is the first port when the config sets no *_PORT_BASE.
const DefaultPortBase = 8000

// dirBowt is bowt's config directory name.
const dirBowt = ".bowt"

// configFile is the bash config sourced inside the config dir.
const configFile = "config"

// Vars are the variables a config file defined, keyed by name. A map (not a
// struct) is correct here: the keys are open-ended shell variable names, not a
// fixed agent-facing JSON shape.
type Vars map[string]string

// Dir returns the config directory inside mainRepo: .bowt if present, else ""
// when it does not exist.
func Dir(mainRepo string) string {
	p := filepath.Join(mainRepo, dirBowt)
	if fi, err := os.Stat(p); err == nil && fi.IsDir() {
		return p
	}
	return ""
}

// Load sources <dir>/config through r and returns the variables it defined.
// A missing dir or missing config file yields empty (non-nil) Vars, not an
// error — a repo without config is normal.
func Load(r run.Runner, dir string) (Vars, error) {
	vars := Vars{}
	if dir == "" {
		return vars, nil
	}
	cfg := filepath.Join(dir, configFile)
	if _, err := os.Stat(cfg); err != nil {
		return vars, nil
	}

	// Symmetric runs: baseline sources nothing, loaded sources the config.
	// Everything shell-injected is identical between the two and cancels in
	// the diff; only the config's own variables remain.
	baseline, err := r.Run(dir, "bash", "-c", "set -a; env")
	if err != nil {
		return nil, fmt.Errorf("config baseline: %w", err)
	}
	loaded, err := r.Run(dir, "bash", "-c", `set -a; . "$1"; env`, "bowt-config", cfg)
	if err != nil {
		return nil, fmt.Errorf("source %s: %w", cfg, err)
	}

	base := parseEnv(baseline)
	for k, v := range parseEnv(loaded) {
		if internalVar(k) {
			continue
		}
		if bv, ok := base[k]; !ok || bv != v {
			vars[k] = v
		}
	}
	return vars, nil
}

// PortBase reads BOWT_PORT_BASE from the config, falling back to
// DefaultPortBase.
func (v Vars) PortBase() int {
	if s, ok := v["BOWT_PORT_BASE"]; ok {
		if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil && n > 0 {
			return n
		}
	}
	return DefaultPortBase
}

// envAssign matches the start of a "NAME=" line in `env` output.
var envAssign = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

// parseEnv turns `env` output into a map. A value spanning multiple lines
// (embedded newline) is stitched back onto the variable that opened it.
func parseEnv(s string) map[string]string {
	m := map[string]string{}
	cur := ""
	// Drop the single trailing newline that terminates `env` output, so it is
	// not mistaken for a blank continuation line of the last variable.
	for _, line := range strings.Split(strings.TrimSuffix(s, "\n"), "\n") {
		if pre := envAssign.FindString(line); pre != "" {
			cur = strings.TrimSuffix(pre, "=")
			m[cur] = line[len(pre):]
		} else if cur != "" {
			m[cur] += "\n" + line
		}
	}
	return m
}

// internalVar reports whether a name is shell bookkeeping that differs between
// the two subshells for reasons unrelated to the config (so it must not leak
// into the diff), rather than something the config set.
func internalVar(name string) bool {
	switch name {
	case "_", "SHLVL", "PWD", "OLDPWD", "SHELL", "SHELLOPTS", "BASH_EXECUTION_STRING":
		return true
	default:
		return false
	}
}
