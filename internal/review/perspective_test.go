package review

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDiscoverAndSelect(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Two real perspectives (carry `layers:` front-matter).
	write("arch.md", "---\nname: arch\nlayers: [service, tests]\n---\n# Arch\n")
	write("security.md", "---\nlayers: [tenant-api]\n---\n# Security\n")
	// A doc that must NOT be treated as a perspective (no layers front-matter).
	write("CONTRACT.md", "# Output contract\nthis is documentation\n")
	write("README.md", "# Readme\n")

	all, err := Discover(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("discovered %d perspectives; want 2 (%v)", len(all), slugsOf(all))
	}
	if slugsOf(all)[0] != "arch" || slugsOf(all)[1] != "security" {
		t.Fatalf("expected sorted [arch security], got %v", slugsOf(all))
	}

	// --all (no explicit list) selects everything.
	sel, err := Select(all, nil)
	if err != nil || len(sel) != 2 {
		t.Fatalf("Select(all)=%v err=%v; want 2", slugsOf(sel), err)
	}

	// Explicit list selects and de-dups; an unknown slug is a hard error.
	sel, err = Select(all, []string{"security", "security"})
	if err != nil || len(sel) != 1 || sel[0].Slug != "security" {
		t.Fatalf("Select(explicit)=%v err=%v; want [security]", slugsOf(sel), err)
	}
	if _, err := Select(all, []string{"nope"}); err == nil {
		t.Fatalf("expected error selecting unknown slug")
	}
}

func TestDiscoverMissingDir(t *testing.T) {
	if _, err := Discover(filepath.Join(t.TempDir(), "does-not-exist")); err == nil {
		t.Fatalf("expected error for missing perspectives dir")
	}
}

func slugsOf(ps []Perspective) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.Slug
	}
	return out
}
