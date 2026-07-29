package gitx

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestNormalizeRepositoryIdentityAcceptsEquivalentGitHubForms(t *testing.T) {
	forms := []string{
		"https://github.com/Kidus-Tiliksew/Conveyor.git",
		"ssh://git@github.com/kidus-tiliksew/conveyor.git",
		"git@github.com:kidus-tiliksew/conveyor.git",
		"git://github.com/kidus-tiliksew/conveyor",
	}
	for _, form := range forms {
		identity, err := NormalizeRepositoryIdentity(form)
		if err != nil {
			t.Fatalf("normalize %q: %v", form, err)
		}
		if identity != "github.com/kidus-tiliksew/conveyor" {
			t.Fatalf("normalize %q = %q", form, identity)
		}
	}
}

func TestPruneRepositoryRemovesOnlyStaleRegistrations(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	ctx := context.Background()
	tmp := t.TempDir()
	origin := filepath.Join(tmp, "origin.git")
	seed := filepath.Join(tmp, "seed")
	primary := filepath.Join(tmp, "primary")
	live := filepath.Join(tmp, "live")
	stale := filepath.Join(tmp, "stale")
	runGitTest(t, "", "init", "--bare", "--initial-branch=main", origin)
	runGitTest(t, "", "init", "-b", "main", seed)
	runGitTest(t, seed, "config", "user.name", "test")
	runGitTest(t, seed, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, seed, "add", ".")
	runGitTest(t, seed, "commit", "-m", "initial")
	runGitTest(t, seed, "remote", "add", "origin", origin)
	runGitTest(t, seed, "push", "-u", "origin", "main")
	runGitTest(t, "", "clone", origin, primary)
	runGitTest(t, primary, "worktree", "add", "-b", "conveyor/task-live", live, "origin/main")
	runGitTest(t, primary, "worktree", "add", "-b", "conveyor/task-stale", stale, "origin/main")
	if err := os.RemoveAll(stale); err != nil {
		t.Fatal(err)
	}

	if err := PruneRepository(ctx, primary); err != nil {
		t.Fatal(err)
	}
	worktrees, err := listLinkedWorktrees(ctx, primary)
	if err != nil {
		t.Fatal(err)
	}
	var liveFound, staleFound bool
	for _, worktree := range worktrees {
		liveFound = liveFound || samePath(worktree.Path, live)
		staleFound = staleFound || samePath(worktree.Path, stale)
	}
	if !liveFound || staleFound {
		t.Fatalf("worktrees after prune: live=%v stale=%v entries=%+v", liveFound, staleFound, worktrees)
	}
	if !refExists(ctx, primary, "refs/heads/conveyor/task-live") ||
		!refExists(ctx, primary, "refs/heads/conveyor/task-stale") {
		t.Fatal("prune removed a task branch")
	}
}

func runGitTest(t *testing.T, dir string, args ...string) {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
}
