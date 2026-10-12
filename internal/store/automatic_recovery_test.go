package store

import (
	"math/rand/v2"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
)

func recoveryPolicy(bound int, window time.Duration) config.RecoveryPolicy {
	policy := config.DefaultRecoveryPolicy()
	policy.AutomaticRecoveries = bound
	policy.Window = window
	policy.WindowText = window.String()
	return policy
}

func TestDecideAutomaticRecovery(t *testing.T) {
	now := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	window := 24 * time.Hour
	policy := recoveryPolicy(2, window)
	due := now.Add(-time.Minute)
	future := now.Add(time.Minute)
	queued := func(order core.WorkOrder) core.WorkOrder {
		order.State = core.WorkOrderQueued
		return order
	}
	cases := []struct {
		name   string
		task   core.Task
		order  core.WorkOrder
		policy config.RecoveryPolicy
		now    time.Time
		want   configKind
	}{}
	_ = cases
	_ = queued
	_ = due
	_ = future
	_ = policy
	type row struct {
		name   string
		task   core.Task
		order  core.WorkOrder
		policy config.RecoveryPolicy
		now    time.Time
		want   AutomaticRecoveryKind
		at     time.Time
		number int
	}
	running := core.Task{State: core.TaskRunning}
	base := func(state core.WorkOrderState) core.WorkOrder {
		return core.WorkOrder{ID: "order", State: state, QueueEnteredAt: now.Add(-time.Hour)}
	}
	rows := []row{
		{name: "awaiting human", task: core.Task{State: core.TaskAwaiting}, order: base(core.WorkOrderTimedOut), policy: policy, now: now, want: AutomaticRecoveryNone},
		{name: "approved", task: core.Task{State: core.TaskApproved}, order: base(core.WorkOrderTimedOut), policy: policy, now: now, want: AutomaticRecoveryNone},
		{name: "merged", task: core.Task{State: core.TaskMerged}, order: base(core.WorkOrderTimedOut), policy: policy, now: now, want: AutomaticRecoveryNone},
		{name: "closed", task: core.Task{State: core.TaskClosed}, order: base(core.WorkOrderTimedOut), policy: policy, now: now, want: AutomaticRecoveryNone},
		{name: "live claim", task: running, order: func() core.WorkOrder {
			order := base(core.WorkOrderClaimed)
			order.LeaseExpiresAt = now.Add(time.Minute)
			order.NextAutomaticRecoveryAt = due
			return order
		}(), policy: policy, now: now, want: AutomaticRecoveryNone},
		{name: "zero lease claim", task: running, order: func() core.WorkOrder {
			order := base(core.WorkOrderClaimed)
			order.NextAutomaticRecoveryAt = due
			return order
		}(), policy: policy, now: now, want: AutomaticRecoveryNone},
		{name: "identical failure", task: running, order: func() core.WorkOrder {
			order := base(core.WorkOrderTimedOut)
			order.RetrySuppressionReason = core.IdenticalFailureSuppressionReason
			order.NextAutomaticRecoveryAt = due
			return order
		}(), policy: policy, now: now, want: AutomaticRecoveryNone},
		{name: "checkpoint reason", task: running, order: func() core.WorkOrder {
			order := base(core.WorkOrderTimedOut)
			order.RetrySuppressionReason = core.WorkOrderReleaseReasonOperatorCheckpointReached
			order.NextAutomaticRecoveryAt = due
			return order
		}(), policy: policy, now: now, want: AutomaticRecoveryNone},
		{name: "plan revision requested", task: running, order: func() core.WorkOrder {
			order := base(core.WorkOrderStale)
			order.RetrySuppressionReason = core.WorkOrderReleaseReasonPlanRevisionRequested
			order.NextAutomaticRecoveryAt = due
			return order
		}(), policy: policy, now: now, want: AutomaticRecoveryNone},
		{name: "superseded head", task: running, order: func() core.WorkOrder {
			order := base(core.WorkOrderTimedOut)
			order.RetrySuppressionReason = "superseded head"
			order.NextAutomaticRecoveryAt = due
			return order
		}(), policy: policy, now: now, want: AutomaticRecoveryNone},
		{name: "already exhausted", task: running, order: func() core.WorkOrder {
			order := base(core.WorkOrderTimedOut)
			order.RetrySuppressionReason = core.AutomaticRecoveryExhaustedReason
			order.NextAutomaticRecoveryAt = due
			return order
		}(), policy: policy, now: now, want: AutomaticRecoveryNone},
		{name: "checkpoint set", task: running, order: func() core.WorkOrder {
			order := base(core.WorkOrderTimedOut)
			order.Checkpoint = &core.WorkOrderCheckpoint{DecisionRequest: "wait"}
			order.NextAutomaticRecoveryAt = due
			return order
		}(), policy: policy, now: now, want: AutomaticRecoveryNone},
		{name: "recovery refusal", task: running, order: func() core.WorkOrder {
			order := base(core.WorkOrderTimedOut)
			order.RecoveryRefusal = "verify dispatch refused"
			order.NextAutomaticRecoveryAt = due
			return order
		}(), policy: policy, now: now, want: AutomaticRecoveryNone},
		{name: "bound zero", task: running, order: func() core.WorkOrder {
			order := base(core.WorkOrderTimedOut)
			order.NextAutomaticRecoveryAt = due
			return order
		}(), policy: recoveryPolicy(0, window), now: now, want: AutomaticRecoveryNone},
		{name: "ordinary release", task: running, order: func() core.WorkOrder {
			order := base(core.WorkOrderQueued)
			order.RetrySuppressed = true
			return order
		}(), policy: policy, now: now, want: AutomaticRecoveryNone},
	}
	for _, state := range []core.WorkOrderState{core.WorkOrderTimedOut, core.WorkOrderStale} {
		state := state
		wantAct := AutomaticRecoveryRecover
		if state == core.WorkOrderStale {
			wantAct = AutomaticRecoveryRedispatch
		}
		rows = append(rows,
			row{name: string(state) + " unscheduled", task: running, order: base(state), policy: policy, now: now, want: AutomaticRecoverySchedule, at: now.Add(time.Minute), number: 1},
			row{name: string(state) + " future", task: running, order: func() core.WorkOrder {
				order := base(state)
				order.NextAutomaticRecoveryAt = future
				return order
			}(), policy: policy, now: now, want: AutomaticRecoveryNone},
			row{name: string(state) + " due", task: running, order: func() core.WorkOrder {
				order := base(state)
				order.NextAutomaticRecoveryAt = due
				return order
			}(), policy: policy, now: now, want: wantAct, number: 1},
			row{name: string(state) + " at bound", task: running, order: func() core.WorkOrder {
				order := base(state)
				order.AutomaticRecoveryCount = 2
				order.AutomaticRecoveryWindowStartedAt = now.Add(-time.Hour)
				order.NextAutomaticRecoveryAt = due
				return order
			}(), policy: policy, now: now, want: AutomaticRecoveryExhausted, number: 2},
		)
	}
	expired := base(core.WorkOrderQueued)
	expired.LastAttemptOutcome = core.WorkOrderOutcomeExpired
	retryLimit := base(core.WorkOrderQueued)
	retryLimit.RetrySuppressed = true
	retryLimit.RetrySuppressionReason = core.AttemptRetriesExhaustedReason
	lostClaim := base(core.WorkOrderClaimed)
	lostClaim.LeaseExpiresAt = now.Add(-time.Second)
	for _, sample := range []struct {
		name  string
		order core.WorkOrder
	}{{"queued expired", expired}, {"queued retry limit", retryLimit}, {"expired lease claim", lostClaim}} {
		unscheduled := sample.order
		futureOrder := sample.order
		futureOrder.NextAutomaticRecoveryAt = future
		dueOrder := sample.order
		dueOrder.NextAutomaticRecoveryAt = due
		boundOrder := sample.order
		boundOrder.AutomaticRecoveryCount = 2
		boundOrder.AutomaticRecoveryWindowStartedAt = now.Add(-time.Hour)
		boundOrder.NextAutomaticRecoveryAt = due
		rows = append(rows,
			row{name: sample.name + " unscheduled", task: running, order: unscheduled, policy: policy, now: now, want: AutomaticRecoverySchedule, at: now.Add(time.Minute), number: 1},
			row{name: sample.name + " future", task: running, order: futureOrder, policy: policy, now: now, want: AutomaticRecoveryNone},
			row{name: sample.name + " due", task: running, order: dueOrder, policy: policy, now: now, want: AutomaticRecoveryRecover, number: 1},
			row{name: sample.name + " at bound", task: running, order: boundOrder, policy: policy, now: now, want: AutomaticRecoveryExhausted, number: 2},
		)
	}
	rows = append(rows,
		row{name: "delay second", task: running, order: func() core.WorkOrder {
			order := base(core.WorkOrderTimedOut)
			order.AutomaticRecoveryCount = 1
			order.AutomaticRecoveryWindowStartedAt = now.Add(-time.Hour)
			return order
		}(), policy: policy, now: now, want: AutomaticRecoverySchedule, at: now.Add(5 * time.Minute), number: 2},
		row{name: "delay third", task: running, order: func() core.WorkOrder {
			order := base(core.WorkOrderTimedOut)
			order.AutomaticRecoveryCount = 2
			order.AutomaticRecoveryWindowStartedAt = now.Add(-time.Hour)
			return order
		}(), policy: recoveryPolicy(10, window), now: now, want: AutomaticRecoverySchedule, at: now.Add(25 * time.Minute), number: 3},
		row{name: "delay cap", task: running, order: func() core.WorkOrder {
			order := base(core.WorkOrderTimedOut)
			order.AutomaticRecoveryCount = 3
			order.AutomaticRecoveryWindowStartedAt = now.Add(-time.Hour)
			return order
		}(), policy: recoveryPolicy(10, window), now: now, want: AutomaticRecoverySchedule, at: now.Add(time.Hour), number: 4},
		row{name: "window exact reset", task: running, order: func() core.WorkOrder {
			order := base(core.WorkOrderTimedOut)
			order.AutomaticRecoveryCount = 2
			order.AutomaticRecoveryWindowStartedAt = now.Add(-window)
			return order
		}(), policy: policy, now: now, want: AutomaticRecoverySchedule, at: now.Add(time.Minute), number: 1},
		row{name: "window not yet reset", task: running, order: func() core.WorkOrder {
			order := base(core.WorkOrderTimedOut)
			order.AutomaticRecoveryCount = 2
			order.AutomaticRecoveryWindowStartedAt = now.Add(-window + time.Nanosecond)
			order.NextAutomaticRecoveryAt = due
			return order
		}(), policy: policy, now: now, want: AutomaticRecoveryExhausted, number: 2},
		row{name: "bound one due", task: running, order: func() core.WorkOrder {
			order := base(core.WorkOrderTimedOut)
			order.NextAutomaticRecoveryAt = due
			return order
		}(), policy: recoveryPolicy(1, window), now: now, want: AutomaticRecoveryRecover, number: 1},
		row{name: "bound ten at bound", task: running, order: func() core.WorkOrder {
			order := base(core.WorkOrderTimedOut)
			order.AutomaticRecoveryCount = 10
			order.AutomaticRecoveryWindowStartedAt = now.Add(-time.Hour)
			order.NextAutomaticRecoveryAt = due
			return order
		}(), policy: recoveryPolicy(10, window), now: now, want: AutomaticRecoveryExhausted, number: 10},
	)
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			got := DecideAutomaticRecovery(row.policy, row.task, row.order, row.now)
			if got.Kind != row.want || got.Number != row.number || (!row.at.IsZero() && !got.At.Equal(row.at)) {
				t.Fatalf("got kind=%s at=%s number=%d want kind=%s at=%s number=%d", got.Kind, got.At, got.Number, row.want, row.at, row.number)
			}
		})
	}
}

