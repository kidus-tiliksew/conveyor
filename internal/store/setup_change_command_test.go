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

func policyCommandTask(t *testing.T, st Store, ctx context.Context, id string) core.Task {
	t.Helper()
	prior := config.ExecutionSetup{Name: "prior", Review: config.ReviewPanel{Seats: []config.ReviewSeat{{}}}}
	task := core.Task{
		ID: id, Workspace: "demo", Repo: "app",
		State: core.TaskRunning, NextStage: core.StageImplement, SetupName: prior.Name, SetupContract: prior,
		CreatedAt: time.Now().UTC(),
	}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	return task
}

func TestChangeTaskPolicyCommandRejectsUnleasedMutation(t *testing.T) {
	ctx := WithWorkspace(context.Background(), "demo")
	st := NewMemory()
	task := policyCommandTask(t, st, ctx, "policy-command-lease")
	enabled := true
	request := SetupChangeRequest{
		TaskID: task.ID, RequestID: "policy-command-request",
		Reason: "verify capability boundary", Policy: &TaskPolicyChange{VerifyStage: &enabled},
	}
	if _, err := st.ChangeTaskPolicyCommand(ctx, taskops.TaskLease{}, request); err == nil || !strings.Contains(err.Error(), "lease does not authorize") {
		t.Fatalf("unleased policy change err=%v", err)
	}
	// A lease admitted for a different task does not authorize this one.
	_, err := taskops.ExecuteSetupChange(ctx, st, "another-task", func(lease taskops.TaskLease) (SetupChangeResult, error) {
		return st.ChangeTaskPolicyCommand(ctx, lease, request)
	})
	if err == nil || !strings.Contains(err.Error(), "lease does not authorize") {
		t.Fatalf("cross-task lease err=%v", err)
	}
	current, err := st.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.SetupContract.VerifyStage {
		t.Fatalf("unleased policy change mutated task=%+v", current)
	}
	if count, _ := st.CountEvents(ctx, task.ID, "task.setup.changed"); count != 0 {
		t.Fatalf("unleased policy change appended %d audit events", count)
	}
}

func TestChangeTaskPolicyCommandRequiresPolicyAndNonblankReason(t *testing.T) {
	ctx := WithActor(WithWorkspace(context.Background(), "demo"), Actor{ID: "operator", Role: core.ActorHuman})
	st := NewMemory()
	task := policyCommandTask(t, st, ctx, "policy-command-required")
	enabled := true
	change := func(request SetupChangeRequest) (SetupChangeResult, error) {
		return taskops.ExecuteSetupChange(ctx, st, task.ID, func(lease taskops.TaskLease) (SetupChangeResult, error) {
			return st.ChangeTaskPolicyCommand(ctx, lease, request)
		})
	}
	for name, request := range map[string]SetupChangeRequest{
		// The retired setup-reassignment shape: a named setup without a policy.
		"setup without policy": {TaskID: task.ID, RequestID: "setup-shape", Reason: "reroute", Setup: config.ExecutionSetup{Name: "next"}},
		"blank reason":         {TaskID: task.ID, RequestID: "blank-reason", Reason: " \t ", Policy: &TaskPolicyChange{VerifyStage: &enabled}},
		"blank request id":     {TaskID: task.ID, RequestID: " ", Reason: "enable", Policy: &TaskPolicyChange{VerifyStage: &enabled}},
		"empty policy":         {TaskID: task.ID, RequestID: "empty-policy", Reason: "enable", Policy: &TaskPolicyChange{}},
	} {
		if _, err := change(request); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	current, _ := st.GetTask(ctx, task.ID)
	if current.SetupName != "prior" || current.SetupContract.Name != "prior" || current.SetupContract.VerifyStage {
		t.Fatalf("refused request mutated task=%+v", current)
	}
	if count, _ := st.CountEvents(ctx, task.ID, "task.setup.changed"); count != 0 {
		t.Fatalf("refused request appended %d audit events", count)
	}
}

func TestChangeTaskPolicyCommandTrimsReasonInAuditEvent(t *testing.T) {
	ctx := WithActor(WithWorkspace(context.Background(), "demo"), Actor{ID: "operator", Role: core.ActorHuman})
	st := NewMemory()
	task := policyCommandTask(t, st, ctx, "policy-command-reason")
	enabled := true
	request := SetupChangeRequest{TaskID: task.ID, RequestID: task.ID + "-request", Reason: "  enable verification  ", Policy: &TaskPolicyChange{VerifyStage: &enabled}}
	result, err := taskops.ExecuteSetupChange(ctx, st, task.ID, func(lease taskops.TaskLease) (SetupChangeResult, error) {
		return st.ChangeTaskPolicyCommand(ctx, lease, request)
	})
	if err != nil || !result.Task.SetupContract.VerifyStage || result.Task.SetupName != "prior" || result.ReviewTransition != "policy_only" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	events, err := st.ListEvents(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var changes int
	for _, event := range events {
		if event.Kind != "task.setup.changed" {
			continue
		}
		changes++
		var payload struct {
			Reason string `json:"reason"`
			Actor  string `json:"actor"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if payload.Reason != "enable verification" || payload.Actor != "operator" {
			t.Fatalf("audit payload=%+v", payload)
		}
	}
	if changes != 1 {
		t.Fatalf("audit events=%d", changes)
	}
}

// Historical setup-change events stay readable: review orders a retired setup
// change superseded remain historical-only for every current-round reader.
func TestHistoricalSetupChangeSupersessionRemainsReadable(t *testing.T) {
	ctx := WithWorkspace(context.Background(), "demo")
	st := NewMemory()
	task := policyCommandTask(t, st, ctx, "historical-supersession")
	payload := map[string]any{"request_id": "retired-setup-change", "previous_setup": map[string]any{"name": "old"}, "new_setup": map[string]any{"name": "next"},
		"review_transition": map[string]any{"kind": "same_round_reconciled", "retained_work_order_ids": []string{"seat-2"}, "superseded_work_order_ids": []string{"seat-1"}}}
	if err := st.AppendEvent(ctx, core.Event{TaskID: task.ID, Kind: "task.setup.changed", Payload: core.JSONPayload(payload)}); err != nil {
		t.Fatal(err)
	}
	events, err := st.ListEvents(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if superseded := SupersededReviewWorkOrders(events); !superseded["seat-1"] || superseded["seat-2"] || len(superseded) != 1 {
		t.Fatalf("superseded=%v", superseded)
	}
	orders := []core.WorkOrder{{ID: "seat-1", Stage: core.StageReview}, {ID: "seat-2", Stage: core.StageReview}, {ID: "seat-1-replacement", Stage: core.StageReview}}
	current := CurrentReviewOrders(orders, events)
	if len(current) != 2 || current[0].ID != "seat-2" || current[1].ID != "seat-1-replacement" {
		t.Fatalf("current=%+v", current)
	}
}
