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

//go:embed templates/generic/config templates/generic/setup.sh templates/generic/teardown.sh templates/generic/AGENTS.md templates/generic/gitignore
var generic embed.FS

// Result reports what Init created, for agent-facing JSON.
type Result struct {
	ConfigDir string   `json:"config_dir"`
	Files     []string `json:"files"`
}

// genericFile maps an embedded template to its destination name in .bowt/. The
// .gitignore is stored embedded without the leading dot (go:embed skips
// dotfiles) and written with it.
type genericFile struct{ src, dest string }

var genericFiles = []genericFile{
	{"config", "config"},
	{"setup.sh", "setup.sh"},
	{"teardown.sh", "teardown.sh"},
	{"AGENTS.md", "AGENTS.md"},
	{"gitignore", ".gitignore"},
}

// Init scaffolds <mainRepo>/.bowt with a generic config, setup/teardown hooks,
// an agent guide, and a .gitignore for bowt's runtime artifacts. The directory
// is meant to be committed so a team shares one worktree setup; only bowt's
// generated artifacts are ignored, via the scaffolded .bowt/.gitignore.
//
// Init errors if .bowt/ already exists rather than clobbering an existing config.
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
	for _, f := range genericFiles {
		data, err := generic.ReadFile("templates/generic/" + f.src)
		if err != nil {
			return Result{}, err
		}
		mode := os.FileMode(0o644)
		if strings.HasSuffix(f.dest, ".sh") {
			mode = 0o755
		}
		if err := os.WriteFile(filepath.Join(cfgDir, f.dest), data, mode); err != nil {
			return Result{}, err
		}
		written = append(written, filepath.Join(".bowt", f.dest))
	}

	return Result{ConfigDir: cfgDir, Files: written}, nil
}