func TestAutomaticRecoveryDelay(t *testing.T) {
	if AutomaticRecoveryDelay(1) != time.Minute || AutomaticRecoveryDelay(2) != 5*time.Minute || AutomaticRecoveryDelay(3) != 25*time.Minute {
		t.Fatalf("series 1m=%s 5m=%s 25m=%s", AutomaticRecoveryDelay(1), AutomaticRecoveryDelay(2), AutomaticRecoveryDelay(3))
	}
	if AutomaticRecoveryDelay(4) != time.Hour || AutomaticRecoveryDelay(10) != time.Hour {
		t.Fatalf("cap n4=%s n10=%s", AutomaticRecoveryDelay(4), AutomaticRecoveryDelay(10))
	}
}

func TestStampAutomaticRecoveryRollsAtWindowEnd(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	window := 24 * time.Hour
	policy := recoveryPolicy(2, window)
	order := core.WorkOrder{AutomaticRecoveryCount: 2, AutomaticRecoveryWindowStartedAt: now.Add(-window)}
	number, start := StampAutomaticRecovery(&order, policy, now)
	if number != 1 || order.AutomaticRecoveryCount != 1 || !start.Equal(now) || !order.AutomaticRecoveryWindowStartedAt.Equal(now) {
		t.Fatalf("at window end number=%d count=%d start=%s window=%s", number, order.AutomaticRecoveryCount, start, order.AutomaticRecoveryWindowStartedAt)
	}
	original := now.Add(-window + time.Nanosecond)
	inside := core.WorkOrder{AutomaticRecoveryCount: 2, AutomaticRecoveryWindowStartedAt: original}
	number, start = StampAutomaticRecovery(&inside, policy, now)
	if number != 3 || inside.AutomaticRecoveryCount != 3 || !start.Equal(original) {
		t.Fatalf("inside window number=%d count=%d start=%s", number, inside.AutomaticRecoveryCount, start)
	}
}

