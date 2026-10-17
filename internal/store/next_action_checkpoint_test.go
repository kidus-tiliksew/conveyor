package store

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/nextaction"
)

// The agent question reads the checkpoint of the latest queued order only,
// so an older order's request never attaches to the order the next action
// selects, and only while that order is still released at the checkpoint
// (component-work-orders, Next action; feature-operator-next-action NA-3
// row 9).
func TestMarkersReadTheLatestQueuedOrdersCheckpoint(t *testing.T) {
	citations := []core.WorkOrderAuthorityConflictCitation{{DocumentID: "design-a", CitedVersion: 3, StatementOrSectionID: "Section"}}
	checkpoint := &core.WorkOrderCheckpoint{DecisionRequest: "Confirm design-a v4.", Class: core.WorkOrderCheckpointClassAuthorityConflict, Citations: citations}
	older := core.WorkOrder{ID: "older", State: core.WorkOrderQueued, Checkpoint: checkpoint}
	claimed := core.WorkOrder{ID: "claimed", State: core.WorkOrderClaimed, Checkpoint: checkpoint}
	latest := core.WorkOrder{ID: "latest", State: core.WorkOrderQueued}
	for name, orders := range map[string][]core.WorkOrder{
		"older queued":  {older, latest},
		"older claimed": {claimed, latest},
	} {
		if got := markersFrom(ActivityMarker{}, nil, orders); got.CheckpointDecision || got.Checkpoint != "" || got.CheckpointClass != "" || got.Citations != nil {
			t.Fatalf("%s: markers = %+v, want no checkpoint on the latest queued order", name, got)
		}
	}
	released := core.WorkOrder{ID: "released", State: core.WorkOrderQueued, LastAttemptID: "attempt-1", LastAttemptOutcome: core.WorkOrderOutcomeReleased, Checkpoint: checkpoint}
	got := markersFrom(ActivityMarker{}, nil, []core.WorkOrder{latest, released})
	if !got.CheckpointDecision || got.Checkpoint != checkpoint.DecisionRequest || got.CheckpointClass != checkpoint.Class || !reflect.DeepEqual(got.Citations, citations) {
		t.Fatalf("markers = %+v, want the latest queued order's checkpoint", got)
	}
	// An operator recovery clears the released outcome and keeps the
	// checkpoint for history; the resumed order no longer asks the question.
	resumed := released
	resumed.LastAttemptOutcome = ""
	resumed.OperatorDirection = "Continue under the confirmed authority."
	resumed.RedispatchCount = 1
	if got := markersFrom(ActivityMarker{}, nil, []core.WorkOrder{latest, resumed}); got.CheckpointDecision || got.Checkpoint != "" || got.CheckpointClass != "" || got.Citations != nil {
		t.Fatalf("resumed: markers = %+v, want no checkpoint marker", got)
	}
}

