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
	// headless runs bin+args as a child with NO interactive stdin, streaming
	// stdout/stderr from opts; blocks until it exits.
	headless(ctx context.Context, bin string, args []string, opts Opts) error
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

// headless launches bin as a CHILD with the same lock-preserving rationale as
// session (a child, not syscall.Exec, so the O_CLOEXEC flock fd the supervisor
// holds is NOT dropped — the lock lives for the agent's whole lifetime). It
// differs from session in two ways: stdin is NEVER wired to the terminal (a
// headless agent can't answer prompts — a nil opts.Stdin gives the child the
// null device), and stdout/stderr come straight from opts so the supervisor can
// capture them to the lane log.
func (osRunner) headless(ctx context.Context, bin string, args []string, opts Opts) error {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdin = opts.Stdin // nil ⇒ /dev/null; never os.Stdin, there is no TTY
	cmd.Stdout = writerOr(opts.Stdout, os.Stdout)
	cmd.Stderr = writerOr(opts.Stderr, os.Stderr)
	if err := cmd.Run(); err != nil {
		// Wrap with %w so an *exec.ExitError stays unwrappable — the supervisor
		// maps the child's exit code to a terminal lane status.
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
