// Package queue defines durable River job contracts without importing the
// dispatcher. Keeping args in a neutral package lets the Postgres store insert
// jobs transactionally while workers remain in internal/dispatch (spec §17.0).
package queue

import (
	"crypto/sha256"
	"encoding/hex"
	"time"
)

const ControlQueue = "control"

const (
	DispatchTaskMaxAttempts   = 5
	DispatchRetryInitialDelay = 10 * time.Second
	DispatchRetryMaximumDelay = 5 * time.Minute
)

// DispatchTaskRetryDelay returns the bounded T12/T13 backoff for the attempt
// that just failed (spec §3.3, §21.41).
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
	WorkspaceID string `json:"workspace_id" river:"unique"`
	TaskID      string `json:"task_id" river:"unique"`
}

func (DispatchTaskArgs) Kind() string { return "dispatch_task" }

type ReviewPublicationArgs struct {
	WorkspaceID       string `json:"workspace_id" river:"unique"`
	ReviewWorkOrderID string `json:"review_work_order_id" river:"unique"`
}

func (ReviewPublicationArgs) Kind() string { return "review_publication" }

type GitHubIssuePublicationArgs struct {
	WorkspaceID string `json:"workspace_id" river:"unique"`
	TaskID      string `json:"task_id" river:"unique"`
}

func (GitHubIssuePublicationArgs) Kind() string { return "github_issue_publication" }

// DispatchQueue isolates workers by workspace even when multiple workspace
// daemons share one Postgres/River cluster. Hashing avoids leaking workspace
// names into queue identifiers and satisfies River's queue-name grammar.
func DispatchQueue(workspace string) string {
	digest := sha256.Sum256([]byte(workspace))
	return "dispatch_" + hex.EncodeToString(digest[:8])
}

func ReviewPublicationQueue(workspace string) string {
	digest := sha256.Sum256([]byte(workspace))
	return "review_publication_" + hex.EncodeToString(digest[:8])
}

func GitHubIssuePublicationQueue(workspace string) string {
	digest := sha256.Sum256([]byte(workspace))
	return "github_issue_publication_" + hex.EncodeToString(digest[:8])
}
