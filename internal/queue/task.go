// Package queue defines the durable job contracts without importing the
// dispatcher. Keeping args in a neutral package lets the store enqueue jobs
// transactionally while handlers remain in internal/dispatch
// (design-task-lifecycle).
package queue

import (
	"encoding/json"
	"time"
)

const (
	// The initial execution counts toward MaxAttempts, so five scheduled
	// retries require six total executions (design-task-lifecycle).
	DispatchTaskRetryLimit    = 5
	DispatchTaskMaxAttempts   = DispatchTaskRetryLimit + 1
	DispatchRetryInitialDelay = 10 * time.Second
	DispatchRetryMaximumDelay = 5 * time.Minute
)

// DispatchTaskRetryDelay returns the bounded T12/T13 backoff for the attempt
// that just failed (design-task-lifecycle).
func DispatchTaskRetryDelay(attempt int) time.Duration {
	delay := DispatchRetryInitialDelay
	for step := 1; step < attempt && delay < DispatchRetryMaximumDelay; step++ {
		if delay > DispatchRetryMaximumDelay/2 {
			return DispatchRetryMaximumDelay
		}
		delay *= 2
	}
	if delay > DispatchRetryMaximumDelay {
		return DispatchRetryMaximumDelay
	}
	return delay
}

type DispatchTaskArgs struct {
	WorkspaceID string `json:"workspace_id"`
	TaskID      string `json:"task_id"`
}

func (DispatchTaskArgs) Kind() string { return "dispatch_task" }

// UniqueKey is the per-workspace key one active job may hold at a time.
func (a DispatchTaskArgs) UniqueKey() string { return a.TaskID }

type ReviewPublicationArgs struct {
	WorkspaceID       string `json:"workspace_id"`
	ReviewWorkOrderID string `json:"review_work_order_id"`
}

func (ReviewPublicationArgs) Kind() string { return "review_publication" }

func (a ReviewPublicationArgs) UniqueKey() string { return a.ReviewWorkOrderID }

type GitHubIssuePublicationArgs struct {
	WorkspaceID string `json:"workspace_id"`
	TaskID      string `json:"task_id"`
}

func (GitHubIssuePublicationArgs) Kind() string { return "github_issue_publication" }

func (a GitHubIssuePublicationArgs) UniqueKey() string { return a.TaskID }

type OrderClockArgs struct {
	WorkspaceID string `json:"workspace_id"`
}

func (OrderClockArgs) Kind() string { return "order_clock" }

// Identity decodes a job's workspace and unique key from its encoded args
// by kind. The clock has no key: it is periodic, not queued per entity.
func Identity(kind string, args []byte) (workspace, key string, ok bool) {
	var decoded struct {
		WorkspaceID       string `json:"workspace_id"`
		TaskID            string `json:"task_id"`
		ReviewWorkOrderID string `json:"review_work_order_id"`
	}
	if err := json.Unmarshal(args, &decoded); err != nil {
		return "", "", false
	}
	switch kind {
	case DispatchTaskArgs{}.Kind(), GitHubIssuePublicationArgs{}.Kind():
		return decoded.WorkspaceID, decoded.TaskID, decoded.TaskID != ""
	case ReviewPublicationArgs{}.Kind():
		return decoded.WorkspaceID, decoded.ReviewWorkOrderID, decoded.ReviewWorkOrderID != ""
	}
	return decoded.WorkspaceID, "", false
}
