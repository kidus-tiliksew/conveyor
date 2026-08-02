package gitx

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type TaskWorktreeCleanupResult struct {
	Worktree        string
	Branch          string
	Path            string
	ProcessWarnings []string
}

type linkedWorktree struct {
	Path     string
	Branch   string
	Locked   bool
	Prunable bool
}

// CleanupTaskWorktree removes one clean registered task worktree while
// retaining its branch. Missing directories are pruned as stale registrations;
// a missing registration is an idempotent no-op (spec §8.1, §8.2).
func CleanupTaskWorktree(ctx context.Context, primary, branch string) (TaskWorktreeCleanupResult, error) {
	result := TaskWorktreeCleanupResult{Worktree: "skipped", Branch: "absent", Path: "-"}
	root, err := repositoryRootAt(ctx, primary)
	if err != nil {
		return result, fmt.Errorf("resolve primary checkout: %w", err)
	}
	actualPrimary, err := primaryCheckoutRoot(ctx, root)
	if err != nil {
		return result, fmt.Errorf("resolve primary checkout: %w", err)
	}
	if !samePath(root, actualPrimary) {
		return result, fmt.Errorf("cleanup must run against the repository primary checkout")
	}
	if refExists(ctx, root, "refs/heads/"+branch) {
		result.Branch = "retained"
	}

	worktrees, err := listLinkedWorktrees(ctx, root)
	if err != nil {
		return result, err
	}
	var assigned *linkedWorktree
	for i := range worktrees {
		if worktrees[i].Branch != "refs/heads/"+branch {
			continue
		}
		if assigned != nil {
			return result, fmt.Errorf("task branch %s is registered in multiple worktrees", branch)
		}
		assigned = &worktrees[i]
	}
	if assigned == nil {
		return result, nil
	}
	result.Path = assigned.Path
	if samePath(assigned.Path, actualPrimary) {
		return result, fmt.Errorf("refusing to remove the primary checkout")
	}
	if assigned.Locked {
		return result, fmt.Errorf("refusing to remove locked task worktree %s", assigned.Path)
	}

	if _, err := os.Stat(assigned.Path); os.IsNotExist(err) {
		if err := run(ctx, root, "git", "worktree", "prune", "--expire", "now"); err != nil {
			return result, fmt.Errorf("prune stale task worktree registration %s: %w", assigned.Path, err)
		}
		remaining, listErr := listLinkedWorktrees(ctx, root)
		if listErr != nil {
			return result, listErr
		}
		for _, entry := range remaining {
			if entry.Branch == "refs/heads/"+branch {
				return result, fmt.Errorf("stale task worktree registration %s remains after prune", assigned.Path)
			}
		}
		result.Worktree = "pruned"
		return result, nil
	} else if err != nil {
		return result, fmt.Errorf("inspect task worktree %s: %w", assigned.Path, err)
	}

	actualBranch, err := safeLinkedWorktree(ctx, assigned.Path)
	if err != nil {
		return result, fmt.Errorf("refusing task worktree cleanup: %w", err)
	}
	if actualBranch != branch {
		return result, fmt.Errorf("task worktree %s owns %s instead of %s", assigned.Path, actualBranch, branch)
	}
	processes, inspectErr := processesWithinPath(ctx, assigned.Path)
	for _, process := range processes {
		result.ProcessWarnings = append(result.ProcessWarnings, fmt.Sprintf(
			"live worktree process pid=%d command=%s cwd=%s", process.PID, process.Command, process.CWD,
		))
	}
	if inspectErr != nil {
		result.ProcessWarnings = append(result.ProcessWarnings, fmt.Sprintf(
			"worktree process inspection for %s was incomplete: %v", assigned.Path, inspectErr,
		))
	}
	if err := run(ctx, root, "git", "worktree", "remove", assigned.Path); err != nil {
		return result, err
	}
	result.Worktree = "removed"
	return result, nil
}

// PruneRepository removes stale linked-worktree registrations only. Git's
// prune operation leaves live worktrees, primary checkouts, and branches
// untouched.
func PruneRepository(ctx context.Context, primary string) error {
	root, err := repositoryRootAt(ctx, primary)
	if err != nil {
		return err
	}
	actualPrimary, err := primaryCheckoutRoot(ctx, root)
	if err != nil {
		return err
	}
	if !samePath(root, actualPrimary) {
		return fmt.Errorf("worktree prune must run against the repository primary checkout")
	}
	return run(ctx, root, "git", "worktree", "prune")
}

func listLinkedWorktrees(ctx context.Context, root string) ([]linkedWorktree, error) {
	listing, err := commandOutput(ctx, root, "git", "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	var entries []linkedWorktree
	var current *linkedWorktree
	for _, line := range strings.Split(listing, "\n") {
		switch {
		case strings.HasPrefix(line, "worktree "):
			entries = append(entries, linkedWorktree{Path: filepath.Clean(strings.TrimPrefix(line, "worktree "))})
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

func safeLinkedWorktree(ctx context.Context, path string) (string, error) {
	branch, err := commandOutput(ctx, path, "git", "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		return "", fmt.Errorf("detached HEAD or unreadable branch")
	}
	status, err := commandOutput(ctx, path, "git", "status", "--porcelain", "--untracked-files=normal")
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(status) != "" {
		return "", fmt.Errorf("worktree has uncommitted or untracked changes")
	}
	for _, marker := range []string{
		"MERGE_HEAD", "CHERRY_PICK_HEAD", "REVERT_HEAD", "REBASE_HEAD", "BISECT_LOG",
		"rebase-merge", "rebase-apply", "sequencer",
	} {
		markerPath, markerErr := commandOutput(ctx, path, "git", "rev-parse", "--git-path", marker)
		if markerErr != nil {
			return "", markerErr
		}
		resolved := strings.TrimSpace(markerPath)
		if !filepath.IsAbs(resolved) {
			resolved = filepath.Join(path, resolved)
		}
		if _, statErr := os.Stat(resolved); statErr == nil {
			return "", fmt.Errorf("Git operation %s is in progress", marker)
		} else if !os.IsNotExist(statErr) {
			return "", fmt.Errorf("inspect Git operation marker %s: %w", marker, statErr)
		}
	}
	return strings.TrimSpace(branch), nil
}

func samePath(left, right string) bool {
	leftResolved, leftErr := filepath.EvalSymlinks(left)
	if leftErr == nil {
		left = leftResolved
	}
	rightResolved, rightErr := filepath.EvalSymlinks(right)
	if rightErr == nil {
		right = rightResolved
	}
	return filepath.Clean(left) == filepath.Clean(right)
}
