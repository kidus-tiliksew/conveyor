package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/kidus-tiliksew/conveyor/cmd/conveyor/localgit"
)

// worktreeWriter is the exclusive local writer lock that conveyor checkout
// holds for its own duration, so two checkouts of one task branch never
// resolve the worktree at once. Process death releases it
// (component-attempt-checkpoints).
type worktreeWriter struct {
	file *os.File
}

func worktreeWriterPath(ctx context.Context, root, branch, repo, repoURL string) (string, error) {
	if err := localgit.VerifyRepositoryIdentity(ctx, root, repo, repoURL); err != nil {
		return "", err
	}
	common, err := gitOutput(ctx, root, "rev-parse", "--git-common-dir")
	if err != nil {
		return "", err
	}
	common = strings.TrimSpace(common)
	if !filepath.IsAbs(common) {
		common = filepath.Join(root, common)
	}
	common, err = filepath.EvalSymlinks(common)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(branch))
	return filepath.Join(common, "conveyor-writers", fmt.Sprintf("%x", sum)), nil
}
func acquireWorktreeWriter(ctx context.Context, path string) (*worktreeWriter, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EAGAIN {
			f.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	return &worktreeWriter{file: f}, nil
}
func (w *worktreeWriter) close() {
	if w != nil && w.file != nil {
		_ = syscall.Flock(int(w.file.Fd()), syscall.LOCK_UN)
		_ = w.file.Close()
		w.file = nil
	}
}
