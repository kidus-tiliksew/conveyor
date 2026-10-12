package storetest

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

func runAutomaticRecoveryProperty(t *testing.T, st store.Store, ctx context.Context, workspace string) {
	t.Helper()
	restoreTimeout := setWorkOrderQueueTimeout(t, st, ctx, time.Minute)
	defer restoreTimeout()
	bound := config.DefaultAutomaticRecoveries
	states := []core.TaskState{core.TaskClaiming, core.TaskQueued, core.TaskRunning, core.TaskAwaiting, core.TaskApproved, core.TaskParked}
	stages := []core.Stage{core.StageSpec, core.StageImplement, core.StageVerify, core.StageReview}
	conditions := []string{"timed_out", "stale", "expired", "retry_limit", "claimed_lease"}
	type tracked struct {
		id       string
		taskID   string
		eligible bool
		label    string
		before   core.WorkOrder
	}
	var orders []tracked
	for _, state := range states {
		for _, stage := range stages {
			for _, condition := range conditions {
				if state == core.TaskApproved && (condition == "expired" || condition == "retry_limit" || condition == "claimed_lease") {
					continue
				}
				id, taskID := seedPropertyOrder(t, st, ctx, workspace, state, stage, true)
				applyPropertyCondition(t, st, ctx, id, condition)
				eligible := state == core.TaskQueued || state == core.TaskRunning
				orders = append(orders, tracked{id: id, taskID: taskID, eligible: eligible, label: string(state) + "/" + string(stage) + "/" + condition})
			}
		}
	}
	excluded := []struct {
		id    string
		label string
	}{
		{seedLiveClaim(t, st, ctx, workspace), "live_claim"},
		{seedCheckpointOrder(t, st, ctx, workspace), "checkpoint"},
		{seedOrdinaryRelease(t, st, ctx, workspace), "ordinary_release"},
		{seedOtherSuppression(t, st, ctx, workspace), "other_suppression"},
		{seedVerifyRefusal(t, st, ctx, workspace), "recovery_refusal"},
	}
	far := time.Now().UTC().Add(48 * time.Hour)
	for i := range excluded {
		got, err := st.GetWorkOrder(ctx, excluded[i].id)
		if err != nil {
			t.Fatal(err)
		}
		got.QueueDeadline = far
		if err = UpdateWorkOrder(ctx, st, got); err != nil {
			t.Fatal(err)
		}
	}
	for _, item := range excluded {
		got, err := st.GetWorkOrder(ctx, item.id)
		if err != nil {
			t.Fatal(err)
		}
		orders = append(orders, tracked{id: item.id, taskID: got.TaskID, label: item.label})
	}
	for i := range orders {
		got, err := st.GetWorkOrder(ctx, orders[i].id)
		if err != nil {
			t.Fatal(err)
		}
		orders[i].before = got
	}
	now := time.Now().UTC().Add(2 * time.Second)
	for step := 0; step < 40; step++ {
		if _, err := taskops.New(st).TickOrderClock(ctx, now); err != nil {
			t.Fatal(err)
		}
		next := time.Time{}
		pending := false
		for _, item := range orders {
			if !item.eligible {
				continue
			}
			got, err := st.GetWorkOrder(ctx, item.id)
			if err != nil {
				t.Fatal(err)
			}
			if got.RetrySuppressionReason == core.AutomaticRecoveryExhaustedReason {
				continue
			}
			pending = true
			recoverableNow := got.State == core.WorkOrderTimedOut || got.State == core.WorkOrderStale ||
				(got.State == core.WorkOrderQueued && (got.LastAttemptOutcome == core.WorkOrderOutcomeExpired || (got.RetrySuppressed && got.RetrySuppressionReason == core.AttemptRetriesExhaustedReason))) ||
				(got.State == core.WorkOrderClaimed && !got.LeaseExpiresAt.IsZero() && !got.LeaseExpiresAt.After(now))
			var due time.Time
			switch {
			case !got.NextAutomaticRecoveryAt.IsZero() && got.NextAutomaticRecoveryAt.After(now):
				due = got.NextAutomaticRecoveryAt
			case recoverableNow:
				due = now.Add(time.Second)
			case got.State == core.WorkOrderQueued && !got.QueueDeadline.IsZero() && got.QueueDeadline.After(now):
				due = got.QueueDeadline.Add(time.Second)
			default:
				due = now.Add(time.Second)
			}
			if next.IsZero() || due.Before(next) {
				next = due
			}
		}
		if !pending {
			break
		}
		if !next.After(now) {
			next = now.Add(time.Second)
		}
		now = next
	}
	for _, item := range orders {
		got, err := st.GetWorkOrder(ctx, item.id)
		if err != nil {
			t.Fatal(err)
		}
		acts, exhausted, exhaustedAt := automaticRecoveryOutcome(t, st, ctx, item.taskID)
		if !item.eligible {
			if got.AutomaticRecoveryCount != item.before.AutomaticRecoveryCount || !got.AutomaticRecoveryWindowStartedAt.Equal(item.before.AutomaticRecoveryWindowStartedAt) || !got.NextAutomaticRecoveryAt.Equal(item.before.NextAutomaticRecoveryAt) || got.RetrySuppressionReason != item.before.RetrySuppressionReason || acts != 0 || exhausted != 0 {
				t.Fatalf("excluded %s changed count %d->%d next %s->%s reason %q->%q acts=%d exhausted=%d state %s->%s", item.label, item.before.AutomaticRecoveryCount, got.AutomaticRecoveryCount, item.before.NextAutomaticRecoveryAt.Format(time.RFC3339), got.NextAutomaticRecoveryAt.Format(time.RFC3339), item.before.RetrySuppressionReason, got.RetrySuppressionReason, acts, exhausted, item.before.State, got.State)
			}
			continue
		}
		if got.AutomaticRecoveryCount > bound || acts > bound || exhausted != 1 || (acts > bound && exhausted > 0) {
			t.Fatalf("%s both or over bound count=%d acts=%d exhausted=%d bound=%d order=%+v", item.label, got.AutomaticRecoveryCount, acts, exhausted, bound, got)
		}
		if !exhaustedAt.IsZero() {
			for _, event := range listAutomaticRecoveryEvents(t, st, ctx, item.taskID) {
				if event.At.After(exhaustedAt) {
					t.Fatalf("%s recovered after exhaustion", item.label)
				}
			}
		}
	}
}

