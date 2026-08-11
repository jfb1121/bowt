// Package lock provides an exclusive per-worktree advisory lock using flock(2).
//
// The kernel releases an flock automatically when the holding process exits —
// INCLUDING on SIGKILL — so there is no stale-lock recovery to write. That is
// the whole reason a compiled bowt can lock cleanly where the bash version
// could not (macOS ships flock(2) the syscall but not flock(1) the CLI).
package lock

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Lock is a held flock. Call Release, or just let the process die.
type Lock struct {
	f *os.File
}

// Acquire takes an exclusive, NON-BLOCKING lock keyed on key (a branch or a
// worktree path). If another process holds it, Acquire fails immediately rather
// than waiting — the "fail fast and say it's busy" contract.
func Acquire(key string) (*Lock, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(home, ".bowt", "locks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	name := filepath.Join(dir, sanitize(key)+".lock")

	f, err := os.OpenFile(name, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("busy: %q is locked by another bowt process", key)
	}
	return &Lock{f: f}, nil
}

// Release unlocks and closes the lockfile. Safe to call on a nil Lock.
func (l *Lock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	return l.f.Close()
}

// sanitize turns a key into a flat, filesystem-safe lockfile name.
func sanitize(s string) string {
	return strings.ReplaceAll(strings.TrimPrefix(s, "/"), "/", "-")
}
