package workorder

import (
	"context"
	"fmt"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

func (s *Service) freshnessSnapshot(ctx context.Context, order core.WorkOrder, snapshot core.ContextSnapshot) (core.ContextSnapshot, error) {
	if order.Stage == core.StageReview || order.Stage == core.StageVerify {
		if order.ServedRequirementSnapshot == nil || order.GovernanceSnapshot == nil {
			return snapshot, fmt.Errorf("pinned context unavailable")
		}
		return core.WithContextAuthority(snapshot, "pinned", order.ServedRequirementSnapshot, order.GovernanceSnapshot), nil
	}
	task, err := s.Store.GetTask(ctx, order.TaskID)
	if err != nil {
		return snapshot, err
	}
	var cfg *config.Config
	if s.ConfigProvider != nil {
		cfg, err = s.config(ctx)
		if err != nil {
			return snapshot, err
		}
	}
	req, err := store.ServedRequirementsForTask(ctx, s.Store, task.ID, config.ServedRequirementAuthorityNodes(cfg))
	if err != nil {
		return snapshot, err
	}
	var gov *core.GovernanceSnapshot
	if order.Stage == core.StageImplement {
		g, e := store.GovernanceForTask(ctx, s.Store, task.ID, task.Repo)
		if e != nil {
			return snapshot, e
		}
		gov = &g
	}
	return core.WithContextAuthority(snapshot, "live", req.Requirements, gov), nil
}
func (s *Service) recordContext(ctx context.Context, order core.WorkOrder, o core.ContextObservation) (core.ContextObservation, error) {
	backend, ok := s.Store.(store.ContextObservationStore)
	if !ok {
		return core.ContextObservation{}, fmt.Errorf("observation unavailable")
	}
	ws, _ := store.WorkspaceFromContext(ctx)
	o.DeliveryBoundary = "service"
	o.Schema = 1
	o.Workspace = ws
	o.TaskID = order.TaskID
	o.WorkOrderID = order.ID
	o.AttemptID = order.AttemptID
	o.SessionID = order.SessionID
	return taskops.ExecuteWorkOrder(ctx, s.Store, order.TaskID, store.ContextObservationCommand, func(lease taskops.TaskLease) (core.ContextObservation, error) {
		return backend.RecordContextObservation(ctx, lease, o)
	})
}
func (s *Service) observeContext(ctx context.Context, order core.WorkOrder, snapshot core.ContextSnapshot, prior, kind string) core.ContextFreshness {
	events, err := s.Store.ListEvents(ctx, order.TaskID)
	history := store.ContextHistory(events, order)
	f := store.ProjectContext(snapshot, history, prior)
	f.ObservedAt = time.Now().UTC()
	if err != nil {
		f.Diagnostic = "observation_unavailable"
		return core.BoundContextFreshness(f)
	}
	if order.State != core.WorkOrderClaimed {
		f.Diagnostic = "observation_not_recorded"
		return f
	}
	bounded := core.BoundContextFreshness(core.ContextFreshness{Snapshot: snapshot}).Snapshot
	receipt, e := s.recordContext(ctx, order, core.ContextObservation{Kind: kind, PriorRevision: prior, SelectionRevision: snapshot.Revision, Snapshot: &bounded, Outcome: "available", ComparisonStatus: f.ComparisonStatus, UnfetchedAdditions: f.UnfetchedAdditions, Truncated: f.Truncated || snapshot.IncompleteCoverage})
	if e != nil {
		f.Diagnostic = "observation_unavailable"
	} else {
		f.ObservationRecorded = true
		f.ObservationID = receipt.ID
		f.ObservedAt = receipt.ObservedAt
	}
	return core.BoundContextFreshness(f)
}
func (s *Service) refreshContext(ctx context.Context, order core.WorkOrder, prior, kind string) core.ContextFreshness {
	lineage, err := s.lineageForOrder(ctx, order)
	if err != nil {
		return core.ContextFreshness{SelectionSchema: 1, ComparisonStatus: "unavailable", Diagnostic: "refresh_unavailable"}
	}
	snapshot, err := s.freshnessSnapshot(ctx, order, lineage.Snapshot)
	if err != nil {
		return core.ContextFreshness{SelectionSchema: 1, ComparisonStatus: "unavailable", Diagnostic: "refresh_unavailable"}
	}
	return s.observeContext(ctx, order, snapshot, prior, kind)
}
func (s *Service) RefreshContext(ctx context.Context, id, session, prior string) (core.ContextFreshness, error) {
	if prior != "" && !core.ValidContextRevision(prior) {
		return core.ContextFreshness{}, fmt.Errorf("invalid prior_revision")
	}
	order, err := s.authorized(ctx, id, session)
	if err != nil {
		return core.ContextFreshness{}, store.ErrWorkOrderClaimUnauthorized
	}
	task, err := s.Store.GetTask(ctx, order.TaskID)
	if err != nil {
		return core.ContextFreshness{}, store.ErrWorkOrderClaimUnauthorized
	}
	owner := ""
	if order.WorkerID != "" {
		owner, err = store.WorkOrderOwnerUserID(ctx, s.Store, order)
		if err != nil {
			return core.ContextFreshness{}, store.ErrWorkOrderClaimUnauthorized
		}
	}
	if err = store.ValidateContextClaim(ctx, task, order, order.AttemptID, session, owner, time.Now()); err != nil {
		return core.ContextFreshness{}, err
	}
	f := s.refreshContext(ctx, order, prior, "context.refresh_observed")
	latest, err := s.authorized(ctx, id, session)
	if err != nil || latest.AttemptID != order.AttemptID {
		return core.ContextFreshness{}, store.ErrWorkOrderClaimUnauthorized
	}
	if err = store.ValidateContextClaim(ctx, task, latest, order.AttemptID, session, owner, time.Now()); err != nil {
		return core.ContextFreshness{}, err
	}
	return f, nil
}
