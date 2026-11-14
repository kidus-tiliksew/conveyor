package store

import (
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// The next-action recovery projection reads a stored stale order as the queued
// order the clock returns it to, even from an unprojected row: an age-only
// schedule or exhaustion projects nothing, and the exhaustion of a genuine
// failed attempt stays (component-work-orders invariant 5; DEC-117(3)).
func TestAutomaticRecoveryForReadsStoredStaleAsWaiting(t *testing.T) {
	now := time.Now().UTC()
	queueOnly := core.WorkOrder{ID: "queue-only", State: core.WorkOrderStale, RetrySuppressed: true,
		RetrySuppressionReason: core.AutomaticRecoveryExhaustedReason, NextAutomaticRecoveryAt: now.Add(time.Hour)}
	scheduled := core.WorkOrder{ID: "scheduled", State: core.WorkOrderStale, NextAutomaticRecoveryAt: now.Add(time.Hour)}
	genuine := core.WorkOrder{ID: "genuine", State: core.WorkOrderStale, LastAttemptOutcome: core.WorkOrderOutcomeExpired,
		RetrySuppressed: true, RetrySuppressionReason: core.AutomaticRecoveryExhaustedReason}
	got := AutomaticRecoveryFor([]core.WorkOrder{queueOnly, scheduled, genuine})
	if _, ok := got["queue-only"]; ok {
		t.Fatalf("queue-only stale order projected %+v", got["queue-only"])
	}
	if _, ok := got["scheduled"]; ok {
		t.Fatalf("age-only schedule projected %+v", got["scheduled"])
	}
	if rec, ok := got["genuine"]; !ok || !rec.Exhausted || rec.NextAt != nil {
		t.Fatalf("genuine failure projection=%+v ok=%t, want exhausted", rec, ok)
	}
}
