package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/config"
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

type attemptCheckpoint struct {
	AttemptID         string
	WorkOrderID       string
	TerminationReason string
}

type attemptCheckpointResult struct {
	Worktree  string
	CommitSHA string
	Pushed    bool
}

type worktreeRootContextKey struct{}

func contextWithWorktreeRoot(ctx context.Context, root string) context.Context {
	return context.WithValue(ctx, worktreeRootContextKey{}, root)
}

func worktreeRootFromContext(ctx context.Context) string {
	root, _ := ctx.Value(worktreeRootContextKey{}).(string)
	return root
}

// checkoutTask resolves one safe, task-dedicated checkout without switching or
// rewriting the operator's primary checkout (design-git-delivery; DEC-10).
func checkoutTask(ctx context.Context, branch, base, repo, repoURL, taskID, destination string) (string, error) {
	path, _, err := checkoutTaskWithCheckpoint(ctx, branch, base, repo, repoURL, taskID, destination, nil)
	return path, err
}

func checkoutTaskAtRoot(ctx context.Context, branch, base, repo, repoURL, taskID, destination, worktreeRoot string) (string, error) {
	path, _, err := checkoutTaskWithCheckpointAtRoot(ctx, branch, base, repo, repoURL, taskID, destination, worktreeRoot, nil)
	return path, err
}

func checkoutTaskWithCheckpoint(ctx context.Context, branch, base, repo, repoURL, taskID, destination string, checkpoint *attemptCheckpoint) (string, *attemptCheckpointResult, error) {
	root, err := defaultImplicitWorktreeRoot()
	if err != nil {
		return "", nil, err
	}
	return checkoutTaskWithCheckpointAtRoot(ctx, branch, base, repo, repoURL, taskID, destination, root, checkpoint)
}

