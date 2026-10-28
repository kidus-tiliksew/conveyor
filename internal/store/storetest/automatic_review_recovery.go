package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// RunAutomaticReviewRecoveryConformance proves the AR-5 review-round rule
// on every backend (feature-bounded-automatic-recovery AC-5.2, AC-5.3): a
// round holding an AR-3-exhausted seat is replaced by one automatic round
// retry while that seat stays unchanged, an automatic in-place recovery
// never re-queues an exhausted seat, and each round gets one automatic act.
func RunAutomaticReviewRecoveryConformance(t *testing.T, st store.Store, ctx context.Context, workspace string, sweep ReviewSweepFunc) {
	restore := setAutomaticRecoveryBound(t, st, ctx, 1)
	defer restore()
	t.Run("ExhaustedSeatRetriedOnce", func(t *testing.T) {
		task, orders := seedReviewRoundWithVerdict(t, st, ctx, workspace)
		seat := orders[1].ID
		before := exhaustReviewSeat(t, st, ctx, seat)
		if decision := decideAutomaticReview(t, st, ctx, task.ID); decision.Kind != store.AutomaticReviewRetry || decision.Round != 1 {
			t.Fatalf("decision with exhausted seat=%+v", decision)
		}
		recover := store.InterruptedReviewRecoveryRequest{TaskID: task.ID, RequestID: store.AutomaticReviewRequestID(task.ID, 1), Round: 1, Automatic: true}
		if _, err := RecoverInterruptedReviewRound(ctx, st, recover, time.Hour); !errors.Is(err, store.ErrReviewRetryConflict) {
			t.Fatalf("automatic recovery of exhausted seat err=%v", err)
		}
		assertSeatUnchanged(t, st, ctx, before)

		jobs2, orders2 := reviewRound(task.ID, 2)
		request := store.ReviewRoundRetryRequest{TaskID: task.ID, RequestID: store.AutomaticReviewRequestID(task.ID, 1), PriorRound: 1, Reason: store.AutomaticReviewReason, PRHead: "fixture-head", Automatic: true}
		result, err := RetryReviewRound(ctx, st, request, jobs2, orders2)
		if err != nil || result.NewRound != 2 || len(result.WorkOrders) != 2 {
			t.Fatalf("automatic retry result=%+v err=%v", result, err)
		}
		if _, err = RetryReviewRound(ctx, st, request, jobs2, orders2); err != nil {
			t.Fatalf("automatic retry replay err=%v", err)
		}
		assertSeatUnchanged(t, st, ctx, before)
		retried := automaticRoundEvents(t, st, ctx, task.ID, "review.round_retried")
		if len(retried) != 1 || !slices.Contains(retried[0].ExhaustedWorkOrderIDs, seat) {
			t.Fatalf("automatic round_retried events=%+v", retried)
		}
		if decision := decideAutomaticReview(t, st, ctx, task.ID); decision.Kind != store.AutomaticReviewNone {
			t.Fatalf("decision after automatic retry=%+v", decision)
		}

		// A need on the round the automatic retry created is left to the
		// operator: the store refuses a second automatic act, and the
		// operator retry still succeeds.
		verdict, err := ClaimWorkOrder(ctx, st, result.WorkOrders[0].ID, core.WorkOrderClaim{WorkerID: "worker", ClaimantID: "worker", SessionID: "verdict-2-" + task.ID, ClientToken: "verdict-2", Lease: time.Hour, ExecutionTimeout: time.Hour})
		requireOK(t, err)
		verdict.State = core.WorkOrderCompleted
		requireOK(t, UpdateWorkOrder(ctx, st, verdict, core.WorkOrderCmdSubmitReviewVerdict))
		exhaustReviewSeat(t, st, ctx, result.WorkOrders[1].ID)
		jobs3, orders3 := reviewRound(task.ID, 3)
		second := store.ReviewRoundRetryRequest{TaskID: task.ID, RequestID: store.AutomaticReviewRequestID(task.ID, 2), PriorRound: 2, Reason: store.AutomaticReviewReason, PRHead: "fixture-head", Automatic: true}
		if _, err = RetryReviewRound(ctx, st, second, jobs3, orders3); !errors.Is(err, store.ErrReviewRetryConflict) {
			t.Fatalf("second automatic retry err=%v", err)
		}
		second.Automatic, second.RequestID, second.Reason = false, "operator-retry-"+task.ID, "operator retry"
		if result, err = RetryReviewRound(ctx, st, second, jobs3, orders3); err != nil || result.NewRound != 3 {
			t.Fatalf("operator retry after the automatic act=%+v err=%v", result, err)
		}
	})
	t.Run("InterruptedRecoveryOncePerRound", func(t *testing.T) {
		task, orders := seedReviewRoundWithVerdict(t, st, ctx, workspace)
		seat := orders[1].ID
		expireReviewSeat(t, st, ctx, seat, "first")
		request := store.InterruptedReviewRecoveryRequest{TaskID: task.ID, RequestID: store.AutomaticReviewRequestID(task.ID, 1), Round: 1, Automatic: true}
		if _, err := RecoverInterruptedReviewRound(ctx, st, request, time.Hour); err != nil {
			t.Fatalf("first automatic recovery err=%v", err)
		}
		if recovered, err := st.GetWorkOrder(ctx, seat); err != nil || recovered.State != core.WorkOrderQueued || recovered.RetrySuppressed {
			t.Fatalf("recovered seat=%+v err=%v", recovered, err)
		}
		expireReviewSeat(t, st, ctx, seat, "second")
		request.RequestID += "-again"
		if _, err := RecoverInterruptedReviewRound(ctx, st, request, time.Hour); !errors.Is(err, store.ErrReviewRetryConflict) {
			t.Fatalf("second automatic recovery of round 1 err=%v", err)
		}
		request.Automatic = false
		request.RequestID = "operator-recover"
		if _, err := RecoverInterruptedReviewRound(ctx, st, request, time.Hour); err != nil {
			t.Fatalf("operator recovery after the automatic act err=%v", err)
		}
		if recovered := automaticRoundEvents(t, st, ctx, task.ID, "review.round_recovered"); len(recovered) != 1 {
			t.Fatalf("automatic round_recovered events=%+v", recovered)
		}
	})
	t.Run("ConcurrentSweepsActOnce", func(t *testing.T) {
		runConcurrentReviewSweeps(t, st, ctx, workspace, sweep)
	})
}

