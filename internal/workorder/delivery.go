package workorder

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/lineagecontext"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// Outbound is the view get_work_order delivers. The internal Context keeps the
// complete snapshot for consultation and validation; this copy names documents
// only in the role prompt and carries each list once. Served requirement
// statements stay in the role prompt, and the whole plan stays in
// approved_spec (req-design-delivery AC-1.1; req-260802-72fc68 AC-2.2; DEC-103).
func (c Context) Outbound() Context {
	c.GovernanceSnapshot = nil
	c.ServedRequirements = nil
	c.Order = core.OutboundWorkOrder(c.Order)
	c.Task.Context.Designs = nil
	c.ContextFreshness = core.OutboundContextFreshness(c.ContextFreshness)
	c.LineageContext = lineagecontext.Outbound(c.LineageContext)
	return c
}

const governanceReferencesUnavailable = "the document list could not be read; get_work_order delivers it"

// OrderDelivery is a claim or renewal response: the order without its pinned
// bodies and, at the response root, its one document list.
type OrderDelivery struct {
	core.WorkOrder
	GovernanceReferences      *core.GovernanceReferences
	GovernanceReferencesError string
}

// MarshalJSON keeps the order's own wire shape, which core.WorkOrder defines
// through its own marshaler, and adds the root fields.
func (d OrderDelivery) MarshalJSON() ([]byte, error) {
	payload, err := json.Marshal(core.OutboundWorkOrder(d.WorkOrder))
	if err != nil {
		return nil, err
	}
	if d.GovernanceReferences != nil {
		references, err := json.Marshal(d.GovernanceReferences)
		if err != nil {
			return nil, err
		}
		payload = core.AppendJSONObjectField(payload, fmt.Sprintf(`"governance_references":%s`, references))
	}
	if d.GovernanceReferencesError != "" {
		message, err := json.Marshal(d.GovernanceReferencesError)
		if err != nil {
			return nil, err
		}
		payload = core.AppendJSONObjectField(payload, fmt.Sprintf(`"governance_references_error":%s`, message))
	}
	return payload, nil
}

// FreshnessDelivery is a refresh_work_order_context response.
type FreshnessDelivery struct {
	core.ContextFreshness
	GovernanceReferences      *core.GovernanceReferences `json:"governance_references,omitempty"`
	GovernanceReferencesError string                     `json:"governance_references_error,omitempty"`
}

// DeliverOrder builds the claim or renewal response. The lifecycle act has
// already succeeded, so a failed list read reports itself instead of failing
// the response and hiding a live claim from its session.
func (s *Service) DeliverOrder(ctx context.Context, order core.WorkOrder) OrderDelivery {
	references, err := s.orderGovernanceReferences(ctx, order)
	if err != nil {
		s.logf("work_order_governance_references work_order_id=%s error=%q", order.ID, err)
		return OrderDelivery{WorkOrder: order, GovernanceReferencesError: governanceReferencesUnavailable}
	}
	return OrderDelivery{WorkOrder: order, GovernanceReferences: references}
}

// DeliverFreshness builds the refresh response for the exact order the refresh
// authorized.
func (s *Service) DeliverFreshness(ctx context.Context, id string, freshness core.ContextFreshness) FreshnessDelivery {
	delivery := FreshnessDelivery{ContextFreshness: core.OutboundContextFreshness(freshness)}
	order, err := s.Store.GetWorkOrder(ctx, id)
	if err == nil {
		delivery.GovernanceReferences, err = s.orderGovernanceReferences(ctx, order)
	}
	if err != nil {
		s.logf("work_order_governance_references work_order_id=%s error=%q", id, err)
		delivery.GovernanceReferencesError = governanceReferencesUnavailable
	}
	return delivery
}

// orderGovernanceReferences lists a review or verify order's pinned versions
// and an implement order's live ones; other stages receive no governance
// authority (req-design-delivery AC-1.4). Review and verify read the persisted
// pin, never newly resolved versions. A renewal holds no snapshot, so it reads
// the order once for it.
func (s *Service) orderGovernanceReferences(ctx context.Context, order core.WorkOrder) (*core.GovernanceReferences, error) {
	task, err := s.Store.GetTask(ctx, order.TaskID)
	if err != nil {
		return nil, err
	}
	var snapshot core.GovernanceSnapshot
	switch order.Stage {
	case core.StageReview, core.StageVerify:
		if order.GovernanceSnapshot == nil {
			if order, err = s.Store.GetWorkOrder(ctx, order.ID); err != nil {
				return nil, err
			}
		}
		if order.GovernanceSnapshot == nil {
			return nil, nil
		}
		snapshot = *order.GovernanceSnapshot
	case core.StageImplement:
		if snapshot, err = store.GovernanceForTask(ctx, s.Store, task.ID, task.Repo); err != nil {
			return nil, err
		}
	default:
		return nil, nil
	}
	events, err := s.Store.ListCompactEvents(ctx, task.ID)
	if err != nil {
		return nil, err
	}
	references := core.ProjectGovernanceReferences(snapshot, task.Repo, store.TaskDesignProvenance(events), nil)
	return &references, nil
}
