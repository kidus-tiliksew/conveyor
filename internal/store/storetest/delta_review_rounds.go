package storetest

// Delta review round conformance (feature-delta-review-rounds DR-2 to DR-5;
// req-review-gates-evidence AC-12.2 to AC-12.6). Every case runs unchanged on
// memory, PostgreSQL, and SingleStore: the judged head and the recorded scope
// are read back from each backend's own events and orders, and every command
// that opens a verify order or a review round for a delta head persists the
// same scope and baseline.

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

const (
	drJudged = "judged-head"
	drHead   = seatFixtureHead
	drBase   = seatFixtureBase
)

func runDeltaReviewRounds(t *testing.T, x Fixture) {
	t.Run("DeltaAfterReviewerBounce", func(t *testing.T) { runDeltaAfterReviewerBounce(t, x) })
	t.Run("DeltaAfterMergeGateRequestChanges", func(t *testing.T) { runDeltaAfterMergeGateRequest(t, x) })
	t.Run("FailFastWithoutCancellationJudgesHead", func(t *testing.T) { runDeltaFailFastJudged(t, x) })
	t.Run("FullFallback", func(t *testing.T) { runDeltaFullFallback(t, x) })
	t.Run("VerifyAndReviewHoldOnePair", func(t *testing.T) { runDeltaVerifyReviewPair(t, x) })
	t.Run("RoundRetryKeepsDelta", func(t *testing.T) { runDeltaRoundRetry(t, x) })
	t.Run("SeatAdditionKeepsDelta", func(t *testing.T) { runDeltaSeatAddition(t, x) })
	t.Run("PolicyHandoffDuringDeltaRound", func(t *testing.T) { runDeltaPolicyHandoff(t, x) })
	t.Run("PolicyHandoffRefusesForeignBaseline", func(t *testing.T) { runDeltaPolicyHandoffMismatch(t, x) })
	t.Run("StaleApprovalRefreshUnchanged", func(t *testing.T) { runDeltaStaleRefresh(t, x) })
}

// drTask is a running review task whose frozen later_round_review is policy.
func drTask(t *testing.T, x Fixture, seats int, policy string, setup func(*core.Task)) core.Task {
	t.Helper()
	task := core.Task{ID: core.NewTaskID(), Workspace: x.Workspace, Repo: "conveyor", BaseBranch: "main", State: core.TaskRunning, NextStage: core.StageReview, ReviewedHeadSHA: drJudged, PolicyVersion: 1, CreatedAt: time.Now().UTC()}
	task.Branch = "conveyor/task-" + task.ID
	task.SetupContract = config.PolicyContract{MaxBounces: 3, Review: config.ReviewPanel{Seats: make([]config.ReviewSeat, seats)}, StageTimeouts: config.PolicyStageTimeouts{Review: "1h"}, LaterRoundReview: policy}
	if setup != nil {
		setup(&task)
	}
	requireOK(t, x.Backend.CreateTask(x.Context, task))
	stored, err := x.Backend.GetTask(x.Context, task.ID)
	requireOK(t, err)
	return stored
}

// drRound opens a round of count seats that review head with the given scope
// and baseline, and returns its orders in seat order.
func drRound(t *testing.T, x Fixture, taskID string, round, count int, head, scope, baseline string) []core.WorkOrder {
	t.Helper()
	seats := resolveSeats(t, x, taskID, round)
	if seats.SeatCount < count {
		seats.SeatCount = count
	}
	jobs, orders := seatMembers(taskID, round, seats.SeatCount)
	for i := range orders {
		orders[i].HeadSHA, orders[i].ReviewScope, orders[i].BaselineSHA = head, scope, baseline
	}
	requireOK(t, createSeatRound(x, taskID, seats, jobs, orders))
	return orders
}

