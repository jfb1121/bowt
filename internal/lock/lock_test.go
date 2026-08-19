package lock

import (
	"path/filepath"
	"testing"
)

func TestExclusiveAndReacquire(t *testing.T) {
	// Override HOME so the lockfile lands in a throwaway dir, not real ~/.bowt.
	t.Setenv("HOME", t.TempDir())

	l, err := Acquire("feature/x")
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}

	// A second acquire while held must fail fast (non-blocking flock).
	if second, err := Acquire("feature/x"); err == nil {
		_ = second.Release()
		t.Fatal("second Acquire succeeded while lock was held; want busy error")
	}

	// After release, the key is acquirable again.
	if err := l.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	l2, err := Acquire("feature/x")
	if err != nil {
		t.Fatalf("reacquire after release: %v", err)
	}
	_ = l2.Release()

	// A different key is independent — never contends with feature/x.
	other, err := Acquire("feature/y")
	if err != nil {
		t.Fatalf("independent key should acquire: %v", err)
	}
	_ = other.Release()
}

// Probe reports free before a lock is taken, held while it is held, and free
// again after Release — the read-only signal the G4 reconciler keys off. It must
// never leave the key locked (a probe that finds it free must drop it at once),
// so a real Acquire after a "free" Probe still succeeds.
func TestProbeFreeHeldFree(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	// Free before anyone holds it.
	if held, err := Probe("wt/probe"); err != nil || held {
		t.Fatalf("Probe before acquire: held=%v err=%v; want held=false", held, err)
	}
	// A free Probe must not have retained the lock: Acquire still succeeds.
	l, err := Acquire("wt/probe")
	if err != nil {
		t.Fatalf("Acquire after free Probe: %v", err)
	}
	// Held while the lock is held.
	if held, err := Probe("wt/probe"); err != nil || !held {
		t.Fatalf("Probe while held: held=%v err=%v; want held=true", held, err)
	}
	// A held Probe must not disturb the real holder: it still releases cleanly.
	if err := l.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	// Free again after Release.
	if held, err := Probe("wt/probe"); err != nil || held {
		t.Fatalf("Probe after release: held=%v err=%v; want held=false", held, err)
	}
	// A distinct key is independent of the probed one.
	if held, err := Probe("wt/other"); err != nil || held {
		t.Fatalf("Probe of independent key: held=%v err=%v; want held=false", held, err)
	}
}

// Shared locks coexist with each other but fail fast against an exclusive
// holder — the semantics `bowt-lock: shared` relies on.
func TestSharedCoexistsButYieldsToExclusive(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	s1, err := AcquireShared("wt/a")
	if err != nil {
		t.Fatalf("first shared acquire: %v", err)
	}
	// A second shared holder is allowed concurrently.
	s2, err := AcquireShared("wt/a")
	if err != nil {
		t.Fatalf("second shared acquire should coexist: %v", err)
	}
	// An exclusive acquire must fail fast while shared holders exist.
	if ex, err := Acquire("wt/a"); err == nil {
		_ = ex.Release()
		t.Fatal("exclusive acquire should fail while shared held")
	}
	_ = s1.Release()
	_ = s2.Release()

	// Once all shared holders release, exclusive is available again.
	ex, err := Acquire("wt/a")
	if err != nil {
		t.Fatalf("exclusive after shared released: %v", err)
	}
	// And a shared acquire fails fast while exclusive is held.
	if s, err := AcquireShared("wt/a"); err == nil {
		_ = s.Release()
		t.Fatal("shared acquire should fail while exclusive held")
	}
	_ = ex.Release()
}

// A descendant of a lock holder must be able to proceed on the same key —
// otherwise a spawn lane cannot run its own test/gate/review, which is the bug
// this pass exists to fix. The negative half matters just as much: without the
// exported key the acquire must still fail, or the pass would be a hole.
func TestAcquireReentrantHonoursAnAncestorsExportedKey(t *testing.T) {
	key := filepath.Join(t.TempDir(), "wt")

	held, err := Acquire(key)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer func() { _ = held.Release() }()

	if _, err := AcquireReentrant(key); err == nil {
		t.Fatal("reentrant acquire succeeded with no exported key — the pass is not scoped")
	}

	t.Setenv(EnvHeld, key)

	l, err := AcquireReentrant(key)
	if err != nil {
		t.Fatalf("reentrant acquire with the key exported: %v", err)
	}
	// Releasing the descendant's sentinel must NOT drop the ancestor's lock.
	if err := l.Release(); err != nil {
		t.Fatalf("release sentinel: %v", err)
	}
	if _, err := Acquire(key); err == nil {
		t.Fatal("ancestor's lock was dropped by the descendant's Release")
	}

	// A different key must still lock normally even while one key is passed.
	other := filepath.Join(t.TempDir(), "other")
	l2, err := AcquireReentrant(other)
	if err != nil {
		t.Fatalf("unrelated key should acquire normally: %v", err)
	}
	_ = l2.Release()
}
