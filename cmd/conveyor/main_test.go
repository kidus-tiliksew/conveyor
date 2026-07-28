package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

type gitFixture struct {
	tmp     string
	origin  string
	primary string
	seed    string
}

func TestAssignedCheckoutFromEnvironmentHonorsOnlyItsOwnTask(t *testing.T) {
	t.Setenv("CONVEYOR_TASK_ID", "task-1")
	t.Setenv("CONVEYOR_TASK_BRANCH", "conveyor/task-1")
	t.Setenv("CONVEYOR_TASK_BASE_BRANCH", "main")
	t.Setenv("CONVEYOR_TASK_REPO", "conveyor")

	branch, base, repo, ok := assignedCheckoutFromEnvironment("task-1")
	if !ok || branch != "conveyor/task-1" || base != "main" || repo != "conveyor" {
		t.Fatalf("assignment = %q %q %q ok=%v", branch, base, repo, ok)
	}
	if _, _, _, ok := assignedCheckoutFromEnvironment("task-2"); ok {
		t.Fatal("assignment for a different task must fall back to the authenticated lookup")
	}
	t.Setenv("CONVEYOR_TASK_BRANCH", "")
	if _, _, _, ok := assignedCheckoutFromEnvironment("task-1"); ok {
		t.Fatal("incomplete assignment must fall back to the authenticated lookup")
	}
}

func TestCheckoutCreatesMissingBranchFromFreshBaseWithoutTouchingPrimary(t *testing.T) {
	fixture := newGitFixture(t)
	primaryHead := mustGitOutput(t, fixture.primary, "rev-parse", "HEAD")
	fixture.advanceMain(t, "fresh-base.txt", "fresh base\n")

	got, err := checkoutTask(context.Background(), "conveyor/task-missing", "main", "conveyor", "missing", "")
	if err != nil {
		t.Fatal(err)
	}
	canonicalPrimary, err := filepath.EvalSymlinks(fixture.primary)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(filepath.Dir(canonicalPrimary), "conveyor-task-missing")
	if got != want {
		t.Fatalf("destination = %q, want %q", got, want)
	}
	assertFile(t, filepath.Join(got, "fresh-base.txt"))
	assertBranch(t, got, "conveyor/task-missing")
	if taskHead := mustGitOutput(t, got, "rev-parse", "HEAD"); taskHead != mustGitOutput(t, fixture.primary, "rev-parse", "origin/main") {
		t.Fatalf("task HEAD = %s, origin/main = %s", taskHead, mustGitOutput(t, fixture.primary, "rev-parse", "origin/main"))
	}
	assertPrimaryUntouched(t, fixture.primary, primaryHead)
}

func TestCheckoutReusesExistingLocalBranchWithoutReset(t *testing.T) {
	fixture := newGitFixture(t)
	branch := "conveyor/task-local"
	localHead := fixture.createLocalBranch(t, branch, "local-only.txt", "preserve me\n")
	destination := filepath.Join(fixture.tmp, "local-task")

	got, err := checkoutTask(context.Background(), branch, "main", "conveyor", "local", destination)
	if err != nil {
		t.Fatal(err)
	}
	if got != destination {
		t.Fatalf("destination = %q", got)
	}
	assertFile(t, filepath.Join(destination, "local-only.txt"))
	if head := mustGitOutput(t, destination, "rev-parse", "HEAD"); head != localHead {
		t.Fatalf("local task commit was reset: got %s want %s", head, localHead)
	}
}

func TestCheckoutTracksExistingRemoteBranch(t *testing.T) {
	fixture := newGitFixture(t)
	branch := "conveyor/task-remote"
	remoteHead := fixture.createRemoteBranch(t, branch, "remote-only.txt", "remote work\n")
	destination := filepath.Join(fixture.tmp, "override-path")

	got, err := checkoutTask(context.Background(), branch, "main", "conveyor", "remote", destination)
	if err != nil {
		t.Fatal(err)
	}
	if got != destination {
		t.Fatalf("destination = %q", got)
	}
	assertFile(t, filepath.Join(destination, "remote-only.txt"))
	if head := mustGitOutput(t, destination, "rev-parse", "HEAD"); head != remoteHead {
		t.Fatalf("remote task HEAD = %s, got %s", remoteHead, head)
	}
	if upstream := mustGitOutput(t, destination, "rev-parse", "--abbrev-ref", "@{upstream}"); upstream != "origin/"+branch {
		t.Fatalf("upstream = %q", upstream)
	}
}

