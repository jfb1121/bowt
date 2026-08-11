package hook

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jfb1121/bowt/internal/run"
)

func TestRunMissingScriptIsNoOp(t *testing.T) {
	f := &run.Fake{}
	ran, err := Run(f, t.TempDir(), Setup, Args{}, nil)
	if ran || err != nil {
		t.Fatalf("missing script: ran=%v err=%v; want false,nil", ran, err)
	}
	if len(f.Calls) != 0 {
		t.Fatalf("missing script should not invoke the runner (calls=%d)", len(f.Calls))
	}
}

func TestRunInvokesEnvBashWithArgs(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, string(Setup)), []byte("#!/usr/bin/env bash\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	f := &run.Fake{}
	a := Args{Path: "/wt", Branch: "feature/x", Offset: 3, Port: 8003}
	ran, err := Run(f, dir, Setup, a, []string{"BOWT_PORT=8003", "GWT_PORT=8003"})
	if !ran || err != nil {
		t.Fatalf("ran=%v err=%v; want true,nil", ran, err)
	}
	if len(f.Calls) != 1 {
		t.Fatalf("calls = %d; want 1", len(f.Calls))
	}
	c := f.Calls[0]
	if c.Dir != "/wt" {
		t.Errorf("cwd = %q; want the worktree path /wt", c.Dir)
	}
	if c.Name != "env" {
		t.Errorf("name = %q; want env", c.Name)
	}
	joined := strings.Join(c.Args, " ")
	wantSuffix := "bash " + filepath.Join(dir, string(Setup)) + " /wt feature/x 3 8003"
	if !strings.HasSuffix(joined, wantSuffix) {
		t.Errorf("args = %q; want suffix %q", joined, wantSuffix)
	}
	if !strings.HasPrefix(joined, "BOWT_PORT=8003 GWT_PORT=8003 ") {
		t.Errorf("env not passed before the command: %q", joined)
	}
}

func TestRunWrapsScriptError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, string(PreSetup)), []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("exit status 3")
	f := &run.Fake{Func: func(_, _ string, _ []string) (string, error) { return "", sentinel }}
	ran, err := Run(f, dir, PreSetup, Args{}, nil)
	if !ran || err == nil {
		t.Fatalf("ran=%v err=%v; want true and an error", ran, err)
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("error not wrapped with %%w: %v", err)
	}
	if !strings.Contains(err.Error(), string(PreSetup)) {
		t.Errorf("error should name the hook: %v", err)
	}
}
