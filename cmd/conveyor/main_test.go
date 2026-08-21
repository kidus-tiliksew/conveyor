package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
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
	t.Setenv("CONVEYOR_TASK_REPO_URL", "https://github.com/kidus-tiliksew/conveyor.git")

	branch, base, repo, repoURL, ok := assignedCheckoutFromEnvironment("task-1")
	if !ok || branch != "conveyor/task-1" || base != "main" || repo != "conveyor" || repoURL != "https://github.com/kidus-tiliksew/conveyor.git" {
		t.Fatalf("assignment = %q %q %q %q ok=%v", branch, base, repo, repoURL, ok)
	}
	if _, _, _, _, ok := assignedCheckoutFromEnvironment("task-2"); ok {
		t.Fatal("assignment for a different task must fall back to the authenticated lookup")
	}
	t.Setenv("CONVEYOR_TASK_REPO_URL", "")
	if _, _, _, _, ok := assignedCheckoutFromEnvironment("task-1"); ok {
		t.Fatal("assignment without configured repository identity must fall back to the authenticated lookup")
	}
	t.Setenv("CONVEYOR_TASK_REPO_URL", "https://github.com/kidus-tiliksew/conveyor.git")
	t.Setenv("CONVEYOR_TASK_BRANCH", "")
	if _, _, _, _, ok := assignedCheckoutFromEnvironment("task-1"); ok {
		t.Fatal("incomplete assignment must fall back to the authenticated lookup")
	}
}

func TestAssignedPredecessorCheckpointRequiresDistinctRecordedAttempt(t *testing.T) {
	t.Setenv("CONVEYOR_TASK_ID", "task-1")
	t.Setenv("CONVEYOR_WORK_ORDER_ID", "task-1-implement-1")
	t.Setenv("CONVEYOR_SESSION_ID", "session-current")
	t.Setenv("CONVEYOR_CURRENT_ATTEMPT_ID", "attempt-current")
	t.Setenv("CONVEYOR_PREVIOUS_ATTEMPT_ID", "attempt-prior")
	t.Setenv("CONVEYOR_PREVIOUS_ATTEMPT_REASON", "harness exited")
	checkpoint := assignedPredecessorCheckpointFromEnvironment("task-1")
	if checkpoint == nil || checkpoint.AttemptID != "attempt-prior" || checkpoint.WorkOrderID != "task-1-implement-1" || checkpoint.TerminationReason != "harness exited" {
		t.Fatalf("checkpoint=%+v", checkpoint)
	}
	t.Setenv("CONVEYOR_PREVIOUS_ATTEMPT_ID", "attempt-current")
	if checkpoint = assignedPredecessorCheckpointFromEnvironment("task-1"); checkpoint != nil {
		t.Fatalf("current attempt accepted as predecessor: %+v", checkpoint)
	}
	t.Setenv("CONVEYOR_PREVIOUS_ATTEMPT_ID", "attempt-prior")
	if checkpoint = assignedPredecessorCheckpointFromEnvironment("other-task"); checkpoint != nil {
		t.Fatalf("cross-task predecessor accepted: %+v", checkpoint)
	}
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

func TestCheckoutCheckpointsAttributedPredecessorWorkAndPushes(t *testing.T) {
	fixture := newGitFixture(t)
	branch := "conveyor/task-checkpoint"
	path, err := checkoutTask(context.Background(), branch, "main", "conveyor", fixture.origin, "checkpoint", "")
	if err != nil {
		t.Fatal(err)
	}
	primaryHead := mustGitOutput(t, fixture.primary, "rev-parse", "HEAD")
	writeFile(t, filepath.Join(path, "predecessor.txt"), "preserve me\n")
	checkpoint := &attemptCheckpoint{AttemptID: "attempt-prior", WorkOrderID: "checkpoint-implement-1", TerminationReason: "harness exited: signal: killed"}

	got, result, err := checkoutTaskWithCheckpoint(context.Background(), branch, "main", "conveyor", fixture.origin, "checkpoint", "", checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if got != path || result == nil || !result.Pushed || result.CommitSHA == "" {
		t.Fatalf("checkout=%q checkpoint=%+v", got, result)
	}
	if status := mustGitOutput(t, path, "status", "--porcelain"); status != "" {
		t.Fatalf("checkpoint left dirty state: %s", status)
	}
	message := mustGitOutput(t, path, "show", "-s", "--format=%B", "HEAD")
	for _, want := range []string{
		"wip(attempt-prior): checkpoint at attempt death",
		"Attempt-ID: attempt-prior", "Work-Order-ID: checkpoint-implement-1",
		"Termination-Reason: harness exited: signal: killed",
	} {
		if !strings.Contains(message, want) {
			t.Fatalf("checkpoint message %q missing %q", message, want)
		}
	}
	if remote := mustGitOutput(t, path, "ls-remote", "--heads", "origin", "refs/heads/"+branch); !strings.HasPrefix(remote, result.CommitSHA+"\t") {
		t.Fatalf("remote branch = %q, want checkpoint %s", remote, result.CommitSHA)
	}
	assertPrimaryUntouched(t, fixture.primary, primaryHead)

	// Retrying recovery recognizes the already-pushed attributed commit and
	// does not manufacture a duplicate.
	_, retry, err := checkoutTaskWithCheckpoint(context.Background(), branch, "main", "conveyor", fixture.origin, "checkpoint", "", checkpoint)
	if err != nil || retry == nil || retry.CommitSHA != result.CommitSHA {
		t.Fatalf("retry checkpoint=%+v err=%v", retry, err)
	}
	if head := mustGitOutput(t, path, "rev-parse", "HEAD"); head != result.CommitSHA {
		t.Fatalf("retry created duplicate commit %s after %s", head, result.CommitSHA)
	}
}

func TestAttemptAuthorityLossPreservesDirtyWorkAndRequiresAuditReconciliation(t *testing.T) {
	fixture := newGitFixture(t)
	branch := "conveyor/task-authority-loss"
	path, err := checkoutTask(context.Background(), branch, "main", "conveyor", fixture.origin, "authority-loss", "")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(path, "interrupted.txt"), "preserve me\n")
	checkpoint := attemptCheckpoint{
		AttemptID: "attempt-lost", WorkOrderID: "authority-loss-implement-1",
		TerminationReason: "claim authority lost: server reports queued",
	}
	var preserved *attemptCheckpointResult
	err = attemptAuthorityLoss(checkpoint.TerminationReason, func(string) error {
		preserved, err = checkpointAssignedTaskWorktree(context.Background(), branch, "conveyor", fixture.origin, checkpoint)
		if err != nil {
			return err
		}
		return fmt.Errorf("checkpoint commit %s was pushed from %s, but its audit event is not durable; successor reconciliation required: claim lost", preserved.CommitSHA, preserved.Worktree)
	})
	if err == nil || !strings.Contains(err.Error(), "successor reconciliation required") {
		t.Fatalf("authority-loss outcome=%v", err)
	}
	if preserved == nil || !preserved.Pushed || preserved.CommitSHA == "" {
		t.Fatalf("dirty work was not preserved: %+v", preserved)
	}
	if remote := mustGitOutput(t, path, "ls-remote", "--heads", "origin", "refs/heads/"+branch); !strings.HasPrefix(remote, preserved.CommitSHA+"\t") {
		t.Fatalf("remote branch = %q, want preserved commit %s", remote, preserved.CommitSHA)
	}

	// The next authorized attempt can identify the exact pushed checkpoint and
	// publish its append-only event through the existing predecessor path.
	reconciled, err := matchingAttemptCheckpointAtHEAD(context.Background(), path, checkpoint)
	if err != nil || reconciled == nil || reconciled.CommitSHA != preserved.CommitSHA {
		t.Fatalf("successor reconciliation=%+v err=%v", reconciled, err)
	}
}

func TestMatchingAttemptCheckpointRejectsMismatchedTerminationReason(t *testing.T) {
	fixture := newGitFixture(t)
	branch := "conveyor/task-checkpoint-reason"
	path, err := checkoutTask(context.Background(), branch, "main", "conveyor", fixture.origin, "checkpoint-reason", "")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(path, "predecessor.txt"), "preserve me\n")
	checkpoint := attemptCheckpoint{AttemptID: "attempt-prior", WorkOrderID: "reason-implement-1", TerminationReason: "harness exited"}
	if _, err = checkpointAssignedTaskWorktree(context.Background(), branch, "conveyor", fixture.origin, checkpoint); err != nil {
		t.Fatal(err)
	}
	checkpoint.TerminationReason = "claim authority lost"
	if _, err = matchingAttemptCheckpointAtHEAD(context.Background(), path, checkpoint); err == nil || !strings.Contains(err.Error(), "not termination reason") {
		t.Fatalf("mismatched reason was reused: %v", err)
	}
}

