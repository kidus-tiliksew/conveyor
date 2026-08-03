package store

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

func TestServedRequirementsForTaskReportsAuthorityTruncation(t *testing.T) {
	ctx := WithWorkspace(t.Context(), "demo")
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
