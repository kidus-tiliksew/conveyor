package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// RunNextActionConformance pins the factory log and the shared attention count.
func RunNextActionConformance(t *testing.T, st store.Store, ctx context.Context) {
	t.Helper()
	workspace, _ := store.WorkspaceFromContext(ctx)
	now := time.Now().UTC().Truncate(time.Second)
	task := core.Task{ID: "factory-acts-" + core.NewTaskID(), Workspace: workspace, Repo: "conveyor", Branch: "factory-" + core.NewTaskID(), State: core.TaskRunning, CreatedAt: now}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	system := core.Event{TaskID: task.ID, Kind: "pipeline.bounced", ActorID: "dispatcher", ActorRole: core.ActorSystem, At: now, Payload: core.JSONPayload(map[string]any{"source": "mcp-review"})}
	agent := core.Event{TaskID: task.ID, Kind: "pipeline.bounced", ActorID: "agent", ActorRole: core.ActorAgent, At: now.Add(time.Second), Payload: core.JSONPayload(map[string]any{"source": "request-changes", "reason_code": "user_changes_requested"})}
	old := core.Event{TaskID: task.ID, Kind: "merge.conflict_fix_dispatched", ActorID: "dispatcher", ActorRole: core.ActorSystem, At: now.Add(-25 * time.Hour)}
	for _, event := range []core.Event{old, system, agent} {
		if err := st.AppendEvent(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	acts, err := st.ListFactoryActs(ctx, now.Add(-24*time.Hour), 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(acts) != 1 || acts[0].Kind != "pipeline.bounced" || !acts[0].Automatic || acts[0].TaskID != task.ID {
		t.Fatalf("factory acts=%+v", acts)
	}
	bounded, err := st.ListFactoryActs(ctx, now.Add(-24*time.Hour), 1)
	if err != nil || len(bounded) != 1 {
		t.Fatalf("limit acts=%+v err=%v", bounded, err)
	}
	isolated := workspace + "-attention-" + core.NewTaskID()
	iso := store.WithWorkspace(ctx, isolated)
	bootstrap, ok := st.(interface {
		BootstrapWorkspaceConfig(context.Context, *config.Config) (bool, error)
	})
	if !ok {
		t.Fatal("store cannot bootstrap an isolated workspace")
	}
	if _, err = bootstrap.BootstrapWorkspaceConfig(iso, &config.Config{Workspace: isolated, Repos: []config.Repo{{Name: "conveyor", Base: "main"}}}); err != nil {
		t.Fatal(err)
	}
	awaiting := core.Task{ID: "attention-" + core.NewTaskID(), Workspace: isolated, Repo: "conveyor", Branch: "attention-" + core.NewTaskID(), State: core.TaskAwaiting, GateKind: core.GateKindPlanApproval, CreatedAt: now}
	running := core.Task{ID: "running-" + core.NewTaskID(), Workspace: isolated, Repo: "conveyor", Branch: "running-" + core.NewTaskID(), State: core.TaskRunning, CreatedAt: now}
	if err = st.CreateTask(iso, awaiting); err != nil {
		t.Fatal(err)
	}
	if err = st.CreateTask(iso, running); err != nil {
		t.Fatal(err)
	}
	listed, err := st.ListTasksFiltered(iso, store.TaskFilter{})
	if err != nil {
		t.Fatal(err)
	}
	candidates := make([]core.Task, 0, len(listed))
	for _, task := range listed {
		if task.Workspace == isolated {
			candidates = append(candidates, task)
		}
	}
	projected, err := store.ReadNextActions(iso, st, candidates, store.NextActionOptions{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	want := 0
	for _, action := range projected {
		if store.TaskNeedsAttention(action) {
			want++
		}
	}
	count, err := st.CountOperatorAttention(iso)
	if err != nil || count != want {
		t.Fatalf("attention count=%d projected=%d err=%v", count, want, err)
	}
	page, err := store.PageOperatorAttention(iso, st, candidates, 0, 10)
	if err != nil || page.Total != want {
		t.Fatalf("page total=%d want=%d err=%v", page.Total, want, err)
	}
}
