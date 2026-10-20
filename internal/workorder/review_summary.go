package workorder

// Review round summary admission (req-review-gates-evidence AC-10.3; DEC-79,
// DEC-19; feature-review-round-summary SU-3).

import (
	"context"
	"errors"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// ReviewSummaryInput is one protocol-neutral request to summarize a settled
// review round. The author's identity and role come from the authenticated
// context, never from this input; AuthorModel is reported text.
type ReviewSummaryInput struct {
	TaskID      string
	ReviewRound int
	RequestID   string
	Summary     string
	Findings    []store.ReviewSummaryFinding
	AuthorModel string
	WorkOrderID string
	SessionID   string
}

// errReviewSummaryRefused marks an admission refusal internally; callers only
// ever see store.ErrWorkOrderClaimUnauthorized.
var errReviewSummaryRefused = errors.New("review summary admission refused")

// RecordReviewSummary admits the caller and records the summary. A user
// credential needs live claim_work membership in the selected workspace. An
// agent credential must present the work_order_id and session_id of an order
// of the same task whose session matches and whose state is claimed with a
// live lease, submitted, or completed, so seat sessions and the bounced
// implementation session are admitted (DEC-79(4)). Every refusal, including a
// worker or missing credential and a missing or foreign task, is the same
// store.ErrWorkOrderClaimUnauthorized, decided before validation, so a refusal
// never discloses whether the task exists or its round settled (DEC-19).
// Admission reads only; it renews, recovers, and records nothing.
func (s *Service) RecordReviewSummary(ctx context.Context, input ReviewSummaryInput) (store.ReviewSummaryResult, error) {
	author, err := s.admitReviewSummary(ctx, input)
	if errors.Is(err, errReviewSummaryRefused) {
		return store.ReviewSummaryResult{}, store.ErrWorkOrderClaimUnauthorized
	}
	if err != nil {
		return store.ReviewSummaryResult{}, err
	}
	result, err := s.Store.RecordReviewSummaryCommand(ctx, store.ReviewSummaryRequest{
		TaskID: input.TaskID, ReviewRound: input.ReviewRound, RequestID: input.RequestID,
		Summary: input.Summary, Findings: input.Findings, Author: author,
	})
	if errors.Is(err, store.ErrNotFound) {
		// The task disappeared between admission and the command.
		return store.ReviewSummaryResult{}, store.ErrWorkOrderClaimUnauthorized
	}
	return result, err
}

func (s *Service) admitReviewSummary(ctx context.Context, input ReviewSummaryInput) (store.ReviewSummaryAuthor, error) {
	refused := store.ReviewSummaryAuthor{}
	credential, ok := store.CredentialFromContext(ctx)
	actor := store.ActorFromContext(ctx)
	workspace, scoped := store.WorkspaceFromContext(ctx)
	if !ok || !scoped || workspace == "" || !actor.Complete() || actor.Role == core.ActorWorker || input.TaskID == "" {
		return refused, errReviewSummaryRefused
	}
	author := store.ReviewSummaryAuthor{ActorID: actor.ID, ActorRole: actor.Role, Model: input.AuthorModel, ModelEnforcement: store.ReviewSummaryModelEnforcement}
	switch credential.Kind {
	case core.CredentialUser:
		if actor.Role != core.ActorUser {
			return refused, errReviewSummaryRefused
		}
		membership, ok := s.Store.(store.MembershipStore)
		if !ok {
			return refused, errReviewSummaryRefused
		}
		allowed, err := membership.AuthorizeWorkspace(ctx, credential.OwnerUserID, workspace, core.CapabilityClaimWork)
		if err != nil {
			return refused, err
		}
		if !allowed {
			return refused, errReviewSummaryRefused
		}
	case core.CredentialAgent:
		if actor.Role != core.ActorAgent || input.WorkOrderID == "" || input.SessionID == "" {
			return refused, errReviewSummaryRefused
		}
		// A conveyor-run child credential stays confined to the exact
		// workspace, order, and session its parent minted it for.
		if credential.RunWorkOrderID != "" && (credential.RunWorkspaceID != workspace || credential.RunWorkOrderID != input.WorkOrderID || credential.RunSessionID != input.SessionID) {
			return refused, errReviewSummaryRefused
		}
		order, err := s.Store.GetWorkOrder(ctx, input.WorkOrderID)
		if err != nil || order.TaskID != input.TaskID || order.SessionID == "" || order.SessionID != input.SessionID || !reviewSummaryAuthorOrderState(order, time.Now()) {
			return refused, errReviewSummaryRefused
		}
		author.WorkOrderID, author.SessionID = order.ID, order.SessionID
	default:
		return refused, errReviewSummaryRefused
	}
	task, err := s.Store.GetTask(ctx, input.TaskID)
	if err != nil || task.Workspace != workspace {
		return refused, errReviewSummaryRefused
	}
	return author, nil
}

// reviewSummaryAuthorOrderState admits a claimed order with a live lease, or
// a submitted or completed order. Unlike ObservableOrder it admits completed
// orders: seat orders and the bounced implementation order are completed
// once their round settles (SU-3).
func reviewSummaryAuthorOrderState(order core.WorkOrder, now time.Time) bool {
	switch order.State {
	case core.WorkOrderClaimed:
		return order.LeaseExpiresAt.After(now)
	case core.WorkOrderSubmitted, core.WorkOrderCompleted:
		return true
	default:
		return false
	}
}
