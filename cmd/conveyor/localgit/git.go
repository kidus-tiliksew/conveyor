package localgit

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

func commandOutput(ctx context.Context, dir, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, out)
	}
	return string(out), nil
}

// CommitsAhead lists commit hashes on the worktree's HEAD that are not
// on the base branch — the dispatcher's "did the agent produce
// anything" check.
func CommitsAhead(ctx context.Context, worktreeDir, base string) ([]string, error) {
	ref := "refs/remotes/origin/" + base
	if !refExists(ctx, worktreeDir, ref) {
		ref = base
	}
	cmd := exec.CommandContext(ctx, "git", "log", "--format=%H", ref+"..HEAD")
	cmd.Dir = worktreeDir
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git log %s..HEAD: %w", ref, err)
	}
	var commits []string
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if l != "" {
			commits = append(commits, l)
		}
	}
	return commits, nil
}

// DiffAgainstBase returns the review input for the independent review stage.
func DiffAgainstBase(ctx context.Context, worktreeDir, base string) (string, error) {
	ref := "refs/remotes/origin/" + base
	if !refExists(ctx, worktreeDir, ref) {
		ref = base
	}
	return commandOutput(ctx, worktreeDir, "git", "diff", "--no-ext-diff", ref+"...HEAD")
}

func refExists(ctx context.Context, repoDir, ref string) bool {
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--verify", "--quiet", ref)
	cmd.Dir = repoDir
	return cmd.Run() == nil
}

func revParse(ctx context.Context, repoDir, ref string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--verify", ref)
	cmd.Dir = repoDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git rev-parse %s: %w: %s", ref, err, out)
	}
	return strings.TrimSpace(string(out)), nil
}

func run(ctx context.Context, dir string, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, out)
	}
	return nil
}
