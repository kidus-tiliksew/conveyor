package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
)

type gitFixture struct {
	tmp     string
	origin  string
	primary string
	seed    string
}

func TestCheckoutRepositoryIdentityAppliesBeforeMutationAndPathOverride(t *testing.T) {
	fixture := newGitFixture(t)
	destination := filepath.Join(fixture.tmp, "operator-selected")
	otherRepository := filepath.Join(fixture.tmp, "other-origin.git")

	_, err := checkoutTask(context.Background(), "conveyor/task-mismatch", "main", "assigned-repo", otherRepository, "mismatch", destination)
	if err == nil || !strings.Contains(err.Error(), "repository identity mismatch") ||
		!strings.Contains(err.Error(), "assigned-repo") || !strings.Contains(err.Error(), "current repository") {
		t.Fatalf("checkout error = %v", err)
	}
	if gitRefExists(context.Background(), fixture.primary, "refs/heads/conveyor/task-mismatch") {
		t.Fatal("task branch created before repository identity rejection")
	}
	if _, statErr := os.Stat(destination); !os.IsNotExist(statErr) {
		t.Fatalf("explicit destination created before repository identity rejection: %v", statErr)
	}
}

func TestCheckoutAcceptsEquivalentConfiguredOriginForms(t *testing.T) {
	fixture := newGitFixture(t)
	destination := filepath.Join(fixture.tmp, "equivalent-origin")

	got, err := checkoutTask(context.Background(), "conveyor/task-equivalent", "main", "conveyor", "file://"+fixture.origin, "equivalent", destination)
	if err != nil {
		t.Fatal(err)
	}
	if got != destination {
		t.Fatalf("destination = %q, want %q", got, destination)
	}
}

func TestCheckoutCreatesMissingBranchFromFreshBaseWithoutTouchingPrimary(t *testing.T) {
	fixture := newGitFixture(t)
	t.Setenv("HOME", fixture.tmp)
	primaryHead := mustGitOutput(t, fixture.primary, "rev-parse", "HEAD")
	fixture.advanceMain(t, "fresh-base.txt", "fresh base\n")

	got, err := checkoutTask(context.Background(), "conveyor/task-missing", "main", "conveyor", fixture.origin, "missing", "")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(fixture.tmp, ".conveyor", "worktrees", "conveyor-task-missing")
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

	got, err := checkoutTask(context.Background(), branch, "main", "conveyor", fixture.origin, "local", destination)
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

	got, err := checkoutTask(context.Background(), branch, "main", "conveyor", fixture.origin, "remote", destination)
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

	if _, err := checkoutTask(context.Background(), branch, "main", "conveyor", fixture.origin, "fast-forward", destination); err != nil {
		t.Fatal(err)
	}
	if head := mustGitOutput(t, destination, "rev-parse", "HEAD"); head != remoteHead {
		t.Fatalf("task branch did not fast-forward: got %s want %s", head, remoteHead)
	}
}

func TestCheckoutReusesOriginalRegisteredWorktreeAcrossRounds(t *testing.T) {
	fixture := newGitFixture(t)
	branch := "conveyor/task-review-round"
	first, err := checkoutTask(context.Background(), branch, "main", "conveyor", fixture.origin, "review-round", "")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(first, "round-one.txt"), "round one\n")
	mustGit(t, first, "add", "round-one.txt")
	mustGit(t, first, "commit", "-m", "round one")
	firstHead := mustGitOutput(t, first, "rev-parse", "HEAD")

	second, err := checkoutTask(context.Background(), branch, "main", "conveyor", fixture.origin, "review-round", filepath.Join(fixture.tmp, "ignored-override"))
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

func TestCheckoutReusesRegisteredWorktreeAtFormerSiblingLocation(t *testing.T) {
	fixture := newGitFixture(t)
	branch := "conveyor/task-legacy-location"
	legacyPath := filepath.Join(fixture.tmp, "conveyor-worktrees", "conveyor-task-legacy-location")
	mustGit(t, fixture.primary, "worktree", "add", "-b", branch, legacyPath, "origin/main")

	got, err := checkoutTask(context.Background(), branch, "main", "conveyor", fixture.origin, "legacy-location", "")
	if err != nil {
		t.Fatal(err)
	}
	if got != legacyPath {
		t.Fatalf("registered legacy worktree = %q, want %q", got, legacyPath)
	}
	if _, err := os.Stat(filepath.Join(fixture.tmp, ".conveyor", "worktrees", "conveyor-task-legacy-location")); !os.IsNotExist(err) {
		t.Fatalf("checkout created a replacement worktree under the new root: %v", err)
	}
	cleanup, err := removeTaskWorktree(context.Background(), branch, core.TaskMerged)
	if err != nil {
		t.Fatal(err)
	}
	if cleanup.Worktree != "removed" || cleanup.Path != legacyPath {
		t.Fatalf("legacy cleanup result = %+v", cleanup)
	}
}

func TestCheckoutSupportsConcurrentTaskWorktreesAndPathOverride(t *testing.T) {
	fixture := newGitFixture(t)
	root := filepath.Join(fixture.tmp, "configured-worktrees")
	primaryHead := mustGitOutput(t, fixture.primary, "rev-parse", "HEAD")
	first, err := checkoutTaskAtRoot(context.Background(), "conveyor/task-one", "main", "conveyor", fixture.origin, "one", "", root)
	if err != nil {
		t.Fatal(err)
	}
	override := filepath.Join(fixture.tmp, "custom-two")
	second, err := checkoutTaskAtRoot(context.Background(), "conveyor/task-two", "main", "conveyor", fixture.origin, "two", override, "relative-root-is-ignored")
	if err != nil {
		t.Fatal(err)
	}
	if first != filepath.Join(root, "conveyor-task-one") || second != override {
		t.Fatalf("worktree paths = %q and %q", first, second)
	}
	assertBranch(t, first, "conveyor/task-one")
	assertBranch(t, second, "conveyor/task-two")
	assertPrimaryUntouched(t, fixture.primary, primaryHead)
}

func TestCheckoutRejectsUnsafeImplicitContainerComponents(t *testing.T) {
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
			_, err := checkoutTask(context.Background(), branch, "main", test.repo, fixture.origin, test.taskID, "")
			if err == nil || !strings.Contains(err.Error(), "refusing implicit checkout destination") || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("checkout error = %v", err)
			}
			if gitRefExists(context.Background(), fixture.primary, "refs/heads/"+branch) {
				t.Fatal("task branch created after unsafe implicit destination failure")
			}
		})
	}
}

