package core

import (
	"errors"
	"strings"
	"time"
)

// Human approvals of an enforcing task's head (req-risk-based-review REQ-8,
// AC-8.3 to AC-8.8; component-risk-review invariants 9 and 10;
// feature-risk-based-review RR-7). An approval names one person and binds to
// the head and review round at which it was recorded. A new head always
// reaches the merge gate through a new review round, so approvals bound to an
// earlier round never count again, even when an earlier head returns.

// TaskApprovalKind is the closed kind of one approval record.
type TaskApprovalKind string

const (
	// TaskApprovalPerson is one person's approval of the head.
	TaskApprovalPerson TaskApprovalKind = "approval"
	// TaskApprovalAuthorWaiver is a non-author's waiver of the trusted
	// author's approval (req-risk-based-review AC-8.5). It counts toward no
	// number.
	TaskApprovalAuthorWaiver TaskApprovalKind = "author_waiver"
)

// TaskApproval is one recorded approval or author waiver. UserID is the
// person whose credential recorded it. Reason is set only on a waiver.
type TaskApproval struct {
	TaskID      string           `json:"task_id"`
	Kind        TaskApprovalKind `json:"kind"`
	HeadSHA     string           `json:"head_sha"`
	ReviewRound int              `json:"review_round"`
	UserID      string           `json:"user_id"`
	Reason      string           `json:"reason,omitempty"`
	// Source names the act that recorded an approval: "approval" for the
	// approvals route and the approve act at the merge gate, "merge" for a
	// merge act short of the required number.
	Source    string    `json:"source,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// TaskApprovalEvent and TaskAuthorWaiverEvent are the audited activity events
// of a recorded approval and waiver.
const (
	TaskApprovalEvent     = "risk.approval_recorded"
	TaskAuthorWaiverEvent = "risk.author_approval_waived"
)

// MaxApprovalWaiverReasonRunes bounds a waiver reason.
const MaxApprovalWaiverReasonRunes = 1000

// Validate checks a record before any write.
func (a TaskApproval) Validate() error {
	switch {
	case strings.TrimSpace(a.TaskID) == "":
		return errors.New("approval task is required")
	case a.Kind != TaskApprovalPerson && a.Kind != TaskApprovalAuthorWaiver:
		return errors.New("approval kind must be approval or author_waiver")
	case strings.TrimSpace(a.HeadSHA) == "" || strings.ContainsAny(a.HeadSHA, " \t\r\n"):
		return errors.New("approval head_sha is required")
	case a.ReviewRound < 1:
		return errors.New("approval review_round must be at least 1")
	case strings.TrimSpace(a.UserID) == "":
		return errors.New("an approval needs the person who records it")
	}
	if a.Kind == TaskApprovalAuthorWaiver {
		return ValidateApprovalWaiverReason(a.Reason)
	}
	if a.Reason != "" {
		return errors.New("an approval carries no reason")
	}
	return nil
}

// ValidateApprovalWaiverReason requires one non-blank line of at most
// MaxApprovalWaiverReasonRunes runes.
func ValidateApprovalWaiverReason(reason string) error {
	trimmed := strings.TrimSpace(reason)
	switch {
	case trimmed == "":
		return errors.New("reason is required")
	case strings.ContainsAny(reason, "\r\n"):
		return errors.New("reason must be one line")
	case len([]rune(reason)) > MaxApprovalWaiverReasonRunes:
		return errors.New("reason must be at most 1000 characters")
	}
	return nil
}

// TaskApprovalPayload is the activity payload of a recorded approval or
// waiver; it names the person, the head, and the round.
func TaskApprovalPayload(a TaskApproval) map[string]any {
	payload := map[string]any{"kind": string(a.Kind), "head_sha": a.HeadSHA, "review_round": a.ReviewRound, "user_id": a.UserID}
	if a.Reason != "" {
		payload["reason"] = a.Reason
	}
	if a.Source != "" {
		payload["source"] = a.Source
	}
	return payload
}
