package store_test

import (
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/nextaction"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// side.retrying.attempt is the number the scheduled automatic recovery will
// stamp: one past the last automatic recovery inside the window, the first of
// a new window once the window has rolled by the scheduled time, and the
// first again after an operator recovery resets the ledger.
func TestTaskFlowRetryingAttemptFollowsTheWindow(t *testing.T) {
	policy := config.DefaultRecoveryPolicy()
	start := time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)
	created := core.Event{TaskID: "t", Kind: "work_order.created", At: start, Payload: core.JSONPayload(map[string]any{"id": "o", "stage": "implement"})}
	automatic := core.Event{TaskID: "t", Kind: "work_order.recovered", At: start.Add(time.Minute), Payload: core.JSONPayload(map[string]any{"work_order_id": "o", "automatic": true, "recovery_number": 1, "window_started_at": start.Add(time.Minute)})}
	manual := core.Event{TaskID: "t", Kind: "work_order.recovered", At: start.Add(2 * time.Minute), Payload: core.JSONPayload(map[string]any{"work_order_id": "o"})}
	inside := start.Add(10 * time.Minute)
	rolled := start.Add(time.Minute + policy.Window)
	for name, tc := range map[string]struct {
		events []core.Event
		at     time.Time
		want   int
	}{
		"first":           {[]core.Event{created}, inside, 1},
		"inside window":   {[]core.Event{created, automatic}, inside, 2},
		"window rolled":   {[]core.Event{created, automatic}, rolled, 1},
		"operator resets": {[]core.Event{created, automatic, manual}, inside, 1},
	} {
		at := tc.at
		action := nextaction.NextAction{Actor: nextaction.ActorFactory, Kind: nextaction.KindRetrying, Deadline: &at, CurrentOrder: &nextaction.OrderView{ID: "o"}, Details: nextaction.Details{ReasonCode: "automatic_recovery"}}
		flow := store.FoldTaskFlow(tc.events, action, policy, at)
		if want := (store.FlowRetrying{Attempt: tc.want, Of: policy.AutomaticRecoveries}); flow.Side.Retrying == nil || *flow.Side.Retrying != want {
			t.Fatalf("%s: side.retrying = %+v, want %+v", name, flow.Side.Retrying, want)
		}
	}
}
