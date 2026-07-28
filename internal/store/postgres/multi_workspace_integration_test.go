package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func TestMultiWorkspaceIsolationIntegration(t *testing.T) {
	databaseURL := integrationDatabaseURL(t)
	root := context.Background()
	st, err := Open(root, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	suffix := core.NewTaskID()
	ids := []string{"isolation-a-" + suffix, "isolation-b-" + suffix}
	for i, id := range ids {
		cfg := isolationConfig(id)
		if _, err := st.CreateWorkspace(store.WithActor(root, store.Actor{ID: "test", Role: core.ActorHuman}), id, []string{"Alpha ", "Beta "}[i]+suffix, cfg); err != nil {
			t.Fatal(err)
		}
	}
	ctxA, ctxB := store.WithWorkspace(root, ids[0]), store.WithWorkspace(root, ids[1])
	taskA, taskB := isolationTask(ids[0], "a-"+suffix), isolationTask(ids[1], "b-"+suffix)
	taskA.IntakeKey, taskB.IntakeKey = "shared-key", "shared-key"
	if err := st.CreateTask(ctxA, taskA); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateTask(ctxB, taskB); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := st.GetTaskByIntakeKey(ctxA, "shared-key"); err != nil || !ok {
		t.Fatalf("workspace A intake ok=%t err=%v", ok, err)
	}
	if _, err := st.GetTask(ctxA, taskB.ID); err == nil {
		t.Fatal("workspace A read workspace B task")
	}
	if tasks, err := st.ListTasks(ctxB); err != nil || len(tasks) != 1 || tasks[0].ID != taskB.ID {
		t.Fatalf("workspace B tasks=%+v err=%v", tasks, err)
	}
	for _, item := range []struct {
		ctx  context.Context
		task core.Task
	}{
		{ctx: ctxA, task: taskA},
		{ctx: ctxB, task: taskB},
	} {
		lifecycle := core.GitHubLifecycle{TaskID: item.task.ID, Repository: "acme/api", SpecVersion: 1}
		if err := st.QueueGitHubLifecycle(item.ctx, lifecycle); err != nil {
			t.Fatal(err)
		}
		lifecycle, _, _ = st.GetGitHubLifecycle(item.ctx, item.task.ID)
		lifecycle.State, lifecycle.IssueNumber = core.GitHubPublicationPublished, 42
		lifecycle.IssueURL = "https://github.com/acme/api/issues/42"
		if err := st.UpdateGitHubLifecycle(item.ctx, lifecycle); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok, err := st.GetGitHubLifecycle(ctxA, taskB.ID); err != nil || ok {
		t.Fatalf("workspace A lifecycle read workspace B ok=%t err=%v", ok, err)
	}

	if err := st.AppendEvent(ctxA, core.Event{TaskID: taskA.ID, Kind: "test.event"}); err != nil {
		t.Fatal(err)
	}
	if events, err := st.ListEvents(ctxB, taskA.ID); err != nil || len(events) != 0 {
		t.Fatalf("cross-workspace events=%+v err=%v", events, err)
	}
	feature := core.Feature{ID: "feature-" + suffix, Name: "Only A"}
	if err := st.CreateFeature(ctxA, feature); err != nil {
		t.Fatal(err)
	}
	if features, err := st.ListFeatures(ctxB); err != nil || len(features) != 0 {
		t.Fatalf("cross-workspace features=%+v err=%v", features, err)
	}
	artifact, err := st.CreateArtifact(ctxA, core.Artifact{Name: "a.txt", ContentType: "text/plain", TaskID: taskA.ID}, []byte("a"))
	if err != nil {
		t.Fatal(err)
	}
	if resolved, content, err := st.GetArtifactForContext(ctxA, artifact.ID, taskA.ID, ""); err != nil || resolved.TaskID != taskA.ID || string(content) != "a" {
		t.Fatalf("authorized task artifact=%+v content=%q err=%v", resolved, content, err)
	}
	if _, _, err := st.GetArtifactForContext(ctxA, artifact.ID, "other-task", ""); err == nil {
		t.Fatal("artifact read through unrelated task context")
	}
	featureArtifact, err := st.CreateArtifact(ctxA, core.Artifact{Name: "feature.txt", ContentType: "text/plain", FeatureID: feature.ID}, []byte("feature"))
	if err != nil {
		t.Fatal(err)
	}
	if resolved, content, err := st.GetArtifactForContext(ctxA, featureArtifact.ID, "", feature.ID); err != nil || resolved.FeatureID != feature.ID || string(content) != "feature" {
		t.Fatalf("authorized feature artifact=%+v content=%q err=%v", resolved, content, err)
	}
	if _, _, err := st.GetArtifact(ctxB, artifact.ID); err == nil {
		t.Fatal("workspace B read workspace A artifact")
	}
	job := core.Job{ID: "job-" + suffix, TaskID: taskA.ID, Stage: core.StageImplement, State: core.JobPending, StartedAt: time.Now()}
	if err := st.CreateJob(ctxA, job); err != nil {
		t.Fatal(err)
	}
	jobB := core.Job{ID: "job-b-" + suffix, TaskID: taskB.ID, Stage: core.StageImplement, State: core.JobPending, StartedAt: time.Now()}
	if err := st.CreateJob(ctxB, jobB); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateWorkOrder(ctxB, core.WorkOrder{ID: "wrong-workspace-" + suffix, TaskID: taskA.ID, JobID: job.ID, Stage: core.StageImplement}); err == nil {
		t.Fatal("cross-workspace work order succeeded")
	}
	if err := st.CreateWorkOrder(ctxA, core.WorkOrder{ID: "wrong-task-" + suffix, TaskID: taskA.ID, JobID: jobB.ID, Stage: core.StageImplement}); err == nil {
		t.Fatal("work order linked a task to another task's job")
	}
	if err := st.CreateWorkOrder(ctxA, core.WorkOrder{ID: "wrong-stage-" + suffix, TaskID: taskA.ID, JobID: job.ID, Stage: core.StageReview}); err == nil {
		t.Fatal("work order linked a job at the wrong stage")
	}
	order := core.WorkOrder{ID: job.ID, TaskID: taskA.ID, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued}
	if err := st.CreateWorkOrder(ctxA, order); err != nil {
		t.Fatal(err)
	}
	if orders, err := st.ListWorkOrders(ctxB); err != nil || len(orders) != 0 {
		t.Fatalf("cross-workspace orders=%+v err=%v", orders, err)
	}

	cfgA := isolationConfig(ids[0])
	cfgA.MaxBounces = 3
	if _, err := st.UpdateWorkspaceConfig(store.WithActor(ctxA, store.Actor{ID: "test", Role: core.ActorHuman}), 1, cfgA); err != nil {
		t.Fatal(err)
	}
	if a, _ := st.WorkspaceConfig(ctxA); a.Version != 2 {
		t.Fatalf("workspace A version=%d", a.Version)
	}
	if b, _ := st.WorkspaceConfig(ctxB); b.Version != 1 {
		t.Fatalf("workspace B version=%d", b.Version)
	}
}

func TestTaskLockSerializesWithinWorkspaceIntegration(t *testing.T) {
	databaseURL := integrationDatabaseURL(t)
	root := context.Background()
	st, err := Open(root, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := store.WithWorkspace(root, "lock-workspace")
	entered, release, done := make(chan int, 2), make(chan struct{}), make(chan error, 2)
	go func() {
		done <- st.WithTaskSideEffectLock(ctx, "same-task", func() error {
			entered <- 1
			<-release
			return nil
		})
	}()
	if first := <-entered; first != 1 {
		t.Fatalf("first entrant=%d", first)
	}
	otherWorkspace := make(chan error, 1)
	go func() {
		otherWorkspace <- st.WithTaskSideEffectLock(store.WithWorkspace(root, "other-workspace"), "same-task", func() error {
			return nil
		})
	}()
	select {
	case err := <-otherWorkspace:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("another workspace was blocked by the task lock")
	}
	go func() {
		done <- st.WithTaskSideEffectLock(ctx, "same-task", func() error {
			entered <- 2
			return nil
		})
	}()
	select {
	case second := <-entered:
		t.Fatalf("second callback entered before release: %d", second)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if second := <-entered; second != 2 {
		t.Fatalf("second entrant=%d", second)
	}
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}

func isolationConfig(workspace string) *config.Config {
	return &config.Config{Workspace: workspace, MaxBounces: 2, Routing: config.Routing{Stages: map[string]config.StageRoute{"triage": {Model: "gpt", Timeout: time.Minute, TimeoutText: "1m", Execution: config.ExecutionInProcess}, "spec": {Model: "gpt", Timeout: time.Minute, TimeoutText: "1m", Execution: config.ExecutionInProcess}, "implement": {Model: "operator", Timeout: time.Hour, TimeoutText: "1h", Execution: config.ExecutionMCP}, "review": {Model: "operator", Timeout: time.Hour, TimeoutText: "1h", Execution: config.ExecutionMCP}}}, Repos: []config.Repo{{Name: "repo", URL: "https://example.test/repo.git", Base: "main"}}}
}
func isolationTask(workspace, id string) core.Task {
	return core.Task{ID: id, Workspace: workspace, Source: "test", Title: "Isolation", Level: core.L2, Repo: "repo", BaseBranch: "main", Branch: "conveyor/" + id, State: core.TaskAwaiting, CreatedAt: time.Now()}
}
