package httpapi

import (
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
	taskCount, err := s.taskAttentionCount(r)
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

func (s *Server) taskAttentionCount(r *http.Request) (int, error) {
	tasks, err := s.Store.ListTasks(r.Context())
	if err != nil {
		return 0, err
	}
	markers, err := s.Store.ListActivityMarkers(r.Context())
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
		if needsAttention(task, marker) {
			count++
		}
	}
	return count, nil
}

func pendingAuthorityForTask(taskID string, orders []core.WorkOrder, proposals []core.PendingProposal) bool {
	underReview := false
	for _, order := range orders {
		if order.Stage == core.StageImplement && order.State == core.WorkOrderSubmitted {
			underReview = true
			break
		}
		if order.Stage == core.StageReview && (order.State == core.WorkOrderQueued || order.State == core.WorkOrderClaimed || order.State == core.WorkOrderSubmitted) {
			underReview = true
			break
		}
	}
	if !underReview {
		return false
	}
	for _, proposal := range proposals {
		if proposal.OriginType == "task" && proposal.OriginID == taskID {
			return true
		}
	}
	return false
}