func seedPropertyOrder(t *testing.T, st store.Store, ctx context.Context, workspace string, state core.TaskState, stage core.Stage, admit bool) (string, string) {
	t.Helper()
	now := time.Now().UTC()
	taskID := core.NewTaskID()
	task := core.Task{ID: taskID, Workspace: workspace, Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + taskID, State: state, NextStage: stage, CreatedAt: now}
	if stage == core.StageVerify && admit {
		task.SetupContract.VerifyStage = true
		task.ReviewedHeadSHA = "head"
	}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	id := taskID + "-" + string(stage)
	if err := st.CreateJob(ctx, core.Job{ID: id, TaskID: taskID, Stage: stage, State: core.JobPending}); err != nil {
		t.Fatal(err)
	}
	order := core.WorkOrder{ID: id, TaskID: taskID, JobID: id, Stage: stage, State: core.WorkOrderQueued, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour)}
	if stage == core.StageVerify && admit {
		order.HeadSHA = "head"
	}
	if err := CreateWorkOrder(ctx, st, order); err != nil {
		t.Fatal(err)
	}
	return id, taskID
}

func applyPropertyCondition(t *testing.T, st store.Store, ctx context.Context, id, condition string) {
	t.Helper()
	switch condition {
	case "timed_out":
		if _, err := ClaimWorkOrder(ctx, st, id, core.WorkOrderClaim{WorkerID: "worker", ClaimantID: "worker", SessionID: "timeout-" + id, ClientToken: "timeout-" + id, Lease: time.Hour, ExecutionTimeout: time.Millisecond}); err != nil {
			t.Fatal(err)
		}
		if _, err := taskops.New(st).TickOrderClock(ctx, time.Now().UTC().Add(time.Second)); err != nil {
			t.Fatal(err)
		}
	case "stale":
		persisted, err := st.GetWorkOrder(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if err = UpdateWorkOrder(ctx, st, persisted, core.WorkOrderCmdMarkStale); err != nil {
			t.Fatal(err)
		}
	case "expired":
		if _, err := ClaimWorkOrder(ctx, st, id, core.WorkOrderClaim{WorkerID: "worker", ClaimantID: "worker", SessionID: "expire-" + id, ClientToken: "expire-" + id, Lease: time.Millisecond, ExecutionTimeout: time.Hour}); err != nil {
			t.Fatal(err)
		}
		if _, err := taskops.New(st).TickOrderClock(ctx, time.Now().UTC().Add(time.Second)); err != nil {
			t.Fatal(err)
		}
	case "retry_limit":
		claimAndRelease(t, st, ctx, id, "limit-"+id, core.WorkOrderRelease{SessionID: "limit-" + id, Outcome: core.WorkOrderOutcomeChildFailure, Reason: "once-" + id, FailureDetail: "once", AutomaticRetryLimit: 1, InitialRetryDelay: time.Nanosecond, MaximumRetryDelay: time.Nanosecond})
		time.Sleep(time.Millisecond)
		if _, err := ClaimWorkOrder(ctx, st, id, core.WorkOrderClaim{WorkerID: "worker", ClaimantID: "worker", SessionID: "limit-2-" + id, ClientToken: "limit-2-" + id, Lease: time.Hour, ExecutionTimeout: time.Hour}); err != nil {
			t.Fatal(err)
		}
		released, err := ReleaseWorkerClaim(ctx, st, id, "worker", core.WorkOrderRelease{SessionID: "limit-2-" + id, Outcome: core.WorkOrderOutcomeChildFailure, Reason: "twice-" + id, FailureDetail: "twice", AutomaticRetryLimit: 1})
		if err != nil || released.RetrySuppressionReason != core.AttemptRetriesExhaustedReason {
			t.Fatalf("retry limit %s=%+v err=%v", id, released, err)
		}
	case "claimed_lease":
		if _, err := ClaimWorkOrder(ctx, st, id, core.WorkOrderClaim{WorkerID: "worker", ClaimantID: "worker", SessionID: "lease-" + id, ClientToken: "lease-" + id, Lease: time.Millisecond, ExecutionTimeout: time.Hour}); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unknown condition %s", condition)
	}
}

func seedLiveClaim(t *testing.T, st store.Store, ctx context.Context, workspace string) string {
	t.Helper()
	id := seedRunningOrder(t, st, ctx, workspace)
	if _, err := ClaimWorkOrder(ctx, st, id, core.WorkOrderClaim{WorkerID: "worker", ClaimantID: "worker", SessionID: "live-property", ClientToken: "live-property", Lease: 48 * time.Hour, ExecutionTimeout: 48 * time.Hour}); err != nil {
		t.Fatal(err)
	}
	return id
}

func seedCheckpointOrder(t *testing.T, st store.Store, ctx context.Context, workspace string) string {
	t.Helper()
	id := seedRunningOrder(t, st, ctx, workspace)
	claimAndRelease(t, st, ctx, id, "checkpoint-property", core.WorkOrderRelease{SessionID: "checkpoint-property", Reason: core.WorkOrderReleaseReasonOperatorCheckpointReached, Outcome: core.WorkOrderOutcomeReleased, Checkpoint: &core.WorkOrderCheckpoint{DecisionRequest: "hold"}})
	return id
}

func seedOrdinaryRelease(t *testing.T, st store.Store, ctx context.Context, workspace string) string {
	t.Helper()
	id := seedRunningOrder(t, st, ctx, workspace)
	claimAndRelease(t, st, ctx, id, "ordinary-property", core.WorkOrderRelease{SessionID: "ordinary-property", Reason: "operator release", Outcome: core.WorkOrderOutcomeReleased})
	return id
}

func seedOtherSuppression(t *testing.T, st store.Store, ctx context.Context, workspace string) string {
	t.Helper()
	id := seedRunningOrder(t, st, ctx, workspace)
	claimAndRelease(t, st, ctx, id, "same-property", core.WorkOrderRelease{SessionID: "same-property", Outcome: core.WorkOrderOutcomeChildFailure, Reason: "same", FailureDetail: "same-output", AutomaticRetryLimit: 3, InitialRetryDelay: time.Nanosecond, MaximumRetryDelay: time.Nanosecond})
	time.Sleep(time.Millisecond)
	claimAndRelease(t, st, ctx, id, "same-property-2", core.WorkOrderRelease{SessionID: "same-property-2", Outcome: core.WorkOrderOutcomeChildFailure, Reason: "same", FailureDetail: "same-output", AutomaticRetryLimit: 3})
	return id
}

func seedVerifyRefusal(t *testing.T, st store.Store, ctx context.Context, workspace string) string {
	t.Helper()
	now := time.Now().UTC()
	taskID := core.NewTaskID()
	if err := st.CreateTask(ctx, core.Task{ID: taskID, Workspace: workspace, Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + taskID, State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	id := taskID + "-verify"
	if err := st.CreateJob(ctx, core.Job{ID: id, TaskID: taskID, Stage: core.StageVerify, State: core.JobPending}); err != nil {
		t.Fatal(err)
	}
	if err := CreateWorkOrder(ctx, st, core.WorkOrder{ID: id, TaskID: taskID, JobID: id, Stage: core.StageVerify, State: core.WorkOrderQueued, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), HeadSHA: "head"}); err != nil {
		t.Fatal(err)
	}
	return id
}

func setAutomaticRecoveryBound(t *testing.T, st store.Store, ctx context.Context, bound int) func() {
	t.Helper()
	updater, ok := st.(interface {
		WorkspaceConfig(context.Context) (config.VersionedDocument, error)
		UpdateWorkspaceConfig(context.Context, int64, *config.Config) (config.UpdateReceipt, error)
	})
	if !ok {
		t.Fatal("store does not expose workspace config")
	}
	current, err := updater.WorkspaceConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	previous := current.Document.Recovery
	window := previous.WindowText
	if window == "" {
		window = config.DefaultRecoveryWindowText
	}
	write := func(version int64, recovery config.RecoveryPolicy) {
		t.Helper()
		next := &config.Config{Workspace: current.Document.Workspace, MaxBounces: current.Document.MaxBounces, WorkOrderQueueTimeoutText: current.Document.WorkOrderQueueTimeoutText, Recovery: recovery}
		if _, err := updater.UpdateWorkspaceConfig(ctx, version, next); err != nil {
			t.Fatal(err)
		}
	}
	write(current.Version, config.RecoveryPolicy{AutomaticRecoveries: bound, WindowText: window})
	return func() {
		record, err := updater.WorkspaceConfig(ctx)
		if err != nil {
			t.Fatal(err)
		}
		write(record.Version, previous)
	}
}

type automaticRecoveryEvent struct {
	Kind      string
	Number    int
	RequestID string
	At        time.Time
}

func listAutomaticRecoveryEvents(t *testing.T, st store.Store, ctx context.Context, taskID string) []automaticRecoveryEvent {
	t.Helper()
	events, err := st.ListEvents(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	var out []automaticRecoveryEvent
	for _, event := range events {
		if event.Kind != "work_order.recovered" && event.Kind != "work_order.redispatched" && event.Kind != "work_order.recovery_exhausted" {
			continue
		}
		var payload struct {
			Automatic      bool   `json:"automatic"`
			RecoveryNumber int    `json:"recovery_number"`
			RequestID      string `json:"request_id"`
			Reason         string `json:"reason"`
		}
		if err = json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if event.Kind != "work_order.recovery_exhausted" && !payload.Automatic {
			continue
		}
		out = append(out, automaticRecoveryEvent{Kind: event.Kind, Number: payload.RecoveryNumber, RequestID: payload.RequestID, At: event.At})
	}
	return out
}

func countWorkOrderEvents(t *testing.T, st store.Store, ctx context.Context, taskID, kind string) int {
	t.Helper()
	n := 0
	for _, event := range listAutomaticRecoveryEvents(t, st, ctx, taskID) {
		if event.Kind == kind {
			n++
		}
	}
	return n
}

func automaticRequestIDs(t *testing.T, st store.Store, ctx context.Context, taskID string) []string {
	t.Helper()
	var ids []string
	for _, event := range listAutomaticRecoveryEvents(t, st, ctx, taskID) {
		if event.RequestID != "" {
			ids = append(ids, event.RequestID)
		}
	}
	return ids
}

func automaticRecoveryOutcome(t *testing.T, st store.Store, ctx context.Context, taskID string) (acts, exhausted int, exhaustedAt time.Time) {
	t.Helper()
	for _, event := range listAutomaticRecoveryEvents(t, st, ctx, taskID) {
		switch event.Kind {
		case "work_order.recovery_exhausted":
			exhausted++
			exhaustedAt = event.At
		case "work_order.recovered", "work_order.redispatched":
			acts++
		}
	}
	return acts, exhausted, exhaustedAt
}
