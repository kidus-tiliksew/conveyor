package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
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
	t.Run("RecoveryRefusal", func(t *testing.T) { runRecoveryRefusal(t, x) })
	t.Run("VerifyPolicyStranded", func(t *testing.T) { runVerifyPolicyStranded(t, x) })
	t.Run("UsageTokenUpdates", func(t *testing.T) { runUsageTokenUpdates(t, x) })
	t.Run("ImplementationSubmission", func(t *testing.T) { runImplementationSubmission(t, x) })
	t.Run("ImplementationSubmissionReviewSeats", func(t *testing.T) { runImplementationSubmissionReviewSeats(t, x) })
	t.Run("BaseConflictCheck", func(t *testing.T) { runBaseConflictCheck(t, x) })
	t.Run("StoredReadKeepsExpiredClaimUnprojected", func(t *testing.T) { runStoredReadKeepsExpiredClaim(t, x) })
	t.Run("LiveListing", func(t *testing.T) { runLiveWorkOrderListing(t, x) })
	t.Run("SnapshotFreeFoldReads", func(t *testing.T) { runSnapshotFreeFoldReads(t, x) })
	st, ctx := x.Backend, x.Context
	order := newAggregateOrder(t, x)
	claim := core.WorkOrderClaim{ClaimantID: "worker", SessionID: "session", ClientToken: "fixture", Lease: time.Minute, ExecutionTimeout: time.Hour}
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
	_, err = ReleaseWorkerClaim(ctx, st, order.ID, "worker", core.WorkOrderRelease{SessionID: "session", Reason: "fixture released", Outcome: core.WorkOrderOutcomeReleased})
	requireOK(t, err)
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

// runStoredReadKeepsExpiredClaim pins that ListTaskWorkOrdersStored returns a
// claim whose lease or execution deadline has elapsed exactly as stored, while
// the projected read settles it, until the order clock persists the expiry
// (component-work-orders "Stages and order minting"; DEC-62). One-nanosecond
// windows put the expiry in the past before the reads run; no sleep is needed.
func runStoredReadKeepsExpiredClaim(t *testing.T, x Fixture) {
	for _, tc := range []struct {
		name          string
		lease, window time.Duration
		projected     core.WorkOrderState
	}{
		{"LeaseExpired", time.Nanosecond, time.Hour, core.WorkOrderQueued},
		{"DeadlinePassed", time.Hour, time.Nanosecond, core.WorkOrderTimedOut},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, ctx := x.Backend, x.Context
			order := newAggregateOrder(t, x)
			claim := core.WorkOrderClaim{ClaimantID: "worker", SessionID: "expired", ClientToken: "fixture", Lease: tc.lease, ExecutionTimeout: tc.window}
			claimed, err := ClaimWorkOrder(ctx, st, order.ID, claim)
			requireOK(t, err)
			find := func(orders []core.WorkOrder) core.WorkOrder {
				t.Helper()
				for _, candidate := range orders {
					if candidate.ID == order.ID {
						return candidate
					}
				}
				t.Fatalf("order %s missing from %+v", order.ID, orders)
				return core.WorkOrder{}
			}
			storedOrders, err := st.ListTaskWorkOrdersStored(ctx, order.TaskID)
			requireOK(t, err)
			stored := find(storedOrders)
			if stored.State != core.WorkOrderClaimed || stored.SessionID != "expired" || stored.ClaimantID != claimed.ClaimantID || stored.AttemptID != claimed.AttemptID || !stored.LeaseExpiresAt.Equal(claimed.LeaseExpiresAt) || !stored.ExecutionDeadline.Equal(claimed.ExecutionDeadline) {
				t.Fatalf("stored read projected the expired claim: %+v", stored)
			}
			projectedOrders, err := st.ListTaskWorkOrders(ctx, order.TaskID)
			requireOK(t, err)
			if projected := find(projectedOrders); projected.State != tc.projected {
				t.Fatalf("projected read state=%s, want %s", projected.State, tc.projected)
			}
		})
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

// submitImplementationReviewSeats admits SubmitImplementationCommand with a
// review seat declaration through the submit_for_review command lease
// (feature-review-seat-minimum RS-4).
func submitImplementationReviewSeats(x Fixture, command core.WorkOrderCommand, order core.WorkOrder, head string, seats store.SubmissionReviewSeats) (core.Job, error) {
	order.State, order.HeadSHA = core.WorkOrderSubmitted, head
	return taskops.ExecuteWorkOrder(x.Context, x.Backend, order.TaskID, command, func(lease taskops.TaskLease) (core.Job, error) {
		return x.Backend.SubmitImplementationCommand(x.Context, lease, store.ImplementationSubmission{Order: order, EndedAt: time.Now().UTC().Truncate(time.Microsecond), ReviewSeats: seats})
	})
}

// submissionUpdatePayload is the order's newest work_order.updated payload.
func submissionUpdatePayload(t *testing.T, x Fixture, order core.WorkOrder) map[string]json.RawMessage {
	t.Helper()
	events, err := x.Backend.ListEvents(x.Context, order.TaskID)
	requireOK(t, err)
	for i := len(events) - 1; i >= 0; i-- {
		var payload map[string]json.RawMessage
		if events[i].Kind != "work_order.updated" || json.Unmarshal(events[i].Payload, &payload) != nil || string(payload["id"]) != `"`+order.ID+`"` {
			continue
		}
		return payload
	}
	t.Fatalf("no work_order.updated event for %s", order.ID)
	return nil
}

