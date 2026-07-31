package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

func TestChangeTaskSetupCommandRejectsUnleasedMutation(t *testing.T) {
	ctx := WithWorkspace(context.Background(), "demo")
	st := NewMemory()
	prior := config.ExecutionSetup{Name: "prior"}
	task := core.Task{
		ID: "setup-command-lease", Workspace: "demo", Repo: "app",
		State: core.TaskRunning, SetupName: prior.Name, SetupContract: prior,
		CreatedAt: time.Now().UTC(),
	}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	request := SetupChangeRequest{
		TaskID: task.ID, RequestID: "setup-command-request",
		Reason: "verify capability boundary", Setup: config.ExecutionSetup{Name: "next"},
	}
	if _, err := st.ChangeTaskSetupCommand(ctx, taskops.TaskLease{}, request); err == nil || !strings.Contains(err.Error(), "lease does not authorize") {
		t.Fatalf("unleased setup change err=%v", err)
	}
	current, err := st.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.SetupName != prior.Name {
		t.Fatalf("unleased setup change mutated task=%+v", current)
	}
}

func TestChangeTaskSetupCommandNormalizesOptionalReasonInAuditEvent(t *testing.T) {
	for _, tc := range []struct {
		name       string
		reason     string
		wantReason string
	}{
		{name: "reasonless", reason: " \t ", wantReason: ""},
		{name: "reason present", reason: "  repair routing  ", wantReason: "repair routing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := WithActor(WithWorkspace(context.Background(), "demo"), Actor{ID: "operator", Role: core.ActorHuman})
			st := NewMemory()
			prior := config.ExecutionSetup{Name: "prior"}
			task := core.Task{
				ID: "setup-command-" + strings.ReplaceAll(tc.name, " ", "-"), Workspace: "demo", Repo: "app",
				State: core.TaskRunning, SetupName: prior.Name, SetupContract: prior,
				CreatedAt: time.Now().UTC(),
			}
			if err := st.CreateTask(ctx, task); err != nil {
				t.Fatal(err)
			}
			request := SetupChangeRequest{
				TaskID: task.ID, RequestID: task.ID + "-request", Reason: tc.reason,
				Setup: config.ExecutionSetup{Name: "next"}, ReviewTransition: "none",
			}
			result, err := taskops.ExecuteSetupChange(ctx, st, task.ID, func(lease taskops.TaskLease) (SetupChangeResult, error) {
				return st.ChangeTaskSetupCommand(ctx, lease, request)
			})
			if err != nil || result.Task.SetupName != "next" {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			events, err := st.ListEvents(ctx, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			var gotReason string
			var changes int
			for _, event := range events {
				if event.Kind != "task.setup.changed" {
					continue
				}
				changes++
				var payload struct {
					Reason string `json:"reason"`
				}
				if err := json.Unmarshal(event.Payload, &payload); err != nil {
					t.Fatal(err)
				}
				gotReason = payload.Reason
			}
			if changes != 1 || gotReason != tc.wantReason {
				t.Fatalf("changes=%d reason=%q want=%q", changes, gotReason, tc.wantReason)
			}
		})
	}
}
