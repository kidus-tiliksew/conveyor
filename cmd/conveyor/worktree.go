package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/kidus-tiliksew/conveyor/cmd/conveyor/localgit"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/gitx"
)

type registeredWorktree struct {
	Path     string
	Branch   string
	Locked   bool
	Prunable bool
}

type worktreeCleanupResult struct {
	Worktree        string
	Branch          string
	Path            string
	ProcessWarnings []string
}

// checkoutTaskAtRoot resolves the task worktree with a fixed container.
func checkoutTaskAtRoot(ctx context.Context, branch, base, repo, repoURL, taskID, destination, worktreeRoot string) (string, error) {
	return checkoutTaskWithPlacement(ctx, branch, base, repo, repoURL, taskID, destination, fixedWorktreePlacement(worktreeRoot))
}

// checkoutTaskWithPlacement resolves one safe, task-dedicated checkout without
// switching or rewriting the operator's primary checkout (component-git-delivery;
// DEC-10). The placement is read only when a new worktree is created.
func checkoutTaskWithPlacement(ctx context.Context, branch, base, repo, repoURL, taskID, destination string, placement worktreePlacement) (string, error) {
	root, err := repositoryRoot(ctx)
	if err != nil {
		return "", fmt.Errorf("checkout must run from inside the target repository; change into its primary checkout: %w", err)
	}
	// Identity precedes fetches, ref inspection, worktree reuse, and creation.
	// A directory label is never accepted as proof of repository ownership
	// (component-git-delivery).
	if err := localgit.VerifyRepositoryIdentity(ctx, root, repo, repoURL); err != nil {
		return "", err
	}
	worktrees, err := listRegisteredWorktrees(ctx, root)
	if err != nil {
		return "", err
	}
	primary, err := primaryWorktreeRoot(ctx, root)
	if err != nil {
		return "", err
	}
	var assigned *registeredWorktree
	for i := range worktrees {
		entry := &worktrees[i]
		if entry.Branch != "refs/heads/"+branch {
			continue
		}
		if assigned != nil {
			return "", fmt.Errorf("task branch %s is registered in multiple worktrees", branch)
		}
		assigned = entry
	}
	reusingAssigned := assigned != nil && filepath.Clean(assigned.Path) != filepath.Clean(primary)
	if operation, err := inProgressGitOperation(ctx, primary); err != nil {
		return "", err
	} else if operation != "" {
		return "", fmt.Errorf("primary checkout is unsafe: Git operation %s is in progress", operation)
	}
	// The implicit container is resolved only for a new worktree. An
	// in-repository container gets its exclude entry before the cleanliness
	// check, since its nested worktrees would otherwise read as untracked.
	implicitDestination := destination == ""
	var worktreeRoot string
	if implicitDestination && assigned == nil {
		if worktreeRoot, err = placement.prepare(ctx, primary, repo, taskID, branch); err != nil {
			return "", err
		}
	}
	// Dirty-primary refusal: creating a new worktree, at any destination,
	// requires a primary checkout on a named branch with no uncommitted or
	// untracked changes (component-git-delivery; req-delegated-execution
	// REQ-3). AC-3.7 exempts reuse of a registered non-primary worktree. The
	// hazards near this refusal, and what guards each:
	//   - Git operation in progress in the primary: applies to any
	//     destination; refused above for creation and reuse (AC-3.7).
	//   - Task branch checked out in the primary: applies; refused below as
	//     the shared primary checkout (AC-3.6; req-task-branch-assignment
	//     AC-2.6), and by Git itself. A dirty primary is refused here first.
	//   - Primary as the destination: applies; refused below (AC-3.6).
	//   - In-repository worktree directory missing its exclude entry, which
	//     exposes nested task worktrees to `git add -A` in the primary:
	//     applies; only this check detects it, and unsafePrimaryError names
	//     it (component-git-delivery).
	//   - Fetches or ref updates moving the primary's branch: does not apply.
	//     Fetches write only refs/remotes/origin/*, and `worktree add -b`
	//     refuses an existing name (AC-3.2; DEC-10).
	//   - Loss of the primary's uncommitted files: does not apply. No command
	//     below writes the primary's index or working tree.
	// Other dirt, and a detached primary HEAD, guard no Git hazard for a
	// separate destination; the refusal stays as a tripwire for edits made in
	// the primary before checkout (REQ-3). Narrowing it needs a confirmed
	// decision.
	if !reusingAssigned {
		if _, err := requireSafeWorktree(ctx, primary); err != nil {
			return "", unsafePrimaryError(ctx, primary, taskID, inRepositoryWorktreeCandidates(primary, placement, worktrees), err)
		}
	}
	var currentCheckoutErr error
	if filepath.Clean(root) != filepath.Clean(primary) {
		_, currentCheckoutErr = requireSafeWorktree(ctx, root)
	}

	if implicitDestination {
		if assigned == nil {
			destination, err = implicitCheckoutDestination(worktreeRoot, repo, taskID, branch)
			if err != nil {
				return "", err
			}
		}
	} else {
		if !filepath.IsAbs(destination) {
			destination, err = filepath.Abs(destination)
			if err != nil {
				return "", err
			}
		}
		destination, err = canonicalWorktreePath(destination)
		if err != nil {
			return "", err
		}
	}

	if currentCheckoutErr != nil {
		return "", fmt.Errorf("current checkout is unsafe: %w", currentCheckoutErr)
	}
	if assigned != nil {
		if filepath.Clean(assigned.Path) == filepath.Clean(primary) {
			return "", fmt.Errorf("task branch %s is checked out in the shared primary checkout %s", branch, primary)
		}
		if assigned.Locked || assigned.Prunable {
			return "", fmt.Errorf("task branch %s is registered in an unavailable worktree at %s", branch, assigned.Path)
		}
		actualBranch, err := worktreeBranch(ctx, assigned.Path)
		if err != nil {
			return "", fmt.Errorf("task worktree %s is unsafe: %w", assigned.Path, err)
		}
		if actualBranch != branch {
			return "", fmt.Errorf("task worktree %s owns %s instead of %s", assigned.Path, actualBranch, branch)
		}
		if operation, opErr := inProgressGitOperation(ctx, assigned.Path); opErr != nil {
			return "", opErr
		} else if operation != "" {
			return "", fmt.Errorf("task worktree %s has Git operation %s in progress", assigned.Path, operation)
		}
		status, statusErr := gitOutput(ctx, assigned.Path, "status", "--porcelain", "--untracked-files=normal")
		if statusErr != nil {
			return "", statusErr
		}
		if strings.TrimSpace(status) != "" {
			return "", fmt.Errorf("task worktree %s is unsafe: worktree has uncommitted or untracked changes", assigned.Path)
		}
	}
	canonicalPrimary, err := canonicalWorktreePath(primary)
	if err != nil {
		return "", err
	}
	if assigned == nil && filepath.Clean(destination) == canonicalPrimary {
		return "", fmt.Errorf("refusing to use the primary checkout %s as the task worktree", primary)
	}
	baseRef := "refs/remotes/origin/" + base
	if _, err := gitOutput(ctx, root, "fetch", "origin", "refs/heads/"+base+":"+baseRef); err != nil {
		return "", fmt.Errorf("fetch assigned base %s: %w", base, err)
	}
	if !gitRefExists(ctx, root, baseRef) {
		return "", fmt.Errorf("assigned base origin/%s is unavailable", base)
	}

	localRef := "refs/heads/" + branch
	remoteRef := "refs/remotes/origin/" + branch
	remoteListing, err := gitOutput(ctx, root, "ls-remote", "--heads", "origin", localRef)
	if err != nil {
		return "", err
	}
	remoteExists := strings.TrimSpace(remoteListing) != ""
	if remoteExists {
		if _, err := gitOutput(ctx, root, "fetch", "origin", localRef+":"+remoteRef); err != nil {
			return "", fmt.Errorf("fetch assigned task branch %s: %w", branch, err)
		}
	}
	localExists := gitRefExists(ctx, root, localRef)
	remoteAhead := false
	if localExists && remoteExists {
		switch {
		case gitIsAncestor(ctx, root, localRef, remoteRef):
			remoteAhead = !gitIsAncestor(ctx, root, remoteRef, localRef)
		case gitIsAncestor(ctx, root, remoteRef, localRef):
			// Preserve local commits that have not been pushed yet.
		default:
			return "", fmt.Errorf("task branch %s diverged between the local clone and origin", branch)
		}
	}

	if assigned != nil {
		if remoteAhead {
			if _, err := gitOutput(ctx, assigned.Path, "merge", "--ff-only", remoteRef); err != nil {
				return "", fmt.Errorf("fast-forward existing task worktree: %w", err)
			}
		}
		return assigned.Path, nil
	}

	for _, entry := range worktrees {
		if filepath.Clean(entry.Path) == destination {
			occupying := strings.TrimPrefix(entry.Branch, "refs/heads/")
			if occupying == "" {
				occupying = entry.Branch
			}
			return "", fmt.Errorf("destination %s is already a registered worktree for %s at %s", destination, occupying, entry.Path)
		}
	}
	if _, err := os.Lstat(destination); err == nil {
		return "", fmt.Errorf("destination %s already exists but is not the assigned task worktree", destination)
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("inspect destination %s: %w", destination, err)
	}
	if implicitDestination && assigned == nil {
		if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
			return "", fmt.Errorf("create implicit worktree container: %w", err)
		}
		validated, err := implicitCheckoutDestination(worktreeRoot, repo, taskID, branch)
		if err != nil {
			return "", err
		}
		if filepath.Clean(validated) != filepath.Clean(destination) {
			return "", fmt.Errorf("implicit worktree destination changed during validation: %s became %s", destination, validated)
		}
	}

	switch {
	case localExists:
		if _, err := gitOutput(ctx, root, "worktree", "add", destination, branch); err != nil {
			return "", err
		}
		if remoteAhead {
			if _, err := gitOutput(ctx, destination, "merge", "--ff-only", remoteRef); err != nil {
				return "", fmt.Errorf("fast-forward new task worktree: %w", err)
			}
		}
	case remoteExists:
		if _, err := gitOutput(ctx, root, "worktree", "add", "--track", "-b", branch, destination, remoteRef); err != nil {
			return "", err
		}
	default:
		if _, err := gitOutput(ctx, root, "worktree", "add", "-b", branch, destination, baseRef); err != nil {
			return "", err
		}
	}
	actualBranch, err := requireSafeWorktree(ctx, destination)
	if err != nil {
		return "", fmt.Errorf("created task worktree is unsafe: %w", err)
	}
	if actualBranch != branch {
		return "", fmt.Errorf("created task worktree owns %s instead of %s", actualBranch, branch)
	}
	return destination, nil
}

