package shell

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestInitContent(t *testing.T) {
	for _, sh := range []string{"bash", "zsh"} {
		out, err := Init(sh)
		if err != nil {
			t.Fatalf("Init(%q): %v", sh, err)
		}
		for _, want := range []string{"bowt()", "command bowt path", "command bowt root", "builtin cd", `command bowt "$@"`} {
			if !strings.Contains(out, want) {
				t.Errorf("Init(%q) missing %q", sh, want)
			}
		}
	}
}

func TestInitUnsupported(t *testing.T) {
	if _, err := Init("fish"); err == nil {
		t.Fatal("Init(fish) should error")
	}
}

// Execute the generated shim in a real bash with a stub `bowt` on PATH, and
// confirm `bowt cd` actually changes directory.
func TestShimCdExecutes(t *testing.T) {
	binDir := t.TempDir()
	target := t.TempDir()

	// Fake binary: `bowt path X` and `bowt root` both echo the target dir.
	fake := "#!/bin/sh\ncase \"$1\" in\n  path|root) echo " + target + " ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(binDir, "bowt"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}

	shim, _ := Init("bash")
	script := shim + "\ncd /\nbowt cd feature\npwd\n"

	cmd := exec.Command("bash", "-c", script)
	cmd.Env = append(os.Environ(), "PATH="+binDir+":"+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bash: %v\n%s", err, out)
	}
	// Compare basenames: macOS /var -> /private/var symlink makes full paths differ.
	if got := strings.TrimSpace(string(out)); filepath.Base(got) != filepath.Base(target) {
		t.Fatalf("cd landed at %q; want under %q", got, target)
	}
}