// The row-10 binding names the sealed checkpoint's context, the one attempt
// that recorded its unresolved operations, every such operation in ID order,
// and that attempt's safe-input digest, so PrepareVerificationRecovery accepts
// a request built from it. Every checkpoint the validator would reject yields
// a refusal naming the limitation and no partial binding
// (feature-operator-next-action NA-3, Disposition binding).
func TestVerificationDispositionBindingMatchesTheRecoveryValidator(t *testing.T) {
	head := strings.Repeat("a", 40)
	safe := map[string]json.RawMessage{"target": json.RawMessage(`"fixture"`)}
	digest := verificationHash(verificationJSON(safe))
	baseTask := core.Task{ID: "task", Repo: "conveyor", ReviewedHeadSHA: head}
	baseOrder := core.WorkOrder{ID: "verify", TaskID: "task", Stage: core.StageVerify, State: core.WorkOrderQueued, HeadSHA: head, LastAttemptID: "attempt-1",
		Checkpoint: &core.WorkOrderCheckpoint{DecisionRequest: "Inspect the provider.", Verification: &core.WorkOrderVerificationCheckpoint{ContextID: "ctx", HeadSHA: head, OperationIDs: []string{"op-b", "op-a"}}}}
	contextRow := func(id, attempt string) VerificationRow {
		return VerificationRow{Table: "verification_contexts", ID: id, TaskID: "task", State: "sealed", Body: verificationJSON(VerificationContext{ID: id, TaskID: "task", WorkOrderID: "verify", WorkOrderAttemptID: attempt, Revisions: []core.VerificationRevision{{Repository: "conveyor", SHA: head}}})}
	}
	runRow := func(id, contextID, attempt string) VerificationRow {
		return VerificationRow{Table: "verification_attempts", ID: id, TaskID: "task", ContextID: contextID, State: "blocked", Body: verificationJSON(VerificationAttempt{ID: id, ContextID: contextID, WorkOrderAttemptID: attempt, SafeInputs: safe})}
	}
	opRow := func(id, contextID, runID, state, inputDigest string) VerificationRow {
		return VerificationRow{Table: "verification_operations", ID: id, TaskID: "task", ContextID: contextID, RunID: runID, State: state, Body: verificationJSON(VerificationOperation{ID: id, ContextID: contextID, RunID: runID, InputDigest: inputDigest})}
	}
	baseRows := func() []VerificationRow {
		return []VerificationRow{contextRow("ctx", "attempt-1"), runRow("run-1", "ctx", "attempt-1"), runRow("run-2", "ctx", "attempt-1"),
			opRow("op-a", "ctx", "run-1", "prepared", digest), opRow("op-b", "ctx", "run-1", "dispatching", digest), opRow("op-done", "ctx", "run-2", "completed", digest)}
	}

	got := verificationDispositionBinding(baseTask, baseOrder, baseRows())
	want := &nextaction.Disposition{ContextID: "ctx", RunID: "run-1", OperationIDs: []string{"op-a", "op-b"}, InputDigest: digest}
	if got.Refusal != "" || !reflect.DeepEqual(got.Binding, want) {
		t.Fatalf("binding = %+v refusal %q, want %+v", got.Binding, got.Refusal, want)
	}
	request := &VerificationRecoveryDisposition{ContextID: want.ContextID, RunID: want.RunID, OperationIDs: want.OperationIDs, InputDigest: want.InputDigest, Disposition: "applied", Reason: "Operator inspected the provider."}
	ctx := WithActor(WithVerificationRecovery(context.Background(), request), Actor{ID: "user:operator", Role: core.ActorUser})
	if _, _, err := PrepareVerificationRecovery(ctx, baseTask, baseOrder, baseRows(), "binding", time.Now().UTC()); err != nil {
		t.Fatalf("recovery built from the binding refused: %v", err)
	}

	cases := []struct {
		name    string
		edit    func(*core.Task, *core.WorkOrder, []VerificationRow) []VerificationRow
		refusal string
	}{
		{"no unresolved operation", func(_ *core.Task, _ *core.WorkOrder, rows []VerificationRow) []VerificationRow {
			rows[3].State, rows[4].State = "applied", "not_applied"
			return rows
		}, "has no unresolved operations"},
		{"mixed runs", func(_ *core.Task, _ *core.WorkOrder, rows []VerificationRow) []VerificationRow {
			rows[4] = opRow("op-b", "ctx", "run-2", "prepared", digest)
			return rows
		}, "span more than one verification run"},
		{"historical context", func(_ *core.Task, _ *core.WorkOrder, rows []VerificationRow) []VerificationRow {
			rows[4] = opRow("op-b", "ctx-old", "run-old", "prepared", digest)
			return rows
		}, "belongs to another verification context"},
		{"another attempt released the order", func(_ *core.Task, order *core.WorkOrder, rows []VerificationRow) []VerificationRow {
			order.LastAttemptID = "attempt-2"
			return rows
		}, "was not sealed by the released attempt"},
		{"wrong order", func(_ *core.Task, order *core.WorkOrder, rows []VerificationRow) []VerificationRow {
			order.ID = "verify-2"
			return rows
		}, "was not sealed by the released attempt"},
		{"moved head", func(task *core.Task, _ *core.WorkOrder, rows []VerificationRow) []VerificationRow {
			task.ReviewedHeadSHA = strings.Repeat("b", 40)
			return rows
		}, "no longer matches"},
		{"run of another attempt", func(_ *core.Task, _ *core.WorkOrder, rows []VerificationRow) []VerificationRow {
			rows[1] = runRow("run-1", "ctx", "attempt-0")
			return rows
		}, "was not recorded by the attempt"},
		{"other inputs", func(_ *core.Task, _ *core.WorkOrder, rows []VerificationRow) []VerificationRow {
			rows[4] = opRow("op-b", "ctx", "run-1", "prepared", "other")
			return rows
		}, "prepared over other inputs"},
		{"unresolved operation outside the checkpoint", func(_ *core.Task, _ *core.WorkOrder, rows []VerificationRow) []VerificationRow {
			return append(rows, opRow("op-c", "ctx", "run-1", "prepared", digest))
		}, "outside checkpoint"},
		{"unreadable context", func(_ *core.Task, _ *core.WorkOrder, rows []VerificationRow) []VerificationRow {
			return rows[1:]
		}, "is not readable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			task, order := baseTask, baseOrder
			rows := tc.edit(&task, &order, baseRows())
			got := verificationDispositionBinding(task, order, rows)
			if got.Binding != nil || !strings.Contains(got.Refusal, tc.refusal) {
				t.Fatalf("binding = %+v refusal %q, want refusal containing %q", got.Binding, got.Refusal, tc.refusal)
			}
		})
	}
}