func seedReviewRoundWithVerdict(t *testing.T, st store.Store, ctx context.Context, workspace string) (core.Task, []core.WorkOrder) {
	t.Helper()
	task := core.Task{ID: core.NewTaskID(), Workspace: workspace, Repo: "conveyor", BaseBranch: "main", State: core.TaskRunning, NextStage: core.StageReview, CreatedAt: time.Now().UTC()}
	task.Branch = "conveyor/task-" + task.ID
	requireOK(t, st.CreateTask(ctx, task))
	jobs, orders := reviewRound(task.ID, 1)
	requireOK(t, CreateReviewRound(ctx, st, task.ID, jobs, orders))
	first, err := ClaimWorkOrder(ctx, st, orders[0].ID, core.WorkOrderClaim{WorkerID: "worker", ClaimantID: "worker", SessionID: "verdict-" + task.ID, ClientToken: "verdict", Lease: time.Hour, ExecutionTimeout: time.Hour})
	requireOK(t, err)
	first.State = core.WorkOrderCompleted
	requireOK(t, UpdateWorkOrder(ctx, st, first, core.WorkOrderCmdSubmitReviewVerdict))
	return task, orders
}

// expireReviewSeat loses one claim's lease so the seat is interrupted.
func expireReviewSeat(t *testing.T, st store.Store, ctx context.Context, id, label string) {
	t.Helper()
	if _, err := ClaimWorkOrder(ctx, st, id, core.WorkOrderClaim{WorkerID: "worker", ClaimantID: "worker", SessionID: label + "-" + id, ClientToken: label + "-" + id, Lease: time.Millisecond, ExecutionTimeout: time.Hour}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	if _, err := taskops.New(st).TickOrderClock(ctx, time.Now().UTC().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
}

// exhaustReviewSeat drives the AR-3 clock at bound 1 until the seat is
// queued, unclaimed, and suppressed as exhausted.
func exhaustReviewSeat(t *testing.T, st store.Store, ctx context.Context, id string) core.WorkOrder {
	t.Helper()
	for attempt := range 6 {
		order, err := st.GetWorkOrder(ctx, id)
		requireOK(t, err)
		switch {
		case order.RetrySuppressionReason == core.AutomaticRecoveryExhaustedReason:
			if !store.ReviewSeatRecoveryExhausted(order) {
				t.Fatalf("exhausted seat is not a queued unclaimed seat: %+v", order)
			}
			return order
		case !order.NextAutomaticRecoveryAt.IsZero():
			if _, err = taskops.New(st).TickOrderClock(ctx, order.NextAutomaticRecoveryAt.Add(time.Second)); err != nil {
				t.Fatal(err)
			}
		case order.State == core.WorkOrderQueued:
			expireReviewSeat(t, st, ctx, id, "exhaust-"+string(rune('a'+attempt)))
		default:
			t.Fatalf("seat cannot reach exhaustion: %+v", order)
		}
	}
	order, _ := st.GetWorkOrder(ctx, id)
	t.Fatalf("seat never exhausted: %+v", order)
	return core.WorkOrder{}
}

func assertSeatUnchanged(t *testing.T, st store.Store, ctx context.Context, before core.WorkOrder) {
	t.Helper()
	after, err := st.GetWorkOrder(ctx, before.ID)
	requireOK(t, err)
	if after.State != before.State || after.RetrySuppressed != before.RetrySuppressed || after.RetrySuppressionReason != before.RetrySuppressionReason ||
		after.AutomaticRecoveryCount != before.AutomaticRecoveryCount || after.SessionID != before.SessionID || after.ReviewRound != before.ReviewRound ||
		!after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("exhausted seat changed\nbefore=%+v\nafter=%+v", before, after)
	}
}

func decideAutomaticReview(t *testing.T, st store.Store, ctx context.Context, taskID string) store.AutomaticReviewDecision {
	t.Helper()
	task, err := st.GetTask(ctx, taskID)
	requireOK(t, err)
	orders, err := st.ListTaskWorkOrders(ctx, taskID)
	requireOK(t, err)
	reader, ok := st.(interface {
		WorkspaceConfig(context.Context) (config.VersionedDocument, error)
	})
	if !ok {
		t.Fatal("store does not expose workspace config")
	}
	current, err := reader.WorkspaceConfig(ctx)
	requireOK(t, err)
	policy := current.Document.Recovery
	policy.Window = automaticRecoveryWindow(t, st, ctx)
	now := time.Now().UTC().Add(time.Hour)
	decision := store.DecideAutomaticReviewAct(policy, task, orders, listEventsOK(t, st, ctx, taskID), now)
	// The AR-5 sweep decides from the batched reads: the task's orders from
	// ListWorkOrdersForTasks and only the folded event kinds. Every decision
	// this suite checks must be the one the full reads give.
	batchedOrders, err := st.ListWorkOrdersForTasks(ctx, []string{taskID})
	requireOK(t, err)
	batchedEvents, err := st.ListAutomaticReviewEvents(ctx, []string{taskID})
	requireOK(t, err)
	if batched := store.DecideAutomaticReviewAct(policy, task, batchedOrders, batchedEvents[taskID], now); batched != decision {
		t.Fatalf("batched AR-5 decision=%+v, full-read decision=%+v", batched, decision)
	}
	return decision
}

type automaticRoundEvent struct {
	Automatic             bool     `json:"automatic"`
	ExhaustedWorkOrderIDs []string `json:"exhausted_work_order_ids"`
}

func automaticRoundEvents(t *testing.T, st store.Store, ctx context.Context, taskID, kind string) []automaticRoundEvent {
	t.Helper()
	var out []automaticRoundEvent
	for _, event := range listEventsOK(t, st, ctx, taskID) {
		if event.Kind != kind {
			continue
		}
		var payload automaticRoundEvent
		requireOK(t, json.Unmarshal(event.Payload, &payload))
		if payload.Automatic {
			out = append(out, payload)
		}
	}
	return out
}
