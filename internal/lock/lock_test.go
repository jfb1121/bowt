package lock

import "testing"

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
