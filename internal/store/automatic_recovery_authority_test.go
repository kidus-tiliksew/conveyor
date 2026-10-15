package store

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
)

func answeredAuthorityOrder() core.WorkOrder {
	return core.WorkOrder{
		ID: "task-1-implement-1", TaskID: "task-1", Stage: core.StageImplement, State: core.WorkOrderQueued,
		LastAttemptOutcome: core.WorkOrderOutcomeReleased, LastFailureMessage: core.WorkOrderReleaseReasonOperatorCheckpointReached,
		RetrySuppressed: true, LastAttemptID: "attempt-1",
		Checkpoint: &core.WorkOrderCheckpoint{
			DecisionRequest: "Confirm req-a v3 and design-b v5, then recover.",
			Class:           core.WorkOrderCheckpointClassAuthorityConflict,
			Citations: []core.WorkOrderAuthorityConflictCitation{
				{DocumentID: "design-b", CitedVersion: 4, StatementOrSectionID: "Recovery"},
				{DocumentID: "req-a", CitedVersion: 2, StatementOrSectionID: "AC-1.1"},
				{DocumentID: "design-b", CitedVersion: 3, StatementOrSectionID: "Next action"},
			},
		},
	}
}

func answeredAuthorityDocuments() map[string]AuthorityDocument {
	return map[string]AuthorityDocument{
		"req-a":    {ID: "req-a", Kind: "requirement", Found: true, CurrentVersion: 3},
		"design-b": {ID: "design-b", Kind: "system_design", Found: true, CurrentVersion: 5},
	}
}

// TestDecideAnsweredAuthorityRecovery reaches every AR-3a condition row
// (req-260820-394cac AC-2.6, AC-2.7; req-bounded-automatic-recovery AC-2.2).
func TestDecideAnsweredAuthorityRecovery(t *testing.T) {
	queued := core.Task{ID: "task-1", State: core.TaskQueued}
	cases := []struct {
		name      string
		task      core.TaskState
		order     func(*core.WorkOrder)
		documents func(map[string]AuthorityDocument)
		recover   bool
	}{
		{name: "every citation answered", recover: true},
		{name: "running task", task: core.TaskRunning, recover: true},
		{name: "awaiting human task", task: core.TaskAwaiting},
		{name: "parked task", task: core.TaskParked},
		{name: "approved task", task: core.TaskApproved},
		{name: "merged task", task: core.TaskMerged},
		{name: "closed task", task: core.TaskClosed},
		{name: "verify stage", order: func(o *core.WorkOrder) { o.Stage = core.StageVerify }},
		{name: "spec stage", order: func(o *core.WorkOrder) { o.Stage = core.StageSpec }},
		{name: "live claim", order: func(o *core.WorkOrder) { o.State = core.WorkOrderClaimed }},
		{name: "stale order", order: func(o *core.WorkOrder) { o.State = core.WorkOrderStale }},
		{name: "already recovered", order: func(o *core.WorkOrder) { o.LastAttemptOutcome = ""; o.RetrySuppressed = false }},
		{name: "expired outcome", order: func(o *core.WorkOrder) { o.LastAttemptOutcome = core.WorkOrderOutcomeExpired }},
		{name: "plan revision release", order: func(o *core.WorkOrder) { o.LastFailureMessage = "plan revision requested" }},
		{name: "retry not suppressed", order: func(o *core.WorkOrder) { o.RetrySuppressed = false }},
		{name: "suppression reason", order: func(o *core.WorkOrder) { o.RetrySuppressionReason = core.AutomaticRecoveryExhaustedReason }},
		{name: "no release attempt", order: func(o *core.WorkOrder) { o.LastAttemptID = "" }},
		{name: "no checkpoint", order: func(o *core.WorkOrder) { o.Checkpoint = nil }},
		{name: "no class", order: func(o *core.WorkOrder) { o.Checkpoint.Class = "" }},
		{name: "other class", order: func(o *core.WorkOrder) { o.Checkpoint.Class = "repository_reality" }},
		{name: "blank decision request", order: func(o *core.WorkOrder) { o.Checkpoint.DecisionRequest = "  " }},
		{name: "no citations", order: func(o *core.WorkOrder) { o.Checkpoint.Citations = nil }},
		{name: "verification checkpoint", order: func(o *core.WorkOrder) {
			o.Checkpoint.Verification = &core.WorkOrderVerificationCheckpoint{ContextID: "ctx"}
		}},
		{name: "recovery refusal", order: func(o *core.WorkOrder) { o.RecoveryRefusal = "superseded" }},
		{name: "partial confirmation", documents: func(d map[string]AuthorityDocument) {
			d["req-a"] = AuthorityDocument{ID: "req-a", Found: true, CurrentVersion: 2}
		}},
		{name: "dismissal leaves cited version current", documents: func(d map[string]AuthorityDocument) {
			d["design-b"] = AuthorityDocument{ID: "design-b", Found: true, CurrentVersion: 4}
		}},
		{name: "duplicate citation unanswered", order: func(o *core.WorkOrder) { o.Checkpoint.Citations[2].CitedVersion = 5 }},
		{name: "pending version on cited document", documents: func(d map[string]AuthorityDocument) {
			doc := d["design-b"]
			doc.PendingVersions = 1
			d["design-b"] = doc
		}},
		{name: "advanced before release with new pending version", documents: func(d map[string]AuthorityDocument) {
			d["req-a"] = AuthorityDocument{ID: "req-a", Found: true, CurrentVersion: 9, PendingVersions: 1}
		}},
		{name: "missing document", documents: func(d map[string]AuthorityDocument) { delete(d, "req-a") }},
		{name: "archived document", documents: func(d map[string]AuthorityDocument) {
			doc := d["req-a"]
			doc.Found = false
			d["req-a"] = doc
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			task := queued
			if tc.task != "" {
				task.State = tc.task
			}
			order := answeredAuthorityOrder()
			if tc.order != nil {
				tc.order(&order)
			}
			documents := answeredAuthorityDocuments()
			if tc.documents != nil {
				tc.documents(documents)
			}
			got := DecideAnsweredAuthorityRecovery(task, order, documents)
			if got.Recover != tc.recover {
				t.Fatalf("Recover = %v, want %v (%+v)", got.Recover, tc.recover, got)
			}
			if !tc.recover && (got.RequestID != "" || got.Direction != "" || got.Citations != nil) {
				t.Fatalf("refused decision carries an act: %+v", got)
			}
		})
	}
}

