package env

import (
	"strings"
	"testing"
)

func toMap(kv []string) map[string]string {
	m := map[string]string{}
	for _, e := range kv {
		if i := strings.IndexByte(e, '='); i >= 0 {
			m[e[:i]] = e[i+1:] // last occurrence wins, matching env(1)
		}
	}
	return m
}

func TestBuildContract(t *testing.T) {
	kv := Build(Info{
		Path:     "/wt",
		Branch:   "feature/x",
		Offset:   2,
		Port:     8002,
		MainRepo: "/main",
		RepoName: "myrepo",
	}, map[string]string{"FOO": "bar"})

	m := toMap(kv)
	wants := map[string]string{
		"BOWT_PORT":          "8002",
		"BOWT_OFFSET":        "2",
		"BOWT_BRANCH":        "feature/x",
		"BOWT_MAIN_REPO":     "/main",
		"BOWT_REPO_NAME":     "myrepo",
		"BOWT_CODE_ONLY":     "0",
		"AUTOENV_ASSUME_YES": "1",
		"FOO":                "bar",
	}
	for k, want := range wants {
		if m[k] != want {
			t.Errorf("%s = %q; want %q", k, m[k], want)
		}
	}
}

func TestBuildCodeOnly(t *testing.T) {
	m := toMap(Build(Info{CodeOnly: true}, nil))
	if m["BOWT_CODE_ONLY"] != "1" {
		t.Fatalf("code-only not set: BOWT_CODE_ONLY=%q", m["BOWT_CODE_ONLY"])
	}
}

// Computed bowt values must come after config vars so they win a name clash.
func TestBuildComputedWinsOverConfig(t *testing.T) {
	kv := Build(Info{Port: 8005}, map[string]string{"BOWT_PORT": "1"})
	// env(1) keeps the last occurrence; assert order, not just presence.
	lastPort := -1
	lastStale := -1
	for i, e := range kv {
		if e == "BOWT_PORT=8005" {
			lastPort = i
		}
		if e == "BOWT_PORT=1" {
			lastStale = i
		}
	}
	if lastPort < 0 || lastStale < 0 || lastPort < lastStale {
		t.Fatalf("computed BOWT_PORT (i=%d) must appear after stale config value (i=%d)", lastPort, lastStale)
	}
}
