package postgres

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func TestMultiWorkspaceIsolationIntegration(t *testing.T) {
	databaseURL := os.Getenv("CONVEYOR_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("CONVEYOR_TEST_DATABASE_URL is not set")
	}
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
	if _, _, err := st.GetArtifact(ctxB, artifact.ID); err == nil {
		t.Fatal("workspace B read workspace A artifact")
	}
	job := core.Job{ID: "job-" + suffix, TaskID: taskA.ID, Stage: core.StageImplement, State: core.JobPending, StartedAt: time.Now()}
	if err := st.CreateJob(ctxA, job); err != nil {
		t.Fatal(err)
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

func isolationConfig(workspace string) *config.Config {
	return &config.Config{Workspace: workspace, MaxBounces: 2, Routing: config.Routing{Stages: map[string]config.StageRoute{"triage": {Model: "gpt", Timeout: time.Minute, TimeoutText: "1m", Execution: config.ExecutionInProcess}, "spec": {Model: "gpt", Timeout: time.Minute, TimeoutText: "1m", Execution: config.ExecutionInProcess}, "implement": {Model: "operator", Timeout: time.Hour, TimeoutText: "1h", Execution: config.ExecutionMCP}, "review": {Model: "operator", Timeout: time.Hour, TimeoutText: "1h", Execution: config.ExecutionMCP}}}, Repos: []config.Repo{{Name: "repo", URL: "https://example.test/repo.git", Base: "main"}}}
}
func isolationTask(workspace, id string) core.Task {
	return core.Task{ID: id, Workspace: workspace, Source: "test", Title: "Isolation", Level: core.L2, Repo: "repo", BaseBranch: "main", Branch: "conveyor/" + id, State: core.TaskAwaiting, CreatedAt: time.Now()}
}
