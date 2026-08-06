package store

import (
	"errors"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

func TestTaskContextFeedsRequirementAndGovernanceAuthority(t *testing.T) {
	ctx := WithWorkspace(t.Context(), "demo")
	st := NewMemory()
	requirement, proposed, err := st.CreateRequirement(ctx, core.Requirement{ID: "req-task", Title: "Task delivery"}, core.RequirementVersion{
		Content: "Task delivery", Origin: core.RequirementOriginOperator,
		Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Deliver task context."}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmRequirementVersion(ctx, requirement.ID, proposed.Version); err != nil {
		t.Fatal(err)
	}
	design, designVersion, err := st.CreateSystemDesign(ctx, core.SystemDesign{ID: "design-task", Title: "Task mechanism", Category: "Architecture"}, core.SystemDesignVersion{
		Content: "# Task mechanism\n\n```conveyor:governs\n- repo: other\n  paths:\n    - internal/**\n```", Origin: core.SystemDesignOriginOperator,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmSystemDesignVersion(ctx, design.ID, designVersion.Version); err != nil {
		t.Fatal(err)
	}
	task := core.Task{ID: "context-task", Workspace: "demo", Repo: "conveyor", State: core.TaskRunning, CreatedAt: time.Now().UTC()}
	if err = st.CreateTaskWithDependenciesAndContext(ctx, task, nil, TaskContextInput{RequirementIDs: []string{requirement.ID}, DesignIDs: []string{design.ID}}); err != nil {
		t.Fatal(err)
	}
	served, err := ServedRequirementsForTask(ctx, st, task.ID)
	if err != nil || len(served.Requirements) != 1 || served.Requirements[0].ID != requirement.ID {
		t.Fatalf("served=%+v err=%v", served, err)
	}
	governance, err := GovernanceForTask(ctx, st, task.ID, task.Repo)
	if err != nil || len(governance.Designs) != 1 || governance.Designs[0].ID != design.ID || governance.Designs[0].Version != 1 {
		t.Fatalf("governance=%+v err=%v", governance, err)
	}

	second, err := st.ProposeSystemDesignVersion(ctx, core.SystemDesignVersion{DocumentID: design.ID, Content: "# Task mechanism v2\n\n```conveyor:governs\n- repo: other\n  paths:\n    - internal/v2/**\n```", Origin: core.SystemDesignOriginOperator})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmSystemDesignVersion(ctx, design.ID, second.Version, 1); err != nil {
		t.Fatal(err)
	}
	governance, err = GovernanceForTask(ctx, st, task.ID, task.Repo)
	if err != nil || governance.Designs[0].Version != 1 {
		t.Fatalf("pinned governance=%+v err=%v", governance, err)
	}
}

func TestTaskContextRejectsUnknownReferenceWithoutCreatingTask(t *testing.T) {
	ctx := WithWorkspace(t.Context(), "demo")
	st := NewMemory()
	task := core.Task{ID: "not-created", Workspace: "demo", State: core.TaskQueued}
	err := st.CreateTaskWithDependenciesAndContext(ctx, task, nil, TaskContextInput{RequirementIDs: []string{"req-missing"}})
	var referenceErr *TaskContextReferenceError
	if !errors.As(err, &referenceErr) || referenceErr.ID != "req-missing" {
		t.Fatalf("error=%v", err)
	}
	if _, err = st.GetTask(ctx, task.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("partial task err=%v", err)
	}
}
