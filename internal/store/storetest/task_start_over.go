package storetest

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

func runTaskStartOver(t *testing.T, factory Factory) {
	t.Run("pull_request_close", func(t *testing.T) { runPullRequestClose(t, factory) })
	for _, withPlan := range []bool{false, true} {
		t.Run(map[bool]string{false: "without_plan", true: "with_plan"}[withPlan], func(t *testing.T) {
			x := factory.fresh(t, nil)
			st, ctx := x.Backend, store.WithActor(x.Context, store.Actor{ID: "restart-operator", Role: core.ActorHuman})
			newTask := func() core.Task {
				return core.Task{ID: core.NewTaskID(), Workspace: x.Workspace, Source: "test", Title: "Restart fixture", Body: "Preserve this task body", Repo: "conveyor", BaseBranch: "main", State: core.TaskRunning, NextStage: core.StageImplement, SpecApproval: true, Hold: true, PolicyVersion: 7, SetupContract: x.Config.FreezePolicy(), CreatedAt: time.Now().UTC()}
			}
			old := newTask()
			old.Branch = "conveyor/task-" + old.ID
			open, merged := newTask(), newTask()
			open.Branch = "conveyor/task-" + open.ID
			merged.Branch = "conveyor/task-" + merged.ID
			for _, task := range []core.Task{open, merged} {
				if err := st.CreateTask(ctx, task); err != nil {
					t.Fatal(err)
				}
			}
			req, _, err := st.CreateRequirement(ctx, core.Requirement{ID: "req-" + core.NewTaskID(), Title: "Restart requirement"}, core.RequirementVersion{Content: "# Baseline", Origin: core.RequirementOriginOperator, Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Keep context."}}})
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err = st.ConfirmRequirementVersion(ctx, req.ID, 1); err != nil {
				t.Fatal(err)
			}
			design, _, err := st.CreateSystemDesign(ctx, core.SystemDesign{ID: "design-" + core.NewTaskID(), Title: "Restart design", Category: "Architecture"}, core.SystemDesignVersion{Content: "# Baseline\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/**\n```", Origin: core.SystemDesignOriginOperator})
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err = st.ConfirmSystemDesignVersion(ctx, design.ID, 1); err != nil {
				t.Fatal(err)
			}
			if err = st.CreateTaskWithDependenciesAndContext(ctx, old, []string{open.ID, merged.ID}, store.TaskContextInput{RequirementIDs: []string{req.ID}, DesignIDs: []string{design.ID}}); err != nil {
				t.Fatal(err)
			}
			if _, err = taskops.New(st).Perform(ctx, merged.ID, taskops.Command{Kind: core.TaskMergeRecover}); err != nil {
				t.Fatal(err)
			}
			updated, err := st.ProposeRequirementVersion(ctx, core.RequirementVersion{RequirementID: req.ID, Content: "# Current", Origin: core.RequirementOriginOperator, Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Keep current context."}}})
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err = st.ConfirmRequirementVersion(ctx, req.ID, updated.Version); err != nil {
				t.Fatal(err)
			}
			newer, err := st.ProposeSystemDesignVersion(ctx, core.SystemDesignVersion{DocumentID: design.ID, Content: "# Newer design\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/**\n```", Origin: core.SystemDesignOriginOperator})
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err = st.ConfirmSystemDesignVersion(ctx, design.ID, newer.Version); err != nil {
				t.Fatal(err)
			}
			ownReq, err := st.ProposeRequirementVersion(ctx, core.RequirementVersion{RequirementID: req.ID, Content: "# Task proposal", Origin: core.RequirementOriginImplementation, OriginTaskID: old.ID, Statements: updated.Statements})
			if err != nil {
				t.Fatal(err)
			}
			ownDesign, err := st.ProposeSystemDesignVersion(ctx, core.SystemDesignVersion{DocumentID: design.ID, Content: "# Task design proposal\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/**\n```", Origin: core.SystemDesignOriginImplementation, OriginTaskID: old.ID})
			if err != nil {
				t.Fatal(err)
			}
			otherReq, err := st.ProposeRequirementVersion(ctx, core.RequirementVersion{RequirementID: req.ID, Content: "# Other task proposal", Origin: core.RequirementOriginImplementation, OriginTaskID: open.ID, Statements: updated.Statements})
			if err != nil {
				t.Fatal(err)
			}
			job := core.Job{ID: old.ID + "-spec-1", TaskID: old.ID, Stage: core.StageSpec, State: core.JobPending}
			order := core.WorkOrder{ID: job.ID, JobID: job.ID, TaskID: old.ID, Stage: job.Stage}
			if _, err := For(st).CreateStageWorkOrder(ctx, job, order); err != nil {
				t.Fatal(err)
			}
			if withPlan {
				spec, err := st.CreateSpecVersion(ctx, core.SpecVersion{TaskID: old.ID, Content: "## Approved previous approach", Acceptance: json.RawMessage(`[]`), Decomposition: json.RawMessage(`[]`)})
				if err != nil {
					t.Fatal(err)
				}
				if err = st.ApproveSpecVersion(ctx, old.ID, spec.Version); err != nil {
					t.Fatal(err)
				}
			}
			request := core.TaskStartOverRequest{TaskID: old.ID, RequestID: "restart-1", Reason: "Try a fresh approach", Note: "Keep the API small."}
			before, _ := st.ListEvents(ctx, old.ID)
			if _, err = taskops.New(st).StartOver(ctx, request); !errors.Is(err, store.ErrStartOverConfirmDocuments) {
				t.Fatalf("missing capability: %v", err)
			}
			after, _ := st.ListEvents(ctx, old.ID)
			if len(before) != len(after) {
				t.Fatal("capability refusal changed events")
			}
			request.CanConfirmDocuments = true
			got, err := taskops.New(st).StartOver(ctx, request)
			if err != nil {
				t.Fatal(err)
			}
			if !got.Created || got.Task.State != core.TaskClosed || got.Task.SupersededBy != got.Successor.ID || got.Successor.Supersedes != old.ID || got.Successor.Branch == old.Branch || got.Successor.State != core.TaskQueued || got.Successor.NextStage != core.StageTriage {
				t.Fatalf("result=%+v", got)
			}
			if got.Successor.Body != old.Body || got.Successor.Source != old.Source || got.Successor.Repo != old.Repo || got.Successor.BaseBranch != old.BaseBranch || got.Successor.SpecApproval != old.SpecApproval || got.Successor.PolicyVersion != old.PolicyVersion || !reflect.DeepEqual(got.Successor.SetupContract, old.SetupContract) {
				t.Fatal("intake contract was not preserved")
			}
			retired, err := st.GetTask(ctx, old.ID)
			if err != nil || retired.SupersededBy != got.Successor.ID || retired.Branch != old.Branch {
				t.Fatalf("retired=%+v err=%v", retired, err)
			}
			successor, err := st.GetTask(ctx, got.Successor.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(successor.Dependencies) != 1 || successor.Dependencies[0].ID != open.ID {
				t.Fatalf("dependencies=%+v", successor.Dependencies)
			}
			context, err := store.TaskContextForTask(ctx, st, successor.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(context.Requirements) != 1 || context.Requirements[0].Version != updated.Version || len(context.Designs) != 1 || context.Designs[0].Version != 1 {
				t.Fatalf("context=%+v", context)
			}
			dismissedReq, err := st.GetRequirementVersion(ctx, req.ID, ownReq.Version)
			if err != nil || !dismissedReq.Retired || dismissedReq.DismissalNote != request.Note {
				t.Fatalf("requirement=%+v err=%v", dismissedReq, err)
			}
			dismissedDesign, err := st.GetSystemDesignVersion(ctx, design.ID, ownDesign.Version)
			if err != nil || !dismissedDesign.Dismissed || dismissedDesign.DismissalNote != request.Note {
				t.Fatalf("design=%+v err=%v", dismissedDesign, err)
			}
			other, err := st.GetRequirementVersion(ctx, req.ID, otherReq.Version)
			if err != nil || other.Retired {
				t.Fatal("another task's proposal was dismissed")
			}
			cancelled, err := st.GetWorkOrder(ctx, order.ID)
			if err != nil || cancelled.State != core.WorkOrderCancelled {
				t.Fatalf("cancelled=%+v err=%v", cancelled, err)
			}
			artifacts, err := st.ListArtifactsForLineage(ctx, []core.LineageNode{{Type: core.LineageTask, ID: successor.ID}})
			if err != nil {
				t.Fatal(err)
			}
			if withPlan {
				if len(artifacts) != 1 {
					t.Fatalf("artifacts=%+v", artifacts)
				}
				_, content, err := st.GetArtifact(ctx, artifacts[0].ID)
				if err != nil || !strings.Contains(string(content), "reference only") || !strings.Contains(string(content), "Approved previous approach") {
					t.Fatalf("plan=%s err=%v", content, err)
				}
			} else if len(artifacts) != 0 {
				t.Fatal("invented a plan")
			}
			job = core.Job{ID: successor.ID + "-spec-1", TaskID: successor.ID, Stage: core.StageSpec, State: core.JobPending}
			order = core.WorkOrder{ID: job.ID, JobID: job.ID, TaskID: successor.ID, Stage: job.Stage}
			if _, err = For(st).CreateStageWorkOrder(ctx, job, order); err != nil {
				t.Fatal(err)
			}
			first, err := st.GetWorkOrder(ctx, order.ID)
			if err != nil || !strings.Contains(first.OperatorDirection, request.Reason) || !strings.Contains(first.OperatorDirection, request.Note) {
				t.Fatalf("first direction=%q err=%v", first.OperatorDirection, err)
			}
			before, _ = st.ListEvents(ctx, old.ID)
			replay, err := taskops.New(st).StartOver(ctx, request)
			if err != nil || replay.Created || replay.Successor.ID != successor.ID {
				t.Fatalf("replay=%+v err=%v", replay, err)
			}
			after, _ = st.ListEvents(ctx, old.ID)
			if len(before) != len(after) {
				t.Fatal("replay appended events")
			}
			request.Note = "changed"
			if _, err = taskops.New(st).StartOver(ctx, request); !errors.Is(err, store.ErrStartOverRequestConflict) {
				t.Fatalf("conflicting replay=%v", err)
			}
			request.RequestID = "RESTART-1"
			if _, err = taskops.New(st).StartOver(ctx, request); !errors.Is(err, store.ErrTaskTerminal) {
				t.Fatalf("terminal=%v", err)
			}
		})
	}
	t.Run("concurrent_retries", func(t *testing.T) {
		x := factory.fresh(t, nil)
		old := core.Task{ID: core.NewTaskID(), Workspace: x.Workspace, Repo: "conveyor", BaseBranch: "main", Body: "Concurrent restart", Title: "Concurrent", State: core.TaskQueued}
		old.Branch = "conveyor/task-" + old.ID
		if err := x.Backend.CreateTask(x.Context, old); err != nil {
			t.Fatal(err)
		}
		results := make([]core.TaskStartOverResult, 4)
		errs := make([]error, 4)
		var wg sync.WaitGroup
		for i := range results {
			wg.Add(1)
			go func() {
				defer wg.Done()
				results[i], errs[i] = taskops.New(x.Backend).StartOver(x.Context, core.TaskStartOverRequest{TaskID: old.ID, RequestID: "same", Reason: "retry"})
			}()
		}
		wg.Wait()
		created := 0
		for i, result := range results {
			if errs[i] != nil {
				t.Fatal(errs[i])
			}
			if result.Successor.ID != results[0].Successor.ID {
				t.Fatal("duplicate successors")
			}
			if result.Created {
				created++
			}
		}
		if created != 1 {
			t.Fatalf("created=%d", created)
		}
	})
	t.Run("different_requests_race", func(t *testing.T) {
		x := factory.fresh(t, nil)
		old := core.Task{ID: core.NewTaskID(), Workspace: x.Workspace, Repo: "conveyor", BaseBranch: "main", Body: "Race", Title: "Race", State: core.TaskRunning}
		old.Branch = "conveyor/task-" + old.ID
		if err := x.Backend.CreateTask(x.Context, old); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for i := range errs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, errs[i] = taskops.New(x.Backend).StartOver(x.Context, core.TaskStartOverRequest{TaskID: old.ID, RequestID: fmt.Sprintf("request-%d", i), Reason: "retry"})
			}()
		}
		wg.Wait()
		if !((errs[0] == nil && errors.Is(errs[1], store.ErrTaskTerminal)) || (errs[1] == nil && errors.Is(errs[0], store.ErrTaskTerminal))) {
			t.Fatalf("race errors=%v", errs)
		}
	})
	t.Run("concurrent_dependency_addition", func(t *testing.T) {
		x := factory.fresh(t, nil)
		for range 8 {
			old := core.Task{ID: core.NewTaskID(), Workspace: x.Workspace, Repo: "conveyor", BaseBranch: "main", Body: "Dependency race", State: core.TaskRunning}
			old.Branch = "conveyor/task-" + old.ID
			dependency := old
			dependency.ID = core.NewTaskID()
			dependency.Branch = "conveyor/task-" + dependency.ID
			for _, task := range []core.Task{old, dependency} {
				if err := x.Backend.CreateTask(x.Context, task); err != nil {
					t.Fatal(err)
				}
			}
			start := make(chan struct{})
			var wg sync.WaitGroup
			var result core.TaskStartOverResult
			var restartErr, addErr error
			wg.Add(2)
			go func() {
				defer wg.Done()
				<-start
				result, restartErr = taskops.New(x.Backend).StartOver(x.Context, core.TaskStartOverRequest{TaskID: old.ID, RequestID: "restart", Reason: "fresh"})
			}()
			go func() {
				defer wg.Done()
				<-start
				_, addErr = x.Backend.AddTaskDependency(x.Context, store.DependencyAdditionRequest{TaskID: old.ID, DependsOnTaskID: dependency.ID, RequestID: "add-" + old.ID, Reason: "required"})
			}()
			close(start)
			wg.Wait()
			if restartErr != nil || (addErr != nil && !errors.Is(addErr, store.ErrTaskTerminal)) {
				t.Fatalf("restart=%v dependency=%v", restartErr, addErr)
			}
			successor, err := x.Backend.GetTask(x.Context, result.Successor.ID)
			if err != nil {
				t.Fatal(err)
			}
			if addErr == nil && (len(successor.Dependencies) != 1 || successor.Dependencies[0].ID != dependency.ID) {
				t.Fatalf("lost committed dependency: %+v", successor.Dependencies)
			}
			if addErr != nil && len(successor.Dependencies) != 0 {
				t.Fatal("copied refused dependency")
			}
		}
	})

	t.Run("already_decided_and_workspace_isolation", func(t *testing.T) {
		x := factory.fresh(t, nil)
		old := core.Task{ID: core.NewTaskID(), Workspace: x.Workspace, Repo: "conveyor", BaseBranch: "main", Body: "Decided", Title: "Decided", State: core.TaskRunning}
		old.Branch = "conveyor/task-" + old.ID
		if err := x.Backend.CreateTask(x.Context, old); err != nil {
			t.Fatal(err)
		}
		req, pending, err := x.Backend.CreateRequirement(x.Context, core.Requirement{ID: "req-" + core.NewTaskID(), Title: "Decided"}, core.RequirementVersion{Content: "# Decided", Origin: core.RequirementOriginImplementation, OriginTaskID: old.ID, Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Decided requirement."}}})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err = x.Backend.ConfirmRequirementVersion(x.Context, req.ID, pending.Version); err != nil {
			t.Fatal(err)
		}
		request := core.TaskStartOverRequest{TaskID: old.ID, RequestID: "decided", Reason: "retry"}
		if _, err = taskops.New(x.Backend).StartOver(store.WithWorkspace(x.Context, x.Workspace+"-foreign"), request); err == nil {
			t.Fatal("foreign task restarted")
		}
		if _, err = taskops.New(x.Backend).StartOver(x.Context, request); err != nil {
			t.Fatalf("decided proposal blocked restart: %v", err)
		}
	})

	t.Run("rollback_after_cancellation", func(t *testing.T) {
		x := factory.fresh(t, nil)
		st, ctx := x.Backend, store.WithActor(x.Context, store.Actor{ID: "restart-operator", Role: core.ActorHuman})
		old := core.Task{ID: core.NewTaskID(), Workspace: x.Workspace, Repo: "conveyor", BaseBranch: "main", Body: "Rollback", Title: "Rollback", State: core.TaskRunning}
		old.Branch = "conveyor/task-" + old.ID
		if err := st.CreateTask(ctx, old); err != nil {
			t.Fatal(err)
		}
		req, _, err := st.CreateRequirement(ctx, core.Requirement{ID: "req-" + core.NewTaskID(), Title: "Rollback proposal"}, core.RequirementVersion{Content: "# Confirmed", Origin: core.RequirementOriginOperator, Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Keep pending."}}})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err = st.ConfirmRequirementVersion(ctx, req.ID, 1); err != nil {
			t.Fatal(err)
		}
		pending, err := st.ProposeRequirementVersion(ctx, core.RequirementVersion{RequirementID: req.ID, Content: "# Pending", Origin: core.RequirementOriginImplementation, OriginTaskID: old.ID, Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Keep pending."}}})
		if err != nil {
			t.Fatal(err)
		}
		job := core.Job{ID: old.ID + "-implement-1", TaskID: old.ID, Stage: core.StageImplement, State: core.JobPending}
		order := core.WorkOrder{ID: job.ID, JobID: job.ID, TaskID: old.ID, Stage: job.Stage}
		if _, err = For(st).CreateStageWorkOrder(ctx, job, order); err != nil {
			t.Fatal(err)
		}
		beforeOrder, err := st.GetWorkOrder(ctx, order.ID)
		if err != nil || beforeOrder.State == core.WorkOrderCancelled {
			t.Fatalf("work order=%+v err=%v", beforeOrder, err)
		}
		beforeProposal, err := st.GetRequirementVersion(ctx, req.ID, pending.Version)
		if err != nil || beforeProposal.Retired {
			t.Fatalf("proposal=%+v err=%v", beforeProposal, err)
		}
		before, _ := st.ListEvents(ctx, old.ID)
		faultCtx := store.WithTaskStartOverFault(ctx, func(step string) error {
			if step == "successor" {
				return fmt.Errorf("injected successor failure")
			}
			return nil
		})
		if _, err = taskops.New(st).StartOver(faultCtx, core.TaskStartOverRequest{TaskID: old.ID, RequestID: "rollback", Reason: "retry", CanConfirmDocuments: true}); err == nil {
			t.Fatal("injected successor failure accepted")
		}
		after, _ := st.ListEvents(ctx, old.ID)
		got, err := st.GetTask(ctx, old.ID)
		if err != nil || got.State != old.State || got.SupersededBy != "" || len(before) != len(after) {
			t.Fatalf("partial cancellation task=%+v err=%v", got, err)
		}
		afterOrder, err := st.GetWorkOrder(ctx, order.ID)
		if err != nil || afterOrder.State != beforeOrder.State {
			t.Fatalf("work order after rollback=%+v err=%v", afterOrder, err)
		}
		afterProposal, err := st.GetRequirementVersion(ctx, req.ID, pending.Version)
		if err != nil || afterProposal.Retired || afterProposal.DismissalNote != beforeProposal.DismissalNote {
			t.Fatalf("proposal after rollback=%+v err=%v", afterProposal, err)
		}
		tasks, err := st.ListTasks(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, task := range tasks {
			if task.Supersedes == old.ID {
				t.Fatalf("successor persisted=%+v", task)
			}
		}
	})

	t.Run("unavailable_context_dropped", func(t *testing.T) {
		x := factory.fresh(t, nil)
		st, ctx := x.Backend, store.WithActor(x.Context, store.Actor{ID: "restart-operator", Role: core.ActorHuman})
		old := core.Task{ID: core.NewTaskID(), Workspace: x.Workspace, Repo: "conveyor", BaseBranch: "main", Body: "Drop", Title: "Drop", State: core.TaskRunning}
		old.Branch = "conveyor/task-" + old.ID
		keepReq, _, err := st.CreateRequirement(ctx, core.Requirement{ID: "req-keep-restart", Title: "Keep"}, core.RequirementVersion{Content: "# Keep", Origin: core.RequirementOriginOperator, Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Keep confirmed."}}})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err = st.ConfirmRequirementVersion(ctx, keepReq.ID, 1); err != nil {
			t.Fatal(err)
		}
		unconfirmed, _, err := st.CreateRequirement(ctx, core.Requirement{ID: "req-unconfirmed-restart", Title: "Unconfirmed"}, core.RequirementVersion{Content: "# Unconfirmed", Origin: core.RequirementOriginOperator, Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Stay unconfirmed."}}})
		if err != nil {
			t.Fatal(err)
		}
		keepDesign, _, err := st.CreateSystemDesign(ctx, core.SystemDesign{ID: "design-keep-restart", Title: "Keep design", Category: "Architecture"}, core.SystemDesignVersion{Content: "# Keep\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/**\n```", Origin: core.SystemDesignOriginOperator})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err = st.ConfirmSystemDesignVersion(ctx, keepDesign.ID, 1); err != nil {
			t.Fatal(err)
		}
		archived, _, err := st.CreateSystemDesign(ctx, core.SystemDesign{ID: "design-archived-restart", Title: "Archived design", Category: "Architecture"}, core.SystemDesignVersion{Content: "# Archived\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - cmd/**\n```", Origin: core.SystemDesignOriginOperator})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err = st.ConfirmSystemDesignVersion(ctx, archived.ID, 1); err != nil {
			t.Fatal(err)
		}
		if err = st.ArchiveSystemDesign(ctx, archived.ID, "operator", nil); err != nil {
			t.Fatal(err)
		}
		if err = st.CreateTaskWithDependenciesAndContext(ctx, old, nil, store.TaskContextInput{RequirementIDs: []string{keepReq.ID}, DesignIDs: []string{keepDesign.ID}}); err != nil {
			t.Fatal(err)
		}
		for _, event := range []core.Event{
			{TaskID: old.ID, Kind: store.TaskContextRequirementAdded, Payload: core.JSONPayload(map[string]any{"id": "missing-restart-requirement"})},
			{TaskID: old.ID, Kind: store.TaskContextRequirementAdded, Payload: core.JSONPayload(map[string]any{"id": unconfirmed.ID})},
			{TaskID: old.ID, Kind: store.TaskContextDesignAdded, Payload: core.JSONPayload(map[string]any{"id": archived.ID, "version": 1})},
		} {
			if err = st.AppendEvent(ctx, event); err != nil {
				t.Fatal(err)
			}
		}
		result, err := taskops.New(st).StartOver(ctx, core.TaskStartOverRequest{TaskID: old.ID, RequestID: "drop", Reason: "retry"})
		if err != nil {
			t.Fatalf("start over: %v", err)
		}
		wantDrops := []core.TaskStartOverDroppedContext{
			{Kind: "requirement", ID: "missing-restart-requirement", Reason: "missing"},
			{Kind: "requirement", ID: unconfirmed.ID, Reason: "unconfirmed"},
			{Kind: "system design", ID: archived.ID, Reason: "archived"},
		}
		if !result.Created || !reflect.DeepEqual(result.DroppedContext, wantDrops) {
			t.Fatalf("result=%+v want=%+v", result, wantDrops)
		}
		inherited, err := store.TaskContextForTask(ctx, st, result.Successor.ID)
		if err != nil || len(inherited.Requirements) != 1 || inherited.Requirements[0].ID != keepReq.ID || inherited.Requirements[0].Version != 1 || len(inherited.Designs) != 1 || inherited.Designs[0].ID != keepDesign.ID || inherited.Designs[0].Version != 1 {
			t.Fatalf("successor context=%+v err=%v", inherited, err)
		}
		retiredEvents, _ := st.ListEvents(ctx, old.ID)
		for _, event := range retiredEvents {
			if event.Kind == store.TaskStartOverContextDropped {
				t.Fatalf("drop event on retired task: %+v", event)
			}
		}
		successorEvents, _ := st.ListEvents(ctx, result.Successor.ID)
		drops := store.StartOverDroppedContextFromEvents(successorEvents)
		if !reflect.DeepEqual(drops, result.DroppedContext) {
			t.Fatalf("successor drops=%+v", drops)
		}
		replay, err := taskops.New(st).StartOver(ctx, core.TaskStartOverRequest{TaskID: old.ID, RequestID: "drop", Reason: "retry"})
		if err != nil || replay.Created || !reflect.DeepEqual(replay.DroppedContext, result.DroppedContext) {
			t.Fatalf("replay=%+v err=%v", replay, err)
		}
	})

}
