// Package hook invokes a repo's lifecycle scripts — pre-setup.sh, setup.sh,
// teardown.sh — from the config directory, through the run.Runner seam. Each
// script is called with bowt's positional contract, "<path> <branch> <offset>
// <port>", and with bowt's environment contract injected. The scripts run via
// `env KEY=VALUE… bash <script> …` so the environment travels through the same
// Runner boundary (no per-Runner env field, no os.Setenv side effects).
package hook

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/jfb1121/bowt/internal/run"
)

// Kind is a lifecycle script filename. Typed so callers can't pass an arbitrary
// string as a hook name.
type Kind string

// The three lifecycle hooks, named exactly as the files in the config dir.
const (
	PreSetup Kind = "pre-setup.sh"
	Setup    Kind = "setup.sh"
	Teardown Kind = "teardown.sh"
)

// Args is the positional contract every hook receives.
type Args struct {
	Path   string
	Branch string
	Offset int
	Port   int
}

// Run executes <dir>/<kind> with cwd = a.Path, the positional args, and env
// injected. A missing script is not an error: ran is false and err is nil.
// The script's stdout is echoed to stderr (stdout stays reserved for bowt's
// JSON result); the runner streams the script's stderr live if configured.
func Run(r run.Runner, dir string, kind Kind, a Args, env []string) (ran bool, err error) {
	script := filepath.Join(dir, string(kind))
	if _, statErr := os.Stat(script); statErr != nil {
		return false, nil
	}

	argv := make([]string, 0, len(env)+5)
	argv = append(argv, env...)
	argv = append(argv, "bash", script, a.Path, a.Branch, strconv.Itoa(a.Offset), strconv.Itoa(a.Port))

	out, runErr := r.Run(a.Path, "env", argv...)
	if out != "" {
		fmt.Fprint(os.Stderr, out)
	}
	if runErr != nil {
		return true, fmt.Errorf("%s: %w", kind, runErr)
	}
	return true, nil
}
