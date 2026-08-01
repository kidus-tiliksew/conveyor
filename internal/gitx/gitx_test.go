package gitx

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestWorktreeLifecycle exercises the bare-mirror + worktree flow
// against a local fixture repo: mirror, add worktree on the task
// branch, re-add (re-dispatch), remove.
func TestWorktreeLifecycle(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	ctx := context.Background()
	tmp := t.TempDir()

	// Fixture "origin" repo with one commit on main.
	origin := filepath.Join(tmp, "origin")
	mustRun(t, "", "git", "init", "-b", "main", origin)
	mustRun(t, origin, "git", "config", "user.email", "test@example.com")
	mustRun(t, origin, "git", "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(origin, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, origin, "git", "add", ".")
	mustRun(t, origin, "git", "commit", "-m", "init")

	m := NewManager(filepath.Join(tmp, "cache"), filepath.Join(tmp, "jobs"))
	repoURL := "file://" + origin

	wt, err := m.AddWorktree(ctx, repoURL, "api", "123", "main")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(wt, "README.md")); err != nil {
		t.Fatalf("worktree not checked out: %v", err)
	}
	if info, err := os.Stat(filepath.Join(wt, ".git")); err != nil || !info.IsDir() {
		t.Fatalf("task checkout is not self-contained: info=%v err=%v", info, err)
	}
	if _, err := os.Stat(filepath.Join(wt, ".git", "objects", "info", "alternates")); !os.IsNotExist(err) {
		t.Fatalf("task checkout still points at shared cache: %v", err)
	}

	// The worktree must be on the task branch.
	out, err := exec.Command("git", "-C", wt, "branch", "--show-current").Output()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(out), BranchName("123")+"\n"; got != want {
		t.Fatalf("branch = %q, want %q", got, want)
	}

	// Re-dispatch with the worktree present returns it untouched.
	again, err := m.AddWorktree(ctx, repoURL, "api", "123", "main")
	if err != nil {
		t.Fatal(err)
	}
	if again != wt {
		t.Fatalf("re-dispatch returned %q, want %q", again, wt)
	}

	// Committed task work must survive worktree removal + re-add: the
	// branch is checked out again, never reset to base.
	if err := os.WriteFile(filepath.Join(wt, "work.txt"), []byte("agent output\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, wt, "git", "add", ".")
	mustRun(t, wt, "git", "-c", "user.email=test@example.com", "-c", "user.name=test", "commit", "-m", "task work")

	if err := m.RemoveWorktree(ctx, repoURL, "api", "123"); err != nil {
		t.Fatal(err)
	}
	wt2, err := m.AddWorktree(ctx, repoURL, "api", "123", "main")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(wt2, "work.txt")); err != nil {
		t.Fatalf("committed task work lost on re-dispatch: %v", err)
	}

	if err := m.RemoveWorktree(ctx, repoURL, "api", "123"); err != nil {
		t.Fatal(err)
	}
	if err := m.Prune(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestPlanningSnapshotPlumbingStaysPinnedAndReadOnly(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	ctx := context.Background()
	tmp := t.TempDir()
	origin := filepath.Join(tmp, "origin")
	mustRun(t, "", "git", "init", "-b", "main", origin)
	mustRun(t, origin, "git", "config", "user.email", "test@example.com")
	mustRun(t, origin, "git", "config", "user.name", "test")
	if err := os.MkdirAll(filepath.Join(origin, "internal"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(origin, "internal", "eligibility.go"), []byte("package internal\n\nfunc eligible() bool { return true }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, origin, "git", "add", ".")
	mustRun(t, origin, "git", "commit", "-m", "initial eligibility")

	manager := NewManager(filepath.Join(tmp, "cache"), "")
	snapshot, err := manager.PinSnapshot(ctx, "file://"+origin, "main")
	if err != nil {
		t.Fatal(err)
	}
	entries, truncated, err := manager.ListSnapshotTree(ctx, snapshot, "", defaultSnapshotOutputBytes)
	if err != nil || truncated || len(entries) != 1 || entries[0].Path != "internal/eligibility.go" {
		t.Fatalf("entries=%+v err=%v", entries, err)
	}
	content, err := manager.ReadSnapshotBlob(ctx, snapshot, "internal/eligibility.go", defaultSnapshotOutputBytes)
	if err != nil || !strings.Contains(string(content), "return true") {
		t.Fatalf("content=%q err=%v", content, err)
	}
	matches, err := manager.GrepSnapshot(ctx, snapshot, "eligible", "internal", 0, false, false, 200, defaultSnapshotOutputBytes)
	if err != nil || !strings.Contains(matches, "eligibility.go:3:") {
		t.Fatalf("matches=%q err=%v", matches, err)
	}
	history, err := manager.SnapshotHistory(ctx, snapshot, "internal/eligibility.go", 20, defaultSnapshotOutputBytes)
	if err != nil || !strings.Contains(history, "initial eligibility") || !strings.Contains(history, "Latest commit context") {
		t.Fatalf("history=%q err=%v", history, err)
	}

	if err := os.WriteFile(filepath.Join(origin, "internal", "eligibility.go"), []byte("package internal\n\nfunc eligible() bool { return false }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, origin, "git", "add", ".")
	mustRun(t, origin, "git", "commit", "-m", "advance main")
	reopened, err := manager.OpenSnapshot(ctx, "file://"+origin, snapshot.Revision)
	if err != nil {
		t.Fatal(err)
	}
	content, err = manager.ReadSnapshotBlob(ctx, reopened, "internal/eligibility.go", defaultSnapshotOutputBytes)
	if err != nil || !strings.Contains(string(content), "return true") || strings.Contains(string(content), "return false") {
		t.Fatalf("pinned content changed: %q err=%v", content, err)
	}
}

func TestPlanningSnapshotPlumbingBoundsLargeSearchAndRejectsOversizedBlob(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	ctx := context.Background()
	tmp := t.TempDir()
	origin := filepath.Join(tmp, "origin")
	mustRun(t, "", "git", "init", "-b", "main", origin)
	mustRun(t, origin, "git", "config", "user.email", "test@example.com")
	mustRun(t, origin, "git", "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(origin, "large.txt"),
		[]byte(strings.Repeat("match bounded exploration output\n", 20_000)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(origin, "large.bin"), []byte(strings.Repeat("\x00\xff", 4_096)), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, origin, "git", "add", ".")
	mustRun(t, origin, "git", "commit", "-m", "large planning fixtures")
	manager := NewManager(filepath.Join(tmp, "cache"), "")
	snapshot, err := manager.PinSnapshot(ctx, "file://"+origin, "main")
	if err != nil {
		t.Fatal(err)
	}
	const outputLimit = 512
	matches, err := manager.GrepSnapshot(ctx, snapshot, ".", "large.txt", 0, false, false, 50, outputLimit)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) > outputLimit || !strings.Contains(matches, "truncated at git boundary") {
		t.Fatalf("bounded grep returned %d bytes:\n%s", len(matches), matches)
	}
	if _, err = manager.ReadSnapshotBlob(ctx, snapshot, "large.bin", outputLimit); err == nil ||
		!strings.Contains(err.Error(), "read limit") {
		t.Fatalf("oversized binary read error=%v", err)
	}
	if _, err = manager.GrepSnapshot(ctx, snapshot, "[", "large.txt", 0, false, false, 50, outputLimit); err == nil {
		t.Fatal("invalid git grep pattern unexpectedly succeeded")
	}
}

func TestExistingTaskCheckoutFastForwardsHumanPush(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	ctx := context.Background()
	tmp := t.TempDir()
	origin := filepath.Join(tmp, "origin")
	mustRun(t, "", "git", "init", "--bare", "--initial-branch=main", origin)
	seed := filepath.Join(tmp, "seed")
	mustRun(t, "", "git", "clone", origin, seed)
	mustRun(t, seed, "git", "config", "user.email", "test@example.com")
	mustRun(t, seed, "git", "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, seed, "git", "add", ".")
	mustRun(t, seed, "git", "commit", "-m", "initial")
	mustRun(t, seed, "git", "push", "origin", "main")

	m := NewManager(filepath.Join(tmp, "cache"), filepath.Join(tmp, "jobs"))
	repoURL := "file://" + origin
	wt, err := m.AddWorktree(ctx, repoURL, "api", "human", "main")
	if err != nil {
		t.Fatal(err)
	}
	mustRun(t, wt, "git", "push", "--set-upstream", "origin", BranchName("human"))

	human := filepath.Join(tmp, "human")
	mustRun(t, "", "git", "clone", "--branch", BranchName("human"), origin, human)
	mustRun(t, human, "git", "config", "user.email", "human@example.com")
	mustRun(t, human, "git", "config", "user.name", "human")
	if err := os.WriteFile(filepath.Join(human, "human.txt"), []byte("handoff\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, human, "git", "add", ".")
	mustRun(t, human, "git", "commit", "-m", "human handoff")
	mustRun(t, human, "git", "push", "origin", BranchName("human"))

	resumed, err := m.AddWorktree(ctx, repoURL, "api", "human", "main")
	if err != nil {
		t.Fatal(err)
	}
	if resumed != wt {
		t.Fatalf("resumed path = %q", resumed)
	}
	if _, err := os.Stat(filepath.Join(wt, "human.txt")); err != nil {
		t.Fatalf("runner checkout did not fast-forward human work: %v", err)
	}
}

// TestFetchPreservesTaskBranches guards the clone-mode regression: with
// a mirror clone, fetch --prune deletes local conveyor/task-* refs.
func TestFetchPreservesTaskBranches(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	ctx := context.Background()
	tmp := t.TempDir()

	origin := filepath.Join(tmp, "origin")
	mustRun(t, "", "git", "init", "-b", "main", origin)
	mustRun(t, origin, "git", "config", "user.email", "test@example.com")
	mustRun(t, origin, "git", "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(origin, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, origin, "git", "add", ".")
	mustRun(t, origin, "git", "commit", "-m", "init")

	m := NewManager(filepath.Join(tmp, "cache"), filepath.Join(tmp, "jobs"))
	repoURL := "file://" + origin

	if _, err := m.AddWorktree(ctx, repoURL, "api", "77", "main"); err != nil {
		t.Fatal(err)
	}
	// Eviction copies the task ref and its private objects into the trusted
	// cache. A later upstream fetch must not prune that preserved branch.
	if err := m.RemoveWorktree(ctx, repoURL, "api", "77"); err != nil {
		t.Fatal(err)
	}
	mirror, err := m.EnsureMirror(ctx, repoURL)
	if err != nil {
		t.Fatal(err)
	}
	if !refExists(ctx, mirror, "refs/heads/"+BranchName("77")) {
		t.Fatal("fetch --prune deleted the unpushed task branch")
	}
}

func TestRemoveWorktreeSerializesCopyBackWithMirrorFetches(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	ctx := context.Background()
	tmp := t.TempDir()
	origin := filepath.Join(tmp, "origin")
	mustRun(t, "", "git", "init", "-b", "main", origin)
	mustRun(t, origin, "git", "config", "user.email", "test@example.com")
	mustRun(t, origin, "git", "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(origin, "README.md"), []byte("main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, origin, "git", "add", ".")
	mustRun(t, origin, "git", "commit", "-m", "initial")

	m := NewManager(filepath.Join(tmp, "cache"), filepath.Join(tmp, "jobs"))
	repoURL := "file://" + origin
	if _, err := m.AddWorktree(ctx, repoURL, "api", "locked", "main"); err != nil {
		t.Fatal(err)
	}
	mirror, err := m.mirrorPath(repoURL)
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := lockRepo(mirror)
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		done <- m.RemoveWorktree(ctx, repoURL, "api", "locked")
	}()
	select {
	case err := <-done:
		unlock()
		t.Fatalf("RemoveWorktree bypassed mirror lock: %v", err)
	case <-time.After(150 * time.Millisecond):
		// Expected: copy-back is waiting for the lock held above.
	}
	unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RemoveWorktree did not continue after mirror lock release")
	}
}

func TestBranchDiffReadsPushedBranchFromBareCache(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	ctx := context.Background()
	tmp := t.TempDir()

	origin := filepath.Join(tmp, "origin")
	mustRun(t, "", "git", "init", "-b", "main", origin)
	mustRun(t, origin, "git", "config", "user.email", "test@example.com")
	mustRun(t, origin, "git", "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(origin, "app.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, origin, "git", "add", ".")
	mustRun(t, origin, "git", "commit", "-m", "init")

	m := NewManager(filepath.Join(tmp, "cache"), filepath.Join(tmp, "jobs"))
	repoURL := "file://" + origin
	branch := BranchName("42")

	// A branch origin has never seen must error, not read as an empty change.
	if _, err := m.BranchDiff(ctx, repoURL, branch, "main"); err == nil {
		t.Fatal("BranchDiff succeeded for a branch that was never pushed")
	}

	// Simulate the implementing agent's push (spec §21.8): commit task work on
	// the branch in origin, as `git push` from a task worktree would produce.
	mustRun(t, origin, "git", "checkout", "-b", branch)
	if err := os.WriteFile(filepath.Join(origin, "app.txt"), []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, origin, "git", "add", ".")
	mustRun(t, origin, "git", "commit", "-m", "task work")
	mustRun(t, origin, "git", "checkout", "main")

	diff, err := m.BranchDiff(ctx, repoURL, branch, "main")
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"diff --git a/app.txt b/app.txt", "-v1", "+v2"} {
		if !strings.Contains(diff, expected) {
			t.Fatalf("branch diff missing %q:\n%s", expected, diff)
		}
	}
}

func mustRun(t *testing.T, dir string, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v: %s", name, args, err, out)
	}
}