func TestCheckoutRejectsImplicitDestinationResolvedOutsideContainer(t *testing.T) {
	fixture := newGitFixture(t)
	target := filepath.Join(fixture.tmp, "nested", "outside")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	container := filepath.Join(fixture.tmp, "conveyor-worktrees")
	if err := os.Mkdir(container, 0o755); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(container, "conveyor-task-symlink")
	if err := os.Symlink(target, destination); err != nil {
		t.Fatal(err)
	}

	_, err := checkoutTaskAtRoot(context.Background(), "conveyor/task-symlink", "main", "conveyor", fixture.origin, "symlink", "", container)
	if err == nil || !strings.Contains(err.Error(), "resolved path is not inside canonical container") {
		t.Fatalf("checkout error = %v", err)
	}
	if gitRefExists(context.Background(), fixture.primary, "refs/heads/conveyor/task-symlink") {
		t.Fatal("task branch created after resolved sibling failure")
	}
}

func TestCheckoutRejectsSymlinkedImplicitContainer(t *testing.T) {
	fixture := newGitFixture(t)
	outside := filepath.Join(fixture.tmp, "outside-container")
	if err := os.Mkdir(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	container := filepath.Join(fixture.tmp, "configured-worktrees")
	if err := os.Symlink(outside, container); err != nil {
		t.Fatal(err)
	}

	_, err := checkoutTaskAtRoot(context.Background(), "conveyor/task-container-symlink", "main", "conveyor", fixture.origin, "container-symlink", "", container)
	if err == nil || !strings.Contains(err.Error(), "worktree root") || !strings.Contains(err.Error(), "canonical path") {
		t.Fatalf("checkout error = %v", err)
	}
	if gitRefExists(context.Background(), fixture.primary, "refs/heads/conveyor/task-container-symlink") {
		t.Fatal("task branch created after symlinked container rejection")
	}
}

func TestCheckoutRejectsSymlinkedDefaultWorktreeRoot(t *testing.T) {
	fixture := newGitFixture(t)
	outside := filepath.Join(fixture.tmp, "outside-default-root")
	if err := os.Mkdir(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	conveyorHome := filepath.Join(fixture.tmp, ".conveyor")
	if err := os.Mkdir(conveyorHome, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(conveyorHome, "worktrees")); err != nil {
		t.Fatal(err)
	}

	_, err := checkoutTask(context.Background(), "conveyor/task-default-root-symlink", "main", "conveyor", fixture.origin, "default-root-symlink", "")
	if err == nil || !strings.Contains(err.Error(), "worktree root") || !strings.Contains(err.Error(), "canonical path") {
		t.Fatalf("checkout error = %v", err)
	}
	if gitRefExists(context.Background(), fixture.primary, "refs/heads/conveyor/task-default-root-symlink") {
		t.Fatal("task branch created after symlinked default root rejection")
	}
}

func TestCheckoutRejectsRelativeConfiguredWorktreeRoot(t *testing.T) {
	fixture := newGitFixture(t)
	_, err := checkoutTaskAtRoot(context.Background(), "conveyor/task-relative-root", "main", "conveyor", fixture.origin, "relative-root", "", "relative/worktrees")
	if err == nil || !strings.Contains(err.Error(), "worktree root") || !strings.Contains(err.Error(), "not absolute") {
		t.Fatalf("checkout error = %v", err)
	}
	if gitRefExists(context.Background(), fixture.primary, "refs/heads/conveyor/task-relative-root") {
		t.Fatal("task branch created after relative worktree root rejection")
	}
}

func TestCheckoutPathOverrideBypassesImplicitContainerGuard(t *testing.T) {
	fixture := newGitFixture(t)
	destination := filepath.Join(fixture.tmp, "operator-selected")
	got, err := checkoutTask(context.Background(), "conveyor/task-path-override", "main", "../malformed", fixture.origin, "../malformed", destination)
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
		_, err := checkoutTask(context.Background(), "conveyor/task-dirty", "main", "conveyor", fixture.origin, "dirty", destination)
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
		_, err := checkoutTask(context.Background(), "conveyor/task-operation", "main", "conveyor", fixture.origin, "operation", filepath.Join(fixture.tmp, "operation-task"))
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
		_, err := checkoutTask(context.Background(), "conveyor/task-occupied", "main", "conveyor", fixture.origin, "occupied", destination)
		if err == nil || !strings.Contains(err.Error(), "already exists") {
			t.Fatalf("checkout error = %v", err)
		}
	})
}