func TestCheckoutFastForwardsAncestrySafeLocalBranch(t *testing.T) {
	fixture := newGitFixture(t)
	branch := "conveyor/task-fast-forward"
	mustGit(t, fixture.primary, "branch", branch, "origin/main")
	remoteHead := fixture.createRemoteBranch(t, branch, "remote-ahead.txt", "ahead\n")
	destination := filepath.Join(fixture.tmp, "fast-forward")

	if _, err := checkoutTask(context.Background(), branch, "main", "conveyor", "fast-forward", destination); err != nil {
		t.Fatal(err)
	}
	if head := mustGitOutput(t, destination, "rev-parse", "HEAD"); head != remoteHead {
		t.Fatalf("task branch did not fast-forward: got %s want %s", head, remoteHead)
	}
}

func TestCheckoutReusesOriginalRegisteredWorktreeAcrossRounds(t *testing.T) {
	fixture := newGitFixture(t)
	branch := "conveyor/task-review-round"
	first, err := checkoutTask(context.Background(), branch, "main", "conveyor", "review-round", "")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(first, "round-one.txt"), "round one\n")
	mustGit(t, first, "add", "round-one.txt")
	mustGit(t, first, "commit", "-m", "round one")
	firstHead := mustGitOutput(t, first, "rev-parse", "HEAD")

	second, err := checkoutTask(context.Background(), branch, "main", "conveyor", "review-round", filepath.Join(fixture.tmp, "ignored-override"))
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Fatalf("review round created a second worktree: first %q second %q", first, second)
	}
	if head := mustGitOutput(t, second, "rev-parse", "HEAD"); head != firstHead {
		t.Fatalf("review round lost existing commit: got %s want %s", head, firstHead)
	}
}

func TestCheckoutSupportsConcurrentTaskWorktreesAndPathOverride(t *testing.T) {
	fixture := newGitFixture(t)
	primaryHead := mustGitOutput(t, fixture.primary, "rev-parse", "HEAD")
	first, err := checkoutTask(context.Background(), "conveyor/task-one", "main", "conveyor", "one", "")
	if err != nil {
		t.Fatal(err)
	}
	override := filepath.Join(fixture.tmp, "custom-two")
	second, err := checkoutTask(context.Background(), "conveyor/task-two", "main", "conveyor", "two", override)
	if err != nil {
		t.Fatal(err)
	}
	if first == second || second != override {
		t.Fatalf("worktree paths = %q and %q", first, second)
	}
	assertBranch(t, first, "conveyor/task-one")
	assertBranch(t, second, "conveyor/task-two")
	assertPrimaryUntouched(t, fixture.primary, primaryHead)
}

func TestCheckoutRejectsUnsafeImplicitSiblingComponents(t *testing.T) {
	for _, test := range []struct {
		name   string
		repo   string
		taskID string
		want   string
	}{
		{name: "repository separator", repo: "../outside", taskID: "safe", want: "repository name"},
		{name: "repository backslash", repo: `..\outside`, taskID: "safe", want: "repository name"},
		{name: "repository traversal", repo: "..", taskID: "safe", want: "repository name"},
		{name: "task separator", repo: "conveyor", taskID: "../outside", want: "task ID"},
		{name: "task backslash", repo: "conveyor", taskID: `..\outside`, want: "task ID"},
		{name: "task traversal", repo: "conveyor", taskID: "..", want: "task ID"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newGitFixture(t)
			branch := "conveyor/task-unsafe-" + strings.ReplaceAll(test.name, " ", "-")
			_, err := checkoutTask(context.Background(), branch, "main", test.repo, test.taskID, "")
			if err == nil || !strings.Contains(err.Error(), "refusing implicit checkout destination") || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("checkout error = %v", err)
			}
			if gitRefExists(context.Background(), fixture.primary, "refs/heads/"+branch) {
				t.Fatal("task branch created after unsafe implicit destination failure")
			}
		})
	}
}

