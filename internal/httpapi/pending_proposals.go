package httpapi

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
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
