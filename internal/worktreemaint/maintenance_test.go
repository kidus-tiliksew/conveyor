package worktreemaint

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func TestTerminalCleanupRetriesDirtyWorktreesAndRecordsNoOps(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	ctx := store.WithWorkspace(context.Background(), "demo")
	tmp := t.TempDir()
	origin := filepath.Join(tmp, "origin.git")
	seed := filepath.Join(tmp, "seed")
	primary := filepath.Join(tmp, "conveyor")
	cleanPath := filepath.Join(tmp, "conveyor-worktrees", "conveyor-task-clean")
	dirtyPath := filepath.Join(tmp, "conveyor-worktrees", "conveyor-task-dirty")
	stalePath := filepath.Join(tmp, "conveyor-worktrees", "conveyor-task-stale")
	initializeRepository(t, origin, seed, primary)
	for branch, path := range map[string]string{
		"conveyor/task-clean": cleanPath,
		"conveyor/task-dirty": dirtyPath,
		"conveyor/task-stale": stalePath,
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		runGit(t, primary, "worktree", "add", "-b", branch, path, "origin/main")
	}
	if err := os.WriteFile(filepath.Join(dirtyPath, "uncommitted.txt"), []byte("keep\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(stalePath); err != nil {
		t.Fatal(err)
	}

	st := store.NewMemory()
	for _, task := range []core.Task{
		{ID: "clean", Workspace: "demo", Repo: "conveyor", Branch: "conveyor/task-clean", State: core.TaskMerged},
		{ID: "dirty", Workspace: "demo", Repo: "conveyor", Branch: "conveyor/task-dirty", State: core.TaskClosed},
		{ID: "stale", Workspace: "demo", Repo: "conveyor", Branch: "conveyor/task-stale", State: core.TaskMerged},
		{ID: "absent", Workspace: "demo", Repo: "conveyor", Branch: "conveyor/task-absent", State: core.TaskClosed},
		{ID: "unavailable", Workspace: "demo", Repo: "missing", Branch: "conveyor/task-unavailable", State: core.TaskMerged},
		{ID: "running", Workspace: "demo", Repo: "conveyor", Branch: "conveyor/task-running", State: core.TaskRunning},
	} {
		if err := st.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &config.Config{
		Workspace: "demo",
		CacheDir:  filepath.Join(tmp, "cache"),
		Repos: []config.Repo{
			{Name: "conveyor", URL: origin, Base: "main"},
			{Name: "missing", URL: filepath.Join(tmp, "missing-origin.git"), Base: "main"},
		},
	}
	maintainer := &Maintainer{
		Store: st, StartDir: primary,
		ConfigProvider: func(context.Context) (*config.Config, error) { return cfg, nil },
	}

	first, err := maintainer.Reconcile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if first.Cleaned != 3 || first.Failed == 0 {
		t.Fatalf("first reconciliation = %+v", first)
	}
	assertMissing(t, cleanPath)
	assertPresent(t, dirtyPath)
	assertMissing(t, stalePath)
	assertCompletionCount(t, st, ctx, "clean", 1)
	assertCompletionCount(t, st, ctx, "stale", 1)
	assertCompletionCount(t, st, ctx, "absent", 1)
	assertCompletionCount(t, st, ctx, "dirty", 0)
	assertCompletionCount(t, st, ctx, "unavailable", 0)
	assertCompletionCount(t, st, ctx, "running", 0)

	dirtyFile := filepath.Join(dirtyPath, "uncommitted.txt")
	if err := os.Remove(dirtyFile); err != nil {
		t.Fatal(err)
	}
	second, err := maintainer.Reconcile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if second.Cleaned != 1 {
		t.Fatalf("second reconciliation = %+v", second)
	}
	assertMissing(t, dirtyPath)
	assertCompletionCount(t, st, ctx, "dirty", 1)

	if _, err := maintainer.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	for _, taskID := range []string{"clean", "dirty", "stale", "absent"} {
		assertCompletionCount(t, st, ctx, taskID, 1)
	}
	for _, branch := range []string{
		"conveyor/task-clean", "conveyor/task-dirty", "conveyor/task-stale",
	} {
		runGit(t, primary, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	}
}

func initializeRepository(t *testing.T, origin, seed, primary string) {
	t.Helper()
	runGit(t, "", "init", "--bare", "--initial-branch=main", origin)
	runGit(t, "", "init", "-b", "main", seed)
	runGit(t, seed, "config", "user.name", "test")
	runGit(t, seed, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, seed, "add", ".")
	runGit(t, seed, "commit", "-m", "initial")
	runGit(t, seed, "remote", "add", "origin", origin)
	runGit(t, seed, "push", "-u", "origin", "main")
	runGit(t, "", "clone", origin, primary)
}

func assertCompletionCount(t *testing.T, st store.Store, ctx context.Context, taskID string, want int) {
	t.Helper()
	got, err := st.CountEvents(ctx, taskID, CleanupCompletedEvent)
	if err != nil || got != want {
		t.Fatalf("completion count for %s = %d, want %d (err=%v)", taskID, got, want, err)
	}
}

func assertMissing(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("%s still exists: %v", path, err)
	}
}

func assertPresent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("%s is unavailable: %v", path, err)
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
}
