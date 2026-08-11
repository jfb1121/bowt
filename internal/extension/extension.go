// Package extension implements bowt's per-repo extension mechanism: a repo adds
// a `bowt <cmd>` by dropping <configDir>/extensions/<cmd>.sh, without patching
// bowt. When <cmd> is not a built-in, bowt resolves and runs the script as a
// subprocess — inherited stdio, propagated exit code — with the per-worktree
// BOWT_*/GWT_* environment injected. This is the git-style "dispatch an unknown
// command to an external program" pattern.
//
// Contract difference from twig: twig's extensions were *sourced* bash
// functions (_twig_ext_<cmd>), because twig itself was a shell function that
// could source them into its own process. A compiled bowt binary cannot source
// a bash function, so bowt's contract is an INVOKABLE script — one that does its
// work directly when run, not one that defines a function. Migrating the
// existing twig _twig_ext_* extensions to this contract is out of scope here.
package extension

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// subdir is the directory under a repo's config dir that holds extensions.
const subdir = "extensions"

// KeyLib is the environment variable pointing an extension at the sourceable
// helper lib (see Lib). The script does: source "$BOWT_LIB".
const KeyLib = "BOWT_LIB"

// LockMode is how bowt locks the worktree around an extension run — declared by
// the script's `# bowt-lock:` manifest line and enforced by bowt, so the
// extension author writes no lock code. A typed value set (house style), not a
// bare string.
type LockMode string

const (
	// LockNone runs the extension without a bowt-held lock (the default).
	LockNone LockMode = "none"
	// LockShared takes a shared (read) lock: coexists with other readers, yields
	// to an exclusive writer.
	LockShared LockMode = "shared"
	// LockExclusive takes the exclusive per-worktree lock — for an extension that
	// mutates the tree.
	LockExclusive LockMode = "exclusive"
)

// Manifest is the metadata bowt reads from an extension's leading comment lines
// (`# bowt-<key>: <value>`). Unknown keys are ignored; a missing lock defaults
// to none.
type Manifest struct {
	Lock LockMode `json:"lock"`
	Desc string   `json:"desc"`
}

// Extension is one resolved per-repo extension.
type Extension struct {
	Cmd      string   `json:"cmd"`
	Path     string   `json:"path"`
	Manifest Manifest `json:"manifest"`
}

// Dir returns the extensions directory for a config dir, or "" when configDir
// is "" (no .bowt/.twig).
func Dir(configDir string) string {
	if configDir == "" {
		return ""
	}
	return filepath.Join(configDir, subdir)
}

// safeCmd reports whether cmd is a plain command name (no path separators, no
// "..") so it can only ever resolve to a file directly inside the extensions
// dir — never traverse out of it.
func safeCmd(cmd string) bool {
	if cmd == "" || strings.Contains(cmd, "..") {
		return false
	}
	return !strings.ContainsRune(cmd, '/') && !strings.ContainsRune(cmd, filepath.Separator)
}

// Find resolves <configDir>/extensions/<cmd>.sh. found is false (with a nil
// error) when there is no such extension — the caller then falls through to
// cobra's normal unknown-command error.
func Find(configDir, cmd string) (ext Extension, found bool, err error) {
	dir := Dir(configDir)
	if dir == "" || !safeCmd(cmd) {
		return Extension{}, false, nil
	}
	path := filepath.Join(dir, cmd+".sh")
	fi, statErr := os.Stat(path)
	if statErr != nil {
		if os.IsNotExist(statErr) {
			return Extension{}, false, nil
		}
		return Extension{}, false, statErr
	}
	if fi.IsDir() {
		return Extension{}, false, nil
	}
	m, err := manifestOf(path)
	if err != nil {
		return Extension{}, false, err
	}
	return Extension{Cmd: cmd, Path: path, Manifest: m}, true, nil
}

// List returns every extension in the repo's extensions dir, sorted by command
// name, each with its parsed manifest. A missing dir yields an empty list.
func List(configDir string) ([]Extension, error) {
	dir := Dir(configDir)
	if dir == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var exts []Extension
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sh") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		m, err := manifestOf(path)
		if err != nil {
			return nil, err
		}
		exts = append(exts, Extension{
			Cmd:      strings.TrimSuffix(e.Name(), ".sh"),
			Path:     path,
			Manifest: m,
		})
	}
	sort.Slice(exts, func(i, j int) bool { return exts[i].Cmd < exts[j].Cmd })
	return exts, nil
}

// Run executes the extension as `bash <path> <args...>` with cwd=worktree, the
// given streams, and env appended onto the current environment. BOWT_LIB is
// injected pointing at a materialized copy of the helper lib. It returns the
// child's exit code; err is non-nil only when the child could not be started.
//
// This uses os/exec directly rather than the run.Runner seam: the Runner
// captures stdout into a buffer and folds a non-zero exit into an error, which
// is incompatible with the extension contract (inherited stdio, propagated exit
// code). `bowt exec` and `bowt spawn` bypass the Runner for the same reason.
func Run(e Extension, worktree string, env, args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	lib, cleanup, err := materializeLib()
	if err != nil {
		return 1, err
	}
	defer cleanup()

	cmd := exec.Command("bash", append([]string{e.Path}, args...)...)
	cmd.Dir = worktree
	cmd.Env = append(append(os.Environ(), env...), KeyLib+"="+lib)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr

	if runErr := cmd.Run(); runErr != nil {
		var ee *exec.ExitError
		if errors.As(runErr, &ee) {
			// The extension ran and exited non-zero: propagate its code, not our own.
			return ee.ExitCode(), nil
		}
		return 1, fmt.Errorf("run extension %q: %w", e.Cmd, runErr)
	}
	return 0, nil
}

// manifestLine matches a manifest header line: `# bowt-<key>: <value>`.
var manifestLine = regexp.MustCompile(`^#\s*bowt-([a-z]+):\s*(.*)$`)

// manifestOf opens path and parses its header manifest.
func manifestOf(path string) (Manifest, error) {
	f, err := os.Open(path)
	if err != nil {
		return Manifest{}, err
	}
	defer func() { _ = f.Close() }()
	return parseManifest(f), nil
}

// parseManifest reads the leading comment block of a script, collecting
// `# bowt-<key>: <value>` lines. Blank lines and other comments (a shebang, a
// prose comment) are skipped; the first non-comment line ends the header. An
// absent or unrecognized lock value leaves Lock at its none default.
func parseManifest(r io.Reader) Manifest {
	m := Manifest{Lock: LockNone}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue // blank line inside the header
		}
		if !strings.HasPrefix(line, "#") {
			break // first line of actual code ends the manifest header
		}
		match := manifestLine.FindStringSubmatch(line)
		if match == nil {
			continue // shebang or a plain comment
		}
		key, val := match[1], strings.TrimSpace(match[2])
		switch key {
		case "lock":
			switch LockMode(val) {
			case LockNone, LockShared, LockExclusive:
				m.Lock = LockMode(val)
			}
		case "desc":
			m.Desc = val
		}
	}
	return m
}

// materializeLib writes Lib to a temp file and returns its path plus a cleanup
// func. The extension sources it via $BOWT_LIB. A temp file (not a fixed path)
// keeps concurrent bowt runs from racing on the same file.
func materializeLib() (path string, cleanup func(), err error) {
	f, err := os.CreateTemp("", "bowt-lib-*.sh")
	if err != nil {
		return "", func() {}, err
	}
	if _, err := f.WriteString(Lib); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "", func() {}, err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return "", func() {}, err
	}
	return f.Name(), func() { _ = os.Remove(f.Name()) }, nil
}
