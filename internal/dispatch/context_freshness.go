package dispatch

import (
	"context"
	"encoding/json"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/inprocess"
	"github.com/kidus-tiliksew/conveyor/internal/lineagecontext"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

func (d *Dispatcher) recordInputObservation(ctx context.Context, task core.Task, job core.Job, o core.ContextObservation) (core.ContextObservation, error) {
	o.Schema = 1
	o.Workspace = task.Workspace
	o.TaskID = task.ID
	o.JobID = job.ID
	o.AttemptID = job.ID
	o.DeliveryBoundary = "provider_input"
	backend, ok := d.Store.(store.ContextObservationStore)
	if !ok {
		return core.ContextObservation{}, store.ErrWorkOrderClaimUnauthorized
	}
	return taskops.ExecuteWorkOrder(ctx, d.Store, task.ID, store.ContextObservationCommand, func(lease taskops.TaskLease) (core.ContextObservation, error) {
		return backend.RecordInProcessContextObservation(ctx, lease, o)
	})
}
func (d *Dispatcher) observeInProcessInput(ctx context.Context, task core.Task, job core.Job, input inprocess.Input) core.ContextFreshness {
	f := input.ContextFreshness
	snapshot := core.BoundContextFreshness(f).Snapshot
	receipt, err := d.recordInputObservation(ctx, task, job, core.ContextObservation{Kind: "context.baseline_observed", Snapshot: &snapshot, SelectionRevision: snapshot.Revision, Outcome: "available"})
	f.ObservationRecorded = err == nil
	f.ObservationID = receipt.ID
	f.ObservedAt = receipt.ObservedAt
	for i := range f.Deliveries {
		status := &f.Deliveries[i]
		o := core.ContextObservation{Kind: "context.artifact_fetch_observed", SelectionRevision: snapshot.Revision, ArtifactID: status.ArtifactID, Outcome: "omitted"}
		for _, a := range input.Attachments {
			if a.ID == status.ArtifactID {
				o.Outcome = "truncated"
				if core.ContextBytesDigest(a.Content) == a.ID {
					o.Outcome = "fetched"
					o.ContentDigest = a.ID
					o.ReturnedBytes = int64(len(a.Content))
				}
				break
			}
		}
		if _, e := d.recordInputObservation(ctx, task, job, o); e != nil {
			f.ObservationRecorded = false
			continue
		}
		status.Fetched = o.Outcome == "fetched"
		status.Omitted = o.Outcome == "omitted"
		status.Truncated = o.Outcome == "truncated"
		if status.Fetched {
			status.State = "fetched"
		}
	}
	f.ObservationRevision = core.ContextDigest(f.Deliveries)
	if !f.ObservationRecorded {
		f.Diagnostic = "observation_unavailable"
	}
	return core.BoundContextFreshness(f)
}
func (d *Dispatcher) refreshInProcessVerdict(ctx context.Context, cfg *config.Config, task core.Task, job core.Job, input inprocess.Input) core.ContextFreshness {
	// Bypass the per-dispatch memo: a correction may arrive during the model call.
	r, err := lineagecontext.Assemble(ctx, d.Store, cfg, []core.LineageNode{{Type: core.LineageTask, ID: task.ID}}, task.ID, false)
	if err != nil {
		return core.ContextFreshness{SelectionSchema: 1, ComparisonStatus: "unavailable", Diagnostic: "refresh_unavailable"}
	}
	if input.ContextFreshness.Snapshot.AuthoritySource != "" {
		r.Snapshot = core.WithContextAuthority(r.Snapshot, input.ContextFreshness.Snapshot.AuthoritySource, input.ServedRequirementSnapshot, input.GovernanceSnapshot)
	}
	events, e := d.Store.ListEvents(ctx, task.ID)
	f := store.ProjectContext(r.Snapshot, store.ContextHistory(events, core.WorkOrder{TaskID: task.ID, AttemptID: job.ID}), input.ContextFreshness.SelectionRevision)
	snapshot := core.BoundContextFreshness(f).Snapshot
	receipt, recordErr := d.recordInputObservation(ctx, task, job, core.ContextObservation{Kind: "context.verdict_refresh_observed", PriorRevision: input.ContextFreshness.SelectionRevision, SelectionRevision: snapshot.Revision, Snapshot: &snapshot, Outcome: "available", ComparisonStatus: f.ComparisonStatus, UnfetchedAdditions: f.UnfetchedAdditions, Truncated: f.Truncated || snapshot.IncompleteCoverage})
	f.ObservedAt = time.Now().UTC()
	f.ObservationRecorded = e == nil && recordErr == nil
	if f.ObservationRecorded {
		f.ObservationID = receipt.ID
		f.ObservedAt = receipt.ObservedAt
	} else {
		f.Diagnostic = "observation_unavailable"
	}
	return core.BoundContextFreshness(f)
}

// The next ordinary input may report the prior verdict boundary. This is not
// a cross-attempt fetch receipt or a reason to start another provider loop.
func (d *Dispatcher) priorContextDiagnostic(ctx context.Context, task core.Task) string {
	events, err := d.Store.ListEvents(ctx, task.ID)
	if err != nil {
		return ""
	}
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Kind != "context.verdict_refresh_observed" {
			continue
		}
		var o core.ContextObservation
		if json.Unmarshal(events[i].Payload, &o) != nil || o.TaskID != task.ID || o.Workspace != task.Workspace {
			continue
		}
		b := core.JSONPayload(map[string]any{"prior_attempt_id": o.AttemptID, "selection_revision": o.SelectionRevision, "comparison_status": o.ComparisonStatus, "unfetched_additions": o.UnfetchedAdditions, "truncated": o.Truncated, "diagnostic": o.Diagnostic})
		if len(b) > 2048 {
			return "prior_refresh_summary_unavailable"
		}
		return string(b)
	}
	return ""
}