// requireDeclaredReviewSeats reads the stored declaration through both the
// exact and list reads.
func requireDeclaredReviewSeats(t *testing.T, x Fixture, order core.WorkOrder, want *int) {
	t.Helper()
	current, err := x.Backend.GetWorkOrder(x.Context, order.ID)
	requireOK(t, err)
	listed, err := x.Backend.ListTaskWorkOrders(x.Context, order.TaskID)
	requireOK(t, err)
	var fromList *int
	found := false
	for _, candidate := range listed {
		if candidate.ID == order.ID {
			fromList, found = candidate.DeclaredReviewSeats, true
		}
	}
	for label, got := range map[string]*int{"get": current.DeclaredReviewSeats, "list": fromList} {
		if (want == nil) != (got == nil) || (want != nil && *want != *got) {
			t.Fatalf("%s declared_review_seats=%s, want %s (listed=%t)", label, core.JSONPayload(got), core.JSONPayload(want), found)
		}
	}
}

// runImplementationSubmissionReviewSeats proves the submission-only writer of
// declared_review_seats: an accepted declaration is written atomically with
// the order update and its event and survives later metadata updates, caller
// pointers cannot alter stored state, omission stays null, an unused refresh
// count is recorded only in the event, and every refusal rolls back
// (feature-review-seat-minimum RS-4).
func runImplementationSubmissionReviewSeats(t *testing.T, x Fixture) {
	st, ctx := x.Backend, x.Context
	claim := core.WorkOrderClaim{ClaimantID: core.TaskRunClaimantID("usr-seats"), OwnerUserID: "usr-seats", SessionID: "seats", ClientToken: "fixture", Lease: time.Hour, ExecutionTimeout: time.Hour}
	three := 3

	accepted := newAggregateOrder(t, x)
	claimed, err := ClaimWorkOrder(ctx, st, accepted.ID, claim)
	requireOK(t, err)
	// A caller-supplied order field is never the writer.
	claimed.DeclaredReviewSeats = &three
	declared := 2
	_, err = submitImplementationReviewSeats(x, core.WorkOrderCmdSubmitForReview, claimed, "seats-head", store.SubmissionReviewSeats{Declared: &declared})
	requireOK(t, err)
	declared = 9
	two := 2
	requireDeclaredReviewSeats(t, x, accepted, &two)
	payload := submissionUpdatePayload(t, x, accepted)
	if string(payload["declared_review_seats"]) != "2" || string(payload["state"]) != `"submitted"` || payload["review_seats"] != nil || payload["review_seats_applied"] != nil {
		t.Fatalf("accepted submission payload=%s", core.JSONPayload(payload))
	}
	current, err := st.GetWorkOrder(ctx, accepted.ID)
	requireOK(t, err)
	*current.DeclaredReviewSeats = 8
	requireDeclaredReviewSeats(t, x, accepted, &two)
	// A later metadata update cannot replace or clear the declaration, and
	// its event carries the stored value.
	current.Progress = "metadata"
	current.DeclaredReviewSeats = &three
	requireOK(t, UpdateWorkOrder(ctx, st, current))
	requireDeclaredReviewSeats(t, x, accepted, &two)
	if payload = submissionUpdatePayload(t, x, accepted); string(payload["declared_review_seats"]) != "2" {
		t.Fatalf("metadata update payload=%s", core.JSONPayload(payload))
	}
	current.DeclaredReviewSeats = nil
	current.Progress = "metadata-clear"
	requireOK(t, UpdateWorkOrder(ctx, st, current))
	requireDeclaredReviewSeats(t, x, accepted, &two)

	// Omission keeps the column null and the event shape unchanged.
	omitted := newAggregateOrder(t, x)
	claimed, err = ClaimWorkOrder(ctx, st, omitted.ID, claim)
	requireOK(t, err)
	_, err = submitImplementationReviewSeats(x, core.WorkOrderCmdSubmitForReview, claimed, "seats-head", store.SubmissionReviewSeats{})
	requireOK(t, err)
	requireDeclaredReviewSeats(t, x, omitted, nil)
	if payload = submissionUpdatePayload(t, x, omitted); payload["declared_review_seats"] != nil || payload["review_seats"] != nil || payload["review_seats_applied"] != nil {
		t.Fatalf("omitted submission payload=%s", core.JSONPayload(payload))
	}

	// An unused refresh count is recorded in the event only.
	unused := newAggregateOrder(t, x)
	claimed, err = ClaimWorkOrder(ctx, st, unused.ID, claim)
	requireOK(t, err)
	four := 4
	_, err = submitImplementationReviewSeats(x, core.WorkOrderCmdSubmitForReview, claimed, "seats-head", store.SubmissionReviewSeats{Unused: &four})
	requireOK(t, err)
	requireDeclaredReviewSeats(t, x, unused, nil)
	if payload = submissionUpdatePayload(t, x, unused); payload["declared_review_seats"] != nil || string(payload["review_seats"]) != "4" || string(payload["review_seats_applied"]) != "false" || string(payload["state"]) != `"submitted"` {
		t.Fatalf("unused submission payload=%s", core.JSONPayload(payload))
	}

	// Refusals write nothing: invalid counts, a non-running job after the
	// order update, a foreign session, and a lease for another command.
	refused := newAggregateOrder(t, x)
	refusedClaim, err := ClaimWorkOrder(ctx, st, refused.ID, claim)
	requireOK(t, err)
	zero := 0
	for _, seats := range []store.SubmissionReviewSeats{{Declared: &zero}, {Unused: &zero}, {Declared: &two, Unused: &four}} {
		beforeOrder, _, beforeEvents, _ := submissionSnapshot(t, x, refused)
		if _, err = submitImplementationReviewSeats(x, core.WorkOrderCmdSubmitForReview, refusedClaim, "seats-head", seats); err == nil {
			t.Fatalf("invalid review seats %+v accepted", seats)
		}
		afterOrder, _, afterEvents, _ := submissionSnapshot(t, x, refused)
		if afterOrder.State != beforeOrder.State || afterEvents != beforeEvents {
			t.Fatalf("invalid review seats wrote state")
		}
	}
	for _, attempt := range []struct {
		session string
		command core.WorkOrderCommand
	}{{"intruder", core.WorkOrderCmdSubmitForReview}, {"seats", core.WorkOrderCmdCreate}} {
		beforeOrder, _, beforeEvents, _ := submissionSnapshot(t, x, refused)
		foreign := refusedClaim
		foreign.SessionID = attempt.session
		if _, err = submitImplementationReviewSeats(x, attempt.command, foreign, "seats-head", store.SubmissionReviewSeats{Declared: &two}); err == nil {
			t.Fatalf("submission %+v accepted", attempt)
		}
		afterOrder, _, afterEvents, _ := submissionSnapshot(t, x, refused)
		if afterOrder.State != beforeOrder.State || afterEvents != beforeEvents {
			t.Fatal("refused submission wrote state")
		}
		requireDeclaredReviewSeats(t, x, refused, nil)
	}
	requireOK(t, st.UpdateJob(ctx, core.Job{ID: refused.JobID, TaskID: refused.TaskID, Stage: core.StageImplement, State: core.JobFailed, EndedAt: time.Now().UTC()}))
	beforeOrder, _, beforeEvents, _ := submissionSnapshot(t, x, refused)
	if _, err = submitImplementationReviewSeats(x, core.WorkOrderCmdSubmitForReview, refusedClaim, "seats-head", store.SubmissionReviewSeats{Declared: &two}); err == nil {
		t.Fatal("submission with a failed job accepted")
	}
	afterOrder, _, afterEvents, _ := submissionSnapshot(t, x, refused)
	if afterOrder.State != beforeOrder.State || afterOrder.HeadSHA != beforeOrder.HeadSHA || afterEvents != beforeEvents {
		t.Fatal("failed job submission did not roll back")
	}
	requireDeclaredReviewSeats(t, x, refused, nil)
}

