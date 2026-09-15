package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/kidus-tiliksew/conveyor/cmd/conveyor/localgit"
	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// The OS lock spans the writing child and its final Git/audit operations.
// The atomic record survives process death but never grants authority alone
// (component-git-delivery CP-2 and CP-3).
type worktreeWriterRecord struct {
	Writer    core.WorktreeIdentity  `json:"writer"`
	Producer  *core.WorktreeIdentity `json:"producer,omitempty"`
	Parent    string                 `json:"parent,omitempty"`
	CommitSHA string                 `json:"commit_sha,omitempty"`
}
type worktreeWriter struct {
	file   *os.File
	path   string
	record worktreeWriterRecord
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
	w := &worktreeWriter{file: f, path: path}
	data, err := os.ReadFile(path + ".json")
	if err == nil {
		err = json.Unmarshal(data, &w.record)
	}
	if err != nil && !os.IsNotExist(err) {
		w.close()
		return nil, fmt.Errorf("read writer provenance: %w", err)
	}
	return w, nil
}
func (w *worktreeWriter) close() {
	if w != nil && w.file != nil {
		_ = syscall.Flock(int(w.file.Fd()), syscall.LOCK_UN)
		_ = w.file.Close()
		w.file = nil
	}
}
func (w *worktreeWriter) save() error {
	data, err := json.Marshal(w.record)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(w.path), "writer-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(name, w.path+".json"); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(w.path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
func (w *worktreeWriter) verify() error {
	var actual worktreeWriterRecord
	data, err := os.ReadFile(w.path + ".json")
	if err != nil {
		return err
	}
	if err = json.Unmarshal(data, &actual); err != nil {
		return err
	}
	if actual.Writer.SessionID != w.record.Writer.SessionID || actual.Writer.Generation != w.record.Writer.Generation {
		return fmt.Errorf("worktree writer generation was superseded")
	}
	return nil
}
func joinWorktreeWriter(path, session, generation string) (*worktreeWriter, error) {
	if path == "" || session == "" || generation == "" {
		return nil, fmt.Errorf("missing launcher writer identity")
	}
	data, err := os.ReadFile(path + ".json")
	if err != nil {
		return nil, err
	}
	w := &worktreeWriter{path: path}
	if err = json.Unmarshal(data, &w.record); err != nil {
		return nil, err
	}
	if w.record.Writer.SessionID != session || w.record.Writer.Generation != generation {
		return nil, fmt.Errorf("checkout writer identity does not match launcher")
	}
	f, err := os.OpenFile(path+".lock", os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		return nil, fmt.Errorf("launcher no longer owns worktree lock")
	}
	if err != syscall.EWOULDBLOCK && err != syscall.EAGAIN {
		return nil, err
	}
	return w, nil
}

func prepareWorktreeWriter(ctx context.Context, root string, w *worktreeWriter, h core.WorktreeHandoff) error {
	// A record from another repository/task cannot explain leftover content.
	if p := w.record.Producer; p != nil {
		if h.Predecessor == nil || p.Workspace != h.Predecessor.Workspace || p.TaskID != h.Predecessor.TaskID || p.Repository != h.Predecessor.Repository || p.Branch != h.Predecessor.Branch || p.WorkOrderID != h.Predecessor.WorkOrderID || p.AttemptID != h.Predecessor.AttemptID || p.SessionID != h.Predecessor.SessionID {
			return fmt.Errorf("local producer record does not match durable predecessor")
		}
	}
	worktrees, err := listRegisteredWorktrees(ctx, root)
	if err != nil {
		return err
	}
	for _, entry := range worktrees {
		if entry.Branch == "refs/heads/"+h.Writer.Branch {
			status, err := gitOutput(ctx, entry.Path, "status", "--porcelain", "--untracked-files=normal")
			if err != nil {
				return err
			}
			if strings.TrimSpace(status) != "" && w.record.Producer == nil {
				return fmt.Errorf("dirty task worktree has no producing writer evidence")
			}
		}
	}
	w.record.Writer = h.Writer
	if h.Predecessor == nil {
		p := h.Writer
		w.record.Producer = &p
	}
	return w.save()
}
