package httpapi

import (
	"context"
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
	OperatorDecisions    int `json:"operator_decisions"`
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
	decisions, err := s.Store.CountOperatorAttention(r.Context())
	if err != nil {
		log.Printf("list pending proposals: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, pendingProposalsResponse{Items: items, Attention: attentionSummary{TaskCount: decisions, PendingProposalCount: len(items), OperatorDecisions: decisions, Total: decisions}})
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

// proposalClaimWaitingByTask returns, for each task in its claim window, the
// versions that withhold its verify and review claims. It is
// store.ClaimWaitingProposals, the selection next-action row 8 also reads, so
// proposal_claim_waiting, waiting_proposals, and the next action agree with
// the claim gate (req-260810-70ce2f REQ-1; component-document-corpus). The
// normalized pending list is never filtered here: it drops the raw origin and
// archived documents, which the claim still counts.
func (s *Server) proposalClaimWaitingByTask(ctx context.Context, orders []core.WorkOrder) (map[string][]waitingProposal, error) {
	selected, err := store.ClaimWaitingProposals(ctx, s.Store, orders)
	if err != nil {
		return nil, err
	}
	result := make(map[string][]waitingProposal, len(selected))
	for taskID, items := range selected {
		for _, item := range items {
			result[taskID] = append(result[taskID], waitingProposal{Tier: item.Tier, ID: item.ID, Version: item.Version})
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
	waiting, err := s.proposalClaimWaitingByTask(ctx, orders)
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
