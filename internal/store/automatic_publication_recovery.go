package store

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// PublicationRetryCommand admits the bounded automatic re-enqueue of one
// failed forge publication (feature-bounded-automatic-recovery AR-5,
// component-durable-queue). The transition is the existing failed->retrying
// publication.retry command (DEC-50).
const PublicationRetryCommand core.WorkOrderCommand = "publication.retry"

// AutomaticPublicationKind names the publication outbox a retry belongs to.
type AutomaticPublicationKind string

const (
	AutomaticPublicationIssue  AutomaticPublicationKind = "github_issue"
	AutomaticPublicationReview AutomaticPublicationKind = "review"
)

// AutomaticPublicationRetry identifies one failed publication that is due
// for its automatic re-enqueue. Key is the task ID of a GitHub-issue
// lifecycle or the review work-order ID of a review publication.
type AutomaticPublicationRetry struct {
	Kind   AutomaticPublicationKind
	TaskID string
	Key    string
}

// RequestID names one automatic re-enqueue of the row at now. A row is
// re-enqueued at most once per window, so the ID never repeats for a row;
// concurrent sweeps at the same tick derive the same ID.
func (r AutomaticPublicationRetry) RequestID(now time.Time) string {
	kind := "issue"
	if r.Kind == AutomaticPublicationReview {
		kind = "review"
	}
	return "auto-publication/" + kind + "/" + r.Key + "/" + strconv.FormatInt(now.UTC().UnixMicro(), 10)
}

// ErrAutomaticPublicationJobActive rolls back a re-enqueue whose publication
// job is still active on its stream. The row stays failed for the next tick.
var ErrAutomaticPublicationJobActive = errors.New("publication job still active")

// AutomaticPublicationDue is the shared AR-5 publication rule. A failed
// publication is re-enqueued at most once per recovery window. A bound of 0
// disables it (AC-5.1). Tasks that are terminal, parked, or waiting at an
// operator gate are left alone (AC-2.3).
func AutomaticPublicationDue(policy config.RecoveryPolicy, taskState core.TaskState, failed bool, reenqueuedAt, now time.Time) bool {
	if !failed || policy.AutomaticRecoveries == 0 || policy.Window <= 0 {
		return false
	}
	if core.TaskTerminal(taskState) || taskState == core.TaskAwaiting || taskState == core.TaskParked {
		return false
	}
	return reenqueuedAt.IsZero() || !reenqueuedAt.After(now.Add(-policy.Window))
}

// AutomaticPublicationActor is the system actor of an automatic re-enqueue.
func AutomaticPublicationActor(taskID string) Actor {
	return SystemActor("queue:auto-publication:" + taskID)
}

// The automatic re-enqueue opens the row's recovery window: the next one is
// due one window after window_started_at, the stamp written with it.
type automaticGitHubIssueRetryPayload struct {
	core.GitHubLifecycle
	Automatic       bool      `json:"automatic"`
	RequestID       string    `json:"request_id"`
	WindowStartedAt time.Time `json:"window_started_at"`
}

type automaticReviewRetryPayload struct {
	core.ReviewPublication
	Automatic       bool      `json:"automatic"`
	RequestID       string    `json:"request_id"`
	WindowStartedAt time.Time `json:"window_started_at"`
}

// AutomaticGitHubIssueRetryEvent is the audit event of one automatic
// GitHub-issue re-enqueue (AR-6). The lifecycle carries the new state; at is
// the re-enqueue stamp and so the start of the row's recovery window.
func AutomaticGitHubIssueRetryEvent(lifecycle core.GitHubLifecycle, requestID string, at time.Time) core.Event {
	return core.Event{TaskID: lifecycle.TaskID, Kind: "github_issue.publication_retry", At: at,
		Payload: core.JSONPayload(automaticGitHubIssueRetryPayload{GitHubLifecycle: lifecycle, Automatic: true, RequestID: requestID, WindowStartedAt: at})}
}

// AutomaticReviewRetryEvent is the audit event of one automatic review
// publication re-enqueue (AR-6). The verdict fields are copied unchanged; at
// is the re-enqueue stamp and so the start of the row's recovery window.
func AutomaticReviewRetryEvent(publication core.ReviewPublication, requestID string, at time.Time) core.Event {
	return core.Event{TaskID: publication.TaskID, JobID: publication.JobID, Kind: "review.publication_retry", At: at,
		Payload: core.JSONPayload(automaticReviewRetryPayload{ReviewPublication: publication, Automatic: true, RequestID: requestID, WindowStartedAt: at})}
}

// ReconcileFailedPublications runs one AR-5 publication sweep. Each due row
// is re-enqueued by its own lease-bound store command, which re-checks the
// rule inside its transaction, so concurrent sweeps act once.
func ReconcileFailedPublications(ctx context.Context, st Store, now time.Time) (int, error) {
	retries, err := st.ListAutomaticPublicationRetries(ctx, now)
	if err != nil {
		return 0, err
	}
	reenqueued := 0
	for _, retry := range retries {
		retry := retry
		ok, err := taskops.ExecuteWorkOrder(WithActor(ctx, AutomaticPublicationActor(retry.TaskID)), st, retry.TaskID, PublicationRetryCommand, func(lease taskops.TaskLease) (bool, error) {
			return st.AutomaticPublicationRetryCommand(WithActor(ctx, AutomaticPublicationActor(retry.TaskID)), lease, retry, now)
		})
		if err != nil {
			return reenqueued, err
		}
		if ok {
			reenqueued++
		}
	}
	return reenqueued, nil
}