func checkoutTaskWithCheckpointAtRoot(ctx context.Context, branch, base, repo, repoURL, taskID, destination, worktreeRoot string, checkpoint *attemptCheckpoint) (string, *attemptCheckpointResult, error) {
	root, err := repositoryRoot(ctx)
	if err != nil {
		return "", nil, fmt.Errorf("checkout must run from inside the target repository; change into its primary checkout (dispatched launches may configure repos[].checkout): %w", err)
	}
	// Identity precedes fetches, ref inspection, worktree reuse, and creation.
	// A directory label is never accepted as proof of repository ownership
	// (design-git-delivery).
	if err := gitx.VerifyRepositoryIdentity(ctx, root, repo, repoURL); err != nil {
		return "", nil, err
	}
	worktrees, err := listRegisteredWorktrees(ctx, root)
	if err != nil {
		return "", nil, err
	}
	primary, err := primaryWorktreeRoot(ctx, root)
	if err != nil {
		return "", nil, err
	}
	if _, err := requireSafeWorktree(ctx, primary); err != nil {
		return "", nil, fmt.Errorf("primary checkout is unsafe: %w", err)
	}
	var currentCheckoutErr error
	if filepath.Clean(root) != filepath.Clean(primary) {
		_, currentCheckoutErr = requireSafeWorktree(ctx, root)
	}

	implicitDestination := destination == ""
	if implicitDestination {
		destination, err = implicitCheckoutDestination(worktreeRoot, repo, taskID)
		if err != nil {
			return "", nil, err
		}
	} else {
		if !filepath.IsAbs(destination) {
			destination, err = filepath.Abs(destination)
			if err != nil {
				return "", nil, err
			}
		}
		destination, err = canonicalWorktreePath(destination)
		if err != nil {
			return "", nil, err
		}
	}

	var assigned *registeredWorktree
	var assignedCheckpoint *attemptCheckpointResult
	for i := range worktrees {
		entry := &worktrees[i]
		if entry.Branch != "refs/heads/"+branch {
			continue
		}
		if assigned != nil {
			return "", nil, fmt.Errorf("task branch %s is registered in multiple worktrees", branch)
		}
		assigned = entry
	}
	if currentCheckoutErr != nil && (checkpoint == nil || assigned == nil || filepath.Clean(root) != filepath.Clean(assigned.Path)) {
		return "", nil, fmt.Errorf("current checkout is unsafe: %w", currentCheckoutErr)
	}
	if assigned != nil {
		if filepath.Clean(assigned.Path) == filepath.Clean(primary) && filepath.Clean(assigned.Path) != destination {
			return "", nil, fmt.Errorf("task branch %s is checked out in the shared primary checkout %s", branch, primary)
		}
		if assigned.Locked || assigned.Prunable {
			return "", nil, fmt.Errorf("task branch %s is registered in an unavailable worktree at %s", branch, assigned.Path)
		}
		actualBranch, err := worktreeBranch(ctx, assigned.Path)
		if err != nil {
			return "", nil, fmt.Errorf("task worktree %s is unsafe: %w", assigned.Path, err)
		}
		if actualBranch != branch {
			return "", nil, fmt.Errorf("task worktree %s owns %s instead of %s", assigned.Path, actualBranch, branch)
		}
		if operation, opErr := inProgressGitOperation(ctx, assigned.Path); opErr != nil {
			return "", nil, opErr
		} else if operation != "" {
			return "", nil, fmt.Errorf("task worktree %s has Git operation %s in progress", assigned.Path, operation)
		}
		status, statusErr := gitOutput(ctx, assigned.Path, "status", "--porcelain", "--untracked-files=normal")
		if statusErr != nil {
			return "", nil, statusErr
		}
		var checkpointed *attemptCheckpointResult
		if strings.TrimSpace(status) != "" {
			if checkpoint == nil {
				return "", nil, fmt.Errorf("task worktree %s is unsafe: worktree has uncommitted or untracked changes", assigned.Path)
			}
			checkpointed, err = checkpointTaskWorktreeAtPath(ctx, assigned.Path, branch, primary, *checkpoint)
			if err != nil {
				return "", nil, err
			}
		} else if checkpoint != nil {
			checkpointed, err = matchingAttemptCheckpointAtHEAD(ctx, assigned.Path, *checkpoint)
			if err != nil {
				return "", nil, err
			}
		}
		assignedCheckpoint = checkpointed
	}
	baseRef := "refs/remotes/origin/" + base
	if _, err := gitOutput(ctx, root, "fetch", "origin", "refs/heads/"+base+":"+baseRef); err != nil {
		return "", nil, fmt.Errorf("fetch assigned base %s: %w", base, err)
	}
	if !gitRefExists(ctx, root, baseRef) {
		return "", nil, fmt.Errorf("assigned base origin/%s is unavailable", base)
	}

	localRef := "refs/heads/" + branch
	remoteRef := "refs/remotes/origin/" + branch
	remoteListing, err := gitOutput(ctx, root, "ls-remote", "--heads", "origin", localRef)
	if err != nil {
		return "", nil, err
	}
	remoteExists := strings.TrimSpace(remoteListing) != ""
	if remoteExists {
		if _, err := gitOutput(ctx, root, "fetch", "origin", localRef+":"+remoteRef); err != nil {
			return "", nil, fmt.Errorf("fetch assigned task branch %s: %w", branch, err)
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
			return "", nil, fmt.Errorf("task branch %s diverged between the local clone and origin", branch)
		}
	}

	if assigned != nil {
		if remoteAhead {
			if _, err := gitOutput(ctx, assigned.Path, "merge", "--ff-only", remoteRef); err != nil {
				return "", nil, fmt.Errorf("fast-forward existing task worktree: %w", err)
			}
		}
		return assigned.Path, assignedCheckpoint, nil
	}

	for _, entry := range worktrees {
		if filepath.Clean(entry.Path) == destination {
			return "", nil, fmt.Errorf("destination %s is already a registered worktree for %s", destination, entry.Branch)
		}
	}
	if _, err := os.Lstat(destination); err == nil {
		return "", nil, fmt.Errorf("destination %s already exists but is not the assigned task worktree", destination)
	} else if !os.IsNotExist(err) {
		return "", nil, fmt.Errorf("inspect destination %s: %w", destination, err)
	}
	if implicitDestination {
		if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
			return "", nil, fmt.Errorf("create implicit worktree container: %w", err)
		}
		validated, err := implicitCheckoutDestination(worktreeRoot, repo, taskID)
		if err != nil {
			return "", nil, err
		}
		if filepath.Clean(validated) != filepath.Clean(destination) {
			return "", nil, fmt.Errorf("implicit worktree destination changed during validation: %s became %s", destination, validated)
		}
	}

	switch {
	case localExists:
		if _, err := gitOutput(ctx, root, "worktree", "add", destination, branch); err != nil {
			return "", nil, err
		}
		if remoteAhead {
			if _, err := gitOutput(ctx, destination, "merge", "--ff-only", remoteRef); err != nil {
				return "", nil, fmt.Errorf("fast-forward new task worktree: %w", err)
			}
		}
	case remoteExists:
		if _, err := gitOutput(ctx, root, "worktree", "add", "--track", "-b", branch, destination, remoteRef); err != nil {
			return "", nil, err
		}
	default:
		if _, err := gitOutput(ctx, root, "worktree", "add", "-b", branch, destination, baseRef); err != nil {
			return "", nil, err
		}
	}
	actualBranch, err := requireSafeWorktree(ctx, destination)
	if err != nil {
		return "", nil, fmt.Errorf("created task worktree is unsafe: %w", err)
	}
	if actualBranch != branch {
		return "", nil, fmt.Errorf("created task worktree owns %s instead of %s", actualBranch, branch)
	}
	return destination, nil, nil
}

