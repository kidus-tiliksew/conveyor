package gitx

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
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
	// A later dispatch for another task re-fetches the mirror; the
	// unpushed task branch must survive the prune.
	mirror, err := m.EnsureMirror(ctx, repoURL)
	if err != nil {
		t.Fatal(err)
	}
	if !refExists(ctx, mirror, "refs/heads/"+BranchName("77")) {
		t.Fatal("fetch --prune deleted the unpushed task branch")
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