// drVerdict claims a seat and submits a verdict on commit.
func drVerdict(t *testing.T, x Fixture, order core.WorkOrder, verdict, commit string, gate bool) error {
	t.Helper()
	session := ffClaim(t, x, order)
	decision := core.ReviewDecision{TaskID: order.TaskID, JobID: order.JobID, ReviewWorkOrderID: order.ID, ReviewRound: order.ReviewRound, ReviewSeat: order.ReviewSeat,
		ClaimSession: session, Verdict: verdict, ReasonCode: "verified", Summary: fmt.Sprintf("seat %d", order.ReviewSeat),
		HeadSHA: order.HeadSHA, ReviewedCommitSHA: commit, ReviewScope: order.ReviewScope, BaselineSHA: order.BaselineSHA, PolicyVersion: 1, MergeApproval: gate, MaxBounces: 3}
	if verdict == "changes_requested" {
		decision.ReasonCode, decision.Feedback = "defect", fmt.Sprintf("seat %d defect", order.ReviewSeat)
	}
	return taskops.New(x.Backend).AcceptReviewDecision(x.Context, decision)
}

// drJudgedHead reads the judged head from the backend's own events and orders.
func drJudgedHead(t *testing.T, x Fixture, taskID string) (string, bool) {
	t.Helper()
	events, err := x.Backend.ListCompactEvents(x.Context, taskID)
	requireOK(t, err)
	orders, err := x.Backend.ListWorkOrdersForTasks(x.Context, []string{taskID})
	requireOK(t, err)
	return store.JudgedHead(events, orders)
}

// drRecord records a submission comparison for head the way submission does.
func drRecord(t *testing.T, x Fixture, taskID, base, head, scope, baseline string) {
	t.Helper()
	payload := map[string]any{"base_sha": base, "head_sha": head, "url": "https://github.com/acme/conveyor/pull/1", "number": 1}
	if scope != "" {
		payload["review_scope"], payload["review_baseline_sha"] = scope, baseline
	}
	requireOK(t, x.Backend.AppendEvent(x.Context, core.Event{TaskID: taskID, Kind: store.SubmissionComparisonEventKind, Payload: core.JSONPayload(payload)}))
}

func drDecide(task core.Task, judged string, ancestry store.SubmissionAncestry, ancestryErr error) (string, string) {
	return store.DecideSubmissionReviewScope(store.SubmissionReviewScopeInput{
		LaterRoundReview: task.SetupContract.LaterRoundReviewScope(), JudgedHead: judged, SubmittedHead: drHead, BaseSHA: drBase,
		Ancestry: func() (store.SubmissionAncestry, error) { return ancestry, ancestryErr },
	})
}

var drAhead = store.SubmissionAncestry{Status: "ahead"}

func drOrders(t *testing.T, x Fixture, ids []string) []core.WorkOrder {
	t.Helper()
	orders := make([]core.WorkOrder, 0, len(ids))
	for _, id := range ids {
		order, err := x.Backend.GetWorkOrder(x.Context, id)
		requireOK(t, err)
		orders = append(orders, order)
	}
	return orders
}

func drRequireOrders(t *testing.T, label string, orders []core.WorkOrder, scope, baseline, head string) {
	t.Helper()
	if len(orders) == 0 {
		t.Fatalf("%s: no orders", label)
	}
	for _, order := range orders {
		if order.ReviewScope != scope || order.BaselineSHA != baseline || order.HeadSHA != head || order.ReviewKind != "" {
			t.Fatalf("%s: order %s scope=%q baseline=%q head=%q kind=%q, want scope=%q baseline=%q head=%q and no kind", label, order.ID, order.ReviewScope, order.BaselineSHA, order.HeadSHA, order.ReviewKind, scope, baseline, head)
		}
	}
}