func worktreeBranch(ctx context.Context, path string) (string, error) {
	branch, err := gitOutput(ctx, path, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		return "", fmt.Errorf("detached HEAD or unreadable branch")
	}
	return strings.TrimSpace(branch), nil
}

// checkpointTaskWorktreeAtPath preserves dirty state with a normal additive
// commit and a non-force push. The caller has already resolved this path from
// Git's registered worktree inventory, rather than trusting a directory name
// (design-git-delivery).
func checkpointTaskWorktreeAtPath(ctx context.Context, path, branch, primary string, checkpoint attemptCheckpoint) (*attemptCheckpointResult, error) {
	canonicalPath, err := canonicalWorktreePath(path)
	if err != nil {
		return nil, err
	}
	canonicalPrimary, err := canonicalWorktreePath(primary)
	if err != nil {
		return nil, err
	}
	if canonicalPath == canonicalPrimary {
		return nil, fmt.Errorf("refusing to checkpoint the primary checkout %s", canonicalPrimary)
	}
	actualBranch, err := worktreeBranch(ctx, canonicalPath)
	if err != nil {
		return nil, err
	}
	if actualBranch != branch {
		return nil, fmt.Errorf("refusing to checkpoint %s: branch is %s, want %s", canonicalPath, actualBranch, branch)
	}
	if operation, opErr := inProgressGitOperation(ctx, canonicalPath); opErr != nil {
		return nil, opErr
	} else if operation != "" {
		return nil, fmt.Errorf("refusing to checkpoint %s while Git operation %s is in progress", canonicalPath, operation)
	}
	status, err := gitOutput(ctx, canonicalPath, "status", "--porcelain", "--untracked-files=normal")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(status) == "" {
		return matchingAttemptCheckpointAtHEAD(ctx, canonicalPath, checkpoint)
	}
	if checkpoint.AttemptID == "" || checkpoint.WorkOrderID == "" || strings.TrimSpace(checkpoint.TerminationReason) == "" {
		return nil, fmt.Errorf("attempt ID, work-order ID, and termination reason are required to checkpoint %s", canonicalPath)
	}
	if _, err = gitOutput(ctx, canonicalPath, "add", "-A"); err != nil {
		return nil, fmt.Errorf("stage attempt checkpoint in %s: %w", canonicalPath, err)
	}
	subject := fmt.Sprintf("wip(%s): checkpoint at attempt death", checkpoint.AttemptID)
	body := fmt.Sprintf("Attempt-ID: %s\nWork-Order-ID: %s\nTermination-Reason: %s", checkpoint.AttemptID, checkpoint.WorkOrderID, oneLine(checkpoint.TerminationReason))
	if _, err = gitOutput(ctx, canonicalPath, "commit", "-m", subject, "-m", body); err != nil {
		return nil, fmt.Errorf("commit attempt checkpoint in %s: %w", canonicalPath, err)
	}
	commitSHA, err := gitOutput(ctx, canonicalPath, "rev-parse", "HEAD")
	if err != nil {
		return nil, err
	}
	commitSHA = strings.TrimSpace(commitSHA)
	if _, err = gitOutput(ctx, canonicalPath, "push", "origin", "HEAD:refs/heads/"+branch); err != nil {
		return nil, fmt.Errorf("checkpoint commit %s preserved locally in %s but push failed: %w", commitSHA, canonicalPath, err)
	}
	return &attemptCheckpointResult{Worktree: canonicalPath, CommitSHA: commitSHA, Pushed: true}, nil
}

