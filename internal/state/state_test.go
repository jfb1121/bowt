package state

import (
	"path/filepath"
	"testing"
)

// mustOpen gives each test its own throwaway SQLite file, auto-cleaned.
func mustOpen(t *testing.T) Store {
	t.Helper()
	st, err := OpenAt(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// Run with: go test ./...
func TestAddListNextOffset(t *testing.T) {
	st := mustOpen(t)

	if n, err := st.NextOffset("demo"); err != nil || n != 1 {
		t.Fatalf("first offset = %d, %v; want 1, nil", n, err)
	}

	if err := st.Add(Worktree{Repo: "demo", Branch: "feat", Offset: 1, Port: 8001}); err != nil {
		t.Fatal(err)
	}

	// Adding the same repo+branch again must be refused with the sentinel error.
	if err := st.Add(Worktree{Repo: "demo", Branch: "feat"}); err != ErrExists {
		t.Fatalf("duplicate Add = %v; want ErrExists", err)
	}

	got, err := st.List("demo")
	if err != nil || len(got) != 1 {
		t.Fatalf("List = %v, %v; want exactly 1 entry", got, err)
	}

	if n, _ := st.NextOffset("demo"); n != 2 {
		t.Fatalf("next offset = %d; want 2", n)
	}
}

func TestRemoveAndRepoIsolation(t *testing.T) {
	st := mustOpen(t)

	mustAdd(t, st, Worktree{Repo: "a", Branch: "x", Offset: 1})
	mustAdd(t, st, Worktree{Repo: "a", Branch: "y", Offset: 2})
	mustAdd(t, st, Worktree{Repo: "b", Branch: "x", Offset: 1}) // same branch, different repo

	if got, _ := st.List("a"); len(got) != 2 {
		t.Fatalf("repo a has %d; want 2", len(got))
	}
	if got, _ := st.List("b"); len(got) != 1 {
		t.Fatalf("repo b has %d; want 1", len(got))
	}

	if err := st.Remove("a", "x"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := st.Get("a", "x"); ok {
		t.Fatal("a/x should be gone after Remove")
	}
	if _, ok, _ := st.Get("a", "y"); !ok {
		t.Fatal("a/y should remain after removing a/x")
	}
	if _, ok, _ := st.Get("b", "x"); !ok {
		t.Fatal("b/x (different repo) should be untouched")
	}
}

// TestPersistsAcrossReopen proves the row actually hit disk — a JSON-map fake
// would pass every other test but fail this one.
func TestPersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")

	st1, err := OpenAt(path)
	if err != nil {
		t.Fatal(err)
	}
	mustAdd(t, st1, Worktree{Repo: "demo", Branch: "feat", Offset: 1, Port: 8001})

	st2, err := OpenAt(path) // reopen the same file
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := st2.Get("demo", "feat"); err != nil || !ok {
		t.Fatalf("row did not persist across reopen: ok=%v err=%v", ok, err)
	}
}

func mustAdd(t *testing.T, st Store, wt Worktree) {
	t.Helper()
	if err := st.Add(wt); err != nil {
		t.Fatalf("Add(%s/%s): %v", wt.Repo, wt.Branch, err)
	}
}
