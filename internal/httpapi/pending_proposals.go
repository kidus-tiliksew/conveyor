package httpapi

import (
	"context"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

type pendingProposalItem struct {
	core.PendingProposal
	AgeSeconds int64 `json:"age_seconds"`
}

type attentionSummary struct {
	TaskCount            int `json:"task_count"`
	PendingProposalCount int `json:"pending_proposal_count"`
	Total                int `json:"total"`
}

type pendingProposalsResponse struct {
	Items     []pendingProposalItem `json:"items"`
	Attention attentionSummary      `json:"attention"`
}

func (s *Server) listPendingProposals(w http.ResponseWriter, r *http.Request) {
	projection, err := s.Store.PendingProposalsProjection(r.Context())
	if err != nil {
		log.Printf("list pending proposals: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	now := time.Now().UTC()
	items := make([]pendingProposalItem, 0, len(projection.Items))
	for _, proposal := range projection.Items {
		if proposal.Tier == "task_context" {
			continue
		}
		age := now.Sub(proposal.ProposedAt)
		if age < 0 {
			age = 0
		}
		items = append(items, pendingProposalItem{PendingProposal: proposal, AgeSeconds: int64(age / time.Second)})
	}
	taskCount := projection.TaskCount
	writeJSON(w, http.StatusOK, pendingProposalsResponse{Items: items, Attention: attentionSummary{TaskCount: taskCount, PendingProposalCount: len(items), Total: taskCount + len(items)}})
}

func pendingAuthorityForTask(taskID string, orders []core.WorkOrder, proposals []core.PendingProposal) bool {
	return pendingAuthorityByTask(orders, proposals)[taskID]
}

func (s *Server) pendingAuthorityTasks(ctx context.Context, proposals []core.PendingProposal, taskIDs []string) (map[string]bool, error) {
	orders, err := s.Store.ListWorkOrdersForTasks(ctx, taskIDs)
	if err != nil {
		return nil, err
	}
	return pendingAuthorityByTask(orders, proposals), nil
}

// pendingAuthorityByTask is the operator-attention signal (req-260810-23b69f
// AC-2.1, AC-2.2): any pending proposal this task authored, in any tier, while
// the task is submitted for or in review. It is a signal only and never states
// that a claim is withheld; proposalClaimWaitingByTask owns that narrower fact.
func pendingAuthorityByTask(orders []core.WorkOrder, proposals []core.PendingProposal) map[string]bool {
	originTasks := make(map[string]bool)
	for _, proposal := range proposals {
		if proposal.OriginType == "task" && proposal.OriginID != "" {
			originTasks[proposal.OriginID] = true
		}
	}
	result := make(map[string]bool)
	for _, order := range orders {
		if !originTasks[order.TaskID] {
			continue
		}
		if order.Stage == core.StageImplement && order.State == core.WorkOrderSubmitted {
			result[order.TaskID] = true
			continue
		}
		if order.Stage == core.StageReview && (order.State == core.WorkOrderQueued || order.State == core.WorkOrderClaimed || order.State == core.WorkOrderSubmitted) {
			result[order.TaskID] = true
		}
	}
	return result
}

// waitingProposal identifies one pending proposal that withholds its origin
// task's verify and review claims.
type waitingProposal struct {
	Tier    string `json:"tier"`
	ID      string `json:"id"`
	Version int    `json:"version"`
}

// store.RequirementVersionWithholdsClaims and
// store.SystemDesignVersionWithholdsClaims are the claim-gate predicate of
// req-260810-70ce2f REQ-1 (AC-1.1–AC-1.4), stated once in the store package.
// Work-order listing, the claim precheck, and the store claim transactions
// apply the same predicate (component-work-orders); the parity tests in
// pending_proposals_test.go claim real verify and review orders against each
// fixture this projection classifies. Decisions, task-context suggestions, and
// operator, session, drift, or other-task origins never withhold a claim.

// claimWaitWindowTasks reports the tasks whose verify or review claim is
// pending or in flight: a submitted implementation (which stays submitted until
// its review round is terminal) or a verify or review order not yet terminal.
func claimWaitWindowTasks(orders []core.WorkOrder) map[string]bool {
	result := make(map[string]bool)
	for _, order := range orders {
		switch {
		case order.Stage == core.StageImplement && order.State == core.WorkOrderSubmitted:
			result[order.TaskID] = true
		case (order.Stage == core.StageVerify || order.Stage == core.StageReview) &&
			(order.State == core.WorkOrderQueued || order.State == core.WorkOrderClaimed || order.State == core.WorkOrderSubmitted):
			result[order.TaskID] = true
		}
	}
	return result
}

// proposalClaimWaitingByTask returns, for each task in its claim window, the
// pending proposals that withhold its verify and review claims. The normalized
// PendingProposal drops the raw origin, so each candidate requirement or System
// Design version authored by a windowed task is re-read and judged by the
// claim-gate predicate. Reads are bounded by those candidates and deduplicated
// within the call; a version resolved or removed between the list and the read
// is judged on its re-read state. Other storage failures are returned.
func (s *Server) proposalClaimWaitingByTask(ctx context.Context, orders []core.WorkOrder, proposals []core.PendingProposal) (map[string][]waitingProposal, error) {
	window := claimWaitWindowTasks(orders)
	result := make(map[string][]waitingProposal)
	seen := make(map[waitingProposal]bool)
	for _, proposal := range proposals {
		if proposal.OriginType != "task" || !window[proposal.OriginID] {
			continue
		}
		if proposal.Tier != "requirement" && proposal.Tier != "system_design" {
			continue
		}
		identity := waitingProposal{Tier: proposal.Tier, ID: proposal.ID, Version: proposal.Version}
		if seen[identity] {
			continue
		}
		seen[identity] = true
		taskID := proposal.OriginID
		var withholds bool
		switch proposal.Tier {
		case "requirement":
			version, err := s.Store.GetRequirementVersion(ctx, proposal.ID, proposal.Version)
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			if err != nil {
				return nil, err
			}
			withholds = store.RequirementVersionWithholdsClaims(taskID, version)
		case "system_design":
			version, err := s.Store.GetSystemDesignVersion(ctx, proposal.ID, proposal.Version)
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			if err != nil {
				return nil, err
			}
			withholds = store.SystemDesignVersionWithholdsClaims(taskID, version)
		}
		if withholds {
			result[taskID] = append(result[taskID], identity)
		}
	}
	return result, nil
}

// proposalSignals reads one task page's orders once and derives both the
// attention signal and the narrower claim-wait projection from them.
func (s *Server) proposalSignals(ctx context.Context, proposals []core.PendingProposal, taskIDs []string) (map[string]bool, map[string][]waitingProposal, error) {
	orders, err := s.Store.ListWorkOrdersForTasks(ctx, taskIDs)
	if err != nil {
		return nil, nil, err
	}
	waiting, err := s.proposalClaimWaitingByTask(ctx, orders, proposals)
	if err != nil {
		return nil, nil, err
	}
	return pendingAuthorityByTask(orders, proposals), waiting, nil
}

func pendingTaskContextByTask(proposals []core.PendingProposal) map[string]bool {
	result := make(map[string]bool)
	for _, proposal := range proposals {
		if proposal.Tier == "task_context" && proposal.OriginType == "task" && proposal.OriginID != "" {
			result[proposal.OriginID] = true
		}
	}
	return result
}