func checkpointAssignedTaskWorktree(ctx context.Context, branch, repo, repoURL string, checkpoint attemptCheckpoint) (*attemptCheckpointResult, error) {
	root, err := repositoryRoot(ctx)
	if err != nil {
		return nil, fmt.Errorf("checkpoint must run inside the target repository: %w", err)
	}
	if err = gitx.VerifyRepositoryIdentity(ctx, root, repo, repoURL); err != nil {
		return nil, err
	}
	primary, err := primaryWorktreeRoot(ctx, root)
	if err != nil {
		return nil, err
	}
	worktrees, err := listRegisteredWorktrees(ctx, root)
	if err != nil {
		return nil, err
	}
	var assigned *registeredWorktree
	for i := range worktrees {
		if worktrees[i].Branch != "refs/heads/"+branch {
			continue
		}
		if assigned != nil {
			return nil, fmt.Errorf("task branch %s is registered in multiple worktrees", branch)
		}
		assigned = &worktrees[i]
	}
	if assigned == nil {
		return nil, nil
	}
	if assigned.Locked || assigned.Prunable {
		return nil, fmt.Errorf("task branch %s is registered in an unavailable worktree at %s", branch, assigned.Path)
	}
	return checkpointTaskWorktreeAtPath(ctx, assigned.Path, branch, primary, checkpoint)
}

func matchingAttemptCheckpointAtHEAD(ctx context.Context, path string, checkpoint attemptCheckpoint) (*attemptCheckpointResult, error) {
	if checkpoint.AttemptID == "" || checkpoint.WorkOrderID == "" || strings.TrimSpace(checkpoint.TerminationReason) == "" {
		return nil, nil
	}
	message, err := gitOutput(ctx, path, "show", "-s", "--format=%B", "HEAD")
	if err != nil {
		return nil, err
	}
	subject := fmt.Sprintf("wip(%s): checkpoint at attempt death", checkpoint.AttemptID)
	lines := strings.Split(strings.TrimSpace(message), "\n")
	if len(lines) == 0 || lines[0] != subject ||
		checkpointMessageField(lines, "Attempt-ID") != checkpoint.AttemptID ||
		checkpointMessageField(lines, "Work-Order-ID") != checkpoint.WorkOrderID {
		return nil, nil
	}
	if checkpointMessageField(lines, "Termination-Reason") != oneLine(checkpoint.TerminationReason) {
		return nil, fmt.Errorf("checkpoint at HEAD matches attempt %s and work order %s but not termination reason %q; refusing ambiguous reuse", checkpoint.AttemptID, checkpoint.WorkOrderID, oneLine(checkpoint.TerminationReason))
	}
	commitSHA, err := gitOutput(ctx, path, "rev-parse", "HEAD")
	if err != nil {
		return nil, err
	}
	commitSHA = strings.TrimSpace(commitSHA)
	branch, err := worktreeBranch(ctx, path)
	if err != nil {
		return nil, err
	}
	remote, err := gitOutput(ctx, path, "ls-remote", "--heads", "origin", "refs/heads/"+branch)
	if err != nil {
		return nil, err
	}
	pushed := strings.HasPrefix(strings.TrimSpace(remote), commitSHA+"\t")
	if !pushed {
		if _, err = gitOutput(ctx, path, "push", "origin", "HEAD:refs/heads/"+branch); err != nil {
			return nil, fmt.Errorf("checkpoint commit %s preserved locally in %s but push failed: %w", commitSHA, path, err)
		}
		pushed = true
	}
	return &attemptCheckpointResult{Worktree: path, CommitSHA: commitSHA, Pushed: pushed}, nil
}

