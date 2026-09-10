// Package gitx retains local Git helpers and the planning-only snapshot mirror.
package gitx

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Manager owns the two host directories defined by design-git-delivery:
//
//	<CacheDir>/github.com/acme/api.git   bare, shared, fetch-only
//	<JobsDir>/task-123/api/              isolated clone on conveyor/task-123
type Manager struct {
	CacheDir string
	JobsDir  string
}

func NewManager(cacheDir, jobsDir string) *Manager {
	return &Manager{CacheDir: cacheDir, JobsDir: jobsDir}
}

// BranchName returns the task branch: conveyor/task-<id> (design-git-delivery).
func BranchName(taskID string) string {
	return "conveyor/task-" + taskID
}

// mirrorPath maps a repo URL to its bare cache path. file:// URLs
// (tests, fully local repos) cache under a "local" pseudo-host.
func (m *Manager) mirrorPath(repoURL string) (string, error) {
	u, err := url.Parse(repoURL)
	if err != nil {
		return "", fmt.Errorf("repo url %q: %v", repoURL, err)
	}
	host := u.Host
	if host == "" {
		if u.Scheme != "file" || u.Path == "" {
			return "", fmt.Errorf("repo url %q: no host", repoURL)
		}
		host = "local"
	}
	p := strings.TrimSuffix(strings.TrimPrefix(u.Path, "/"), ".git")
	return filepath.Join(m.CacheDir, host, p+".git"), nil
}

// EnsureMirror clones or fetches the bare cache for repoURL. Fetches
// into a bare cache are serialized with a per-repo lock; concurrent
// fetches into one bare repo are forbidden — ref corruption risk
// (design-git-delivery).
//
// Deliberately NOT `clone --mirror`: a mirror's +refs/*:refs/* refspec
// makes `fetch --prune` delete local conveyor/task-* branches the
// remote has never seen, and remote.origin.mirror=true turns any push
// from a worktree into a full mirror push. Instead, upstream refs live
// in their own namespace (refs/remotes/origin/*) so pruning only ever
// touches them, and task branches in refs/heads/* are never at risk.
func (m *Manager) EnsureMirror(ctx context.Context, repoURL string) (string, error) {
	dir, err := m.mirrorPath(repoURL)
	if err != nil {
		return "", err
	}
	unlock, err := lockRepo(dir)
	if err != nil {
		return "", err
	}
	defer unlock()

	if _, err := os.Stat(dir); os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
			return "", err
		}
		if err := run(ctx, "", "git", "clone", "--bare", repoURL, dir); err != nil {
			return "", err
		}
		if err := run(ctx, dir, "git", "config", "remote.origin.fetch",
			"+refs/heads/*:refs/remotes/origin/*"); err != nil {
			return "", err
		}
	}
	if err := run(ctx, dir, "git", "fetch", "--prune", "origin"); err != nil {
		return "", err
	}
	return dir, nil
}

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