func worktreeBranch(ctx context.Context, path string) (string, error) {
	branch, err := gitOutput(ctx, path, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		return "", fmt.Errorf("detached HEAD or unreadable branch")
	}
	return strings.TrimSpace(branch), nil
}

// maybeAttachOperatorCheckout remaps Task.Branch to the current non-primary
// named HEAD when it differs, then leaves checkout to adopt that worktree
// (req-task-branch-assignment AC-2.2). Primary checkouts and HEAD matching
// the current assignment skip attach (AC-2.3, AC-2.6).
func maybeAttachOperatorCheckout(ctx context.Context, c *client, taskID, assignment, repo, repoURL string) (string, error) {
	if c == nil {
		return assignment, fmt.Errorf("a credential is required to attach a task branch; run `conveyor auth login`")
	}
	root, err := repositoryRoot(ctx)
	if err != nil {
		return assignment, fmt.Errorf("checkout must run from inside the target repository; change into its primary checkout (dispatched launches may configure repos[].checkout): %w", err)
	}
	if err := localgit.VerifyRepositoryIdentity(ctx, root, repo, repoURL); err != nil {
		return assignment, err
	}
	primary, err := primaryWorktreeRoot(ctx, root)
	if err != nil {
		return assignment, err
	}
	current, err := canonicalWorktreePath(root)
	if err != nil {
		return assignment, err
	}
	canonicalPrimary, err := canonicalWorktreePath(primary)
	if err != nil {
		return assignment, err
	}
	if current == canonicalPrimary {
		return assignment, nil
	}
	head, err := gitOutput(ctx, root, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		return assignment, fmt.Errorf("detached HEAD or unreadable branch")
	}
	head = strings.TrimSpace(head)
	if head == "" {
		return assignment, fmt.Errorf("detached HEAD or unreadable branch")
	}
	if head == assignment {
		return assignment, nil
	}
	status, err := gitOutput(ctx, root, "status", "--porcelain", "--untracked-files=normal")
	if err != nil {
		return assignment, err
	}
	if strings.TrimSpace(status) != "" {
		return assignment, fmt.Errorf("worktree has uncommitted or untracked changes")
	}
	task, err := c.attachTaskBranch(taskID, head)
	if err != nil {
		return assignment, err
	}
	if strings.TrimSpace(task.Branch) != "" {
		return task.Branch, nil
	}
	return head, nil
}

