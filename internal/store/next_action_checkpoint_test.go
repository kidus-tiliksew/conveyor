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
	"github.com/kidus-tiliksew/conveyor/internal/verification"
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

// A checkpoint with no unresolved operation binds the attempt the start-path
// retry gate would refuse to retry: the latest attempt of a subject among the
// verify order's contexts, ended, under an operator_action_required contract,
// with no unused gate-acceptable authorization. Several gated subjects bind
// the earliest by start time first, and the binding passes
// PrepareVerificationRecovery (feature-operator-next-action NA-3,
// Disposition binding; req-verification-kits AC-3.4).
func TestVerificationDispositionBindingBindsTheRetryGatedAttempt(t *testing.T) {
	head := strings.Repeat("a", 40)
	safe := map[string]json.RawMessage{}
	digest := verificationHash(verificationJSON(safe))
	task := core.Task{ID: "task", Repo: "conveyor", ReviewedHeadSHA: head}
	order := core.WorkOrder{ID: "verify", TaskID: "task", Stage: core.StageVerify, State: core.WorkOrderQueued, HeadSHA: head, LastAttemptID: "attempt-2",
		Checkpoint: &core.WorkOrderCheckpoint{DecisionRequest: "Authorize one retry.", Verification: &core.WorkOrderVerificationCheckpoint{ContextID: "ctx-b", HeadSHA: head, OperationIDs: []string{}}}}
	base := time.Date(2026, 10, 29, 11, 0, 0, 0, time.UTC)
	contextRow := func(id, orderID, attempt string) VerificationRow {
		return VerificationRow{Table: "verification_contexts", ID: id, TaskID: "task", State: "sealed", Body: verificationJSON(VerificationContext{ID: id, TaskID: "task", WorkOrderID: orderID, WorkOrderAttemptID: attempt, Revisions: []core.VerificationRevision{{Repository: "conveyor", SHA: head}}})}
	}
	obligationRow := func(contextID, id, policy string) VerificationRow {
		return VerificationRow{Table: "verification_obligations", ID: contextID + ":" + id, TaskID: "task", ContextID: contextID, Body: verificationJSON(VerificationObligation{ID: id, ContextID: contextID, Digest: "digest-" + id, Contract: verification.Exercise{ID: id, RetryPolicy: policy}})}
	}
	type runOption func(*VerificationAttempt)
	runRow := func(id, contextID, attempt, obligation string, minute int, options ...runOption) VerificationRow {
		ended := base.Add(time.Duration(minute)*time.Minute + time.Second)
		run := VerificationAttempt{ID: id, ContextID: contextID, WorkOrderAttemptID: attempt, Subject: core.VerificationSubject{Kind: "ordinary", ObligationID: obligation, ContractDigest: "digest-" + obligation}, SafeInputs: safe, State: "succeeded", StartedAt: base.Add(time.Duration(minute) * time.Minute), EndedAt: &ended}
		for _, option := range options {
			option(&run)
		}
		return VerificationRow{Table: "verification_attempts", ID: id, TaskID: "task", ContextID: contextID, State: run.State, Body: verificationJSON(run)}
	}
	running := func(run *VerificationAttempt) { run.State, run.EndedAt = "running", nil }
	authorized := func(consumed bool) runOption {
		return func(run *VerificationAttempt) {
			a := VerificationReplayAuthorization{ID: "recovery:r", Actor: "user:operator", Disposition: "applied", Original: core.VerificationBinding{ContextID: run.ContextID, WorkOrderID: "verify", RunID: run.ID}}
			if consumed {
				a.Successor = &core.VerificationBinding{RunID: "successor"}
			}
			run.Recovery = append(run.Recovery, a)
		}
	}
	baseRows := func() []VerificationRow {
		return []VerificationRow{
			contextRow("ctx-a", "verify", "attempt-1"), contextRow("ctx-b", "verify", "attempt-2"),
			obligationRow("ctx-a", "snapshot", "operator_action_required"), obligationRow("ctx-b", "snapshot", "operator_action_required"),
			runRow("run-a", "ctx-a", "attempt-1", "snapshot", 1),
		}
	}
	bound := func(runID, contextID string) *nextaction.Disposition {
		return &nextaction.Disposition{ContextID: contextID, RunID: runID, OperationIDs: []string{}, InputDigest: digest}
	}

	got := verificationDispositionBinding(task, order, baseRows())
	if got.Refusal != "" || !reflect.DeepEqual(got.Binding, bound("run-a", "ctx-a")) {
		t.Fatalf("binding = %+v refusal %q, want run-a", got.Binding, got.Refusal)
	}
	request := &VerificationRecoveryDisposition{ContextID: "ctx-a", RunID: "run-a", OperationIDs: got.Binding.OperationIDs, InputDigest: got.Binding.InputDigest, Disposition: "applied", Reason: "Succeeded before claim loss."}
	ctx := WithActor(WithVerificationRecovery(context.Background(), request), Actor{ID: "user:operator", Role: core.ActorUser})
	if _, _, err := PrepareVerificationRecovery(ctx, task, order, baseRows(), "binding", time.Now().UTC()); err != nil {
		t.Fatalf("recovery built from the gated binding refused: %v", err)
	}

	cases := []struct {
		name string
		rows func([]VerificationRow) []VerificationRow
		want *nextaction.Disposition
	}{
		{"latest attempt of the subject", func(rows []VerificationRow) []VerificationRow {
			return append(rows, runRow("run-a2", "ctx-a", "attempt-1", "snapshot", 2))
		}, bound("run-a2", "ctx-a")},
		{"earliest gated subject first", func(rows []VerificationRow) []VerificationRow {
			return append(rows, obligationRow("ctx-a", "login", "operator_action_required"), runRow("run-login", "ctx-a", "attempt-1", "login", 0))
		}, bound("run-login", "ctx-a")},
		{"unused authorization moves to the next subject", func(rows []VerificationRow) []VerificationRow {
			return append(rows, obligationRow("ctx-a", "login", "operator_action_required"), runRow("run-login", "ctx-a", "attempt-1", "login", 0, authorized(false)))
		}, bound("run-a", "ctx-a")},
		{"consumed authorization binds again", func(rows []VerificationRow) []VerificationRow {
			rows[4] = runRow("run-a", "ctx-a", "attempt-1", "snapshot", 1, authorized(true))
			return rows
		}, bound("run-a", "ctx-a")},
		{"unused authorization", func(rows []VerificationRow) []VerificationRow {
			rows[4] = runRow("run-a", "ctx-a", "attempt-1", "snapshot", 1, authorized(false))
			return rows
		}, nil},
		{"running attempt", func(rows []VerificationRow) []VerificationRow {
			rows[4] = runRow("run-a", "ctx-a", "attempt-1", "snapshot", 1, running)
			return rows
		}, nil},
		{"safe-to-replay contract", func(rows []VerificationRow) []VerificationRow {
			rows[2] = obligationRow("ctx-a", "snapshot", "safe_to_replay")
			return rows
		}, nil},
		{"another order's context", func(rows []VerificationRow) []VerificationRow {
			rows[0] = contextRow("ctx-a", "verify-1", "attempt-1")
			return rows
		}, nil},
		{"unresolved operation on the run", func(rows []VerificationRow) []VerificationRow {
			return append(rows, VerificationRow{Table: "verification_operations", ID: "op", TaskID: "task", ContextID: "ctx-a", RunID: "run-a", State: "prepared", Body: verificationJSON(VerificationOperation{ID: "op", ContextID: "ctx-a", RunID: "run-a", InputDigest: digest})})
		}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := verificationDispositionBinding(task, order, tc.rows(baseRows()))
			if tc.want == nil {
				if got.Binding != nil || !strings.Contains(got.Refusal, "has no unresolved operations") {
					t.Fatalf("binding = %+v refusal %q, want the no-operations refusal", got.Binding, got.Refusal)
				}
				return
			}
			if got.Refusal != "" || !reflect.DeepEqual(got.Binding, tc.want) {
				t.Fatalf("binding = %+v refusal %q, want %+v", got.Binding, got.Refusal, tc.want)
			}
		})
	}
}
