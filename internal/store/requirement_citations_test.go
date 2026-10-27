package store

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

func TestServedRequirementsForTaskReportsAuthorityTruncation(t *testing.T) {
	ctx := WithWorkspace(WithActor(t.Context(), SystemActor()), "demo")
	st := NewMemory()
	task := core.Task{ID: "authority-root", Workspace: "demo", State: core.TaskRunning, CreatedAt: time.Now().UTC()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	const authorityNodes = 8
	for i := 0; i < authorityNodes; i++ {
		if err := st.AppendEvent(ctx, core.Event{TaskID: task.ID, Kind: "requirement.serves_confirmed", Payload: core.JSONPayload(map[string]any{
			"requirement_id": fmt.Sprintf("req-%03d", i),
		})}); err != nil {
			t.Fatal(err)
		}
	}
	_, err := ServedRequirementsForTask(ctx, st, task.ID, authorityNodes)
	var budgetErr *AuthorityBudgetError
	if !errors.As(err, &budgetErr) || budgetErr.Limit != authorityNodes || budgetErr.TaskID != task.ID {
		t.Fatalf("budget error=%+v err=%v", budgetErr, err)
	}
}

// A requirement that serves many other tasks is a hub whose links the
// authority walk never needs: resolution reads only the roots' own links, so
// it returns the one requirement however many tasks the requirement serves
// (component-work-orders; req-260802-72fc68 AC-2.1).
func TestServedRequirementsForTaskIgnoresHubFanoutBeyondRoots(t *testing.T) {
	ctx := WithWorkspace(WithActor(t.Context(), SystemActor()), "demo")
	human := WithActor(ctx, Actor{ID: "operator", Role: core.ActorHuman})
	st := NewMemory()
	requirement, version, err := st.CreateRequirement(human, core.Requirement{ID: "req-authority-hub", Title: "Authority hub"}, core.RequirementVersion{
		Content: "# Authority hub", Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Serve many tasks."}}, Origin: core.RequirementOriginOperator,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmRequirementVersion(human, requirement.ID, version.Version); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 300; index++ {
		task := core.Task{ID: fmt.Sprintf("authority-hub-task-%03d", index), Workspace: "demo", State: core.TaskRunning, CreatedAt: time.Now().UTC()}
		if err = st.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
		if _, err = st.UpdateTaskContext(ctx, task.ID, TaskContextChange{Add: TaskContextInput{RequirementIDs: []string{requirement.ID}}}); err != nil {
			t.Fatal(err)
		}
	}
	// 300 serving tasks exceed both the fan-out (64) and the node budget
	// (256) of the default authority walk.
	for _, taskID := range []string{"authority-hub-task-000", "authority-hub-task-299"} {
		served, err := ServedRequirementsForTask(ctx, st, taskID)
		if err != nil {
			t.Fatalf("task %s: %v", taskID, err)
		}
		if len(served.Requirements) != 1 || served.Requirements[0].ID != requirement.ID || served.Requirements[0].Version != version.Version {
			t.Fatalf("task %s served=%+v", taskID, served.Requirements)
		}
	}
}
