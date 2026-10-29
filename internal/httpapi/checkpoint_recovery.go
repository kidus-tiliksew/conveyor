package httpapi

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// workOrderActivityView adds task-detail-only checkpoint presentation data to
// the durable work-order shape. The embedded order remains the REST contract;
// only its checkpoint field is replaced by the enriched read model below
// (req-260820-394cac REQ-2/AC-2.2, AC-2.3; component-http-api).
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
	// Verification is the verify-checkpoint context reference of
	// component-verification-service; it never confers review admission.
	Verification *core.WorkOrderVerificationCheckpoint `json:"verification,omitempty"`
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

// checkpointWorkOrderViews enriches every order's checkpoint citations with
// one ListDocumentMetadata read (component-http-api, Request-scoped detail
// reads). The task detail shares that read with its context instead.
func (s *Server) checkpointWorkOrderViews(ctx context.Context, orders []core.WorkOrder, proposals []core.PendingProposal) ([]workOrderActivityView, error) {
	metadata := store.NewDocumentMetadataSet()
	if ids := taskDetailDocumentIDs(nil, nil, orders); len(ids) > 0 {
		var err error
		if metadata, err = s.Store.ListDocumentMetadata(ctx, ids); err != nil {
			return nil, err
		}
	}
	return checkpointViewsFromMetadata(orders, proposals, metadata)
}

// checkpointViewsFromMetadata enriches checkpoint citations from one metadata
// set: a citation resolves to the requirement row when
// one exists and otherwise to the System Design row, and a citation with
// neither fails the read.
func checkpointViewsFromMetadata(orders []core.WorkOrder, proposals []core.PendingProposal, metadata store.DocumentMetadataSet) ([]workOrderActivityView, error) {
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
			Verification:    order.Checkpoint.Verification,
		}
		if len(order.Checkpoint.Citations) > 0 {
			checkpoint.Citations = make([]checkpointCitationActivityView, 0, len(order.Checkpoint.Citations))
		}
		for _, citation := range order.Checkpoint.Citations {
			enriched := checkpointCitationActivityView{
				DocumentID:           citation.DocumentID,
				CitedVersion:         citation.CitedVersion,
				StatementOrSectionID: citation.StatementOrSectionID,
				PendingProposals:     []core.PendingProposal{},
			}
			if requirement, ok := metadata.Requirements[citation.DocumentID]; ok {
				enriched.DocumentKind, enriched.DocumentTitle, enriched.CurrentConfirmedVersion = "requirement", requirement.Title, requirement.CurrentVersion
			} else if design, ok := metadata.Designs[citation.DocumentID]; ok {
				enriched.DocumentKind, enriched.DocumentTitle, enriched.CurrentConfirmedVersion = "system_design", design.Title, design.CurrentVersion
			} else {
				return nil, fmt.Errorf("enrich checkpoint citation %s: no requirement or system design", citation.DocumentID)
			}
			enriched.NewerConfirmed = enriched.CurrentConfirmedVersion > enriched.CitedVersion
			for _, proposal := range proposals {
				if proposal.ID == citation.DocumentID && proposal.Tier == enriched.DocumentKind {
					enriched.PendingProposals = append(enriched.PendingProposals, proposal)
				}
			}
			checkpoint.Citations = append(checkpoint.Citations, enriched)
		}
		view.Checkpoint = checkpoint
		views = append(views, view)
	}
	return views, nil
}
