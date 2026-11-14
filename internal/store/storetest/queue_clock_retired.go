package storetest

import (
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/nextaction"
	"github.com/kidus-tiliksew/conveyor/internal/riskpolicy"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// runQueueClockRetired proves on every backend that an order nobody has
// claimed since it entered the queue has no clock: it stays queued and
// claimable in its queue position for any length of time, no clock marks it
// stale, automatic recovery never schedules or counts anything for it, and its
// next action waits for an agent. A claimed order's lease and execution
// deadline behave as before. A stored order that still holds the retired stale
// value returns to queued at the next clock fire, keeping its checkpoint and
// position, with one work_order.redispatched and no recovery counted
// (req-task-lifecycle-and-queue REQ-9, AC-9.4, AC-9.5; DEC-117(1), (2);
// component-work-orders invariants 5 to 8 and 46).
func runQueueClockRetired(t *testing.T, x Fixture) {
	t.Run("UnclaimedOrdersWaitOnEveryStage", func(t *testing.T) { runUnclaimedOrdersWait(t, x) })
	t.Run("ClaimedClocksUnchanged", func(t *testing.T) { runClaimedClocksUnchanged(t, x) })
	t.Run("StoredStaleReturnsToQueue", func(t *testing.T) { runStoredStaleReturnsToQueue(t, x) })
	t.Run("StoredStaleKeepsCheckpoint", func(t *testing.T) { runStoredStaleKeepsCheckpoint(t, x) })
	t.Run("StoredStaleKeepsGenuineFailure", func(t *testing.T) { runStoredStaleKeepsGenuineFailure(t, x) })
	t.Run("StoredStaleConcurrentTicks", func(t *testing.T) { runStoredStaleConcurrentTicks(t, x) })
	t.Run("StoredStaleClaimedBeforeTheClock", func(t *testing.T) { runStoredStaleClaimedBeforeTheClock(t, x) })
}

// runStoredStaleClaimedBeforeTheClock claims a stored stale order before any
// clock fire. Reads offer it as claimable, so the claim returns it to queued
// in the same act and claims it in its queue position, with the one
// work_order.redispatched the clock would record (component-work-orders
// invariant 5; req-task-lifecycle-and-queue AC-9.5).
func runStoredStaleClaimedBeforeTheClock(t *testing.T, x Fixture) {
	order := waitingOrder(t, x, core.StageImplement, nil)
	seedStoredStale(t, x, order, false, time.Time{})
	claimed, err := ClaimWorkOrder(x.Context, x.Backend, order.ID, core.WorkOrderClaim{ClaimantID: "run:stale-owner", SessionID: "stale-claim-" + order.ID, ClientToken: "stale-claim-" + order.ID, Lease: time.Hour, ExecutionTimeout: time.Hour})
	requireOK(t, err)
	if claimed.State != core.WorkOrderClaimed || !claimed.QueueEnteredAt.Equal(order.QueueEnteredAt) || claimed.RedispatchCount != 0 || claimed.AutomaticRecoveryCount != 0 {
		t.Fatalf("claim of a stored stale order=%+v, want it claimed in its queue position", claimed)
	}
	requireOneNormalization(t, x, order)
	if acted := tickTask(t, x, order.TaskID, time.Now().UTC()); acted != 0 {
		t.Fatalf("clock fire after the claim acted %d times", acted)
	}
	requireOneNormalization(t, x, order)
}

// formerQueueAge is how long the waiting orders of this case have waited: two
// days, a day past the 24-hour deadline the retired queue clock stored.
const formerQueueAge = 48 * time.Hour

// waitingOrder files a task at stage and one order of that stage that entered
// the queue formerQueueAge ago, through the ordinary creation command. Its
// stored queue_deadline passed a day ago.
func waitingOrder(t *testing.T, x Fixture, stage core.Stage, mutate func(*core.Task)) core.WorkOrder {
	t.Helper()
	st, ctx := x.Backend, x.Context
	now := time.Now().UTC().Truncate(time.Microsecond)
	task := core.Task{ID: core.NewTaskID(), Workspace: x.Workspace, Repo: "conveyor", BaseBranch: "main", State: core.TaskRunning, NextStage: stage, CreatedAt: now.Add(-formerQueueAge)}
	task.Branch = "conveyor/task-" + task.ID
	if mutate != nil {
		mutate(&task)
	}
	requireOK(t, st.CreateTask(ctx, task))
	id := task.ID + "-" + string(stage) + "-1"
	order := core.WorkOrder{ID: id, TaskID: task.ID, JobID: id, Stage: stage, State: core.WorkOrderQueued, HeadSHA: task.ReviewedHeadSHA,
		QueueEnteredAt: now.Add(-formerQueueAge), QueueDeadline: now.Add(-formerQueueAge / 2), CreatedAt: now.Add(-formerQueueAge)}
	if stage == core.StageReview {
		order.ID, order.JobID, order.ReviewRound, order.ReviewSeat = task.ID+"-review-1-seat-1", task.ID+"-review-1-seat-1", 1, 1
	}
	requireOK(t, st.CreateJob(ctx, core.Job{ID: order.JobID, TaskID: task.ID, Stage: stage, State: core.JobPending}))
	requireOK(t, CreateWorkOrder(ctx, st, order))
	stored, err := st.GetWorkOrder(ctx, order.ID)
	requireOK(t, err)
	return stored
}

// waitingRiskOrder files a record-mode task and one risk order created
// formerQueueAge ago through the risk creation command, with a one-hour
// queue timeout: the risk stage follows the common rule with no special case
// (feature-risk-based-review RR-15).
func waitingRiskOrder(t *testing.T, x Fixture) core.WorkOrder {
	t.Helper()
	st, ctx := x.Backend, x.Context
	now := time.Now().UTC().Truncate(time.Microsecond)
	task := core.Task{ID: core.NewTaskID(), Workspace: x.Workspace, Repo: "conveyor", BaseBranch: "main", State: core.TaskRunning, NextStage: core.StageReview, PolicyVersion: 1, CreatedAt: now.Add(-formerQueueAge)}
	task.Branch = "conveyor/task-" + task.ID
	task.SetupContract = config.PolicyContract{MaxBounces: 3, Risk: &riskpolicy.Policy{Mode: riskpolicy.ModeRecord, Tiers: riskpolicy.DefaultTable()},
		StageTimeouts: config.PolicyStageTimeouts{Spec: "30m", Implement: "4h", Verify: "1h", Review: "45m"}}
	requireOK(t, st.CreateTask(ctx, task))
	order, err := taskops.ExecuteWorkOrder(ctx, st, task.ID, core.WorkOrderCmdCreate, func(lease taskops.TaskLease) (core.WorkOrder, error) {
		created, made, err := st.CreateRiskOrderCommand(ctx, lease, store.RiskOrderCreateRequest{TaskID: task.ID, Subject: core.RiskOrderSubject{HeadSHA: "waiting-head", BaseSHA: "waiting-base"},
			QueueTimeout: time.Hour, Now: now.Add(-formerQueueAge)})
		if err == nil && !made {
			err = errors.New("risk order was not created")
		}
		return created, err
	})
	requireOK(t, err)
	stored, err := st.GetWorkOrder(ctx, order.ID)
	requireOK(t, err)
	if !stored.QueueDeadline.Before(now) {
		t.Fatalf("risk order queue deadline %s is not in the past", stored.QueueDeadline)
	}
	return stored
}

// tickTask runs the order clock for one task at at, as the order clock does.
func tickTask(t *testing.T, x Fixture, taskID string, at time.Time) int {
	t.Helper()
	acted, err := taskops.ExecuteWorkOrder(x.Context, x.Backend, taskID, core.WorkOrderCmdTimeout, func(lease taskops.TaskLease) (int, error) {
		return x.Backend.ApplyWorkOrderClock(x.Context, lease, taskID, at)
	})
	requireOK(t, err)
	return acted
}

func queueClockEvents(t *testing.T, x Fixture, taskID string) map[string][]core.Event {
	t.Helper()
	events, err := x.Backend.ListEvents(x.Context, taskID)
	requireOK(t, err)
	out := map[string][]core.Event{}
	for _, event := range events {
		switch event.Kind {
		case "work_order.stale", "work_order.redispatched", "work_order.recovered", "work_order.recovery_exhausted":
			out[event.Kind] = append(out[event.Kind], event)
		}
	}
	return out
}

// requireWaitingInPlace proves the order is queued and claimable in its
// original queue position with no recovery scheduled, counted, or exhausted.
func requireWaitingInPlace(t *testing.T, x Fixture, label string, before core.WorkOrder) {
	t.Helper()
	got, err := x.Backend.GetWorkOrder(x.Context, before.ID)
	requireOK(t, err)
	if got.State != core.WorkOrderQueued || !got.Claimable || !got.QueueEnteredAt.Equal(before.QueueEnteredAt) || got.RedispatchCount != before.RedispatchCount ||
		got.AutomaticRecoveryCount != 0 || !got.AutomaticRecoveryWindowStartedAt.IsZero() || !got.NextAutomaticRecoveryAt.IsZero() || got.RetrySuppressed {
		t.Fatalf("%s: order=%+v, want it queued and claimable in place with no recovery", label, got)
	}
	if events := queueClockEvents(t, x, before.TaskID); len(events) != 0 {
		t.Fatalf("%s: recorded %v, want no stale, redispatch, recovery, or exhaustion", label, events)
	}
}

func requireWaitingForAgentAction(t *testing.T, x Fixture, label, taskID string, at time.Time) {
	t.Helper()
	action, err := store.ReadNextAction(x.Context, x.Backend, taskID, store.NextActionOptions{Now: at})
	requireOK(t, err)
	if action.Kind != nextaction.KindQueued || action.Actor != nextaction.ActorAgent || action.Judgment || action.Details.StallReason != "" || len(action.Choices) != 0 || action.Deadline != nil || store.TaskNeedsAttention(action) {
		t.Fatalf("%s: next action %s/%s stall=%q choices=%+v deadline=%v attention=%t, want an order waiting for an agent", label, action.Actor, action.Kind, action.Details.StallReason, action.Choices, action.Deadline, store.TaskNeedsAttention(action))
	}
}

func runUnclaimedOrdersWait(t *testing.T, x Fixture) {
	st, ctx := x.Backend, x.Context
	orders := map[string]core.WorkOrder{
		"spec":      waitingOrder(t, x, core.StageSpec, nil),
		"implement": waitingOrder(t, x, core.StageImplement, nil),
		"verify": waitingOrder(t, x, core.StageVerify, func(task *core.Task) {
			task.ReviewedHeadSHA = "verify-head"
			task.SetupContract.VerifyStage = true
		}),
		"review": waitingOrder(t, x, core.StageReview, func(task *core.Task) { task.ReviewedHeadSHA = "review-head" }),
		"risk":   waitingRiskOrder(t, x),
	}
	now := time.Now().UTC()
	for _, at := range []time.Time{now, now.Add(25 * time.Hour), now.Add(30 * 24 * time.Hour)} {
		elapsed, err := st.ListElapsedWorkOrderTaskIDs(ctx, at)
		requireOK(t, err)
		for stage, order := range orders {
			if slices.Contains(elapsed, order.TaskID) {
				t.Fatalf("%s: clock at %s selected the waiting order's task", stage, at)
			}
			if acted := tickTask(t, x, order.TaskID, at); acted != 0 {
				t.Fatalf("%s: clock at %s acted %d times on a waiting order", stage, at, acted)
			}
			requireWaitingInPlace(t, x, stage+" after a tick at "+at.String(), order)
			if deadlines := store.WorkOrderDeadlines(order); len(deadlines) != 0 {
				t.Fatalf("%s: deadlines=%v, want none for a waiting order", stage, deadlines)
			}
		}
	}
	for _, stage := range []string{"spec", "implement"} {
		requireWaitingForAgentAction(t, x, stage, orders[stage].TaskID, now.Add(30*24*time.Hour))
	}
	for _, stage := range []string{"spec", "implement", "risk"} {
		order := orders[stage]
		claimed, err := ClaimWorkOrder(ctx, st, order.ID, core.WorkOrderClaim{ClaimantID: "run:waiting-owner", SessionID: "waiting-" + stage, ClientToken: "waiting-token-" + stage,
			Agent: "claude-code", Model: "model-waiting-" + stage, Lease: time.Hour, ExecutionTimeout: time.Hour})
		requireOK(t, err)
		if claimed.State != core.WorkOrderClaimed || !claimed.QueueEnteredAt.Equal(order.QueueEnteredAt) || claimed.RedispatchCount != 0 {
			t.Fatalf("%s: claim=%+v, want the order claimed in its queue position", stage, claimed)
		}
	}
}

// runClaimedClocksUnchanged proves that a claim of an order past its former
// queue deadline keeps the claim clocks: an elapsed lease returns the order
// to queued with outcome expired, and an elapsed execution deadline times it
// out (component-work-orders invariants 6 to 8; DEC-117(1)).
func runClaimedClocksUnchanged(t *testing.T, x Fixture) {
	st, ctx := x.Backend, x.Context
	leased := waitingOrder(t, x, core.StageImplement, nil)
	if _, err := ClaimWorkOrder(ctx, st, leased.ID, core.WorkOrderClaim{ClaimantID: "worker", SessionID: "lease-" + leased.ID, ClientToken: "lease-" + leased.ID, Lease: time.Millisecond, ExecutionTimeout: time.Hour}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	if acted := tickTask(t, x, leased.TaskID, time.Now().UTC()); acted == 0 {
		t.Fatal("clock did not persist the elapsed lease")
	}
	expired, err := st.GetWorkOrder(ctx, leased.ID)
	requireOK(t, err)
	if expired.State != core.WorkOrderQueued || expired.LastAttemptOutcome != core.WorkOrderOutcomeExpired || !expired.RetrySuppressed {
		t.Fatalf("lease expiry=%+v, want queued with outcome expired and retry suppressed", expired)
	}

	timing := waitingOrder(t, x, core.StageImplement, nil)
	if _, err = ClaimWorkOrder(ctx, st, timing.ID, core.WorkOrderClaim{ClaimantID: "worker", SessionID: "deadline-" + timing.ID, ClientToken: "deadline-" + timing.ID, Lease: time.Hour, ExecutionTimeout: time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	if acted := tickTask(t, x, timing.TaskID, time.Now().UTC()); acted == 0 {
		t.Fatal("clock did not persist the elapsed execution deadline")
	}
	timedOut, err := st.GetWorkOrder(ctx, timing.ID)
	requireOK(t, err)
	if timedOut.State != core.WorkOrderTimedOut {
		t.Fatalf("execution deadline=%+v, want timed_out", timedOut)
	}
	if _, err = ClaimWorkOrder(ctx, st, timing.ID, core.WorkOrderClaim{ClaimantID: "worker", SessionID: "deadline-again", ClientToken: "deadline-again", Lease: time.Hour, ExecutionTimeout: time.Hour}); !errors.Is(err, store.ErrWorkOrderTimedOut) {
		t.Fatalf("claim of a timed-out order err=%v, want ErrWorkOrderTimedOut", err)
	}
}

// seedStoredStale writes the retired stale value on one stored order. It is
// the only hand-set state in this case: no command writes stale any more
// (component-work-orders invariant 5).
func seedStoredStale(t *testing.T, x Fixture, order core.WorkOrder, exhausted bool, scheduledAt time.Time) {
	t.Helper()
	if x.SeedStoredStaleWorkOrder == nil {
		t.Fatal("conformance fixture must provide SeedStoredStaleWorkOrder for the stored stale cases (component-work-orders invariant 5)")
	}
	x.SeedStoredStaleWorkOrder(t, x.Context, order.ID, exhausted, scheduledAt)
	if stored := storedOrder(t, x, order); stored.State != core.WorkOrderStale {
		t.Fatalf("seed did not store order %s as stale: %+v", order.ID, stored)
	}
}

// requireOneNormalization proves exactly one work_order.redispatched from the
// stale value to queued, naming the retired queue clock, and no recovery.
func requireOneNormalization(t *testing.T, x Fixture, order core.WorkOrder) {
	t.Helper()
	events := queueClockEvents(t, x, order.TaskID)
	if len(events["work_order.redispatched"]) != 1 || len(events["work_order.recovered"]) != 0 || len(events["work_order.recovery_exhausted"]) != 0 || len(events["work_order.stale"]) != 0 {
		t.Fatalf("events=%v, want exactly one work_order.redispatched", events)
	}
	var payload struct {
		WorkOrderID string `json:"work_order_id"`
		PriorState  string `json:"prior_state"`
		NewState    string `json:"new_state"`
		Reason      string `json:"reason"`
		Automatic   bool   `json:"automatic"`
	}
	requireOK(t, json.Unmarshal(events["work_order.redispatched"][0].Payload, &payload))
	if payload.WorkOrderID != order.ID || payload.PriorState != string(core.WorkOrderStale) || payload.NewState != string(core.WorkOrderQueued) || payload.Reason != store.LegacyStaleNormalizationReason || payload.Automatic {
		t.Fatalf("redispatched payload=%+v", payload)
	}
}

func storedOrder(t *testing.T, x Fixture, order core.WorkOrder) core.WorkOrder {
	t.Helper()
	stored, err := x.Backend.ListTaskWorkOrdersStored(x.Context, order.TaskID)
	requireOK(t, err)
	for _, candidate := range stored {
		if candidate.ID == order.ID {
			return candidate
		}
	}
	t.Fatalf("order %s missing from the stored read", order.ID)
	return core.WorkOrder{}
}

// runStoredStaleReturnsToQueue seeds a queue-only stale order that also holds
// the age-only exhaustion and schedule the retired clock left. Reads already
// present it as waiting; the next clock fire returns it to queued in place,
// clears the age-only signals, counts no recovery, and records one
// work_order.redispatched; repeated fires change nothing.
func runStoredStaleReturnsToQueue(t *testing.T, x Fixture) {
	st, ctx := x.Backend, x.Context
	order := waitingOrder(t, x, core.StageImplement, nil)
	now := time.Now().UTC()
	seedStoredStale(t, x, order, true, now.Add(time.Hour))
	seeded := storedOrder(t, x, order)

	projected, err := st.GetWorkOrder(ctx, order.ID)
	requireOK(t, err)
	if projected.State != core.WorkOrderQueued || !projected.Claimable || projected.RetrySuppressed || !projected.NextAutomaticRecoveryAt.IsZero() {
		t.Fatalf("projected stored stale order=%+v, want it read as waiting", projected)
	}
	if events := queueClockEvents(t, x, order.TaskID); len(events) != 0 {
		t.Fatalf("a read wrote %v", events)
	}
	requireWaitingForAgentAction(t, x, "before the clock fire", order.TaskID, now)
	elapsed, err := st.ListElapsedWorkOrderTaskIDs(ctx, now)
	requireOK(t, err)
	if !slices.Contains(elapsed, order.TaskID) {
		t.Fatalf("clock candidates=%v, missing the stored stale order's task", elapsed)
	}

	if acted := tickTask(t, x, order.TaskID, now); acted != 1 {
		t.Fatalf("first clock fire acted %d times, want 1", acted)
	}
	normalized := storedOrder(t, x, order)
	if normalized.State != core.WorkOrderQueued || !normalized.QueueEnteredAt.Equal(order.QueueEnteredAt) || normalized.RedispatchCount != seeded.RedispatchCount ||
		normalized.AutomaticRecoveryCount != seeded.AutomaticRecoveryCount || normalized.RetrySuppressed || normalized.RetrySuppressionReason != "" || !normalized.NextAutomaticRecoveryAt.IsZero() {
		t.Fatalf("normalized=%+v seeded=%+v, want queued in place with the age-only signals cleared and no recovery counted", normalized, seeded)
	}
	requireOneNormalization(t, x, order)
	for _, at := range []time.Time{now, now.Add(25 * time.Hour)} {
		if acted := tickTask(t, x, order.TaskID, at); acted != 0 {
			t.Fatalf("repeated clock fire at %s acted %d times", at, acted)
		}
		elapsed, err = st.ListElapsedWorkOrderTaskIDs(ctx, at)
		requireOK(t, err)
		if slices.Contains(elapsed, order.TaskID) {
			t.Fatalf("clock at %s still selects the normalized order's task", at)
		}
	}
	requireOneNormalization(t, x, order)
	requireWaitingForAgentAction(t, x, "after the clock fire", order.TaskID, now.Add(25*time.Hour))
	claimed, err := ClaimWorkOrder(ctx, st, order.ID, core.WorkOrderClaim{ClaimantID: "run:normalized-owner", SessionID: "normalized-" + order.ID, ClientToken: "normalized-" + order.ID, Lease: time.Hour, ExecutionTimeout: time.Hour})
	requireOK(t, err)
	if claimed.State != core.WorkOrderClaimed || !claimed.QueueEnteredAt.Equal(order.QueueEnteredAt) {
		t.Fatalf("claim after normalization=%+v", claimed)
	}
}

// runStoredStaleKeepsCheckpoint seeds the stale value on an order released at
// an operator checkpoint. The clock returns it to queued with its checkpoint
// and queue position, and the unanswered question stays the operator's
// (component-work-orders invariant 5; DEC-117(3)).
func runStoredStaleKeepsCheckpoint(t *testing.T, x Fixture) {
	st, ctx := x.Backend, x.Context
	order := waitingOrder(t, x, core.StageImplement, nil)
	claimed, err := ClaimWorkOrder(ctx, st, order.ID, core.WorkOrderClaim{ClaimantID: "worker", SessionID: "checkpoint-" + order.ID, ClientToken: "checkpoint-" + order.ID, Lease: time.Hour, ExecutionTimeout: time.Hour})
	requireOK(t, err)
	_, err = ReleaseWorkerClaim(ctx, st, order.ID, claimed.WorkerID, core.WorkOrderRelease{SessionID: claimed.SessionID, Reason: core.WorkOrderReleaseReasonOperatorCheckpointReached,
		Cause: core.WorkOrderReleaseCauseOperatorAction, Outcome: core.WorkOrderOutcomeReleased, Checkpoint: &core.WorkOrderCheckpoint{DecisionRequest: "Which branch should the fix target?"}})
	requireOK(t, err)
	released := storedOrder(t, x, order)
	seedStoredStale(t, x, order, false, time.Time{})
	if acted := tickTask(t, x, order.TaskID, time.Now().UTC()); acted != 1 {
		t.Fatalf("clock fire acted %d times, want 1", acted)
	}
	normalized := storedOrder(t, x, order)
	if normalized.State != core.WorkOrderQueued || normalized.Checkpoint == nil || normalized.Checkpoint.DecisionRequest != "Which branch should the fix target?" ||
		!normalized.QueueEnteredAt.Equal(released.QueueEnteredAt) || normalized.LastAttemptOutcome != released.LastAttemptOutcome || normalized.AutomaticRecoveryCount != 0 || !normalized.NextAutomaticRecoveryAt.IsZero() {
		t.Fatalf("normalized=%+v released=%+v, want queued with the checkpoint and position kept", normalized, released)
	}
	requireOneNormalization(t, x, order)
	action, err := store.ReadNextAction(ctx, st, order.TaskID, store.NextActionOptions{Now: time.Now().UTC()})
	requireOK(t, err)
	if action.Kind != nextaction.KindAgentQuestion || action.Actor != nextaction.ActorOperator {
		t.Fatalf("next action %s/%s, want the unanswered agent question", action.Actor, action.Kind)
	}
}

// runStoredStaleKeepsGenuineFailure seeds the stale value, with an
// automatic-recovery exhaustion, on an order whose claim lease expired. That
// exhaustion belongs to a genuine attempt failure, so the clock returns the
// order to queued and keeps the expired outcome, the exhaustion, and the
// operator stop (DEC-117(1), (3); component-work-orders invariant 48).
func runStoredStaleKeepsGenuineFailure(t *testing.T, x Fixture) {
	st, ctx := x.Backend, x.Context
	order := waitingOrder(t, x, core.StageImplement, nil)
	if _, err := ClaimWorkOrder(ctx, st, order.ID, core.WorkOrderClaim{ClaimantID: "worker", SessionID: "genuine-" + order.ID, ClientToken: "genuine-" + order.ID, Lease: time.Millisecond, ExecutionTimeout: time.Hour}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	tickTask(t, x, order.TaskID, time.Now().UTC())
	expired := storedOrder(t, x, order)
	if expired.LastAttemptOutcome != core.WorkOrderOutcomeExpired {
		t.Fatalf("lease expiry=%+v", expired)
	}
	seedStoredStale(t, x, order, true, time.Time{})
	if acted := tickTask(t, x, order.TaskID, time.Now().UTC()); acted != 1 {
		t.Fatalf("clock fire acted %d times, want 1", acted)
	}
	normalized := storedOrder(t, x, order)
	if normalized.State != core.WorkOrderQueued || normalized.LastAttemptOutcome != core.WorkOrderOutcomeExpired || !normalized.RetrySuppressed ||
		normalized.RetrySuppressionReason != core.AutomaticRecoveryExhaustedReason || !normalized.QueueEnteredAt.Equal(expired.QueueEnteredAt) {
		t.Fatalf("normalized=%+v, want queued with the expired attempt and its exhaustion kept", normalized)
	}
	requireOneNormalization(t, x, order)
	action, err := store.ReadNextAction(ctx, st, order.TaskID, store.NextActionOptions{Now: time.Now().UTC()})
	requireOK(t, err)
	if action.Kind != nextaction.KindStalled || action.Actor != nextaction.ActorOperator || !store.TaskNeedsAttention(action) {
		t.Fatalf("next action %s/%s stall=%q, want the operator stop of the genuine failure", action.Actor, action.Kind, action.Details.StallReason)
	}
}

// runStoredStaleConcurrentTicks fires the clock for one stored stale order
// from several goroutines at once. The state change and its event persist
// together exactly once (component-work-orders invariants 5 and 47).
func runStoredStaleConcurrentTicks(t *testing.T, x Fixture) {
	order := waitingOrder(t, x, core.StageImplement, nil)
	seedStoredStale(t, x, order, false, time.Time{})
	now := time.Now().UTC()
	const ticks = 4
	var wg sync.WaitGroup
	results := make(chan error, ticks)
	acts := make(chan int, ticks)
	for range ticks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			acted, err := taskops.ExecuteWorkOrder(x.Context, x.Backend, order.TaskID, core.WorkOrderCmdTimeout, func(lease taskops.TaskLease) (int, error) {
				return x.Backend.ApplyWorkOrderClock(x.Context, lease, order.TaskID, now)
			})
			results <- err
			acts <- acted
		}()
	}
	wg.Wait()
	close(results)
	close(acts)
	succeeded := 0
	for err := range results {
		if err == nil {
			succeeded++
		} else if !errors.Is(err, store.ErrRetryable) {
			t.Fatalf("concurrent tick err=%v", err)
		}
	}
	total := 0
	for acted := range acts {
		total += acted
	}
	if succeeded == 0 || total != 1 {
		t.Fatalf("concurrent ticks succeeded=%d acted=%d, want one transition", succeeded, total)
	}
	if normalized := storedOrder(t, x, order); normalized.State != core.WorkOrderQueued || !normalized.QueueEnteredAt.Equal(order.QueueEnteredAt) {
		t.Fatalf("normalized=%+v", normalized)
	}
	requireOneNormalization(t, x, order)
}
