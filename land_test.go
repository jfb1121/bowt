package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jfb1121/bowt/internal/repo"
	"github.com/jfb1121/bowt/internal/state"
)

// git runs a git command in dir and fails the test on error. Fixtures build
// real repos so land exercises real merge/ff/branch semantics, not a fake.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s (in %s): %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// passHook / failHook are stub gate.sh bodies emitting a BOWT_CHECK line so the
// gate verdict is real, without invoking make/pytest/etc.
const (
	passHook = "#!/usr/bin/env bash\necho 'BOWT_CHECK build pass 0'\nexit 0\n"
	failHook = "#!/usr/bin/env bash\necho 'BOWT_CHECK build fail 1'\nexit 1\n"
)

// landFixture builds a main repo on branch `main` with one commit, a `feature`
// worktree one commit ahead (fast-forwardable), a registered store row, an
// optional gate.sh, and chdirs into the main repo. It returns the store, the
// repo's registry name, the main repo path, and the feature worktree path.
type landFixture struct {
	st       state.Store
	name     string
	mainRepo string
	wtPath   string
	origin   string // bare repo standing in for "origin"
}

func newLandFixture(t *testing.T, hookBody string) landFixture {
	t.Helper()
	// Isolate ~/.bowt (locks, default state) from the real home.
	t.Setenv("HOME", t.TempDir())

	// A bare repo as "origin" so land's push (and remote-branch delete) exercise
	// real git remote semantics. Harmless for --no-push tests, which never push.
	origin := t.TempDir()
	git(t, origin, "init", "-q", "--bare")

	mainRepo := t.TempDir()
	git(t, mainRepo, "init", "-q")
	git(t, mainRepo, "config", "user.email", "t@example.com")
	git(t, mainRepo, "config", "user.name", "t")
	git(t, mainRepo, "remote", "add", "origin", origin)
	if err := os.WriteFile(filepath.Join(mainRepo, "base.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, mainRepo, "add", ".")
	git(t, mainRepo, "commit", "-q", "-m", "base")
	git(t, mainRepo, "branch", "-M", "main")

	if hookBody != "" {
		cfg := filepath.Join(mainRepo, ".bowt")
		if err := os.MkdirAll(cfg, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cfg, "gate.sh"), []byte(hookBody), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// A feature worktree, one commit ahead of main so it fast-forwards.
	wtPath := filepath.Join(t.TempDir(), "feature-wt")
	git(t, mainRepo, "worktree", "add", "-q", "-b", "feature", wtPath, "main")
	if err := os.WriteFile(filepath.Join(wtPath, "feat.txt"), []byte("feat\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, wtPath, "add", ".")
	git(t, wtPath, "commit", "-q", "-m", "feat")

	t.Chdir(mainRepo)
	name, err := repo.Name()
	if err != nil {
		t.Fatalf("repo.Name: %v", err)
	}

	st, err := state.OpenAt(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("OpenAt: %v", err)
	}
	if err := st.Add(state.Worktree{
		Repo: name, Branch: "feature", Offset: 1, Port: 3001, Path: wtPath, Created: time.Now(),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return landFixture{st: st, name: name, mainRepo: mainRepo, wtPath: wtPath, origin: origin}
}

// remoteBranchExists reports whether refs/heads/branch is present on origin.
func (f landFixture) remoteBranchExists(t *testing.T, branch string) bool {
	t.Helper()
	cmd := exec.Command("git", "-C", f.origin, "rev-parse", "--verify", "refs/heads/"+branch)
	return cmd.Run() == nil
}

func (f landFixture) head(t *testing.T, dir string) string {
	t.Helper()
	h, _, err := repo.Head(dir)
	if err != nil {
		t.Fatalf("head %s: %v", dir, err)
	}
	return h
}

// captureStderr redirects os.Stderr to a temp file for the duration of fn and
// returns what was written — how land's warnings (output.Errf) reach a test.
// A file (not a pipe) sidesteps any blocking when nothing drains the reader.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stderr
	os.Stderr = f
	defer func() { os.Stderr = orig }()
	fn()
	_ = f.Close()
	out, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// TestLandHappyPath: green gate + clean + FF → lands (ff-merge happened, base
// advanced to the branch HEAD, worktree removed, branch deleted, registry row
// gone), and reports it.
func TestLandHappyPath(t *testing.T) {
	f := newLandFixture(t, passHook)
	featHead := f.head(t, f.wtPath)

	if err := cmdLand(f.st, "feature", landOpts{noPush: true}); err != nil {
		t.Fatalf("land: %v", err)
	}

	// The ff-merge happened: main now points at the feature HEAD.
	if got := f.head(t, f.mainRepo); got != featHead {
		t.Errorf("main HEAD = %s; want feature HEAD %s (ff-merge did not happen)", got, featHead)
	}
	// Worktree removed from disk.
	if _, err := os.Stat(f.wtPath); !os.IsNotExist(err) {
		t.Errorf("worktree %s still present; want removed", f.wtPath)
	}
	// Local branch deleted.
	if repo.BranchExists(f.mainRepo, "feature") {
		t.Error("branch 'feature' still exists; want deleted")
	}
	// Registry row gone.
	if _, ok, _ := f.st.Get(f.name, "feature"); ok {
		t.Error("registry still has 'feature'; want deregistered")
	}
}

// TestLandDeletesRemoteBranch: a normal land (push enabled) deletes the merged
// remote branch on origin along with the local branch and worktree.
func TestLandDeletesRemoteBranch(t *testing.T) {
	f := newLandFixture(t, passHook)
	// The feature branch exists on origin (as it would after a spawn/push).
	git(t, f.wtPath, "push", "-q", "origin", "feature")
	if !f.remoteBranchExists(t, "feature") {
		t.Fatal("setup: feature not on origin")
	}

	if err := cmdLand(f.st, "feature", landOpts{}); err != nil {
		t.Fatalf("land: %v", err)
	}
	if f.remoteBranchExists(t, "feature") {
		t.Error("origin/feature still present; want deleted on a normal land")
	}
	// Local cleanup still happened too.
	if repo.BranchExists(f.mainRepo, "feature") {
		t.Error("local branch 'feature' still exists; want deleted")
	}
}

// TestLandDeletesRemoteBranchNeverPushed: a normal land where the branch was
// never pushed to origin still succeeds — remote-delete is a no-op, not a fault.
func TestLandDeletesRemoteBranchNeverPushed(t *testing.T) {
	f := newLandFixture(t, passHook)
	// No `git push origin feature` here: origin has no feature ref.
	if f.remoteBranchExists(t, "feature") {
		t.Fatal("setup: feature unexpectedly on origin")
	}

	if err := cmdLand(f.st, "feature", landOpts{}); err != nil {
		t.Fatalf("land should tolerate a never-pushed branch: %v", err)
	}
}

// TestLandKeepKeepsRemoteBranch: --keep preserves the remote branch too.
func TestLandKeepKeepsRemoteBranch(t *testing.T) {
	f := newLandFixture(t, passHook)
	git(t, f.wtPath, "push", "-q", "origin", "feature")

	if err := cmdLand(f.st, "feature", landOpts{keep: true}); err != nil {
		t.Fatalf("land: %v", err)
	}
	if !f.remoteBranchExists(t, "feature") {
		t.Error("origin/feature deleted despite --keep")
	}
}

// TestLandNoPushKeepsRemoteBranch: --no-push pushes nothing, so the remote
// branch is left alone.
func TestLandNoPushKeepsRemoteBranch(t *testing.T) {
	f := newLandFixture(t, passHook)
	git(t, f.wtPath, "push", "-q", "origin", "feature")

	if err := cmdLand(f.st, "feature", landOpts{noPush: true}); err != nil {
		t.Fatalf("land: %v", err)
	}
	if !f.remoteBranchExists(t, "feature") {
		t.Error("origin/feature deleted despite --no-push")
	}
}

// TestLandKeep: --keep lands but preserves the worktree and branch.
func TestLandKeep(t *testing.T) {
	f := newLandFixture(t, passHook)
	if err := cmdLand(f.st, "feature", landOpts{noPush: true, keep: true}); err != nil {
		t.Fatalf("land: %v", err)
	}
	if _, err := os.Stat(f.wtPath); err != nil {
		t.Errorf("worktree removed despite --keep: %v", err)
	}
	if !repo.BranchExists(f.mainRepo, "feature") {
		t.Error("branch deleted despite --keep")
	}
}

// TestLandGateFailRefuses: a red gate → refuse, and base is untouched.
func TestLandGateFailRefuses(t *testing.T) {
	f := newLandFixture(t, failHook)
	before := f.head(t, f.mainRepo)

	err := cmdLand(f.st, "feature", landOpts{noPush: true})
	if err == nil {
		t.Fatal("land should refuse on a failing gate")
	}
	if !strings.Contains(err.Error(), "gate did not pass") {
		t.Errorf("error = %q; want a gate-fail refusal", err)
	}
	if got := f.head(t, f.mainRepo); got != before {
		t.Errorf("main HEAD moved to %s on a refusal; want unchanged %s", got, before)
	}
	if _, err := os.Stat(f.wtPath); err != nil {
		t.Errorf("worktree removed on a refusal: %v", err)
	}
}

// TestLandDirtyRefuses: uncommitted changes in the worktree → refuse.
func TestLandDirtyRefuses(t *testing.T) {
	f := newLandFixture(t, passHook)
	if err := os.WriteFile(filepath.Join(f.wtPath, "dirty.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := f.head(t, f.mainRepo)

	err := cmdLand(f.st, "feature", landOpts{noPush: true})
	if err == nil {
		t.Fatal("land should refuse a dirty worktree")
	}
	if !strings.Contains(err.Error(), "uncommitted changes") {
		t.Errorf("error = %q; want a dirty refusal", err)
	}
	if got := f.head(t, f.mainRepo); got != before {
		t.Errorf("main HEAD moved on a refusal; want unchanged")
	}
}

// TestLandNonFFRefuses: base has a commit the branch lacks → not a
// fast-forward → refuse.
func TestLandNonFFRefuses(t *testing.T) {
	f := newLandFixture(t, passHook)
	// Advance main past the branch point so feature no longer fast-forwards.
	if err := os.WriteFile(filepath.Join(f.mainRepo, "base2.txt"), []byte("more\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, f.mainRepo, "add", ".")
	git(t, f.mainRepo, "commit", "-q", "-m", "diverge")
	before := f.head(t, f.mainRepo)

	err := cmdLand(f.st, "feature", landOpts{noPush: true})
	if err == nil {
		t.Fatal("land should refuse a non-fast-forward branch")
	}
	if !strings.Contains(err.Error(), "fast-forward") {
		t.Errorf("error = %q; want a non-FF refusal", err)
	}
	if got := f.head(t, f.mainRepo); got != before {
		t.Errorf("main HEAD moved on a refusal; want unchanged")
	}
}

// TestLandNoHookHardErrors: no gate.sh and no --no-gate → hard error, no land.
func TestLandNoHookHardErrors(t *testing.T) {
	f := newLandFixture(t, "") // no hook
	before := f.head(t, f.mainRepo)

	err := cmdLand(f.st, "feature", landOpts{noPush: true})
	if err == nil {
		t.Fatal("land should hard-error when there is no gate hook and no --no-gate")
	}
	if !strings.Contains(err.Error(), "no gate hook") {
		t.Errorf("error = %q; want a missing-hook error", err)
	}
	if got := f.head(t, f.mainRepo); got != before {
		t.Errorf("main HEAD moved; want unchanged")
	}
}

// TestLandNoGateWarnsAndProceeds: --no-gate lands without a hook but warns loudly.
func TestLandNoGateWarnsAndProceeds(t *testing.T) {
	f := newLandFixture(t, "") // no hook
	featHead := f.head(t, f.wtPath)

	var landErr error
	stderr := captureStderr(t, func() {
		landErr = cmdLand(f.st, "feature", landOpts{noPush: true, noGate: true})
	})
	if landErr != nil {
		t.Fatalf("land --no-gate should proceed: %v", landErr)
	}
	if got := f.head(t, f.mainRepo); got != featHead {
		t.Errorf("main HEAD = %s; want feature HEAD %s", got, featHead)
	}
	if !strings.Contains(stderr, "WARNING") {
		t.Errorf("stderr = %q; want a loud WARNING for --no-gate", stderr)
	}
}

// TestLandUnregisteredRefuses: an unknown branch → refuse (nothing to land).
func TestLandUnregisteredRefuses(t *testing.T) {
	f := newLandFixture(t, passHook)
	if err := cmdLand(f.st, "ghost", landOpts{noPush: true}); err == nil {
		t.Fatal("land should refuse an unregistered branch")
	} else if !strings.Contains(err.Error(), "no worktree registered") {
		t.Errorf("error = %q; want an unregistered refusal", err)
	}
}
