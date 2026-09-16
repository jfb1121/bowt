package scaffold

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func withExclude(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".git", "info"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".git", "info", "exclude"), []byte("# git ls-files --others exclude\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return repo
}

func TestInitScaffoldsConfig(t *testing.T) {
	repo := withExclude(t)

	res, err := Init(repo)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}

	for _, f := range genericFiles {
		info, err := os.Stat(filepath.Join(repo, ".bowt", f))
		if err != nil {
			t.Errorf("missing %s: %v", f, err)
			continue
		}
		if strings.HasSuffix(f, ".sh") && info.Mode().Perm()&0o100 == 0 {
			t.Errorf("%s should be executable, got mode %v", f, info.Mode().Perm())
		}
	}

	if !res.Excluded {
		t.Error("expected .bowt/ to be added to .git/info/exclude")
	}
	excl, _ := os.ReadFile(filepath.Join(repo, ".git", "info", "exclude"))
	if !strings.Contains(string(excl), ".bowt/") {
		t.Errorf("exclude missing .bowt/: %q", excl)
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

func TestEnsureExcludedIdempotent(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".git", "info"), 0o755); err != nil {
		t.Fatal(err)
	}
	excl := filepath.Join(repo, ".git", "info", "exclude")
	if err := os.WriteFile(excl, []byte(".bowt/\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := Init(repo)
	if err != nil {
		t.Fatal(err)
	}
	if res.Excluded {
		t.Error("expected Excluded=false when .bowt/ already present")
	}
	data, _ := os.ReadFile(excl)
	if got := strings.Count(string(data), ".bowt/"); got != 1 {
		t.Errorf("expected exactly one .bowt/ line, got %d: %q", got, data)
	}
}
