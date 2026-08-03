package lineagecontext

import (
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func TestAssembleReportsRenderableByteExhaustion(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	task := core.Task{ID: "task-budget", Workspace: "demo", Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-task-budget", State: core.TaskRunning, CreatedAt: time.Now().UTC()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateArtifact(ctx, core.Artifact{Name: "context.md", ContentType: "text/markdown", TaskID: task.ID}, []byte("bounded context")); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{ExecutionSettings: &config.ContextualExecutionSettings{ControlPlane: config.ControlPlaneSettings{Planning: config.PlanningSettings{Context: config.LineageContextSettings{Depth: 3, Nodes: 32, RenderableBytes: 1}}}}}
	result, err := Assemble(ctx, st, cfg, []core.LineageNode{{Type: core.LineageTask, ID: task.ID}}, task.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Items) != 0 || result.OmittedCount != 1 {
		t.Fatalf("items=%d omitted=%d, want zero items and one omission", len(result.Items), result.OmittedCount)
	}
	if len(result.ExhaustionReasons) != 1 || result.ExhaustionReasons[0] != "renderable_bytes" {
		t.Fatalf("exhaustion reasons=%v, want renderable_bytes", result.ExhaustionReasons)
	}
}
