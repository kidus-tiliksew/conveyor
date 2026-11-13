package store

import (
	"context"
	"encoding/json"
	"errors"
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

// The row-11 disposition set holds each attempt of the verify order with an
// unresolved operation, with every such operation in ID order and the
// attempt's safe-input digest, subject, and state, so
// PrepareVerificationRecovery accepts a request built from it. Every
// checkpoint or record the validator would reject yields a refusal naming the
// limitation and no partial set (component-next-action invariant 15;
// component-verification-service invariant 45).
func TestVerificationDispositionSetMatchesTheRecoveryValidator(t *testing.T) {
	head := strings.Repeat("a", 40)
	safe := map[string]json.RawMessage{"target": json.RawMessage(`"fixture"`)}
	digest := verificationHash(verificationJSON(safe))
	base := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	baseTask := core.Task{ID: "task", Repo: "conveyor", ReviewedHeadSHA: head}
	baseOrder := core.WorkOrder{ID: "verify", TaskID: "task", Stage: core.StageVerify, State: core.WorkOrderQueued, HeadSHA: head, LastAttemptID: "attempt-1",
		Checkpoint: &core.WorkOrderCheckpoint{DecisionRequest: "Inspect the provider.", Verification: &core.WorkOrderVerificationCheckpoint{ContextID: "ctx", HeadSHA: head, OperationIDs: []string{"op-b", "op-a"}}}}
	contextRow := func(id, attempt string) VerificationRow {
		return VerificationRow{Table: "verification_contexts", ID: id, TaskID: "task", State: "sealed", Body: verificationJSON(VerificationContext{ID: id, TaskID: "task", WorkOrderID: "verify", WorkOrderAttemptID: attempt, Revisions: []core.VerificationRevision{{Repository: "conveyor", SHA: head}}})}
	}
	runRow := func(id, contextID, attempt string, minute int) VerificationRow {
		ended := base.Add(time.Duration(minute)*time.Minute + time.Second)
		return VerificationRow{Table: "verification_attempts", ID: id, TaskID: "task", ContextID: contextID, State: "blocked", Body: verificationJSON(VerificationAttempt{ID: id, ContextID: contextID, WorkOrderAttemptID: attempt, Subject: core.VerificationSubject{Kind: "kit", KitID: "provider", ExerciseID: id}, SafeInputs: safe, State: "blocked", StartedAt: base.Add(time.Duration(minute) * time.Minute), EndedAt: &ended})}
	}
	opRow := func(id, contextID, runID, state, inputDigest string) VerificationRow {
		return VerificationRow{Table: "verification_operations", ID: id, TaskID: "task", ContextID: contextID, RunID: runID, State: state, Body: verificationJSON(VerificationOperation{ID: id, ContextID: contextID, RunID: runID, InputDigest: inputDigest})}
	}
	baseRows := func() []VerificationRow {
		return []VerificationRow{contextRow("ctx", "attempt-1"), runRow("run-1", "ctx", "attempt-1", 2), runRow("run-2", "ctx", "attempt-1", 1),
			opRow("op-a", "ctx", "run-1", "prepared", digest), opRow("op-b", "ctx", "run-1", "dispatching", digest), opRow("op-done", "ctx", "run-2", "completed", digest)}
	}
	entry := func(runID string, operationIDs ...string) nextaction.Disposition {
		return nextaction.Disposition{ContextID: "ctx", RunID: runID, OperationIDs: append([]string{}, operationIDs...), InputDigest: digest, Subject: "kit:provider/" + runID, State: "blocked"}
	}
	request := func(set []nextaction.Disposition, dispositions ...string) *VerificationRecovery {
		r := &VerificationRecovery{Reason: "Operator inspected the provider."}
		for i, e := range set {
			r.Dispositions = append(r.Dispositions, VerificationRecoveryDisposition{ContextID: e.ContextID, RunID: e.RunID, OperationIDs: e.OperationIDs, InputDigest: e.InputDigest, Disposition: dispositions[i%len(dispositions)]})
		}
		return r
	}
	prepare := func(task core.Task, order core.WorkOrder, rows []VerificationRow, r *VerificationRecovery) ([]VerificationRow, []core.Event, error) {
		ctx := WithActor(WithVerificationRecovery(context.Background(), r), Actor{ID: "user:operator", Role: core.ActorUser})
		return PrepareVerificationRecovery(ctx, task, order, rows, "binding", time.Now().UTC())
	}

	got := verificationDispositionBinding(baseTask, baseOrder, baseRows())
	want := []nextaction.Disposition{entry("run-1", "op-a", "op-b")}
	if got.Refusal != "" || !reflect.DeepEqual(got.Set, want) {
		t.Fatalf("set = %+v refusal %q, want %+v", got.Set, got.Refusal, want)
	}
	changed, events, err := prepare(baseTask, baseOrder, baseRows(), request(got.Set, "applied"))
	if err != nil || len(changed) != 3 || len(events) != 1 {
		t.Fatalf("recovery built from the set = %d rows, %d events, %v; want 3 rows and 1 event", len(changed), len(events), err)
	}

	// Unresolved operations of two attempts form two entries, ordered by
	// attempt start time, and one recovery disposes both.
	twoRuns := baseRows()
	twoRuns[4] = opRow("op-b", "ctx", "run-2", "prepared", digest)
	got = verificationDispositionBinding(baseTask, baseOrder, twoRuns)
	want = []nextaction.Disposition{entry("run-2", "op-b"), entry("run-1", "op-a")}
	if got.Refusal != "" || !reflect.DeepEqual(got.Set, want) {
		t.Fatalf("two-run set = %+v refusal %q, want %+v", got.Set, got.Refusal, want)
	}
	changed, events, err = prepare(baseTask, baseOrder, twoRuns, request(got.Set, "applied", "not_applied"))
	if err != nil || len(changed) != 4 || len(events) != 2 {
		t.Fatalf("two-run recovery = %d rows, %d events, %v; want 4 rows and 2 events", len(changed), len(events), err)
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
			rows[1] = runRow("run-1", "ctx", "attempt-0", 2)
			return rows
		}, "no longer matches the attempt or authority"},
		{"other inputs", func(_ *core.Task, _ *core.WorkOrder, rows []VerificationRow) []VerificationRow {
			rows[4] = opRow("op-b", "ctx", "run-1", "prepared", "other")
			return rows
		}, "prepared over other inputs"},
		{"unreadable run", func(_ *core.Task, _ *core.WorkOrder, rows []VerificationRow) []VerificationRow {
			return append(rows, opRow("op-c", "ctx", "run-missing", "prepared", digest))
		}, "is not readable"},
		{"unreadable checkpoint operation", func(_ *core.Task, order *core.WorkOrder, rows []VerificationRow) []VerificationRow {
			checkpoint := *order.Checkpoint
			verification := *checkpoint.Verification
			verification.OperationIDs = []string{"op-missing"}
			checkpoint.Verification = &verification
			order.Checkpoint = &checkpoint
			return rows
		}, "is not readable"},
		{"unreadable context", func(_ *core.Task, _ *core.WorkOrder, rows []VerificationRow) []VerificationRow {
			return rows[1:]
		}, "is not readable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			task, order := baseTask, baseOrder
			rows := tc.edit(&task, &order, baseRows())
			got := verificationDispositionBinding(task, order, rows)
			if got.Set != nil || !strings.Contains(got.Refusal, tc.refusal) {
				t.Fatalf("set = %+v refusal %q, want refusal containing %q", got.Set, got.Refusal, tc.refusal)
			}
		})
	}
}

