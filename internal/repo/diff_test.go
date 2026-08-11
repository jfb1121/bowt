package repo

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestReviewScope(t *testing.T) {
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		full := append([]string{"-C", dir}, args...)
		if out, err := exec.Command("git", full...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(name, content string) {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	git("init", "-q")
	git("config", "user.email", "t@bowt.dev")
	git("config", "user.name", "bowt test")
	write("README.md", "base\n")
	git("add", "-A")
	git("commit", "-q", "-m", "base")
	base, err := run(dir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}

	// A committed source file (adds a symbol), plus a committed binary.
	write("pkg/new.go", "package pkg\n\nfunc Foo() {}\n\ntype Bar struct{}\n")
	write("assets/logo.png", "\x89PNG\x00\x00\x01\x02binary\x00data")
	git("add", "-A")
	git("commit", "-q", "-m", "work")

	// An untracked source file — invisible to the diff, must be surfaced.
	write("pkg/untracked.go", "package pkg\n")

	facts, err := ReviewScope(dir, base)
	if err != nil {
		t.Fatalf("ReviewScope: %v", err)
	}

	if facts.MergeBase == "" || facts.DiffCmd != "git diff "+facts.MergeBase {
		t.Fatalf("bad merge-base/diffcmd: %+v", facts)
	}
	if !contains(facts.ChangedFiles, "pkg/new.go") || !contains(facts.ChangedFiles, "assets/logo.png") {
		t.Fatalf("changed files missing entries: %v", facts.ChangedFiles)
	}
	if facts.NewFiles < 2 {
		t.Fatalf("NewFiles = %d; want >= 2", facts.NewFiles)
	}
	if !contains(facts.BinaryAdded, "assets/logo.png") {
		t.Fatalf("binary not detected: %v", facts.BinaryAdded)
	}
	if !contains(facts.AddedSymbols, "Foo") || !contains(facts.AddedSymbols, "Bar") {
		t.Fatalf("added symbols missing: %v", facts.AddedSymbols)
	}
	if !contains(facts.UntrackedSource, "pkg/untracked.go") {
		t.Fatalf("untracked source not surfaced: %v", facts.UntrackedSource)
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if strings.TrimSpace(s) == want {
			return true
		}
	}
	return false
}
