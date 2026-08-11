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
