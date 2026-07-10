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

// EnsureMirror clones or fetches the bare mirror for repoURL. Fetches
// into a bare cache are serialized with a per-repo lock; concurrent
// fetches into one bare repo are forbidden — ref corruption risk
// (spec §8.1).
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
		if err := run(ctx, "", "git", "clone", "--mirror", repoURL, dir); err != nil {
			return "", err
		}
		return dir, nil
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
	if err := os.MkdirAll(filepath.Dir(wt), 0o755); err != nil {
		return "", err
	}
	branch := BranchName(taskID)
	// -B: re-dispatch into an existing task branch reuses it (spec §8.3).
	if err := run(ctx, mirror, "git", "worktree", "add", "-B", branch, wt, base); err != nil {
		return "", err
	}
	return wt, nil
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
