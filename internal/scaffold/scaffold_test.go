package scaffold

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInitScaffoldsConfig(t *testing.T) {
	repo := t.TempDir()

	res, err := Init(repo)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}

	// name -> should be executable
	want := map[string]bool{
		".gitignore":  false,
		"config":      false,
		"setup.sh":    true,
		"teardown.sh": true,
		"AGENTS.md":   false,
	}
	for name, wantExec := range want {
		info, err := os.Stat(filepath.Join(repo, ".bowt", name))
		if err != nil {
			t.Errorf("missing %s: %v", name, err)
			continue
		}
		if gotExec := info.Mode().Perm()&0o100 != 0; gotExec != wantExec {
			t.Errorf("%s exec = %v, want %v (mode %v)", name, gotExec, wantExec, info.Mode().Perm())
		}
	}

	// The scaffolded .gitignore covers bowt's runtime artifacts so the rest of
	// .bowt/ can be committed cleanly.
	gi, err := os.ReadFile(filepath.Join(repo, ".bowt", ".gitignore"))
	if err != nil {
		t.Fatalf("read .bowt/.gitignore: %v", err)
	}
	if !strings.Contains(string(gi), "gate.json") {
		t.Errorf(".bowt/.gitignore missing gate.json: %q", gi)
	}

	if res.ConfigDir != filepath.Join(repo, ".bowt") {
		t.Errorf("ConfigDir = %q, want %q", res.ConfigDir, filepath.Join(repo, ".bowt"))
	}
}

// Init must NOT ignore .bowt/ globally — it's meant to be committed/shared.
func TestInitDoesNotTouchGitExclude(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".git", "info"), 0o755); err != nil {
		t.Fatal(err)
	}
	excl := filepath.Join(repo, ".git", "info", "exclude")
	if err := os.WriteFile(excl, []byte("# existing\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Init(repo); err != nil {
		t.Fatal(err)
	}

	data, _ := os.ReadFile(excl)
	if strings.Contains(string(data), ".bowt") {
		t.Errorf(".git/info/exclude should be untouched, got: %q", data)
	}
}

func TestInitRefusesToClobber(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".bowt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Init(repo); err == nil {
		t.Fatal("expected error when .bowt/ already exists")
	}
}
