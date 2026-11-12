package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// planLedger builds a ledger and its interventions for derivePlanCycles. Each
// decision records the intervention and its intervention.<action> event with
// the same action, time, actor, reason code, and comment, as every backend
// writes them.
type planLedger struct {
	events        []core.Event
	interventions []core.Intervention
	nextEvent     int64
	nextDecision  int64
}

func (l *planLedger) event(seconds int, kind, actor, payload string) int64 {
	l.nextEvent++
	event := summaryEvent(l.nextEvent, seconds, kind, "", payload)
	if actor != "" {
		event.ActorID, event.ActorRole = actor, core.ActorHuman
	}
	l.events = append(l.events, event)
	return l.nextEvent
}

func (l *planLedger) decide(seconds int, action core.InterventionAction, reason, comment, actor string) int64 {
	l.nextDecision++
	role := core.ActorHuman
	if !strings.HasPrefix(actor, "user:") {
		role = core.ActorSystem
	}
	at := summaryEpoch.Add(time.Duration(seconds) * time.Second)
	l.interventions = append(l.interventions, core.Intervention{ID: l.nextDecision, TaskID: "summary-task", ActorID: actor, ActorRole: role,
		Action: action, ReasonCode: reason, Comment: comment, At: at})
	payload, _ := json.Marshal(map[string]string{"reason_code": reason, "comment": comment})
	l.nextEvent++
	l.events = append(l.events, core.Event{ID: l.nextEvent, TaskID: "summary-task", Kind: "intervention." + string(action),
		ActorID: actor, ActorRole: role, At: at, Payload: payload})
	return l.nextDecision
}

func (l *planLedger) cycles() []taskPlanCycle {
	// The fold never relies on slice order: reverse both inputs.
	events := make([]core.Event, len(l.events))
	for i, event := range l.events {
		events[len(events)-1-i] = event
	}
	interventions := make([]core.Intervention, len(l.interventions))
	for i, intervention := range l.interventions {
		interventions[len(interventions)-1-i] = intervention
	}
	cycles := derivePlanCycles(events, interventions)
	resolvePlanCyclePeople(cycles, nil)
	return cycles
}

// stepLine prints a step compactly: kind, version, person, decision, text.
func stepLine(step taskPlanCycleStep) string {
	parts := []string{step.Kind}
	if step.PlanVersion != nil {
		parts = append(parts, fmt.Sprintf("v%d", *step.PlanVersion))
	}
	if step.UserID != "" {
		parts = append(parts, "by "+step.UserID)
	}
	if step.InterventionID != nil {
		parts = append(parts, fmt.Sprintf("i%d", *step.InterventionID))
	}
	parts = append(parts, "e"+step.EventID)
	if step.Text != "" {
		parts = append(parts, fmt.Sprintf("%q", step.Text))
	}
	return strings.Join(parts, " ")
}

func versionText(version *int) string {
	if version == nil {
		return "-"
	}
	return fmt.Sprintf("%d", *version)
}

// cycleLines prints each cycle header and its steps.
func cycleLines(cycles []taskPlanCycle) []string {
	var lines []string
	for _, cycle := range cycles {
		lines = append(lines, fmt.Sprintf("%s contested=%s versions=%s..%s outcome=%s", cycle.OpenedBy,
			versionText(cycle.ContestedVersion), versionText(cycle.FirstVersion), versionText(cycle.FinalVersion), cycle.Outcome))
		for _, step := range cycle.Steps {
			lines = append(lines, "  "+stepLine(step))
		}
	}
	return lines
}

