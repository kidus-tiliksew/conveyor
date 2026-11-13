package nextaction

import (
	"reflect"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// A disposition marker selects row 11 only when a verify order can carry the
// record_dispositions choice, and row 11 then names the verify order released
// at its verification checkpoint, never a later review order or a later
// verify order. A marker with no queued or claimed verify order behind it
// falls through to the task's other rows instead of offering a refused choice
// (component-next-action invariant 15; task 261030-54fbd3).
func TestVerificationDispositionNeedsAnOrderToDispose(t *testing.T) {
	now := time.Date(2026, 10, 30, 17, 35, 0, 0, time.UTC)
	checkpoint := &core.WorkOrderCheckpoint{Verification: &core.WorkOrderVerificationCheckpoint{ContextID: "ctx", OperationIDs: []string{"op"}}}
	atCheckpoint := core.WorkOrder{ID: "verify-1", Stage: core.StageVerify, State: core.WorkOrderQueued, LastAttemptID: "a", LastAttemptOutcome: core.WorkOrderOutcomeReleased, RetrySuppressed: true, Checkpoint: checkpoint, QueueDeadline: now.Add(time.Hour)}
	review := core.WorkOrder{ID: "review-1-seat-1", Stage: core.StageReview, State: core.WorkOrderQueued, Claimable: true, QueueDeadline: now.Add(time.Hour)}
	binding := map[string]DispositionInput{"verify-1": {Set: []Disposition{{ContextID: "ctx", RunID: "run", OperationIDs: []string{"op"}, InputDigest: "d", Subject: "ordinary:o", State: "blocked"}}}}

	for name, orders := range map[string][]core.WorkOrder{
		"verify order alone":                  {atCheckpoint},
		"with a later unclaimed review order": {atCheckpoint, review},
	} {
		in := Input{Task: core.Task{ID: "t", State: core.TaskRunning}, Markers: Markers{VerificationDisposition: true}, Orders: orders, Dispositions: binding, Capabilities: allCaps(), Now: now}
		got := Project(in)
		if got.Kind != KindVerificationDisposition || got.CurrentOrder == nil || got.CurrentOrder.ID != "verify-1" {
			t.Fatalf("%s: next action = %s on %+v, want verification_disposition on verify-1", name, got.Kind, got.CurrentOrder)
		}
		for _, id := range []string{"record_dispositions"} {
			choice, ok := choiceByID(got, id)
			if !ok || choice.Path != "/v1/work-orders/verify-1/recover" || !choice.Allowed {
				t.Fatalf("%s: %s = %+v (refusals %+v), want an allowed recover of verify-1", name, id, choice, got.Details.Refusals)
			}
		}
	}

	// A later verify order without the checkpoint does not displace the
	// order the disposition set was read for.
	later := core.WorkOrder{ID: "verify-2", Stage: core.StageVerify, State: core.WorkOrderQueued, QueueDeadline: now.Add(time.Hour)}
	in := Input{Task: core.Task{ID: "t", State: core.TaskRunning}, Markers: Markers{VerificationDisposition: true}, Orders: []core.WorkOrder{atCheckpoint, later}, Dispositions: binding, Capabilities: allCaps(), Now: now}
	if got := Project(in); got.Kind != KindVerificationDisposition || got.CurrentOrder == nil || got.CurrentOrder.ID != "verify-1" {
		t.Fatalf("later verify order: next action = %s on %+v, want verification_disposition on verify-1", got.Kind, got.CurrentOrder)
	}

	// The verify order moved on after recovery; its checkpoint is history.
	for _, state := range []core.WorkOrderState{core.WorkOrderSubmitted, core.WorkOrderCompleted, core.WorkOrderCancelled} {
		done := atCheckpoint
		done.State, done.LastAttemptOutcome, done.RetrySuppressed = state, "", false
		in := Input{Task: core.Task{ID: "t", State: core.TaskRunning}, Markers: Markers{VerificationDisposition: true}, Orders: []core.WorkOrder{done, review}, Capabilities: allCaps(), Now: now}
		got := Project(in)
		if got.Kind == KindVerificationDisposition || hasSecondary(got, KindVerificationDisposition) {
			t.Fatalf("verify order %s: next action = %s secondary %+v, want no verification_disposition", state, got.Kind, got.Details.Secondary)
		}
		if hasRefusalText(got, "record_dispositions", "no order to recover") {
			t.Fatalf("verify order %s: refusals %+v", state, got.Details.Refusals)
		}
	}
}

func TestAtVerificationCheckpoint(t *testing.T) {
	checkpoint := &core.WorkOrderCheckpoint{Verification: &core.WorkOrderVerificationCheckpoint{ContextID: "ctx"}}
	base := core.WorkOrder{Stage: core.StageVerify, State: core.WorkOrderQueued, LastAttemptOutcome: core.WorkOrderOutcomeReleased, Checkpoint: checkpoint}
	if !AtVerificationCheckpoint(base) {
		t.Fatal("a queued verify order released at its verification checkpoint is at the checkpoint")
	}
	for name, mutate := range map[string]func(*core.WorkOrder){
		"recovered":           func(o *core.WorkOrder) { o.LastAttemptOutcome = "" },
		"expired":             func(o *core.WorkOrder) { o.LastAttemptOutcome = core.WorkOrderOutcomeExpired },
		"claimed":             func(o *core.WorkOrder) { o.State = core.WorkOrderClaimed },
		"submitted":           func(o *core.WorkOrder) { o.State = core.WorkOrderSubmitted },
		"completed":           func(o *core.WorkOrder) { o.State = core.WorkOrderCompleted },
		"no checkpoint":       func(o *core.WorkOrder) { o.Checkpoint = nil },
		"decision checkpoint": func(o *core.WorkOrder) { o.Checkpoint = &core.WorkOrderCheckpoint{DecisionRequest: "Confirm."} },
		"implement stage":     func(o *core.WorkOrder) { o.Stage = core.StageImplement },
	} {
		order := base
		mutate(&order)
		if AtVerificationCheckpoint(order) {
			t.Fatalf("%s: reported at the verification checkpoint", name)
		}
	}
}

// sealedCheckpointInput is a verify order released at its sealed
// verification checkpoint with the decision request the seal writes, so both
// the agent-question marker and the disposition marker are set, as the
// assembler folds them (component-next-action invariant 15).
func sealedCheckpointInput(now time.Time, input *DispositionInput) Input {
	request := "Two exercises need an operator\nRequired operator action: Restore the fixtures"
	checkpoint := &core.WorkOrderCheckpoint{DecisionRequest: request, Verification: &core.WorkOrderVerificationCheckpoint{ContextID: "ctx", OperationIDs: []string{"op"}}}
	order := core.WorkOrder{ID: "verify-1", Stage: core.StageVerify, State: core.WorkOrderQueued, LastAttemptID: "a", LastAttemptOutcome: core.WorkOrderOutcomeReleased, RetrySuppressed: true, Checkpoint: checkpoint, QueueDeadline: now.Add(time.Hour)}
	in := Input{
		Task:         core.Task{ID: "t", State: core.TaskRunning},
		Markers:      Markers{CheckpointDecision: true, Checkpoint: request, VerificationDisposition: true},
		Orders:       []core.WorkOrder{order},
		Capabilities: allCaps(),
		Now:          now,
	}
	if input != nil {
		in.Dispositions = map[string]DispositionInput{order.ID: *input}
	}
	return in
}

func sealedCheckpointSet() []Disposition {
	return []Disposition{
		{ContextID: "ctx", RunID: "run-login", OperationIDs: []string{}, InputDigest: "d1", Subject: "ordinary:login", State: "blocked"},
		{ContextID: "ctx", RunID: "run-checkout", OperationIDs: []string{"op"}, InputDigest: "d2", Subject: "ordinary:checkout", State: "blocked"},
	}
}

// A released verify order whose disposition set has at least one entry
// matches row 11 and never row 10. The statement carries the order's decision
// request in details.checkpoint, the whole set, and only record_dispositions,
// and agent_question is no secondary match. An empty or refused set keeps the
// decision request at row 10 with no set (component-next-action invariant 3
// row 10, invariant 15; DEC-133(1)).
func TestSealedCheckpointOffersDispositionsBeforeTheQuestion(t *testing.T) {
	now := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	set := sealedCheckpointSet()
	in := sealedCheckpointInput(now, &DispositionInput{Set: set})
	got := Project(in)
	if got.Kind != KindVerificationDisposition || got.Actor != ActorOperator || !got.Judgment || got.CurrentOrder == nil || got.CurrentOrder.ID != "verify-1" {
		t.Fatalf("sealed checkpoint = %s/%s on %+v, want operator verification_disposition on verify-1", got.Actor, got.Kind, got.CurrentOrder)
	}
	if got.Details.Checkpoint != in.Orders[0].Checkpoint.DecisionRequest {
		t.Fatalf("details.checkpoint = %q, want the order's decision request", got.Details.Checkpoint)
	}
	if !reflect.DeepEqual(got.Details.Dispositions, set) {
		t.Fatalf("dispositions = %+v, want %+v", got.Details.Dispositions, set)
	}
	if len(got.Choices) != 1 || got.Choices[0].ID != "record_dispositions" || !got.Choices[0].Allowed || got.Choices[0].Path != "/v1/work-orders/verify-1/recover" {
		t.Fatalf("choices = %+v, want one allowed record_dispositions", got.Choices)
	}
	if hasSecondary(got, KindAgentQuestion) || got.Details.ReleaseOutcome != "" || got.Details.CheckpointClass != "" {
		t.Fatalf("details = %+v, want no agent_question secondary or release outcome", got.Details)
	}

	// The assembler's first projection holds no set. Row 11 keeps the order,
	// so the assembler reads the set, on a compact read too, and projects
	// again.
	unread := Project(sealedCheckpointInput(now, nil))
	if unread.Kind != KindVerificationDisposition || unread.Details.Checkpoint != got.Details.Checkpoint || hasChoice(unread, "record_dispositions") || !hasRefusal(unread, "record_dispositions") {
		t.Fatalf("unread set = %s checkpoint %q choices %+v, want row 11 with record_dispositions withheld", unread.Kind, unread.Details.Checkpoint, unread.Choices)
	}
	if !reflect.DeepEqual(CompactFrom(unread), CompactFrom(got)) {
		t.Fatalf("compact unread = %+v, want %+v", CompactFrom(unread), CompactFrom(got))
	}

	for name, input := range map[string]DispositionInput{
		"empty set":               {},
		"refused set":             {Refusal: "verify order verify-1 has no unresolved operations and no retry-gated attempts to dispose"},
		"set with a refusal":      {Set: set, Refusal: "verification records are not readable from this store"},
		"empty set and a refusal": {Set: []Disposition{}, Refusal: "the disposition set of verify order verify-1 cannot be stated"},
	} {
		got := Project(sealedCheckpointInput(now, &input))
		if got.Kind != KindAgentQuestion || got.Details.Checkpoint != in.Orders[0].Checkpoint.DecisionRequest || got.Details.ReleaseOutcome != ReleaseCheckpoint {
			t.Fatalf("%s: = %s checkpoint %q, want agent_question with the decision request", name, got.Kind, got.Details.Checkpoint)
		}
		if got.Details.Dispositions != nil || hasChoice(got, "record_dispositions") || !hasSecondary(got, KindVerificationDisposition) {
			t.Fatalf("%s: dispositions %+v choices %+v secondary %+v, want no set and row 11 secondary", name, got.Details.Dispositions, got.Choices, got.Details.Secondary)
		}
	}
}

// Only the verify order that row 11 selects gives up its question. A later
// queued order released with its own question keeps row 10 whatever an
// older verify order's set holds, a question on a verify order without a
// sealed verification checkpoint keeps row 10, and a higher row still wins
// over a sealed checkpoint with a set (component-next-action invariant 3).
func TestSealedCheckpointPrecedenceIsNarrow(t *testing.T) {
	now := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	set := sealedCheckpointSet()

	later := sealedCheckpointInput(now, &DispositionInput{Set: set})
	later.Orders = append(later.Orders, core.WorkOrder{ID: "implement-2", Stage: core.StageImplement, State: core.WorkOrderQueued, LastAttemptID: "b", LastAttemptOutcome: core.WorkOrderOutcomeReleased, Checkpoint: &core.WorkOrderCheckpoint{DecisionRequest: "Which branch?"}, QueueDeadline: now.Add(time.Hour)})
	later.Markers.Checkpoint = "Which branch?"
	if got := Project(later); got.Kind != KindAgentQuestion || got.CurrentOrder == nil || got.CurrentOrder.ID != "implement-2" || got.Details.Checkpoint != "Which branch?" || !hasSecondary(got, KindVerificationDisposition) {
		t.Fatalf("later question = %s on %+v checkpoint %q, want agent_question on implement-2", got.Kind, got.CurrentOrder, got.Details.Checkpoint)
	}

	plain := sealedCheckpointInput(now, &DispositionInput{Set: set})
	plain.Orders[0].Checkpoint = &core.WorkOrderCheckpoint{DecisionRequest: "Confirm the cited requirement."}
	plain.Markers.Checkpoint = "Confirm the cited requirement."
	if got := Project(plain); got.Kind != KindAgentQuestion || got.Details.Checkpoint != "Confirm the cited requirement." {
		t.Fatalf("verify question without a verification checkpoint = %s, want agent_question", got.Kind)
	}

	implement := checkpointInput(now, "", nil, nil)
	implement.Dispositions = map[string]DispositionInput{"question": {Set: set}}
	if got := Project(implement); got.Kind != KindAgentQuestion || got.Details.Dispositions != nil {
		t.Fatalf("implement question = %s dispositions %+v, want agent_question without a set", got.Kind, got.Details.Dispositions)
	}

	gate := sealedCheckpointInput(now, &DispositionInput{Set: set})
	gate.Task.State, gate.Task.GateKind = core.TaskAwaiting, core.GateKindPlanRevision
	if got := Project(gate); got.Kind != KindPlanRevision || got.Details.Dispositions != nil {
		t.Fatalf("plan revision gate = %s, want plan_revision", got.Kind)
	}
	merging := sealedCheckpointInput(now, &DispositionInput{Set: set})
	merging.Task.State = core.TaskApproved
	if got := Project(merging); got.Kind != KindMerging || !hasSecondary(got, KindVerificationDisposition) || hasSecondary(got, KindAgentQuestion) {
		t.Fatalf("approved task = %s secondary %+v, want merging with row 11 secondary only", got.Kind, got.Details.Secondary)
	}

	// Without a decision request the sealed order keeps its row 11 statement
	// with no checkpoint text.
	bare := sealedCheckpointInput(now, &DispositionInput{Set: set})
	bare.Orders[0].Checkpoint.DecisionRequest = ""
	bare.Markers.CheckpointDecision, bare.Markers.Checkpoint = false, ""
	if got := Project(bare); got.Kind != KindVerificationDisposition || got.Details.Checkpoint != "" || !reflect.DeepEqual(got.Details.Dispositions, set) {
		t.Fatalf("no decision request = %s checkpoint %q, want verification_disposition without checkpoint text", got.Kind, got.Details.Checkpoint)
	}
}

// assembleForTest runs the assembler's two projections: the first without a
// set, then, when row 11 wins and the read reads the set, a second with the
// set the store would state. A compact read reads the set only when
// DispositionDecidesRow holds (component-next-action invariant 15).
func assembleForTest(in Input, input DispositionInput, compact bool) NextAction {
	in.Dispositions = nil
	first := Project(in)
	if first.Kind != KindVerificationDisposition || first.CurrentOrder == nil || (compact && !DispositionDecidesRow(in)) {
		return first
	}
	in.Dispositions = map[string]DispositionInput{first.CurrentOrder.ID: input}
	return Project(in)
}

// A compact read and a full read of the same sealed checkpoint state the same
// compact fields for a usable set, an empty set, and a refused set, with and
// without the decision request. A usable set with a decision request states
// row 11; an empty or refused set with one states row 10.
func TestSealedCheckpointCompactParity(t *testing.T) {
	now := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	set := sealedCheckpointSet()
	inputs := map[string]DispositionInput{
		"usable set":         {Set: set},
		"empty set":          {Refusal: "verify order verify-1 has no unresolved operations and no retry-gated attempts to dispose"},
		"refused set":        {Refusal: "verification records are not readable from this store"},
		"set with a refusal": {Set: set, Refusal: "verification records are not readable from this store"},
	}
	for name, input := range inputs {
		for _, question := range []bool{true, false} {
			in := sealedCheckpointInput(now, nil)
			if !question {
				in.Orders[0].Checkpoint.DecisionRequest = ""
				in.Markers.CheckpointDecision, in.Markers.Checkpoint = false, ""
			}
			full := assembleForTest(in, input, false)
			compact := assembleForTest(in, input, true)
			if !reflect.DeepEqual(CompactFrom(compact), CompactFrom(full)) {
				t.Fatalf("%s question=%t: compact %+v, full %+v", name, question, CompactFrom(compact), CompactFrom(full))
			}
			want := KindVerificationDisposition
			if question && (len(input.Set) == 0 || input.Refusal != "") {
				want = KindAgentQuestion
			}
			if full.Kind != want {
				t.Fatalf("%s question=%t: kind %s, want %s", name, question, full.Kind, want)
			}
		}
	}
}

// DispositionDecidesRow holds only for the sealed verify order row 11
// selects while it is the latest queued order and carries a decision
// request; every other shape lets a compact read skip the set.
func TestDispositionDecidesRow(t *testing.T) {
	now := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	if !DispositionDecidesRow(sealedCheckpointInput(now, nil)) {
		t.Fatal("a sealed verify order with a decision request does not decide the row")
	}
	for name, mutate := range map[string]func(*Input){
		"no decision request": func(in *Input) {
			in.Orders[0].Checkpoint.DecisionRequest = ""
			in.Markers.CheckpointDecision = false
		},
		"no verification checkpoint": func(in *Input) {
			in.Orders[0].Checkpoint = &core.WorkOrderCheckpoint{DecisionRequest: "Confirm."}
		},
		"recovered": func(in *Input) { in.Orders[0].LastAttemptOutcome = "" },
		"claimed":   func(in *Input) { in.Orders[0].State = core.WorkOrderClaimed },
		"later queued question": func(in *Input) {
			in.Orders = append(in.Orders, core.WorkOrder{ID: "implement-2", Stage: core.StageImplement, State: core.WorkOrderQueued, LastAttemptOutcome: core.WorkOrderOutcomeReleased, Checkpoint: &core.WorkOrderCheckpoint{DecisionRequest: "Which branch?"}})
		},
		"implement question": func(in *Input) {
			*in = checkpointInput(now, "", nil, nil)
		},
	} {
		in := sealedCheckpointInput(now, nil)
		mutate(&in)
		if DispositionDecidesRow(in) {
			t.Fatalf("%s: decides the row", name)
		}
	}
}
