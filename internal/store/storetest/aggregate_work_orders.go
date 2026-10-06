package storetest

import (
	"errors"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

func newAggregateOrder(t *testing.T, x Fixture, stages ...core.Stage) core.WorkOrder {
	t.Helper()
	stage := core.StageImplement
	if len(stages) > 0 {
		stage = stages[0]
	}
	task := newAggregateTask(t, x, stage)
	job := core.Job{ID: task.ID + "-" + string(stage) + "-1", TaskID: task.ID, Stage: stage, State: core.JobPending}
	order := core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: job.Stage, State: core.WorkOrderQueued, HeadSHA: task.ReviewedHeadSHA, QueueEnteredAt: task.CreatedAt, QueueDeadline: task.CreatedAt.Add(time.Hour), CreatedAt: task.CreatedAt}
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
	t.Run("VerifyPolicy", func(t *testing.T) { runVerifyPolicy(t, x) })
	t.Run("VerifyAdmission", func(t *testing.T) { runVerifyAdmission(t, x) })
	t.Run("UsageTokenUpdates", func(t *testing.T) { runUsageTokenUpdates(t, x) })
	t.Run("ImplementationSubmission", func(t *testing.T) { runImplementationSubmission(t, x) })
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
	for _, stage := range []core.Stage{core.StageImplement, core.StageVerify} {
		t.Run(string(stage), func(t *testing.T) { runWorkOrderStageClocks(t, x, stage) })
	}
}

