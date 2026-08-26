package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	workerservice "github.com/kidus-tiliksew/conveyor/internal/worker"
)

type terminalCleanupStatus struct {
	Terminal  bool `json:"terminal"`
	Completed bool `json:"completed"`
}

type terminalCleanupRecord struct {
	Repository      string   `json:"repository"`
	Branch          string   `json:"branch"`
	Worktree        string   `json:"worktree"`
	BranchResult    string   `json:"branch_result"`
	Path            string   `json:"path"`
	ProcessWarnings []string `json:"process_warnings,omitempty"`
}

type terminalCleanupReceipt struct {
	Completed bool `json:"completed"`
	Recorded  bool `json:"recorded"`
}

type terminalCleanupAttempt struct {
	Completed bool
	Pending   bool
	Cleanup   worktreeCleanupResult
}

func terminalCleanupPath(dispatch, taskID string) (string, error) {
	escaped := url.PathEscape(taskID)
	switch dispatch {
	case "run":
		return "/v1/tasks/" + escaped + "/worktree-cleanup", nil
	case "worker":
		return "/v1/worker/tasks/" + escaped + "/worktree-cleanup", nil
	default:
		return "", fmt.Errorf("unsupported cleanup dispatch %q", dispatch)
	}
}

func (c *client) terminalCleanupStatusContext(ctx context.Context, credential, dispatch, taskID string) (terminalCleanupStatus, error) {
	var result terminalCleanupStatus
	path, err := terminalCleanupPath(dispatch, taskID)
	if err != nil {
		return result, err
	}
	err = c.workerDoContext(ctx, http.MethodGet, path, nil, &result, credential)
	return result, err
}

func (c *client) recordTerminalCleanupContext(ctx context.Context, credential, dispatch, taskID string, record terminalCleanupRecord) (terminalCleanupReceipt, error) {
	var result terminalCleanupReceipt
	path, err := terminalCleanupPath(dispatch, taskID)
	if err != nil {
		return result, err
	}
	payload, err := json.Marshal(record)
	if err != nil {
		return result, err
	}
	err = c.workerDoContext(ctx, http.MethodPost, path, payload, &result, credential)
	return result, err
}

var cleanupTerminalTaskWorktree = func(ctx context.Context, local *config.Config, item workerservice.DispatchOrder) (worktreeCleanupResult, error) {
	if local == nil {
		return worktreeCleanupResult{}, fmt.Errorf("local execution configuration is unavailable")
	}
	if strings.TrimSpace(item.Repository.URL) == "" {
		if repository, ok := local.Repo(item.Task.Repo); ok {
			item.Repository = repository
		}
	}
	primary, err := resolveHarnessWorkingDirectory(ctx, local, item)
	if err != nil {
		return worktreeCleanupResult{}, err
	}
	return removeTaskWorktreeAtPrimary(ctx, primary, item.Task.Branch, item.Task.State)
}

// attemptTerminalWorktreeCleanup checks the durable completion marker before
// touching Git. Cleanup and completion recording are separate so a temporary
// recording failure retries safely after the local worktree is already gone.
func attemptTerminalWorktreeCleanup(ctx context.Context, c *client, credential string, item workerservice.DispatchOrder, local *config.Config) (terminalCleanupAttempt, error) {
	status, err := c.terminalCleanupStatusContext(ctx, credential, item.Dispatch, item.Task.ID)
	if err != nil {
		return terminalCleanupAttempt{}, fmt.Errorf("check recorded completion: %w", err)
	}
	if status.Completed {
		return terminalCleanupAttempt{Completed: true}, nil
	}
	if !status.Terminal {
		return terminalCleanupAttempt{Pending: true}, nil
	}
	if item.Task.State != core.TaskMerged && item.Task.State != core.TaskClosed {
		item.Task.State = core.TaskMerged
	}
	cleanup, err := cleanupTerminalTaskWorktree(ctx, local, item)
	if err != nil {
		return terminalCleanupAttempt{}, fmt.Errorf("remove local task worktree: %w", err)
	}
	receipt, err := c.recordTerminalCleanupContext(ctx, credential, item.Dispatch, item.Task.ID, terminalCleanupRecord{
		Repository: item.Task.Repo, Branch: item.Task.Branch,
		Worktree: cleanup.Worktree, BranchResult: cleanup.Branch, Path: cleanup.Path,
		ProcessWarnings: cleanup.ProcessWarnings,
	})
	if err != nil {
		return terminalCleanupAttempt{Cleanup: cleanup}, fmt.Errorf("record completion: %w", err)
	}
	if !receipt.Completed {
		return terminalCleanupAttempt{Cleanup: cleanup}, fmt.Errorf("record completion: server did not confirm completion")
	}
	return terminalCleanupAttempt{Completed: true, Cleanup: cleanup}, nil
}
