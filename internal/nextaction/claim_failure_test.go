package nextaction

import (
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// A queued order whose consecutive backend-failed claims reach the threshold
// states the observed claim_failing stall with its shared label, its record
// in details, and only the acts the routes accept
// (feature-operator-next-action NA-3, NA-4; req-operator-next-action AC-1.4,
// AC-1.8, AC-2.5).
func TestClaimFailingStall(t *testing.T) {
	now := time.Date(2026, 10, 30, 16, 0, 0, 0, time.UTC)
	first := now.Add(-time.Hour)
	order := core.WorkOrder{ID: "t-review-1-seat-1", TaskID: "t", Stage: core.StageReview, State: core.WorkOrderQueued, Claimable: true, QueueDeadline: now.Add(time.Hour)}
	task := core.Task{ID: "t", Title: "Task", State: core.TaskRunning, NextStage: core.StageReview}
	failure := func(count int) map[string]core.WorkOrderClaimFailure {
		return map[string]core.WorkOrderClaimFailure{order.ID: {
			WorkOrderID: order.ID, ConsecutiveFailures: count, FirstFailedAt: first, LastFailedAt: now.Add(-time.Minute),
			ErrorClass: core.WorkOrderClaimFailureClassBackendOperation,
		}}
	}
	input := func(o core.WorkOrder, failures map[string]core.WorkOrderClaimFailure) Input {
		return Input{Task: task, Orders: []core.WorkOrder{o}, ClaimFailures: failures, Capabilities: allCaps(), Now: now}
	}

	t.Run("below the threshold the order stays queued", func(t *testing.T) {
		got := Project(input(order, failure(ClaimFailureStallThreshold-1)))
		if got.Kind != KindQueued || got.Actor != ActorAgent || got.Details.ClaimFailure != nil {
			t.Fatalf("next action = %s/%s claim_failure=%+v, want queued agent work without a record", got.Actor, got.Kind, got.Details.ClaimFailure)
		}
	})

	t.Run("at the threshold the stall names the server failure", func(t *testing.T) {
		got := Project(input(order, failure(ClaimFailureStallThreshold)))
		if got.Kind != KindStalled || got.Actor != ActorOperator || got.Judgment || got.Details.StallReason != StallClaimFailing {
			t.Fatalf("next action = %s/%s judgment=%v stall=%q, want an operator stall without judgment", got.Actor, got.Kind, got.Judgment, got.Details.StallReason)
		}
		if got.Label != "Claims failing" || got.Summary != "Claims of this order keep failing with a server database error." {
			t.Fatalf("label %q summary %q", got.Label, got.Summary)
		}
		if got.IfIgnored != "Claims keep failing until the server's database error is fixed." {
			t.Fatalf("if_ignored %q", got.IfIgnored)
		}
		record := got.Details.ClaimFailure
		if record == nil || record.ConsecutiveFailures != ClaimFailureStallThreshold || !record.FirstFailedAt.Equal(first) || record.ErrorClass != core.WorkOrderClaimFailureClassBackendOperation {
			t.Fatalf("details.claim_failure = %+v", record)
		}
		if got.CurrentOrder == nil || got.CurrentOrder.ID != order.ID {
			t.Fatalf("current order = %+v, want %s", got.CurrentOrder, order.ID)
		}
		// A never-attempted queued order is refused by the recover route, so
		// only start over is offered and the refusal is recorded.
		if len(got.Choices) != 1 || got.Choices[0].ID != "start_over" || got.Choices[0].Path != "/v1/tasks/t/restart" {
			t.Fatalf("choices = %+v, want start over only", got.Choices)
		}
		if !hasRefusalText(got, "recover", "work order t-review-1-seat-1 is not released, expired, or retry-suppressed") {
			t.Fatalf("refusals = %+v, want the recover route's refusal", got.Details.Refusals)
		}
		for _, choice := range got.Choices {
			if choice.ID == "redispatch" || choice.ID == "change_policy" {
				t.Fatalf("claim_failing offered %s: %+v", choice.ID, got.Choices)
			}
		}
		for _, text := range []string{got.Summary, got.Question, got.IfIgnored} {
			if strings.Contains(text, order.ID) || strings.Contains(text, StallClaimFailing) {
				t.Fatalf("sentence %q carries a machine identifier", text)
			}
		}
		compact := CompactFrom(got)
		if compact.Label != got.Label || compact.Summary != got.Summary || compact.Judgment {
			t.Fatalf("compact = %+v, want the same label and summary without judgment", compact)
		}
	})

	t.Run("a released order offers recovery", func(t *testing.T) {
		released := order
		released.LastAttemptOutcome = core.WorkOrderOutcomeExpired
		got := Project(input(released, failure(ClaimFailureStallThreshold+5)))
		if got.Details.StallReason != StallClaimFailing {
			t.Fatalf("stall = %q, want claim_failing", got.Details.StallReason)
		}
		if len(got.Choices) != 2 || got.Choices[0].ID != "recover" || got.Choices[0].Path != "/v1/work-orders/t-review-1-seat-1/recover" || got.Choices[1].ID != "start_over" {
			t.Fatalf("choices = %+v, want recover then start over", got.Choices)
		}
	})

	t.Run("a stale order keeps its deterministic reason", func(t *testing.T) {
		stale := order
		stale.State = core.WorkOrderStale
		got := Project(input(stale, failure(ClaimFailureStallThreshold)))
		if got.Details.StallReason != StallStale || got.Details.ClaimFailure != nil {
			t.Fatalf("stall = %q claim_failure=%+v, want stale without a record", got.Details.StallReason, got.Details.ClaimFailure)
		}
	})

	t.Run("a deterministic reason wins over the record", func(t *testing.T) {
		suppressed := order
		suppressed.RetrySuppressed = true
		got := Project(input(suppressed, failure(ClaimFailureStallThreshold)))
		if got.Details.StallReason != StallRetrySuppressed {
			t.Fatalf("stall = %q, want retry_suppressed", got.Details.StallReason)
		}
	})

	t.Run("a record of another order does not stall this one", func(t *testing.T) {
		other := failure(ClaimFailureStallThreshold)
		other["other-order"] = other[order.ID]
		delete(other, order.ID)
		got := Project(input(order, other))
		if got.Kind != KindQueued {
			t.Fatalf("kind = %s, want queued", got.Kind)
		}
	})
}
