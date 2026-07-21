package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

func TestInterruptedReviewRecoveryNeededEmitsEmptyCollectionsAsArrays(t *testing.T) {
	orders := []core.WorkOrder{
		{ID: "review-seat-1", Stage: core.StageReview, State: core.WorkOrderQueued, ReviewRound: 1, ReviewSeat: 1, RetrySuppressed: true},
		{ID: "review-seat-2", Stage: core.StageReview, State: core.WorkOrderQueued, ReviewRound: 1, ReviewSeat: 2, RetrySuppressed: true},
	}
	recovery := InterruptedReviewRecoveryNeeded(orders)
	if recovery == nil || recovery.EligibleOrders == nil || recovery.RetainedOrders == nil || len(recovery.EligibleOrders) != 2 || len(recovery.RetainedOrders) != 0 {
		t.Fatalf("recovery=%+v", recovery)
	}
	data, err := json.Marshal(recovery)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"eligible_orders":[`) || !strings.Contains(string(data), `"retained_orders":[]`) {
		t.Fatalf("recovery JSON contains nullable collections: %s", data)
	}
}

func TestMemoryInterruptedReviewRecoveryResultEmitsEmptyRetainedOrdersAsArray(t *testing.T) {
	st := NewMemory()
	ctx := WithActor(WithWorkspace(t.Context(), "demo"), Actor{ID: "operator", Role: core.ActorHuman})
	now := time.Now().UTC()
	task := core.Task{ID: "all-interrupted-review", Workspace: "demo", Repo: "app", State: core.TaskRunning, NextStage: core.StageReview, CreatedAt: now}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	for seat := 1; seat <= 2; seat++ {
		id := fmt.Sprintf("%s-review-1-seat-%d", task.ID, seat)
		if err := st.CreateJob(ctx, core.Job{ID: id, TaskID: task.ID, Stage: core.StageReview, State: core.JobPending}); err != nil {
			t.Fatal(err)
		}
		if err := st.CreateWorkOrder(ctx, core.WorkOrder{ID: id, TaskID: task.ID, JobID: id, Stage: core.StageReview, State: core.WorkOrderQueued, ReviewRound: 1, ReviewSeat: seat, RetrySuppressed: true, CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	result, err := st.RecoverInterruptedReviewRound(ctx, InterruptedReviewRecoveryRequest{TaskID: task.ID, RequestID: "recover-empty-retained", Round: 1}, time.Hour)
	if err != nil || result.RecoveredOrders == nil || result.RetainedOrders == nil || len(result.RecoveredOrders) != 2 || len(result.RetainedOrders) != 0 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"recovered_orders":[`) || !strings.Contains(string(data), `"retained_orders":[]`) {
		t.Fatalf("recovery result JSON contains nullable collections: %s", data)
	}
}

func interruptedReviewFixture(t *testing.T, st Store) context.Context {
	t.Helper()
	ctx := WithActor(WithWorkspace(t.Context(), "demo"), Actor{ID: "operator", Role: core.ActorHuman})
	task := core.Task{ID: "interrupted-review", Workspace: "demo", Repo: "app", State: core.TaskRunning, NextStage: core.StageReview, CreatedAt: time.Now().UTC()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	orders := []core.WorkOrder{
		{ID: "interrupted-review-review-1-seat-1", TaskID: task.ID, JobID: "job-1", Stage: core.StageReview, State: core.WorkOrderCompleted, ReviewRound: 1, ReviewSeat: 1, CreatedAt: task.CreatedAt},
		{ID: "interrupted-review-review-1-seat-2", TaskID: task.ID, JobID: "job-2", Stage: core.StageReview, State: core.WorkOrderQueued, ReviewRound: 1, ReviewSeat: 2, RetrySuppressed: true, LastAttemptOutcome: core.WorkOrderOutcomeExpired, CreatedAt: task.CreatedAt},
	}
	for _, order := range orders {
		if err := st.CreateJob(ctx, core.Job{ID: order.JobID, TaskID: task.ID, Stage: core.StageReview, State: core.JobPending}); err != nil {
			t.Fatal(err)
		}
		if err := st.CreateWorkOrder(ctx, order); err != nil {
			t.Fatal(err)
		}
	}
	return ctx
}

func TestMemoryInterruptedReviewRecoveryRetainsCompletedSeatAndIsIdempotent(t *testing.T) {
	st := NewMemory()
	ctx := interruptedReviewFixture(t, st)
	request := InterruptedReviewRecoveryRequest{TaskID: "interrupted-review", RequestID: "recover-1", Round: 1}
	result, err := st.RecoverInterruptedReviewRound(ctx, request, time.Hour)
	if err != nil || len(result.RecoveredOrders) != 1 || result.RecoveredOrders[0].ReviewSeat != 2 || len(result.RetainedOrders) != 1 || result.RetainedOrders[0].ReviewSeat != 1 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	duplicate, err := st.RecoverInterruptedReviewRound(ctx, request, time.Hour)
	if err != nil || duplicate.RequestID != result.RequestID || len(duplicate.RecoveredOrders) != 1 {
		t.Fatalf("idempotent result=%+v err=%v", duplicate, err)
	}
	if _, err = st.RecoverInterruptedReviewRound(ctx, InterruptedReviewRecoveryRequest{TaskID: "interrupted-review", RequestID: "recover-1", Round: 2}, time.Hour); !errors.Is(err, ErrReviewRetryConflict) {
		t.Fatalf("divergent request err=%v", err)
	}
	events, _ := st.ListEvents(ctx, "interrupted-review")
	var round, recovered, retained int
	for _, event := range events {
		switch event.Kind {
		case "review.round_recovered":
			round++
		case "review.seat_recovered":
			recovered++
		case "review.seat_recovery_skipped":
			retained++
		}
	}
	if round != 1 || recovered != 1 || retained != 1 {
		t.Fatalf("audit events round=%d recovered=%d retained=%d", round, recovered, retained)
	}
}

func TestMemoryInterruptedReviewRecoverySerializesConcurrentRequests(t *testing.T) {
	st := NewMemory()
	ctx := interruptedReviewFixture(t, st)
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, id := range []string{"concurrent-a", "concurrent-b"} {
		id := id
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := st.RecoverInterruptedReviewRound(ctx, InterruptedReviewRecoveryRequest{TaskID: "interrupted-review", RequestID: id, Round: 1}, time.Hour)
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	var successes int
	for err := range results {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("concurrent successes=%d want=1", successes)
	}
}