// A reviewer's bounce leaves the round's commit as the judged head. The next
// head's submission fixes delta from it, the first recorded scope of the head
// stands over a later record, and the next round's seats persist the scope and
// the judged head with an empty review kind (AC-12.2 to AC-12.4).
func runDeltaAfterReviewerBounce(t *testing.T, x Fixture) {
	task := drTask(t, x, 2, config.LaterRoundReviewDelta, nil)
	round := drRound(t, x, task.ID, 1, 2, drJudged, "", drBase)
	requireOK(t, drVerdict(t, x, round[0], "approve", drJudged, false))
	requireOK(t, drVerdict(t, x, round[1], "changes_requested", drJudged, false))
	if bounced, err := x.Backend.GetTask(x.Context, task.ID); err != nil || bounced.NextStage != core.StageImplement {
		t.Fatalf("bounce next=%s err=%v", bounced.NextStage, err)
	}
	judged, ok := drJudgedHead(t, x, task.ID)
	if !ok || judged != drJudged {
		t.Fatalf("judged head=%q ok=%t, want %q", judged, ok, drJudged)
	}
	scope, baseline := drDecide(task, judged, drAhead, nil)
	if scope != store.ReviewScopeDelta || baseline != drJudged {
		t.Fatalf("scope=%q baseline=%q, want delta from the judged head", scope, baseline)
	}
	drRecord(t, x, task.ID, drBase, drHead, scope, baseline)
	// A later record of the same head never moves the scope fixed first.
	drRecord(t, x, task.ID, drBase, drHead, store.ReviewScopeFull, drBase)
	events, err := x.Backend.ListCompactEvents(x.Context, task.ID)
	requireOK(t, err)
	fixed, ok := store.FixedHeadComparison(events, drHead)
	if !ok || !fixed.Delta() || fixed.ReviewBaselineSHA != drJudged || fixed.BaseSHA != drBase {
		t.Fatalf("fixed comparison=%+v ok=%t", fixed, ok)
	}
	// The whole-change pair stays the observed base and the head (DR-5).
	if base, head, ok := store.RecordedSubmissionComparison(events); !ok || base != drBase || head != drHead {
		t.Fatalf("whole-change pair=(%q,%q,%t)", base, head, ok)
	}
	// The round reads the latest record, which this case wrote as full; a
	// submission writes its fixed scope on every record, so read the first.
	events = events[:len(events)-1]
	current, err := x.Backend.GetTask(x.Context, task.ID)
	requireOK(t, err)
	baseline, head, roundScope, recorded := store.ReviewRoundDispatchComparison(current, events)
	if !recorded || roundScope != store.ReviewScopeDelta || baseline != drJudged || head != drHead {
		t.Fatalf("dispatch comparison=(%q,%q,%q,%t)", baseline, head, roundScope, recorded)
	}
	next := drRound(t, x, task.ID, 2, 2, head, roundScope, baseline)
	drRequireOrders(t, "created round", next, store.ReviewScopeDelta, drJudged, drHead)
	drRequireOrders(t, "stored round", reviewOrdersInRound(t, x, task.ID, 2), store.ReviewScopeDelta, drJudged, drHead)
	// The delta round's verdicts carry the same fields as a full verdict and
	// settle the round as AC-2.1 states (AC-12.5).
}

// An operator's request for changes at the merge gate leaves the approved
// round's commit as the judged head (AC-12.2, AC-12.3).
func runDeltaAfterMergeGateRequest(t *testing.T, x Fixture) {
	task := drTask(t, x, 2, config.LaterRoundReviewDelta, func(task *core.Task) { task.MergeApproval, task.Level = true, core.L2 })
	round := drRound(t, x, task.ID, 1, 2, drJudged, "", drBase)
	for _, order := range round {
		requireOK(t, drVerdict(t, x, order, "approve", drJudged, true))
	}
	gated, err := x.Backend.GetTask(x.Context, task.ID)
	requireOK(t, err)
	if gated.State != core.TaskAwaiting || gated.GateKind != core.GateKindMergeApproval {
		t.Fatalf("gate state=%s kind=%s", gated.State, gated.GateKind)
	}
	user := store.WithActor(x.Context, store.Actor{ID: "operator-delta", Role: core.ActorUser})
	if _, err = taskops.New(x.Backend).RequestChanges(user, taskops.RequestChanges{TaskID: task.ID, Feedback: "tighten the error text", MaxBounces: 3}); err != nil {
		t.Fatal(err)
	}
	judged, ok := drJudgedHead(t, x, task.ID)
	if !ok || judged != drJudged {
		t.Fatalf("judged head after request changes=%q ok=%t", judged, ok)
	}
	if scope, baseline := drDecide(gated, judged, drAhead, nil); scope != store.ReviewScopeDelta || baseline != drJudged {
		t.Fatalf("scope=%q baseline=%q", scope, baseline)
	}
}

// A fail-fast round whose last seat requested changes cancels no seat, so its
// commit is the judged head (DR-2; AC-12.2).
func runDeltaFailFastJudged(t *testing.T, x Fixture) {
	task := drTask(t, x, 1, config.LaterRoundReviewDelta, func(task *core.Task) { task.SetupContract.Review.FailFast = true })
	round := drRound(t, x, task.ID, 1, 1, drJudged, "", drBase)
	requireOK(t, drVerdict(t, x, round[0], "changes_requested", drJudged, false))
	if judged, ok := drJudgedHead(t, x, task.ID); !ok || judged != drJudged {
		t.Fatalf("a fail-fast round that cancelled nothing gave judged=%q ok=%t", judged, ok)
	}
}