// runLiveWorkOrderListing stores one order in each of the seven states and
// proves ListLiveWorkOrders returns exactly the queued, claimed, stale, and
// timed-out ones, in ListWorkOrders order and isolated to the workspace
// (component-work-orders, Listing).
func runLiveWorkOrderListing(t *testing.T, x Fixture) {
	st, ctx := x.Backend, x.Context
	claim := func(order core.WorkOrder, session string) core.WorkOrder {
		t.Helper()
		claimed, err := ClaimWorkOrder(ctx, st, order.ID, core.WorkOrderClaim{SessionID: session, ClientToken: session, Lease: time.Hour, ExecutionTimeout: time.Hour})
		requireOK(t, err)
		return claimed
	}
	queued := newAggregateOrder(t, x)
	claimed := claim(newAggregateOrder(t, x), "live-claimed")
	stale := newAggregateOrder(t, x)
	_, err := taskops.ExecuteWorkOrder(ctx, st, stale.TaskID, core.WorkOrderCmdMarkStale, func(lease taskops.TaskLease) (int, error) {
		return st.ApplyWorkOrderClock(ctx, lease, stale.TaskID, stale.QueueDeadline.Add(time.Second))
	})
	requireOK(t, err)
	timedOut, err := st.GetWorkOrder(ctx, newAggregateOrder(t, x).ID)
	requireOK(t, err)
	timedOut.State = core.WorkOrderTimedOut
	requireOK(t, UpdateWorkOrder(ctx, st, timedOut, core.WorkOrderCmdTimeout))
	submitted := claim(newAggregateOrder(t, x), "live-submitted")
	submitted.State = core.WorkOrderSubmitted
	requireOK(t, UpdateWorkOrder(ctx, st, submitted, core.WorkOrderCmdSubmitForReview))
	completed := claim(newAggregateOrder(t, x, core.StageSpec), "live-completed")
	completed.State = core.WorkOrderCompleted
	requireOK(t, UpdateWorkOrder(ctx, st, completed, core.WorkOrderCmdSubmitSpec))
	cancelled, err := st.GetWorkOrder(ctx, newAggregateOrder(t, x).ID)
	requireOK(t, err)
	cancelled.State = core.WorkOrderCancelled
	requireOK(t, UpdateWorkOrder(ctx, st, cancelled, core.WorkOrderCmdCancel))

	seeded := map[string]core.WorkOrderState{
		queued.ID: core.WorkOrderQueued, claimed.ID: core.WorkOrderClaimed, stale.ID: core.WorkOrderStale, timedOut.ID: core.WorkOrderTimedOut,
		submitted.ID: core.WorkOrderSubmitted, completed.ID: core.WorkOrderCompleted, cancelled.ID: core.WorkOrderCancelled,
	}
	for id, want := range seeded {
		got, err := st.GetWorkOrder(ctx, id)
		requireOK(t, err)
		if got.State != want {
			t.Fatalf("seeded order %s state=%s, want %s", id, got.State, want)
		}
	}
	all, err := st.ListWorkOrders(ctx)
	requireOK(t, err)
	var want []string
	for _, order := range all {
		if order.State == core.WorkOrderQueued || order.State == core.WorkOrderClaimed || order.State == core.WorkOrderStale || order.State == core.WorkOrderTimedOut {
			want = append(want, order.ID)
		}
	}
	live, err := st.ListLiveWorkOrders(ctx)
	requireOK(t, err)
	got := make([]string, 0, len(live))
	listed := map[string]bool{}
	for _, order := range live {
		got = append(got, order.ID)
		listed[order.ID] = true
	}
	if !equalStrings(got, want) {
		t.Fatalf("ListLiveWorkOrders=%v, want the live ListWorkOrders sequence %v", got, want)
	}
	for id, state := range seeded {
		live := state == core.WorkOrderQueued || state == core.WorkOrderClaimed || state == core.WorkOrderStale || state == core.WorkOrderTimedOut
		if listed[id] != live {
			t.Fatalf("order %s in state %s listed=%v, want %v", id, state, listed[id], live)
		}
	}
	foreign, err := st.ListLiveWorkOrders(store.WithWorkspace(ctx, x.Workspace+"-live-foreign"))
	requireOK(t, err)
	for _, order := range foreign {
		if _, ok := seeded[order.ID]; ok {
			t.Fatalf("foreign workspace listed order %s", order.ID)
		}
	}
}