func TestCheckoutIgnoresHarnessScratchButRejectsOtherExhaust(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source path")
	}
	ignore, err := os.ReadFile(filepath.Join(filepath.Dir(currentFile), "..", "..", ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}

	fixture := newGitFixture(t)
	writeFile(t, filepath.Join(fixture.primary, ".gitignore"), string(ignore))
	mustGit(t, fixture.primary, "add", ".gitignore")
	mustGit(t, fixture.primary, "commit", "-m", "test fixture ignores")
	mustGit(t, fixture.primary, "push", "origin", "main")

	if err := os.Mkdir(filepath.Join(fixture.primary, ".codex-go-cache"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(fixture.primary, ".codex-go-cache", "entry"), "ignored\n")
	branch := "conveyor/task-ignored-harness-scratch"
	destination, err := checkoutTask(context.Background(), branch, "main", "conveyor", fixture.origin, "ignored-harness-scratch", "")
	if err != nil {
		t.Fatalf("ignored primary scratch blocked checkout: %v", err)
	}

	if err := os.Mkdir(filepath.Join(destination, ".codex-go-cache"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(destination, ".codex-go-cache", "entry"), "ignored\n")
	if _, err = checkoutTask(context.Background(), branch, "main", "conveyor", fixture.origin, "ignored-harness-scratch", ""); err != nil {
		t.Fatalf("ignored task-worktree scratch blocked checkout: %v", err)
	}

	if err := os.Mkdir(filepath.Join(destination, "unignored-exhaust"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(destination, "unignored-exhaust", "entry"), "visible\n")
	if _, err = checkoutTask(context.Background(), branch, "main", "conveyor", fixture.origin, "ignored-harness-scratch", ""); err == nil || !strings.Contains(err.Error(), "uncommitted") {
		t.Fatalf("unignored task-worktree exhaust was accepted: %v", err)
	}
}

func TestCheckoutRejectsDivergentLocalAndRemoteTaskBranches(t *testing.T) {
	fixture := newGitFixture(t)
	branch := "conveyor/task-diverged"
	localHead := fixture.createLocalBranch(t, branch, "local.txt", "local\n")
	fixture.createRemoteBranch(t, branch, "remote.txt", "remote\n")
	destination := filepath.Join(fixture.tmp, "diverged")

	_, err := checkoutTask(context.Background(), branch, "main", "conveyor", fixture.origin, "diverged", destination)
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
	fixture := newGitFixture(t)
	branch := "conveyor/task-dirty-target"
	destination, err := checkoutTask(context.Background(), branch, "main", "conveyor", fixture.origin, "dirty-target", "")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(destination, "uncommitted.txt"), "keep\n")
	_, err = checkoutTask(context.Background(), branch, "main", "conveyor", fixture.origin, "dirty-target", "")
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

	_, err := checkoutTask(context.Background(), branch, "main", "conveyor", fixture.origin, "primary", "")
	if err == nil || !strings.Contains(err.Error(), "shared primary checkout") {
		t.Fatalf("checkout error = %v", err)
	}
	assertBranch(t, fixture.primary, branch)
	if head := mustGitOutput(t, fixture.primary, "rev-parse", "HEAD"); head != primaryHead {
		t.Fatalf("primary task branch changed: got %s want %s", head, primaryHead)
	}
}

// A dirty primary that holds the task branch is refused before any fetch or
// worktree creation, and its branch, HEAD, and files stay as they were.
func TestCheckoutRefusesTaskBranchHeldByDirtyPrimary(t *testing.T) {
	fixture := newGitFixture(t)
	branch := "conveyor/task-dirty-primary-branch"
	mustGit(t, fixture.primary, "checkout", "-b", branch, "origin/main")
	primaryHead := mustGitOutput(t, fixture.primary, "rev-parse", "HEAD")
	writeFile(t, filepath.Join(fixture.primary, "README.md"), "operator edit\n")
	writeFile(t, filepath.Join(fixture.primary, "notes.txt"), "operator notes\n")
	status := mustGitOutput(t, fixture.primary, "status", "--porcelain", "--untracked-files=normal")
	destination := filepath.Join(fixture.tmp, "dirty-primary-branch")

	_, err := checkoutTask(context.Background(), branch, "main", "conveyor", fixture.origin, "dirty-primary-branch", destination)
	if err == nil || !strings.Contains(err.Error(), "primary checkout") {
		t.Fatalf("checkout error = %v", err)
	}
	assertBranch(t, fixture.primary, branch)
	if head := mustGitOutput(t, fixture.primary, "rev-parse", "HEAD"); head != primaryHead {
		t.Fatalf("primary HEAD changed: got %s want %s", head, primaryHead)
	}
	if got := mustGitOutput(t, fixture.primary, "status", "--porcelain", "--untracked-files=normal"); got != status {
		t.Fatalf("primary status changed: got %q want %q", got, status)
	}
	if _, statErr := os.Stat(destination); !os.IsNotExist(statErr) {
		t.Fatalf("destination created for a branch held by the primary: %v", statErr)
	}
	if worktrees := mustGitOutput(t, fixture.primary, "worktree", "list", "--porcelain"); strings.Count(worktrees, "worktree ") != 1 {
		t.Fatalf("worktree registered for a branch held by the primary:\n%s", worktrees)
	}
}

// A conflicted merge leaves the primary both dirty and mid-operation. The
// in-progress refusal is checked first and names the operation, so it does
// not depend on the dirty-primary refusal.
func TestCheckoutNamesInProgressMergeInDirtyPrimary(t *testing.T) {
	fixture := newGitFixture(t)
	mustGit(t, fixture.primary, "checkout", "-b", "side")
	writeFile(t, filepath.Join(fixture.primary, "README.md"), "side\n")
	mustGit(t, fixture.primary, "commit", "-am", "side")
	mustGit(t, fixture.primary, "checkout", "main")
	writeFile(t, filepath.Join(fixture.primary, "README.md"), "main edit\n")
	mustGit(t, fixture.primary, "commit", "-am", "main edit")
	merge := exec.Command("git", "merge", "side")
	merge.Dir = fixture.primary
	if out, err := merge.CombinedOutput(); err == nil {
		t.Fatalf("merge unexpectedly succeeded: %s", out)
	}
	conflicted, err := os.ReadFile(filepath.Join(fixture.primary, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	branch := "conveyor/task-merge-in-progress"
	destination := filepath.Join(fixture.tmp, "merge-in-progress")

	_, err = checkoutTask(context.Background(), branch, "main", "conveyor", fixture.origin, "merge-in-progress", destination)
	if err == nil || !strings.Contains(err.Error(), "Git operation MERGE_HEAD is in progress") {
		t.Fatalf("checkout error = %v", err)
	}
	if !gitRefExists(context.Background(), fixture.primary, "MERGE_HEAD") {
		t.Fatal("primary merge state was cleared")
	}
	if after, err := os.ReadFile(filepath.Join(fixture.primary, "README.md")); err != nil || string(after) != string(conflicted) {
		t.Fatalf("primary conflicted file changed: %q (%v)", after, err)
	}
	if gitRefExists(context.Background(), fixture.primary, "refs/heads/"+branch) {
		t.Fatal("task branch created during an in-progress primary merge")
	}
	if _, statErr := os.Stat(destination); !os.IsNotExist(statErr) {
		t.Fatalf("destination created during an in-progress primary merge: %v", statErr)
	}
}

func TestCheckoutAdoptsExplicitDedicatedClone(t *testing.T) {
	fixture := newGitFixture(t)
	branch := "conveyor/task-dedicated-clone"
	dedicated := filepath.Join(fixture.tmp, "dedicated-clone")
	mustGit(t, fixture.primary, "worktree", "add", "-b", branch, dedicated, "origin/main")

	got, err := checkoutTask(context.Background(), branch, "main", "conveyor", fixture.origin, "dedicated-clone", dedicated)
	if err != nil {
		t.Fatal(err)
	}
	if got != dedicated {
		t.Fatalf("dedicated clone path = %q, want %q", got, dedicated)
	}
}

func TestCheckoutRefusesPrimaryCheckoutAsDestination(t *testing.T) {
	fixture := newGitFixture(t)
	_, err := checkoutTask(context.Background(), "conveyor/task-primary-dest", "main", "conveyor", fixture.origin, "primary-dest", fixture.primary)
	if err == nil || !strings.Contains(err.Error(), "primary checkout") {
		t.Fatalf("checkout error = %v", err)
	}
}

func TestCheckoutReusesAssignedWorktreeWithDirtyPrimary(t *testing.T) {
	fixture := newGitFixture(t)
	branch := "conveyor/task-reuse-dirty-primary"
	path, err := checkoutTask(context.Background(), branch, "main", "conveyor", fixture.origin, "reuse-dirty-primary", "")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(fixture.primary, "unrelated.txt"), "primary dirt\n")
	got, err := checkoutTask(context.Background(), branch, "main", "conveyor", fixture.origin, "reuse-dirty-primary", "")
	if err != nil {
		t.Fatalf("reuse with dirty primary: %v", err)
	}
	if got != path {
		t.Fatalf("reuse path = %q want %q", got, path)
	}
}

func TestCheckoutReusesAssignedWorktreeBlockedByPrimarySequencer(t *testing.T) {
	fixture := newGitFixture(t)
	branch := "conveyor/task-reuse-sequencer"
	if _, err := checkoutTask(context.Background(), branch, "main", "conveyor", fixture.origin, "reuse-sequencer", ""); err != nil {
		t.Fatal(err)
	}
	marker := mustGitOutput(t, fixture.primary, "rev-parse", "--git-path", "sequencer")
	if !filepath.IsAbs(marker) {
		marker = filepath.Join(fixture.primary, marker)
	}
	if err := os.MkdirAll(marker, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := checkoutTask(context.Background(), branch, "main", "conveyor", fixture.origin, "reuse-sequencer", "")
	if err == nil || !strings.Contains(err.Error(), "Git operation sequencer is in progress") {
		t.Fatalf("checkout error = %v", err)
	}
}

func TestCheckoutCustomAssignmentUsesDistinctImplicitDestination(t *testing.T) {
	fixture := newGitFixture(t)
	taskID := "custom-dest"
	previous, err := checkoutTask(context.Background(), "conveyor/task-custom-dest", "main", "conveyor", fixture.origin, taskID, "")
	if err != nil {
		t.Fatal(err)
	}
	custom, err := checkoutTask(context.Background(), "feature/adopt", "main", "conveyor", fixture.origin, taskID, "")
	if err != nil {
		t.Fatal(err)
	}
	wantCustom := filepath.Join(filepath.Dir(previous), "conveyor-task-custom-dest-feature-adopt")
	if custom != wantCustom {
		t.Fatalf("custom dest = %q want %q", custom, wantCustom)
	}
	if custom == previous {
		t.Fatal("custom assignment reused previous-name worktree")
	}
	if _, err := os.Stat(previous); err != nil {
		t.Fatalf("previous-name worktree was removed: %v", err)
	}
	if !gitRefExists(context.Background(), fixture.primary, "refs/heads/conveyor/task-custom-dest") {
		t.Fatal("previous-name branch was deleted")
	}
}

func TestCheckoutRefusesDestinationOccupiedByOtherBranch(t *testing.T) {
	fixture := newGitFixture(t)
	occupied := filepath.Join(fixture.tmp, "occupied-dest")
	mustGit(t, fixture.primary, "worktree", "add", "-b", "other/branch", occupied, "origin/main")
	_, err := checkoutTask(context.Background(), "conveyor/task-collision", "main", "conveyor", fixture.origin, "collision", occupied)
	if err == nil || !strings.Contains(err.Error(), "already a registered worktree") || !strings.Contains(err.Error(), "other/branch") {
		t.Fatalf("checkout error = %v", err)
	}
}

func TestImplicitCheckoutDestinationKeepsDefaultAndSanitizesCustom(t *testing.T) {
	root := filepath.Join(t.TempDir(), "worktrees")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := implicitCheckoutDestination(root, "conveyor", "task-1", "conveyor/task-task-1")
	if err != nil || got != filepath.Join(root, "conveyor-task-task-1") {
		t.Fatalf("default dest = %q err=%v", got, err)
	}
	got, err = implicitCheckoutDestination(root, "conveyor", "task-1", "feature/adopt")
	if err != nil || got != filepath.Join(root, "conveyor-task-task-1-feature-adopt") {
		t.Fatalf("custom dest = %q err=%v", got, err)
	}
}
func TestOtherOpenTaskHoldingBranch(t *testing.T) {
	tasks := []core.Task{
		{ID: "self", Repo: "conveyor", Branch: "feature/x", State: core.TaskQueued},
		{ID: "done", Repo: "conveyor", Branch: "feature/x", State: core.TaskMerged},
		{ID: "other-repo", Repo: "other", Branch: "feature/x", State: core.TaskQueued},
		{ID: "open", Repo: "conveyor", Branch: "feature/x", State: core.TaskQueued},
	}
	other, occupied := otherOpenTaskHoldingBranch(tasks, "self", "conveyor", "feature/x")
	if !occupied || other != "open" {
		t.Fatalf("occupancy = %q %v", other, occupied)
	}
	if other, occupied := otherOpenTaskHoldingBranch(tasks, "self", "conveyor", "feature/missing"); occupied || other != "" {
		t.Fatalf("missing branch occupancy = %q %v", other, occupied)
	}
}

func TestMaybeAttachOperatorCheckoutFromNonPrimaryThenAdopt(t *testing.T) {
	fixture := newGitFixture(t)
	path := filepath.Join(fixture.tmp, "operator-tree")
	mustGit(t, fixture.primary, "worktree", "add", "-b", "feature/adopt", path, "origin/main")
	t.Chdir(path)

	var posts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/tasks/task-1/branch" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["branch"] != "feature/adopt" {
			t.Fatalf("branch = %q", body["branch"])
		}
		posts++
		_ = json.NewEncoder(w).Encode(core.Task{ID: "task-1", Branch: "feature/adopt"})
	}))
	defer server.Close()

	assigned := "conveyor/task-task-1"
	got, err := maybeAttachOperatorCheckout(context.Background(), &client{base: server.URL, token: "secret-token", workspace: "demo"}, "task-1", assigned, "conveyor", fixture.origin)
	if err != nil || got != "feature/adopt" || posts != 1 {
		t.Fatalf("attach = %q posts=%d err=%v", got, posts, err)
	}
	adopted, err := checkoutTask(context.Background(), got, "main", "conveyor", fixture.origin, "task-1", "")
	if err != nil {
		t.Fatal(err)
	}
	if adopted != path {
		t.Fatalf("adopted = %q want %q", adopted, path)
	}
	previousName := filepath.Join(fixture.tmp, ".conveyor", "worktrees", "conveyor-task-task-1")
	if _, err := os.Stat(previousName); !os.IsNotExist(err) {
		t.Fatalf("created previous-name worktree: %v", err)
	}
}