func requireCycles(t *testing.T, seen map[string]bool, got []taskPlanCycle, want ...string) {
	t.Helper()
	for _, cycle := range got {
		seen["outcome "+cycle.Outcome] = true
		for _, step := range cycle.Steps {
			seen["kind "+step.Kind] = true
		}
	}
	lines := cycleLines(got)
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("plan cycles\n got:\n%s\nwant:\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
}

const (
	planOperator = "user:usr_meron"
	planOwner    = "user:usr_abel"
)

// TestPlanCyclesFold covers every step kind and outcome of
// component-web-task-surfaces invariant 67 (DEC-130(1); req-260809-a40c75
// AC-4.1, AC-4.2).
func TestPlanCyclesFold(t *testing.T) {
	seen := map[string]bool{}
	t.Run("intake plan then a revision keeps recorded versions 1 and 2", func(t *testing.T) {
		var l planLedger
		l.event(0, core.EventTaskPlanStateChanged, "", `{"from":null,"to":"awaiting_approval","cause":"intake"}`)
		l.event(0, "spec.version_created", planOperator, `{"version":1,"acceptance_count":0,"origin":"intake"}`)
		l.decide(0, core.InterventionApprove, core.IntakePlanApprovalReasonCode, "", planOperator)
		l.event(0, "spec.version_approved", "", `{"version":1}`)
		l.event(10, "work_order.plan_revision_requested", planOperator, `{"rationale":"The plan names a missing file.","plan_version":1}`)
		l.decide(20, core.InterventionRedirect, "plan-revision-approved", "Go ahead.", planOperator)
		l.event(20, core.EventTaskPlanStateChanged, "", `{"from":"completed","to":"needed","cause":"plan_revision_approved"}`)
		// An agent draft recorded under the operator's account stays an agent
		// draft: the kind follows the origin, never the actor.
		l.event(30, "spec.version_created", planOperator, `{"version":2,"acceptance_count":0,"origin":"plan_order"}`)
		l.event(30, core.EventTaskPlanStateChanged, "", `{"from":"needed","to":"awaiting_approval","cause":"plan_submitted"}`)
		l.decide(40, core.InterventionApprove, "", "Looks right.", planOperator)
		l.event(41, "spec.version_approved", "", `{"version":2}`)
		l.event(41, core.EventTaskPlanStateChanged, "", `{"from":"awaiting_approval","to":"completed","cause":"plan_approved"}`)
		requireCycles(t, seen, l.cycles(),
			"plan contested=- versions=1..1 outcome=approved",
			"  plan_filed v1 e2",
			"  plan_approved v1 by usr_meron i1 e3",
			"revision_request contested=1 versions=2..2 outcome=approved",
			`  revision_requested v1 e5 "The plan names a missing file."`,
			`  revision_approved v1 by usr_meron i2 e6 "Go ahead."`,
			"  plan_drafted v2 e8",
			`  plan_approved v2 by usr_meron i3 e10 "Looks right."`,
		)
	})

	t.Run("changes requested keeps the cycle and a legacy version without origin is a draft", func(t *testing.T) {
		var l planLedger
		l.event(0, "spec.version_created", "", `{"version":1,"acceptance_count":0}`)
		l.decide(10, core.InterventionRedirect, "changes-requested", "Split step 2.", planOperator)
		l.event(10, core.EventTaskPlanStateChanged, "", `{"from":"awaiting_approval","to":"needed","cause":"plan_redirected"}`)
		l.event(20, "spec.version_created", "", `{"version":2,"acceptance_count":0}`)
		requireCycles(t, seen, l.cycles(),
			"plan contested=- versions=1..2 outcome=awaiting_decision",
			"  plan_drafted v1 e1",
			`  changes_requested v1 by usr_meron i1 e2 "Split step 2."`,
			"  plan_drafted v2 e4",
		)
	})

	t.Run("a planning request, an attached plan, and an approval without a decision", func(t *testing.T) {
		var l planLedger
		l.event(0, "work_order.plan_revision_requested", "", `{"rationale":"This needs a plan."}`)
		l.decide(5, core.InterventionRedirect, "plan-revision-approved", "", planOperator)
		l.event(5, core.EventTaskPlanStateChanged, "", `{"from":"not_required","to":"needed","cause":"planning_requested_approved"}`)
		l.event(9, core.EventTaskPlanStateChanged, "", `{"from":"needed","to":"awaiting_approval","cause":"plan_attached"}`)
		l.event(9, "spec.version_created", planOwner, `{"version":1,"acceptance_count":0,"origin":"operator","reason":"Use my plan.","request_id":"r1"}`)
		l.event(12, "spec.version_approved", "", `{"version":1}`)
		requireCycles(t, seen, l.cycles(),
			"planning_request contested=- versions=1..1 outcome=approved",
			`  planning_requested e1 "This needs a plan."`,
			"  revision_approved by usr_meron i1 e2",
			`  plan_attached v1 by usr_abel e5 "Use my plan."`,
			"  plan_approved v1 e6",
		)
	})

	t.Run("a declined revision and a rejected revision", func(t *testing.T) {
		var l planLedger
		l.event(0, "spec.version_created", "", `{"version":1,"origin":"plan_order"}`)
		l.event(1, core.EventTaskPlanStateChanged, "", `{"from":"awaiting_approval","to":"completed","cause":"plan_approved"}`)
		l.event(10, "work_order.plan_revision_requested", "", `{"rationale":"r1","plan_version":1}`)
		l.decide(11, core.InterventionRedirect, "plan-revision-declined", "Keep v1.", planOperator)
		l.event(20, "work_order.plan_revision_requested", "", `{"rationale":"r2","plan_version":1}`)
		l.decide(21, core.InterventionReject, "plan-revision-rejected", "Stop here.", planOperator)
		l.event(21, "task.state_changed", "", `{"from":"awaiting","to":"closed","command":"intervention.reject"}`)
		requireCycles(t, seen, l.cycles(),
			"plan contested=- versions=1..1 outcome=approved",
			"  plan_drafted v1 e1",
			"  plan_approved v1 e2",
			"revision_request contested=1 versions=-..- outcome=declined",
			`  revision_requested v1 e3 "r1"`,
			`  revision_declined v1 by usr_meron i1 e4 "Keep v1."`,
			"revision_request contested=1 versions=-..- outcome=closed",
			`  revision_requested v1 e5 "r2"`,
			`  revision_rejected v1 by usr_meron i2 e6 "Stop here."`,
		)
	})

	t.Run("an operator sends the plan back, then sets no plan needed", func(t *testing.T) {
		var l planLedger
		l.event(0, "spec.version_created", "", `{"version":1,"origin":"plan_order"}`)
		l.event(0, core.EventTaskPlanStateChanged, "", `{"from":"needed","to":"awaiting_approval","cause":"plan_submitted"}`)
		// The move is recorded first and the act's task.setup.changed after
		// it, in the same transaction.
		l.event(10, core.EventTaskPlanStateChanged, "", `{"from":"awaiting_approval","to":"needed","cause":"operator"}`)
		l.event(10, "task.setup.changed", planOwner, `{"actor":"user:usr_abel","reason":"Plan again.","plan_state":{"from":"awaiting_approval","to":"needed"}}`)
		l.event(20, "spec.version_created", "", `{"version":2,"origin":"plan_order"}`)
		l.event(30, core.EventTaskPlanStateChanged, "", `{"from":"awaiting_approval","to":"not_required","cause":"operator"}`)
		l.event(30, "task.setup.changed", planOperator, `{"actor":"user:usr_meron","reason":"","plan_state":{"from":"awaiting_approval","to":"not_required"}}`)
		requireCycles(t, seen, l.cycles(),
			"plan contested=- versions=1..2 outcome=not_needed",
			"  plan_drafted v1 e1",
			`  plan_sent_back v1 by usr_abel e3 "Plan again."`,
			"  plan_drafted v2 e5",
			"  plan_not_needed v2 by usr_meron e6",
		)
	})

	t.Run("a cycle planning after a send-back stays planning", func(t *testing.T) {
		var l planLedger
		l.event(0, "spec.version_created", "", `{"version":1,"origin":"plan_order"}`)
		l.event(10, core.EventTaskPlanStateChanged, "", `{"from":"awaiting_approval","to":"needed","cause":"operator"}`)
		requireCycles(t, seen, l.cycles(),
			"plan contested=- versions=1..1 outcome=planning",
			"  plan_drafted v1 e1",
			"  plan_sent_back v1 e2",
		)
	})

	t.Run("a revision request waits for the operator", func(t *testing.T) {
		var l planLedger
		l.event(0, "spec.version_created", "", `{"version":3,"origin":"plan_order"}`)
		l.decide(1, core.InterventionApprove, "", "", planOperator)
		l.event(5, "work_order.plan_revision_requested", "", `{"rationale":"r","plan_version":3}`)
		// A request without a rationale, or with an invalid version, is not
		// decidable and opens nothing.
		l.event(6, "work_order.plan_revision_requested", "", `{"rationale":"  ","plan_version":3}`)
		l.event(7, "work_order.plan_revision_requested", "", `{"rationale":"x","plan_version":0}`)
		requireCycles(t, seen, l.cycles(),
			"plan contested=- versions=3..3 outcome=approved",
			"  plan_drafted v3 e1",
			"  plan_approved v3 by usr_meron i1 e2",
			"revision_request contested=3 versions=-..- outcome=awaiting_decision",
			`  revision_requested v3 e3 "r"`,
		)
	})

	t.Run("open cycles close when the task merges, closes, or starts over", func(t *testing.T) {
		for _, tc := range []struct {
			payload, step, outcome string
		}{
			{`{"from":"awaiting","to":"merged","command":"merge.recover"}`, "task_merged", "merged"},
			{`{"from":"queued","to":"closed","command":"blueprint.close"}`, "task_closed", "closed"},
			{`{"from":"awaiting","to":"closed","command":"task.start_over"}`, "task_started_over", "closed"},
		} {
			var l planLedger
			l.event(0, "spec.version_created", "", `{"version":1,"origin":"plan_order"}`)
			l.event(5, "task.state_changed", "", tc.payload)
			l.event(9, "task.state_changed", "", `{"from":"closed","to":"merged","command":"merge.recover"}`)
			requireCycles(t, seen, l.cycles(),
				"plan contested=- versions=1..1 outcome="+tc.outcome,
				"  plan_drafted v1 e1",
				"  "+tc.step+" e2",
			)
		}
	})

	t.Run("a reject at the plan gate and a cancel close the cycle with the decision", func(t *testing.T) {
		var l planLedger
		l.event(0, "spec.version_created", "", `{"version":1,"origin":"plan_order"}`)
		l.decide(5, core.InterventionReject, "wrong-approach", "No.", planOperator)
		l.event(5, "task.state_changed", "", `{"from":"awaiting","to":"closed","command":"intervention.reject"}`)
		requireCycles(t, seen, l.cycles(),
			"plan contested=- versions=1..1 outcome=closed",
			"  plan_drafted v1 e1",
			`  task_closed by usr_meron i1 e2 "No."`,
		)
		var c planLedger
		c.event(0, "work_order.plan_revision_requested", "", `{"rationale":"r","plan_version":2}`)
		c.decide(5, core.InterventionRedirect, "plan-revision-approved", "", planOperator)
		c.decide(6, core.InterventionCancel, "", "Not needed now.", planOperator)
		c.event(6, "task.state_changed", "", `{"from":"queued","to":"closed","command":"task.cancel"}`)
		requireCycles(t, seen, c.cycles(),
			"revision_request contested=2 versions=-..- outcome=closed",
			`  revision_requested v2 e1 "r"`,
			"  revision_approved v2 by usr_meron i1 e2",
			`  task_closed by usr_meron i2 e3 "Not needed now."`,
		)
	})

	t.Run("decisions outside the plan gates stay out of every cycle", func(t *testing.T) {
		var l planLedger
		l.event(0, "spec.version_created", "", `{"version":1,"origin":"plan_order"}`)
		l.decide(1, core.InterventionApprove, "", "", planOperator)
		l.decide(50, core.InterventionRedirect, "", "Fix the test.", "review:round:1")
		l.decide(60, core.InterventionApprove, "", "Ship it.", planOperator)
		cycles := l.cycles()
		requireCycles(t, seen, cycles,
			"plan contested=- versions=1..1 outcome=approved",
			"  plan_drafted v1 e1",
			"  plan_approved v1 by usr_meron i1 e2",
		)
	})

	t.Run("equal times fold by event ID and the intervention joins by its recorded fields", func(t *testing.T) {
		var l planLedger
		l.event(0, "spec.version_created", "", `{"version":1,"origin":"plan_order"}`)
		// Two approvals at the same instant by different people: each step
		// names the intervention whose actor the event recorded.
		l.decide(1, core.InterventionApprove, "", "first", planOwner)
		l.event(1, "spec.version_created", "", `{"version":2,"origin":"plan_order"}`)
		l.decide(1, core.InterventionApprove, "", "second", planOperator)
		requireCycles(t, seen, l.cycles(),
			"plan contested=- versions=1..1 outcome=approved",
			"  plan_drafted v1 e1",
			`  plan_approved v1 by usr_abel i1 e2 "first"`,
			"plan contested=- versions=2..2 outcome=approved",
			"  plan_drafted v2 e3",
			`  plan_approved v2 by usr_meron i2 e4 "second"`,
		)
	})

	t.Run("long text is cut at a rune boundary and marked", func(t *testing.T) {
		var l planLedger
		rationale := strings.Repeat("é", taskSummaryTextMaxBytes)
		payload, _ := json.Marshal(map[string]any{"rationale": rationale, "plan_version": 1})
		l.event(0, "work_order.plan_revision_requested", "", string(payload))
		step := l.cycles()[0].Steps[0]
		if !step.Truncated || len(step.Text) > taskSummaryTextMaxBytes || !strings.HasPrefix(rationale, step.Text) || step.EventID != "1" {
			t.Fatalf("cut step: truncated=%v bytes=%d event=%s", step.Truncated, len(step.Text), step.EventID)
		}
	})

	t.Run("a task with no plan history has no cycles", func(t *testing.T) {
		var l planLedger
		l.event(0, core.EventTaskPlanStateChanged, "", `{"from":null,"to":"not_required","cause":"intake"}`)
		if got := l.cycles(); got == nil || len(got) != 0 {
			t.Fatalf("cycles = %#v, want empty", got)
		}
	})

	// The fixtures above reach the whole vocabulary of invariant 67.
	t.Run("every step kind and outcome is covered", func(t *testing.T) {
		for _, kind := range []string{
			planCycleStepRevisionRequested, planCycleStepPlanningRequested, planCycleStepRevisionApproved,
			planCycleStepRevisionDeclined, planCycleStepRevisionRejected, planCycleStepPlanDrafted, planCycleStepPlanFiled,
			planCycleStepPlanAttached, planCycleStepChangesRequested, planCycleStepPlanApproved, planCycleStepPlanSentBack,
			planCycleStepPlanNotNeeded, planCycleStepTaskStartedOver, planCycleStepTaskClosed, planCycleStepTaskMerged,
		} {
			if !seen["kind "+kind] {
				t.Errorf("no fixture folds step kind %s", kind)
			}
		}
		for _, outcome := range []string{
			planCycleOutcomeApproved, planCycleOutcomeAwaitingDecision, planCycleOutcomePlanning, planCycleOutcomeDeclined,
			planCycleOutcomeNotNeeded, planCycleOutcomeClosed, planCycleOutcomeMerged,
		} {
			if !seen["outcome "+outcome] {
				t.Errorf("no fixture folds outcome %s", outcome)
			}
		}
	})
}

// TestBoundedDetailPlanCyclesOutsideEventPage folds plan cycles from the
// complete ledger when more than 100 human events push the plan history off
// the bounded page, and resolves each person from the intervention record
// (component-web-task-surfaces invariants 23, 25, and 67).
func TestBoundedDetailPlanCyclesOutsideEventPage(t *testing.T) {
	st := store.NewMemory()
	h := newHistoryHarness(t, st)
	ctx := store.WithWorkspace(store.WithActor(context.Background(), store.SystemActor()), historyWorkspace)
	if err := st.CreateTask(ctx, core.Task{ID: "cycle-task", Workspace: historyWorkspace, State: core.TaskParked, Title: "Cycles", CreatedAt: historyEpoch}); err != nil {
		t.Fatal(err)
	}
	appendAt := func(ms int, kind, payload string) {
		t.Helper()
		if err := st.AppendEvent(ctx, core.Event{TaskID: "cycle-task", Kind: kind, ActorID: "user:usr_a", ActorRole: core.ActorHuman,
			At: historyEpoch.Add(time.Duration(ms) * time.Millisecond), Payload: json.RawMessage(payload)}); err != nil {
			t.Fatal(err)
		}
	}
	operator := store.WithActor(ctx, store.Actor{ID: "user:usr_a", Role: core.ActorHuman})
	decide := func(ms int, action core.InterventionAction, reason, comment string) {
		t.Helper()
		if err := st.CreateIntervention(operator, core.Intervention{TaskID: "cycle-task", Action: action, ReasonCode: reason, Comment: comment,
			At: historyEpoch.Add(time.Duration(ms) * time.Millisecond)}); err != nil {
			t.Fatal(err)
		}
	}
	appendAt(1, "spec.version_created", `{"version":1,"acceptance_count":0,"origin":"intake"}`)
	decide(2, core.InterventionApprove, core.IntakePlanApprovalReasonCode, "")
	appendAt(3, "work_order.plan_revision_requested", `{"rationale":"Needs a second step.","plan_version":1}`)
	decide(4, core.InterventionRedirect, "plan-revision-approved", "Agreed.")
	appendAt(5, "spec.version_created", `{"version":2,"acceptance_count":0,"origin":"plan_order"}`)
	decide(6, core.InterventionApprove, "", "")
	for i := range 130 {
		appendAt(10+i, "job.timeout", `{"stage":"implement"}`)
	}
	detail, response := h.bounded("cycle-task")
	for _, event := range detail.Events {
		if event.Kind == "work_order.plan_revision_requested" {
			t.Fatal("the request is on the presentation page; the fixture must place it outside")
		}
	}
	var body struct {
		Summaries struct {
			PlanCycles []struct {
				OpenedBy         string `json:"opened_by"`
				ContestedVersion *int   `json:"contested_version"`
				FirstVersion     *int   `json:"first_version"`
				FinalVersion     *int   `json:"final_version"`
				Outcome          string `json:"outcome"`
				Steps            []struct {
					Kind           string `json:"kind"`
					EventID        string `json:"event_id"`
					PlanVersion    *int   `json:"plan_version"`
					UserID         string `json:"user_id"`
					InterventionID *int64 `json:"intervention_id"`
					Text           string `json:"text"`
				} `json:"steps"`
			} `json:"plan_cycles"`
		} `json:"summaries"`
		Interventions []core.Intervention `json:"interventions"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	cycles := body.Summaries.PlanCycles
	if len(cycles) != 2 || cycles[0].Outcome != "approved" || cycles[1].Outcome != "approved" {
		t.Fatalf("plan cycles = %+v", cycles)
	}
	revision := cycles[1]
	if revision.OpenedBy != "revision_request" || revision.ContestedVersion == nil || *revision.ContestedVersion != 1 ||
		revision.FirstVersion == nil || *revision.FirstVersion != 2 || revision.FinalVersion == nil || *revision.FinalVersion != 2 {
		t.Fatalf("revision cycle = %+v", revision)
	}
	kinds := []string{}
	for _, step := range revision.Steps {
		kinds = append(kinds, step.Kind)
	}
	if strings.Join(kinds, ",") != "revision_requested,revision_approved,plan_drafted,plan_approved" {
		t.Fatalf("revision steps = %v", kinds)
	}
	approval := revision.Steps[3]
	if approval.UserID != "usr_a" || approval.InterventionID == nil || approval.PlanVersion == nil || *approval.PlanVersion != 2 {
		t.Fatalf("approval step = %+v", approval)
	}
	if len(body.Interventions) != 3 || *approval.InterventionID != body.Interventions[2].ID {
		t.Fatalf("approval names intervention %d of %+v", *approval.InterventionID, body.Interventions)
	}
	if revision.Steps[1].Text != "Agreed." || revision.Steps[0].Text != "Needs a second step." {
		t.Fatalf("texts = %q, %q", revision.Steps[0].Text, revision.Steps[1].Text)
	}
}

// TestPlanCyclesDecisionAfterItsStep folds the ledger order every backend
// writes for a plan-gate decision: the dispatcher records the version
// approval, the plan-state move, and the task state change before the API
// records the intervention. The intervention event names itself on the step
// those events recorded, so no decision renders as its own entry
// (component-web-task-surfaces invariants 67 to 69; req-260809-a40c75 AC-4.1,
// AC-4.2; DEC-130(1)).
func TestPlanCyclesDecisionAfterItsStep(t *testing.T) {
	t.Run("the reported approval names intervention 2024 and its person", func(t *testing.T) {
		// Task 261112-b903b7 as PostgreSQL recorded it: the version approval
		// and the plan-state move are 27 ms before the intervention event.
		at := func(value string) time.Time {
			parsed, err := time.Parse(time.RFC3339Nano, value)
			if err != nil {
				t.Fatal(err)
			}
			return parsed
		}
		const actor = "user:usr_L-rsl-N4aIe3UaaF"
		event := func(id int64, when, kind, jobID, payload string) core.Event {
			return core.Event{ID: id, TaskID: "261112-b903b7", JobID: jobID, Kind: kind, ActorID: actor, ActorRole: core.ActorHuman,
				At: at(when), Payload: json.RawMessage(payload)}
		}
		decidedAt := "2026-11-12T08:41:47.204596Z"
		events := []core.Event{
			event(842588, "2026-11-12T08:25:52.263653Z", core.EventTaskPlanStateChanged, "", `{"cause":"intake","from":null,"to":"needed"}`),
			event(842623, "2026-11-12T08:35:49.894659Z", "spec.version_created", "", `{"version":1,"acceptance_count":0,"origin":"plan_order"}`),
			event(842624, "2026-11-12T08:35:49.93348Z", "task.state_changed", "", `{"from":"running","to":"awaiting_human","command":"gate.spec"}`),
			event(842625, "2026-11-12T08:35:49.935037Z", core.EventTaskPlanStateChanged, "", `{"cause":"plan_submitted","from":"needed","to":"awaiting_approval"}`),
			event(842629, "2026-11-12T08:41:47.177094Z", "spec.version_approved", "", `{"version":1}`),
			event(842630, "2026-11-12T08:41:47.178625Z", core.EventTaskPlanStateChanged, "", `{"cause":"plan_approved","from":"awaiting_approval","to":"completed"}`),
			event(842631, "2026-11-12T08:41:47.18749Z", "risk.rated", "", `{"source":"plan"}`),
			event(842632, "2026-11-12T08:41:47.194943Z", "task.state_changed", "", `{"from":"awaiting_human","to":"queued","command":"intervention.approve_spec"}`),
			event(842633, "2026-11-12T08:41:47.195603Z", "pipeline.transition_decided", "", `{"state":"queued"}`),
			event(842634, decidedAt, "intervention.approve", "261112-b903b7-spec-1", `{"reason_code":"approved","comment":""}`),
			event(842635, "2026-11-12T08:41:47.23584Z", "job.created", "261112-b903b7-implement-1", `{"id":"261112-b903b7-implement-1","state":"pending"}`),
			event(842687, "2026-11-12T08:56:16.476167Z", "task.state_changed", "261112-b903b7-implement-1", `{"from":"queued","to":"running","command":"order.claim"}`),
		}
		interventions := []core.Intervention{{ID: 2024, TaskID: "261112-b903b7", JobID: "261112-b903b7-spec-1", ActorID: actor, ActorRole: core.ActorHuman,
			Action: core.InterventionApprove, ReasonCode: "approved", At: at(decidedAt)}}
		cycles := derivePlanCycles(events, interventions)
		resolvePlanCyclePeople(cycles, nil)
		requireCycles(t, map[string]bool{}, cycles,
			"plan contested=- versions=1..1 outcome=approved",
			"  plan_drafted v1 e842623",
			"  plan_approved v1 by usr_L-rsl-N4aIe3UaaF i2024 e842629",
		)
	})

	t.Run("an approval recorded only by the plan-state move", func(t *testing.T) {
		var l planLedger
		l.event(0, "spec.version_created", "", `{"version":1,"origin":"plan_order"}`)
		l.event(10, core.EventTaskPlanStateChanged, "", `{"from":"awaiting_approval","to":"completed","cause":"plan_approved"}`)
		l.event(10, "task.state_changed", "", `{"from":"awaiting_human","to":"queued","command":"intervention.approve_spec"}`)
		l.decide(10, core.InterventionApprove, "approved", "Good plan.", planOperator)
		requireCycles(t, map[string]bool{}, l.cycles(),
			"plan contested=- versions=1..1 outcome=approved",
			"  plan_drafted v1 e1",
			`  plan_approved v1 by usr_meron i1 e2 "Good plan."`,
		)
	})

	t.Run("changes requested, then the next version is approved, with identical comments", func(t *testing.T) {
		var l planLedger
		l.event(0, "spec.version_created", "", `{"version":1,"origin":"plan_order"}`)
		l.event(10, core.EventTaskPlanStateChanged, "", `{"from":"awaiting_approval","to":"needed","cause":"plan_redirected"}`)
		l.event(10, "task.state_changed", "", `{"from":"awaiting_human","to":"queued","command":"intervention.redirect"}`)
		l.decide(10, core.InterventionRedirect, "changes-requested", "Same words.", planOperator)
		l.event(15, "task.state_changed", "", `{"from":"queued","to":"running","command":"order.claim"}`)
		l.event(20, "spec.version_created", "", `{"version":2,"origin":"plan_order"}`)
		l.event(30, "spec.version_approved", "", `{"version":2}`)
		l.event(30, core.EventTaskPlanStateChanged, "", `{"from":"awaiting_approval","to":"completed","cause":"plan_approved"}`)
		l.decide(30, core.InterventionApprove, "approved", "Same words.", planOwner)
		requireCycles(t, map[string]bool{}, l.cycles(),
			"plan contested=- versions=1..2 outcome=approved",
			"  plan_drafted v1 e1",
			`  changes_requested v1 by usr_meron i1 e2 "Same words."`,
			"  plan_drafted v2 e6",
			`  plan_approved v2 by usr_abel i2 e7 "Same words."`,
		)
	})

	t.Run("revision and planning approvals recorded after their moves", func(t *testing.T) {
		var l planLedger
		l.event(0, "spec.version_created", "", `{"version":1,"origin":"intake"}`)
		l.event(0, "spec.version_approved", "", `{"version":1}`)
		l.event(10, "work_order.plan_revision_requested", "", `{"rationale":"r1","plan_version":1}`)
		l.event(20, core.EventTaskPlanStateChanged, "", `{"from":"completed","to":"needed","cause":"plan_revision_approved"}`)
		l.decide(20, core.InterventionRedirect, "plan-revision-approved", "Go ahead.", planOperator)
		l.event(30, "spec.version_created", "", `{"version":2,"origin":"plan_order"}`)
		var p planLedger
		p.event(0, "work_order.plan_revision_requested", "", `{"rationale":"This needs a plan."}`)
		p.event(5, core.EventTaskPlanStateChanged, "", `{"from":"not_required","to":"needed","cause":"planning_requested_approved"}`)
		p.decide(5, core.InterventionRedirect, "plan-revision-approved", "", planOperator)
		requireCycles(t, map[string]bool{}, append(l.cycles(), p.cycles()...),
			"plan contested=- versions=1..1 outcome=approved",
			"  plan_filed v1 e1",
			"  plan_approved v1 e2",
			"revision_request contested=1 versions=2..2 outcome=awaiting_decision",
			`  revision_requested v1 e3 "r1"`,
			`  revision_approved v1 by usr_meron i1 e4 "Go ahead."`,
			"  plan_drafted v2 e6",
			"planning_request contested=- versions=-..- outcome=planning",
			`  planning_requested e1 "This needs a plan."`,
			"  revision_approved by usr_meron i1 e2",
		)
	})

	t.Run("a reject at the plan gate recorded after the task closed", func(t *testing.T) {
		var l planLedger
		l.event(0, "spec.version_created", "", `{"version":1,"origin":"plan_order"}`)
		l.event(5, "task.state_changed", "", `{"from":"awaiting_human","to":"closed","command":"intervention.reject"}`)
		l.decide(5, core.InterventionReject, "wrong-approach", "No.", planOperator)
		requireCycles(t, map[string]bool{}, l.cycles(),
			"plan contested=- versions=1..1 outcome=closed",
			"  plan_drafted v1 e1",
			`  task_closed by usr_meron i1 e2 "No."`,
		)
	})

	t.Run("an operator send-back keeps its actor and comment in either order", func(t *testing.T) {
		var l planLedger
		l.event(0, "spec.version_created", "", `{"version":1,"origin":"plan_order"}`)
		l.event(10, "task.setup.changed", planOwner, `{"actor":"user:usr_abel","reason":"Plan again.","plan_state":{"from":"awaiting_approval","to":"needed"}}`)
		l.event(10, core.EventTaskPlanStateChanged, "", `{"from":"awaiting_approval","to":"needed","cause":"operator"}`)
		l.event(20, "spec.version_created", "", `{"version":2,"origin":"plan_order"}`)
		l.event(30, core.EventTaskPlanStateChanged, "", `{"from":"awaiting_approval","to":"needed","cause":"operator"}`)
		l.event(30, "task.setup.changed", planOperator, `{"actor":"user:usr_meron","reason":"Once more.","plan_state":{"from":"awaiting_approval","to":"needed"}}`)
		requireCycles(t, map[string]bool{}, l.cycles(),
			"plan contested=- versions=1..2 outcome=planning",
			"  plan_drafted v1 e1",
			`  plan_sent_back v1 by usr_abel e3 "Plan again."`,
			"  plan_drafted v2 e4",
			`  plan_sent_back v2 by usr_meron e5 "Once more."`,
		)
	})

	t.Run("a later merge approval or review redirect never joins an automatic approval", func(t *testing.T) {
		var l planLedger
		l.event(0, "spec.version_created", "", `{"version":1,"origin":"plan_order"}`)
		l.event(0, "spec.version_approved", "", `{"version":1}`)
		l.event(0, core.EventTaskPlanStateChanged, "", `{"from":"awaiting_approval","to":"completed","cause":"plan_approved"}`)
		l.event(0, "task.state_changed", "", `{"from":"running","to":"queued","command":"stage.advance"}`)
		l.event(10, "task.state_changed", "", `{"from":"queued","to":"running","command":"order.claim"}`)
		l.event(20, "task.state_changed", "", `{"from":"running","to":"awaiting_human","command":"gate.merge"}`)
		l.decide(30, core.InterventionRedirect, "", "Fix the test.", "review:round:1")
		l.event(30, "task.state_changed", "", `{"from":"awaiting_human","to":"queued","command":"intervention.redirect"}`)
		l.decide(40, core.InterventionApprove, "", "Ship it.", planOperator)
		requireCycles(t, map[string]bool{}, l.cycles(),
			"plan contested=- versions=1..1 outcome=approved",
			"  plan_drafted v1 e1",
			"  plan_approved v1 e2",
		)
	})

	t.Run("a changes-requested step without its decision keeps a later review redirect out", func(t *testing.T) {
		var l planLedger
		l.event(0, "spec.version_created", "", `{"version":1,"origin":"plan_order"}`)
		l.event(10, core.EventTaskPlanStateChanged, "", `{"from":"awaiting_approval","to":"needed","cause":"plan_redirected"}`)
		l.event(10, "task.state_changed", "", `{"from":"awaiting_human","to":"queued","command":"intervention.redirect"}`)
		l.event(11, "task.state_changed", "", `{"from":"queued","to":"running","command":"order.claim"}`)
		l.decide(50, core.InterventionRedirect, "", "Fix the test.", "review:round:1")
		requireCycles(t, map[string]bool{}, l.cycles(),
			"plan contested=- versions=1..1 outcome=planning",
			"  plan_drafted v1 e1",
			"  changes_requested v1 e2",
		)
	})

	t.Run("a decision of another action ends the wait", func(t *testing.T) {
		var l planLedger
		l.event(0, "spec.version_created", "", `{"version":1,"origin":"plan_order"}`)
		l.event(10, core.EventTaskPlanStateChanged, "", `{"from":"awaiting_approval","to":"completed","cause":"plan_approved"}`)
		l.decide(10, core.InterventionPull, "", "", planOperator)
		l.decide(10, core.InterventionApprove, "approved", "", planOperator)
		requireCycles(t, map[string]bool{}, l.cycles(),
			"plan contested=- versions=1..1 outcome=approved",
			"  plan_drafted v1 e1",
			"  plan_approved v1 e2",
		)
	})

	t.Run("a missing or mismatched intervention record adds no person and no step", func(t *testing.T) {
		var l planLedger
		l.event(0, "spec.version_created", "", `{"version":1,"origin":"plan_order"}`)
		l.event(10, "spec.version_approved", "", `{"version":1}`)
		l.decide(10, core.InterventionApprove, "approved", "Approved without a record.", planOperator)
		// The record is lost: only the intervention event stays.
		l.interventions = nil
		l.event(20, "spec.version_created", "", `{"version":2,"origin":"plan_order"}`)
		l.event(30, "spec.version_approved", "", `{"version":2}`)
		l.decide(30, core.InterventionApprove, "approved", "", planOperator)
		// The record names another actor than the event.
		l.interventions[0].ActorID = planOwner
		requireCycles(t, map[string]bool{}, l.cycles(),
			"plan contested=- versions=1..1 outcome=approved",
			"  plan_drafted v1 e1",
			`  plan_approved v1 e2 "Approved without a record."`,
			"plan contested=- versions=2..2 outcome=approved",
			"  plan_drafted v2 e4",
			"  plan_approved v2 e5",
		)
	})

	t.Run("a decision recorded first still suppresses the later fallback", func(t *testing.T) {
		var l planLedger
		l.event(0, "spec.version_created", "", `{"version":1,"origin":"plan_order"}`)
		l.decide(10, core.InterventionApprove, "approved", "", planOperator)
		l.event(10, "spec.version_approved", "", `{"version":1}`)
		l.event(10, core.EventTaskPlanStateChanged, "", `{"from":"awaiting_approval","to":"completed","cause":"plan_approved"}`)
		l.event(20, "spec.version_created", "", `{"version":2,"origin":"plan_order"}`)
		l.decide(30, core.InterventionRedirect, "changes-requested", "Again.", planOperator)
		l.event(30, core.EventTaskPlanStateChanged, "", `{"from":"awaiting_approval","to":"needed","cause":"plan_redirected"}`)
		requireCycles(t, map[string]bool{}, l.cycles(),
			"plan contested=- versions=1..1 outcome=approved",
			"  plan_drafted v1 e1",
			"  plan_approved v1 by usr_meron i1 e2",
			"plan contested=- versions=2..2 outcome=planning",
			"  plan_drafted v2 e5",
			`  changes_requested v2 by usr_meron i2 e6 "Again."`,
		)
	})
}

// TestBoundedDetailPlanApprovalAfterItsStep reads the bounded detail when the
// plan approval's intervention follows the version approval and the
// plan-state move, as the dispatcher and the API write them, and more than
// 100 human events push the approval off the bounded page. The approval step
// names the intervention and its person, so the dashboard absorbs it from the
// summary alone (component-web-task-surfaces invariants 25, 67, 68, and 71).
func TestBoundedDetailPlanApprovalAfterItsStep(t *testing.T) {
	st := store.NewMemory()
	h := newHistoryHarness(t, st)
	ctx := store.WithWorkspace(store.WithActor(context.Background(), store.SystemActor()), historyWorkspace)
	if err := st.CreateTask(ctx, core.Task{ID: "approval-task", Workspace: historyWorkspace, State: core.TaskParked, Title: "Approval", CreatedAt: historyEpoch}); err != nil {
		t.Fatal(err)
	}
	appendAt := func(ms int, kind, payload string) {
		t.Helper()
		if err := st.AppendEvent(ctx, core.Event{TaskID: "approval-task", Kind: kind, ActorID: "user:usr_a", ActorRole: core.ActorHuman,
			At: historyEpoch.Add(time.Duration(ms) * time.Millisecond), Payload: json.RawMessage(payload)}); err != nil {
			t.Fatal(err)
		}
	}
	appendAt(1, "spec.version_created", `{"version":1,"acceptance_count":0,"origin":"plan_order"}`)
	appendAt(2, core.EventTaskPlanStateChanged, `{"from":"needed","to":"awaiting_approval","cause":"plan_submitted"}`)
	appendAt(10, "spec.version_approved", `{"version":1}`)
	appendAt(11, core.EventTaskPlanStateChanged, `{"from":"awaiting_approval","to":"completed","cause":"plan_approved"}`)
	appendAt(12, "task.state_changed", `{"from":"awaiting_human","to":"queued","command":"intervention.approve_spec"}`)
	operator := store.WithActor(ctx, store.Actor{ID: "user:usr_a", Role: core.ActorHuman})
	if err := st.CreateIntervention(operator, core.Intervention{TaskID: "approval-task", Action: core.InterventionApprove, ReasonCode: "approved",
		Comment: "Looks right.", At: historyEpoch.Add(37 * time.Millisecond)}); err != nil {
		t.Fatal(err)
	}
	for i := range 130 {
		appendAt(100+i, "job.timeout", `{"stage":"implement"}`)
	}
	detail, response := h.bounded("approval-task")
	for _, event := range detail.Events {
		if event.Kind == "spec.version_approved" || event.Kind == "intervention.approve" {
			t.Fatalf("the approval is on the presentation page; the fixture must place it outside: %s", event.Kind)
		}
	}
	var body struct {
		Summaries struct {
			PlanCycles []struct {
				Outcome string `json:"outcome"`
				Steps   []struct {
					Kind           string `json:"kind"`
					PlanVersion    *int   `json:"plan_version"`
					UserID         string `json:"user_id"`
					InterventionID *int64 `json:"intervention_id"`
					Text           string `json:"text"`
				} `json:"steps"`
			} `json:"plan_cycles"`
		} `json:"summaries"`
		Interventions []core.Intervention `json:"interventions"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	cycles := body.Summaries.PlanCycles
	if len(cycles) != 1 || cycles[0].Outcome != "approved" || len(cycles[0].Steps) != 2 {
		t.Fatalf("plan cycles = %+v", cycles)
	}
	approval := cycles[0].Steps[1]
	if len(body.Interventions) != 1 {
		t.Fatalf("interventions = %+v", body.Interventions)
	}
	if approval.Kind != "plan_approved" || approval.PlanVersion == nil || *approval.PlanVersion != 1 || approval.UserID != "usr_a" ||
		approval.InterventionID == nil || *approval.InterventionID != body.Interventions[0].ID || approval.Text != "Looks right." {
		t.Fatalf("approval step = %+v", approval)
	}
}
