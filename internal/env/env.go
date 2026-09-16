// Package env defines bowt's environment contract — the BOWT_* variables
// injected into exec'd commands and lifecycle hooks, plus AUTOENV_ASSUME_YES
// so a repo's autoenv setup runs non-interactively.
package env

import (
	"sort"
	"strconv"
)

// Canonical bowt environment keys.
const (
	KeyPort     = "BOWT_PORT"
	KeyOffset   = "BOWT_OFFSET"
	KeyBranch   = "BOWT_BRANCH"
	KeyMainRepo = "BOWT_MAIN_REPO"
	KeyRepoName = "BOWT_REPO_NAME"
	KeyCodeOnly = "BOWT_CODE_ONLY"
)

// Info is the per-worktree data the contract is built from.
type Info struct {
	Path     string
	Branch   string
	Offset   int
	Port     int
	MainRepo string
	RepoName string
	CodeOnly bool
}

// Build returns the KEY=VALUE entries to append onto os.Environ() for a hook or
// exec'd command. Order is: config-derived vars first, then the bowt contract —
// so bowt's computed values win over any stale same-named value carried in from
// the config (env keeps the last occurrence of a duplicate key).
func Build(i Info, cfg map[string]string) []string {
	codeOnly := "0"
	if i.CodeOnly {
		codeOnly = "1"
	}
	port := strconv.Itoa(i.Port)
	offset := strconv.Itoa(i.Offset)

	var out []string
	for _, k := range sortedKeys(cfg) {
		out = append(out, k+"="+cfg[k])
	}
	out = append(out,
		KeyPort+"="+port,
		KeyOffset+"="+offset,
		KeyBranch+"="+i.Branch,
		KeyMainRepo+"="+i.MainRepo,
		KeyRepoName+"="+i.RepoName,
		KeyCodeOnly+"="+codeOnly,
		"AUTOENV_ASSUME_YES=1",
	)
	return out
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
