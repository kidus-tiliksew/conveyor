package storetest

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
	"github.com/kidus-tiliksew/conveyor/internal/verification"
)

// runVerifyReviewRoundAdmission pins one review-round admission predicate on
// every backend: with verify_stage on, round creation needs a completed verify
// order at the verify head that carries its sealed verification context, as
// core.VerifyReviewReady states. A refusal writes no job, order, or event
// (req-verification-kits REQ-4/AC-4.3; component-verification-service).
func runVerifyReviewRoundAdmission(t *testing.T, x Fixture) {
	st, ctx := x.Backend, x.Context
	type state struct{ events, jobs, orders int }
	observe := func(t *testing.T, taskID string) state {
		t.Helper()
		events, err := st.ListEvents(ctx, taskID)
		requireOK(t, err)
		jobs, err := st.ListJobs(ctx, taskID)
		requireOK(t, err)
		orders, err := st.ListTaskWorkOrders(ctx, taskID)
		requireOK(t, err)
		return state{len(events), len(jobs), len(orders)}
	}
	refused := func(t *testing.T, taskID string) {
		t.Helper()
		before := observe(t, taskID)
		jobs, orders := reviewRound(taskID, 1)
		err := CreateReviewRound(ctx, st, taskID, jobs, orders)
		if err == nil || !strings.Contains(err.Error(), "review requires completed verification") {
			t.Fatalf("review round admitted without sealed verification: %v", err)
		}
		if after := observe(t, taskID); after != before {
			t.Fatalf("refused round wrote state: before=%+v after=%+v", before, after)
		}
	}
	t.Run("CompletedWithoutContextRefused", func(t *testing.T) {
		order := newAggregateOrder(t, x, core.StageVerify)
		claimed, err := ClaimWorkOrder(ctx, st, order.ID, core.WorkOrderClaim{WorkerID: "worker", ClaimantID: "worker", SessionID: "unsealed", ClientToken: "unsealed", Lease: time.Minute, ExecutionTimeout: time.Hour})
		requireOK(t, err)
		claimed.State = core.WorkOrderCompleted
		requireOK(t, UpdateWorkOrder(ctx, st, claimed, core.WorkOrderCmdSubmitVerification))
		completed, err := st.GetWorkOrder(ctx, order.ID)
		requireOK(t, err)
		if completed.State != core.WorkOrderCompleted || completed.HeadSHA == "" || completed.VerificationContextID != "" {
			t.Fatalf("fixture is not a completed unsealed verify order at the head: %+v", completed)
		}
		refused(t, order.TaskID)
	})
	t.Run("SealedAdmittedAndReplayed", func(t *testing.T) {
		v := sealedVerifyFixture(t, x)
		jobs, orders := reviewRound(v.access.TaskID, 1)
		forgeReviewContext(orders)
		requireOK(t, CreateReviewRound(ctx, st, v.access.TaskID, jobs, orders))
		requireReviewContext(t, x, orders, v.contextID)
		before := observe(t, v.access.TaskID)
		requireOK(t, CreateReviewRound(ctx, st, v.access.TaskID, jobs, orders))
		if after := observe(t, v.access.TaskID); after != before {
			t.Fatalf("round replay wrote state: before=%+v after=%+v", before, after)
		}
		requireReviewContext(t, x, orders, v.contextID)
	})
	t.Run("AddedSeatNamesContext", func(t *testing.T) {
		v := sealedVerifyFixture(t, x)
		jobs, orders := reviewRound(v.access.TaskID, 1)
		requireOK(t, CreateReviewRound(ctx, st, v.access.TaskID, jobs, orders))
		result, err := rs5Add(x, rs5Request(v.access.TaskID, "verify-context-seat", 1))
		requireOK(t, err)
		if result.SeatCount != 3 {
			t.Fatalf("seat addition result %+v", result)
		}
		requireReviewContext(t, x, []core.WorkOrder{{ID: v.access.TaskID + "-review-1-seat-3"}}, v.contextID)
	})
	t.Run("RetriedAndRecoveredSeatsKeepContext", func(t *testing.T) {
		for _, interrupted := range []bool{false, true} {
			v := sealedVerifyFixture(t, x)
			taskID := v.access.TaskID
			jobs, orders := reviewRound(taskID, 1)
			requireOK(t, CreateReviewRound(ctx, st, taskID, jobs, orders))
			first, err := ClaimWorkOrder(ctx, st, orders[0].ID, core.WorkOrderClaim{SessionID: "seat-one", ClientToken: "one", Lease: time.Minute, ExecutionTimeout: time.Hour})
			requireOK(t, err)
			first.State = core.WorkOrderCompleted
			requireOK(t, UpdateWorkOrder(ctx, st, first, core.WorkOrderCmdSubmitReviewVerdict))
			claim := core.WorkOrderClaim{SessionID: "seat-two", ClientToken: "two", Lease: time.Minute, ExecutionTimeout: time.Nanosecond}
			if interrupted {
				claim.Lease, claim.ExecutionTimeout = time.Nanosecond, time.Hour
			}
			_, err = ClaimWorkOrder(ctx, st, orders[1].ID, claim)
			requireOK(t, err)
			_, err = taskops.New(st).TickOrderClock(ctx, time.Now().UTC())
			requireOK(t, err)
			if interrupted {
				_, err = RecoverInterruptedReviewRound(ctx, st, store.InterruptedReviewRecoveryRequest{TaskID: taskID, RequestID: "recover-context", Round: 1}, time.Hour)
				requireOK(t, err)
				requireReviewContext(t, x, orders, v.contextID)
				continue
			}
			jobs2, orders2 := reviewRound(taskID, 2)
			forgeReviewContext(orders2)
			_, err = RetryReviewRound(ctx, st, store.ReviewRoundRetryRequest{TaskID: taskID, RequestID: "retry-context", PriorRound: 1, Reason: "seat timed out", PRHead: v.revisions[0].SHA}, jobs2, orders2)
			requireOK(t, err)
			requireReviewContext(t, x, orders2, v.contextID)
		}
	})
	t.Run("ResealAtNewHeadMovesContext", func(t *testing.T) {
		v := sealedVerifyFixture(t, x)
		jobs, orders := reviewRound(v.access.TaskID, 1)
		requireOK(t, CreateReviewRound(ctx, st, v.access.TaskID, jobs, orders))
		next := resealAtHead(t, x, v, strings.Repeat("d", 40))
		if next.contextID == v.contextID {
			t.Fatal("reseal reused the earlier context")
		}
		jobs2, orders2 := reviewRound(v.access.TaskID, 2)
		task, err := st.GetTask(ctx, v.access.TaskID)
		requireOK(t, err)
		for i := range orders2 {
			orders2[i].HeadSHA, orders2[i].BaselineSHA, orders2[i].ReviewScope = task.RefreshHeadSHA, task.RefreshBaselineSHA, task.RefreshReviewScope
		}
		requireOK(t, CreateReviewRound(ctx, st, v.access.TaskID, jobs2, orders2))
		requireReviewContext(t, x, orders2, next.contextID)
		requireReviewContext(t, x, orders, v.contextID)
	})
	t.Run("SealedAtAnotherHeadRefused", func(t *testing.T) {
		v := sealedVerifyFixture(t, x)
		changed, err := st.MarkTaskApprovalStale(ctx, v.access.TaskID, v.revisions[0].SHA, strings.Repeat("c", 40), "full", "source changed after verification")
		requireOK(t, err)
		if !changed {
			t.Fatal("head advance did not move the verify head")
		}
		refused(t, v.access.TaskID)
	})
	t.Run("PolicyOffAdmitted", func(t *testing.T) {
		task := newAggregateTask(t, x)
		jobs, orders := reviewRound(task.ID, 1)
		forgeReviewContext(orders)
		requireOK(t, CreateReviewRound(ctx, st, task.ID, jobs, orders))
		requireReviewContext(t, x, orders, "")
	})
}

