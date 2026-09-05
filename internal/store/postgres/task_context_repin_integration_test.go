package postgres

import (
	"encoding/json"
	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"testing"
	"time"
)

func TestPostgresSubmissionGovernanceAttachmentIntegration(t *testing.T) {
	st, err := Open(t.Context(), integrationDatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	workspace := "postgres-submission-governance-" + core.NewTaskID()
	ctx := store.WithWorkspace(t.Context(), workspace)
	cfg := &config.Config{Workspace: workspace, Repos: []config.Repo{{Name: "app", URL: "https://example.test/app", Base: "main"}}}
	if _, err = st.BootstrapWorkspaceConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	design, version, err := st.CreateSystemDesign(ctx, core.SystemDesign{ID: "design-" + core.NewTaskID(), Title: "Submission authority", Category: "Architecture"}, core.SystemDesignVersion{
		Content: "# Submission\n\n```conveyor:governs\n- repo: app\n  paths:\n    - internal/**\n```", Origin: core.SystemDesignOriginOperator,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmSystemDesignVersion(ctx, design.ID, version.Version); err != nil {
		t.Fatal(err)
	}
	taskID := "task-" + core.NewTaskID()
	task := core.Task{ID: taskID, Workspace: workspace, Repo: "app", Branch: "conveyor/" + taskID, BaseBranch: "main", State: core.TaskRunning, CreatedAt: time.Now().UTC()}
	if err = st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	attribution := store.SubmissionGovernanceAttribution{WorkOrderID: "implement-1", SessionID: "worker-1"}
	attached, err := st.AttachSubmissionGovernance(ctx, task.ID, task.Repo, []string{"internal/workorder/service.go"}, attribution)
	if err != nil || len(attached) != 1 || attached[0].ID != design.ID || attached[0].Version != version.Version {
		t.Fatalf("attached=%+v err=%v", attached, err)
	}
	if repeated, repeatErr := st.AttachSubmissionGovernance(ctx, task.ID, task.Repo, []string{"internal/workorder/service.go"}, attribution); repeatErr != nil || len(repeated) != 0 {
		t.Fatalf("repeated=%+v err=%v", repeated, repeatErr)
	}
	context, err := store.TaskContextForTask(ctx, st, task.ID)
	if err != nil || len(context.Designs) != 1 || context.Designs[0].Version != version.Version {
		t.Fatalf("context=%+v err=%v", context, err)
	}
	events, err := st.ListEvents(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	derived := 0
	for _, event := range events {
		if event.Kind != store.TaskContextDesignAdded {
			continue
		}
		var payload struct {
			Source      string `json:"source"`
			WorkOrderID string `json:"work_order_id"`
			SessionID   string `json:"session_id"`
		}
		if json.Unmarshal(event.Payload, &payload) == nil && payload.Source == "submission_diff" {
			derived++
			if payload.WorkOrderID != attribution.WorkOrderID || payload.SessionID != attribution.SessionID {
				t.Fatalf("payload=%+v", payload)
			}
		}
	}
	if derived != 1 {
		t.Fatalf("derived events=%d", derived)
	}
}