func otherOpenTaskHoldingBranch(tasks []core.Task, taskID, repo, branch string) (string, bool) {
	for _, item := range tasks {
		if item.ID == taskID || item.Repo != repo || item.Branch != branch {
			continue
		}
		if core.TaskTerminal(item.State) {
			continue
		}
		return item.ID, true
	}
	return "", false
}

func skippedOccupiedWorktreeCleanup() worktreeCleanupResult {
	return worktreeCleanupResult{Worktree: "skipped", Branch: "retained", Path: "-"}
}

func removeTaskWorktree(ctx context.Context, branch string, state core.TaskState) (worktreeCleanupResult, error) {
	result := worktreeCleanupResult{Worktree: "skipped", Branch: "absent", Path: "-"}
	if state != core.TaskMerged && state != core.TaskClosed {
		return result, fmt.Errorf("task must be merged or closed before worktree cleanup (state %s)", state)
	}
	root, err := repositoryRoot(ctx)
	if err != nil {
		return result, fmt.Errorf("done must run inside the repository's primary checkout: %w", err)
	}
	primary, err := primaryWorktreeRoot(ctx, root)
	if err != nil {
		return result, err
	}
	if filepath.Clean(root) != filepath.Clean(primary) {
		return result, fmt.Errorf("done must run inside the repository's primary checkout")
	}
	return removeTaskWorktreeAtPrimary(ctx, primary, branch, state)
}

