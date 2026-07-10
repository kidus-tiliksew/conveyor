// Package gitx implements bare-clone + worktree management (spec §8):
// one shared bare mirror per repo, many task worktrees checked out from
// it. Creating a task workspace costs a checkout, not a clone.
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

// Manager owns the two host directories from spec §8.1:
//
//	<CacheDir>/github.com/acme/api.git   bare, shared, fetch-only
//	<JobsDir>/task-123/api/              worktree on conveyor/task-123
type Manager struct {
	CacheDir string
	JobsDir  string
}

func NewManager(cacheDir, jobsDir string) *Manager {
	return &Manager{CacheDir: cacheDir, JobsDir: jobsDir}
}

// BranchName returns the task branch: conveyor/task-<id> (spec §8.2).
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
// (spec §8.1).
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

// AddWorktree creates the task branch from base and checks out a
// worktree for it under JobsDir. repoName is the directory name inside
// the task's job dir (one per repo in the worktree set, spec §7).
func (m *Manager) AddWorktree(ctx context.Context, repoURL, repoName, taskID, base string) (string, error) {
	mirror, err := m.EnsureMirror(ctx, repoURL)
	if err != nil {
		return "", err
	}
	wt := filepath.Join(m.JobsDir, "task-"+taskID, repoName)
	// Re-dispatch resumes the existing worktree untouched (spec §8.3).
	if _, err := os.Stat(wt); err == nil {
		return wt, nil
	}
	if err := os.MkdirAll(filepath.Dir(wt), 0o755); err != nil {
		return "", err
	}
	branch := BranchName(taskID)
	if refExists(ctx, mirror, "refs/heads/"+branch) {
		// The task branch survives worktree removal (eviction, GC) and
		// carries committed work — check it out without resetting.
		if err := run(ctx, mirror, "git", "worktree", "add", wt, branch); err != nil {
			return "", err
		}
		return wt, nil
	}
	// New branch: cut from the freshly fetched upstream ref when the
	// base is a remote branch; a plain ref (e.g. a stacked parent's
	// task branch, spec §8.6) is used as-is.
	start := base
	if refExists(ctx, mirror, "refs/remotes/origin/"+base) {
		start = "refs/remotes/origin/" + base
	}
	if err := run(ctx, mirror, "git", "worktree", "add", "-b", branch, wt, start); err != nil {
		return "", err
	}
	return wt, nil
}

func refExists(ctx context.Context, repoDir, ref string) bool {
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--verify", "--quiet", ref)
	cmd.Dir = repoDir
	return cmd.Run() == nil
}

// RemoveWorktree removes a task worktree; called on merge, close, or
// staleness TTL (default 14 days, spec §8.3).
func (m *Manager) RemoveWorktree(ctx context.Context, repoURL, repoName, taskID string) error {
	mirror, err := m.mirrorPath(repoURL)
	if err != nil {
		return err
	}
	wt := filepath.Join(m.JobsDir, "task-"+taskID, repoName)
	return run(ctx, mirror, "git", "worktree", "remove", "--force", wt)
}

// Prune runs `git worktree prune` across all mirrors — the background
// chore from spec §8.3. Orphaned-worktree disk creep is a monitored
// metric.
func (m *Manager) Prune(ctx context.Context) error {
	return filepath.WalkDir(m.CacheDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || !d.IsDir() || !strings.HasSuffix(path, ".git") {
			return err
		}
		if err := run(ctx, path, "git", "worktree", "prune"); err != nil {
			return err
		}
		return filepath.SkipDir
	})
}

func run(ctx context.Context, dir string, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, out)
	}
	return nil
}
