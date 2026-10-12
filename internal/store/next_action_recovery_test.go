package store_test

import (
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/nextaction"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// Only the AR-3 exhaustion reason is automatic-recovery exhaustion: an
// identical-failure suppression stays the retry_suppressed stall the operator
// judges (AC-2.1), and an order with no ledger is absent from the map.
func TestAutomaticRecoveryForProjectsOnlyTheLedger(t *testing.T) {
	now := time.Now().UTC()
	next := now.Add(time.Minute)
	orders := []core.WorkOrder{
		{ID: "scheduled", State: core.WorkOrderTimedOut, AutomaticRecoveryCount: 1, NextAutomaticRecoveryAt: next},
		{ID: "exhausted", State: core.WorkOrderTimedOut, RetrySuppressed: true, RetrySuppressionReason: core.AutomaticRecoveryExhaustedReason},
		{ID: "identical", State: core.WorkOrderQueued, RetrySuppressed: true, RetrySuppressionReason: core.IdenticalFailureSuppressionReason},
		{ID: "plain", State: core.WorkOrderQueued},
	}
	got := store.AutomaticRecoveryFor(orders)
	if len(got) != 2 {
		t.Fatalf("entries = %+v, want scheduled and exhausted only", got)
	}
	if rec := got["scheduled"]; rec.NextAt == nil || !rec.NextAt.Equal(next) || rec.Exhausted || rec.Attempt != 2 {
		t.Fatalf("scheduled = %+v", rec)
	}
	if rec := got["exhausted"]; !rec.Exhausted || rec.NextAt != nil {
		t.Fatalf("exhausted = %+v", rec)
	}
	task := core.Task{State: core.TaskRunning}
	for id, want := range map[string]string{"identical": nextaction.StallRetrySuppressed, "exhausted": nextaction.StallAutomaticRecoveryExhausted} {
		var order []core.WorkOrder
		for _, candidate := range orders {
			if candidate.ID == id {
				order = append(order, candidate)
			}
		}
		hit := nextaction.Stall(task, order, got, nil, now)
		if hit == nil || hit.Reason != want {
			t.Fatalf("%s stall = %+v, want %s", id, hit, want)
		}
	}
}