// Every case outside delta gives full from the observed base commit: a round
// that cancelled a seat, a round whose seats reviewed different commits, a
// frozen full policy, an equal head, and a comparison that fails or reports
// any status other than ahead with nothing behind (AC-12.2, AC-12.3).
func runDeltaFullFallback(t *testing.T, x Fixture) {
	full := func(t *testing.T, label string, scope, baseline string) {
		t.Helper()
		if scope != store.ReviewScopeFull || baseline != drBase {
			t.Fatalf("%s: scope=%q baseline=%q, want full from the base", label, scope, baseline)
		}
	}
	t.Run("CancelledSeat", func(t *testing.T) {
		task := drTask(t, x, 3, config.LaterRoundReviewDelta, func(task *core.Task) { task.SetupContract.Review.FailFast = true })
		round := drRound(t, x, task.ID, 1, 3, drJudged, "", drBase)
		requireOK(t, drVerdict(t, x, round[0], "changes_requested", drJudged, false))
		settlement, count := ffRoundCompleted(t, x, task.ID)
		if count != 1 || len(settlement.CancelledSeats) == 0 {
			t.Fatalf("fail-fast settlement=%+v count=%d", settlement, count)
		}
		judged, ok := drJudgedHead(t, x, task.ID)
		if ok {
			t.Fatalf("a round with a cancelled seat gave judged head %q", judged)
		}
		scope, baseline := drDecide(task, judged, drAhead, nil)
		full(t, "cancelled seat", scope, baseline)
	})
	t.Run("HeadMismatch", func(t *testing.T) {
		task := drTask(t, x, 2, config.LaterRoundReviewDelta, nil)
		round := drRound(t, x, task.ID, 1, 2, drJudged, "", drBase)
		requireOK(t, drVerdict(t, x, round[0], "approve", drJudged, false))
		requireOK(t, drVerdict(t, x, round[1], "approve", "another-head", false))
		judged, ok := drJudgedHead(t, x, task.ID)
		if ok {
			t.Fatalf("a head-mismatch round gave judged head %q", judged)
		}
		scope, baseline := drDecide(task, judged, drAhead, nil)
		full(t, "head mismatch", scope, baseline)
	})
	t.Run("PolicyAndAncestry", func(t *testing.T) {
		delta := drTask(t, x, 1, config.LaterRoundReviewDelta, nil)
		round := drRound(t, x, delta.ID, 1, 1, drJudged, "", drBase)
		requireOK(t, drVerdict(t, x, round[0], "changes_requested", drJudged, false))
		judged, ok := drJudgedHead(t, x, delta.ID)
		if !ok {
			t.Fatal("complete round gave no judged head")
		}
		for _, policy := range []string{"", config.LaterRoundReviewFull} {
			task := drTask(t, x, 1, policy, nil)
			scope, baseline := drDecide(task, judged, drAhead, nil)
			full(t, "policy "+policy, scope, baseline)
		}
		for _, tc := range []struct {
			name     string
			ancestry store.SubmissionAncestry
			err      error
		}{
			{"Behind", store.SubmissionAncestry{Status: "behind", BehindBy: 1}, nil},
			{"Diverged", store.SubmissionAncestry{Status: "diverged", BehindBy: 2}, nil},
			{"Identical", store.SubmissionAncestry{Status: "identical"}, nil},
			{"AheadButBehind", store.SubmissionAncestry{Status: "ahead", BehindBy: 1}, nil},
			{"ReadFailed", store.SubmissionAncestry{}, errors.New("compare failed")},
		} {
			scope, baseline := drDecide(delta, judged, tc.ancestry, tc.err)
			full(t, tc.name, scope, baseline)
		}
		equal, _ := store.DecideSubmissionReviewScope(store.SubmissionReviewScopeInput{LaterRoundReview: config.LaterRoundReviewDelta, JudgedHead: drHead, SubmittedHead: drHead, BaseSHA: drBase,
			Ancestry: func() (store.SubmissionAncestry, error) { return drAhead, nil }})
		if equal != store.ReviewScopeFull {
			t.Fatalf("an equal head gave %q", equal)
		}
	})
}

