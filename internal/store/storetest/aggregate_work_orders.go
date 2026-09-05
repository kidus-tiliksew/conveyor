package storetest

import (
	"errors"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

func newAggregateOrder(t *testing.T, x Fixture) core.WorkOrder {
	t.Helper()
	task := newAggregateTask(t, x)
	job := core.Job{ID: task.ID + "-implement-1", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
	order := core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: job.Stage, State: core.WorkOrderQueued, QueueEnteredAt: task.CreatedAt, QueueDeadline: task.CreatedAt.Add(time.Hour), CreatedAt: task.CreatedAt}
	created, err := CreateStageWorkOrder(x.Context, x.Backend, job, order)
	requireOK(t, err)
	if !created {
		t.Fatal("new stage was not created")
	}
	created, err = CreateStageWorkOrder(x.Context, x.Backend, job, order)
	requireOK(t, err)
	if created {
		t.Fatal("stage creation was not idempotent")
	}
	return order
}

func runWorkOrders(t *testing.T, x Fixture) {
	st, ctx := x.Backend, x.Context
	order := newAggregateOrder(t, x)
	claim := core.WorkOrderClaim{WorkerID: "worker", ClaimantID: "worker", SessionID: "session", ClientToken: "fixture", Lease: time.Minute, ExecutionTimeout: time.Hour}
	claimed, err := ClaimWorkOrder(ctx, st, order.ID, claim)
	requireOK(t, err)
	if claimed.AttemptID == "" || claimed.State != core.WorkOrderClaimed {
		t.Fatal("claim did not establish an attempt")
	}
	renewed, err := RenewWorkerClaim(ctx, st, order.ID, "worker", "session", 2*time.Minute)
	requireOK(t, err)
	if renewed.AttemptID != claimed.AttemptID || !renewed.ExecutionDeadline.Equal(claimed.ExecutionDeadline) || !renewed.LeaseExpiresAt.After(claimed.LeaseExpiresAt) {
		t.Fatal("renewal changed the attempt or failed to extend its lease")
	}
	identity := core.WorkOrderClaimIdentity{WorkerID: "worker", ClaimantID: "worker", SessionID: "session"}
	continued, err := st.RecordWorkOrderContinuation(ctx, order.ID, identity, core.WorkOrderContinuation{SessionID: "native-session", AttemptID: claimed.AttemptID, Harness: "codex", LaunchEnvironment: "test"})
	requireOK(t, err)
	if continued.AttemptID != claimed.AttemptID {
		t.Fatal("continuation changed attempt identity")
	}
	requireOK(t, st.UpsertWorkOrderActivitySnapshot(ctx, order.ID, identity, "first output"))
	requireOK(t, st.UpsertWorkOrderActivitySnapshot(ctx, order.ID, identity, "latest output"))
	snapshot, found, err := st.GetWorkOrderActivitySnapshot(ctx, order.ID)
	requireOK(t, err)
	if !found || snapshot.Content != "latest output" {
		t.Fatal("activity snapshot is not latest-only")
	}
	_, err = ReleaseWorkerClaim(ctx, st, order.ID, "worker", core.WorkOrderRelease{SessionID: "session", Reason: "fixture released", Outcome: core.WorkOrderOutcomeReleased})
	requireOK(t, err)
	checkpoint := core.WorkOrderAttemptCheckpoint{SessionID: "session", AttemptID: claimed.AttemptID, TerminationReason: "fixture released", Transcript: &core.WorkOrderAttemptTranscript{Content: "fixture transcript"}}
	recorded, err := st.RecordWorkOrderAttemptCheckpoint(ctx, order.ID, "worker", checkpoint)
	requireOK(t, err)
	if !recorded {
		t.Fatal("attempt checkpoint missing")
	}
	requireOK(t, st.FinalizeWorkOrderAttemptObservability(ctx, order.ID, "worker", checkpoint))
	captures, err := st.ListWorkOrderTranscriptCaptures(ctx, order.ID)
	requireOK(t, err)
	if len(captures) != 1 {
		t.Fatal("attempt transcript was not captured exactly once")
	}
	recovered, err := RecoverWorkOrder(ctx, st, order.ID, "recover-1", time.Hour)
	requireOK(t, err)
	if recovered.State != core.WorkOrderQueued {
		t.Fatal("recovery did not queue order")
	}
	second, err := ClaimWorkOrder(ctx, st, order.ID, claim)
	requireOK(t, err)
	if second.AttemptID == claimed.AttemptID {
		t.Fatal("reclaim reused attempt identity")
	}
	preempt, err := taskops.ExecuteWorkOrder(ctx, st, order.TaskID, core.WorkOrderCmdPreempt, func(lease taskops.TaskLease) (store.WorkOrderPreemptResult, error) {
		return st.PreemptWorkOrderCommand(ctx, lease, store.WorkOrderPreemptRequest{WorkOrderID: order.ID, RequestID: "preempt-1", Reason: "fixture preempt"})
	})
	requireOK(t, err)
	if preempt.RevokedAttemptID != second.AttemptID {
		t.Fatal("preempt retired the wrong attempt")
	}
	if _, err := RenewWorkerClaim(ctx, st, order.ID, "worker", "session", time.Minute); !errors.Is(err, store.ErrWorkOrderPreempted) {
		t.Fatalf("preempted renewal error=%v", err)
	}
	claim.SessionID = "successor"
	_, err = ClaimWorkOrder(ctx, st, order.ID, claim)
	requireOK(t, err)
	_, err = taskops.New(st).Cancel(ctx, core.Intervention{TaskID: order.TaskID, Action: core.InterventionCancel, ReasonCode: "obsolete"})
	requireOK(t, err)
	if _, err := RenewWorkerClaim(ctx, st, order.ID, "worker", "successor", time.Minute); !errors.Is(err, store.ErrWorkOrderCancelled) {
		t.Fatalf("cancelled renewal error=%v", err)
	}
	orders, err := st.ListTaskWorkOrdersSnapshot(ctx, order.TaskID)
	requireOK(t, err)
	if len(orders) != 1 || orders[0].State != core.WorkOrderCancelled {
		t.Fatal("cancelled order snapshot differs")
	}
}

func runWorkOrderClocks(t *testing.T, x Fixture) {
	st, ctx := x.Backend, x.Context
	stale := newAggregateOrder(t, x)
	_, err := taskops.ExecuteWorkOrder(ctx, st, stale.TaskID, core.WorkOrderCmdMarkStale, func(lease taskops.TaskLease) (int, error) {
		return st.ApplyWorkOrderClock(ctx, lease, stale.TaskID, stale.QueueDeadline.Add(time.Second))
	})
	requireOK(t, err)
	claim := core.WorkOrderClaim{SessionID: "clock", ClientToken: "fixture", Lease: time.Minute, ExecutionTimeout: time.Hour}
	if _, err := ClaimWorkOrder(ctx, st, stale.ID, claim); !errors.Is(err, store.ErrWorkOrderStale) {
		t.Fatalf("stale claim error=%v", err)
	}
	redispatched, err := RedispatchWorkOrder(ctx, st, stale.ID, time.Hour)
	requireOK(t, err)
	if redispatched.State != core.WorkOrderQueued || redispatched.RedispatchCount != 1 {
		t.Fatal("redispatch did not renew queue clock")
	}
	for _, direction := range []string{"", "Use the existing fixture evidence."} {
		order := newAggregateOrder(t, x)
		claim.ExecutionTimeout = time.Nanosecond
		claimed, err := ClaimWorkOrder(ctx, st, order.ID, claim)
		requireOK(t, err)
		_, err = taskops.New(st).TickOrderClock(ctx, time.Now().UTC())
		requireOK(t, err)
		if _, err := ClaimWorkOrder(ctx, st, order.ID, claim); !errors.Is(err, store.ErrWorkOrderTimedOut) {
			t.Fatalf("timed-out claim error=%v", err)
		}
		claimed.Progress = "late update"
		if err := UpdateWorkOrder(ctx, st, claimed); !errors.Is(err, store.ErrWorkOrderTimedOut) {
			t.Fatalf("timed-out update error=%v", err)
		}
		recovered, err := RecoverWorkOrderWithDirection(ctx, st, order.ID, "recover-"+core.NewTaskID(), direction, time.Hour)
		requireOK(t, err)
		if recovered.State != core.WorkOrderQueued || recovered.OperatorDirection != direction {
			t.Fatal("recovery direction or state differs")
		}
	}
}
