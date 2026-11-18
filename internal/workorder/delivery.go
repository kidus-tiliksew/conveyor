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
// complete snapshot for validation; this copy names documents
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
// authorized. The document list needs the order's stage and task only, so the
// order is read without its snapshots.
func (s *Service) DeliverFreshness(ctx context.Context, id string, freshness core.ContextFreshness) FreshnessDelivery {
	delivery := FreshnessDelivery{ContextFreshness: core.OutboundContextFreshness(freshness)}
	order, err := s.Store.GetWorkOrderWithoutSnapshots(ctx, id)
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
// authority and read nothing (req-design-delivery AC-1.4). Review and verify
// use the order's own snapshot when it carries one, as a claim response and a
// same-claim acknowledgement do. Otherwise, as on a renewal or refresh, they
// read only the persisted pin's reference fields, never newly resolved
// versions and never a snapshot column. The list needs the task's repository
// and its design attachment history, so the task body and the rest of the
// timeline are not read (component-work-orders invariants 19, 20, 23, and 24).
func (s *Service) orderGovernanceReferences(ctx context.Context, order core.WorkOrder) (*core.GovernanceReferences, error) {
	var snapshot core.GovernanceSnapshot
	switch order.Stage {
	case core.StageReview, core.StageVerify:
		pin := order.GovernanceSnapshot
		if pin == nil {
			var err error
			if pin, err = s.Store.GetWorkOrderGovernancePin(ctx, order.ID); err != nil {
				return nil, err
			}
		}
		if pin == nil {
			return nil, nil
		}
		snapshot = *pin
	case core.StageImplement:
	default:
		return nil, nil
	}
	repository, err := s.Store.GetTaskRepository(ctx, order.TaskID)
	if err != nil {
		return nil, err
	}
	if order.Stage == core.StageImplement {
		if snapshot, err = store.GovernanceForTask(ctx, s.Store, order.TaskID, repository); err != nil {
			return nil, err
		}
	}
	events, err := s.Store.ListTaskDesignContextEvents(ctx, order.TaskID)
	if err != nil {
		return nil, err
	}
	references := core.ProjectGovernanceReferences(snapshot, repository, store.TaskDesignProvenance(events), nil)
	return &references, nil
}