func TestMaybeAttachOperatorCheckoutSkipsPrimaryAndMatchingHead(t *testing.T) {
	fixture := newGitFixture(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("unexpected attach request %s %s", r.Method, r.URL.Path)
	}))
	defer server.Close()
	c := &client{base: server.URL, token: "secret-token", workspace: "demo"}
	assigned := "conveyor/task-head-eq"
	got, err := maybeAttachOperatorCheckout(context.Background(), c, "head-eq", assigned, "conveyor", fixture.origin)
	if err != nil || got != assigned {
		t.Fatalf("primary skip = %q err=%v", got, err)
	}

	path := filepath.Join(fixture.tmp, "matching-head")
	mustGit(t, fixture.primary, "worktree", "add", "-b", assigned, path, "origin/main")
	t.Chdir(path)
	got, err = maybeAttachOperatorCheckout(context.Background(), c, "head-eq", assigned, "conveyor", fixture.origin)
	if err != nil || got != assigned {
		t.Fatalf("matching HEAD skip = %q err=%v", got, err)
	}
}

// conveyor checkout reads the assignment from the task with the caller's
// credential and ignores every launcher variable: it takes and releases the
// writer lock, creates the worktree under the configured location, makes no
// checkpoint commit, and sends no write request (req-delegated-execution
// REQ-3; DEC-97(1); component-attempt-checkpoints).
func TestCheckoutTakesWriterLockWithoutLauncherEnvironment(t *testing.T) {
	fixture := newGitFixture(t)
	outsideRoot := filepath.Join(t.TempDir(), "launcher-root")
	for key, value := range map[string]string{
		"CONVEYOR_TASK_ID":                 "env-task",
		"CONVEYOR_TASK_BRANCH":             "conveyor/task-stale-task",
		"CONVEYOR_TASK_BASE_BRANCH":        "stale-base",
		"CONVEYOR_TASK_REPO":               "stale-repo",
		"CONVEYOR_TASK_REPO_URL":           "https://example.invalid/stale.git",
		"CONVEYOR_PREDECESSOR":             `{"task_id":"env-task","branch":"conveyor/task-env-task"}`,
		"CONVEYOR_PREVIOUS_WORK_ORDER_ID":  "env-task-implement-1",
		"CONVEYOR_PREVIOUS_ATTEMPT_ID":     "attempt-previous",
		"CONVEYOR_CURRENT_ATTEMPT_ID":      "attempt-current",
		"CONVEYOR_PREVIOUS_ATTEMPT_REASON": "stale failure",
		"CONVEYOR_WORK_ORDER_ID":           "env-task-implement-1",
		"CONVEYOR_SESSION_ID":              "session-current",
		"CONVEYOR_WORKTREE_ROOT":           outsideRoot,
		"CONVEYOR_WRITER_GENERATION":       "stale-generation",
		"CONVEYOR_WRITER_PATH":             filepath.Join(outsideRoot, "stale-writer.json"),
	} {
		t.Setenv(key, value)
	}
	var writes []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writes = append(writes, r.Method+" "+r.URL.Path)
			http.Error(w, "unexpected write", http.StatusMethodNotAllowed)
			return
		}
		switch r.URL.Path {
		case "/v1/tasks/env-task":
			_ = json.NewEncoder(w).Encode(core.Task{ID: "env-task", Workspace: "demo", Repo: "conveyor", Branch: "conveyor/task-env-task", BaseBranch: "main", State: core.TaskRunning})
		case "/v1/workspace/config":
			_ = json.NewEncoder(w).Encode(config.VersionedDocument{Document: config.WorkspaceDocument{Repos: []config.Repo{{Name: "conveyor", URL: fixture.origin}}}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	t.Setenv("CONVEYOR_ADDR", server.URL)
	t.Setenv("CONVEYOR_API_TOKEN", "checkout-token")
	t.Setenv("CONVEYOR_WORKSPACE", "demo")
	root := filepath.Join(fixture.tmp, "configured-root")
	t.Setenv("CONVEYOR_CONFIG", worktreeLocationTestConfig(t, "  mode: machine\n  root: "+root+"\n"))

	cmd := checkoutCmd()
	cmd.SetArgs([]string{"env-task"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	worktree := filepath.Join(root, "conveyor-task-env-task")
	assertBranch(t, worktree, "conveyor/task-env-task")
	if head, base := mustGitOutput(t, worktree, "rev-parse", "HEAD"), mustGitOutput(t, fixture.primary, "rev-parse", "origin/main"); head != base {
		t.Fatalf("checkout HEAD = %s, want the fresh base %s with no checkpoint commit", head, base)
	}
	if _, err := os.Stat(outsideRoot); !os.IsNotExist(err) {
		t.Fatalf("launcher worktree root was used: %v", err)
	}
	if len(writes) != 0 {
		t.Fatalf("checkout sent writes %v", writes)
	}
	writerPath, err := worktreeWriterPath(context.Background(), fixture.primary, "conveyor/task-env-task", "conveyor", fixture.origin)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	lock, err := acquireWorktreeWriter(ctx, writerPath)
	if err != nil {
		t.Fatalf("writer lock still held after checkout: %v", err)
	}
	lock.close()
}

func TestMaybeAttachOperatorCheckoutRefusalsPreserveAssignment(t *testing.T) {
	fixture := newGitFixture(t)
	assigned := "conveyor/task-refuse"
	path := filepath.Join(fixture.tmp, "refuse-tree")
	mustGit(t, fixture.primary, "worktree", "add", "-b", "feature/refuse", path, "origin/main")

	t.Run("identity", func(t *testing.T) {
		t.Chdir(path)
		other := filepath.Join(fixture.tmp, "other-origin.git")
		mustGit(t, "", "init", "--bare", "--initial-branch=main", other)
		got, err := maybeAttachOperatorCheckout(context.Background(), &client{token: "secret-token"}, "refuse", assigned, "conveyor", other)
		if err == nil || !strings.Contains(err.Error(), "repository identity mismatch") || got != assigned {
			t.Fatalf("identity = %q err=%v", got, err)
		}
	})
	t.Run("dirty", func(t *testing.T) {
		t.Chdir(path)
		writeFile(t, filepath.Join(path, "dirty.txt"), "keep\n")
		t.Cleanup(func() { _ = os.Remove(filepath.Join(path, "dirty.txt")) })
		got, err := maybeAttachOperatorCheckout(context.Background(), &client{token: "secret-token"}, "refuse", assigned, "conveyor", fixture.origin)
		if err == nil || !strings.Contains(err.Error(), "uncommitted") || got != assigned {
			t.Fatalf("dirty = %q err=%v", got, err)
		}
	})
	t.Run("detached", func(t *testing.T) {
		t.Chdir(path)
		mustGit(t, path, "checkout", "--detach")
		t.Cleanup(func() { mustGit(t, path, "checkout", "feature/refuse") })
		got, err := maybeAttachOperatorCheckout(context.Background(), &client{token: "secret-token"}, "refuse", assigned, "conveyor", fixture.origin)
		if err == nil || !strings.Contains(err.Error(), "detached HEAD") || got != assigned {
			t.Fatalf("detached = %q err=%v", got, err)
		}
	})
	t.Run("server refusal", func(t *testing.T) {
		t.Chdir(path)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "pull_request_recorded", "message": "pull request recorded"})
		}))
		defer server.Close()
		got, err := maybeAttachOperatorCheckout(context.Background(), &client{base: server.URL, token: "secret-token", workspace: "demo"}, "refuse", assigned, "conveyor", fixture.origin)
		if err == nil || !strings.Contains(err.Error(), "pull_request_recorded") || got != assigned {
			t.Fatalf("server = %q err=%v", got, err)
		}
	})
}