func runWorkOrderStageClocks(t *testing.T, x Fixture, stage core.Stage) {
	st, ctx := x.Backend, x.Context
	stale := newAggregateOrder(t, x, stage)
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
		order := newAggregateOrder(t, x, stage)
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

// submitImplementation admits SubmitImplementationCommand through the
// submit_for_review command lease, exactly as the work-order service does.
func submitImplementation(x Fixture, command core.WorkOrderCommand, order core.WorkOrder, head string) (core.Job, error) {
	order.State, order.HeadSHA = core.WorkOrderSubmitted, head
	return taskops.ExecuteWorkOrder(x.Context, x.Backend, order.TaskID, command, func(lease taskops.TaskLease) (core.Job, error) {
		return x.Backend.SubmitImplementationCommand(x.Context, lease, store.ImplementationSubmission{Order: order, EndedAt: time.Now().UTC().Truncate(time.Microsecond)})
	})
}

func submissionSnapshot(t *testing.T, x Fixture, order core.WorkOrder) (core.WorkOrder, map[string]core.JobState, int, core.Task) {
	t.Helper()
	current, err := x.Backend.GetWorkOrder(x.Context, order.ID)
	requireOK(t, err)
	jobs, err := x.Backend.ListJobs(x.Context, order.TaskID)
	requireOK(t, err)
	states := map[string]core.JobState{}
	for _, job := range jobs {
		states[job.ID] = job.State
	}
	events, err := x.Backend.ListEvents(x.Context, order.TaskID)
	requireOK(t, err)
	task, err := x.Backend.GetTask(x.Context, order.TaskID)
	requireOK(t, err)
	return current, states, len(events), task
}

// requireSubmissionRefusedWithoutWrites proves a refused submission rolled
// back the order update as well as the job completion.
func requireSubmissionRefusedWithoutWrites(t *testing.T, x Fixture, order core.WorkOrder, head string, command core.WorkOrderCommand, session string) {
	t.Helper()
	beforeOrder, beforeJobs, beforeEvents, beforeTask := submissionSnapshot(t, x, order)
	attempt := beforeOrder
	attempt.SessionID = session
	if _, err := submitImplementation(x, command, attempt, head); err == nil {
		t.Fatal("submission was not refused")
	}
	afterOrder, afterJobs, afterEvents, afterTask := submissionSnapshot(t, x, order)
	if afterOrder.State != beforeOrder.State || afterOrder.HeadSHA != beforeOrder.HeadSHA || afterOrder.SessionID != beforeOrder.SessionID || afterEvents != beforeEvents || afterTask.ReviewedHeadSHA != beforeTask.ReviewedHeadSHA {
		t.Fatalf("refused submission wrote state: order %s/%q→%s/%q events %d→%d reviewed %q→%q", beforeOrder.State, beforeOrder.HeadSHA, afterOrder.State, afterOrder.HeadSHA, beforeEvents, afterEvents, beforeTask.ReviewedHeadSHA, afterTask.ReviewedHeadSHA)
	}
	for id, state := range beforeJobs {
		if afterJobs[id] != state {
			t.Fatalf("refused submission changed job %s %s→%s", id, state, afterJobs[id])
		}
	}
}

func runImplementationSubmission(t *testing.T, x Fixture) {
	st, ctx := x.Backend, x.Context
	claim := core.WorkOrderClaim{ClaimantID: core.TaskRunClaimantID("usr-submit"), OwnerUserID: "usr-submit", SessionID: "submitter", ClientToken: "fixture", Lease: time.Hour, ExecutionTimeout: time.Hour}

	// The order's own job completes even when a newer job is the task's latest.
	order := newAggregateOrder(t, x)
	claimed, err := ClaimWorkOrder(ctx, st, order.ID, claim)
	requireOK(t, err)
	newer := core.Job{ID: order.TaskID + "-review-9-seat-1", TaskID: order.TaskID, Stage: core.StageReview, State: core.JobPending}
	requireOK(t, st.CreateJob(ctx, newer))
	_, _, before, _ := submissionSnapshot(t, x, order)
	completed, err := submitImplementation(x, core.WorkOrderCmdSubmitForReview, claimed, "submitted-head")
	requireOK(t, err)
	if completed.ID != order.JobID || completed.State != core.JobDone || completed.EndedAt.IsZero() {
		t.Fatalf("completed job=%+v", completed)
	}
	current, jobs, after, task := submissionSnapshot(t, x, order)
	if current.State != core.WorkOrderSubmitted || current.HeadSHA != "submitted-head" || jobs[order.JobID] != core.JobDone || jobs[newer.ID] != core.JobPending || task.ReviewedHeadSHA != "submitted-head" {
		t.Fatalf("order=%s/%s jobs=%v reviewed=%s", current.State, current.HeadSHA, jobs, task.ReviewedHeadSHA)
	}
	if after != before+2 {
		t.Fatalf("submission events %d→%d, want work_order.updated and job.updated", before, after)
	}
	events, err := st.ListEvents(ctx, order.TaskID)
	requireOK(t, err)
	if events[len(events)-2].Kind != "work_order.updated" || events[len(events)-1].Kind != "job.updated" || events[len(events)-1].JobID != order.JobID {
		t.Fatalf("submission events=%s,%s", events[len(events)-2].Kind, events[len(events)-1].Kind)
	}
	// A repeated completion is refused without a duplicate event.
	requireSubmissionRefusedWithoutWrites(t, x, order, "submitted-head", core.WorkOrderCmdSubmitForReview, "submitter")

	// A non-running job refuses and rolls back the already-applied order
	// update in the same transaction.
	failed := newAggregateOrder(t, x)
	_, err = ClaimWorkOrder(ctx, st, failed.ID, claim)
	requireOK(t, err)
	requireOK(t, st.UpdateJob(ctx, core.Job{ID: failed.JobID, TaskID: failed.TaskID, Stage: core.StageImplement, State: core.JobFailed, EndedAt: time.Now().UTC()}))
	requireSubmissionRefusedWithoutWrites(t, x, failed, "submitted-head", core.WorkOrderCmdSubmitForReview, "submitter")

	// Foreign sessions and a lease admitted for another command are refused.
	foreign := newAggregateOrder(t, x)
	foreign, err = ClaimWorkOrder(ctx, st, foreign.ID, claim)
	requireOK(t, err)
	requireSubmissionRefusedWithoutWrites(t, x, foreign, "submitted-head", core.WorkOrderCmdSubmitForReview, "intruder")
	requireSubmissionRefusedWithoutWrites(t, x, foreign, "submitted-head", core.WorkOrderCmdCreate, "submitter")
	if _, err = submitImplementation(x, core.WorkOrderCmdSubmitForReview, foreign, ""); err == nil {
		t.Fatal("submission without a head was accepted")
	}
	completed, err = submitImplementation(x, core.WorkOrderCmdSubmitForReview, foreign, "foreign-head")
	requireOK(t, err)
	if completed.ID != foreign.JobID {
		t.Fatalf("completed job=%s", completed.ID)
	}
}
