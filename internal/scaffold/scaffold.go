// Package scaffold creates a repo's .bowt/ config directory from embedded
// templates. It is the mechanism behind `bowt init`.
package scaffold

import (
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

//go:embed templates/generic/config templates/generic/setup.sh templates/generic/teardown.sh templates/generic/AGENTS.md
var generic embed.FS

// Result reports what Init created, for agent-facing JSON.
type Result struct {
	ConfigDir string   `json:"config_dir"`
	Files     []string `json:"files"`
	Excluded  bool     `json:"git_excluded"`
}

// genericFiles are the template files copied into .bowt/, in write order.
// The .sh files are written executable. AGENTS.md is an agent-facing guide for
// wiring up this repo's hooks (so an agent scaffolds the stack, not us).
var genericFiles = []string{"config", "setup.sh", "teardown.sh", "AGENTS.md"}

// Init scaffolds <mainRepo>/.bowt with a generic config and setup/teardown
// hooks, and adds ".bowt/" to the repo's .git/info/exclude. It errors if
// .bowt/ already exists rather than clobbering an existing config.
func Init(mainRepo string) (Result, error) {
	cfgDir := filepath.Join(mainRepo, ".bowt")
	if _, err := os.Stat(cfgDir); err == nil {
		return Result{}, fmt.Errorf(".bowt/ already exists in %s — remove it to reinitialize", mainRepo)
	} else if !os.IsNotExist(err) {
		return Result{}, err
	}

	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		return Result{}, err
	}

	written := make([]string, 0, len(genericFiles))
	for _, name := range genericFiles {
		data, err := generic.ReadFile("templates/generic/" + name)
		if err != nil {
			return Result{}, err
		}
		mode := os.FileMode(0o644)
		if strings.HasSuffix(name, ".sh") {
			mode = 0o755
		}
		if err := os.WriteFile(filepath.Join(cfgDir, name), data, mode); err != nil {
			return Result{}, err
		}
		written = append(written, filepath.Join(".bowt", name))
	}

	excluded, err := ensureExcluded(mainRepo, ".bowt/")
	if err != nil {
		return Result{}, err
	}

	return Result{ConfigDir: cfgDir, Files: written, Excluded: excluded}, nil
}

// ensureExcluded appends pattern to <mainRepo>/.git/info/exclude if not already
// present. A missing exclude file (e.g. running inside a linked worktree) is not
// an error — it just means nothing was excluded.
func ensureExcluded(mainRepo, pattern string) (bool, error) {
	excl := filepath.Join(mainRepo, ".git", "info", "exclude")
	data, err := os.ReadFile(excl)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == pattern {
			return false, nil
		}
	}

	f, err := os.OpenFile(excl, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()

	// Guard against a file that lacks a trailing newline.
	prefix := ""
	if len(data) > 0 && !strings.HasSuffix(string(data), "\n") {
		prefix = "\n"
	}
	if _, err := f.WriteString(prefix + pattern + "\n"); err != nil {
		return false, err
	}
	return true, nil
}