func TestCheckoutRejectsImplicitDestinationResolvedOutsideSiblingDirectory(t *testing.T) {
	fixture := newGitFixture(t)
	target := filepath.Join(fixture.tmp, "nested", "outside")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(fixture.tmp, "conveyor-task-symlink")
	if err := os.Symlink(target, destination); err != nil {
		t.Fatal(err)
	}

	_, err := checkoutTask(context.Background(), "conveyor/task-symlink", "main", "conveyor", "symlink", "")
	if err == nil || !strings.Contains(err.Error(), "resolved path is not a sibling") {
		t.Fatalf("checkout error = %v", err)
	}
	if gitRefExists(context.Background(), fixture.primary, "refs/heads/conveyor/task-symlink") {
		t.Fatal("task branch created after resolved sibling failure")
	}
}

func TestCheckoutPathOverrideBypassesImplicitSiblingGuard(t *testing.T) {
	fixture := newGitFixture(t)
	destination := filepath.Join(fixture.tmp, "operator-selected")
	got, err := checkoutTask(context.Background(), "conveyor/task-path-override", "main", "../malformed", "../malformed", destination)
	if err != nil {
		t.Fatal(err)
	}
	if got != destination {
		t.Fatalf("destination = %q, want %q", got, destination)
	}
}

func TestCheckoutFailsSafelyForDirtyInProgressAndExistingPaths(t *testing.T) {
	t.Run("dirty primary", func(t *testing.T) {
		fixture := newGitFixture(t)
		writeFile(t, filepath.Join(fixture.primary, "unrelated.txt"), "do not touch\n")
		destination := filepath.Join(fixture.tmp, "dirty-task")
		_, err := checkoutTask(context.Background(), "conveyor/task-dirty", "main", "conveyor", "dirty", destination)
		if err == nil || !strings.Contains(err.Error(), "primary checkout is unsafe") {
			t.Fatalf("checkout error = %v", err)
		}
		if _, statErr := os.Stat(destination); !os.IsNotExist(statErr) {
			t.Fatalf("destination created after dirty-state failure: %v", statErr)
		}
		if gitRefExists(context.Background(), fixture.primary, "refs/heads/conveyor/task-dirty") {
			t.Fatal("task branch created after dirty-state failure")
		}
	})

	t.Run("in-progress operation", func(t *testing.T) {
		fixture := newGitFixture(t)
		marker := mustGitOutput(t, fixture.primary, "rev-parse", "--git-path", "sequencer")
		if !filepath.IsAbs(marker) {
			marker = filepath.Join(fixture.primary, marker)
		}
		if err := os.MkdirAll(marker, 0o755); err != nil {
			t.Fatal(err)
		}
		_, err := checkoutTask(context.Background(), "conveyor/task-operation", "main", "conveyor", "operation", filepath.Join(fixture.tmp, "operation-task"))
		if err == nil || !strings.Contains(err.Error(), "Git operation sequencer is in progress") {
			t.Fatalf("checkout error = %v", err)
		}
	})

	t.Run("unregistered destination", func(t *testing.T) {
		fixture := newGitFixture(t)
		destination := filepath.Join(fixture.tmp, "occupied")
		if err := os.Mkdir(destination, 0o755); err != nil {
			t.Fatal(err)
		}
		_, err := checkoutTask(context.Background(), "conveyor/task-occupied", "main", "conveyor", "occupied", destination)
		if err == nil || !strings.Contains(err.Error(), "already exists") {
			t.Fatalf("checkout error = %v", err)
		}
	})
}

func TestCheckoutRejectsDivergentLocalAndRemoteTaskBranches(t *testing.T) {
	fixture := newGitFixture(t)
	branch := "conveyor/task-diverged"
	localHead := fixture.createLocalBranch(t, branch, "local.txt", "local\n")
	fixture.createRemoteBranch(t, branch, "remote.txt", "remote\n")
	destination := filepath.Join(fixture.tmp, "diverged")

	_, err := checkoutTask(context.Background(), branch, "main", "conveyor", "diverged", destination)
	if err == nil || !strings.Contains(err.Error(), "diverged") {
		t.Fatalf("checkout error = %v", err)
	}
	if head := mustGitOutput(t, fixture.primary, "rev-parse", branch); head != localHead {
		t.Fatalf("divergent local branch changed: got %s want %s", head, localHead)
	}
	if _, statErr := os.Stat(destination); !os.IsNotExist(statErr) {
		t.Fatalf("destination created for divergent branch: %v", statErr)
	}
}

