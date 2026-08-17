package postgres

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

func TestSiblingReapingSerializesConcurrentClaimIntegration(t *testing.T) {
	databaseURL := integrationDatabaseURL(t)
	st, err := Open(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	workspace := "sibling-claim-race-" + core.NewTaskID()
	ctx := store.WithWorkspace(context.Background(), workspace)
	cfg := &config.Config{Workspace: workspace, WorkOrderQueueTimeout: time.Hour, Repos: []config.Repo{{Name: "conveyor", URL: "https://example.test/conveyor", Base: "main"}}}
	if _, err = st.BootstrapWorkspaceConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}

	t.Run("completion", func(t *testing.T) {
		now := time.Now().UTC().Truncate(time.Microsecond)
		task := core.Task{ID: core.NewTaskID(), Workspace: workspace, Repo: "conveyor", Branch: "conveyor/task-" + core.NewTaskID(), BaseBranch: "main", State: core.TaskRunning, NextStage: core.StageSpec, CreatedAt: now.Add(-time.Hour)}
		if err := st.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
		authoritativeJob := core.Job{ID: task.ID + "-spec-2", TaskID: task.ID, Stage: core.StageSpec, State: core.JobPending}
		siblingJob := core.Job{ID: task.ID + "-spec-1", TaskID: task.ID, Stage: core.StageSpec, State: core.JobPending}
		for _, job := range []core.Job{authoritativeJob, siblingJob} {
			if err := st.CreateJob(ctx, job); err != nil {
				t.Fatal(err)
			}
		}
		authoritative := core.WorkOrder{ID: authoritativeJob.ID, TaskID: task.ID, JobID: authoritativeJob.ID, Stage: core.StageSpec, State: core.WorkOrderQueued, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now}
		sibling := core.WorkOrder{ID: siblingJob.ID, TaskID: task.ID, JobID: siblingJob.ID, Stage: core.StageSpec, State: core.WorkOrderQueued, QueueEnteredAt: now.Add(-time.Minute), QueueDeadline: now.Add(time.Hour), CreatedAt: now.Add(-time.Minute)}
		// Insert the older sibling second so creation itself does not retire the
		// newer authoritative order used to exercise completion.
		if err := storetest.For(st).CreateWorkOrder(ctx, authoritative); err != nil {
			t.Fatal(err)
		}
		if err := storetest.For(st).CreateWorkOrder(ctx, sibling); err != nil {
			t.Fatal(err)
		}
		claimed, err := storetest.For(st).ClaimWorkOrder(ctx, authoritative.ID, core.WorkOrderClaim{SessionID: "authoritative-session", ClientToken: "authoritative-token", ClaimantID: "authoritative-agent", Agent: "codex", Model: "gpt", Lease: time.Minute, ExecutionTimeout: time.Hour})
		if err != nil {
			t.Fatal(err)
		}
		completed := claimed
		completed.State = core.WorkOrderCompleted

		start := make(chan struct{})
		var ready sync.WaitGroup
		ready.Add(2)
		results := make(chan error, 2)
		go func() {
			ready.Done()
			<-start
			results <- storetest.For(st).UpdateWorkOrder(ctx, completed, core.WorkOrderCmdSubmitSpec)
		}()
		go func() {
			ready.Done()
			<-start
			_, claimErr := storetest.For(st).ClaimWorkOrder(ctx, sibling.ID, core.WorkOrderClaim{SessionID: "sibling-session", ClientToken: "sibling-token", ClaimantID: "sibling-agent", Agent: "codex", Model: "gpt", Lease: time.Minute, ExecutionTimeout: time.Hour})
			results <- claimErr
		}()
		ready.Wait()
		close(start)
		firstErr, secondErr := <-results, <-results
		if firstErr == nil && secondErr == nil {
			t.Fatal("completion and competing sibling claim both succeeded")
		}
		persisted, err := st.ListTaskWorkOrders(ctx, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		claimedOrClaimable := 0
		for _, order := range persisted {
			if order.State == core.WorkOrderClaimed || order.Claimable {
				claimedOrClaimable++
			}
		}
		if claimedOrClaimable != 0 {
			t.Fatalf("competing claimed or claimable order survived completion: %+v", persisted)
		}
	})

	t.Run("successor creation", func(t *testing.T) {
		now := time.Now().UTC().Truncate(time.Microsecond)
		task := core.Task{ID: core.NewTaskID(), Workspace: workspace, Repo: "conveyor", Branch: "conveyor/task-" + core.NewTaskID(), BaseBranch: "main", State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: now.Add(-time.Hour)}
		if err := st.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
		oldJob := core.Job{ID: task.ID + "-implement-1", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
		newJob := core.Job{ID: task.ID + "-implement-2", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
		for _, job := range []core.Job{oldJob, newJob} {
			if err := st.CreateJob(ctx, job); err != nil {
				t.Fatal(err)
			}
		}
		oldOrder := core.WorkOrder{ID: oldJob.ID, TaskID: task.ID, JobID: oldJob.ID, Stage: core.StageImplement, State: core.WorkOrderQueued, QueueEnteredAt: now.Add(-time.Minute), QueueDeadline: now.Add(time.Hour), CreatedAt: now.Add(-time.Minute)}
		newOrder := core.WorkOrder{ID: newJob.ID, TaskID: task.ID, JobID: newJob.ID, Stage: core.StageImplement, State: core.WorkOrderQueued, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now}
		if err := storetest.For(st).CreateWorkOrder(ctx, oldOrder); err != nil {
			t.Fatal(err)
		}

		start := make(chan struct{})
		var ready sync.WaitGroup
		ready.Add(2)
		results := make(chan error, 2)
		go func() {
			ready.Done()
			<-start
			_, claimErr := storetest.For(st).ClaimWorkOrder(ctx, oldOrder.ID, core.WorkOrderClaim{SessionID: "old-session", ClientToken: "old-token", ClaimantID: "old-agent", Agent: "codex", Model: "gpt", Lease: time.Minute, ExecutionTimeout: time.Hour})
			results <- claimErr
		}()
		go func() {
			ready.Done()
			<-start
			results <- storetest.For(st).CreateWorkOrder(ctx, newOrder)
		}()
		ready.Wait()
		close(start)
		firstErr, secondErr := <-results, <-results
		if (firstErr == nil) == (secondErr == nil) {
			t.Fatalf("claim/create successes must be mutually exclusive: first=%v second=%v", firstErr, secondErr)
		}
		persisted, err := st.ListTaskWorkOrders(ctx, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		claimedOrClaimable := 0
		for _, order := range persisted {
			if order.State == core.WorkOrderClaimed || order.Claimable {
				claimedOrClaimable++
			}
		}
		if claimedOrClaimable != 1 {
			t.Fatalf("successor race left %d competing claimed/claimable orders: %+v", claimedOrClaimable, persisted)
		}
	})
}

func TestRecoveryRejectsSupersededOrderAndAllowsLatestIntegration(t *testing.T) {
	databaseURL := integrationDatabaseURL(t)
	st, err := Open(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	workspace := "recovery-supersession-" + core.NewTaskID()
	ctx := store.WithWorkspace(context.Background(), workspace)
	cfg := &config.Config{Workspace: workspace, WorkOrderQueueTimeout: time.Hour, Repos: []config.Repo{{Name: "conveyor", URL: "https://example.test/conveyor", Base: "main"}}}
	if _, err = st.BootstrapWorkspaceConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	task := core.Task{ID: core.NewTaskID(), Workspace: workspace, Repo: "conveyor", Branch: "conveyor/task-" + core.NewTaskID(), BaseBranch: "main", State: core.TaskRunning, CreatedAt: now.Add(-time.Hour)}
	if err = st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	successorJob := core.Job{ID: task.ID + "-spec-2", TaskID: task.ID, Stage: core.StageSpec, State: core.JobPending}
	targetJob := core.Job{ID: task.ID + "-spec-1", TaskID: task.ID, Stage: core.StageSpec, State: core.JobPending}
	if err = st.CreateJob(ctx, successorJob); err != nil {
		t.Fatal(err)
	}
	if err = st.CreateJob(ctx, targetJob); err != nil {
		t.Fatal(err)
	}
	successor := core.WorkOrder{ID: successorJob.ID, TaskID: task.ID, JobID: successorJob.ID, Stage: core.StageSpec, State: core.WorkOrderQueued, LastAttemptOutcome: core.WorkOrderOutcomeChildFailure, RetrySuppressed: true, CreatedAt: now, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour)}
	target := core.WorkOrder{ID: targetJob.ID, TaskID: task.ID, JobID: targetJob.ID, Stage: core.StageSpec, State: core.WorkOrderQueued, CreatedAt: now.Add(-time.Hour), QueueEnteredAt: now.Add(-time.Hour), QueueDeadline: now.Add(-time.Minute)}
	if err = storetest.For(st).CreateWorkOrder(ctx, successor); err != nil {
		t.Fatal(err)
	}
	if err = storetest.For(st).CreateWorkOrder(ctx, target); err != nil {
		t.Fatal(err)
	}
	if _, err = storetest.For(st).RecoverWorkOrder(ctx, target.ID, "recover-superseded", time.Hour); err == nil || !strings.Contains(err.Error(), successor.ID) {
		t.Fatalf("superseded recovery err=%v", err)
	}
	if recovered, recoverErr := storetest.For(st).RecoverWorkOrder(ctx, successor.ID, "recover-latest", time.Hour); recoverErr != nil || recovered.State != core.WorkOrderQueued || !recovered.Claimable {
		t.Fatalf("latest recovered=%+v err=%v", recovered, recoverErr)
	}
}

func TestWorkOrderZombieBackfillMigrationRetiresPassedStageAndIsRerunSafeIntegration(t *testing.T) {
	st := newIdentityIntegrationStore(t, 98)
	workspace := "zombie-backfill-" + core.NewTaskID()
	ctx := store.WithWorkspace(t.Context(), workspace)
	cfg := &config.Config{Workspace: workspace, Repos: []config.Repo{{Name: "conveyor", URL: "https://example.test/conveyor", Base: "main"}}}
	if _, err := st.BootstrapWorkspaceConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	task := core.Task{ID: "260815-4804be", Workspace: workspace, Repo: "conveyor", Branch: "conveyor/task-260815-4804be-" + core.NewTaskID(), BaseBranch: "main", State: core.TaskAwaiting, RecoveryStage: core.StageImplement, CreatedAt: now}
	job := core.Job{ID: task.ID + "-spec-1", TaskID: task.ID, Stage: core.StageSpec, State: core.JobPending}
	order := core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageSpec, State: core.WorkOrderQueued, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := storetest.For(st).CreateWorkOrder(ctx, order); err != nil {
		t.Fatal(err)
	}
	if err := migrateControlPlaneToVersion(t.Context(), st.pool, 101); err != nil {
		t.Fatalf("apply migration 101: %v", err)
	}
	var state core.WorkOrderState
	var retirementEvents int
	if err := st.pool.QueryRow(t.Context(), `SELECT state FROM work_orders WHERE workspace_id=$1 AND id=$2`, workspace, order.ID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if err := st.pool.QueryRow(t.Context(), `SELECT count(*) FROM events WHERE workspace_id=$1 AND task_id=$2 AND job_id=$3 AND kind='work_order.retired'`, workspace, task.ID, job.ID).Scan(&retirementEvents); err != nil {
		t.Fatal(err)
	}
	if state != core.WorkOrderCancelled || retirementEvents != 1 {
		t.Fatalf("backfill state=%s retirement_events=%d", state, retirementEvents)
	}
	raw, err := migrationFiles.ReadFile("migrations/101_work_order_zombie_backfill.sql")
	if err != nil {
		t.Fatal(err)
	}
	sql, err := renderMigration(raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.pool.Exec(t.Context(), string(sql)); err != nil {
		t.Fatalf("rerun migration 101 projection: %v", err)
	}
	if err = st.pool.QueryRow(t.Context(), `SELECT count(*) FROM events WHERE workspace_id=$1 AND task_id=$2 AND job_id=$3 AND kind='work_order.retired'`, workspace, task.ID, job.ID).Scan(&retirementEvents); err != nil || retirementEvents != 1 {
		t.Fatalf("retirement events after rerun=%d err=%v", retirementEvents, err)
	}
}