// foldGovernanceBytes is the size of the design body each snapshot-bearing
// fixture order pins. It dominates the encoded snapshot, so a read that
// decodes even one snapshot reports at least this many bytes.
const foldGovernanceBytes = 64 << 10

// snapshotBytes sums the encoded served-requirement and governance snapshots
// that a read returned. A durable backend fills either field only by decoding
// the selected column, so zero means the read selected neither body.
func snapshotBytes(t *testing.T, orders []core.WorkOrder) int {
	t.Helper()
	total := 0
	for _, order := range orders {
		if order.ServedRequirementSnapshot != nil {
			encoded, err := json.Marshal(order.ServedRequirementSnapshot)
			requireOK(t, err)
			total += len(encoded)
		}
		if order.GovernanceSnapshot != nil {
			encoded, err := json.Marshal(order.GovernanceSnapshot)
			requireOK(t, err)
			total += len(encoded)
		}
	}
	return total
}

// withoutSnapshots returns copies of orders with both authority snapshots
// cleared, the shape every snapshot-free read must return.
func withoutSnapshots(orders []core.WorkOrder) []core.WorkOrder {
	out := make([]core.WorkOrder, len(orders))
	for i, order := range orders {
		order.ServedRequirementSnapshot, order.GovernanceSnapshot = nil, nil
		out[i] = order
	}
	return out
}

func ordersOfTasks(orders []core.WorkOrder, taskIDs ...string) []core.WorkOrder {
	wanted := map[string]bool{}
	for _, id := range taskIDs {
		wanted[id] = true
	}
	out := []core.WorkOrder{}
	for _, order := range orders {
		if wanted[order.TaskID] {
			out = append(out, order)
		}
	}
	return out
}

func requireSameOrders(t *testing.T, label string, got, want []core.WorkOrder) {
	t.Helper()
	gotJSON, err := json.Marshal(got)
	requireOK(t, err)
	wantJSON, err := json.Marshal(want)
	requireOK(t, err)
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("%s differs from the full read without snapshots:\ngot  %s\nwant %s", label, gotJSON, wantJSON)
	}
}