// PrepareVerificationRecovery refuses a request that does not name the
// order's disposition set exactly once, and stages nothing. A replay must
// match the whole verification object (component-verification-service
// invariant 45; DEC-133(1)).
func TestVerificationRecoveryRequiresTheWholeDispositionSet(t *testing.T) {
	head := strings.Repeat("a", 40)
	safe := map[string]json.RawMessage{}
	digest := verificationHash(verificationJSON(safe))
	base := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	task := core.Task{ID: "task", Repo: "conveyor", ReviewedHeadSHA: head}
	order := core.WorkOrder{ID: "verify", TaskID: "task", Stage: core.StageVerify, State: core.WorkOrderQueued, HeadSHA: head}
	ended := base.Add(time.Minute)
	attempt := func(id, obligation string, minute int, running bool) VerificationRow {
		run := VerificationAttempt{ID: id, ContextID: "ctx", WorkOrderAttemptID: "attempt-1", Subject: core.VerificationSubject{Kind: "ordinary", ObligationID: obligation}, SafeInputs: safe, State: "failed", StartedAt: base.Add(time.Duration(minute) * time.Minute), EndedAt: &ended}
		if running {
			run.State, run.EndedAt = "running", nil
		}
		return VerificationRow{Table: "verification_attempts", ID: id, TaskID: "task", ContextID: "ctx", State: run.State, Body: verificationJSON(run)}
	}
	obligation := func(id, policy string) VerificationRow {
		return VerificationRow{Table: "verification_obligations", ID: "ctx:" + id, TaskID: "task", ContextID: "ctx", Body: verificationJSON(VerificationObligation{ID: id, ContextID: "ctx", Contract: verification.Exercise{ID: id, RetryPolicy: policy}})}
	}
	rows := func() []VerificationRow {
		return []VerificationRow{
			{Table: "verification_contexts", ID: "ctx", TaskID: "task", State: "sealed", Body: verificationJSON(VerificationContext{ID: "ctx", TaskID: "task", WorkOrderID: "verify", WorkOrderAttemptID: "attempt-1", Revisions: []core.VerificationRevision{{Repository: "conveyor", SHA: head}}})},
			obligation("login", "operator_action_required"), obligation("checkout", "operator_action_required"), obligation("read", "safe_to_replay"),
			attempt("run-login", "login", 1, false), attempt("run-checkout", "checkout", 2, true), attempt("run-read", "read", 3, false),
		}
	}
	entry := func(runID, disposition string) VerificationRecoveryDisposition {
		return VerificationRecoveryDisposition{ContextID: "ctx", RunID: runID, OperationIDs: []string{}, InputDigest: digest, Disposition: disposition}
	}
	prepare := func(rs []VerificationRow, requestID string, r *VerificationRecovery) ([]VerificationRow, []core.Event, error) {
		ctx := WithActor(WithVerificationRecovery(context.Background(), r), Actor{ID: "user:operator", Role: core.ActorUser})
		return PrepareVerificationRecovery(ctx, task, order, rs, requestID, time.Now().UTC())
	}
	complete := &VerificationRecovery{Reason: "Operator restored both fixtures.", Dispositions: []VerificationRecoveryDisposition{entry("run-checkout", "not_applied"), entry("run-login", "applied")}}

	for name, r := range map[string]*VerificationRecovery{
		"omitted member":    {Reason: "One answer.", Dispositions: []VerificationRecoveryDisposition{entry("run-login", "applied")}},
		"duplicate attempt": {Reason: "Twice.", Dispositions: []VerificationRecoveryDisposition{entry("run-login", "applied"), entry("run-login", "not_applied"), entry("run-checkout", "applied")}},
		"run outside set":   {Reason: "Extra.", Dispositions: []VerificationRecoveryDisposition{entry("run-login", "applied"), entry("run-checkout", "applied"), entry("run-read", "applied")}},
		"empty list":        {Reason: "Nothing.", Dispositions: []VerificationRecoveryDisposition{}},
	} {
		changed, events, err := prepare(rows(), "refused", r)
		if !errors.Is(err, ErrVerificationInvalid) || changed != nil || events != nil {
			t.Fatalf("%s: rows %d events %d err %v, want an invalid refusal that stages nothing", name, len(changed), len(events), err)
		}
	}

	changed, events, err := prepare(rows(), "both", complete)
	if err != nil || len(changed) != 2 || len(events) != 2 {
		t.Fatalf("complete recovery = %d rows, %d events, %v", len(changed), len(events), err)
	}
	recorded := rows()
	for _, r := range changed {
		run := verificationDecode[VerificationAttempt](r)
		if len(run.Recovery) != 1 || run.Recovery[0].ID != "recovery:both" || run.Recovery[0].Reason != complete.Reason {
			t.Fatalf("attempt %s authorizations = %+v", run.ID, run.Recovery)
		}
		if run.ID == "run-checkout" && (r.State != "cancelled" || run.EndedAt == nil) {
			t.Fatalf("running member was not cancelled: state %s", r.State)
		}
		for i := range recorded {
			if recorded[i].Table == r.Table && recorded[i].ID == r.ID {
				recorded[i] = r
			}
		}
	}
	// Events follow the set's order: attempt start time, then run ID.
	var order0 struct {
		Disposition string                   `json:"disposition"`
		Original    core.VerificationBinding `json:"original"`
	}
	if err := json.Unmarshal(events[0].Payload, &order0); err != nil || order0.Original.RunID != "run-login" || order0.Disposition != "applied" {
		t.Fatalf("first event = %s, want run-login applied", events[0].Payload)
	}

	if changed, events, err = prepare(recorded, "both", complete); err != nil || changed != nil || events != nil {
		t.Fatalf("whole-object replay = %d rows, %d events, %v; want an idempotent no-op", len(changed), len(events), err)
	}
	changedReason := *complete
	changedReason.Reason = "Another reason."
	partial := &VerificationRecovery{Reason: complete.Reason, Dispositions: complete.Dispositions[:1]}
	flipped := &VerificationRecovery{Reason: complete.Reason, Dispositions: []VerificationRecoveryDisposition{entry("run-checkout", "applied"), entry("run-login", "applied")}}
	for name, r := range map[string]*VerificationRecovery{"changed reason": &changedReason, "missing entry": partial, "changed disposition": flipped} {
		if changed, events, err = prepare(recorded, "both", r); !errors.Is(err, ErrVerificationConflict) || changed != nil || events != nil {
			t.Fatalf("%s replay = %d rows, %d events, %v; want a conflict", name, len(changed), len(events), err)
		}
	}
	// The recorded authorizations are unused, so the set is now empty.
	if got, err := verificationDispositionSet(task, order, recorded); err != nil || len(got) != 0 {
		t.Fatalf("set after recovery = %+v, err = %v", got, err)
	}
}

