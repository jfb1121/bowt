// Package lock provides an exclusive per-worktree advisory lock using flock(2).
//
// The kernel releases an flock automatically when the holding process exits —
// INCLUDING on SIGKILL — so there is no stale-lock recovery to write. That is
// the whole reason a compiled bowt can lock cleanly where the bash version
// could not (macOS ships flock(2) the syscall but not flock(1) the CLI).
package lock

import (
	"errors"
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

// EnvHeld names the environment variable a lock holder exports so its own
// descendants can tell that the lock is already held on their behalf. Its value
// is the lock key (a worktree path), so the pass is scoped to that one key.
const EnvHeld = "BOWT_LOCK_HELD"

// HeldByAncestor reports whether an ancestor of this process already holds the
// lock for key and exported EnvHeld to say so.
//
// This exists because flock is owned by an open file description, not by a
// process tree: a child can never take a lock its parent holds, and a
// non-blocking acquire fails immediately rather than waiting. A spawn
// supervisor holds the worktree lock for the whole life of the agent it runs,
// so without this the agent could not run `bowt test`, `bowt gate` or
// `bowt review` inside its own lane — every one would report the worktree busy
// against its own supervisor. Matching on the key (rather than a bare boolean)
// keeps the pass narrow: a lane working in worktree A still locks normally when
// it reaches into worktree B.
func HeldByAncestor(key string) bool {
	return key != "" && os.Getenv(EnvHeld) == key
}

// ExportHeld records key in the current process's environment so descendants
// see it via HeldByAncestor. Callers do this immediately after a successful
// Acquire whose lifetime spans a child process.
func ExportHeld(key string) error {
	return os.Setenv(EnvHeld, key)
}

// AcquireReentrant behaves like Acquire, except that when an ancestor already
// holds key it returns a Lock that owns no file descriptor. Release on such a
// Lock is a no-op, so the real lock stays held by the ancestor for its full
// lifetime and a deferred Release in the descendant cannot drop it early.
func AcquireReentrant(key string) (*Lock, error) {
	if HeldByAncestor(key) {
		return &Lock{}, nil
	}
	return Acquire(key)
}

// AcquireSharedReentrant is AcquireShared with the same ancestor pass as
// AcquireReentrant.
func AcquireSharedReentrant(key string) (*Lock, error) {
	if HeldByAncestor(key) {
		return &Lock{}, nil
	}
	return AcquireShared(key)
}

// Acquire takes an exclusive, NON-BLOCKING lock keyed on key (a branch or a
// worktree path). If another process holds it, Acquire fails immediately rather
// than waiting — the "fail fast and say it's busy" contract.
func Acquire(key string) (*Lock, error) {
	return acquire(key, syscall.LOCK_EX)
}

// AcquireShared takes a SHARED (read) lock keyed on key, also non-blocking.
// Multiple shared holders coexist, but a shared acquire fails fast if an
// exclusive holder has the key — so a read-only extension (`bowt-lock: shared`)
// runs concurrently with other readers yet still yields to a writer.
func AcquireShared(key string) (*Lock, error) {
	return acquire(key, syscall.LOCK_SH)
}

// acquire opens the keyed lockfile and takes a non-blocking flock of mode
// (LOCK_EX or LOCK_SH). It is the shared body of Acquire/AcquireShared.
func acquire(key string, mode int) (*Lock, error) {
	name, err := lockfilePath(key)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(name, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), mode|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("busy: %q is locked by another bowt process", key)
	}
	return &Lock{f: f}, nil
}

// Probe reports whether the lock keyed on key is currently held by a live
// holder, WITHOUT retaining it. It opens the same lockfile Acquire uses (via the
// shared lockfilePath) and attempts a non-blocking exclusive flock:
//   - EWOULDBLOCK/EAGAIN ⇒ held=true (another open file description holds it —
//     including one in a different process, which is what a supervisor is);
//   - success ⇒ held=false, and the probe IMMEDIATELY drops the lock (LOCK_UN +
//     close) so it never becomes a holder itself.
//
// A supervisor holds LOCK_EX for its whole run, so "Probe reports free" is the
// exact signal that no live supervisor owns the worktree — what the G4
// reconciler needs. Holder IDENTITY is out of scope: the cockpit attributes a
// held lock to the running lane on that worktree (from the lanes table), which
// is more useful than a PID and needs no pidfile.
//
// Caveat — momentary acquire: a Probe that finds the lock FREE briefly took and
// released it, so "free" means "free at the instant of the probe". That is safe
// for the reconciler, because a live supervisor would still be holding LOCK_EX
// and the probe's non-blocking acquire would fail; the momentary acquire can
// only ever race a lock that had no live holder to begin with.
func Probe(key string) (held bool, err error) {
	name, err := lockfilePath(key)
	if err != nil {
		return false, err
	}
	f, err := os.OpenFile(name, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()

	if flerr := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); flerr != nil {
		if errors.Is(flerr, syscall.EWOULDBLOCK) || errors.Is(flerr, syscall.EAGAIN) {
			return true, nil
		}
		return false, fmt.Errorf("probe lock %q: %w", key, flerr)
	}
	// Free: we just took it non-destructively; release at once so the probe is
	// never itself the holder the next Probe/Acquire sees.
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false, nil
}

// lockfilePath is the on-disk path of the lockfile keyed on key: the shared
// path/sanitize logic used by both acquire (holds it) and Probe (tests it), so
// the two can never key off different files. It creates ~/.bowt/locks as needed.
func lockfilePath(key string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".bowt", "locks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return filepath.Join(dir, sanitize(key)+".lock"), nil
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
