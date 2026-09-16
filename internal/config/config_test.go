package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jfb1121/bowt/internal/run"
)

func TestDirResolvesBowt(t *testing.T) {
	root := t.TempDir()
	if got := Dir(root); got != "" {
		t.Fatalf("Dir with no config dir = %q; want \"\"", got)
	}

	bowt := filepath.Join(root, ".bowt")
	if err := os.MkdirAll(bowt, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := Dir(root); got != bowt {
		t.Fatalf("Dir = %q; want .bowt %q", got, bowt)
	}
}

func TestLoadReturnsEmptyWhenNoConfig(t *testing.T) {
	dir := t.TempDir()
	vars, err := Load(run.Exec{}, dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(vars) != 0 {
		t.Fatalf("vars = %v; want empty", vars)
	}
	if got := vars.PortBase(); got != DefaultPortBase {
		t.Fatalf("PortBase = %d; want %d", got, DefaultPortBase)
	}
}

// Real bash: a config file that sets vars is sourced and diffed cleanly.
func TestLoadSourcesConfig(t *testing.T) {
	dir := t.TempDir()
	cfg := "BOWT_PORT_BASE=9000\nFOO=bar\nexport BAZ=\"a b\"\n"
	if err := os.WriteFile(filepath.Join(dir, "config"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	vars, err := Load(run.Exec{}, dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if vars["BOWT_PORT_BASE"] != "9000" {
		t.Errorf("BOWT_PORT_BASE = %q; want 9000", vars["BOWT_PORT_BASE"])
	}
	if vars["FOO"] != "bar" {
		t.Errorf("FOO = %q; want bar", vars["FOO"])
	}
	if vars["BAZ"] != "a b" {
		t.Errorf("BAZ = %q; want 'a b'", vars["BAZ"])
	}
	// Shell bookkeeping must not leak in.
	for _, k := range []string{"_", "SHLVL", "PWD", "BASH_EXECUTION_STRING"} {
		if _, ok := vars[k]; ok {
			t.Errorf("internal var %q leaked into config vars", k)
		}
	}
	if got := vars.PortBase(); got != 9000 {
		t.Fatalf("PortBase = %d; want 9000", got)
	}
}

func TestPortBaseReadsBowtKey(t *testing.T) {
	v := Vars{"BOWT_PORT_BASE": "7000"}
	if got := v.PortBase(); got != 7000 {
		t.Fatalf("PortBase = %d; want 7000", got)
	}
	if got := (Vars{"BOWT_PORT_BASE": "bogus"}).PortBase(); got != DefaultPortBase {
		t.Fatalf("PortBase with bad value = %d; want default", got)
	}
}

// The diff logic in isolation, driven by a Fake so no real bash is needed.
func TestLoadDiffLogicWithFake(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := &run.Fake{Func: func(_, _ string, args []string) (string, error) {
		// The loaded run is the one that sources ($1 present via ". \"$1\"").
		sourced := false
		for _, a := range args {
			if a == "bowt-config" {
				sourced = true
			}
		}
		if sourced {
			return "PATH=/bin\nSHLVL=1\nNEWVAR=hi\nMULTI=line1\nline2\n", nil
		}
		return "PATH=/bin\nSHLVL=1\n", nil
	}}
	vars, err := Load(f, dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if vars["NEWVAR"] != "hi" {
		t.Errorf("NEWVAR = %q; want hi", vars["NEWVAR"])
	}
	if vars["MULTI"] != "line1\nline2" {
		t.Errorf("MULTI = %q; want multi-line stitched", vars["MULTI"])
	}
	if _, ok := vars["PATH"]; ok {
		t.Error("PATH unchanged between runs; should not be a config var")
	}
}