func TestDoneCleansOnlyClosedTasksAndRetainsBranches(t *testing.T) {
	fixture := newGitFixture(t)
	branch := "conveyor/task-cleanup"
	destination, err := checkoutTask(context.Background(), branch, "main", "conveyor", fixture.origin, "cleanup", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := removeTaskWorktree(context.Background(), branch, core.TaskRunning); err == nil || !strings.Contains(err.Error(), "merged or closed") {
		t.Fatalf("active cleanup error = %v", err)
	}
	leaked := exec.Command("sh", "-c", "exec sleep 30")
	leaked.Dir = destination
	if err := leaked.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = leaked.Process.Kill()
		_ = leaked.Wait()
	})
	result, err := removeTaskWorktree(context.Background(), branch, core.TaskMerged)
	if err != nil {
		t.Fatal(err)
	}
	if result.Worktree != "removed" || result.Branch != "retained" || result.Path != destination {
		t.Fatalf("cleanup result = %+v", result)
	}
	warnings := strings.Join(result.ProcessWarnings, "\n")
	if !strings.Contains(warnings, strconv.Itoa(leaked.Process.Pid)) || !strings.Contains(warnings, destination) {
		t.Fatalf("cleanup warnings do not identify process %d at %s: %q", leaked.Process.Pid, destination, warnings)
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
	fixture := newGitFixture(t)
	dirtyBranch := "conveyor/task-dirty-cleanup"
	dirtyPath, err := checkoutTask(context.Background(), dirtyBranch, "main", "conveyor", fixture.origin, "dirty-cleanup", "")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dirtyPath, "dirty.txt"), "keep\n")
	if _, err := removeTaskWorktree(context.Background(), dirtyBranch, core.TaskClosed); err == nil || !strings.Contains(err.Error(), "uncommitted") {
		t.Fatalf("dirty cleanup error = %v", err)
	}
	assertFile(t, filepath.Join(dirtyPath, "dirty.txt"))

	missingBranch := "conveyor/task-missing-directory"
	missingPath, err := checkoutTask(context.Background(), missingBranch, "main", "conveyor", fixture.origin, "missing-directory", "")
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
	if result.Worktree != "pruned" || result.Branch != "retained" {
		t.Fatalf("missing-directory cleanup result = %+v", result)
	}
}

