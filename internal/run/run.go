// Package run is the process-execution seam. Config sourcing and lifecycle
// hooks shell out through a Runner rather than calling os/exec directly, so
// tests can substitute a Fake and future slices (gate, spawn) reuse the same
// boundary. The interface is intentionally tiny — one method — per the
// "small interfaces at the consumer" rule; it earns its keep because it has
// both a real implementation (Exec) and a test fake (Fake).
package run

import (
	"bytes"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// Runner executes name+args in dir and returns the child's captured stdout.
// dir == "" means the current working directory.
type Runner interface {
	Run(dir, name string, args ...string) (stdout string, err error)
}

// Exec is the real Runner, backed by os/exec.
//
// stdout is always captured and returned. stderr streams to Stderr when set
// (so a slow setup.sh shows progress); when Stderr is nil it is captured and,
// on failure, folded into the returned error so the caller sees why it failed.
type Exec struct {
	Stderr io.Writer
}

// Run implements Runner.
func (e Exec) Run(dir, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	var errBuf bytes.Buffer
	if e.Stderr != nil {
		cmd.Stderr = e.Stderr
	} else {
		cmd.Stderr = &errBuf
	}
	if err := cmd.Run(); err != nil {
		if e.Stderr == nil && errBuf.Len() > 0 {
			return out.String(), fmt.Errorf("%s: %s: %w", name, strings.TrimSpace(errBuf.String()), err)
		}
		return out.String(), fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return out.String(), nil
}