func TestCheckoutRejectsDirtyRegisteredTaskWorktree(t *testing.T) {
	newGitFixture(t)
	branch := "conveyor/task-dirty-target"
	destination, err := checkoutTask(context.Background(), branch, "main", "conveyor", "dirty-target", "")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(destination, "uncommitted.txt"), "keep\n")
	_, err = checkoutTask(context.Background(), branch, "main", "conveyor", "dirty-target", "")
	if err == nil || !strings.Contains(err.Error(), "task worktree") || !strings.Contains(err.Error(), "uncommitted") {
		t.Fatalf("checkout error = %v", err)
	}
	assertFile(t, filepath.Join(destination, "uncommitted.txt"))
}

func TestCheckoutRejectsAssignedBranchInSharedPrimaryCheckout(t *testing.T) {
	fixture := newGitFixture(t)
	branch := "conveyor/task-primary"
	mustGit(t, fixture.primary, "checkout", "-b", branch, "origin/main")
	primaryHead := mustGitOutput(t, fixture.primary, "rev-parse", "HEAD")

	_, err := checkoutTask(context.Background(), branch, "main", "conveyor", "primary", "")
	if err == nil || !strings.Contains(err.Error(), "shared primary checkout") {
		t.Fatalf("checkout error = %v", err)
	}
	assertBranch(t, fixture.primary, branch)
	if head := mustGitOutput(t, fixture.primary, "rev-parse", "HEAD"); head != primaryHead {
		t.Fatalf("primary task branch changed: got %s want %s", head, primaryHead)
	}
}

func TestCheckoutAdoptsExplicitDedicatedClone(t *testing.T) {
	fixture := newGitFixture(t)
	branch := "conveyor/task-dedicated-clone"
	mustGit(t, fixture.primary, "checkout", "-b", branch, "origin/main")

	got, err := checkoutTask(context.Background(), branch, "main", "conveyor", "dedicated-clone", fixture.primary)
	if err != nil {
		t.Fatal(err)
	}
	canonicalPrimary, err := filepath.EvalSymlinks(fixture.primary)
	if err != nil {
		t.Fatal(err)
	}
	if got != canonicalPrimary {
		t.Fatalf("dedicated clone path = %q, want %q", got, canonicalPrimary)
	}
}

func TestDoneCleansOnlyClosedTasksAndRetainsBranches(t *testing.T) {
	fixture := newGitFixture(t)
	branch := "conveyor/task-cleanup"
	destination, err := checkoutTask(context.Background(), branch, "main", "conveyor", "cleanup", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := removeTaskWorktree(context.Background(), branch, core.TaskRunning); err == nil || !strings.Contains(err.Error(), "merged or closed") {
		t.Fatalf("active cleanup error = %v", err)
	}
	result, err := removeTaskWorktree(context.Background(), branch, core.TaskMerged)
	if err != nil {
		t.Fatal(err)
	}
	if result.Worktree != "removed" || result.Branch != "retained" || result.Path != destination {
		t.Fatalf("cleanup result = %+v", result)
	}
	if _, err := os.Stat(destination); !os.IsNotExist(err) {
		t.Fatalf("worktree still exists: %v", err)
	}
	if !gitRefExists(context.Background(), fixture.primary, "refs/heads/"+branch) {
		t.Fatal("cleanup deleted the task branch")
	}
	result, err = removeTaskWorktree(context.Background(), branch, core.TaskMerged)
	if err != nil {
		t.Fatal(err)
	}
	if result.Worktree != "skipped" || result.Branch != "retained" {
		t.Fatalf("idempotent cleanup result = %+v", result)
	}
}

func TestDoneRefusesDirtyWorktreeAndHandlesMissingDirectory(t *testing.T) {
	newGitFixture(t)
	dirtyBranch := "conveyor/task-dirty-cleanup"
	dirtyPath, err := checkoutTask(context.Background(), dirtyBranch, "main", "conveyor", "dirty-cleanup", "")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dirtyPath, "dirty.txt"), "keep\n")
	if _, err := removeTaskWorktree(context.Background(), dirtyBranch, core.TaskClosed); err == nil || !strings.Contains(err.Error(), "uncommitted") {
		t.Fatalf("dirty cleanup error = %v", err)
	}
	assertFile(t, filepath.Join(dirtyPath, "dirty.txt"))

	missingBranch := "conveyor/task-missing-directory"
	missingPath, err := checkoutTask(context.Background(), missingBranch, "main", "conveyor", "missing-directory", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(missingPath); err != nil {
		t.Fatal(err)
	}
	result, err := removeTaskWorktree(context.Background(), missingBranch, core.TaskClosed)
	if err != nil {
		t.Fatal(err)
	}
	if result.Worktree != "removed" || result.Branch != "retained" {
		t.Fatalf("missing-directory cleanup result = %+v", result)
	}
}