func TestAttemptCheckpointCleanNoopAndUnattributableDirtyStateStillBlocks(t *testing.T) {
	fixture := newGitFixture(t)
	branch := "conveyor/task-checkpoint-guard"
	path, err := checkoutTask(context.Background(), branch, "main", "conveyor", fixture.origin, "checkpoint-guard", "")
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := attemptCheckpoint{AttemptID: "attempt-prior", WorkOrderID: "guard-implement-1", TerminationReason: "worker lost"}
	result, err := checkpointAssignedTaskWorktree(context.Background(), branch, "conveyor", fixture.origin, checkpoint)
	if err != nil || result != nil {
		t.Fatalf("clean checkpoint=%+v err=%v, want no-op", result, err)
	}
	writeFile(t, filepath.Join(path, "unknown.txt"), "unknown provenance\n")
	if _, err = checkoutTask(context.Background(), branch, "main", "conveyor", fixture.origin, "checkpoint-guard", ""); err == nil || !strings.Contains(err.Error(), "uncommitted") {
		t.Fatalf("unattributable checkout err=%v", err)
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

func TestCheckoutAdoptsExplicitDedicatedClone(t *testing.T) {
	fixture := newGitFixture(t)
	branch := "conveyor/task-dedicated-clone"
	mustGit(t, fixture.primary, "checkout", "-b", branch, "origin/main")

	got, err := checkoutTask(context.Background(), branch, "main", "conveyor", fixture.origin, "dedicated-clone", fixture.primary)
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