// forgeReviewContext sets a caller-supplied context ID that every backend
// must replace with the one it derives inside the round transaction.
func forgeReviewContext(orders []core.WorkOrder) {
	for i := range orders {
		orders[i].VerificationContextID = "forged-context"
	}
}

// requireReviewContext reads each review order back and requires it to name
// the sealed context (component-verification-service; req-verification-kits
// AC-4.2, AC-8.3).
func requireReviewContext(t *testing.T, x Fixture, orders []core.WorkOrder, want string) {
	t.Helper()
	for _, order := range orders {
		stored, err := x.Backend.GetWorkOrder(x.Context, order.ID)
		requireOK(t, err)
		if stored.Stage != core.StageReview || stored.VerificationContextID != want {
			t.Fatalf("review order %s names context %q, want %q", order.ID, stored.VerificationContextID, want)
		}
	}
}

// resealAtHead moves the verify head as a refresh does, routes the task back
// to verify, and seals a new no-kit context there through a second verify
// order. It returns that order's fixture.
func resealAtHead(t *testing.T, x Fixture, v verificationFixture, head string) verificationFixture {
	t.Helper()
	st, ctx := x.Backend, x.Context
	changed, err := st.MarkTaskApprovalStale(ctx, v.access.TaskID, v.revisions[0].SHA, head, "full", "source changed after review")
	requireOK(t, err)
	if !changed {
		t.Fatal("head advance did not move the verify head")
	}
	// Without a sealed context at the new head, routing to review lands on verify.
	if task, err := st.GetTask(ctx, v.access.TaskID); err == nil && task.State == core.TaskQueued {
		_, err = taskops.New(st).Perform(ctx, v.access.TaskID, taskops.Command{Kind: core.TaskDispatchStart})
		requireOK(t, err)
	}
	_, err = taskops.New(st).Perform(ctx, v.access.TaskID, taskops.Command{Kind: core.TaskStageAdvance, NextStage: core.StageReview, ProjectStages: true})
	requireOK(t, err)
	task, err := st.GetTask(ctx, v.access.TaskID)
	requireOK(t, err)
	if task.NextStage != core.StageVerify || core.VerifyStageHead(task) != head {
		t.Fatalf("task did not return to verify at the new head: next=%s head=%s", task.NextStage, core.VerifyStageHead(task))
	}
	job := core.Job{ID: task.ID + "-verify-2", TaskID: task.ID, Stage: core.StageVerify, State: core.JobPending}
	o := core.WorkOrder{ID: job.ID, JobID: job.ID, TaskID: task.ID, Stage: core.StageVerify, State: core.WorkOrderQueued, HeadSHA: head, ReviewScope: task.RefreshReviewScope, BaselineSHA: task.RefreshBaselineSHA, QueueEnteredAt: time.Now().UTC(), QueueDeadline: time.Now().UTC().Add(time.Hour), CreatedAt: time.Now().UTC()}
	_, err = CreateStageWorkOrder(ctx, st, job, o)
	requireOK(t, err)
	claim := core.WorkOrderClaim{WorkerID: "worker", ClaimantID: "worker", SessionID: "reseal", ClientToken: "reseal-token", Lease: time.Hour, ExecutionTimeout: time.Hour}
	claim.Requirements = []core.ServedRequirementContext{{ID: "req-fixture", Version: 1, Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Observe the state", AcceptanceCriteria: []core.AcceptanceCriterion{{ID: "AC-1.1", Statement: "Retain the observation"}}}}}}
	o, err = ClaimWorkOrder(ctx, st, o.ID, claim)
	requireOK(t, err)
	next := v
	next.access = store.VerificationAccess{TaskID: o.TaskID, WorkOrderID: o.ID, WorkOrderAttemptID: o.AttemptID, ClientToken: claim.ClientToken, Claim: core.WorkOrderClaimIdentity{WorkerID: claim.WorkerID, ClaimantID: claim.ClaimantID, SessionID: claim.SessionID}}
	next.revisions = []core.VerificationRevision{{Repository: v.revisions[0].Repository, RemoteIdentity: v.revisions[0].RemoteIdentity, SHA: head}}
	next.contextID, next.runID = "", ""
	r := next.apply(t, store.VerificationCommand{Kind: store.VerificationCreateContext, Key: "reseal", Context: &store.VerificationContext{ReviewScope: o.ReviewScope, BaselineSHA: o.BaselineSHA, Revisions: next.revisions, GoverningPins: next.pins, Discovery: []json.RawMessage{core.JSONPayload(map[string]any{"repository": next.revisions[0].Repository, "revision": head, "state": "no_manifest"})}}})
	next.contextID = r.ID
	next.apply(t, store.VerificationCommand{Kind: store.VerificationRecordSelection, Selection: &store.VerificationSelection{Receipt: verification.SelectionReceipt{SchemaVersion: 1, Stage: "verify", ContextPins: []verification.Pin{{Kind: "requirement", DocumentID: "req-fixture", Version: 1}}, Kits: []verification.KitReceipt{}}, Subjects: []store.VerificationSubjectContract{}}})
	next.apply(t, store.VerificationCommand{Kind: store.VerificationSeal, Submission: &store.VerificationSubmission{Outcome: "succeeded", Coverage: verificationFixtureCoverage(next.snapshot(t))}})
	sealed, err := st.GetWorkOrder(ctx, o.ID)
	requireOK(t, err)
	if sealed.State != core.WorkOrderCompleted || sealed.VerificationContextID != next.contextID {
		t.Fatalf("reseal did not complete the second verify order with its context: %+v", sealed)
	}
	return next
}

// sealedVerifyFixture seals a no-kit context with outcome succeeded, which is
// the only path that sets the verify order's verification_context_id.
func sealedVerifyFixture(t *testing.T, x Fixture) verificationFixture {
	t.Helper()
	v := newVerificationFixture(t, x, true)
	v.contextID, v.runID = "", ""
	r := v.apply(t, store.VerificationCommand{Kind: store.VerificationCreateContext, Key: "review-admission", Context: &store.VerificationContext{Revisions: v.revisions, GoverningPins: v.pins, Discovery: []json.RawMessage{core.JSONPayload(map[string]any{"repository": "conveyor", "revision": v.revisions[0].SHA, "state": "no_manifest"})}}})
	v.contextID = r.ID
	v.apply(t, store.VerificationCommand{Kind: store.VerificationRecordSelection, Selection: &store.VerificationSelection{Receipt: verification.SelectionReceipt{SchemaVersion: 1, Stage: "verify", ContextPins: []verification.Pin{{Kind: "requirement", DocumentID: "req-fixture", Version: 1}}, Kits: []verification.KitReceipt{}}, Subjects: []store.VerificationSubjectContract{}}})
	v.apply(t, store.VerificationCommand{Kind: store.VerificationSeal, Submission: &store.VerificationSubmission{Outcome: "succeeded", Coverage: verificationFixtureCoverage(v.snapshot(t))}})
	order, err := x.Backend.GetWorkOrder(x.Context, v.access.WorkOrderID)
	requireOK(t, err)
	if order.State != core.WorkOrderCompleted || order.VerificationContextID != v.contextID {
		t.Fatalf("seal did not complete the verify order with its context: %+v", order)
	}
	return v
}
