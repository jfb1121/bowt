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
		// The cd shim must survive alongside the completion loader.
		for _, want := range []string{"bowt()", "command bowt path", "command bowt root", "builtin cd", `command bowt "$@"`} {
			if !strings.Contains(out, want) {
				t.Errorf("Init(%q) missing shim marker %q", sh, want)
			}
		}
		// …and the completion loader must be wired in for the right shell.
		if !strings.Contains(out, "command bowt completion "+sh) {
			t.Errorf("Init(%q) missing completion loader for %s", sh, sh)
		}
	}
}

func TestInitUnsupported(t *testing.T) {
	if _, err := Init("fish"); err == nil {
		t.Fatal("Init(fish) should error")
	}
}

// Execute the generated integration in a real bash with a stub `bowt` on PATH,
// and confirm the cd shim still changes directory AND the completion loader
// sources cleanly (the two must coexist). The stub answers both the path/root
// lookups the shim makes and the `completion bash` the loader sources.
func TestShimAndCompletionCoexist(t *testing.T) {
	binDir := t.TempDir()
	target := t.TempDir()

	// Fake binary: path/root echo the target dir; `completion bash` emits a
	// trivial-but-valid bash completion registration for the `bowt` word.
	fake := "#!/bin/sh\n" +
		"case \"$1\" in\n" +
		"  path|root) echo " + target + " ;;\n" +
		"  completion) echo 'complete -W \"\" bowt' ;;\n" +
		"esac\n"
	if err := os.WriteFile(filepath.Join(binDir, "bowt"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}

	integration, _ := Init("bash")
	// Source the full snippet (shim + completion loader), then exercise cd and
	// confirm the completion function is registered on the `bowt` word.
	script := integration + "\ncd /\nbowt cd feature\npwd\ncomplete -p bowt >/dev/null 2>&1 && echo COMPLETION_OK\n"

	cmd := exec.Command("bash", "-c", script)
	cmd.Env = append(os.Environ(), "PATH="+binDir+":"+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bash: %v\n%s", err, out)
	}
	got := strings.TrimSpace(string(out))
	// Compare basenames: macOS /var -> /private/var symlink makes full paths differ.
	lines := strings.Split(got, "\n")
	if len(lines) == 0 || filepath.Base(lines[0]) != filepath.Base(target) {
		t.Fatalf("cd landed at %q; want under %q\nfull output:\n%s", got, target, out)
	}
	if !strings.Contains(got, "COMPLETION_OK") {
		t.Fatalf("completion not registered on `bowt` word; output:\n%s", out)
	}
}
