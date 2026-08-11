// Package env defines bowt's environment contract — the variables injected
// into exec'd commands and lifecycle hooks. The canonical names are BOWT_*;
// the historical GWT_* names (plus TWIG_REPO_NAME and AUTOENV_ASSUME_YES) are
// exported alongside them so an existing twig .twig/config and its setup.sh /
// teardown.sh keep working unchanged.
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
// exec'd command. Order is: config-derived vars first, then the bowt contract,
// then the GWT_* back-compat aliases — so bowt's computed values win over any
// stale same-named value carried in from the config (env keeps the last
// occurrence of a duplicate key).
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
		// GWT_* back-compat aliases + twig extras.
		"GWT_PORT="+port,
		"GWT_OFFSET="+offset,
		"GWT_BRANCH="+i.Branch,
		"GWT_MAIN_REPO="+i.MainRepo,
		"GWT_CODE_ONLY="+codeOnly,
		"TWIG_REPO_NAME="+i.RepoName,
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
