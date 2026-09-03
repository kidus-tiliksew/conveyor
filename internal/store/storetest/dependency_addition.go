package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// RunDependencyAdditionConformance verifies the audited post-creation link
// contract against both memory and PostgreSQL stores (REQ-4/AC-4.6).
func RunDependencyAdditionConformance(t *testing.T, st store.Store, ctx context.Context, workspace string) {
	t.Helper()
	ctx = store.WithActor(ctx, store.Actor{ID: "operator-dependency-link", Role: core.ActorHuman})
	now := time.Now().UTC()
	create := func(id string, state core.TaskState) {
		t.Helper()
		if err := st.CreateTask(ctx, core.Task{ID: id, Workspace: workspace, Repo: "conveyor", Branch: "conveyor/" + id, State: state, CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	dependent, dependency, third := "link-dependent", "link-dependency", "link-third"
	create(dependent, core.TaskRunning)
	create(dependency, core.TaskRunning)
	create(third, core.TaskRunning)
	claimedTask := "link-claimed"
	create(claimedTask, core.TaskRunning)
	queuedAt := now.Add(-time.Minute).Truncate(time.Microsecond)
	for _, stage := range []core.Stage{core.StageSpec, core.StageImplement} {
		id := dependent + "-" + string(stage)
		if err := st.CreateJob(ctx, core.Job{ID: id, TaskID: dependent, Stage: stage, State: core.JobPending}); err != nil {
			t.Fatal(err)
		}
		if err := For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: id, JobID: id, TaskID: dependent, Stage: stage, QueueEnteredAt: queuedAt, QueueDeadline: queuedAt.Add(time.Hour), CreatedAt: queuedAt}); err != nil {
			t.Fatal(err)
		}
	}
	claimedOrderID := claimedTask + "-implement"
	if err := st.CreateJob(ctx, core.Job{ID: claimedOrderID, TaskID: claimedTask, Stage: core.StageImplement, State: core.JobPending}); err != nil {
		t.Fatal(err)
	}
	if err := For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: claimedOrderID, JobID: claimedOrderID, TaskID: claimedTask, Stage: core.StageImplement, QueueEnteredAt: queuedAt, QueueDeadline: queuedAt.Add(time.Hour), CreatedAt: queuedAt}); err != nil {
		t.Fatal(err)
	}
	claimedBefore, err := For(st).ClaimWorkOrder(ctx, claimedOrderID, core.WorkOrderClaim{SessionID: "link-claimed-session", ClientToken: "link-claimed-token", Lease: time.Minute, ExecutionTimeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}

	request := store.DependencyAdditionRequest{TaskID: dependent, DependsOnTaskID: dependency, Reason: "deliver dependency first", RequestID: "link-request-1"}
	result, err := st.AddTaskDependency(ctx, request)
	if err != nil || !result.Added || len(result.Task.Dependencies) != 1 || result.Task.Dependencies[0].ID != dependency {
		t.Fatalf("add result=%+v err=%v", result, err)
	}
	implementOrder, err := st.GetWorkOrder(ctx, dependent+"-implement")
	if err != nil || implementOrder.QueueBlockedAt.IsZero() || implementOrder.Claimable || !implementOrder.QueueEnteredAt.Equal(queuedAt) {
		t.Fatalf("blocked implement order=%+v err=%v", implementOrder, err)
	}
	planOrder, err := st.GetWorkOrder(ctx, dependent+"-spec")
	if err != nil || !planOrder.QueueBlockedAt.IsZero() {
		t.Fatalf("plan order changed=%+v err=%v", planOrder, err)
	}
	if _, err = st.AddTaskDependency(ctx, store.DependencyAdditionRequest{TaskID: claimedTask, DependsOnTaskID: dependency, Reason: "link after claim", RequestID: "link-claimed-request"}); err != nil {
		t.Fatal(err)
	}
	claimedAfter, err := st.GetWorkOrder(ctx, claimedOrderID)
	if err != nil || claimedAfter.State != core.WorkOrderClaimed || claimedAfter.SessionID != claimedBefore.SessionID || !claimedAfter.LeaseExpiresAt.Equal(claimedBefore.LeaseExpiresAt) {
		t.Fatalf("claimed order changed before=%+v after=%+v err=%v", claimedBefore, claimedAfter, err)
	}
	blockers, err := st.ListDependencyBlockers(ctx, []string{dependent})
	if err != nil || len(blockers[dependent].BlockingTaskIDs) != 1 || blockers[dependent].BlockingTaskIDs[0] != dependency {
		t.Fatalf("blockers=%+v err=%v", blockers, err)
	}
	retry, err := st.AddTaskDependency(ctx, request)
	if err != nil || !retry.Added {
		t.Fatalf("idempotent retry=%+v err=%v", retry, err)
	}
	changed := request
	changed.Reason = "changed"
	if _, err = st.AddTaskDependency(ctx, changed); !errors.Is(err, store.ErrTaskDependencyConflict) {
		t.Fatalf("changed request error=%v", err)
	}
	existing, err := st.AddTaskDependency(ctx, store.DependencyAdditionRequest{TaskID: dependent, DependsOnTaskID: dependency, Reason: "record existing link", RequestID: "link-existing"})
	if err != nil || existing.Added {
		t.Fatalf("existing edge result=%+v err=%v", existing, err)
	}
	if count, countErr := st.CountEvents(ctx, dependent, "task.dependency_added"); countErr != nil || count != 1 {
		t.Fatalf("dependency events=%d err=%v", count, countErr)
	}
	events, err := st.ListEvents(ctx, dependent)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range events {
		if event.Kind == "task.dependency_added" {
			var payload map[string]any
			if err = json.Unmarshal(event.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			found = event.ActorID == "operator-dependency-link" && payload["task_id"] == dependent &&
				payload["depends_on_task_id"] == dependency && payload["reason"] == request.Reason &&
				payload["request_id"] == request.RequestID
		}
	}
	if !found {
		t.Fatalf("audited dependency event missing: %+v", events)
	}

	if _, err = st.AddTaskDependency(ctx, store.DependencyAdditionRequest{TaskID: dependency, DependsOnTaskID: third, Reason: "middle link", RequestID: "link-request-2"}); err != nil {
		t.Fatal(err)
	}
	if _, err = st.AddTaskDependency(ctx, store.DependencyAdditionRequest{TaskID: third, DependsOnTaskID: dependent, Reason: "close cycle", RequestID: "link-request-cycle"}); !errors.Is(err, store.ErrTaskDependencyCycle) {
		t.Fatalf("cycle error=%v", err)
	}
	if _, err = st.AddTaskDependency(ctx, store.DependencyAdditionRequest{TaskID: dependent, DependsOnTaskID: dependent, Reason: "self", RequestID: "link-request-self"}); !errors.Is(err, store.ErrTaskDependencyConflict) {
		t.Fatalf("self-link error=%v", err)
	}

	terminal := "link-terminal"
	create(terminal, core.TaskClosed)
	if _, err = st.AddTaskDependency(ctx, store.DependencyAdditionRequest{TaskID: dependent, DependsOnTaskID: terminal, Reason: "terminal dependency", RequestID: "link-terminal-dependency"}); !errors.Is(err, store.ErrTaskTerminal) {
		t.Fatalf("terminal dependency error=%v", err)
	}
	if _, err = st.AddTaskDependency(ctx, store.DependencyAdditionRequest{TaskID: terminal, DependsOnTaskID: dependency, Reason: "terminal task", RequestID: "link-terminal-task"}); !errors.Is(err, store.ErrTaskTerminal) {
		t.Fatalf("terminal task error=%v", err)
	}
	if _, err = st.AddTaskDependency(ctx, store.DependencyAdditionRequest{TaskID: dependent, DependsOnTaskID: "missing", Reason: "missing", RequestID: "link-missing"}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing dependency error=%v", err)
	}
}
