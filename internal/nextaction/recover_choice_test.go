package nextaction

import (
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// Every choice on the recover route asks the route's own queued eligibility,
// so a queued order the route refuses offers none of them and records the
// route's refusal text in details.refusals (req-operator-next-action AC-1.4;
// feature-operator-next-action NA-3; component-work-orders, Recovery).
func TestRecoverRouteChoicesFollowTheRouteEligibility(t *testing.T) {
	now := time.Date(2026, 10, 17, 10, 25, 46, 0, time.UTC)
	ineligible := core.WorkOrder{ID: "o", State: core.WorkOrderQueued, Stage: core.StageImplement, Claimable: true, RedispatchCount: 1, OperatorDirection: "Continue."}
	routeText := "work order o is not released, expired, or retry-suppressed"
	if got := core.WorkOrderRecoverStateRefusal(ineligible); got != routeText {
		t.Fatalf("route refusal = %q, want %q", got, routeText)
	}
	sets := map[string][]ChoiceSpec{
		"answer":      answerChoices(),
		"recover":     stalledChoices(),
		"applied":     dispositionChoices(),
		"not_applied": dispositionChoices(),
	}
	in := Input{Task: core.Task{ID: "t", State: core.TaskRunning}, Capabilities: allCaps(), Now: now}
	for id, specs := range sets {
		order := ineligible
		choices, refusals := selectChoices(in, specs, &order)
		for _, choice := range choices {
			if choice.ID == id {
				t.Fatalf("%s offered on an ineligible queued order: %+v", id, choices)
			}
		}
		if !refusedWith(refusals, id, routeText) {
			t.Fatalf("%s refusals = %+v, want %q", id, refusals, routeText)
		}
	}

	// Each queued eligibility flag alone admits the recover-route choices.
	later := now.Add(time.Minute)
	for name, mutate := range map[string]func(*core.WorkOrder){
		"released":         func(o *core.WorkOrder) { o.LastAttemptOutcome = core.WorkOrderOutcomeReleased },
		"retry-suppressed": func(o *core.WorkOrder) { o.RetrySuppressed = true },
		"retry scheduled":  func(o *core.WorkOrder) { o.NextRetryAt = later },
	} {
		order := ineligible
		mutate(&order)
		for _, specs := range [][]ChoiceSpec{answerChoices(), stalledChoices()} {
			choices, refusals := selectChoices(in, specs, &order)
			if len(choices) == 0 || choices[0].Path != "/v1/work-orders/o/recover" {
				t.Fatalf("%s: choices = %+v refusals = %+v, want the recover-route choice", name, choices, refusals)
			}
		}
	}

	// Row 10 on a verify order the route refuses lists neither disposition.
	verify := Input{
		Task:         core.Task{ID: "t", State: core.TaskRunning},
		Markers:      Markers{VerificationDisposition: true},
		Orders:       []core.WorkOrder{{ID: "v", State: core.WorkOrderQueued, Stage: core.StageVerify}},
		Dispositions: map[string]DispositionInput{"v": {Binding: &Disposition{ContextID: "ctx", RunID: "run", OperationIDs: []string{"op"}}}},
		Capabilities: allCaps(), Now: now,
	}
	got := Project(verify)
	if got.Kind != KindVerificationDisposition {
		t.Fatalf("kind = %s, want verification_disposition", got.Kind)
	}
	for _, id := range []string{"applied", "not_applied"} {
		if hasChoice(got, id) || !hasRefusalText(got, id, "work order v is not released, expired, or retry-suppressed") {
			t.Fatalf("row 10 %s: choices %+v refusals %+v", id, got.Choices, got.Details.Refusals)
		}
	}
}

func refusedWith(refusals []Refusal, id, text string) bool {
	for _, refusal := range refusals {
		if refusal.Choice == id && refusal.Text == text {
			return true
		}
	}
	return false
}

// A triage-parked task offers Recover on the task-scoped redispatch route
// with no current order, first and allowed only under recover_work, while the
// order-scoped recover of the stalled and agent-question sets still refuses
// without an order (feature-operator-next-action NA-3; req-operator-next-action
// AC-1.4; task 261021-cd3bf8).
func TestParkedRecoverNeedsNoOrder(t *testing.T) {
	now := time.Date(2026, 10, 21, 7, 0, 0, 0, time.UTC)
	parked := Input{Task: core.Task{ID: "t", State: core.TaskParked}, Markers: Markers{ParkCommand: "triage.park"}, Capabilities: allCaps(), Now: now}
	got := Project(parked)
	if got.Kind != KindParked {
		t.Fatalf("kind = %s, want parked", got.Kind)
	}
	ids := make([]string, 0, len(got.Choices))
	for _, choice := range got.Choices {
		ids = append(ids, choice.ID)
	}
	if len(ids) != 3 || ids[0] != "recover" || ids[1] != "close" || ids[2] != "start_over" {
		t.Fatalf("choices = %q, want recover, close, start_over (refusals %+v)", ids, got.Details.Refusals)
	}
	recover := got.Choices[0]
	if recover.Method != "POST" || recover.Path != "/v1/tasks/t/redispatch" || recover.Capability != string(core.CapabilityRecoverWork) || !recover.Allowed {
		t.Fatalf("recover = %+v, want an allowed POST /v1/tasks/t/redispatch under recover_work", recover)
	}
	for _, refusal := range got.Details.Refusals {
		if refusal.Choice == "recover" {
			t.Fatalf("parked recover refused: %+v", refusal)
		}
	}

	withoutRecover := parked
	withoutRecover.Capabilities = map[core.Capability]bool{core.CapabilityOperateGates: true}
	denied := Project(withoutRecover)
	if len(denied.Choices) == 0 || denied.Choices[0].ID != "recover" || denied.Choices[0].Allowed {
		t.Fatalf("without recover_work: choices = %+v, want recover listed and not allowed", denied.Choices)
	}

	in := Input{Task: core.Task{ID: "t", State: core.TaskRunning}, Capabilities: allCaps(), Now: now}
	for name, specs := range map[string][]ChoiceSpec{"stalled": stalledChoices(), "agent question": answerChoices()} {
		choices, refusals := selectChoices(in, specs, nil)
		for _, choice := range choices {
			if choice.ID == "recover" || choice.ID == "answer" {
				t.Fatalf("%s: order-scoped %s offered without an order", name, choice.ID)
			}
		}
		id := "recover"
		if name == "agent question" {
			id = "answer"
		}
		if !refusedWith(refusals, id, "no order to recover") {
			t.Fatalf("%s: refusals = %+v, want %s refused with no order to recover", name, refusals, id)
		}
	}
}
