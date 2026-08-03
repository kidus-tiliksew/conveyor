package store

import (
	"fmt"
	"strings"
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
	for i := 0; i < ServedRequirementAuthorityMaxNodes; i++ {
		if err := st.AppendEvent(ctx, core.Event{TaskID: task.ID, Kind: "requirement.serves_confirmed", Payload: core.JSONPayload(map[string]any{
			"requirement_id": fmt.Sprintf("req-%03d", i),
		})}); err != nil {
			t.Fatal(err)
		}
	}
	result, err := ServedRequirementsForTask(ctx, st, task.ID)
	if err == nil || !result.Truncated || result.Omitted == 0 || !strings.Contains(err.Error(), "authority") {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}