func removeTaskWorktreeAtPrimary(ctx context.Context, primary, branch string, state core.TaskState) (worktreeCleanupResult, error) {
	result := worktreeCleanupResult{Worktree: "skipped", Branch: "absent", Path: "-"}
	if state != core.TaskMerged && state != core.TaskClosed {
		return result, fmt.Errorf("task must be merged or closed before worktree cleanup (state %s)", state)
	}
	cleanup, err := localgit.CleanupTaskWorktree(ctx, primary, branch)
	return worktreeCleanupResult(cleanup), err
}

func repositoryRoot(ctx context.Context) (string, error) {
	return repositoryRootAt(ctx, "")
}

// repositoryRootAt resolves the repository containing dir; an empty dir means
// the process working directory.
func repositoryRootAt(ctx context.Context, dir string) (string, error) {
	root, err := gitOutput(ctx, strings.TrimSpace(dir), "rev-parse", "--show-toplevel")
	return strings.TrimSpace(root), err
}

func primaryWorktreeRoot(ctx context.Context, root string) (string, error) {
	commonDir, err := gitOutput(ctx, root, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", err
	}
	return filepath.Clean(filepath.Dir(strings.TrimSpace(commonDir))), nil
}

func canonicalWorktreePath(path string) (string, error) {
	cleaned := filepath.Clean(path)
	resolved, err := filepath.EvalSymlinks(cleaned)
	if err == nil {
		return filepath.Clean(resolved), nil
	}
	if !os.IsNotExist(err) {
		return "", fmt.Errorf("resolve worktree path %s: %w", cleaned, err)
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(cleaned))
	if err != nil {
		return "", fmt.Errorf("resolve worktree parent %s: %w", filepath.Dir(cleaned), err)
	}
	return filepath.Join(parent, filepath.Base(cleaned)), nil
}

// implicitCheckoutDestination keeps the deterministic worktree name beneath
// one canonical container resolved from the configured worktree location
// (component-git-delivery). Default conveyor/task-<id>
// assignments keep <root>/<repo>-task-<task-id>; a custom assignment appends
// a sanitized branch component so a leftover previous-name tree is not reused.
func implicitCheckoutDestination(worktreeRoot, repo, taskID, branch string) (string, error) {
	if !safeImplicitCheckoutComponent(repo) {
		return "", fmt.Errorf("refusing implicit checkout destination: repository name %q is not one safe path component", repo)
	}
	if !safeImplicitCheckoutComponent(taskID) {
		return "", fmt.Errorf("refusing implicit checkout destination: task ID %q is not one safe path component", taskID)
	}
	if !filepath.IsAbs(worktreeRoot) {
		return "", fmt.Errorf("refusing implicit checkout destination: worktree root %q is not absolute", worktreeRoot)
	}
	name := implicitWorktreeName(repo, taskID, branch)
	if strings.TrimSpace(branch) != gitx.BranchName(taskID) && !safeImplicitCheckoutComponent(safeAssignmentPathComponent(branch)) {
		return "", fmt.Errorf("refusing implicit checkout destination: assignment %q is not one safe path component", branch)
	}
	container := filepath.Clean(worktreeRoot)
	canonicalContainer, err := canonicalPathThroughExistingParent(container)
	if err != nil {
		return "", fmt.Errorf("resolve worktree root %s: %w", container, err)
	}
	if canonicalContainer != container {
		return "", fmt.Errorf("refusing implicit checkout destination: worktree root %s resolves outside the canonical path", container)
	}
	destination := filepath.Join(container, name)
	canonicalDestination, err := canonicalPathThroughExistingParent(destination)
	if err != nil {
		return "", fmt.Errorf("resolve worktree path %s: %w", destination, err)
	}
	if canonicalDestination != filepath.Clean(destination) || filepath.Clean(filepath.Dir(canonicalDestination)) != container {
		return "", fmt.Errorf("refusing implicit checkout destination %s: resolved path is not inside canonical container %s", destination, container)
	}
	return canonicalDestination, nil
}