// checkoutTask checks out under the fixture's machine-mode worktree location,
// $HOME/.conveyor/worktrees; newGitFixture redirects HOME.
func checkoutTask(ctx context.Context, branch, base, repo, repoURL, taskID, destination string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return checkoutTaskAtRoot(ctx, branch, base, repo, repoURL, taskID, destination, config.DefaultWorktreeRoot(home))
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
	t.Setenv("HOME", tmp)
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

// worktreeLocationTestConfig writes a valid local execution document from the
// example with its worktree_location block replaced by location (or removed
// when location is empty).
func worktreeLocationTestConfig(t *testing.T, location string) string {
	t.Helper()
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source path")
	}
	example, err := os.ReadFile(filepath.Join(filepath.Dir(source), "..", "..", "conveyor.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var kept []string
	skipping := false
	for _, line := range strings.Split(string(example), "\n") {
		if strings.HasPrefix(line, "worktree_location:") {
			skipping = true
			continue
		}
		if skipping && (strings.HasPrefix(line, " ") || line == "") {
			continue
		}
		skipping = false
		kept = append(kept, line)
	}
	document := strings.Join(kept, "\n")
	if location != "" {
		document += "\nworktree_location:\n" + location
	}
	path := filepath.Join(t.TempDir(), "conveyor.yaml")
	if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func stubCheckoutTerminals(t *testing.T, stdin, stdout bool) {
	t.Helper()
	previous := streamIsTerminal
	streamIsTerminal = func(file *os.File) bool {
		switch file {
		case os.Stdin:
			return stdin
		case os.Stdout:
			return stdout
		}
		return false
	}
	t.Cleanup(func() { streamIsTerminal = previous })
}

func stubWorktreeLocationPrompt(t *testing.T, prompt func(repositoryPath, machinePath string) (config.WorktreeLocation, error)) {
	t.Helper()
	previous := promptWorktreeLocation
	promptWorktreeLocation = func(_ io.Reader, _ io.Writer, repositoryPath, machinePath string) (config.WorktreeLocation, error) {
		return prompt(repositoryPath, machinePath)
	}
	t.Cleanup(func() { promptWorktreeLocation = previous })
}

func checkoutWithLocationConfig(t *testing.T, fixture gitFixture, configPath, taskID, destination string) (string, error) {
	t.Helper()
	placement, err := checkoutWorktreePlacement(configPath)
	if err != nil {
		return "", err
	}
	return checkoutTaskWithPlacement(context.Background(), "conveyor/task-"+taskID, "main", "conveyor", fixture.origin, taskID, destination, placement)
}

func TestCheckoutWorktreeLocationNonInteractiveRefusesAndCreatesNothing(t *testing.T) {
	fixture := newGitFixture(t)
	stubCheckoutTerminals(t, false, false)
	stubWorktreeLocationPrompt(t, func(string, string) (config.WorktreeLocation, error) {
		t.Fatal("non-interactive checkout prompted")
		return config.WorktreeLocation{}, nil
	})
	configPath := filepath.Join(fixture.tmp, "missing", "conveyor.yaml")
	_, err := checkoutWithLocationConfig(t, fixture, configPath, "unconfigured", "")
	if err == nil {
		t.Fatal("checkout without a worktree location succeeded")
	}
	for _, want := range []string{"no worktree location is configured", "repository (", "machine (", "conveyor config set worktree_location repository --config " + configPath, "conveyor config set worktree_location machine --config " + configPath} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q lacks %q", err, want)
		}
	}
	if gitRefExists(context.Background(), fixture.primary, "refs/heads/conveyor/task-unconfigured") {
		t.Fatal("task branch created without a worktree location")
	}
	for _, path := range []string{filepath.Join(fixture.tmp, ".conveyor", "worktrees"), filepath.Join(fixture.primary, "_worktrees"), configPath} {
		if _, statErr := os.Lstat(path); !os.IsNotExist(statErr) {
			t.Fatalf("%s exists after refused checkout: %v", path, statErr)
		}
	}
}

func TestCheckoutWorktreeLocationOneTerminalStreamDoesNotPrompt(t *testing.T) {
	for _, streams := range [][2]bool{{true, false}, {false, true}} {
		fixture := newGitFixture(t)
		stubCheckoutTerminals(t, streams[0], streams[1])
		stubWorktreeLocationPrompt(t, func(string, string) (config.WorktreeLocation, error) {
			t.Fatalf("checkout prompted with stdin terminal=%v stdout terminal=%v", streams[0], streams[1])
			return config.WorktreeLocation{}, nil
		})
		_, err := checkoutWithLocationConfig(t, fixture, worktreeLocationTestConfig(t, ""), "one-stream", "")
		if err == nil || !strings.Contains(err.Error(), "no worktree location is configured") {
			t.Fatalf("stdin=%v stdout=%v error = %v", streams[0], streams[1], err)
		}
	}
}

func TestCheckoutWorktreeLocationInteractivePromptPersistsChoice(t *testing.T) {
	fixture := newGitFixture(t)
	stubCheckoutTerminals(t, true, true)
	var shownRepository, shownMachine string
	prompts := 0
	stubWorktreeLocationPrompt(t, func(repositoryPath, machinePath string) (config.WorktreeLocation, error) {
		prompts++
		shownRepository, shownMachine = repositoryPath, machinePath
		return config.WorktreeLocation{Mode: config.WorktreeLocationRepository}, nil
	})
	configPath := worktreeLocationTestConfig(t, "")
	got, err := checkoutWithLocationConfig(t, fixture, configPath, "interactive", "")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(fixture.primary, "_worktrees", "conveyor-task-interactive")
	if got != want || shownRepository != want || shownMachine != filepath.Join(fixture.tmp, ".conveyor", "worktrees", "conveyor-task-interactive") {
		t.Fatalf("path=%q shown repository=%q machine=%q, want %q", got, shownRepository, shownMachine, want)
	}
	loaded, err := config.LoadClient(configPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	if location, ok := loaded.ConfiguredWorktreeLocation(); !ok || location != (config.WorktreeLocation{Mode: config.WorktreeLocationRepository, Dir: "_worktrees"}) {
		t.Fatalf("persisted location = %+v, %v", location, ok)
	}
	if _, err = checkoutWithLocationConfig(t, fixture, configPath, "second", ""); err != nil {
		t.Fatal(err)
	}
	if prompts != 1 {
		t.Fatalf("prompted %d times, want once", prompts)
	}
}

func TestCheckoutWorktreeLocationRepositoryModeExcludesBeforeCleanlinessCheck(t *testing.T) {
	fixture := newGitFixture(t)
	configPath := worktreeLocationTestConfig(t, "  mode: repository\n")
	// A leftover, unignored container would make the primary read as dirty;
	// checkout adds the exclude entry before its cleanliness check.
	if err := os.MkdirAll(filepath.Join(fixture.primary, "_worktrees", "leftover"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(fixture.primary, "_worktrees", "leftover", "notes.txt"), "left\n")
	got, err := checkoutWithLocationConfig(t, fixture, configPath, "repo-mode", "")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(fixture.primary, "_worktrees", "conveyor-task-repo-mode"); got != want {
		t.Fatalf("path = %q, want %q", got, want)
	}
	if _, err = checkoutWithLocationConfig(t, fixture, configPath, "repo-mode-two", ""); err != nil {
		t.Fatal(err)
	}
	exclude, err := os.ReadFile(filepath.Join(fixture.primary, ".git", "info", "exclude"))
	if err != nil {
		t.Fatal(err)
	}
	if count := strings.Count(string(exclude), "_worktrees/\n"); count != 1 {
		t.Fatalf("exclude entry count = %d in %q", count, exclude)
	}
	if status := mustGitOutput(t, fixture.primary, "status", "--porcelain", "--untracked-files=normal"); status != "" {
		t.Fatalf("primary status after in-repository checkouts = %q", status)
	}
}

func TestCheckoutWorktreeLocationNamesMissingExcludeEntry(t *testing.T) {
	fixture := newGitFixture(t)
	configPath := worktreeLocationTestConfig(t, "  mode: repository\n")
	if _, err := checkoutWithLocationConfig(t, fixture, configPath, "named-first", ""); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.primary, ".git", "info", "exclude"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := checkoutWithLocationConfig(t, fixture, configPath, "named-second", filepath.Join(fixture.tmp, "explicit"))
	if err == nil || !strings.Contains(err.Error(), "in-repository worktree directory _worktrees/ is not excluded") || !strings.Contains(err.Error(), "conveyor checkout named-second") {
		t.Fatalf("error = %v", err)
	}
}

func TestCheckoutWorktreeLocationRefusesTrackedDirectory(t *testing.T) {
	fixture := newGitFixture(t)
	if err := os.MkdirAll(filepath.Join(fixture.primary, "_worktrees"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(fixture.primary, "_worktrees", "tracked.txt"), "tracked\n")
	mustGit(t, fixture.primary, "add", "_worktrees/tracked.txt")
	mustGit(t, fixture.primary, "commit", "-m", "track the container")
	_, err := checkoutWithLocationConfig(t, fixture, worktreeLocationTestConfig(t, "  mode: repository\n"), "tracked", "")
	if err == nil || !strings.Contains(err.Error(), "holds tracked files") {
		t.Fatalf("error = %v", err)
	}
	if gitRefExists(context.Background(), fixture.primary, "refs/heads/conveyor/task-tracked") {
		t.Fatal("task branch created for a tracked container")
	}
}

func TestCheckoutWorktreeLocationRefusesEscapingSymlink(t *testing.T) {
	fixture := newGitFixture(t)
	outside := filepath.Join(fixture.tmp, "outside")
	if err := os.Mkdir(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(fixture.primary, "_worktrees")); err != nil {
		t.Fatal(err)
	}
	_, err := checkoutWithLocationConfig(t, fixture, worktreeLocationTestConfig(t, "  mode: repository\n"), "escape", "")
	if err == nil || !strings.Contains(err.Error(), "outside the repository") {
		t.Fatalf("error = %v", err)
	}
	if gitRefExists(context.Background(), fixture.primary, "refs/heads/conveyor/task-escape") {
		t.Fatal("task branch created through an escaping container")
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("worktree created outside the repository: %v", entries)
	}
}

func TestCheckoutWorktreeLocationMachineRootAndPathPrecedence(t *testing.T) {
	fixture := newGitFixture(t)
	root := filepath.Join(fixture.tmp, "machine-trees")
	configPath := worktreeLocationTestConfig(t, "  mode: machine\n  root: "+root+"\n")
	got, err := checkoutWithLocationConfig(t, fixture, configPath, "machine", "")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(root, "conveyor-task-machine"); got != want {
		t.Fatalf("machine path = %q, want %q", got, want)
	}
	explicit := filepath.Join(fixture.tmp, "explicit-path")
	if got, err = checkoutWithLocationConfig(t, fixture, configPath, "explicit", explicit); err != nil || got != explicit {
		t.Fatalf("--path = %q, %v; want %q", got, err, explicit)
	}
	// A launcher's forwarded container is no longer read (DEC-97(1)).
	t.Setenv("CONVEYOR_WORKTREE_ROOT", filepath.Join(fixture.tmp, "forwarded"))
	if got, err = checkoutWithLocationConfig(t, fixture, configPath, "forwarded", ""); err != nil || filepath.Dir(got) == filepath.Join(fixture.tmp, "forwarded") {
		t.Fatalf("forwarded path = %q, %v; want the configured location", got, err)
	}
}

func TestCheckoutWorktreeLocationReusesRegisteredWorktreeWithoutLocation(t *testing.T) {
	fixture := newGitFixture(t)
	first, err := checkoutTaskAtRoot(context.Background(), "conveyor/task-reuse-unconfigured", "main", "conveyor", fixture.origin, "reuse-unconfigured", "", filepath.Join(fixture.tmp, "earlier-root"))
	if err != nil {
		t.Fatal(err)
	}
	stubCheckoutTerminals(t, false, false)
	got, err := checkoutWithLocationConfig(t, fixture, worktreeLocationTestConfig(t, ""), "reuse-unconfigured", "")
	if err != nil || got != first {
		t.Fatalf("reuse = %q, %v; want %q", got, err, first)
	}
}

// conveyor run, conveyor worker, and conveyor setup are retired with the
// launcher, and conveyor config keeps no execution setup acts (DEC-97(1);
// DEC-104(2); component-runtime).
func TestRetiredLauncherCommandsAreUnknown(t *testing.T) {
	root := newRootCmd()
	for _, command := range root.Commands() {
		switch command.Name() {
		case "run", "worker", "setup":
			t.Errorf("root command tree still registers %q", command.Name())
		}
	}
	var help strings.Builder
	root.SetOut(&help)
	root.SetArgs([]string{"--help"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(help.String(), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && (fields[0] == "run" || fields[0] == "worker" || fields[0] == "setup") {
			t.Errorf("conveyor --help lists %q", line)
		}
	}
	for _, args := range [][]string{{"run", "261101-abc123"}, {"worker", "start"}, {"setup", "list"}} {
		cmd := newRootCmd()
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		cmd.SetArgs(args)
		err := cmd.Execute()
		if err == nil || !strings.Contains(err.Error(), `unknown command "`+args[0]+`"`) {
			t.Errorf("conveyor %s: err = %v, want unknown command", strings.Join(args, " "), err)
		}
	}
	configPath := filepath.Join(t.TempDir(), "conveyor.yaml")
	for _, args := range [][]string{
		{"config", "set", "--config", configPath, "execution.implement.model", "gpt-5.6"},
		{"config", "init-execution", "--config", configPath},
	} {
		cmd := newRootCmd()
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		cmd.SetArgs(args)
		if err := cmd.Execute(); err == nil {
			t.Errorf("conveyor %s succeeded", strings.Join(args, " "))
		}
	}
	if _, err := os.Stat(configPath); !os.IsNotExist(err) {
		t.Fatalf("a refused config act wrote %s: %v", configPath, err)
	}
}
