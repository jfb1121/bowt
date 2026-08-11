package agent

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// runner is the process-execution seam. It has one real implementation
// (osRunner) and a test fake, so provider logic — argument construction, knob
// degradation, capability errors — is tested without launching a real CLI.
type runner interface {
	// session runs bin+args as a child with streaming stdio from opts,
	// blocking until it exits.
	session(ctx context.Context, bin string, args []string, opts Opts) error
	// oneshot runs bin+args feeding stdin, returning captured stdout.
	oneshot(ctx context.Context, bin string, args []string, stdin string) (string, error)
}

// osRunner is the real runner, backed by os/exec.
type osRunner struct{}

// session launches bin as a CHILD (not syscall.Exec) with inherited stdio.
// Running it as a child is deliberate: Go opens the flock fd O_CLOEXEC, so an
// exec-replace would drop the per-worktree lock spawn holds; as a child the
// lock is held for the agent's whole lifetime and released cleanly on return.
func (osRunner) session(ctx context.Context, bin string, args []string, opts Opts) error {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdin = readerOr(opts.Stdin, os.Stdin)
	cmd.Stdout = writerOr(opts.Stdout, os.Stdout)
	cmd.Stderr = writerOr(opts.Stderr, os.Stderr)
	if err := cmd.Run(); err != nil {
		// Wrap with %w so an *exec.ExitError stays unwrappable by the caller
		// (spawn mirrors the child's exit code); the message matches the prior
		// hand-rolled path ("run claude: …") for non-exit failures.
		return fmt.Errorf("run %s: %w", bin, err)
	}
	return nil
}

// oneshot feeds stdin and captures stdout; stderr is folded into the error on
// failure so the caller sees why. No side effects on the terminal.
func (osRunner) oneshot(ctx context.Context, bin string, args []string, stdin string) (string, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdin = strings.NewReader(stdin)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		if errBuf.Len() > 0 {
			return out.String(), fmt.Errorf("run %s: %s: %w", bin, strings.TrimSpace(errBuf.String()), err)
		}
		return out.String(), fmt.Errorf("run %s: %w", bin, err)
	}
	return out.String(), nil
}

func readerOr(r io.Reader, def io.Reader) io.Reader {
	if r != nil {
		return r
	}
	return def
}

func writerOr(w io.Writer, def io.Writer) io.Writer {
	if w != nil {
		return w
	}
	return def
}