// runSnapshotFreeFoldReads seeds orders that pin served-requirement and
// 64 KiB governance snapshots on several tasks, and proves the fold reads the
// dispatcher and the live list use decode no snapshot byte, return only the
// requested task's rows, and otherwise equal the full read. Review readiness
// gives the same answer over the task-scoped read as over the workspace read,
// for a pending and a sealed verify order (component-work-orders,
// Snapshot-free fold reads and Listing).
func runSnapshotFreeFoldReads(t *testing.T, x Fixture) {
	st, ctx := x.Backend, x.Context
	governance := core.GovernanceSnapshot{
		Designs:                []core.GovernanceDesignContext{{ID: "component-fold-fixture", Title: "Fold fixture", Category: "component", Version: 1, Content: strings.Repeat("g", foldGovernanceBytes), Governs: []core.GovernedScope{}}},
		Decisions:              []core.Decision{},
		PendingDesignProposals: []core.PendingSystemDesignProposal{},
	}
	served := []core.ServedRequirementContext{{ID: "req-fold-fixture", Version: 1, Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Observe the fold"}}}}
	pinned := func(name string, complete bool) core.WorkOrder {
		t.Helper()
		order := newAggregateOrder(t, x, core.StageVerify)
		claimed, err := ClaimWorkOrder(ctx, st, order.ID, core.WorkOrderClaim{ClaimantID: core.TaskRunClaimantID("worker"), SessionID: "fold-" + name, ClientToken: "fold-" + name, Lease: time.Hour, ExecutionTimeout: time.Hour, Requirements: served, Governance: &governance})
		requireOK(t, err)
		if complete {
			claimed.State = core.WorkOrderCompleted
			requireOK(t, UpdateWorkOrder(ctx, st, claimed, core.WorkOrderCmdSubmitVerification))
		}
		stored, err := st.GetWorkOrder(ctx, order.ID)
		requireOK(t, err)
		if stored.GovernanceSnapshot == nil || len(stored.GovernanceSnapshot.Designs) != 1 || len(stored.GovernanceSnapshot.Designs[0].Content) != foldGovernanceBytes || stored.ServedRequirementSnapshot == nil {
			t.Fatalf("GetWorkOrder lost the pinned snapshots of %s: %+v", order.ID, stored.GovernanceSnapshot)
		}
		return stored
	}
	live := pinned("live", false)
	completed := pinned("completed", true)
	foreign := pinned("foreign", false)
	pending := newVerificationFixture(t, x, true)
	sealed := sealedVerifyFixture(t, x)
	taskIDs := []string{live.TaskID, completed.TaskID, foreign.TaskID, pending.access.TaskID, sealed.access.TaskID}

	full, err := st.ListWorkOrders(ctx)
	requireOK(t, err)
	if seeded := snapshotBytes(t, ordersOfTasks(full, live.TaskID, completed.TaskID, foreign.TaskID)); seeded < 3*foldGovernanceBytes {
		t.Fatalf("full read decoded %d snapshot bytes of the three pinned orders, want at least %d", seeded, 3*foldGovernanceBytes)
	}
	for _, id := range []string{pending.access.TaskID, sealed.access.TaskID} {
		if snapshotBytes(t, ordersOfTasks(full, id)) == 0 {
			t.Fatalf("full read decoded no served-requirement snapshot for verification task %s", id)
		}
	}

	listed, err := st.ListLiveWorkOrders(ctx)
	requireOK(t, err)
	if n := snapshotBytes(t, listed); n != 0 {
		t.Fatalf("ListLiveWorkOrders decoded %d snapshot bytes, want 0", n)
	}
	var wantLive []core.WorkOrder
	for _, order := range ordersOfTasks(full, taskIDs...) {
		if order.State == core.WorkOrderQueued || order.State == core.WorkOrderClaimed || order.State == core.WorkOrderStale || order.State == core.WorkOrderTimedOut {
			wantLive = append(wantLive, order)
		}
	}
	requireSameOrders(t, "ListLiveWorkOrders", ordersOfTasks(listed, taskIDs...), withoutSnapshots(wantLive))

	for _, id := range taskIDs {
		task, err := st.GetTask(ctx, id)
		requireOK(t, err)
		scoped, err := st.ListWorkOrdersForTasks(ctx, []string{id})
		requireOK(t, err)
		if n := snapshotBytes(t, scoped); n != 0 {
			t.Fatalf("ListWorkOrdersForTasks(%s) decoded %d snapshot bytes, want 0", id, n)
		}
		if len(ordersOfTasks(scoped, id)) != len(scoped) || len(scoped) == 0 {
			t.Fatalf("ListWorkOrdersForTasks(%s) returned %d rows, %d of them the task's", id, len(scoped), len(ordersOfTasks(scoped, id)))
		}
		requireSameOrders(t, "ListWorkOrdersForTasks("+id+")", scoped, withoutSnapshots(ordersOfTasks(full, id)))

		stored, err := st.ListTaskWorkOrdersStored(ctx, id)
		requireOK(t, err)
		if n := snapshotBytes(t, stored); n != 0 {
			t.Fatalf("ListTaskWorkOrdersStored(%s) decoded %d snapshot bytes, want 0", id, n)
		}
		want := ordersOfTasks(full, id)
		if len(stored) != len(want) {
			t.Fatalf("ListTaskWorkOrdersStored(%s) returned %d rows, want the task's %d", id, len(stored), len(want))
		}
		for i := range stored {
			got, wanted := stored[i], want[i]
			if got.TaskID != id || got.ID != wanted.ID || got.Stage != wanted.Stage || got.State != wanted.State || got.HeadSHA != wanted.HeadSHA ||
				got.VerificationContextID != wanted.VerificationContextID || got.AttemptID != wanted.AttemptID || got.SessionID != wanted.SessionID ||
				got.ReasonCode != wanted.ReasonCode || got.ReviewRound != wanted.ReviewRound || !got.CreatedAt.Equal(wanted.CreatedAt) ||
				!got.LeaseExpiresAt.Equal(wanted.LeaseExpiresAt) || !got.ExecutionDeadline.Equal(wanted.ExecutionDeadline) {
				t.Fatalf("ListTaskWorkOrdersStored(%s)[%d] fold fields differ:\ngot  %+v\nwant %+v", id, i, got, wanted)
			}
		}

		// Readiness folds the same answer from the task-scoped read as from
		// the workspace read the dispatcher used before.
		if scopedReady, workspaceReady := core.VerifyReviewReady(task, scoped), core.VerifyReviewReady(task, full); scopedReady != workspaceReady {
			t.Fatalf("VerifyReviewReady(%s) over the task read=%v, over the workspace read=%v", id, scopedReady, workspaceReady)
		}
	}
	readiness := func(id string) bool {
		t.Helper()
		task, err := st.GetTask(ctx, id)
		requireOK(t, err)
		orders, err := st.ListWorkOrdersForTasks(ctx, []string{id})
		requireOK(t, err)
		return core.VerifyReviewReady(task, orders)
	}
	if readiness(pending.access.TaskID) || readiness(completed.TaskID) || !readiness(sealed.access.TaskID) {
		t.Fatalf("readiness pending=%v completed-unsealed=%v sealed=%v, want false false true", readiness(pending.access.TaskID), readiness(completed.TaskID), readiness(sealed.access.TaskID))
	}

	pair, err := st.ListWorkOrdersForTasks(ctx, []string{live.TaskID, sealed.access.TaskID})
	requireOK(t, err)
	if len(ordersOfTasks(pair, live.TaskID, sealed.access.TaskID)) != len(pair) || snapshotBytes(t, pair) != 0 {
		t.Fatalf("ListWorkOrdersForTasks of two tasks returned other tasks' rows or snapshots: %d rows", len(pair))
	}
	requireSameOrders(t, "ListWorkOrdersForTasks(pair)", pair, withoutSnapshots(ordersOfTasks(full, live.TaskID, sealed.access.TaskID)))

	// The full per-task read keeps the pins for the readers that need them.
	kept, err := st.ListTaskWorkOrders(ctx, live.TaskID)
	requireOK(t, err)
	if snapshotBytes(t, kept) < foldGovernanceBytes {
		t.Fatal("ListTaskWorkOrders dropped the pinned snapshots its claim-time readers need")
	}
	if other, err := st.ListWorkOrdersForTasks(store.WithWorkspace(ctx, x.Workspace+"-fold-foreign"), taskIDs); err != nil || len(other) != 0 {
		t.Fatalf("another workspace read %d of the fixture's orders: %v", len(other), err)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

type identityDeactivator interface {
	DeactivateIdentityUser(context.Context, string) (core.IdentityUser, error)
}

// deactivateUser marks an account inactive through the backend's deactivation
// operation, or through the fixture's hook where the backend has none.
func deactivateUser(t *testing.T, x Fixture, ctx context.Context, userID string) {
	t.Helper()
	if x.DeactivateUser != nil {
		x.DeactivateUser(t, ctx, userID)
		return
	}
	deactivator, ok := x.Backend.(identityDeactivator)
	if !ok {
		t.Fatal("backend cannot deactivate identities and the fixture supplies no hook")
	}
	_, err := deactivator.DeactivateIdentityUser(ctx, userID)
	requireOK(t, err)
}

// runHoldRetirement proves a stored historical hold reserves nothing: an
// order of a held task, and the fresh implement order a merge-gate bounce
// mints, are listed claimable and claimed through the real work-order claim
// service as a run:<owner> session (component-work-orders, Claim lifecycle;
// component-task-lifecycle; DEC-74(3), DEC-97(1)).
func runHoldRetirement(t *testing.T, x Fixture, claimService ClaimServiceFunc) {
	if claimService == nil {
		t.Fatal("conformance factory must provide ClaimService for the hold-retirement cases (DEC-74(3))")
	}
	claim := claimService(x.Backend, x.Config)
	t.Run("StoredTaskHoldIsClaimable", func(t *testing.T) { runStoredTaskHoldIsClaimable(t, x, claim) })
	t.Run("RequestChangesRemainsClaimable", func(t *testing.T) { runRequestChangesRemainsClaimable(t, x, claim, false) })
	t.Run("RequestChangesRemainsClaimableWithStoredHold", func(t *testing.T) { runRequestChangesRemainsClaimable(t, x, claim, true) })
	t.Run("HistoricalHoldEventsRemainReadable", func(t *testing.T) { runHistoricalHoldEventsRemainReadable(t, x) })
}

type holdRetirementClaim = func(ctx context.Context, id string, claim core.WorkOrderClaim) (core.WorkOrder, error)

func holdRetirementSessionClaim(owner, session string) core.WorkOrderClaim {
	return core.WorkOrderClaim{SessionID: session, ClientToken: session + "-token", ClaimantID: core.TaskRunClaimantID(owner), OwnerUserID: owner, Agent: "codex", Model: "model", Lease: time.Minute, ExecutionTimeout: time.Hour}
}

func queuedHoldRetirementOrder(t *testing.T, x Fixture, ctx context.Context, name string) core.WorkOrder {
	t.Helper()
	task := core.Task{ID: "hold-" + name + "-" + core.NewTaskID(), Workspace: x.Workspace, Title: "hold retirement " + name, State: core.TaskQueued, NextStage: core.StageImplement, Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-hold-" + name + "-" + core.NewTaskID(), CreatedAt: time.Now().UTC()}
	requireOK(t, x.Backend.CreateTask(ctx, task))
	job := core.Job{ID: task.ID + "-implement", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending, StartedAt: time.Now().UTC()}
	order := core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued}
	created, err := For(x.Backend).CreateStageWorkOrder(ctx, job, order)
	requireOK(t, err)
	if !created {
		t.Fatal("stage work order was not created")
	}
	return order
}

// requireListedClaimable checks the live work-order listing's claimability
// of one order.
func requireListedClaimable(t *testing.T, x Fixture, ctx context.Context, orderID string, claimable bool) {
	t.Helper()
	orders, err := x.Backend.ListLiveWorkOrders(ctx)
	requireOK(t, err)
	for _, order := range orders {
		if order.ID == orderID {
			if got := order.State == core.WorkOrderQueued && order.ClaimableAt(time.Now().UTC()); got != claimable {
				t.Fatalf("listed order %s claimable=%v, want %v: %+v", orderID, got, claimable, order)
			}
			return
		}
	}
	t.Fatalf("live listing omits %s", orderID)
}

// requireNoHoldEvents proves no hold event was appended for the task: the
// retired reservation has no writer (DEC-74(3)).
func requireNoHoldEvents(t *testing.T, x Fixture, taskID string) {
	t.Helper()
	events, err := x.Backend.ListEvents(x.Context, taskID)
	requireOK(t, err)
	for _, event := range events {
		if strings.HasPrefix(event.Kind, "task.hold") {
			t.Fatalf("hold event appended after the retirement: %+v", event)
		}
	}
}

// requireTaskJSONOmitsHold proves the task projection carries no hold key.
func requireTaskJSONOmitsHold(t *testing.T, x Fixture, taskID string) {
	t.Helper()
	task, err := x.Backend.GetTask(x.Context, taskID)
	requireOK(t, err)
	encoded, err := json.Marshal(task)
	requireOK(t, err)
	var fields map[string]json.RawMessage
	requireOK(t, json.Unmarshal(encoded, &fields))
	if _, present := fields["hold"]; present {
		t.Fatalf("task JSON still carries hold: %s", encoded)
	}
}

func runStoredTaskHoldIsClaimable(t *testing.T, x Fixture, claim holdRetirementClaim) {
	seed := requireStoredTaskHoldSeed(t, x.SeedStoredTaskHold)
	st, ctx := x.Backend, x.Context
	owner := "hold-owner-" + core.NewTaskID()
	held := queuedHoldRetirementOrder(t, x, ctx, "stored")
	seed(t, ctx, held.TaskID)
	assertStoredHold(t, ctx, st, held.TaskID, true)
	requireListedClaimable(t, x, ctx, held.ID, true)
	claimed, err := claim(ctx, held.ID, holdRetirementSessionClaim(owner, "hold-stored-session"))
	requireOK(t, err)
	if claimed.WorkerID != "" || claimed.ClaimantID != core.TaskRunClaimantID(owner) || claimed.State != core.WorkOrderClaimed {
		t.Fatalf("claim=%+v", claimed)
	}
	assertStoredHold(t, ctx, st, held.TaskID, true)
	requireTaskJSONOmitsHold(t, x, held.TaskID)
	requireNoHoldEvents(t, x, held.TaskID)

	// Assignment to another user still refuses, so the retirement broadens
	// no other eligibility (DEC-74(4)).
	theirs := queuedHoldRetirementOrder(t, x, ctx, "theirs")
	seed(t, ctx, theirs.TaskID)
	operatorCtx, _ := bootstrapOwner(t, x)
	otherEmail := "hold-other-" + strings.ToLower(core.NewTaskID()) + "@hold.test"
	other, err := st.ProvisionIdentityUser(operatorCtx, otherEmail, "Other assignee")
	requireOK(t, err)
	_, err = st.GrantWorkspaceRole(operatorCtx, otherEmail, x.Workspace, core.WorkspaceRoleContributor)
	requireOK(t, err)
	_, err = taskops.New(st).SetAssignee(ctx, theirs.TaskID, other.ID)
	requireOK(t, err)
	if _, err := claim(ctx, theirs.ID, holdRetirementSessionClaim(owner, "hold-theirs-session")); err == nil {
		t.Fatal("a session claimed a task assigned to another user")
	}
	if persisted, err := st.GetWorkOrder(ctx, theirs.ID); err != nil || persisted.State != core.WorkOrderQueued {
		t.Fatalf("refused claim changed order: %+v %v", persisted, err)
	}
}

// runRequestChangesRemainsClaimable reaches the merge gate after a
// run:<owner> implement claim, requests changes, and proves the fresh
// implement order is an ordinary eligible order: listed claimable, claimed
// through the claim service, and clearing the returned-for-changes attention,
// with no hold written (DEC-74(3)).
func runRequestChangesRemainsClaimable(t *testing.T, x Fixture, claim holdRetirementClaim, seedHold bool) {
	name := "bounce"
	if seedHold {
		name += "-held"
	}
	st := x.Backend
	requester := "requester-" + core.NewTaskID()
	ctx := store.WithActor(x.Context, store.Actor{ID: store.UserActorID(requester), Role: core.ActorUser})
	now := time.Now().UTC()
	task := core.Task{ID: "hold-" + name + "-" + core.NewTaskID(), Workspace: x.Workspace, Title: "bounce " + name, Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-hold-" + name + "-" + core.NewTaskID(), State: core.TaskRunning, NextStage: core.StageReview, RecoveryStage: core.StageImplement, CreatedAt: now}
	requireOK(t, st.CreateTask(ctx, task))
	reviewJob := core.Job{ID: task.ID + "-review-1-seat-1", TaskID: task.ID, Stage: core.StageReview, State: core.JobDone, StartedAt: now}
	requireOK(t, st.CreateJob(ctx, reviewJob))
	requireOK(t, st.AppendEvent(ctx, core.Event{TaskID: task.ID, Kind: "work_order.claimed", Payload: core.JSONPayload(core.WorkOrder{ID: task.ID + "-implement-1", Stage: core.StageImplement, ClaimantID: core.TaskRunClaimantID(requester)})}))
	if seedHold {
		requireStoredTaskHoldSeed(t, x.SeedStoredTaskHold)(t, ctx, task.ID)
	}
	_, err := taskops.New(st).Perform(ctx, task.ID, taskops.Command{Kind: core.TaskGateMerge, RecoveryStage: core.StageImplement, ProjectStages: true})
	requireOK(t, err)

	feedback := "Line one.\n\n  exact indentation for " + name
	updated, err := taskops.New(st).RequestChanges(ctx, taskops.RequestChanges{TaskID: task.ID, JobID: reviewJob.ID, Feedback: feedback, MaxBounces: 3})
	requireOK(t, err)
	if updated.State != core.TaskQueued || updated.NextStage != core.StageImplement {
		t.Fatalf("bounced task=%+v", updated)
	}
	interventions, err := st.ListInterventions(ctx, task.ID)
	requireOK(t, err)
	if len(interventions) != 1 || interventions[0].Comment != feedback {
		t.Fatalf("interventions=%+v, want the feedback verbatim", interventions)
	}
	markers, err := st.ListActivityMarkersForTasks(ctx, []string{task.ID})
	requireOK(t, err)
	if len(markers) != 1 || !markers[0].UserChangesRequested {
		t.Fatalf("markers after bounce=%+v", markers)
	}
	requireNoHoldEvents(t, x, task.ID)
	assertStoredHold(t, ctx, st, task.ID, seedHold)

	// The dispatcher mints the fresh implement order; storetest cannot import
	// dispatch, so the case mints it through the same store command.
	job := core.Job{ID: task.ID + "-implement-2", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending, StartedAt: time.Now().UTC()}
	order := core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued}
	created, err := For(st).CreateStageWorkOrder(ctx, job, order)
	requireOK(t, err)
	if !created {
		t.Fatal("fresh implement order was not created")
	}
	requireListedClaimable(t, x, ctx, order.ID, true)
	claimed, err := claim(ctx, order.ID, holdRetirementSessionClaim(requester, name+"-session"))
	requireOK(t, err)
	if claimed.ClaimantID != core.TaskRunClaimantID(requester) || claimed.State != core.WorkOrderClaimed {
		t.Fatalf("claim=%+v", claimed)
	}
	markers, err = st.ListActivityMarkersForTasks(ctx, []string{task.ID})
	requireOK(t, err)
	if len(markers) != 1 || markers[0].UserChangesRequested {
		t.Fatalf("markers after the claim=%+v", markers)
	}
	requireNoHoldEvents(t, x, task.ID)
	assertStoredHold(t, ctx, st, task.ID, seedHold)
	requireTaskJSONOmitsHold(t, x, task.ID)
}

// runHistoricalHoldEventsRemainReadable proves events recorded before the
// retirement stay in the ledger exactly as stored (DEC-10, DEC-74(3)).
func runHistoricalHoldEventsRemainReadable(t *testing.T, x Fixture) {
	st, ctx := x.Backend, x.Context
	task := newAggregateTask(t, x)
	historical := []core.Event{
		{TaskID: task.ID, Kind: "task.hold.set", Payload: core.JSONPayload(map[string]any{"hold": true})},
		{TaskID: task.ID, Kind: "task.hold.cleared", Payload: core.JSONPayload(map[string]any{"hold": false})},
		{TaskID: task.ID, Kind: "task.hold_changed", Payload: core.JSONPayload(map[string]any{"hold": true, "reason_code": store.UserRequestChangesReason})},
	}
	for _, event := range historical {
		requireOK(t, st.AppendEvent(ctx, event))
	}
	events, err := st.ListEvents(ctx, task.ID)
	requireOK(t, err)
	for _, want := range historical {
		found := false
		for _, event := range events {
			if event.Kind != want.Kind {
				continue
			}
			var got, expected map[string]any
			requireOK(t, json.Unmarshal(event.Payload, &got))
			requireOK(t, json.Unmarshal(want.Payload, &expected))
			found = found || fmt.Sprint(got) == fmt.Sprint(expected)
		}
		if !found {
			t.Fatalf("historical %s event is not readable as stored: %+v", want.Kind, events)
		}
	}
	requireTaskJSONOmitsHold(t, x, task.ID)
}

type fixtureMemberProvisioner interface {
	ProvisionIdentityUser(context.Context, string, string) (core.IdentityUser, error)
	GrantWorkspaceRole(context.Context, string, string, core.WorkspaceRole) (core.MembershipGrant, error)
}

var fixtureEmailUnsafe = regexp.MustCompile(`[^a-z0-9-]+`)

// enrollFixtureOwner gives a fixture an active contributor of workspace and
// returns its user ID. Backends with identity provisioning get a provisioned
// account; the plain memory store gets its membership projection seeded.
func enrollFixtureOwner(ctx context.Context, st store.Store, workspace, name string) (string, error) {
	provisioner, ok := st.(fixtureMemberProvisioner)
	if !ok {
		owner := "owner-" + name
		return owner, store.SetMemoryWorkspaceMember(st, workspace, owner, true)
	}
	local := strings.Trim(fixtureEmailUnsafe.ReplaceAllString(strings.ToLower(workspace+"-"+name), "-"), "-")
	email := local + "@fixture-member.test"
	user, err := provisioner.ProvisionIdentityUser(ctx, email, "Fixture member "+name)
	if err != nil {
		return "", err
	}
	operatorCtx := store.WithCredential(ctx, core.AuthenticatedCredential{ID: "fixture-operator", OwnerUserID: user.ID, Kind: core.CredentialUser, Scope: core.CredentialScopeOperator})
	if _, err := provisioner.GrantWorkspaceRole(operatorCtx, email, workspace, core.WorkspaceRoleContributor); err != nil {
		return "", err
	}
	return user.ID, nil
}
