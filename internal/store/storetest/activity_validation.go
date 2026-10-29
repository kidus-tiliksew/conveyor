package storetest

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// RunActivityValidationConformance proves the Board validation read and the
// one-selection input read on every backend (component-persistence, Board
// activity validation reads): a stable result for unchanged state, a changed
// result after each change kind, isolation from another workspace, and
// inputs equal to the separate marker and order reads.
func RunActivityValidationConformance(t *testing.T, x Fixture) {
	st, ws := x.Backend, x.Workspace
	ctx := store.WithActor(x.Context, store.Actor{ID: requirementConformanceActor, Role: core.ActorHuman})
	// The instant sits a minute ahead of the backend's own write stamps, so
	// no creation or update time passes between two reads.
	now := time.Now().UTC().Truncate(time.Microsecond).Add(time.Minute)
	read := func(at time.Time) store.ActivityValidation {
		t.Helper()
		validation, err := st.ReadActivityValidation(ctx, at)
		if err != nil {
			t.Fatal(err)
		}
		return validation
	}
	newTask := func(state core.TaskState) string {
		t.Helper()
		id := core.NewTaskID()
		if err := st.CreateTask(ctx, core.Task{ID: id, Workspace: ws, Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + id, Title: "Activity " + id, Body: "activity body", State: state, NextStage: core.StageImplement, CreatedAt: now.Add(-time.Hour)}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	newOrder := func(taskID string, stage core.Stage, deadline time.Time) string {
		t.Helper()
		id := taskID + "-" + string(stage) + "-1"
		if err := st.CreateJob(ctx, core.Job{ID: id, TaskID: taskID, Stage: stage, State: core.JobPending}); err != nil {
			t.Fatal(err)
		}
		order := core.WorkOrder{ID: id, TaskID: taskID, JobID: id, Stage: stage, State: core.WorkOrderQueued, QueueEnteredAt: now.Add(-time.Hour), QueueDeadline: deadline}
		if stage == core.StageReview {
			// A queued review order whose claim lapsed keeps its start time.
			order.ExecutionStartedAt = now.Add(-time.Minute)
		}
		if err := CreateWorkOrder(ctx, st, order); err != nil {
			t.Fatal(err)
		}
		return id
	}
	taskA := newTask(core.TaskQueued)
	orderA := newOrder(taskA, core.StageImplement, now.Add(24*time.Hour))
	if _, version, err := st.CreateRequirement(ctx, core.Requirement{ID: "req-activity-validation", Title: "Activity validation"}, core.RequirementVersion{Content: "# v1", Origin: core.RequirementOriginOperator, Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "First."}}}); err != nil {
		t.Fatal(err)
	} else if _, _, err = st.ConfirmRequirementVersion(ctx, "req-activity-validation", version.Version); err != nil {
		t.Fatal(err)
	}

	current := read(now)
	if again := read(now); again != current {
		t.Fatalf("unchanged state read %+v then %+v", current, again)
	}
	if current.EventCount == 0 || current.EventMaxID == 0 {
		t.Fatalf("validation counts no events: %+v", current)
	}
	expectChange := func(name string, change func()) {
		t.Helper()
		change()
		next := read(now)
		if next == current {
			t.Fatalf("%s left the validation unchanged: %+v", name, next)
		}
		if again := read(now); again != next {
			t.Fatalf("%s: unchanged state read %+v then %+v", name, next, again)
		}
		current = next
	}
	expectChange("new event", func() {
		if err := st.AppendEvent(ctx, core.Event{TaskID: taskA, Kind: "task.updated", At: now.Add(-time.Minute)}); err != nil {
			t.Fatal(err)
		}
	})
	expectChange("older event appended later", func() {
		if err := st.AppendEvent(ctx, core.Event{TaskID: taskA, Kind: "task.updated", At: now.Add(-48 * time.Hour)}); err != nil {
			t.Fatal(err)
		}
	})
	expectChange("new task", func() { newTask(core.TaskQueued) })
	expectChange("task change", func() {
		if err := st.UpdateTaskClassification(ctx, taskA, "bugfix"); err != nil {
			t.Fatal(err)
		}
	})
	claim := core.WorkOrderClaim{SessionID: "activity-session", ClientToken: "activity-secret", ClaimantID: "activity-worker", WorkerID: "activity-worker", Lease: time.Hour}
	expectChange("work-order transition", func() {
		if _, err := For(st).ClaimWorkOrder(ctx, orderA, claim); err != nil {
			t.Fatal(err)
		}
	})
	expectChange("lease renewal", func() {
		if _, err := For(st).RenewWorkerClaim(ctx, orderA, claim.WorkerID, claim.SessionID, 2*time.Hour); err != nil {
			t.Fatal(err)
		}
	})
	var proposed core.RequirementVersion
	expectChange("requirement proposal", func() {
		var err error
		if proposed, err = st.ProposeRequirementVersion(ctx, core.RequirementVersion{RequirementID: "req-activity-validation", Content: "# v2", Origin: core.RequirementOriginOperator, Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Second."}}}); err != nil {
			t.Fatal(err)
		}
	})
	expectChange("requirement confirmation", func() {
		if _, _, err := st.ConfirmRequirementVersion(ctx, "req-activity-validation", proposed.Version); err != nil {
			t.Fatal(err)
		}
	})
	var design core.SystemDesignVersion
	expectChange("system design proposal", func() {
		var err error
		if _, design, err = st.CreateSystemDesign(ctx, core.SystemDesign{ID: "component-activity-validation", Title: "Activity validation", Category: "Component design"}, core.SystemDesignVersion{Content: designContent("activity validation"), Origin: core.SystemDesignOriginOperator}); err != nil {
			t.Fatal(err)
		}
	})
	expectChange("system design confirmation", func() {
		if _, _, err := st.ConfirmSystemDesignVersion(ctx, "component-activity-validation", design.Version); err != nil {
			t.Fatal(err)
		}
	})
	var decision core.Decision
	expectChange("decision proposal", func() {
		var err error
		if decision, err = st.ProposeDecision(ctx, core.Decision{Statement: "Activity validation decision.", Context: "Activity fixture.", AlternativesRejected: "None.", Origin: core.DecisionOriginOperator}); err != nil {
			t.Fatal(err)
		}
	})
	expectChange("decision confirmation", func() {
		if _, err := st.ConfirmDecision(ctx, decision.ID); err != nil {
			t.Fatal(err)
		}
	})
	expectChange("task context proposal", func() {
		if _, _, err := st.ProposeTaskContext(ctx, core.TaskContextProposalInput{TaskID: taskA, TargetKind: core.TaskContextProposalRequirement, TargetID: "req-activity-validation", Source: core.TaskContextProposalPlanning, Justification: "Activity validation."}); err != nil {
			t.Fatal(err)
		}
	})
	expectChange("task context confirmation", func() {
		if _, err := st.ConfirmTaskContextProposal(ctx, taskA, core.TaskContextProposalRequirement, "req-activity-validation"); err != nil {
			t.Fatal(err)
		}
	})
	ownerCtx, _ := bootstrapOwner(t, x)
	var assignee core.IdentityUser
	expectChange("role binding", func() {
		var err error
		if assignee, err = st.ProvisionIdentityUser(ownerCtx, "activity-assignee@example.test", "Activity Assignee"); err != nil {
			t.Fatal(err)
		}
		if _, err = st.GrantWorkspaceRole(ownerCtx, assignee.Email, ws, core.WorkspaceRoleExecutor); err != nil {
			t.Fatal(err)
		}
	})
	expectChange("assignee change", func() {
		if _, err := taskops.New(st).SetAssignee(ownerCtx, taskA, assignee.ID); err != nil {
			t.Fatal(err)
		}
	})
	expectChange("dependency edge", func() {
		id := core.NewTaskID()
		if err := st.CreateTaskWithDependencies(ctx, core.Task{ID: id, Workspace: ws, Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + id, Title: "Dependent " + id, State: core.TaskQueued, NextStage: core.StageImplement, CreatedAt: now.Add(-time.Hour)}, []string{taskA}); err != nil {
			t.Fatal(err)
		}
	})

	// A work-order deadline passing changes only the deadline count.
	deadlineTask := newTask(core.TaskQueued)
	newOrder(deadlineTask, core.StageImplement, now.Add(30*time.Second))
	before, after := read(now), read(now.Add(31*time.Second))
	if after.PassedDeadlines != before.PassedDeadlines+1 || after.Signature != before.Signature || after.EventCount != before.EventCount {
		t.Fatalf("deadline passing: before %+v after %+v", before, after)
	}
	current = before

	// Another workspace's writes never change this workspace's result.
	foreign := ws + "-activity-other"
	foreignCtx := store.WithWorkspace(store.WithActor(context.WithoutCancel(ctx), store.SystemActor()), foreign)
	if _, err := st.BootstrapWorkspaceConfig(foreignCtx, &config.Config{Workspace: foreign, Repos: []config.Repo{{Name: "conveyor", URL: "https://example.test/conveyor", Base: "main"}}}); err != nil {
		t.Fatal(err)
	}
	foreignTask := core.NewTaskID()
	if err := st.CreateTask(foreignCtx, core.Task{ID: foreignTask, Workspace: foreign, Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + foreignTask, Title: "Foreign", State: core.TaskQueued, NextStage: core.StageImplement, CreatedAt: now.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendEvent(foreignCtx, core.Event{TaskID: foreignTask, Kind: "task.updated", At: now.Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if isolated := read(now); isolated != current {
		t.Fatalf("another workspace changed the validation: %+v then %+v", current, isolated)
	}

	// The input read returns the separate reads' markers and orders from one
	// selection, and the review lease horizon of a queued review order.
	reviewTask := newTask(core.TaskRunning)
	reviewOrder := newOrder(reviewTask, core.StageReview, now.Add(24*time.Hour))
	lease := now.Add(45 * time.Second)
	if err := st.AppendEvent(ctx, core.Event{TaskID: reviewTask, JobID: reviewOrder, Kind: "work_order.claimed", At: now.Add(-time.Minute), Payload: core.JSONPayload(map[string]any{"id": reviewOrder, "stage": "review", "lease_expires_at": lease})}); err != nil {
		t.Fatal(err)
	}
	ids := []string{taskA, deadlineTask, reviewTask}
	inputs, err := st.ReadActivityInputs(ctx, ids, now)
	if err != nil {
		t.Fatal(err)
	}
	markers, err := st.ListActivityMarkersForTasks(ctx, ids)
	if err != nil {
		t.Fatal(err)
	}
	orders, err := st.ListWorkOrdersForTasks(ctx, ids)
	if err != nil {
		t.Fatal(err)
	}
	markerKey := func(markers []store.ActivityMarker) []string {
		out := make([]string, 0, len(markers))
		for _, marker := range markers {
			out = append(out, marker.TaskID+"/"+string(marker.LatestStage)+"/"+marker.LastEventAt.UTC().Format(time.RFC3339Nano))
		}
		slices.Sort(out)
		return out
	}
	orderKey := func(orders []core.WorkOrder) []string {
		out := make([]string, 0, len(orders))
		for _, order := range orders {
			out = append(out, order.ID+"/"+string(order.State))
		}
		slices.Sort(out)
		return out
	}
	if got, want := markerKey(inputs.Markers), markerKey(markers); !slices.Equal(got, want) {
		t.Fatalf("input markers = %v, want %v", got, want)
	}
	if got, want := orderKey(inputs.Orders), orderKey(orders); !slices.Equal(got, want) || len(got) != 3 {
		t.Fatalf("input orders = %v, want %v", got, want)
	}
	if !inputs.ValidUntil.Equal(lease) {
		t.Fatalf("review lease horizon = %s, want %s", inputs.ValidUntil, lease)
	}
	if empty, err := st.ReadActivityInputs(ctx, []string{}, now); err != nil || len(empty.Markers) != 0 || len(empty.Orders) != 0 || !empty.ValidUntil.IsZero() {
		t.Fatalf("empty input read = %+v, %v", empty, err)
	}
}