func newGitFixture(t *testing.T) gitFixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	origin := filepath.Join(tmp, "origin.git")
	seed := filepath.Join(tmp, "seed")
	primary := filepath.Join(tmp, "primary")
	mustGit(t, "", "init", "--bare", "--initial-branch=main", origin)
	mustGit(t, "", "init", "-b", "main", seed)
	configureGitUser(t, seed)
	writeFile(t, filepath.Join(seed, "README.md"), "main\n")
	mustGit(t, seed, "add", ".")
	mustGit(t, seed, "commit", "-m", "initial")
	mustGit(t, seed, "remote", "add", "origin", origin)
	mustGit(t, seed, "push", "-u", "origin", "main")
	mustGit(t, "", "clone", origin, primary)
	configureGitUser(t, primary)
	t.Chdir(primary)
	return gitFixture{tmp: tmp, origin: origin, primary: primary, seed: seed}
}

func (f gitFixture) advanceMain(t *testing.T, name, contents string) string {
	t.Helper()
	writeFile(t, filepath.Join(f.seed, name), contents)
	mustGit(t, f.seed, "add", name)
	mustGit(t, f.seed, "commit", "-m", "advance main")
	mustGit(t, f.seed, "push", "origin", "main")
	return mustGitOutput(t, f.seed, "rev-parse", "HEAD")
}

func (f gitFixture) createLocalBranch(t *testing.T, branch, name, contents string) string {
	t.Helper()
	path := filepath.Join(f.tmp, strings.ReplaceAll(branch, "/", "-")+"-local")
	mustGit(t, f.primary, "fetch", "origin", "main")
	mustGit(t, f.primary, "worktree", "add", "-b", branch, path, "origin/main")
	writeFile(t, filepath.Join(path, name), contents)
	mustGit(t, path, "add", name)
	mustGit(t, path, "commit", "-m", "local task commit")
	head := mustGitOutput(t, path, "rev-parse", "HEAD")
	mustGit(t, f.primary, "worktree", "remove", path)
	return head
}

func (f gitFixture) createRemoteBranch(t *testing.T, branch, name, contents string) string {
	t.Helper()
	path := filepath.Join(f.tmp, strings.ReplaceAll(branch, "/", "-")+"-remote")
	mustGit(t, "", "clone", f.origin, path)
	configureGitUser(t, path)
	mustGit(t, path, "checkout", "-b", branch, "origin/main")
	writeFile(t, filepath.Join(path, name), contents)
	mustGit(t, path, "add", name)
	mustGit(t, path, "commit", "-m", "remote task commit")
	mustGit(t, path, "push", "-u", "origin", branch)
	return mustGitOutput(t, path, "rev-parse", "HEAD")
}

func configureGitUser(t *testing.T, dir string) {
	t.Helper()
	mustGit(t, dir, "config", "user.name", "test")
	mustGit(t, dir, "config", "user.email", "test@example.com")
}

func assertPrimaryUntouched(t *testing.T, primary, wantHead string) {
	t.Helper()
	assertBranch(t, primary, "main")
	if head := mustGitOutput(t, primary, "rev-parse", "HEAD"); head != wantHead {
		t.Fatalf("primary HEAD changed: got %s want %s", head, wantHead)
	}
	if status := mustGitOutput(t, primary, "status", "--porcelain"); status != "" {
		t.Fatalf("primary files changed: %s", status)
	}
}

func assertBranch(t *testing.T, dir, want string) {
	t.Helper()
	if got := mustGitOutput(t, dir, "branch", "--show-current"); got != want {
		t.Fatalf("branch = %q, want %q", got, want)
	}
}

func assertFile(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected file %s: %v", path, err)
	}
}

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustGitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	out, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	_ = mustGitOutput(t, dir, args...)
}
