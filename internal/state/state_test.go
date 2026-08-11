package state

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
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

// TestModePersists proves the mode column round-trips: a code-only row reads
// back code-only, and an unset Mode defaults to full on both write and read.
func TestModePersists(t *testing.T) {
	st := mustOpen(t)

	mustAdd(t, st, Worktree{Repo: "demo", Branch: "co", Offset: 1, Mode: ModeCodeOnly})
	mustAdd(t, st, Worktree{Repo: "demo", Branch: "def", Offset: 2}) // no Mode → full

	co, ok, err := st.Get("demo", "co")
	if err != nil || !ok {
		t.Fatalf("Get co: ok=%v err=%v", ok, err)
	}
	if co.Mode != ModeCodeOnly || !co.CodeOnly() {
		t.Fatalf("co: Mode=%q CodeOnly=%v; want code-only/true", co.Mode, co.CodeOnly())
	}

	def, ok, err := st.Get("demo", "def")
	if err != nil || !ok {
		t.Fatalf("Get def: ok=%v err=%v", ok, err)
	}
	if def.Mode != ModeFull || def.CodeOnly() {
		t.Fatalf("def: Mode=%q CodeOnly=%v; want full/false", def.Mode, def.CodeOnly())
	}

	// List surfaces the mode too.
	got, err := st.List("demo")
	if err != nil {
		t.Fatal(err)
	}
	modes := map[string]Mode{}
	for _, wt := range got {
		modes[wt.Branch] = wt.Mode
	}
	if modes["co"] != ModeCodeOnly || modes["def"] != ModeFull {
		t.Fatalf("List modes = %v; want co=code-only def=full", modes)
	}
}

// TestAddModeColumnMigration proves the ALTER migration is idempotent: a
// pre-mode registry (no mode column) is upgraded once on Open, its legacy rows
// read back as full, and a second Open swallows the duplicate-column error.
func TestAddModeColumnMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")

	// Build a pre-mode DB by hand: the worktrees table WITHOUT the mode column,
	// plus one legacy row.
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	oldSchema := `CREATE TABLE worktrees (
		repo     TEXT    NOT NULL,
		branch   TEXT    NOT NULL,
		offset_n INTEGER NOT NULL,
		port     INTEGER NOT NULL,
		path     TEXT    NOT NULL,
		created  TEXT    NOT NULL,
		PRIMARY KEY (repo, branch)
	);`
	if _, err := raw.Exec(oldSchema); err != nil {
		t.Fatalf("old schema: %v", err)
	}
	if _, err := raw.Exec(
		`INSERT INTO worktrees (repo, branch, offset_n, port, path, created) VALUES (?,?,?,?,?,?)`,
		"demo", "legacy", 1, 8001, "/wt", time.Now().Format(time.RFC3339Nano)); err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}
	_ = raw.Close()

	// First Open runs the ADD COLUMN migration; the legacy row must default to full.
	st1, err := OpenAt(path)
	if err != nil {
		t.Fatalf("first open (migration): %v", err)
	}
	wt, ok, err := st1.Get("demo", "legacy")
	if err != nil || !ok {
		t.Fatalf("legacy row: ok=%v err=%v", ok, err)
	}
	if wt.Mode != ModeFull {
		t.Fatalf("legacy Mode = %q; want full", wt.Mode)
	}

	// Second Open must be a no-op: re-running ADD COLUMN hits "duplicate column
	// name", which is swallowed. A non-nil error here means the guard regressed.
	if _, err := OpenAt(path); err != nil {
		t.Fatalf("second open must swallow duplicate column: %v", err)
	}
}

func mustAdd(t *testing.T, st Store, wt Worktree) {
	t.Helper()
	if err := st.Add(wt); err != nil {
		t.Fatalf("Add(%s/%s): %v", wt.Repo, wt.Branch, err)
	}
}