func checkpointMessageField(lines []string, name string) string {
	prefix := name + ": "
	for _, line := range lines[1:] {
		if strings.HasPrefix(line, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(line, prefix))
		}
	}
	return ""
}

func oneLine(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

// removeTaskWorktree performs post-merge/close cleanup only. It intentionally
// retains the task branch so unmerged history is never deleted (design-git-delivery; DEC-10).
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
	cleanup, err := gitx.CleanupTaskWorktree(ctx, primary, branch)
	return worktreeCleanupResult(cleanup), err
}

func repositoryRoot(ctx context.Context) (string, error) {
	root, err := gitOutput(ctx, "", "rev-parse", "--show-toplevel")
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

func defaultImplicitWorktreeRoot() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home for implicit worktree root: %w", err)
	}
	return config.DefaultWorktreeRoot(home), nil
}

// implicitCheckoutDestination keeps the deterministic worktree name beneath
// one fixed canonical client-local root, independently of the primary
// checkout location (design-git-delivery).
func implicitCheckoutDestination(worktreeRoot, repo, taskID string) (string, error) {
	if !safeImplicitCheckoutComponent(repo) {
		return "", fmt.Errorf("refusing implicit checkout destination: repository name %q is not one safe path component", repo)
	}
	if !safeImplicitCheckoutComponent(taskID) {
		return "", fmt.Errorf("refusing implicit checkout destination: task ID %q is not one safe path component", taskID)
	}
	if !filepath.IsAbs(worktreeRoot) {
		return "", fmt.Errorf("refusing implicit checkout destination: worktree root %q is not absolute", worktreeRoot)
	}
	container := filepath.Clean(worktreeRoot)
	canonicalContainer, err := canonicalPathThroughExistingParent(container)
	if err != nil {
		return "", fmt.Errorf("resolve worktree root %s: %w", container, err)
	}
	if canonicalContainer != container {
		return "", fmt.Errorf("refusing implicit checkout destination: worktree root %s resolves outside the canonical path", container)
	}
	destination := filepath.Join(container, repo+"-task-"+taskID)
	canonicalDestination, err := canonicalPathThroughExistingParent(destination)
	if err != nil {
		return "", fmt.Errorf("resolve worktree path %s: %w", destination, err)
	}
	if canonicalDestination != filepath.Clean(destination) || filepath.Clean(filepath.Dir(canonicalDestination)) != container {
		return "", fmt.Errorf("refusing implicit checkout destination %s: resolved path is not inside canonical container %s", destination, container)
	}
	return canonicalDestination, nil
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
	if environment := gitEnvironmentFromContext(ctx); len(environment) > 0 {
		command.Env = isolatedChildEnvironment(os.Environ(), environment)
	}
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
	if environment := gitEnvironmentFromContext(ctx); len(environment) > 0 {
		command.Env = isolatedChildEnvironment(os.Environ(), environment)
	}
	return command.Run() == nil
}

type gitEnvironmentContextKey struct{}

func contextWithGitEnvironment(ctx context.Context, environment map[string]string) context.Context {
	if len(environment) == 0 {
		return ctx
	}
	copy := make(map[string]string, len(environment))
	for key, value := range environment {
		copy[key] = value
	}
	return context.WithValue(ctx, gitEnvironmentContextKey{}, copy)
}

func gitEnvironmentFromContext(ctx context.Context) map[string]string {
	environment, _ := ctx.Value(gitEnvironmentContextKey{}).(map[string]string)
	return environment
}