// A delta head with verify_stage on binds one scope and baseline in its verify
// context and its review seats: the context seals, the round opens, and a
// verdict bound to the pair is accepted, while a verdict that names the full
// pair is refused (component-verification-service invariants 11 and 48;
// AC-12.4, AC-12.5).
func runDeltaVerifyReviewPair(t *testing.T, x Fixture) {
	v := newVerificationFixtureBound(t, x, []core.VerificationRevision{{Repository: "conveyor", RemoteIdentity: "https://example.test/conveyor", SHA: strings.Repeat("d", 40)}},
		func(task *core.Task) { task.SetupContract.LaterRoundReview = config.LaterRoundReviewDelta }, 1, store.ReviewScopeDelta, drJudged, true)
	coverage := verificationFixtureCoverage(v.snapshot(t))
	v.apply(t, store.VerificationCommand{Kind: store.VerificationSeal, Submission: &store.VerificationSubmission{Outcome: "succeeded", Coverage: coverage}})
	verify, err := x.Backend.GetWorkOrder(v.ctx, v.access.WorkOrderID)
	requireOK(t, err)
	if verify.ReviewScope != store.ReviewScopeDelta || verify.BaselineSHA != drJudged {
		t.Fatalf("verify order scope=%q baseline=%q", verify.ReviewScope, verify.BaselineSHA)
	}
	job := core.Job{ID: verify.TaskID + "-review-1-seat-1", TaskID: verify.TaskID, Stage: core.StageReview, State: core.JobPending}
	order := core.WorkOrder{ID: job.ID, JobID: job.ID, TaskID: job.TaskID, Stage: core.StageReview, State: core.WorkOrderQueued, HeadSHA: verify.HeadSHA, ReviewScope: store.ReviewScopeDelta, BaselineSHA: drJudged, ReviewRound: 1, ReviewSeat: 1, CreatedAt: time.Now().UTC()}
	requireOK(t, CreateReviewRound(x.Context, x.Backend, order.TaskID, []core.Job{job}, []core.WorkOrder{order}))
	order, err = ClaimWorkOrder(x.Context, x.Backend, order.ID, core.WorkOrderClaim{ClaimantID: core.TaskRunClaimantID("reviewer"), SessionID: "delta-reviewer", ClientToken: "delta-review-token", Lease: time.Hour, ExecutionTimeout: time.Hour, Requirements: verify.ServedRequirementSnapshot})
	requireOK(t, err)
	if order.ReviewScope != store.ReviewScopeDelta || order.BaselineSHA != drJudged || order.VerificationContextID != v.contextID {
		t.Fatalf("review seat scope=%q baseline=%q context=%q, want delta from %q in %q", order.ReviewScope, order.BaselineSHA, order.VerificationContextID, drJudged, v.contextID)
	}
	ctx := store.WithActor(x.Context, store.Actor{ID: store.UserActorID("reviewer"), Role: core.ActorUser})
	decision := core.ReviewDecision{TaskID: order.TaskID, JobID: order.JobID, ReviewWorkOrderID: order.ID, ClaimSession: "delta-reviewer", ReviewRound: 1, ReviewSeat: 1, Verdict: "approve", ReasonCode: "verified", Summary: "Delta seat assessed",
		ReviewedCommitSHA: verify.HeadSHA, HeadSHA: verify.HeadSHA, ReviewScope: store.ReviewScopeDelta, BaselineSHA: drJudged, MaxBounces: 3,
		VerificationAssessment: &core.VerificationAssessment{ContextIDs: []string{v.contextID}, RunIDs: []string{}, EvidenceIDs: []string{}, Mappings: []core.VerificationAssessmentMapping{{DocumentID: "req-fixture", Version: 1, AcceptanceCriterionID: "AC-1.1", EvidenceIDs: []string{}, Assessment: "The delta context was sealed for this head"}}}}
	fullPair := decision
	fullPair.ReviewScope, fullPair.BaselineSHA = "", ""
	if err = taskops.New(x.Backend).AcceptReviewDecision(ctx, fullPair); err == nil {
		t.Fatal("a verdict naming the full pair was accepted against a delta context")
	}
	requireOK(t, taskops.New(x.Backend).AcceptReviewDecision(ctx, decision))
	settlement, count := ffRoundCompleted(t, x, order.TaskID)
	if count != 1 || settlement.Verdict != "approve" {
		t.Fatalf("delta round settlement=%+v count=%d", settlement, count)
	}
}

