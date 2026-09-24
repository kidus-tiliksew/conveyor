package storetest

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

func runContextFreshness(t *testing.T, x Fixture) {
	ctx, owner := bootstrapOwner(t, x)
	x.Context = ctx
	st := x.Backend
	backend, ok := st.(store.ContextObservationStore)
	if !ok {
		t.Fatal("missing context observation backend")
	}
	order := newAggregateOrder(t, x)
	order, err := ClaimWorkOrder(ctx, st, order.ID, core.WorkOrderClaim{ClaimantID: core.TaskRunClaimantID(owner.ID), SessionID: "fresh-session", ClientToken: "fresh-token", Lease: time.Minute, ExecutionTimeout: time.Hour})
	requireOK(t, err)
	snapshot := core.ContextSnapshot{Schema: 1, Workspace: x.Workspace, TaskID: order.TaskID}
	snapshot.Seal()
	observation := core.ContextObservation{Schema: 1, Workspace: x.Workspace, TaskID: order.TaskID, WorkOrderID: order.ID, AttemptID: order.AttemptID, SessionID: order.SessionID, Kind: "context.baseline_observed", SelectionRevision: snapshot.Revision, Snapshot: &snapshot, Outcome: "available", DeliveryBoundary: "service"}
	record := func(c context.Context, o core.ContextObservation) (core.ContextObservation, error) {
		return taskops.ExecuteWorkOrder(c, st, order.TaskID, store.ContextObservationCommand, func(lease taskops.TaskLease) (core.ContextObservation, error) {
			return backend.RecordContextObservation(c, lease, o)
		})
	}
	if _, err = backend.RecordContextObservation(ctx, taskops.TaskLease{}, observation); err == nil {
		t.Fatal("unguarded observation accepted")
	}
	first, err := record(ctx, observation)
	requireOK(t, err)
	if first.ActorID != store.UserActorID(owner.ID) || first.AttemptID != order.AttemptID || first.ObservedAt.IsZero() {
		t.Fatal(first)
	}
	var wg sync.WaitGroup
	results := make(chan core.ContextObservation, 8)
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); r, e := record(ctx, observation); results <- r; errs <- e }()
	}
	wg.Wait()
	close(results)
	close(errs)
	for e := range errs {
		requireOK(t, e)
	}
	for r := range results {
		if r.ID != first.ID || !r.ObservedAt.Equal(first.ObservedAt) {
			t.Fatal("non-atomic replay")
		}
	}
	before, _ := st.ListEvents(ctx, order.TaskID)
	for _, mutate := range []func(*core.ContextObservation){func(o *core.ContextObservation) { o.SessionID = "foreign" }, func(o *core.ContextObservation) { o.AttemptID = "foreign" }, func(o *core.ContextObservation) { o.Workspace = "foreign" }, func(o *core.ContextObservation) { o.TaskID = "foreign" }, func(o *core.ContextObservation) { o.ID = first.ID; o.PriorRevision = strings.Repeat("f", 64) }} {
		bad := observation
		mutate(&bad)
		if _, e := record(ctx, bad); e == nil {
			t.Fatal("foreign/conflicting observation accepted")
		}
	}
	for _, foreign := range []context.Context{store.WithWorkspace(ctx, "foreign"), store.WithActor(ctx, store.Actor{ID: "worker:foreign", Role: core.ActorWorker}), store.WithActor(ctx, store.Actor{ID: "user:foreign", Role: core.ActorUser})} {
		if _, e := record(foreign, observation); e == nil {
			t.Fatal("foreign principal accepted")
		}
	}
	after, _ := st.ListEvents(ctx, order.TaskID)
	if len(before) != len(after) {
		t.Fatal("refusal mutated ledger")
	}
	fetch := observation
	fetch.Kind = "context.artifact_fetch_observed"
	fetch.Snapshot = nil
	fetch.ArtifactID = core.ContextBytesDigest([]byte("correction"))
	fetch.Outcome = "fetch_failed"
	_, err = record(ctx, fetch)
	requireOK(t, err)
	fetch.Outcome = "fetched"
	fetch.ContentDigest = fetch.ArtifactID
	fetch.ReturnedBytes = 10
	_, err = record(ctx, fetch)
	requireOK(t, err)
	events, err := st.ListEvents(ctx, order.TaskID)
	requireOK(t, err)
	count := 0
	for _, e := range events {
		if strings.HasPrefix(e.Kind, "context.") {
			count++
			var o core.ContextObservation
			requireOK(t, json.Unmarshal(e.Payload, &o))
			if o.ActorID != first.ActorID {
				t.Fatal("actor provenance lost")
			}
		}
	}
	if count != 3 {
		t.Fatalf("events=%d", count)
	}
	links, err := st.ListLineageNeighborhood(ctx, []core.LineageNode{{Type: core.LineageTask, ID: order.TaskID}}, core.LineageTraversalBudget{Workspace: x.Workspace, MaxDepth: 5, MaxNodes: 32, MaxLinks: 128})
	requireOK(t, err)
	for _, l := range links {
		for _, e := range events {
			if strings.HasPrefix(e.Kind, "context.") && l.CreatedByEventID == e.ID {
				t.Fatal("observation projected authority edge")
			}
		}
	}
	// The in-process boundary has an actual running job, no invented session.
	internalCtx := store.WithActor(ctx, store.Actor{ID: "dispatcher", Role: core.ActorSystem})
	internalJob := core.Job{ID: order.ID + "-input", TaskID: order.TaskID, Stage: core.StageReview, Runner: "in-process", State: core.JobRunning, StartedAt: time.Now().UTC()}
	requireOK(t, st.CreateJob(internalCtx, internalJob))
	internal := observation
	internal.WorkOrderID = ""
	internal.SessionID = ""
	internal.JobID = internalJob.ID
	internal.AttemptID = internalJob.ID
	internal.DeliveryBoundary = "provider_input"
	recordInput := func(c context.Context) (core.ContextObservation, error) {
		return taskops.ExecuteWorkOrder(c, st, order.TaskID, store.ContextObservationCommand, func(l taskops.TaskLease) (core.ContextObservation, error) {
			return backend.RecordInProcessContextObservation(c, l, internal)
		})
	}
	_, err = recordInput(internalCtx)
	requireOK(t, err)
	_, err = recordInput(internalCtx)
	requireOK(t, err)
	if _, err = recordInput(ctx); err == nil {
		t.Fatal("user fabricated provider input")
	}
	internalJob.State = core.JobDone
	internalJob.EndedAt = time.Now().UTC()
	requireOK(t, st.UpdateJob(internalCtx, internalJob))
	if _, err = recordInput(internalCtx); err == nil {
		t.Fatal("finished job fabricated provider input")
	}

	// Same-session submitted reads may observe, but cannot append claim history.
	order.State = core.WorkOrderSubmitted
	requireOK(t, UpdateWorkOrder(ctx, st, order, core.WorkOrderCmdSubmitForReview))
	if _, err = record(ctx, observation); err == nil {
		t.Fatal("submitted observation mutated history")
	}
}
