package store

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

func createMemoryReviewOrderInState(t *testing.T, st Store, ctx context.Context, order core.WorkOrder) {
	t.Helper()
	target := order.State
	order.State = core.WorkOrderQueued
	if target == core.WorkOrderTimedOut && order.ExecutionDeadline.IsZero() {
		order.ExecutionDeadline = time.Now().Add(-time.Minute)
	}
	if err := storetestFor(st).CreateWorkOrder(ctx, order); err != nil {
		t.Fatal(err)
	}
	switch target {
	case core.WorkOrderQueued:
		return
	case core.WorkOrderCompleted:
		claimed, err := storetestFor(st).ClaimWorkOrder(ctx, order.ID, core.WorkOrderClaim{SessionID: order.ID + "-session", ClientToken: "test-token", ClaimantID: "worker", WorkerID: "worker", Lease: time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		claimed.State = core.WorkOrderCompleted
		if err = storetestFor(st).UpdateWorkOrder(ctx, claimed, core.WorkOrderCmdSubmitReviewVerdict); err != nil {
			t.Fatal(err)
		}
	case core.WorkOrderTimedOut:
		persisted, err := st.GetWorkOrder(ctx, order.ID)
		if err != nil || persisted.State != core.WorkOrderTimedOut {
			t.Fatalf("timed-out order=%+v err=%v", persisted, err)
		}
	default:
		t.Fatalf("unsupported review-order fixture state %q", target)
	}
}

func TestMemoryRetryReviewRoundPreservesHistoryAndIsIdempotent(t *testing.T) {
	ctx := WithActor(WithWorkspace(context.Background(), "demo"), Actor{ID: "operator-7", Role: core.ActorHuman})
	st := NewMemory()
	task := core.Task{ID: "retry-review", Workspace: "demo", State: core.TaskRunning, NextStage: core.StageReview, CreatedAt: time.Now().UTC()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	prior := []core.WorkOrder{
		{ID: task.ID + "-review-1-seat-1", TaskID: task.ID, JobID: task.ID + "-review-1-seat-1", Stage: core.StageReview, State: core.WorkOrderCompleted, ReviewRound: 1, ReviewSeat: 1},
		{ID: task.ID + "-review-1-seat-2", TaskID: task.ID, JobID: task.ID + "-review-1-seat-2", Stage: core.StageReview, State: core.WorkOrderTimedOut, ReviewRound: 1, ReviewSeat: 2, ExecutionDeadline: time.Now().Add(-time.Minute), LastFailureMessage: "review harness exhausted retries"},
	}
	for _, order := range prior {
		if err := st.CreateJob(ctx, core.Job{ID: order.JobID, TaskID: task.ID, Stage: core.StageReview, State: core.JobDone}); err != nil {
			t.Fatal(err)
		}
		createMemoryReviewOrderInState(t, st, ctx, order)
	}
	if err := st.AppendEvent(ctx, core.Event{TaskID: task.ID, JobID: prior[0].JobID, Kind: "review.completed", Payload: core.JSONPayload(map[string]any{"review_work_order_id": prior[0].ID, "review_round": 1, "review_seat": 1, "verdict": "approve", "reason_code": "approved", "summary": "historical approval"})}); err != nil {
		t.Fatal(err)
	}
	if recovery := ReviewRecoveryNeeded(prior); recovery == nil || recovery.PriorRound != 1 || len(recovery.TimedOutOrders) != 1 {
		t.Fatalf("recovery=%+v", recovery)
	}
	now := time.Now().UTC()
	jobs := []core.Job{
		{ID: task.ID + "-review-2-seat-1", TaskID: task.ID, Stage: core.StageReview, State: core.JobPending},
		{ID: task.ID + "-review-2-seat-2", TaskID: task.ID, Stage: core.StageReview, State: core.JobPending},
	}
	orders := []core.WorkOrder{
		{ID: jobs[0].ID, TaskID: task.ID, JobID: jobs[0].ID, Stage: core.StageReview, ReviewRound: 2, ReviewSeat: 1, RequiredModel: "current-gpt", RequiredHarnessConfig: &core.HarnessSnapshot{Name: "codex", Command: []string{"current-codex"}}, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour)},
		{ID: jobs[1].ID, TaskID: task.ID, JobID: jobs[1].ID, Stage: core.StageReview, ReviewRound: 2, ReviewSeat: 2, RequiredModel: "current-claude", RequiredHarnessConfig: &core.HarnessSnapshot{Name: "claude", Command: []string{"current-claude"}}, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour)},
	}
	request := ReviewRoundRetryRequest{TaskID: task.ID, RequestID: "retry-1", Reason: "seat 2 terminally timed out", PriorRound: 1, PRHead: "abc123"}
	result, err := storetestFor(st).RetryReviewRound(ctx, request, jobs, orders)
	if err != nil || result.NewRound != 2 || len(result.WorkOrders) != 2 || result.WorkOrders[1].RequiredHarnessConfig.Command[0] != "current-claude" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if err = st.AcceptReviewDecision(ctx, core.ReviewDecision{TaskID: task.ID, JobID: jobs[0].ID, ReviewWorkOrderID: orders[0].ID, ReviewRound: 2, ReviewSeat: 1, Verdict: "approve", ReasonCode: "approved", Summary: "new seat one approved", PolicyVersion: 1}); err != nil {
		t.Fatal(err)
	}
	if completed, _ := st.CountEvents(ctx, task.ID, "review.round_completed"); completed != 0 {
		t.Fatalf("historical verdict completed the new round: %d", completed)
	}
	duplicate, err := storetestFor(st).RetryReviewRound(ctx, request, jobs, orders)
	if err != nil || duplicate.NewRound != result.NewRound || len(duplicate.WorkOrders) != 2 {
		t.Fatalf("duplicate=%+v err=%v", duplicate, err)
	}
	changed := request
	changed.Reason = "different request"
	if _, err = storetestFor(st).RetryReviewRound(ctx, changed, jobs, orders); !errors.Is(err, ErrReviewRetryConflict) {
		t.Fatalf("changed idempotency error=%v", err)
	}
	if _, err = storetestFor(st).RetryReviewRound(WithWorkspace(context.Background(), "other"), request, jobs, orders); !errors.Is(err, ErrReviewRetryConflict) {
		t.Fatalf("cross-workspace request-id reuse error=%v", err)
	}
	for _, original := range prior {
		persisted, getErr := st.GetWorkOrder(ctx, original.ID)
		if getErr != nil || persisted.State != original.State || persisted.ReviewRound != 1 {
			t.Fatalf("prior=%+v err=%v", persisted, getErr)
		}
	}
	events, err := st.ListEvents(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	retried := 0
	for _, event := range events {
		if event.Kind != "review.round_retried" {
			continue
		}
		retried++
		var payload struct {
			RequestID  string `json:"request_id"`
			Actor      string `json:"actor"`
			Reason     string `json:"reason"`
			PriorRound int    `json:"prior_round"`
			NewRound   int    `json:"new_round"`
		}
		if json.Unmarshal(event.Payload, &payload) != nil || payload.RequestID != request.RequestID || payload.Actor != "operator-7" || payload.Reason != request.Reason || payload.PriorRound != 1 || payload.NewRound != 2 {
			t.Fatalf("audit=%s", event.Payload)
		}
	}
	if retried != 1 {
		t.Fatalf("retry audit events=%d", retried)
	}
}

func TestReviewRecoveryNeedsTerminalLatestTimedOutRound(t *testing.T) {
	base := []core.WorkOrder{{ID: "old-timeout", Stage: core.StageReview, ReviewRound: 1, State: core.WorkOrderTimedOut}}
	if recovery := ReviewRecoveryNeeded(base); recovery == nil || recovery.PriorRound != 1 {
		t.Fatalf("terminal recovery=%+v", recovery)
	}
	active := append(base, core.WorkOrder{ID: "active", Stage: core.StageReview, ReviewRound: 2, State: core.WorkOrderQueued})
	if recovery := ReviewRecoveryNeeded(active); recovery != nil {
		t.Fatalf("active latest round recovery=%+v", recovery)
	}
	completed := append(base, core.WorkOrder{ID: "complete", Stage: core.StageReview, ReviewRound: 2, State: core.WorkOrderCompleted})
	if recovery := ReviewRecoveryNeeded(completed); recovery != nil {
		t.Fatalf("completed latest round recovery=%+v", recovery)
	}
}

func TestMemoryRetryReviewRoundSerializesConcurrentRequests(t *testing.T) {
	ctx := WithWorkspace(context.Background(), "demo")
	st := NewMemory()
	task := core.Task{ID: "concurrent-review-retry", Workspace: "demo", State: core.TaskRunning, NextStage: core.StageReview, CreatedAt: time.Now().UTC()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	oldID := task.ID + "-review-1-seat-1"
	if err := st.CreateJob(ctx, core.Job{ID: oldID, TaskID: task.ID, Stage: core.StageReview, State: core.JobFailed}); err != nil {
		t.Fatal(err)
	}
	createMemoryReviewOrderInState(t, st, ctx, core.WorkOrder{ID: oldID, TaskID: task.ID, JobID: oldID, Stage: core.StageReview, State: core.WorkOrderTimedOut, ExecutionDeadline: time.Now().Add(-time.Minute), ReviewRound: 1, ReviewSeat: 1})
	newID := task.ID + "-review-2-seat-1"
	jobs := []core.Job{{ID: newID, TaskID: task.ID, Stage: core.StageReview, State: core.JobPending}}
	orders := []core.WorkOrder{{ID: newID, TaskID: task.ID, JobID: newID, Stage: core.StageReview, ReviewRound: 2, ReviewSeat: 1}}
	start := make(chan struct{})
	ready := sync.WaitGroup{}
	ready.Add(2)
	results := make(chan error, 2)
	for _, requestID := range []string{"concurrent-a", "concurrent-b"} {
		requestID := requestID
		go func() {
			ready.Done()
			<-start
			_, retryErr := storetestFor(st).RetryReviewRound(ctx, ReviewRoundRetryRequest{TaskID: task.ID, RequestID: requestID, Reason: "terminal timeout", PriorRound: 1, PRHead: "head"}, jobs, orders)
			results <- retryErr
		}()
	}
	ready.Wait()
	close(start)
	succeeded, conflicted := 0, 0
	for range 2 {
		err := <-results
		if err == nil {
			succeeded++
		} else if errors.Is(err, ErrReviewRetryConflict) {
			conflicted++
		} else {
			t.Fatalf("unexpected error=%v", err)
		}
	}
	if succeeded != 1 || conflicted != 1 {
		t.Fatalf("succeeded=%d conflicted=%d", succeeded, conflicted)
	}
	persisted, err := st.ListTaskWorkOrders(ctx, task.ID)
	if err != nil || len(persisted) != 2 {
		t.Fatalf("orders=%+v err=%v", persisted, err)
	}
}