func TestAnsweredAuthorityDirectionAndRequestID(t *testing.T) {
	order := answeredAuthorityOrder()
	got := DecideAnsweredAuthorityRecovery(core.Task{State: core.TaskQueued}, order, answeredAuthorityDocuments())
	if want := "Continue under the newly confirmed authority: design-b v5, req-a v3."; got.Direction != want {
		t.Fatalf("direction = %q, want %q", got.Direction, want)
	}
	if want := "authority-confirmed/task-1-implement-1/attempt-1"; got.RequestID != want {
		t.Fatalf("request ID = %q, want %q", got.RequestID, want)
	}
	want := []AnsweredAuthorityCitation{{"design-b", 4, 5}, {"req-a", 2, 3}, {"design-b", 3, 5}}
	if len(got.Citations) != len(want) {
		t.Fatalf("citations = %+v", got.Citations)
	}
	for i := range want {
		if got.Citations[i] != want[i] {
			t.Fatalf("citation %d = %+v, want %+v", i, got.Citations[i], want[i])
		}
	}
	order.LastAttemptID = "attempt-2"
	if next := DecideAnsweredAuthorityRecovery(core.Task{State: core.TaskQueued}, order, answeredAuthorityDocuments()); next.RequestID == got.RequestID {
		t.Fatalf("a later release reused request ID %q", next.RequestID)
	}
	if ids := AnsweredAuthorityDocumentIDs(order); len(ids) != 2 || ids[0] != "design-b" || ids[1] != "req-a" {
		t.Fatalf("document IDs = %v", ids)
	}
}

// TestAnsweredAuthorityIsOutsideMechanicalDecision keeps the AR-3 table's
// checkpoint row: the mechanical decision never acts on a checkpoint order.
func TestAnsweredAuthorityIsOutsideMechanicalDecision(t *testing.T) {
	now := time.Date(2026, 10, 15, 14, 0, 0, 0, time.UTC)
	for _, bound := range []int{0, 2} {
		policy := config.DefaultRecoveryPolicy()
		policy.AutomaticRecoveries = bound
		if got := DecideAutomaticRecovery(policy, core.Task{State: core.TaskQueued}, answeredAuthorityOrder(), now); got.Kind != AutomaticRecoveryNone {
			t.Fatalf("bound %d: mechanical decision = %+v", bound, got)
		}
	}
}

func TestAuthorityConfirmedEventPayloadAndReleaseAttempt(t *testing.T) {
	order := answeredAuthorityOrder()
	decision := DecideAnsweredAuthorityRecovery(core.Task{State: core.TaskQueued}, order, answeredAuthorityDocuments())
	payload := AuthorityConfirmedEventPayload("ws", order, decision, 42)
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err = json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]any{"reason": "authority_confirmed", "automatic": true, "command": "", "release_attempt_id": "attempt-1", "release_event_id": float64(42), "prior_state": "queued", "new_state": "queued", "prior_outcome": "released"} {
		if decoded[key] != want {
			t.Fatalf("payload %s = %#v, want %#v", key, decoded[key], want)
		}
	}
	if _, ok := decoded["recovery_number"]; ok {
		t.Fatalf("payload names a ledger recovery number: %s", raw)
	}
	if got := ReleaseEventAttemptID(core.Event{Payload: core.JSONPayload(map[string]any{"attempt_id": "attempt-1"})}); got != "attempt-1" {
		t.Fatalf("release attempt = %q", got)
	}
}
