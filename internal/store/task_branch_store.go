package store

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/gitx"
)

type BranchInUseError struct {
	Branch      string
	OtherTaskID string
}

func (e *BranchInUseError) Error() string {
	return fmt.Sprintf("%s: branch %s already belongs to task %s", ErrTaskBranchConflict, e.Branch, e.OtherTaskID)
}

func (e *BranchInUseError) Unwrap() error { return ErrTaskBranchConflict }

func TaskBranchInUseError(branch, otherID string) error {
	return &BranchInUseError{Branch: branch, OtherTaskID: otherID}
}

func openTaskHoldingBranch(tasks map[string]core.Task, workspace, repo, branch, exceptID string) string {
	if branch == "" {
		return ""
	}
	for _, existing := range tasks {
		if existing.ID == exceptID {
			continue
		}
		if existing.Workspace == workspace && existing.Repo == repo && existing.Branch == branch && !core.TaskTerminal(existing.State) {
			return existing.ID
		}
	}
	return ""
}

// EvaluateTaskBranchAttach applies the attach refusal matrix before any row
// write (req-task-branch-assignment REQ-2, REQ-4; DEC-42). Same-name on a
// non-terminal task is a no-op even when a pull request is recorded or a
// work order is claimed.
func EvaluateTaskBranchAttach(task core.Task, branch string, claimedWorkOrder, pullRequestOpened bool, occupyingOpenTaskID string) error {
	if !gitx.LegalBranchName(branch) {
		return fmt.Errorf("%w", ErrInvalidBranch)
	}
	if core.TaskTerminal(task.State) {
		return fmt.Errorf("%w: task %s", ErrTaskTerminal, task.ID)
	}
	if task.Branch == branch {
		return nil
	}
	if branch == strings.TrimSpace(task.BaseBranch) {
		return fmt.Errorf("%w", ErrBranchNotAttachable)
	}
	if id, ok := gitx.DefaultAssignmentTaskID(branch); ok && id != task.ID {
		return fmt.Errorf("%w", ErrBranchNotAttachable)
	}
	if claimedWorkOrder {
		return ErrWorkOrderClaimed
	}
	if pullRequestOpened {
		return ErrPullRequestRecorded
	}
	if occupyingOpenTaskID != "" {
		return TaskBranchInUseError(branch, occupyingOpenTaskID)
	}
	return nil
}

func (m *memory) AttachTaskBranch(ctx context.Context, taskID, branch string) (core.Task, error) {
	branch = strings.TrimSpace(branch)
	if !gitx.LegalBranchName(branch) {
		return core.Task{}, fmt.Errorf("%w", ErrInvalidBranch)
	}
	var result core.Task
	err := m.WithTaskSideEffectLock(ctx, taskID, func(ctx context.Context) error {
		m.mu.RLock()
		task, exists := m.tasks[taskID]
		m.mu.RUnlock()
		if !exists {
			return ErrNotFound
		}
		return m.WithTaskSideEffectLock(ctx, BranchCloseLockKey(task.Repo, branch), func(ctx context.Context) error {
			m.mu.Lock()
			defer m.mu.Unlock()
			t, ok := m.tasks[taskID]
			if !ok {
				return fmt.Errorf("%w: task %s", ErrNotFound, taskID)
			}
			claimed := false
			for _, order := range m.workOrders {
				if order.TaskID == taskID && order.State == core.WorkOrderClaimed {
					claimed = true
					break
				}
			}
			prOpened := false
			for _, event := range m.events[taskID] {
				if event.Kind == "pull_request.opened" {
					prOpened = true
					break
				}
			}
			occupying := openTaskHoldingBranch(m.tasks, t.Workspace, t.Repo, branch, taskID)
			if err := EvaluateTaskBranchAttach(t, branch, claimed, prOpened, occupying); err != nil {
				return err
			}
			if t.Branch == branch {
				result = t
				return nil
			}
			actor := ActorFromContext(ctx)
			if !utf8.ValidString(actor.ID) || !utf8.ValidString(string(actor.Role)) || strings.ContainsRune(actor.ID, '\x00') || strings.ContainsRune(string(actor.Role), '\x00') {
				return fmt.Errorf("audit actor must be valid UTF-8 without NUL characters")
			}
			previous := t.Branch
			t.Branch = branch
			m.tasks[taskID] = t
			m.appendEventLocked(ctx, core.Event{TaskID: taskID, Kind: "task.branch_attached", Payload: core.JSONPayload(map[string]string{"previous": previous, "branch": branch})})
			result = t
			return nil
		})
	})
	return result, err
}
