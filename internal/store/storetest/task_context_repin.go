package storetest

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// RunTaskContextQueueRepinConformance verifies the append-only design pin
// refresh shared by the memory and PostgreSQL implement queue-entry paths.
func RunTaskContextQueueRepinConformance(t *testing.T, st store.Store, ctx context.Context, cfg *config.Config) {
	t.Helper()
	now := time.Now().UTC()
	designID := "design-repin-" + core.NewTaskID()
	design, first, err := st.CreateSystemDesign(ctx, core.SystemDesign{ID: designID, Title: "Queue authority", Category: "Architecture"}, core.SystemDesignVersion{
		Content: designContent("v1"), Origin: core.SystemDesignOriginOperator,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmSystemDesignVersion(ctx, design.ID, first.Version); err != nil {
		t.Fatal(err)
	}

	createTask := func(label string) core.Task {
		t.Helper()
		id := label + "-" + core.NewTaskID()
		task := core.Task{ID: id, Workspace: cfg.Workspace, Repo: cfg.Repos[0].Name, Branch: "conveyor/" + id, State: core.TaskRunning, CreatedAt: now}
		if createErr := st.CreateTaskWithDependenciesAndContext(ctx, task, nil, store.TaskContextInput{DesignIDs: []string{design.ID}}); createErr != nil {
			t.Fatal(createErr)
		}
		return task
	}
	createOrder := func(task core.Task, label string, mutate func(*core.WorkOrder)) core.WorkOrder {
		t.Helper()
		id := task.ID + "-" + label
		job := core.Job{ID: id, TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
		if createErr := st.CreateJob(ctx, job); createErr != nil {
			t.Fatal(createErr)
		}
		order := core.WorkOrder{ID: id, TaskID: task.ID, JobID: id, Stage: core.StageImplement, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now}
		if mutate != nil {
			mutate(&order)
		}
		if createErr := For(st).CreateWorkOrder(ctx, order); createErr != nil {
			t.Fatal(createErr)
		}
		return order
	}
	assertPin := func(taskID string, want int) {
		t.Helper()
		context, contextErr := store.TaskContextForTask(ctx, st, taskID)
		if contextErr != nil {
			t.Fatal(contextErr)
		}
		if len(context.Designs) != 1 || context.Designs[0].ID != design.ID || context.Designs[0].Version != want {
			t.Fatalf("task %s design context=%+v, want %s v%d", taskID, context.Designs, design.ID, want)
		}
	}
	countPins := func(taskID string) int {
		t.Helper()
		events, listErr := st.ListEvents(ctx, taskID)
		if listErr != nil {
			t.Fatal(listErr)
		}
		count := 0
		for _, event := range events {
			if event.Kind == store.TaskContextDesignAdded {
				count++
			}
		}
		return count
	}

	dispatchTask := createTask("dispatch-repin")
	redispatchTask := createTask("redispatch-repin")
	redispatchOrder := createOrder(redispatchTask, "implement-1", func(order *core.WorkOrder) {
		order.QueueEnteredAt = now.Add(-2 * time.Hour)
		order.QueueDeadline = now.Add(-time.Hour)
	})
	recoveryTask := createTask("recovery-repin")
	recoveryOrder := createOrder(recoveryTask, "implement-1", func(order *core.WorkOrder) {
		order.LastAttemptOutcome = core.WorkOrderOutcomeChildFailure
		order.RetrySuppressed = true
	})
	second, err := st.ProposeSystemDesignVersion(ctx, core.SystemDesignVersion{DocumentID: design.ID, Content: designContent("v2"), Origin: core.SystemDesignOriginOperator})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmSystemDesignVersion(ctx, design.ID, second.Version, first.Version); err != nil {
		t.Fatal(err)
	}
	createOrder(dispatchTask, "implement-1", nil)
	assertPin(dispatchTask.ID, second.Version)
	if got := countPins(dispatchTask.ID); got != 2 {
		t.Fatalf("dispatch pin events=%d, want 2", got)
	}
	createOrder(dispatchTask, "implement-2", nil)
	if got := countPins(dispatchTask.ID); got != 2 {
		t.Fatalf("already-current dispatch pin events=%d, want 2", got)
	}
	pending, err := st.ProposeSystemDesignVersion(ctx, core.SystemDesignVersion{DocumentID: design.ID, Content: designContent("v3"), Origin: core.SystemDesignOriginImplementation, OriginTaskID: dispatchTask.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err = st.AppendEvent(ctx, core.Event{TaskID: dispatchTask.ID, Kind: store.TaskContextDesignAdded, At: now, Payload: core.JSONPayload(map[string]any{"id": design.ID, "version": pending.Version, "unconfirmed": true})}); err != nil {
		t.Fatal(err)
	}
	createOrder(dispatchTask, "implement-3", nil)
	assertPin(dispatchTask.ID, pending.Version)
	if got := countPins(dispatchTask.ID); got != 3 {
		t.Fatalf("newer pending pin events=%d, want 3 (no downgrade append)", got)
	}

	if _, err = For(st).RedispatchWorkOrder(ctx, redispatchOrder.ID, time.Hour); err != nil {
		t.Fatal(err)
	}
	assertPin(redispatchTask.ID, second.Version)

	if _, err = For(st).RecoverWorkOrder(ctx, recoveryOrder.ID, "recover-repin", time.Hour); err != nil {
		t.Fatal(err)
	}
	assertPin(recoveryTask.ID, second.Version)

	reviewID := redispatchTask.ID + "-review-1"
	if err = st.CreateJob(ctx, core.Job{ID: reviewID, TaskID: redispatchTask.ID, Stage: core.StageReview, State: core.JobPending}); err != nil {
		t.Fatal(err)
	}
	if err = For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: reviewID, TaskID: redispatchTask.ID, JobID: reviewID, Stage: core.StageReview, ReviewRound: 1, ReviewSeat: 1, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	governance, err := store.GovernanceForTask(ctx, st, redispatchTask.ID, redispatchTask.Repo)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := For(st).ClaimWorkOrder(ctx, reviewID, core.WorkOrderClaim{SessionID: "review-repin", ClientToken: "review-repin-secret", Lease: time.Minute, ExecutionTimeout: time.Hour, Governance: &governance})
	if err != nil {
		t.Fatal(err)
	}
	if claimed.GovernanceSnapshot == nil || len(claimed.GovernanceSnapshot.Designs) != 1 || claimed.GovernanceSnapshot.Designs[0].Version != second.Version {
		t.Fatalf("review governance snapshot=%+v, want %s v%d", claimed.GovernanceSnapshot, design.ID, second.Version)
	}
}

func designContent(version string) string {
	return fmt.Sprintf("# Queue authority %s\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/store/**\n```", version)
}
