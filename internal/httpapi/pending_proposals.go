package httpapi

import (
	"context"
	"net/http"
	"net/url"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

type pendingProposalItem struct {
	core.PendingProposal
	AgeSeconds int64  `json:"age_seconds"`
	Href       string `json:"href"`
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
	proposals, err := s.Store.ListPendingProposals(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	taskCount, err := s.taskAttentionCount(r.Context(), proposals)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	now := time.Now().UTC()
	items := make([]pendingProposalItem, 0, len(proposals))
	for _, proposal := range proposals {
		age := now.Sub(proposal.ProposedAt)
		if age < 0 {
			age = 0
		}
		items = append(items, pendingProposalItem{PendingProposal: proposal, AgeSeconds: int64(age / time.Second), Href: pendingProposalHref(proposal)})
	}
	writeJSON(w, http.StatusOK, pendingProposalsResponse{Items: items, Attention: attentionSummary{TaskCount: taskCount, PendingProposalCount: len(items), Total: taskCount + len(items)}})
}

func pendingProposalHref(proposal core.PendingProposal) string {
	switch proposal.Tier {
	case "requirement":
		return "/requirements?requirement=" + url.QueryEscape(proposal.ID)
	case "system_design":
		return "/system-design?document=" + url.QueryEscape(proposal.ID)
	case "decision":
		return "/system-design?decision=" + url.QueryEscape(proposal.ID)
	default:
		return ""
	}
}

func (s *Server) taskAttentionCount(ctx context.Context, proposals []core.PendingProposal) (int, error) {
	tasks, err := s.Store.ListTasks(ctx)
	if err != nil {
		return 0, err
	}
	markers, err := s.Store.ListActivityMarkers(ctx)
	if err != nil {
		return 0, err
	}
	pendingAuthority, err := s.pendingAuthorityTasks(ctx, proposals)
	if err != nil {
		return 0, err
	}
	byTask := make(map[string]store.ActivityMarker, len(markers))
	for _, marker := range markers {
		byTask[marker.TaskID] = marker
	}
	count := 0
	for _, task := range tasks {
		if core.BlueprintAnchor(task) {
			continue
		}
		marker := byTask[task.ID]
		if core.TaskTerminal(task.State) {
			marker.Stalled = nil
		}
		if needsAttention(task, marker, pendingAuthority[task.ID]) {
			count++
		}
	}
	return count, nil
}

func pendingAuthorityForTask(taskID string, orders []core.WorkOrder, proposals []core.PendingProposal) bool {
	return pendingAuthorityByTask(orders, proposals)[taskID]
}

func (s *Server) pendingAuthorityTasks(ctx context.Context, proposals []core.PendingProposal) (map[string]bool, error) {
	orders, err := s.Store.ListWorkOrders(ctx)
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
