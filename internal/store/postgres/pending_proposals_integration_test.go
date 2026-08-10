package postgres

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/postgres/db"
)

func TestPendingProposalsProjectionIntegration(t *testing.T) {
	st, err := Open(t.Context(), integrationDatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	workspace := "pending-proposals-" + core.NewTaskID()
	ctx := store.WithWorkspace(t.Context(), workspace)
	if _, err = st.BootstrapWorkspaceConfig(ctx, &config.Config{Workspace: workspace, Repos: []config.Repo{{Name: "conveyor", Base: "main"}}}); err != nil {
		t.Fatal(err)
	}
	taskID := core.NewTaskID()
	if err = st.CreateTask(ctx, core.Task{ID: taskID, Workspace: workspace, Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + taskID, State: core.TaskRunning, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	session, err := st.CreatePlanningSession(ctx, core.PlanningSession{ID: "session-" + core.NewTaskID(), Title: "Pending proposal provenance"})
	if err != nil {
		t.Fatal(err)
	}
	design, designVersion, err := st.CreateSystemDesign(ctx, core.SystemDesign{ID: "design-pending", Title: "Pending design", Category: "Architecture"}, core.SystemDesignVersion{
		Content: "# Pending\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/**\n```", Origin: core.SystemDesignOriginImplementation, OriginTaskID: taskID,
	})
	if err != nil {
		t.Fatal(err)
	}
	requirement, requirementVersion, err := st.CreateRequirement(ctx, core.Requirement{ID: "req-pending", Title: "Pending requirement"}, core.RequirementVersion{
		Content: "Pending", Origin: core.RequirementOriginChat, OriginSessionID: session.ID,
		Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Surface pending authority."}},
	})
	if err != nil {
		t.Fatal(err)
	}
	newerRequirementVersion, err := st.ProposeRequirementVersion(ctx, core.RequirementVersion{
		RequirementID: requirement.ID, Content: "Pending newer", Origin: core.RequirementOriginChat, OriginSessionID: session.ID,
		Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Surface pending authority promptly."}},
	})
	if err != nil {
		t.Fatal(err)
	}
	decision, err := st.ProposeDecision(ctx, core.Decision{Statement: "Pending decision", Context: "Projection", AlternativesRejected: "Hidden proposal", Origin: core.DecisionOriginImplementation, OriginTaskID: taskID})
	if err != nil {
		t.Fatal(err)
	}
	items, err := st.ListPendingProposals(ctx)
	if err != nil || len(items) != 4 {
		t.Fatalf("items=%+v err=%v", items, err)
	}
	seen := map[string]bool{}
	for _, item := range items {
		seen[item.Tier] = true
		if item.ID == "" || item.Title == "" || item.ProposedAt.IsZero() {
			t.Fatalf("incomplete item=%+v", item)
		}
	}
	if items[0].OriginID == "" && items[1].OriginID == "" && items[2].OriginID == "" {
		t.Fatalf("origin provenance missing: %+v", items)
	}
	if !seen["system_design"] || !seen["requirement"] || !seen["decision"] {
		t.Fatalf("tiers=%v", seen)
	}
	if _, _, err = st.ConfirmSystemDesignVersion(ctx, design.ID, designVersion.Version); err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmRequirementVersion(ctx, requirement.ID, requirementVersion.Version); err != nil {
		t.Fatal(err)
	}
	items, err = st.ListPendingProposals(ctx)
	newerVisible := false
	for _, item := range items {
		newerVisible = newerVisible || (item.Tier == "requirement" && item.Version == newerRequirementVersion.Version)
	}
	if err != nil || len(items) != 2 || !newerVisible {
		t.Fatalf("newer pending requirement lost after older confirmation: items=%+v err=%v", items, err)
	}
	if _, _, err = st.ConfirmRequirementVersion(ctx, requirement.ID, newerRequirementVersion.Version); err != nil {
		t.Fatal(err)
	}
	if _, err = st.DismissDecision(store.WithActor(ctx, store.Actor{ID: "operator", Role: "operator"}), decision.ID); err != nil {
		t.Fatal(err)
	}
	items, err = st.ListPendingProposals(ctx)
	if err != nil || len(items) != 0 {
		t.Fatalf("resolved items=%+v err=%v", items, err)
	}
}

func TestPendingProposalsProjectionKeepsTwoStatementsAtProductionCardinalityIntegration(t *testing.T) {
	databaseURL := integrationDatabaseURL(t)
	poolConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	recorder := &queryRecorder{}
	poolConfig.ConnConfig.Tracer = recorder
	pool, err := pgxpool.NewWithConfig(t.Context(), poolConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err = Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	st := &Store{pool: pool, queries: db.New(pool)}
	workspace := "pending-scale-" + core.NewTaskID()
	ctx := store.WithWorkspace(t.Context(), workspace)
	if _, err = st.BootstrapWorkspaceConfig(ctx, &config.Config{Workspace: workspace, Repos: []config.Repo{{Name: "conveyor", Base: "main"}}}); err != nil {
		t.Fatal(err)
	}

	attention := phase61Task(workspace, "attention-"+core.NewTaskID(), core.TaskAwaiting, "")
	attention.CreatedAt = time.Now().UTC()
	if err = st.CreateTask(ctx, attention); err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.CreateSystemDesign(ctx, core.SystemDesign{ID: "design-" + core.NewTaskID(), Title: "Pending scale design", Category: "Architecture"}, core.SystemDesignVersion{
		Content: "# Pending\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/**\n```", Origin: core.SystemDesignOriginImplementation, OriginTaskID: attention.ID,
	}); err != nil {
		t.Fatal(err)
	}

	assertProjection := func(label string) {
		t.Helper()
		recorder.reset()
		projection, projectionErr := st.PendingProposalsProjection(ctx)
		if projectionErr != nil {
			t.Fatal(projectionErr)
		}
		if projection.TaskCount != 1 || len(projection.Items) != 1 {
			t.Fatalf("%s projection=%+v", label, projection)
		}
		queries := recorder.snapshot()
		if len(queries) != 2 {
			t.Fatalf("%s query count=%d want 2: %v", label, len(queries), queries)
		}
		if !strings.Contains(queries[1], "WITH pending_origin_tasks") || strings.Contains(queries[1], "served_requirement_snapshot") || strings.Contains(queries[1], "governance_snapshot") {
			t.Fatalf("%s attention query is not the narrow projection: %s", label, queries[1])
		}
	}
	assertProjection("baseline")

	// Reproduce the diagnosed workspace cardinality without making those rows
	// attention-worthy: 198 tasks and 725 completed historical work orders.
	tasks := make([]core.Task, 0, 198)
	tasks = append(tasks, attention)
	for index := 1; index < 198; index++ {
		task := phase61Task(workspace, fmt.Sprintf("scale-task-%03d-%s", index, core.NewTaskID()), core.TaskRunning, "")
		task.CreatedAt = attention.CreatedAt.Add(time.Duration(index) * time.Millisecond)
		if err = st.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
		tasks = append(tasks, task)
	}
	for index := 0; index < 725; index++ {
		task := tasks[index%len(tasks)]
		jobID := fmt.Sprintf("scale-job-%03d-%s", index, core.NewTaskID())
		if err = st.CreateJob(ctx, core.Job{ID: jobID, TaskID: task.ID, Stage: core.StageImplement, State: core.JobDone, StartedAt: attention.CreatedAt}); err != nil {
			t.Fatal(err)
		}
		orderID := fmt.Sprintf("scale-order-%03d-%s", index, core.NewTaskID())
		if _, err = pool.Exec(ctx, `INSERT INTO work_orders
			(id,workspace_id,task_id,job_id,stage,state,queue_entered_at,queue_deadline,created_at,updated_at)
			VALUES ($1,$2,$3,$4,'implement','completed',$5,$6,$5,$5)`,
			orderID, workspace, task.ID, jobID, attention.CreatedAt, attention.CreatedAt.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	assertProjection("198 tasks and 725 historical work orders")
}

func TestPendingProposalsAttentionTruthTableAndWorkspaceIsolationIntegration(t *testing.T) {
	st, err := Open(t.Context(), integrationDatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	workspace := "pending-truth-" + core.NewTaskID()
	ctx := store.WithWorkspace(t.Context(), workspace)
	if _, err = st.BootstrapWorkspaceConfig(ctx, &config.Config{Workspace: workspace, Repos: []config.Repo{{Name: "conveyor", Base: "main"}}}); err != nil {
		t.Fatal(err)
	}
	created := time.Now().UTC()
	createTask := func(name string, state core.TaskState, parent string) core.Task {
		t.Helper()
		task := phase61Task(workspace, name+"-"+core.NewTaskID(), state, parent)
		task.CreatedAt = created
		if createErr := st.CreateTask(ctx, task); createErr != nil {
			t.Fatal(createErr)
		}
		return task
	}
	addOrder := func(task core.Task, stage core.Stage, state core.WorkOrderState, round, seat int, retrySuppressed bool) {
		t.Helper()
		jobID := "truth-job-" + core.NewTaskID()
		if createErr := st.CreateJob(ctx, core.Job{ID: jobID, TaskID: task.ID, Stage: stage, State: core.JobDone, StartedAt: created}); createErr != nil {
			t.Fatal(createErr)
		}
		if _, insertErr := st.pool.Exec(ctx, `INSERT INTO work_orders
			(id,workspace_id,task_id,job_id,stage,state,review_round,review_seat,retry_suppressed,
			 queue_entered_at,queue_deadline,created_at,updated_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$10,$10)`,
			"truth-order-"+core.NewTaskID(), workspace, task.ID, jobID, stage, state, round, seat,
			retrySuppressed, created, created.Add(time.Hour)); insertErr != nil {
			t.Fatal(insertErr)
		}
	}
	proposeFor := func(task core.Task) {
		t.Helper()
		id := "design-" + core.NewTaskID()
		if _, _, createErr := st.CreateSystemDesign(ctx, core.SystemDesign{ID: id, Title: id, Category: "Architecture"}, core.SystemDesignVersion{
			Content: "# Pending\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/**\n```", Origin: core.SystemDesignOriginImplementation, OriginTaskID: task.ID,
		}); createErr != nil {
			t.Fatal(createErr)
		}
	}

	createTask("awaiting", core.TaskAwaiting, "")
	createTask("parked", core.TaskParked, "")
	forge := createTask("forge", core.TaskRunning, "")
	if err = st.AppendEvent(ctx, core.Event{TaskID: forge.ID, Kind: "merge.failed", Payload: core.JSONPayload(map[string]any{"error": "failed"}), At: created}); err != nil {
		t.Fatal(err)
	}
	forgeResolved := createTask("forge-resolved", core.TaskRunning, "")
	for _, event := range []core.Event{
		{TaskID: forgeResolved.ID, Kind: "merge.failed", Payload: core.JSONPayload(map[string]any{"error": "failed"}), At: created},
		{TaskID: forgeResolved.ID, Kind: "merge.reconciled", At: created.Add(time.Second)},
	} {
		if err = st.AppendEvent(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	stalled := createTask("stalled", core.TaskRunning, "")
	addOrder(stalled, core.StageImplement, core.WorkOrderQueued, 0, 0, true)
	terminalStalled := createTask("terminal-stalled", core.TaskMerged, "")
	addOrder(terminalStalled, core.StageImplement, core.WorkOrderQueued, 0, 0, true)
	blueprint := createTask("blueprint", core.TaskAwaiting, "")
	createTask("blueprint-child", core.TaskRunning, blueprint.ID)

	for _, candidate := range []struct {
		name  string
		stage core.Stage
		state core.WorkOrderState
	}{
		{"pending-implement", core.StageImplement, core.WorkOrderSubmitted},
		{"pending-review-queued", core.StageReview, core.WorkOrderQueued},
		{"pending-review-claimed", core.StageReview, core.WorkOrderClaimed},
		{"pending-review-submitted", core.StageReview, core.WorkOrderSubmitted},
	} {
		task := createTask(candidate.name, core.TaskRunning, "")
		proposeFor(task)
		addOrder(task, candidate.stage, candidate.state, 1, 1, false)
	}
	reviewRecovery := createTask("review-recovery", core.TaskRunning, "")
	addOrder(reviewRecovery, core.StageReview, core.WorkOrderTimedOut, 1, 1, false)
	interrupted := createTask("interrupted", core.TaskClosed, "")
	addOrder(interrupted, core.StageReview, core.WorkOrderQueued, 1, 1, true)

	projection, err := st.PendingProposalsProjection(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if projection.TaskCount != 10 || len(projection.Items) != 4 {
		t.Fatalf("projection=%+v want task_count=10 proposals=4", projection)
	}

	sibling := "pending-truth-sibling-" + core.NewTaskID()
	siblingCtx := store.WithWorkspace(t.Context(), sibling)
	if _, err = st.BootstrapWorkspaceConfig(siblingCtx, &config.Config{Workspace: sibling, Repos: []config.Repo{{Name: "conveyor", Base: "main"}}}); err != nil {
		t.Fatal(err)
	}
	siblingTask := phase61Task(sibling, "sibling-"+core.NewTaskID(), core.TaskParked, "")
	siblingTask.CreatedAt = created
	if err = st.CreateTask(siblingCtx, siblingTask); err != nil {
		t.Fatal(err)
	}
	projection, err = st.PendingProposalsProjection(ctx)
	if err != nil || projection.TaskCount != 10 || len(projection.Items) != 4 {
		t.Fatalf("workspace isolation projection=%+v err=%v", projection, err)
	}
}
