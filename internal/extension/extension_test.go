package extension

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// parseManifest reads the header block: lock + desc, defaulting lock to none,
// skipping the shebang/blank/prose lines, stopping at the first code line, and
// ignoring unknown keys and bad lock values.
func TestParseManifest(t *testing.T) {
	tests := []struct {
		name     string
		script   string
		wantLock LockMode
		wantDesc string
	}{
		{
			name:     "full header after shebang and blank",
			script:   "#!/usr/bin/env bash\n\n# bowt-lock: exclusive\n# bowt-desc: build the thing\necho hi\n",
			wantLock: LockExclusive,
			wantDesc: "build the thing",
		},
		{
			name:     "no manifest defaults to none",
			script:   "#!/usr/bin/env bash\necho hi\n",
			wantLock: LockNone,
			wantDesc: "",
		},
		{
			name:     "shared lock and unknown key ignored",
			script:   "# bowt-lock: shared\n# bowt-scope: repo\n# bowt-desc: read only\ntrue\n",
			wantLock: LockShared,
			wantDesc: "read only",
		},
		{
			name:     "bad lock value falls back to none",
			script:   "# bowt-lock: bogus\n# bowt-desc: x\n",
			wantLock: LockNone,
			wantDesc: "x",
		},
		{
			name:     "manifest after code is not read",
			script:   "echo hi\n# bowt-lock: exclusive\n",
			wantLock: LockNone,
			wantDesc: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := parseManifest(strings.NewReader(tt.script))
			if m.Lock != tt.wantLock {
				t.Errorf("Lock = %q; want %q", m.Lock, tt.wantLock)
			}
			if m.Desc != tt.wantDesc {
				t.Errorf("Desc = %q; want %q", m.Desc, tt.wantDesc)
			}
		})
	}
}

// writeExt drops a script at <configDir>/extensions/<cmd>.sh and returns the
// config dir.
func writeExt(t *testing.T, cmd, body string) string {
	t.Helper()
	cfg := t.TempDir()
	dir := filepath.Join(cfg, "extensions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, cmd+".sh"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// Find resolves a present extension (with its manifest), reports not-found for a
// missing one, and refuses a traversing command name.
func TestFind(t *testing.T) {
	cfg := writeExt(t, "greet", "# bowt-lock: exclusive\n# bowt-desc: say hi\necho hi\n")

	ext, found, err := Find(cfg, "greet")
	if err != nil || !found {
		t.Fatalf("Find(greet) = found %v, err %v; want found", found, err)
	}
	if ext.Manifest.Lock != LockExclusive || ext.Manifest.Desc != "say hi" {
		t.Errorf("manifest = %+v; want exclusive/\"say hi\"", ext.Manifest)
	}
	if filepath.Base(ext.Path) != "greet.sh" {
		t.Errorf("path = %q; want .../greet.sh", ext.Path)
	}

	if _, found, _ := Find(cfg, "absent"); found {
		t.Error("Find(absent) should not be found")
	}
	if _, found, _ := Find(cfg, "../../etc/passwd"); found {
		t.Error("Find must reject a traversing command name")
	}
	if _, found, _ := Find("", "greet"); found {
		t.Error("Find with empty config dir should not be found")
	}
}

// List returns every *.sh sorted by command, each with its manifest.
func TestList(t *testing.T) {
	cfg := writeExt(t, "zeta", "# bowt-desc: last\n")
	dir := filepath.Join(cfg, "extensions")
	if err := os.WriteFile(filepath.Join(dir, "alpha.sh"), []byte("# bowt-lock: shared\n# bowt-desc: first\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A non-.sh file is ignored.
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}

	exts, err := List(cfg)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(exts) != 2 {
		t.Fatalf("List returned %d; want 2 (%+v)", len(exts), exts)
	}
	if exts[0].Cmd != "alpha" || exts[1].Cmd != "zeta" {
		t.Errorf("not sorted by cmd: %q, %q", exts[0].Cmd, exts[1].Cmd)
	}
	if exts[0].Manifest.Lock != LockShared || exts[0].Manifest.Desc != "first" {
		t.Errorf("alpha manifest = %+v", exts[0].Manifest)
	}
}

// Run invokes the script with cwd=worktree, the injected env + args visible,
// $BOWT_LIB sourceable (bowt_log reaches stderr), and the exit code propagated.
func TestRunSeesEnvArgsCwdExitAndLib(t *testing.T) {
	body := `#!/usr/bin/env bash
source "$BOWT_LIB"
echo "port=$BOWT_PORT branch=$GWT_BRANCH"
echo "args=$*"
echo "cwd=$PWD"
bowt_log "hello-from-lib"
exit 7
`
	cfg := writeExt(t, "probe", body)
	ext, found, err := Find(cfg, "probe")
	if err != nil || !found {
		t.Fatalf("Find: found %v err %v", found, err)
	}

	wt := t.TempDir()
	// t.TempDir may be a /var symlink to /private/var on macOS; $PWD resolves the
	// real path, so compare against the resolved worktree.
	realWT, err := filepath.EvalSymlinks(wt)
	if err != nil {
		t.Fatal(err)
	}
	envKV := []string{"BOWT_PORT=8123", "GWT_BRANCH=feature/x"}

	var stdout, stderr bytes.Buffer
	code, err := Run(ext, wt, envKV, []string{"a", "b c"}, nil, &stdout, &stderr)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if code != 7 {
		t.Errorf("exit code = %d; want 7 (propagated)", code)
	}
	out := stdout.String()
	for _, want := range []string{"port=8123 branch=feature/x", "args=a b c", "cwd=" + realWT} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q; got:\n%s", want, out)
		}
	}
	if !strings.Contains(stderr.String(), "bowt: hello-from-lib") {
		t.Errorf("stderr missing bowt_log output; got:\n%s", stderr.String())
	}
}
