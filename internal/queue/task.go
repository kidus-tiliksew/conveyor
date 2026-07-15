// Package queue defines durable River job contracts without importing the
// dispatcher. Keeping args in a neutral package lets the Postgres store insert
// jobs transactionally while workers remain in internal/dispatch (spec §17.0).
package queue

import (
	"crypto/sha256"
	"encoding/hex"
)

const ControlQueue = "control"

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
