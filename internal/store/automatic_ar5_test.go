package store

import (
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
)

func TestAutomaticPublicationDue(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	policy := config.RecoveryPolicy{AutomaticRecoveries: 2, Window: time.Hour}
	for _, tc := range []struct {
		name   string
		policy config.RecoveryPolicy
		state  core.TaskState
		failed bool
		stamp  time.Time
		want   bool
	}{
		{"first failure", policy, core.TaskRunning, true, time.Time{}, true},
		{"queued task", policy, core.TaskQueued, true, time.Time{}, true},
		{"not failed", policy, core.TaskRunning, false, time.Time{}, false},
		{"bound zero", config.RecoveryPolicy{Window: time.Hour}, core.TaskRunning, true, time.Time{}, false},
		{"stamped inside window", policy, core.TaskRunning, true, now.Add(-time.Hour + time.Second), false},
		{"stamped exactly one window ago", policy, core.TaskRunning, true, now.Add(-time.Hour), true},
		{"stamped before window", policy, core.TaskRunning, true, now.Add(-2 * time.Hour), true},
		{"merged", policy, core.TaskMerged, true, time.Time{}, false},
		{"closed", policy, core.TaskClosed, true, time.Time{}, false},
		{"awaiting", policy, core.TaskAwaiting, true, time.Time{}, false},
		{"parked", policy, core.TaskParked, true, time.Time{}, false},
	} {
		if got := AutomaticPublicationDue(tc.policy, tc.state, tc.failed, tc.stamp, now); got != tc.want {
			t.Errorf("%s: due=%t want %t", tc.name, got, tc.want)
		}
	}
}

func TestLatestForgeFailureAutomaticRetryExhausted(t *testing.T) {
	at := time.Now().UTC()
	failed := func(kind string, offset int) core.Event {
		return core.Event{Kind: kind, JobID: "review-1", At: at.Add(time.Duration(offset) * time.Second), Payload: core.JSONPayload(map[string]any{"review_work_order_id": "review-1", "forge_error_category": "forge_request", "last_error": "boom"})}
	}
	retry := func(kind string, automatic bool, offset int) core.Event {
		payload := map[string]any{"review_work_order_id": "review-1"}
		if automatic {
			payload["automatic"] = true
		}
		return core.Event{Kind: kind, JobID: "review-1", At: at.Add(time.Duration(offset) * time.Second), Payload: core.JSONPayload(payload)}
	}
	for _, surface := range []string{"github_issue", "review"} {
		events := []core.Event{retry(surface+".publication_retry", false, 0), failed(surface+".publication_failed", 1)}
		if failure := LatestForgeFailure(events); failure == nil || failure.AutomaticRetryExhausted {
			t.Fatalf("%s first failure=%+v", surface, failure)
		}
		events = append(events, retry(surface+".publication_retry", true, 2), failed(surface+".publication_failed", 3))
		if failure := LatestForgeFailure(events); failure == nil || !failure.AutomaticRetryExhausted {
			t.Fatalf("%s failure after automatic retry=%+v", surface, failure)
		}
		events = append(events, retry(surface+".publication_published", false, 4), failed(surface+".publication_failed", 5))
		if failure := LatestForgeFailure(events); failure == nil || failure.AutomaticRetryExhausted {
			t.Fatalf("%s failure after publish=%+v", surface, failure)
		}
	}
}

