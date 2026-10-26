package store

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// TestVerifyClaimRepinsAuthority covers each verification record that binds
// a verify order's pins (component-work-orders, Verify authority at claim).
func TestVerifyClaimRepinsAuthority(t *testing.T) {
	sealedAt := time.Now().UTC()
	order := core.WorkOrder{ID: "task-verify-1", TaskID: "task", Stage: core.StageVerify, State: core.WorkOrderQueued}
	body := func(v any) json.RawMessage {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	context := func(id, workOrder string, sealed *time.Time) VerificationRow {
		return VerificationRow{Table: "verification_contexts", ID: id, TaskID: order.TaskID, Body: body(VerificationContext{ID: id, WorkOrderID: workOrder, SealedAt: sealed})}
	}
	operation := func(contextID, state string) VerificationRow {
		return VerificationRow{Table: "verification_operations", ID: "op-" + contextID, TaskID: order.TaskID, ContextID: contextID, State: state}
	}
	attempt := func(contextID string, recovery ...VerificationReplayAuthorization) VerificationRow {
		return VerificationRow{Table: "verification_attempts", ID: "run-" + contextID, TaskID: order.TaskID, ContextID: contextID, Body: body(VerificationAttempt{Recovery: recovery})}
	}
	withContextID := order
	withContextID.VerificationContextID = "succeeded"
	withCheckpoint := order
	withCheckpoint.Checkpoint = &core.WorkOrderCheckpoint{DecisionRequest: "grant", Verification: &core.WorkOrderVerificationCheckpoint{ContextID: "sealed"}}
	withReleaseCheckpoint := order
	withReleaseCheckpoint.Checkpoint = &core.WorkOrderCheckpoint{DecisionRequest: "operator checkpoint reached"}
	review := order
	review.Stage = core.StageReview
	implement := order
	implement.Stage = core.StageImplement
	tests := []struct {
		name  string
		order core.WorkOrder
		rows  []VerificationRow
		want  bool
	}{
		{"no records", order, nil, true},
		{"release checkpoint without verification reference", withReleaseCheckpoint, nil, true},
		{"unsealed context with resolved work", order, []VerificationRow{context("open", order.ID, nil), operation("open", "applied"), attempt("open", VerificationReplayAuthorization{ID: "recovery:used", Successor: &core.VerificationBinding{}})}, true},
		{"another order's sealed context", order, []VerificationRow{context("other", "task-verify-0", &sealedAt), operation("other", "outcome_unknown"), attempt("other", VerificationReplayAuthorization{ID: "recovery:open"})}, true},
		{"succeeded context on the order", withContextID, nil, false},
		{"verification checkpoint on the order", withCheckpoint, nil, false},
		{"sealed context of the order", order, []VerificationRow{context("sealed", order.ID, &sealedAt)}, false},
		{"unresolved operation", order, []VerificationRow{context("open", order.ID, nil), operation("open", "outcome_unknown")}, false},
		{"unconsumed recovery authorization", order, []VerificationRow{context("open", order.ID, nil), attempt("open", VerificationReplayAuthorization{ID: "recovery:open"})}, false},
		{"review seat", review, nil, false},
		{"implement order", implement, nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := VerifyClaimRepinsAuthority(tt.order, tt.rows); got != tt.want {
				t.Fatalf("VerifyClaimRepinsAuthority = %v, want %v", got, tt.want)
			}
		})
	}
}