// A retried round of a delta head persists the delta scope, the judged head,
// and the head on every new seat (DR-4; AC-12.4).
func runDeltaRoundRetry(t *testing.T, x Fixture) {
	task := drTask(t, x, 2, config.LaterRoundReviewDelta, nil)
	orders := drRound(t, x, task.ID, 1, 2, drHead, store.ReviewScopeDelta, drJudged)
	first, err := ClaimWorkOrder(x.Context, x.Backend, orders[0].ID, core.WorkOrderClaim{SessionID: "delta-seat-one-" + task.ID, ClientToken: "one", Lease: time.Minute, ExecutionTimeout: time.Hour})
	requireOK(t, err)
	first.State = core.WorkOrderCompleted
	requireOK(t, UpdateWorkOrder(x.Context, x.Backend, first, core.WorkOrderCmdSubmitReviewVerdict))
	_, err = ClaimWorkOrder(x.Context, x.Backend, orders[1].ID, core.WorkOrderClaim{SessionID: "delta-seat-two-" + task.ID, ClientToken: "timeout", Lease: time.Minute, ExecutionTimeout: time.Nanosecond})
	requireOK(t, err)
	_, err = taskops.New(x.Backend).TickOrderClock(x.Context, time.Now().UTC().Add(time.Second))
	requireOK(t, err)
	seats := resolveSeats(t, x, task.ID, 2)
	jobs, retried := seatMembers(task.ID, 2, seats.SeatCount)
	for i := range retried {
		retried[i].HeadSHA, retried[i].ReviewScope, retried[i].BaselineSHA = drHead, store.ReviewScopeDelta, drJudged
	}
	result, err := retrySeatRound(x, store.ReviewRoundRetryRequest{TaskID: task.ID, RequestID: "delta-retry-" + task.ID, PriorRound: 1, Reason: "seat timed out", PRHead: drHead}, seats, jobs, retried)
	requireOK(t, err)
	drRequireOrders(t, "retry result", result.WorkOrders, store.ReviewScopeDelta, drJudged, drHead)
	drRequireOrders(t, "stored retry", reviewOrdersInRound(t, x, task.ID, 2), store.ReviewScopeDelta, drJudged, drHead)
}

// An operator's added seat joins a delta round with its scope and baseline
// (component-review invariant 8; DR-4).
func runDeltaSeatAddition(t *testing.T, x Fixture) {
	task := drTask(t, x, 1, config.LaterRoundReviewDelta, nil)
	drRound(t, x, task.ID, 1, 1, drHead, store.ReviewScopeDelta, drJudged)
	if _, err := rs5Add(x, rs5Request(task.ID, "delta-add-"+task.ID, 1)); err != nil {
		t.Fatal(err)
	}
	drRequireOrders(t, "added seats", reviewOrdersInRound(t, x, task.ID, 1), store.ReviewScopeDelta, drJudged, drHead)
}

// drHandoffTask is a review task at a delta head: its submission recorded
// delta from the judged head, and its round 1 seats are queued with that pair.
func drHandoffTask(t *testing.T, x Fixture, recordedBaseline string) core.Task {
	t.Helper()
	task := drTask(t, x, 2, config.LaterRoundReviewDelta, func(task *core.Task) { task.ReviewedHeadSHA = drHead })
	drRecord(t, x, task.ID, drBase, drHead, store.ReviewScopeDelta, recordedBaseline)
	drRound(t, x, task.ID, 1, 2, drHead, store.ReviewScopeDelta, drJudged)
	return task
}