func TestDecideAutomaticRecoveryProperty(t *testing.T) {
	now := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	rng := rand.New(rand.NewPCG(261011, 4))
	states := []core.TaskState{core.TaskQueued, core.TaskRunning, core.TaskClaiming, core.TaskAwaiting, core.TaskApproved, core.TaskParked, core.TaskMerged, core.TaskClosed}
	stages := []core.Stage{core.StageSpec, core.StageImplement, core.StageVerify, core.StageReview}
	for range 200 {
		bound := rng.IntN(11)
		count := rng.IntN(12)
		window := time.Duration(rng.IntN(48)+1) * time.Hour
		started := now.Add(-time.Duration(rng.IntN(72)) * time.Hour)
		state := states[rng.IntN(len(states))]
		stage := stages[rng.IntN(len(stages))]
		policy := recoveryPolicy(bound, window)
		order := core.WorkOrder{State: core.WorkOrderTimedOut, Stage: stage, AutomaticRecoveryCount: count, AutomaticRecoveryWindowStartedAt: started, NextAutomaticRecoveryAt: now.Add(-time.Minute)}
		got := DecideAutomaticRecovery(policy, core.Task{State: state}, order, now)
		if state != core.TaskQueued && state != core.TaskRunning || core.TaskTerminal(state) {
			if got.Kind != AutomaticRecoveryNone {
				t.Fatalf("state %s kind=%s", state, got.Kind)
			}
			continue
		}
		inWindow := now.Before(started.Add(window))
		switch {
		case bound == 0 && got.Kind != AutomaticRecoveryNone:
			t.Fatalf("disabled kind=%s", got.Kind)
		case bound != 0 && !inWindow && got.Kind != AutomaticRecoveryRecover:
			t.Fatalf("expired window stage=%s kind=%s", stage, got.Kind)
		case bound != 0 && inWindow && count >= bound && got.Kind != AutomaticRecoveryExhausted:
			t.Fatalf("bound count=%d bound=%d kind=%s", count, bound, got.Kind)
		case bound != 0 && inWindow && count < bound && got.Kind != AutomaticRecoveryRecover:
			t.Fatalf("inside bound stage=%s kind=%s count=%d bound=%d", stage, got.Kind, count, bound)
		}
	}
}

type configKind = AutomaticRecoveryKind
