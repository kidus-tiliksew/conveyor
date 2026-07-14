// Package gitx implements the Phase 1 bare-cache + isolated task checkout
// amendment to spec §8: one shared fetch-only bare mirror per repo seeds
// self-contained task clones. Sandboxes never mount the cache, so agents can
// write commits without receiving write access to shared refs or objects.
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
//	<JobsDir>/task-123/api/              isolated clone on conveyor/task-123
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

// SandboxPath is stable across runner hosts (spec §8.3 note 3).
func SandboxPath(taskID, repoName string) string {
	return filepath.Join("/conveyor/jobs", "task-"+taskID, repoName)
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

// AddWorktree creates the task branch from base in an isolated clone under
// JobsDir. The historical method name remains part of the manager API; the
// v1.1 amendment changed its storage strategy so the bare cache can stay
// entirely outside the sandbox.
func (m *Manager) AddWorktree(ctx context.Context, repoURL, repoName, taskID, base string) (string, error) {
	mirror, err := m.EnsureMirror(ctx, repoURL)
	if err != nil {
		return "", err
	}
	wt := filepath.Join(m.JobsDir, "task-"+taskID, repoName)
	// Re-dispatch resumes the existing worktree untouched (spec §8.3).
	if _, err := os.Stat(wt); err == nil {
		if err := syncTaskBranch(ctx, wt, BranchName(taskID)); err != nil {
			return "", err
		}
		return wt, nil
	}
	if err := os.MkdirAll(filepath.Dir(wt), 0o755); err != nil {
		return "", err
	}
	// --no-hardlinks makes the task repo self-contained: neither .git nor
	// object alternates point back to the host cache. The cache therefore
	// needs no sandbox mount at all (spec §8.1, §8.5).
	if err := run(ctx, "", "git", "clone", "--no-checkout", "--no-hardlinks", mirror, wt); err != nil {
		return "", err
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(wt)
		}
	}()
	if err := run(ctx, wt, "git", "remote", "set-url", "origin", repoURL); err != nil {
		return "", err
	}
	if err := run(ctx, wt, "git", "config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*"); err != nil {
		return "", err
	}

	baseRef := base
	if refExists(ctx, mirror, "refs/remotes/origin/"+base) {
		baseRef = "refs/remotes/origin/" + base
	}
	baseCommit, err := revParse(ctx, mirror, baseRef)
	if err != nil {
		return "", err
	}
	if err := run(ctx, wt, "git", "update-ref", "refs/remotes/origin/"+base, baseCommit); err != nil {
		return "", err
	}

	branch := BranchName(taskID)
	startCommit := baseCommit
	if refExists(ctx, mirror, "refs/heads/"+branch) {
		startCommit, err = revParse(ctx, mirror, "refs/heads/"+branch)
		if err != nil {
			return "", err
		}
	}
	if err := run(ctx, wt, "git", "checkout", "-b", branch, startCommit); err != nil {
		return "", err
	}
	cleanup = false
	return wt, nil
}

func syncTaskBranch(ctx context.Context, wt, branch string) error {
	// A missing remote branch is normal before an operator-owned agent's first
	// push; an existing agent-pushed branch is fast-forwarded only.
	remote, err := commandOutput(ctx, wt, "git", "ls-remote", "--heads", "origin", "refs/heads/"+branch)
	if err != nil {
		return err
	}
	if strings.TrimSpace(remote) == "" {
		return nil
	}
	remoteRef := "refs/remotes/origin/" + branch
	if err := run(ctx, wt, "git", "fetch", "origin", "+refs/heads/"+branch+":"+remoteRef); err != nil {
		return err
	}
	if refExists(ctx, wt, "HEAD") && isAncestor(ctx, wt, "HEAD", remoteRef) {
		return run(ctx, wt, "git", "merge", "--ff-only", remoteRef)
	}
	if isAncestor(ctx, wt, remoteRef, "HEAD") {
		return nil
	}
	return fmt.Errorf("task branch %s diverged between runner and origin", branch)
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

func isAncestor(ctx context.Context, repoDir, older, newer string) bool {
	cmd := exec.CommandContext(ctx, "git", "merge-base", "--is-ancestor", older, newer)
	cmd.Dir = repoDir
	return cmd.Run() == nil
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

// RemoveWorktree removes a task worktree; called on merge, close, or
// staleness TTL (default 14 days, spec §8.3).
func (m *Manager) RemoveWorktree(ctx context.Context, repoURL, repoName, taskID string) error {
	mirror, err := m.mirrorPath(repoURL)
	if err != nil {
		return err
	}
	wt := filepath.Join(m.JobsDir, "task-"+taskID, repoName)
	if _, err := os.Stat(wt); os.IsNotExist(err) {
		return nil
	}
	branch := BranchName(taskID)
	// Copy task-only objects and the branch ref back into the trusted cache
	// before eviction, so a later re-dispatch restores committed work. This
	// mutates the bare cache just like EnsureMirror's upstream fetch, so it must
	// take the same cross-process repository lock (spec §8.1).
	unlock, err := lockRepo(mirror)
	if err != nil {
		return err
	}
	fetchErr := run(ctx, mirror, "git", "fetch", "--no-tags", wt,
		"+refs/heads/"+branch+":refs/heads/"+branch)
	unlock()
	if fetchErr != nil {
		return fetchErr
	}
	return os.RemoveAll(wt)
}

// Prune keeps compatibility with the background maintenance hook. Isolated
// clones are removed directly; pruning also cleans metadata left by older
// linked-worktree deployments.
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