// A policy handoff during a delta round inherits the head's recorded scope and
// baseline instead of refusing, in both directions (DR-4; AC-12.4).
func runDeltaPolicyHandoff(t *testing.T, x Fixture) {
	task := drHandoffTask(t, x, drJudged)
	enabled := true
	toVerify, err := changeSeatPolicy(x, store.SetupChangeRequest{TaskID: task.ID, RequestID: "delta-verify-" + task.ID, Reason: "enable verification", Policy: &store.TaskPolicyChange{VerifyStage: &enabled}})
	requireOK(t, err)
	if toVerify.Task.NextStage != core.StageVerify || len(toVerify.CreatedWorkOrders) != 1 || len(toVerify.SupersededWorkOrders) != 2 {
		t.Fatalf("verify handoff=%+v", toVerify)
	}
	drRequireOrders(t, "handoff verify order", drOrders(t, x, toVerify.CreatedWorkOrders), store.ReviewScopeDelta, drJudged, drHead)
	disabled := false
	toReview, err := changeSeatPolicy(x, store.SetupChangeRequest{TaskID: task.ID, RequestID: "delta-review-" + task.ID, Reason: "disable verification", Policy: &store.TaskPolicyChange{VerifyStage: &disabled}})
	requireOK(t, err)
	if toReview.Task.NextStage != core.StageReview || len(toReview.CreatedWorkOrders) == 0 {
		t.Fatalf("review handoff=%+v", toReview)
	}
	drRequireOrders(t, "handoff review round", reviewOrdersInRound(t, x, task.ID, 2), store.ReviewScopeDelta, drJudged, drHead)
}

// A handoff refuses when the current orders' baseline is not the head's
// recorded delta baseline, and changes nothing.
func runDeltaPolicyHandoffMismatch(t *testing.T, x Fixture) {
	task := drHandoffTask(t, x, "another-judged-head")
	before := takeSeatSnapshot(t, x, task.ID)
	enabled := true
	_, err := changeSeatPolicy(x, store.SetupChangeRequest{TaskID: task.ID, RequestID: "delta-mismatch-" + task.ID, Reason: "enable verification", Policy: &store.TaskPolicyChange{VerifyStage: &enabled}})
	if !errors.Is(err, store.ErrSetupChangeConflict) {
		t.Fatalf("mismatched baseline handoff err=%v", err)
	}
	requireSeatSnapshot(t, "refused handoff", takeSeatSnapshot(t, x, task.ID), before)
}

// While the approval is stale, the refresh pair and scope alone decide a
// round, whatever scope a submission recorded, and a policy handoff keeps the
// refresh binding (AC-12.6).
func runDeltaStaleRefresh(t *testing.T, x Fixture) {
	task := drTask(t, x, 1, config.LaterRoundReviewDelta, func(task *core.Task) {
		task.ReviewedHeadSHA, task.ApprovedHeadSHA, task.ApprovalStale = drHead, "approved-head", true
		task.RefreshBaselineSHA, task.RefreshHeadSHA, task.RefreshReviewScope = "approved-head", drHead, config.RefreshReviewFull
	})
	drRecord(t, x, task.ID, drBase, drHead, store.ReviewScopeDelta, drJudged)
	events, err := x.Backend.ListCompactEvents(x.Context, task.ID)
	requireOK(t, err)
	baseline, head, scope, recorded := store.ReviewRoundDispatchComparison(task, events)
	if recorded || baseline != "approved-head" || head != drHead || scope != config.RefreshReviewFull {
		t.Fatalf("stale comparison=(%q,%q,%q,%t), want the refresh pair", baseline, head, scope, recorded)
	}
	jobs, orders := seatMembers(task.ID, 1, 1)
	orders[0].HeadSHA, orders[0].BaselineSHA, orders[0].ReviewScope, orders[0].ReviewKind = drHead, "approved-head", config.RefreshReviewFull, "refresh"
	requireOK(t, createSeatRound(x, task.ID, resolveSeats(t, x, task.ID, 1), jobs, orders))
	enabled := true
	result, err := changeSeatPolicy(x, store.SetupChangeRequest{TaskID: task.ID, RequestID: "delta-stale-" + task.ID, Reason: "enable verification", Policy: &store.TaskPolicyChange{VerifyStage: &enabled}})
	requireOK(t, err)
	created := drOrders(t, x, result.CreatedWorkOrders)
	if len(created) != 1 || created[0].ReviewScope != config.RefreshReviewFull || created[0].BaselineSHA != "approved-head" || created[0].HeadSHA != drHead {
		t.Fatalf("stale handoff=%+v", created)
	}
}
