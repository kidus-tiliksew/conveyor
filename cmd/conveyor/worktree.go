package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

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

// checkoutTask resolves one safe, task-dedicated checkout without switching or
// rewriting the operator's primary checkout (spec §21.8).
func checkoutTask(ctx context.Context, branch, base, repo, repoURL, taskID, destination string) (string, error) {
	root, err := repositoryRoot(ctx)
	if err != nil {
		return "", fmt.Errorf("checkout must run inside the target repository: %w", err)
	}
	// Identity precedes fetches, ref inspection, worktree reuse, and creation.
	// A directory label is never accepted as proof of repository ownership
	// (spec §8.2).
	if err := gitx.VerifyRepositoryIdentity(ctx, root, repo, repoURL); err != nil {
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
	if _, err := requireSafeWorktree(ctx, primary); err != nil {
		return "", fmt.Errorf("primary checkout is unsafe: %w", err)
	}
	if filepath.Clean(root) != filepath.Clean(primary) {
		if _, err := requireSafeWorktree(ctx, root); err != nil {
			return "", fmt.Errorf("current checkout is unsafe: %w", err)
		}
	}

	implicitDestination := destination == ""
	if implicitDestination {
		destination, err = implicitCheckoutDestination(primary, repo, taskID)
		if err != nil {
			return "", err
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
	if assigned != nil {
		if filepath.Clean(assigned.Path) == filepath.Clean(primary) && filepath.Clean(assigned.Path) != destination {
			return "", fmt.Errorf("task branch %s is checked out in the shared primary checkout %s", branch, primary)
		}
		if assigned.Locked || assigned.Prunable {
			return "", fmt.Errorf("task branch %s is registered in an unavailable worktree at %s", branch, assigned.Path)
		}
		actualBranch, err := requireSafeWorktree(ctx, assigned.Path)
		if err != nil {
			return "", fmt.Errorf("task worktree %s is unsafe: %w", assigned.Path, err)
		}
		if actualBranch != branch {
			return "", fmt.Errorf("task worktree %s owns %s instead of %s", assigned.Path, actualBranch, branch)
		}
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
			return "", fmt.Errorf("destination %s is already a registered worktree for %s", destination, entry.Branch)
		}
	}
	if _, err := os.Lstat(destination); err == nil {
		return "", fmt.Errorf("destination %s already exists but is not the assigned task worktree", destination)
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("inspect destination %s: %w", destination, err)
	}
	if implicitDestination {
		if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
			return "", fmt.Errorf("create implicit worktree container: %w", err)
		}
		validated, err := implicitCheckoutDestination(primary, repo, taskID)
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

// removeTaskWorktree performs post-merge/close cleanup only. It intentionally
// retains the task branch so unmerged history is never deleted (spec §21.8).
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

// implicitCheckoutDestination keeps the deterministic worktree name beneath
// one fixed canonical container directly beside the primary checkout,
// independently of configuration validation (spec §8.2).
func implicitCheckoutDestination(primary, repo, taskID string) (string, error) {
	if !safeImplicitCheckoutComponent(repo) {
		return "", fmt.Errorf("refusing implicit checkout destination: repository name %q is not one safe path component", repo)
	}
	if !safeImplicitCheckoutComponent(taskID) {
		return "", fmt.Errorf("refusing implicit checkout destination: task ID %q is not one safe path component", taskID)
	}
	canonicalPrimary, err := canonicalWorktreePath(primary)
	if err != nil {
		return "", fmt.Errorf("resolve primary checkout path: %w", err)
	}
	siblingParent := filepath.Dir(canonicalPrimary)
	container := filepath.Join(siblingParent, "conveyor-worktrees")
	if resolved, resolveErr := filepath.EvalSymlinks(container); resolveErr == nil {
		if filepath.Clean(resolved) != filepath.Clean(container) {
			return "", fmt.Errorf("refusing implicit checkout destination: container %s resolves outside the canonical path", container)
		}
	} else if !os.IsNotExist(resolveErr) {
		return "", fmt.Errorf("resolve worktree container %s: %w", container, resolveErr)
	}
	destination := filepath.Join(container, repo+"-task-"+taskID)
	if resolved, resolveErr := filepath.EvalSymlinks(destination); resolveErr == nil {
		destination = filepath.Clean(resolved)
	} else if !os.IsNotExist(resolveErr) {
		return "", fmt.Errorf("resolve worktree path %s: %w", destination, resolveErr)
	}
	if filepath.Clean(filepath.Dir(destination)) != filepath.Clean(container) {
		return "", fmt.Errorf("refusing implicit checkout destination %s: resolved path is not inside canonical container %s", destination, container)
	}
	return destination, nil
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
