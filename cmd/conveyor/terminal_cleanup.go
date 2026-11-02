package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// conveyor done records each local cleanup on the user route
// POST /v1/tasks/{id}/worktree-cleanup with the caller's credential. The CLI
// calls no worker route (DEC-95; component-git-delivery).

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

func terminalCleanupPath(taskID string) string {
	return "/v1/tasks/" + url.PathEscape(taskID) + "/worktree-cleanup"
}

func (c *client) recordTerminalCleanupContext(ctx context.Context, taskID string, record terminalCleanupRecord) (terminalCleanupReceipt, error) {
	var result terminalCleanupReceipt
	payload, err := json.Marshal(record)
	if err != nil {
		return result, err
	}
	err = c.workerDoContext(ctx, http.MethodPost, terminalCleanupPath(taskID), payload, &result, c.token)
	return result, err
}

// recordDoneCleanup records the result of one conveyor done run. A refused or
// failed recording never changes the local result or the exit status; it
// returns the warning line done prints, or "" when the server confirmed the
// completion (req-task-branch-assignment AC-3.5).
func recordDoneCleanup(ctx context.Context, c *client, task core.Task, cleanup worktreeCleanupResult) string {
	receipt, err := c.recordTerminalCleanupContext(ctx, task.ID, terminalCleanupRecord{
		Repository: task.Repo, Branch: task.Branch,
		Worktree: cleanup.Worktree, BranchResult: cleanup.Branch, Path: cleanup.Path,
		ProcessWarnings: cleanup.ProcessWarnings,
	})
	switch {
	case err != nil:
		return fmt.Sprintf("worktree cleanup not recorded: %v", err)
	case !receipt.Completed:
		return "worktree cleanup not recorded: server did not confirm completion"
	}
	return ""
}