// implicitWorktreeName is <repo>-task-<task-id>, with a sanitized branch
// component appended after a custom assignment.
func implicitWorktreeName(repo, taskID, branch string) string {
	name := repo + "-task-" + taskID
	if strings.TrimSpace(branch) != gitx.BranchName(taskID) {
		name = name + "-" + safeAssignmentPathComponent(branch)
	}
	return name
}

func safeAssignmentPathComponent(branch string) string {
	if branch == "" {
		return ""
	}
	mapped := make([]byte, len(branch))
	for i := range len(branch) {

		character := branch[i]
		if (character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') ||
			character == '.' || character == '_' || character == '-' {
			mapped[i] = character
			continue
		}
		mapped[i] = '-'
	}

	return string(mapped)
}

func canonicalPathThroughExistingParent(path string) (string, error) {
	cleaned := filepath.Clean(path)
	missing := make([]string, 0, 2)
	candidate := cleaned
	for {
		resolved, err := filepath.EvalSymlinks(candidate)
		if err == nil {
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return filepath.Clean(resolved), nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(candidate)
		if parent == candidate {
			return "", err
		}
		missing = append(missing, filepath.Base(candidate))
		candidate = parent
	}
}

func safeImplicitCheckoutComponent(value string) bool {
	if value == "" || value == "." || value == ".." {
		return false
	}
	for i := 0; i < len(value); i++ {
		character := value[i]
		if (character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') ||
			character == '.' || character == '_' || character == '-' {
			continue
		}
		return false
	}
	return true
}

func listRegisteredWorktrees(ctx context.Context, root string) ([]registeredWorktree, error) {
	listing, err := gitOutput(ctx, root, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	var entries []registeredWorktree
	var current *registeredWorktree
	for _, line := range strings.Split(listing, "\n") {
		switch {
		case strings.HasPrefix(line, "worktree "):
			entries = append(entries, registeredWorktree{Path: filepath.Clean(strings.TrimPrefix(line, "worktree "))})
			current = &entries[len(entries)-1]
		case current != nil && strings.HasPrefix(line, "branch "):
			current.Branch = strings.TrimPrefix(line, "branch ")
		case current != nil && strings.HasPrefix(line, "locked"):
			current.Locked = true
		case current != nil && strings.HasPrefix(line, "prunable"):
			current.Prunable = true
		}
	}
	return entries, nil
}

func requireSafeWorktree(ctx context.Context, path string) (string, error) {
	branch, err := gitOutput(ctx, path, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		return "", fmt.Errorf("detached HEAD or unreadable branch")
	}
	status, err := gitOutput(ctx, path, "status", "--porcelain", "--untracked-files=normal")
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(status) != "" {
		return "", fmt.Errorf("worktree has uncommitted or untracked changes")
	}
	if operation, err := inProgressGitOperation(ctx, path); err != nil {
		return "", err
	} else if operation != "" {
		return "", fmt.Errorf("Git operation %s is in progress", operation)
	}
	return strings.TrimSpace(branch), nil
}

func inProgressGitOperation(ctx context.Context, path string) (string, error) {
	markers := []string{
		"MERGE_HEAD", "CHERRY_PICK_HEAD", "REVERT_HEAD", "REBASE_HEAD", "BISECT_LOG",
		"rebase-merge", "rebase-apply", "sequencer",
	}
	for _, marker := range markers {
		markerPath, err := gitOutput(ctx, path, "rev-parse", "--git-path", marker)
		if err != nil {
			return "", err
		}
		resolved := strings.TrimSpace(markerPath)
		if !filepath.IsAbs(resolved) {
			resolved = filepath.Join(path, resolved)
		}
		if _, err := os.Stat(resolved); err == nil {
			return marker, nil
		} else if !os.IsNotExist(err) {
			return "", fmt.Errorf("inspect Git operation marker %s: %w", marker, err)
		}
	}
	return "", nil
}

func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, "git", args...)
	command.Dir = dir
	out, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func gitRefExists(ctx context.Context, dir, ref string) bool {
	_, err := gitOutput(ctx, dir, "rev-parse", "--verify", "--quiet", ref)
	return err == nil
}

func gitIsAncestor(ctx context.Context, dir, older, newer string) bool {
	command := exec.CommandContext(ctx, "git", "merge-base", "--is-ancestor", older, newer)
	command.Dir = dir
	return command.Run() == nil
}
