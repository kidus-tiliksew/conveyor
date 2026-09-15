package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/kidus-tiliksew/conveyor/cmd/conveyor/localgit"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	workerservice "github.com/kidus-tiliksew/conveyor/internal/worker"
)

func checkoutWithWriter(ctx context.Context, c *client, taskID, branch, base, repo, repoURL, destination, worktreeRoot string) (string, error) {
	root, err := repositoryRoot(ctx)
	if err != nil {
		return "", err
	}
	expectedPath, err := worktreeWriterPath(ctx, root, branch, repo, repoURL)
	if err != nil {
		return "", err
	}
	if os.Getenv("CONVEYOR_WRITER_PATH") != expectedPath {
		return "", fmt.Errorf("writer lock does not belong to assigned repository and branch")
	}
	session := os.Getenv("CONVEYOR_SESSION_ID")
	generation := os.Getenv("CONVEYOR_WRITER_GENERATION")
	writer, err := joinWorktreeWriter(expectedPath, session, generation)
	if err != nil {
		return "", err
	}
	item := workerservice.DispatchOrder{Order: core.WorkOrder{ID: os.Getenv("CONVEYOR_WORK_ORDER_ID")}, Task: core.Task{ID: taskID}}
	request := core.WorktreeHandoffRequest{SessionID: session, Generation: generation, Action: "preserve"}
	h, err := c.worktreeHandoffContext(ctx, c.token, item, request)
	if err != nil {
		return "", err
	}
	if h.Writer.TaskID != taskID || h.Writer.Branch != branch || h.Writer.AttemptID != os.Getenv("CONVEYOR_CURRENT_ATTEMPT_ID") {
		return "", fmt.Errorf("current writer assignment differs from durable authority")
	}
	originIdentity, err := localgit.RepositoryOriginIdentity(ctx, root)
	if err != nil {
		return "", err
	}
	if h.Writer.Repository != originIdentity {
		return "", fmt.Errorf("server repository identity differs from checkout")
	}
	var checkpoint *attemptCheckpoint
	if raw := os.Getenv("CONVEYOR_PREDECESSOR"); raw != "" {
		var predecessor core.WorktreeIdentity
		if len(raw) > 8192 || json.Unmarshal([]byte(raw), &predecessor) != nil || h.Predecessor == nil {
			return "", fmt.Errorf("invalid predecessor descriptor")
		}
		if predecessor.TaskID != h.Predecessor.TaskID || predecessor.Workspace != h.Predecessor.Workspace || predecessor.Repository != h.Predecessor.Repository || predecessor.Branch != h.Predecessor.Branch || predecessor.WorkOrderID != h.Predecessor.WorkOrderID || predecessor.AttemptID != h.Predecessor.AttemptID || predecessor.SessionID != h.Predecessor.SessionID || predecessor.ClaimEventID != h.Predecessor.ClaimEventID {
			return "", fmt.Errorf("predecessor descriptor differs from durable evidence")
		}
		predecessor = *h.Predecessor
		reason := predecessor.Reason
		if reason == "" {
			reason = "successor recovered interrupted predecessor"
		}
		checkpoint = &attemptCheckpoint{AttemptID: predecessor.AttemptID, WorkOrderID: predecessor.WorkOrderID, TerminationReason: reason, Identity: &predecessor, Writer: writer, Authorize: func(ctx context.Context) error {
			if err := writer.verify(); err != nil {
				return err
			}
			_, err := c.worktreeHandoffContext(ctx, c.token, item, request)
			return err
		}}
	}
	path, preserved, err := checkoutTaskWithCheckpointAtRoot(ctx, branch, base, repo, repoURL, taskID, destination, worktreeRoot, checkpoint)
	if err != nil {
		return "", err
	}
	if preserved != nil {
		message, err := gitOutput(ctx, path, "show", "-s", "--format=%B", preserved.CommitSHA)
		if err != nil {
			return "", err
		}
		originalReason := checkpointMessageField(strings.Split(strings.TrimSpace(message), "\n"), "Termination-Reason")
		auditRequest := core.WorktreeHandoffRequest{SessionID: session, Generation: generation, Action: "audit", Producer: checkpoint.Identity, CommitSHA: preserved.CommitSHA, OriginalReason: originalReason}
		_, err = c.worktreeHandoffContext(ctx, c.token, item, auditRequest)
		if transientWorkerError(err) {
			_, err = c.worktreeHandoffContext(ctx, c.token, item, auditRequest)
		}

		if err != nil {
			return "", fmt.Errorf("checkpoint %s pushed but recovery audit pending: %w", preserved.CommitSHA, err)
		}
	}
	head, err := gitOutput(ctx, path, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	// After recovery succeeds, only this writer can produce subsequent edits.
	request.Action = "ready"
	if _, err = c.worktreeHandoffContext(ctx, c.token, item, request); err != nil {
		return "", err
	}
	writer.record.Producer = &h.Writer
	writer.record.Parent = strings.TrimSpace(head)
	writer.record.CommitSHA = ""
	if err = writer.save(); err != nil {
		return "", err
	}
	return path, nil
}
