package storetest

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
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
		requireOK(t, CreateReviewRound(ctx, st, v.access.TaskID, jobs, orders))
		before := observe(t, v.access.TaskID)
		requireOK(t, CreateReviewRound(ctx, st, v.access.TaskID, jobs, orders))
		if after := observe(t, v.access.TaskID); after != before {
			t.Fatalf("round replay wrote state: before=%+v after=%+v", before, after)
		}
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
		requireOK(t, CreateReviewRound(ctx, st, task.ID, jobs, orders))
	})
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