// The set also holds the latest attempt of each operator_action_required
// subject among the verify order's contexts that holds no unused
// gate-acceptable authorization, whatever its state, ordered by start time
// with the attempts that hold unresolved operations, each attempt once
// (component-verification-service invariant 45; req-verification-kits
// AC-3.4).
func TestVerificationDispositionSetHoldsEveryRetryGatedAttempt(t *testing.T) {
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
	member := func(runID, contextID, obligation, state string, operationIDs ...string) nextaction.Disposition {
		return nextaction.Disposition{ContextID: contextID, RunID: runID, OperationIDs: append([]string{}, operationIDs...), InputDigest: digest, Subject: "ordinary:" + obligation, State: state}
	}

	got := verificationDispositionBinding(task, order, baseRows())
	if got.Refusal != "" || !reflect.DeepEqual(got.Set, []nextaction.Disposition{member("run-a", "ctx-a", "snapshot", "succeeded")}) {
		t.Fatalf("set = %+v refusal %q, want run-a", got.Set, got.Refusal)
	}
	request := &VerificationRecovery{Reason: "Succeeded before claim loss.", Dispositions: []VerificationRecoveryDisposition{{ContextID: "ctx-a", RunID: "run-a", OperationIDs: got.Set[0].OperationIDs, InputDigest: got.Set[0].InputDigest, Disposition: "applied"}}}
	ctx := WithActor(WithVerificationRecovery(context.Background(), request), Actor{ID: "user:operator", Role: core.ActorUser})
	if _, _, err := PrepareVerificationRecovery(ctx, task, order, baseRows(), "binding", time.Now().UTC()); err != nil {
		t.Fatalf("recovery built from the gated set refused: %v", err)
	}

	cases := []struct {
		name string
		rows func([]VerificationRow) []VerificationRow
		want []nextaction.Disposition
	}{
		{"latest attempt of the subject", func(rows []VerificationRow) []VerificationRow {
			return append(rows, runRow("run-a2", "ctx-a", "attempt-1", "snapshot", 2))
		}, []nextaction.Disposition{member("run-a2", "ctx-a", "snapshot", "succeeded")}},
		{"every gated subject by start time", func(rows []VerificationRow) []VerificationRow {
			return append(rows, obligationRow("ctx-a", "login", "operator_action_required"), runRow("run-login", "ctx-a", "attempt-1", "login", 0))
		}, []nextaction.Disposition{member("run-login", "ctx-a", "login", "succeeded"), member("run-a", "ctx-a", "snapshot", "succeeded")}},
		{"gated subjects across contexts", func(rows []VerificationRow) []VerificationRow {
			return append(rows, obligationRow("ctx-b", "login", "operator_action_required"), runRow("run-login-b", "ctx-b", "attempt-2", "login", 3, func(run *VerificationAttempt) { run.State = "blocked" }))
		}, []nextaction.Disposition{member("run-a", "ctx-a", "snapshot", "succeeded"), member("run-login-b", "ctx-b", "login", "blocked")}},
		{"unused authorization leaves the subject out", func(rows []VerificationRow) []VerificationRow {
			return append(rows, obligationRow("ctx-a", "login", "operator_action_required"), runRow("run-login", "ctx-a", "attempt-1", "login", 0, authorized(false)))
		}, []nextaction.Disposition{member("run-a", "ctx-a", "snapshot", "succeeded")}},
		{"consumed authorization joins again", func(rows []VerificationRow) []VerificationRow {
			rows[4] = runRow("run-a", "ctx-a", "attempt-1", "snapshot", 1, authorized(true))
			return rows
		}, []nextaction.Disposition{member("run-a", "ctx-a", "snapshot", "succeeded")}},
		{"running attempt", func(rows []VerificationRow) []VerificationRow {
			rows[4] = runRow("run-a", "ctx-a", "attempt-1", "snapshot", 1, running)
			return rows
		}, []nextaction.Disposition{member("run-a", "ctx-a", "snapshot", "running")}},
		{"unresolved operation on the gated run is one entry", func(rows []VerificationRow) []VerificationRow {
			return append(rows, VerificationRow{Table: "verification_operations", ID: "op", TaskID: "task", ContextID: "ctx-a", RunID: "run-a", State: "prepared", Body: verificationJSON(VerificationOperation{ID: "op", ContextID: "ctx-a", RunID: "run-a", InputDigest: digest})})
		}, []nextaction.Disposition{member("run-a", "ctx-a", "snapshot", "succeeded", "op")}},
		{"unused authorization", func(rows []VerificationRow) []VerificationRow {
			rows[4] = runRow("run-a", "ctx-a", "attempt-1", "snapshot", 1, authorized(false))
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
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := verificationDispositionBinding(task, order, tc.rows(baseRows()))
			if tc.want == nil {
				if got.Set != nil || !strings.Contains(got.Refusal, "has no unresolved operations") {
					t.Fatalf("set = %+v refusal %q, want the empty-set refusal", got.Set, got.Refusal)
				}
				return
			}
			if got.Refusal != "" || !reflect.DeepEqual(got.Set, tc.want) {
				t.Fatalf("set = %+v refusal %q, want %+v", got.Set, got.Refusal, tc.want)
			}
		})
	}
}
