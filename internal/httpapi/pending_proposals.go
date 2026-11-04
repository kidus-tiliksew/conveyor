package httpapi

import (
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
	tasks, err := s.Store.CountOperatorAttention(r.Context())
	if err != nil {
		log.Printf("list pending proposals: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	// Each pending document proposal is one operator decision alongside the
	// tasks that need the operator: it holds no task's work, so no task row
	// counts it (req-260810-23b69f AC-1.1, AC-4.2; req-operator-next-action
	// AC-2.7).
	decisions := tasks + len(items)
	writeJSON(w, http.StatusOK, pendingProposalsResponse{Items: items, Attention: attentionSummary{TaskCount: tasks, PendingProposalCount: len(items), OperatorDecisions: decisions, Total: decisions}})
}
