package storetest

import (
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

func runWorktreeHandoff(t *testing.T, x Fixture) {
	st, ctx := x.Backend, x.Context
	order := newAggregateOrder(t, x)
	claimed, err := ClaimWorkOrder(ctx, st, order.ID, core.WorkOrderClaim{WorkerID: "writer", ClaimantID: "writer", SessionID: "producer", ClientToken: "producer-token", Lease: time.Minute, ExecutionTimeout: time.Hour})
	requireOK(t, err)
	claim := core.WorkOrderClaimIdentity{WorkerID: "writer", ClaimantID: "writer", SessionID: "producer"}
	request := core.WorktreeHandoffRequest{SessionID: "producer", Generation: "generation-one", Action: "admit"}
	perform := func(o core.WorkOrder, c core.WorkOrderClaimIdentity, r core.WorktreeHandoffRequest) (core.WorktreeHandoff, error) {
		return taskops.ExecuteWorkOrder(ctx, st, o.TaskID, store.WorktreeHandoffCommand, func(lease taskops.TaskLease) (core.WorktreeHandoff, error) {
			return st.WorktreeHandoffCommand(ctx, lease, o.ID, c, "github.com/example/repository", r)
		})
	}
	admission, err := perform(claimed, claim, request)
	requireOK(t, err)
	if admission.Writer.AttemptID != claimed.AttemptID || admission.Writer.ClaimEventID == 0 || !admission.Created {
		t.Fatalf("admission=%+v", admission)
	}
	again, err := perform(claimed, claim, request)
	requireOK(t, err)
	if again.Created {
		t.Fatal("duplicate admission")
	}
	request.Action = "ready"
	_, err = perform(claimed, claim, request)
	requireOK(t, err)
	_, err = ReleaseWorkerClaim(ctx, st, order.ID, "writer", core.WorkOrderRelease{SessionID: "producer", Reason: core.WorkOrderReleaseReasonPlanRevisionRequested, Outcome: core.WorkOrderOutcomeReleased})
	requireOK(t, err)
	request.Action = "preserve"
	handoff, err := perform(claimed, claim, request)
	requireOK(t, err)
	if handoff.Writer.Reason != core.WorkOrderReleaseReasonPlanRevisionRequested {
		t.Fatal("release evidence missing")
	}
	request.Action = "audit"
	request.Producer = &admission.Writer
	request.CommitSHA = strings.Repeat("a", 40)
	request.OriginalReason = "renewal transport said lease lost"
	_, err = perform(claimed, claim, request)
	requireOK(t, err)
	_, err = perform(claimed, claim, request)
	requireOK(t, err)
	events, err := st.ListEvents(ctx, order.TaskID)
	requireOK(t, err)
	counts := map[string]int{}
	for _, e := range events {
		counts[e.Kind]++
	}
	if counts["work_order.attempt_checkpointed"] != 1 || counts["work_order.checkpoint_reconciled"] != 1 {
		t.Fatalf("duplicate audit: %v", counts)
	}
	// A fresh implement order has no LastAttemptID. Its predecessor comes from
	// task history across an intervening plan order, not from a retry field.
	retired, err := st.GetWorkOrder(ctx, order.ID)
	requireOK(t, err)
	retired.State = core.WorkOrderCancelled
	requireOK(t, UpdateWorkOrder(ctx, st, retired, core.WorkOrderCmdCancel))
	successor := order
	successor.ID = order.ID + "-successor"
	successor.JobID = successor.ID
	job := core.Job{ID: successor.ID, TaskID: order.TaskID, Stage: core.StageImplement, State: core.JobPending}
	_, err = CreateStageWorkOrder(ctx, st, job, successor)
	requireOK(t, err)
	next, err := ClaimWorkOrder(ctx, st, successor.ID, core.WorkOrderClaim{WorkerID: "writer", ClaimantID: "writer", SessionID: "successor", ClientToken: "successor-token", Lease: time.Minute, ExecutionTimeout: time.Hour})
	requireOK(t, err)
	nextClaim := core.WorkOrderClaimIdentity{WorkerID: "writer", ClaimantID: "writer", SessionID: "successor"}
	nextRequest := core.WorktreeHandoffRequest{SessionID: "successor", Generation: "generation-two", Action: "admit"}
	admitted, err := perform(next, nextClaim, nextRequest)
	requireOK(t, err)
	if admitted.Predecessor == nil || admitted.Predecessor.WorkOrderID != claimed.ID || admitted.Predecessor.AttemptID != claimed.AttemptID || admitted.Predecessor.CommitSHA != request.CommitSHA {
		t.Fatalf("predecessor=%+v", admitted.Predecessor)
	}
	for _, action := range []string{"admit", "preserve", "audit"} {
		request.Action = action
		if _, err = perform(claimed, claim, request); err == nil {
			t.Fatalf("stale writer admitted %s", action)
		}
	}
	if _, err = st.RecordWorkOrderAttemptCheckpoint(ctx, claimed.ID, "writer", core.WorkOrderAttemptCheckpoint{SessionID: "producer", AttemptID: claimed.AttemptID, CommitSHA: request.CommitSHA, PushResult: "pushed", TerminationReason: "old API"}); err == nil {
		t.Fatal("legacy API bypassed writer fence")
	}
	nextRequest.Action = "audit"
	nextRequest.Producer = admitted.Predecessor
	nextRequest.CommitSHA = request.CommitSHA
	nextRequest.OriginalReason = request.OriginalReason
	_, err = perform(next, nextClaim, nextRequest)
	requireOK(t, err)
	for _, mutate := range []func(*core.WorktreeIdentity){func(p *core.WorktreeIdentity) { p.TaskID = "wrong" }, func(p *core.WorktreeIdentity) { p.WorkOrderID = "wrong" }, func(p *core.WorktreeIdentity) { p.AttemptID = "wrong" }, func(p *core.WorktreeIdentity) { p.Repository = "wrong" }, func(p *core.WorktreeIdentity) { p.Branch = "wrong" }, func(p *core.WorktreeIdentity) { p.SessionID = "wrong" }} {
		wrong := *admitted.Predecessor
		mutate(&wrong)
		nextRequest.Producer = &wrong
		if _, err = perform(next, nextClaim, nextRequest); err == nil {
			t.Fatalf("accepted wrong identity %+v", wrong)
		}
	}
	nextRequest.Producer = admitted.Predecessor
	nextRequest.CommitSHA = strings.Repeat("b", 40)
	if _, err = perform(next, nextClaim, nextRequest); err == nil {
		t.Fatal("accepted conflicting SHA")
	}
}