func TestDecideAutomaticReviewAct(t *testing.T) {
	now := time.Now().UTC()
	policy := config.RecoveryPolicy{AutomaticRecoveries: 2, Window: time.Hour}
	running := core.Task{ID: "task", State: core.TaskRunning, NextStage: core.StageReview}
	verdict := core.WorkOrder{ID: "seat-1", TaskID: "task", Stage: core.StageReview, State: core.WorkOrderCompleted, ReviewRound: 1, ReviewSeat: 1}
	seat := func(round int, mutate func(*core.WorkOrder)) core.WorkOrder {
		order := core.WorkOrder{ID: "seat-2-r" + string(rune('0'+round)), TaskID: "task", Stage: core.StageReview, State: core.WorkOrderQueued, ReviewRound: round, ReviewSeat: 2}
		mutate(&order)
		return order
	}
	exhausted := func(o *core.WorkOrder) {
		o.RetrySuppressed, o.RetrySuppressionReason, o.AutomaticRecoveryCount = true, core.AutomaticRecoveryExhaustedReason, 2
		o.LastAttemptOutcome = core.WorkOrderOutcomeExpired
	}
	interrupted := func(o *core.WorkOrder) { o.RetrySuppressed, o.RetrySuppressionReason = true, "operator release" }
	completedEvent := core.Event{Kind: "review.completed", Payload: core.JSONPayload(map[string]any{"review_work_order_id": "seat-1", "review_round": 1})}
	act := func(kind string, payload map[string]any) core.Event {
		payload["automatic"] = true
		return core.Event{Kind: kind, Payload: core.JSONPayload(payload)}
	}
	for _, tc := range []struct {
		name   string
		policy config.RecoveryPolicy
		task   core.Task
		orders []core.WorkOrder
		events []core.Event
		want   AutomaticReviewDecision
	}{
		{"exhausted seat is replaced by a retry", policy, running, []core.WorkOrder{verdict, seat(1, exhausted)}, []core.Event{completedEvent}, AutomaticReviewDecision{Kind: AutomaticReviewRetry, Round: 1}},
		{"timed-out seat after the clock gave up", policy, running, []core.WorkOrder{verdict, seat(1, func(o *core.WorkOrder) {
			o.State, o.RetrySuppressed, o.RetrySuppressionReason = core.WorkOrderTimedOut, true, core.AutomaticRecoveryExhaustedReason
		})}, []core.Event{completedEvent}, AutomaticReviewDecision{Kind: AutomaticReviewRetry, Round: 1}},
		{"contradictory completed seat", policy, running, []core.WorkOrder{verdict, seat(1, func(o *core.WorkOrder) {
			o.State, o.LastFailureMessage = core.WorkOrderCompleted, "child crashed"
		})}, []core.Event{completedEvent}, AutomaticReviewDecision{Kind: AutomaticReviewRetry, Round: 1}},
		{"interrupted seat is recovered in place", policy, running, []core.WorkOrder{verdict, seat(1, interrupted)}, []core.Event{completedEvent}, AutomaticReviewDecision{Kind: AutomaticReviewRecover, Round: 1}},
		{"defers while the clock schedules the seat", policy, running, []core.WorkOrder{verdict, seat(1, func(o *core.WorkOrder) {
			o.State, o.NextAutomaticRecoveryAt = core.WorkOrderTimedOut, now.Add(time.Minute)
		})}, []core.Event{completedEvent}, AutomaticReviewDecision{Kind: AutomaticReviewNone}},
		{"bound zero", config.RecoveryPolicy{Window: time.Hour}, running, []core.WorkOrder{verdict, seat(1, exhausted)}, []core.Event{completedEvent}, AutomaticReviewDecision{Kind: AutomaticReviewNone}},
		{"awaiting task", policy, core.Task{ID: "task", State: core.TaskAwaiting, NextStage: core.StageReview}, []core.WorkOrder{verdict, seat(1, exhausted)}, []core.Event{completedEvent}, AutomaticReviewDecision{Kind: AutomaticReviewNone}},
		{"approved task", policy, core.Task{ID: "task", State: core.TaskApproved, NextStage: core.StageReview}, []core.WorkOrder{verdict, seat(1, exhausted)}, []core.Event{completedEvent}, AutomaticReviewDecision{Kind: AutomaticReviewNone}},
		{"identical-failure seat", policy, running, []core.WorkOrder{verdict, seat(1, func(o *core.WorkOrder) {
			o.RetrySuppressed, o.RetrySuppressionReason = true, core.IdenticalFailureSuppressionReason
		})}, []core.Event{completedEvent}, AutomaticReviewDecision{Kind: AutomaticReviewNone}},
		{"checkpoint seat", policy, running, []core.WorkOrder{verdict, seat(1, func(o *core.WorkOrder) {
			interrupted(o)
			o.Checkpoint = &core.WorkOrderCheckpoint{DecisionRequest: "hold"}
		})}, []core.Event{completedEvent}, AutomaticReviewDecision{Kind: AutomaticReviewNone}},
		{"round already recovered automatically", policy, running, []core.WorkOrder{verdict, seat(1, interrupted)},
			[]core.Event{completedEvent, act("review.round_recovered", map[string]any{"review_round": 1})}, AutomaticReviewDecision{Kind: AutomaticReviewNone}},
		{"round created by an automatic retry", policy, running, []core.WorkOrder{seat(2, interrupted)},
			[]core.Event{act("review.round_retried", map[string]any{"prior_round": 1, "new_round": 2})}, AutomaticReviewDecision{Kind: AutomaticReviewNone}},
		{"operator act does not count", policy, running, []core.WorkOrder{verdict, seat(1, interrupted)},
			[]core.Event{completedEvent, {Kind: "review.round_recovered", Payload: core.JSONPayload(map[string]any{"review_round": 1})}}, AutomaticReviewDecision{Kind: AutomaticReviewRecover, Round: 1}},
	} {
		if got := DecideAutomaticReviewAct(tc.policy, tc.task, tc.orders, tc.events, now); got != tc.want {
			t.Errorf("%s: decision=%+v want %+v", tc.name, got, tc.want)
		}
	}
}
