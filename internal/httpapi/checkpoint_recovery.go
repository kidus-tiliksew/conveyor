package httpapi

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// workOrderActivityView adds task-detail-only checkpoint presentation data to
// the durable work-order shape. The embedded order remains the REST contract;
// only its checkpoint field is replaced by the enriched read model below
// (req-260820-394cac REQ-2/AC-2.2, AC-2.3; design-http-api).
type workOrderActivityView struct {
	core.WorkOrder
	Checkpoint *checkpointActivityView `json:"checkpoint,omitempty"`
}

func (view workOrderActivityView) MarshalJSON() ([]byte, error) {
	// encoding/json otherwise promotes the embedded durable checkpoint field.
	// Replace that one key after serializing the unchanged work-order contract.
	data, err := json.Marshal(view.WorkOrder)
	if err != nil {
		return nil, err
	}
	fields := map[string]json.RawMessage{}
	if err = json.Unmarshal(data, &fields); err != nil {
		return nil, err
	}
	if view.Checkpoint == nil {
		delete(fields, "checkpoint")
	} else if fields["checkpoint"], err = json.Marshal(view.Checkpoint); err != nil {
		return nil, err
	}
	return json.Marshal(fields)
}

type checkpointActivityView struct {
	DecisionRequest string                           `json:"decision_request,omitempty"`
	Class           string                           `json:"class,omitempty"`
	Citations       []checkpointCitationActivityView `json:"citations,omitempty"`
}

type checkpointCitationActivityView struct {
	DocumentID              string                 `json:"document_id"`
	DocumentKind            string                 `json:"document_kind"`
	DocumentTitle           string                 `json:"document_title"`
	CitedVersion            int                    `json:"cited_version"`
	StatementOrSectionID    string                 `json:"statement_or_section_id,omitempty"`
	CurrentConfirmedVersion int                    `json:"current_confirmed_version"`
	NewerConfirmed          bool                   `json:"newer_confirmed"`
	PendingProposals        []core.PendingProposal `json:"pending_proposals"`
}

func (s *Server) checkpointWorkOrderViews(ctx context.Context, orders []core.WorkOrder, proposals []core.PendingProposal) ([]workOrderActivityView, error) {
	views := make([]workOrderActivityView, 0, len(orders))
	for _, order := range orders {
		view := workOrderActivityView{WorkOrder: order}
		if order.Checkpoint == nil {
			views = append(views, view)
			continue
		}
		checkpoint := &checkpointActivityView{
			DecisionRequest: order.Checkpoint.DecisionRequest,
			Class:           order.Checkpoint.Class,
		}
		if len(order.Checkpoint.Citations) > 0 {
			checkpoint.Citations = make([]checkpointCitationActivityView, 0, len(order.Checkpoint.Citations))
		}
		for _, citation := range order.Checkpoint.Citations {
			enriched, err := s.checkpointCitationActivityView(ctx, citation, proposals)
			if err != nil {
				return nil, err
			}
			checkpoint.Citations = append(checkpoint.Citations, enriched)
		}
		view.Checkpoint = checkpoint
		views = append(views, view)
	}
	return views, nil
}

func (s *Server) checkpointCitationActivityView(ctx context.Context, citation core.WorkOrderAuthorityConflictCitation, proposals []core.PendingProposal) (checkpointCitationActivityView, error) {
	view := checkpointCitationActivityView{
		DocumentID:           citation.DocumentID,
		CitedVersion:         citation.CitedVersion,
		StatementOrSectionID: citation.StatementOrSectionID,
		PendingProposals:     []core.PendingProposal{},
	}
	if requirement, err := s.Store.GetRequirement(ctx, citation.DocumentID); err == nil {
		view.DocumentKind = "requirement"
		view.DocumentTitle = requirement.Title
		view.CurrentConfirmedVersion = requirement.CurrentVersion
	} else if document, designErr := s.Store.GetSystemDesign(ctx, citation.DocumentID); designErr == nil {
		view.DocumentKind = "system_design"
		view.DocumentTitle = document.Title
		view.CurrentConfirmedVersion = document.CurrentVersion
	} else {
		return checkpointCitationActivityView{}, fmt.Errorf("enrich checkpoint citation %s: requirement lookup: %v; system design lookup: %v", citation.DocumentID, err, designErr)
	}
	view.NewerConfirmed = view.CurrentConfirmedVersion > view.CitedVersion
	for _, proposal := range proposals {
		if proposal.ID != citation.DocumentID || proposal.Tier != view.DocumentKind {
			continue
		}
		view.PendingProposals = append(view.PendingProposals, proposal)
	}
	return view, nil
}
