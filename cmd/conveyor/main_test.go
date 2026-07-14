package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckoutAndDoneWorktreeLifecycle(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	tmp := t.TempDir()
	origin := filepath.Join(tmp, "origin")
	mustGit(t, "", "init", "-b", "main", origin)
	mustGit(t, origin, "config", "user.name", "test")
	mustGit(t, origin, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(origin, "README.md"), []byte("main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, origin, "add", ".")
	mustGit(t, origin, "commit", "-m", "initial")
	mustGit(t, origin, "checkout", "-b", "conveyor/task-123")
	if err := os.WriteFile(filepath.Join(origin, "task.txt"), []byte("task\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, origin, "add", ".")
	mustGit(t, origin, "commit", "-m", "task")
	mustGit(t, origin, "checkout", "main")

	primary := filepath.Join(tmp, "primary")
	mustGit(t, "", "clone", origin, primary)
	t.Chdir(primary)
	destination := filepath.Join(tmp, "human-task")
	got, err := checkoutTask(context.Background(), "conveyor/task-123", "api", "123", destination)
	if err != nil {
		t.Fatal(err)
	}
	if got != destination {
		t.Fatalf("destination = %q", got)
	}
	if _, err := os.Stat(filepath.Join(destination, "task.txt")); err != nil {
		t.Fatalf("task branch not checked out: %v", err)
	}
	mustGit(t, destination, "config", "user.name", "human")
	mustGit(t, destination, "config", "user.email", "human@example.com")
	if err := os.WriteFile(filepath.Join(destination, "human.txt"), []byte("handoff\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, destination, "add", "human.txt")
	mustGit(t, destination, "commit", "-m", "human handoff")
	if err := removeTaskWorktree(context.Background(), "conveyor/task-123", true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(destination); !os.IsNotExist(err) {
		t.Fatalf("worktree still exists: %v", err)
	}
	mustGit(t, origin, "show", "conveyor/task-123:human.txt")

	// The local branch remains after worktree removal. A later agent push must
	// fast-forward that ref before a second human checkout, not reopen stale
	// state or silently choose a side.
	mustGit(t, origin, "checkout", "conveyor/task-123")
	if err := os.WriteFile(filepath.Join(origin, "agent-second.txt"), []byte("second run\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, origin, "add", "agent-second.txt")
	mustGit(t, origin, "commit", "-m", "second agent run")
	mustGit(t, origin, "checkout", "main")

	secondDestination := filepath.Join(tmp, "human-task-second")
	if _, err := checkoutTask(context.Background(), "conveyor/task-123", "api", "123", secondDestination); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(secondDestination, "agent-second.txt")); err != nil {
		t.Fatalf("second checkout did not fast-forward: %v", err)
	}
	if err := removeTaskWorktree(context.Background(), "conveyor/task-123", false); err != nil {
		t.Fatal(err)
	}
}

func TestCheckoutWaitsForImplementationAgentToPushBranch(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	tmp := t.TempDir()
	origin := filepath.Join(tmp, "origin")
	mustGit(t, "", "init", "-b", "main", origin)
	mustGit(t, origin, "config", "user.name", "test")
	mustGit(t, origin, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(origin, "README.md"), []byte("main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, origin, "add", ".")
	mustGit(t, origin, "commit", "-m", "initial")

	primary := filepath.Join(tmp, "primary")
	mustGit(t, "", "clone", origin, primary)
	t.Chdir(primary)
	_, err := checkoutTask(context.Background(), "conveyor/task-not-pushed", "api", "not-pushed", filepath.Join(tmp, "human-task"))
	if err == nil || !strings.Contains(err.Error(), "checkout becomes available after the implementation agent pushes it") {
		t.Fatalf("checkout error = %v", err)
	}
	if gitRefExists(context.Background(), primary, "refs/heads/conveyor/task-not-pushed") {
		t.Fatal("checkout created the assigned task branch before the implementation agent pushed it")
	}
}

func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}
