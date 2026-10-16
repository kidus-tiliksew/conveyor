package store

import (
	"encoding/json"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/nextaction"
)

var flowT0 = time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC)

type flowLedger struct{ events []core.Event }

func (l *flowLedger) add(kind string, role core.ActorRole, jobID string, payload any) *flowLedger {
	n := int64(len(l.events) + 1)
	l.events = append(l.events, core.Event{
		ID: n, TaskID: "task", JobID: jobID, Kind: kind, ActorID: string(role) + ":actor", ActorRole: role,
		Payload: core.JSONPayload(payload), At: flowT0.Add(time.Duration(n) * time.Minute),
	})
	return l
}

func (l *flowLedger) created(task core.Task) *flowLedger {
	task.ID = "task"
	if task.State == "" {
		task.State = core.TaskQueued
	}
	return l.add("task.created", core.ActorHuman, "", task)
}

func (l *flowLedger) state(to core.TaskState, command core.TaskCommand) *flowLedger {
	return l.add("task.state_changed", core.ActorSystem, "", map[string]any{"to": to, "command": command})
}

func (l *flowLedger) transition(from, next core.Stage) *flowLedger {
	return l.add("pipeline.transition_decided", core.ActorSystem, "", map[string]any{"from_stage": from, "next_stage": next})
}

func (l *flowLedger) order(id string, stage core.Stage, role core.ActorRole) *flowLedger {
	return l.add("work_order.created", role, "job-"+id, core.WorkOrder{ID: id, JobID: "job-" + id, Stage: stage, State: core.WorkOrderQueued})
}

func (l *flowLedger) claim(id string, stage core.Stage, attempt string) *flowLedger {
	at := flowT0.Add(time.Duration(len(l.events)+1) * time.Minute)
	return l.add("work_order.claimed", core.ActorAgent, "job-"+id, core.WorkOrder{
		ID: id, JobID: "job-" + id, Stage: stage, State: core.WorkOrderClaimed, AttemptID: attempt,
		ClaimantID: "worker-1", SessionID: "session-" + attempt, Agent: "omp", Model: "opus",
		ExecutionStartedAt: at, LastAgentActivityAt: at, LeaseExpiresAt: at.Add(time.Hour),
	})
}

// fold folds at flowT0, before any lease in these ledgers runs out.
func (l *flowLedger) fold(action nextaction.NextAction) TaskFlow {
	return l.foldAt(action, flowT0)
}

func (l *flowLedger) foldAt(action nextaction.NextAction, now time.Time) TaskFlow {
	return FoldTaskFlow(l.events, action, config.DefaultRecoveryPolicy(), now)
}

func flowStage(t *testing.T, flow TaskFlow, id core.Stage) FlowStage {
	t.Helper()
	for _, stage := range flow.Stages {
		if stage.ID == id {
			return stage
		}
	}
	t.Fatalf("stage %s missing from %+v", id, flow.Stages)
	return FlowStage{}
}

func flowStatuses(flow TaskFlow) map[core.Stage]string {
	out := map[core.Stage]string{}
	for _, stage := range flow.Stages {
		out[stage.ID] = stage.Status
	}
	return out
}

func approvalTask() core.Task {
	return core.Task{SpecApproval: true, MergeApproval: true, SetupContract: config.ExecutionSetup{VerifyStage: true}}
}

func TestFoldTaskFlowStages(t *testing.T) {
	flow := new(flowLedger).created(approvalTask()).fold(nextaction.NextAction{Actor: nextaction.ActorFactory})
	want := []core.Stage{core.StageTriage, core.StageSpec, core.StageImplement, core.StageVerify, core.StageReview, core.StageMerge}
	gates := map[core.Stage]string{core.StageSpec: "plan_approval", core.StageMerge: "merge_approval"}
	if len(flow.Stages) != len(want) {
		t.Fatalf("stages = %+v", flow.Stages)
	}
	for i, stage := range flow.Stages {
		if stage.ID != want[i] {
			t.Fatalf("stage %d = %s, want %s", i, stage.ID, want[i])
		}
		got := ""
		if stage.Gate != nil {
			got = *stage.Gate
		}
		if got != gates[stage.ID] {
			t.Fatalf("stage %s gate = %q, want %q", stage.ID, got, gates[stage.ID])
		}
		if stage.Attempts == nil {
			t.Fatalf("stage %s attempts must encode as []", stage.ID)
		}
	}
	body, err := json.Marshal(flow)
	if err != nil {
		t.Fatal(err)
	}
	var shape map[string]json.RawMessage
	if err := json.Unmarshal(body, &shape); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"stages", "loops", "side", "next_action", "raw"} {
		if _, ok := shape[key]; !ok {
			t.Fatalf("flow JSON lacks %q: %s", key, body)
		}
	}
	if string(shape["loops"]) != "[]" {
		t.Fatalf("loops = %s, want []", shape["loops"])
	}
}

func TestFoldTaskFlowFrozenPolicy(t *testing.T) {
	action := nextaction.NextAction{Actor: nextaction.ActorFactory}
	off := core.Task{SpecApproval: false, MergeApproval: false, SetupContract: config.ExecutionSetup{VerifyStage: false}}
	flow := new(flowLedger).created(off).fold(action)
	spec, verify, merge := flowStage(t, flow, core.StageSpec), flowStage(t, flow, core.StageVerify), flowStage(t, flow, core.StageMerge)
	if spec.Status != FlowOff || spec.Gate != nil {
		t.Fatalf("spec_approval=false: spec = %+v, want off with null gate", spec)
	}
	if verify.Status != FlowOff {
		t.Fatalf("verify_stage=false: verify = %s, want off", verify.Status)
	}
	if merge.Gate != nil || merge.Status == FlowOff {
		t.Fatalf("merge_approval=false: merge = %+v, want null gate and stage on", merge)
	}

	onlyVerifyOff := approvalTask()
	onlyVerifyOff.SetupContract.VerifyStage = false
	flow = new(flowLedger).created(onlyVerifyOff).fold(action)
	if got := flowStage(t, flow, core.StageSpec); got.Status == FlowOff || got.Gate == nil {
		t.Fatalf("spec approval on must keep spec on with its gate: %+v", got)
	}
	if got := flowStage(t, flow, core.StageVerify).Status; got != FlowOff {
		t.Fatalf("verify_stage=false beside spec_approval=true: verify = %s, want off", got)
	}
	ledger := new(flowLedger).created(onlyVerifyOff)
	ledger.add("task.setup.changed", core.ActorHuman, "", map[string]any{"new_setup": map[string]any{"verify_stage": true}})
	if got := flowStage(t, ledger.fold(action), core.StageVerify).Status; got == FlowOff {
		t.Fatalf("task.setup.changed verify_stage=true left verify off")
	}
}

func TestFoldTaskFlowStatusPrecedence(t *testing.T) {
	operator := nextaction.NextAction{Actor: nextaction.ActorOperator, Kind: nextaction.KindPlanApproval}
	t.Run("plan approval gate needs the operator", func(t *testing.T) {
		ledger := new(flowLedger).created(approvalTask()).transition(core.StageTriage, core.StageSpec).
			order("spec-1", core.StageSpec, core.ActorSystem).claim("spec-1", core.StageSpec, "a1").
			add("work_order.updated", core.ActorAgent, "job-spec-1", map[string]any{"work_order_id": "spec-1", "new_state": core.WorkOrderSubmitted}).
			state(core.TaskAwaiting, core.TaskGateSpec)
		flow := ledger.fold(operator)
		want := map[core.Stage]string{
			core.StageTriage: FlowDone, core.StageSpec: FlowNeedsOperator, core.StageImplement: FlowUpcoming,
			core.StageVerify: FlowUpcoming, core.StageReview: FlowUpcoming, core.StageMerge: FlowUpcoming,
		}
		if got := flowStatuses(flow); !reflect.DeepEqual(got, want) {
			t.Fatalf("statuses = %v, want %v", got, want)
		}
		// needs_operator comes from the next action's actor, not the task state.
		if got := flowStage(t, ledger.fold(nextaction.NextAction{Actor: nextaction.ActorAgent}), core.StageSpec).Status; got == FlowNeedsOperator {
			t.Fatalf("awaiting_human without an operator next action must not be needs_operator")
		}
	})
	t.Run("claimed is in progress and queued is waiting", func(t *testing.T) {
		ledger := new(flowLedger).created(approvalTask()).transition(core.StageTriage, core.StageImplement).
			order("impl-1", core.StageImplement, core.ActorSystem)
		if got := flowStage(t, ledger.fold(nextaction.NextAction{Actor: nextaction.ActorFactory}), core.StageImplement).Status; got != FlowWaiting {
			t.Fatalf("queued order: implement = %s, want waiting", got)
		}
		ledger.claim("impl-1", core.StageImplement, "a1")
		if got := flowStage(t, ledger.fold(nextaction.NextAction{Actor: nextaction.ActorAgent}), core.StageImplement).Status; got != FlowInProgress {
			t.Fatalf("claimed order: implement = %s, want in_progress", got)
		}
	})
	t.Run("approved with the merge gate off is in progress", func(t *testing.T) {
		// DEC-64(8) R2: new histories record one review.accept; historical ones
		// keep the gate.merge + intervention.approve_review pair. Both replay to
		// approved with no gate.
		histories := map[string]func(*flowLedger) *flowLedger{
			"review.accept": func(l *flowLedger) *flowLedger { return l.state(core.TaskApproved, core.TaskReviewAccept) },
			"historical pair": func(l *flowLedger) *flowLedger {
				return l.state(core.TaskAwaiting, core.TaskGateMerge).state(core.TaskApproved, core.TaskInterventionApproveReview)
			},
		}
		for name, accept := range histories {
			task := approvalTask()
			task.MergeApproval = false
			ledger := accept(new(flowLedger).created(task).transition(core.StageReview, core.StageMerge))
			got := flowStage(t, ledger.fold(nextaction.NextAction{Actor: nextaction.ActorFactory, Kind: nextaction.KindMerging}), core.StageMerge).Status
			if got != FlowInProgress {
				t.Fatalf("%s: merge = %s, want in_progress", name, got)
			}
		}
	})
	t.Run("merged makes visited stages done", func(t *testing.T) {
		ledger := new(flowLedger).created(approvalTask()).transition(core.StageTriage, core.StageSpec).
			transition(core.StageSpec, core.StageImplement).transition(core.StageImplement, core.StageReview).
			transition(core.StageReview, core.StageMerge).state(core.TaskMerged, core.TaskMergeConfirm)
		got := flowStatuses(ledger.fold(nextaction.NextAction{Actor: nextaction.ActorNone, Kind: nextaction.KindTerminal}))
		want := map[core.Stage]string{
			core.StageTriage: FlowDone, core.StageSpec: FlowDone, core.StageImplement: FlowDone,
			core.StageVerify: FlowUpcoming, core.StageReview: FlowDone, core.StageMerge: FlowDone,
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("statuses = %v, want %v", got, want)
		}
	})
	t.Run("operator gates name the stage they hold", func(t *testing.T) {
		failed := new(flowLedger).created(approvalTask()).transition(core.StageImplement, core.StageReview).
			order("review-1", core.StageReview, core.ActorSystem).claim("review-1", core.StageReview, "a1").
			add("work_order.child_failed", core.ActorSystem, "job-review-1", map[string]any{"work_order_id": "review-1"}).
			transition(core.StageReview, core.StageMerge).state(core.TaskAwaiting, core.TaskJobFail)
		revision := new(flowLedger).created(approvalTask()).transition(core.StageSpec, core.StageImplement).
			state(core.TaskAwaiting, core.TaskGatePlanRevision)
		for name, tc := range map[string]struct {
			ledger *flowLedger
			stage  core.Stage
		}{"job failure": {failed, core.StageReview}, "plan revision": {revision, core.StageSpec}} {
			got := flowStatuses(tc.ledger.fold(nextaction.NextAction{Actor: nextaction.ActorOperator}))
			for stage, status := range got {
				if (status == FlowNeedsOperator) != (stage == tc.stage) {
					t.Fatalf("%s: statuses = %v, want only %s needs_operator", name, got, tc.stage)
				}
			}
		}
	})
	t.Run("a running factory job outranks a queued order", func(t *testing.T) {
		ledger := new(flowLedger).created(approvalTask()).transition(core.StageImplement, core.StageVerify).
			order("verify-1", core.StageVerify, core.ActorSystem)
		ledger.add("job.created", core.ActorSystem, "job-check", core.Job{ID: "job-check", Stage: core.StageVerify, State: core.JobRunning})
		if got := flowStage(t, ledger.fold(nextaction.NextAction{Actor: nextaction.ActorFactory}), core.StageVerify).Status; got != FlowInProgress {
			t.Fatalf("verify = %s, want in_progress", got)
		}
	})
}

func TestFoldTaskFlowAttempts(t *testing.T) {
	ledger := new(flowLedger).created(approvalTask()).transition(core.StageTriage, core.StageSpec).
		order("spec-1", core.StageSpec, core.ActorSystem).claim("spec-1", core.StageSpec, "a1").
		add("work_order.stale", core.ActorSystem, "job-spec-1", map[string]any{"work_order_id": "spec-1"}).
		order("spec-2", core.StageSpec, core.ActorSystem).
		add("work_order.retired", core.ActorSystem, "job-spec-1", map[string]any{"work_order_id": "spec-1"}).
		claim("spec-2", core.StageSpec, "a2").
		add("work_order.released", core.ActorAgent, "job-spec-2", map[string]any{"attempt_id": "a2", "outcome": "released"}).
		add("work_order.recovered", core.ActorHuman, "job-spec-2", map[string]any{"work_order_id": "spec-2"}).
		add("work_order.expired", core.ActorSystem, "job-spec-2", map[string]any{"attempt_id": ""}).
		add("work_order.redispatched", core.ActorSystem, "job-spec-2", map[string]any{"work_order_id": "spec-2", "automatic": true, "recovery_number": 1})
	attempts := flowStage(t, ledger.fold(nextaction.NextAction{Actor: nextaction.ActorFactory}), core.StageSpec).Attempts
	type row struct {
		order, attempt, outcome string
		superseded, auto, ended bool
	}
	var got []row
	for _, attempt := range attempts {
		got = append(got, row{attempt.OrderID, attempt.AttemptID, attempt.Outcome, attempt.Superseded, attempt.Auto, attempt.EndedAt != nil})
		if attempt.AttemptID != "" && attempt.StartedAt == nil {
			t.Fatalf("claimed attempt %s has no started_at", attempt.AttemptID)
		}
	}
	want := []row{
		{"spec-1", "a1", "stale", true, false, true},
		{"spec-2", "a2", "released", false, true, true},
		{"spec-2", "", "expired", false, false, true},
		{"spec-2", "", "", false, true, false},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("attempts =\n%+v\nwant\n%+v", got, want)
	}
}

// A successor's creation and its sibling's retirement share a transaction;
// backends list them in either order, and both fold the successor as an
// automatic replacement, also when the sibling's episode ended in an earlier
// pass. A human-created successor is not automatic.
func TestFoldTaskFlowReplacementOrder(t *testing.T) {
	retire := func(l *flowLedger) *flowLedger {
		return l.add("work_order.retired", core.ActorSystem, "job-spec-1", map[string]any{"work_order_id": "spec-1", "authoritative_work_order_id": "spec-2"})
	}
	for _, tc := range []struct {
		name               string
		role               core.ActorRole
		first, ended, auto bool
	}{
		{"retired then created", core.ActorSystem, true, false, true},
		{"created then retired", core.ActorSystem, false, false, true},
		{"retired then created in a later pass", core.ActorSystem, true, true, true},
		{"created then retired in a later pass", core.ActorSystem, false, true, true},
		{"human successor retired first", core.ActorHuman, true, false, false},
		{"human successor created first", core.ActorHuman, false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ledger := new(flowLedger).created(approvalTask()).transition(core.StageTriage, core.StageSpec).
				order("spec-1", core.StageSpec, core.ActorSystem)
			if tc.ended {
				ledger.add("work_order.stale", core.ActorSystem, "job-spec-1", map[string]any{"work_order_id": "spec-1"}).
					transition(core.StageImplement, core.StageSpec)
			}
			if tc.first {
				retire(ledger).order("spec-2", core.StageSpec, tc.role)
			} else {
				retire(ledger.order("spec-2", core.StageSpec, tc.role))
			}
			attempts := flowStage(t, ledger.fold(nextaction.NextAction{Actor: nextaction.ActorFactory}), core.StageSpec).Attempts
			if len(attempts) != 2 || attempts[0].Auto || !attempts[0].Superseded || attempts[1].OrderID != "spec-2" || attempts[1].Auto != tc.auto {
				t.Fatalf("attempts = %+v", attempts)
			}
		})
	}
}

func TestFoldTaskFlowCurrentAttemptFromLedger(t *testing.T) {
	ledger := new(flowLedger).created(approvalTask())
	ledger.add("task.assignee.set", core.ActorHuman, "", map[string]any{"assignee_user_id": "user-7"})
	ledger.transition(core.StageTriage, core.StageImplement).order("impl-1", core.StageImplement, core.ActorSystem).
		claim("impl-1", core.StageImplement, "a1")
	renewedLease := flowT0.Add(5 * time.Hour)
	ledger.add("work_order.lease_renewed", core.ActorAgent, "job-impl-1", core.WorkOrder{ID: "impl-1", Stage: core.StageImplement, State: core.WorkOrderClaimed, AttemptID: "a1", LeaseExpiresAt: renewedLease})
	ledger.add("work_order.progress_reported", core.ActorAgent, "job-impl-1", map[string]any{"work_order_id": "impl-1", "message": "tests green"})
	// The next action ranks a document proposal above the claim and names
	// another order; the stage's attempt still comes from its own claim.
	action := nextaction.NextAction{Actor: nextaction.ActorOperator, Kind: nextaction.KindDocumentProposal, CurrentOrder: &nextaction.OrderView{ID: "spec-9"}}
	view := flowStage(t, ledger.fold(action), core.StageImplement).CurrentAttempt
	if view == nil {
		t.Fatal("claimed implement stage has no current_attempt")
	}
	claimEvent := ledger.events[len(ledger.events)-3]
	if view.AttemptID != "a1" || view.SessionID != "session-a1" || view.ClaimedBy != "worker-1" || view.Harness != "omp" || view.Model != "opus" || view.User != "user-7" {
		t.Fatalf("attempt identity = %+v", view)
	}
	if view.ClaimedAt == nil || !view.ClaimedAt.Equal(claimEvent.At) {
		t.Fatalf("claimed_at = %v, want %v", view.ClaimedAt, claimEvent.At)
	}
	if view.LeaseExpiresAt == nil || !view.LeaseExpiresAt.Equal(renewedLease) {
		t.Fatalf("lease_expires_at = %v, want renewed %v", view.LeaseExpiresAt, renewedLease)
	}
	if last := ledger.events[len(ledger.events)-1]; view.LastCheckIn == nil || !view.LastCheckIn.Equal(last.At) || view.Progress != "tests green" {
		t.Fatalf("check-in = %v progress %q", view.LastCheckIn, view.Progress)
	}
	ledger.add("work_order.released", core.ActorAgent, "job-impl-1", map[string]any{"attempt_id": "a1"})
	if got := flowStage(t, ledger.fold(action), core.StageImplement).CurrentAttempt; got != nil {
		t.Fatalf("released order keeps current_attempt %+v", got)
	}
}

func TestFoldTaskFlowLoopsWindow(t *testing.T) {
	ledger := new(flowLedger).created(approvalTask())
	bounce := func() {
		ledger.add("pipeline.bounced", core.ActorSystem, "", map[string]any{"from": "review", "to": "implement"})
	}
	bounce()
	bounce()
	ledger.add("pipeline.bounce_limit", core.ActorSystem, "", map[string]any{"from": "review", "to": "implement"})
	action := nextaction.NextAction{Actor: nextaction.ActorFactory}
	if got := ledger.fold(action).Loops; !reflect.DeepEqual(got, []FlowLoop{{From: "review", To: "implement", Count: 2, Label: "sent back ×2"}}) {
		t.Fatalf("loops = %+v, want review->implement x2", got)
	}
	ledger.add("intervention.redirect", core.ActorSystem, "", map[string]any{})
	if got := ledger.fold(action).Loops; len(got) != 1 || got[0].Count != 2 {
		t.Fatalf("system intervention reset the window: %+v", got)
	}
	ledger.add("intervention.redirect", core.ActorHuman, "", map[string]any{})
	redirect := ledger.events[len(ledger.events)-1]
	bounce()
	ledger.events[len(ledger.events)-1].At = redirect.At // same instant: excluded
	bounce()
	if got := ledger.fold(action).Loops; !reflect.DeepEqual(got, []FlowLoop{{From: "review", To: "implement", Count: 1, Label: "sent back ×1"}}) {
		t.Fatalf("loops after human redirect = %+v, want count 1", got)
	}
}

func TestFoldTaskFlowSide(t *testing.T) {
	ledger := new(flowLedger).created(approvalTask())
	ledger.add("task.dependency_added", core.ActorHuman, "", map[string]any{"task_id": "task", "depends_on_task_id": "dep-a"})
	ledger.add("task.dependency_added", core.ActorHuman, "", map[string]any{"task_id": "task", "depends_on_task_id": "dep-b"})
	ledger.add("task.dependency_removed", core.ActorHuman, "", map[string]any{"task_id": "task", "depends_on_task_id": "dep-b"})
	blocked := nextaction.NextAction{Actor: nextaction.ActorDependency, Kind: nextaction.KindBlocked, BlockedBy: []nextaction.BlockerRef{
		{TaskID: "dep-a", Title: "A", State: "running"}, {TaskID: "dep-b", Title: "B", State: "running"},
	}}
	side := ledger.fold(blocked).Side
	if !reflect.DeepEqual(side.BlockedBy, []nextaction.BlockerRef{{TaskID: "dep-a", Title: "A", State: "running"}}) {
		t.Fatalf("blocked_by = %+v, want only open dep-a", side.BlockedBy)
	}
	if side.Retrying != nil || side.AgentQuestion || side.Parked || side.Closed {
		t.Fatalf("side = %+v", side)
	}
	if !ledger.fold(nextaction.NextAction{Actor: nextaction.ActorOperator, Kind: nextaction.KindAgentQuestion}).Side.AgentQuestion {
		t.Fatal("agent_question next action did not set side.agent_question")
	}
	if ledger.fold(nextaction.NextAction{Actor: nextaction.ActorOperator, Kind: nextaction.KindPlanApproval}).Side.AgentQuestion {
		t.Fatal("an operator next action other than agent_question set side.agent_question")
	}
	parked := new(flowLedger).created(approvalTask()).state(core.TaskParked, core.TaskTriagePark).fold(nextaction.NextAction{Kind: nextaction.KindParked})
	if !parked.Side.Parked || parked.Side.Closed {
		t.Fatalf("parked side = %+v", parked.Side)
	}
	closed := new(flowLedger).created(approvalTask()).state(core.TaskClosed, core.TaskCancel).fold(nextaction.NextAction{Kind: nextaction.KindTerminal})
	if !closed.Side.Closed || closed.Side.Parked {
		t.Fatalf("closed side = %+v", closed.Side)
	}

	retry := new(flowLedger).created(approvalTask()).order("impl-1", core.StageImplement, core.ActorSystem).
		add("work_order.stale", core.ActorSystem, "job-impl-1", map[string]any{"work_order_id": "impl-1"}).
		add("work_order.redispatched", core.ActorSystem, "job-impl-1", map[string]any{"work_order_id": "impl-1", "automatic": true, "recovery_number": 1}).
		add("work_order.stale", core.ActorSystem, "job-impl-1", map[string]any{"work_order_id": "impl-1"}).
		add("work_order.redispatched", core.ActorSystem, "job-impl-1", map[string]any{"work_order_id": "impl-1", "automatic": true, "recovery_number": 2})
	policy := config.RecoveryPolicy{AutomaticRecoveries: 3, WindowText: "24h"}
	if err := policy.Normalize(); err != nil {
		t.Fatal(err)
	}
	retrying := FoldTaskFlow(retry.events, nextaction.NextAction{Actor: nextaction.ActorFactory, Kind: nextaction.KindRetrying, CurrentOrder: &nextaction.OrderView{ID: "impl-1"}}, policy, flowT0)
	if got := retrying.Side.Retrying; got == nil || got.Attempt != 2 || got.Of == nil || *got.Of != 3 {
		t.Fatalf("retrying = %+v, want 2 of 3", got)
	}
	if got := FoldTaskFlow(retry.events, nextaction.NextAction{Actor: nextaction.ActorFactory, Kind: nextaction.KindQueued}, policy, flowT0).Side.Retrying; got != nil {
		t.Fatalf("retrying without a retrying next action: %+v", got)
	}
}

// Each present side state carries its server-stated label and an absent one
// carries none; combined states carry one label each (feature-operator-next-action
// NA-7; req-operator-next-action AC-1.8, AC-4.1).
func TestFoldTaskFlowSideLabels(t *testing.T) {
	policy := config.RecoveryPolicy{AutomaticRecoveries: 3, WindowText: "24h"}
	if err := policy.Normalize(); err != nil {
		t.Fatal(err)
	}
	blockedLedger := new(flowLedger).created(approvalTask())
	blockedLedger.add("task.dependency_added", core.ActorHuman, "", map[string]any{"task_id": "task", "depends_on_task_id": "dep-a"})
	blockedBy := []nextaction.BlockerRef{{TaskID: "dep-a", Title: "A", State: "running"}}
	retryLedger := new(flowLedger).created(approvalTask()).order("impl-1", core.StageImplement, core.ActorSystem)
	retryLedger.add("task.dependency_added", core.ActorHuman, "", map[string]any{"task_id": "task", "depends_on_task_id": "dep-a"})
	cases := []struct {
		name   string
		events []core.Event
		action nextaction.NextAction
		want   map[string]string
	}{
		{"none", new(flowLedger).created(approvalTask()).events, nextaction.NextAction{Actor: nextaction.ActorAgent, Kind: nextaction.KindQueued}, map[string]string{}},
		{"blocked_by", blockedLedger.events, nextaction.NextAction{Actor: nextaction.ActorDependency, Kind: nextaction.KindBlocked, BlockedBy: blockedBy}, map[string]string{FlowSideBlockedBy: "Waiting on another task"}},
		{"retrying", new(flowLedger).created(approvalTask()).order("impl-1", core.StageImplement, core.ActorSystem).events, nextaction.NextAction{Actor: nextaction.ActorFactory, Kind: nextaction.KindRetrying, CurrentOrder: &nextaction.OrderView{ID: "impl-1"}}, map[string]string{FlowSideRetrying: "Auto-retrying"}},
		{"parked", new(flowLedger).created(approvalTask()).state(core.TaskParked, core.TaskTriagePark).events, nextaction.NextAction{Kind: nextaction.KindParked}, map[string]string{FlowSideParked: "Parked"}},
		{"closed", new(flowLedger).created(approvalTask()).state(core.TaskClosed, core.TaskCancel).events, nextaction.NextAction{Kind: nextaction.KindTerminal}, map[string]string{FlowSideClosed: "Closed"}},
		{"agent_question", new(flowLedger).created(approvalTask()).events, nextaction.NextAction{Actor: nextaction.ActorOperator, Kind: nextaction.KindAgentQuestion}, map[string]string{FlowSideAgentQuestion: "Agent asks you"}},
		{"retrying and blocked_by", retryLedger.events, nextaction.NextAction{Actor: nextaction.ActorFactory, Kind: nextaction.KindRetrying, CurrentOrder: &nextaction.OrderView{ID: "impl-1"}, BlockedBy: blockedBy}, map[string]string{FlowSideRetrying: "Auto-retrying", FlowSideBlockedBy: "Waiting on another task"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			side := FoldTaskFlow(tc.events, tc.action, policy, flowT0).Side
			if !reflect.DeepEqual(side.Labels, tc.want) {
				t.Fatalf("labels = %v, want %v (side %+v)", side.Labels, tc.want, side)
			}
			present := map[string]bool{
				FlowSideBlockedBy: len(side.BlockedBy) > 0, FlowSideRetrying: side.Retrying != nil,
				FlowSideParked: side.Parked, FlowSideClosed: side.Closed, FlowSideAgentQuestion: side.AgentQuestion,
			}
			for name, on := range present {
				if _, labelled := side.Labels[name]; labelled != on {
					t.Fatalf("%s present=%v labelled=%v", name, on, labelled)
				}
			}
			raw, err := json.Marshal(side)
			if err != nil {
				t.Fatal(err)
			}
			var wire map[string]json.RawMessage
			if err := json.Unmarshal(raw, &wire); err != nil {
				t.Fatal(err)
			}
			for _, field := range []string{"blocked_by", "retrying", "parked", "closed", "agent_question", "labels"} {
				if _, ok := wire[field]; !ok {
					t.Fatalf("side JSON %s lacks %s", raw, field)
				}
			}
		})
	}
}

func TestFoldTaskFlowRaw(t *testing.T) {
	ledger := new(flowLedger).created(approvalTask()).
		order("spec-2", core.StageSpec, core.ActorSystem).order("spec-1", core.StageSpec, core.ActorSystem).
		add("work_order.retired", core.ActorSystem, "job-spec-2", map[string]any{"work_order_id": "spec-2"}).
		add("pipeline.transition_decided", core.ActorSystem, "", map[string]any{"from_stage": "spec", "next_stage": "implement", "recovery_stage": "spec"}).
		state(core.TaskAwaiting, core.TaskGatePlanRevision)
	raw := ledger.fold(nextaction.NextAction{Actor: nextaction.ActorOperator, Kind: nextaction.KindPlanRevision}).Raw
	want := FlowRaw{State: "awaiting_human", NextStage: "implement", RecoveryStage: "spec", GateKind: "plan_revision", OrderIDs: []string{"spec-2", "spec-1"}}
	if !reflect.DeepEqual(raw, want) {
		t.Fatalf("raw = %+v, want %+v", raw, want)
	}
}

func TestFoldTaskFlowIgnoresMalformedEvents(t *testing.T) {
	ledger := new(flowLedger).created(approvalTask()).transition(core.StageTriage, core.StageImplement).
		order("impl-1", core.StageImplement, core.ActorSystem).claim("impl-1", core.StageImplement, "a1")
	ledger.add("pipeline.bounced", core.ActorSystem, "", map[string]any{"from": "review", "to": "implement"})
	action := nextaction.NextAction{Actor: nextaction.ActorAgent, Kind: nextaction.KindWorking}
	want := ledger.fold(action)
	noisy := &flowLedger{events: append([]core.Event(nil), ledger.events...)}
	noisy.add("mystery.kind", core.ActorSystem, "", map[string]any{"x": 1})
	for _, kind := range []string{"pipeline.bounced", "work_order.claimed", "task.state_changed", "task.created", "job.created"} {
		noisy.add(kind, core.ActorSystem, "job-impl-1", nil)
		noisy.events[len(noisy.events)-1].Payload = json.RawMessage(`{"broken`)
	}
	if got := noisy.fold(action); !reflect.DeepEqual(got, want) {
		t.Fatalf("malformed events changed the flow:\n%+v\nwant\n%+v", got, want)
	}
}

func TestFoldTaskFlowDeterministic(t *testing.T) {
	ledger := new(flowLedger).created(approvalTask()).transition(core.StageTriage, core.StageSpec).
		order("spec-1", core.StageSpec, core.ActorSystem).order("spec-2", core.StageSpec, core.ActorSystem).
		add("work_order.retired", core.ActorSystem, "job-spec-1", map[string]any{"work_order_id": "spec-1"})
	ledger.add("task.dependency_added", core.ActorHuman, "", map[string]any{"depends_on_task_id": "dep-a"})
	ledger.add("task.dependency_added", core.ActorHuman, "", map[string]any{"depends_on_task_id": "dep-b"})
	for _, pair := range [][2]string{{"review", "implement"}, {"verify", "implement"}, {"review", "implement"}} {
		ledger.add("pipeline.bounced", core.ActorSystem, "", map[string]any{"from": pair[0], "to": pair[1]})
	}
	action := nextaction.NextAction{Actor: nextaction.ActorDependency, Kind: nextaction.KindBlocked, BlockedBy: []nextaction.BlockerRef{{TaskID: "dep-b"}, {TaskID: "dep-a"}}}
	reversed := make([]core.Event, len(ledger.events))
	for i, event := range ledger.events {
		reversed[len(reversed)-1-i] = event
	}
	first, err := json.Marshal(FoldTaskFlow(reversed, action, config.DefaultRecoveryPolicy(), flowT0))
	if err != nil {
		t.Fatal(err)
	}
	for i := range 50 {
		again, err := json.Marshal(ledger.fold(action))
		if err != nil {
			t.Fatal(err)
		}
		if string(again) != string(first) {
			t.Fatalf("fold %d differs:\n%s\nwant\n%s", i, again, first)
		}
	}
	flow := ledger.fold(action)
	if got := []string{flow.Side.BlockedBy[0].TaskID, flow.Side.BlockedBy[1].TaskID}; !reflect.DeepEqual(got, []string{"dep-a", "dep-b"}) {
		t.Fatalf("blocked_by order = %v, want event order", got)
	}
	if want := []FlowLoop{{From: "review", To: "implement", Count: 2, Label: "sent back ×2"}, {From: "verify", To: "implement", Count: 1, Label: "sent back ×1"}}; !reflect.DeepEqual(flow.Loops, want) {
		t.Fatalf("loops = %+v, want first-occurrence order %+v", flow.Loops, want)
	}
	// Events sharing an instant fold in ID order, whatever order they arrive in.
	tied := new(flowLedger).created(approvalTask()).state(core.TaskRunning, "").state(core.TaskParked, core.TaskTriagePark)
	tied.events[2].At = tied.events[1].At
	tied.events[1], tied.events[2] = tied.events[2], tied.events[1]
	if got := tied.fold(action).Raw.State; got != string(core.TaskParked) {
		t.Fatalf("same-instant state changes fold to %s, want the higher-ID parked", got)
	}
}

func flowGateMarks(flow TaskFlow) map[core.Stage]string {
	out := map[core.Stage]string{}
	for _, stage := range flow.Stages {
		if stage.GateStatus != nil {
			out[stage.ID] = *stage.GateStatus
		}
	}
	return out
}

// A gate needs the operator only when the next action is that gate's own
// decision; any other operator stop stays on the row status of the stage
// that holds it (req-operator-next-action AC-7.5, AC-7.7).
func TestFoldTaskFlowGateMarks(t *testing.T) {
	planGate := new(flowLedger).created(approvalTask()).transition(core.StageTriage, core.StageSpec).
		order("spec-1", core.StageSpec, core.ActorSystem).claim("spec-1", core.StageSpec, "a1").
		add("work_order.updated", core.ActorAgent, "job-spec-1", map[string]any{"work_order_id": "spec-1", "new_state": core.WorkOrderSubmitted}).
		state(core.TaskAwaiting, core.TaskGateSpec)
	atPlan := new(flowLedger).created(approvalTask()).transition(core.StageTriage, core.StageSpec)
	revision := new(flowLedger).created(approvalTask()).transition(core.StageTriage, core.StageSpec).
		transition(core.StageSpec, core.StageImplement).state(core.TaskAwaiting, core.TaskGatePlanRevision)
	mergeGate := new(flowLedger).created(approvalTask()).transition(core.StageTriage, core.StageSpec).
		transition(core.StageSpec, core.StageImplement).transition(core.StageImplement, core.StageReview).
		transition(core.StageReview, core.StageMerge).state(core.TaskAwaiting, core.TaskGateMerge)
	operator := func(kind string) nextaction.NextAction {
		return nextaction.NextAction{Actor: nextaction.ActorOperator, Kind: kind}
	}
	for name, tc := range map[string]struct {
		ledger      *flowLedger
		action      nextaction.NextAction
		rowStage    core.Stage
		plan, merge string
	}{
		"plan approval":                 {planGate, operator(nextaction.KindPlanApproval), core.StageSpec, FlowNeedsOperator, FlowUpcoming},
		"plan revision":                 {revision, operator(nextaction.KindPlanRevision), core.StageSpec, FlowNeedsOperator, FlowUpcoming},
		"merge approval":                {mergeGate, operator(nextaction.KindMergeApproval), core.StageMerge, FlowDone, FlowNeedsOperator},
		"document proposal at the gate": {planGate, operator(nextaction.KindDocumentProposal), core.StageSpec, FlowWaiting, FlowUpcoming},
		"agent question at the gate":    {mergeGate, operator(nextaction.KindAgentQuestion), core.StageMerge, FlowDone, FlowWaiting},
		"context proposal at Plan":      {atPlan, operator(nextaction.KindContextProposal), core.StageSpec, FlowUpcoming, FlowUpcoming},
		"agent question at Plan":        {atPlan, operator(nextaction.KindAgentQuestion), core.StageSpec, FlowUpcoming, FlowUpcoming},
	} {
		t.Run(name, func(t *testing.T) {
			flow := tc.ledger.fold(tc.action)
			// The aggregate row status is the same for every operator stop.
			if got := flowStage(t, flow, tc.rowStage).Status; got != FlowNeedsOperator {
				t.Fatalf("%s status = %s, want needs_operator", tc.rowStage, got)
			}
			want := map[core.Stage]string{core.StageSpec: tc.plan, core.StageMerge: tc.merge}
			if got := flowGateMarks(flow); !reflect.DeepEqual(got, want) {
				t.Fatalf("gate marks = %v, want %v", got, want)
			}
			for _, stage := range flow.Stages {
				if stage.GateStatus == nil {
					if stage.GateStatusLabel != "" || stage.GateStatusText != "" || stage.GateLabel != "" {
						t.Fatalf("%s has gate copy without a gate mark: %+v", stage.ID, stage)
					}
					continue
				}
				copy := flowGateCopy[*stage.GateStatus]
				if stage.GateStatusLabel != copy.label || stage.GateStatusText != copy.sentence(stage.GateLabel) {
					t.Fatalf("%s gate copy = (%q, %q)", stage.ID, stage.GateStatusLabel, stage.GateStatusText)
				}
			}
		})
	}

	// Disabled gates are off whatever the next action says, and keep their
	// node names.
	off := approvalTask()
	off.SpecApproval, off.MergeApproval = false, false
	flow := new(flowLedger).created(off).fold(operator(nextaction.KindPlanApproval))
	if got := flowGateMarks(flow); !reflect.DeepEqual(got, map[core.Stage]string{core.StageSpec: FlowOff, core.StageMerge: FlowOff}) {
		t.Fatalf("gate marks with both gates off = %v", got)
	}
	if spec := flowStage(t, flow, core.StageSpec); spec.Gate != nil || spec.GateLabel != "Plan approval" || spec.GateStatusText != "This task's policy turned Plan approval off." {
		t.Fatalf("spec with the plan gate off = %+v", spec)
	}
	// A gate the approved task passed reads done.
	approved := new(flowLedger).created(approvalTask()).transition(core.StageReview, core.StageMerge).
		state(core.TaskAwaiting, core.TaskGateMerge).state(core.TaskApproved, core.TaskInterventionApproveReview)
	if got := flowGateMarks(approved.fold(nextaction.NextAction{Actor: nextaction.ActorOperator, Kind: nextaction.KindMergeExecution}))[core.StageMerge]; got != FlowDone {
		t.Fatalf("merge gate on an approved task = %s, want done", got)
	}
}

// traversed and gate_traversed state the recorded path: a reached node is
// traversed, an off node only once a later node is reached.
func TestFoldTaskFlowTraversal(t *testing.T) {
	type marks struct{ stage, gate bool }
	read := func(flow TaskFlow) map[core.Stage]marks {
		out := map[core.Stage]marks{}
		for _, stage := range flow.Stages {
			out[stage.ID] = marks{stage.Traversed, stage.GateTraversed}
		}
		return out
	}
	skipped := core.Task{SpecApproval: false, MergeApproval: false, SetupContract: config.ExecutionSetup{VerifyStage: false}}
	fresh := new(flowLedger).created(skipped)
	if got := read(fresh.fold(nextaction.NextAction{Actor: nextaction.ActorFactory})); got[core.StageSpec] != (marks{}) || got[core.StageVerify] != (marks{}) {
		t.Fatalf("off nodes before the task moved = %v, want untraversed", got)
	}
	passed := new(flowLedger).created(skipped).transition(core.StageTriage, core.StageImplement).
		transition(core.StageImplement, core.StageReview).order("review-1", core.StageReview, core.ActorSystem)
	got := read(passed.fold(nextaction.NextAction{Actor: nextaction.ActorAgent, Kind: nextaction.KindQueued}))
	want := map[core.Stage]marks{
		core.StageTriage: {true, false}, core.StageSpec: {true, true}, core.StageImplement: {true, false},
		core.StageVerify: {true, false}, core.StageReview: {true, false}, core.StageMerge: {false, false},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("traversal at review = %v, want %v", got, want)
	}
	merging := new(flowLedger).created(skipped).transition(core.StageTriage, core.StageImplement).
		transition(core.StageImplement, core.StageReview).transition(core.StageReview, core.StageMerge).state(core.TaskApproved, core.TaskReviewAccept)
	if got := read(merging.fold(nextaction.NextAction{Actor: nextaction.ActorFactory, Kind: nextaction.KindMerging}))[core.StageMerge]; got != (marks{true, true}) {
		t.Fatalf("merge with its gate off while merging = %+v, want stage and skipped gate traversed", got)
	}
	// A bounce keeps the stages already passed done and traversed.
	bounced := new(flowLedger).created(approvalTask()).transition(core.StageTriage, core.StageSpec).
		transition(core.StageSpec, core.StageImplement).transition(core.StageImplement, core.StageReview).
		add("pipeline.bounced", core.ActorSystem, "", map[string]any{"from": "review", "to": "implement"}).
		transition(core.StageReview, core.StageImplement).order("impl-2", core.StageImplement, core.ActorSystem)
	flow := bounced.fold(nextaction.NextAction{Actor: nextaction.ActorAgent, Kind: nextaction.KindQueued})
	if review := flowStage(t, flow, core.StageReview); review.Status != FlowDone || !review.Traversed {
		t.Fatalf("review after a bounce = %+v", review)
	}
	if spec := flowStage(t, flow, core.StageSpec); *spec.GateStatus != FlowDone || !spec.GateTraversed {
		t.Fatalf("plan gate after a bounce = %+v", spec)
	}
}

// side.retrying pairs an attempt with the limit of the same counter, and the
// label states only what is recorded (req-operator-next-action AC-7.5).
func TestFoldTaskFlowRetryPairing(t *testing.T) {
	policy := config.RecoveryPolicy{AutomaticRecoveries: 3, WindowText: "24h"}
	if err := policy.Normalize(); err != nil {
		t.Fatal(err)
	}
	retrying := nextaction.NextAction{Actor: nextaction.ActorFactory, Kind: nextaction.KindRetrying, CurrentOrder: &nextaction.OrderView{ID: "impl-1"}}
	order := func() *flowLedger {
		return new(flowLedger).created(approvalTask()).order("impl-1", core.StageImplement, core.ActorSystem)
	}
	workerRetry := order().add("work_order.requeued", core.ActorSystem, "job-impl-1", map[string]any{"work_order_id": "impl-1", "automatic_retry_count": 2})
	recovered := order().add("work_order.redispatched", core.ActorSystem, "job-impl-1", map[string]any{"work_order_id": "impl-1", "automatic": true, "recovery_number": 1})
	both := order().add("work_order.redispatched", core.ActorSystem, "job-impl-1", map[string]any{"work_order_id": "impl-1", "automatic": true, "recovery_number": 1}).
		add("work_order.requeued", core.ActorSystem, "job-impl-1", map[string]any{"work_order_id": "impl-1", "automatic_retry_count": 5})
	scheduled := retrying
	scheduled.Details.ReasonCode = "automatic_recovery"
	for name, tc := range map[string]struct {
		ledger  *flowLedger
		action  nextaction.NextAction
		attempt int
		of      string
		label   string
	}{
		"scheduled recovery":           {recovered, scheduled, 2, "3", "Auto-retrying · 2 of 3"},
		"recorded recovery":            {recovered, retrying, 1, "3", "Auto-retrying · 1 of 3"},
		"worker retry has no limit":    {workerRetry, retrying, 2, "null", "Auto-retrying · 2"},
		"worker count beside recovery": {both, retrying, 5, "null", "Auto-retrying · 5"},
		"unknown count":                {order(), retrying, 0, "null", "Auto-retrying"},
		"no order":                     {order(), nextaction.NextAction{Actor: nextaction.ActorFactory, Kind: nextaction.KindRetrying}, 0, "null", "Auto-retrying"},
	} {
		t.Run(name, func(t *testing.T) {
			flow := FoldTaskFlow(tc.ledger.events, tc.action, policy, flowT0)
			got := flow.Side.Retrying
			if got == nil || got.Attempt != tc.attempt {
				t.Fatalf("retrying = %+v, want attempt %d", got, tc.attempt)
			}
			raw, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			if want := `{"attempt":` + strconv.Itoa(tc.attempt) + `,"of":` + tc.of + `}`; string(raw) != want {
				t.Fatalf("retrying JSON = %s, want %s", raw, want)
			}
			if label := flow.Side.Labels[FlowSideRetrying]; label != tc.label {
				t.Fatalf("retrying label = %q, want %q", label, tc.label)
			}
		})
	}
	if got := FoldTaskFlow(recovered.events, nextaction.NextAction{Actor: nextaction.ActorFactory, Kind: nextaction.KindQueued}, policy, flowT0).Side.Retrying; got != nil {
		t.Fatalf("retrying without a retrying next action = %+v", got)
	}
}

// elapsed_seconds is the fold's arithmetic over its own now and the claim
// time: truncated, never negative, zero without a claim time, and never reset
// by a renewal or a progress report (req-operator-next-action AC-7.6).
func TestFoldTaskFlowElapsedSeconds(t *testing.T) {
	agent := nextaction.NextAction{Actor: nextaction.ActorAgent, Kind: nextaction.KindWorking}
	claimed := new(flowLedger).created(approvalTask()).transition(core.StageTriage, core.StageImplement).
		order("impl-1", core.StageImplement, core.ActorSystem).claim("impl-1", core.StageImplement, "a1")
	claimedAt := claimed.events[len(claimed.events)-1].At
	elapsed := func(ledger *flowLedger, now time.Time) int64 {
		t.Helper()
		view := flowStage(t, ledger.foldAt(agent, now), core.StageImplement).CurrentAttempt
		if view == nil {
			t.Fatal("claimed stage has no current_attempt")
		}
		return view.ElapsedSeconds
	}
	for name, tc := range map[string]struct {
		now  time.Time
		want int64
	}{
		"ninety seconds":   {claimedAt.Add(90 * time.Second), 90},
		"truncated":        {claimedAt.Add(90*time.Second + 999*time.Millisecond), 90},
		"at the claim":     {claimedAt, 0},
		"future claim":     {claimedAt.Add(-time.Minute), 0},
		"one hour and one": {claimedAt.Add(time.Hour + time.Second), 3601},
	} {
		t.Run(name, func(t *testing.T) {
			if got := elapsed(claimed, tc.now); got != tc.want {
				t.Fatalf("elapsed_seconds = %d, want %d", got, tc.want)
			}
		})
	}
	renewed := &flowLedger{events: append([]core.Event(nil), claimed.events...)}
	renewed.add("work_order.lease_renewed", core.ActorAgent, "job-impl-1", core.WorkOrder{ID: "impl-1", Stage: core.StageImplement, State: core.WorkOrderClaimed, AttemptID: "a1", LeaseExpiresAt: claimedAt.Add(3 * time.Hour)})
	renewed.add("work_order.progress_reported", core.ActorAgent, "job-impl-1", map[string]any{"work_order_id": "impl-1", "message": "halfway"})
	if got := elapsed(renewed, claimedAt.Add(10*time.Minute)); got != 600 {
		t.Fatalf("elapsed after renewal and progress = %d, want 600 from the claim", got)
	}
	// Without execution_started_at the claim event's own time is the start.
	bare := new(flowLedger).created(approvalTask()).transition(core.StageTriage, core.StageImplement).
		order("impl-1", core.StageImplement, core.ActorSystem)
	bare.add("work_order.claimed", core.ActorAgent, "job-impl-1", map[string]any{"work_order_id": "impl-1", "stage": "implement", "attempt_id": "a1", "state": "claimed"})
	eventAt := bare.events[len(bare.events)-1].At
	if got := elapsed(bare, eventAt.Add(42*time.Second)); got != 42 {
		t.Fatalf("elapsed from the claim event = %d, want 42", got)
	}
	// Execution start, when recorded, wins over the claim event's time.
	started := new(flowLedger).created(approvalTask()).transition(core.StageTriage, core.StageImplement).
		order("impl-1", core.StageImplement, core.ActorSystem)
	start := flowT0.Add(-time.Hour)
	started.add("work_order.claimed", core.ActorAgent, "job-impl-1", core.WorkOrder{ID: "impl-1", Stage: core.StageImplement, State: core.WorkOrderClaimed, AttemptID: "a1", ExecutionStartedAt: start})
	if got := elapsed(started, start.Add(2*time.Minute)); got != 120 {
		t.Fatalf("elapsed from execution_started_at = %d, want 120", got)
	}
	if !new(flowLedger).created(approvalTask()).foldAt(agent, claimedAt).AsOf.Equal(claimedAt) {
		t.Fatal("as_of is not the fold's now")
	}
}

// The display, node and loop copy is complete and server-stated, and the
// served shape carries every added field (req-operator-next-action AC-1.8,
// AC-7.7).
func TestFoldTaskFlowDisplayCopy(t *testing.T) {
	ledger := new(flowLedger).created(approvalTask())
	ledger.add("pipeline.bounced", core.ActorSystem, "", map[string]any{"from": "review", "to": "implement", "source": "verification"})
	flow := ledger.fold(nextaction.NextAction{Actor: nextaction.ActorFactory})
	if want := []FlowLoop{{From: core.StageVerify, To: core.StageImplement, Count: 1, Label: "sent back ×1"}}; !reflect.DeepEqual(flow.Loops, want) {
		t.Fatalf("verification loop = %+v, want %+v", flow.Loops, want)
	}
	labels := map[core.Stage][2]string{}
	for _, stage := range flow.Stages {
		labels[stage.ID] = [2]string{stage.Label, stage.GateLabel}
	}
	wantLabels := map[core.Stage][2]string{
		core.StageTriage: {"Triage", ""}, core.StageSpec: {"Plan", "Plan approval"}, core.StageImplement: {"Implement", ""},
		core.StageVerify: {"Verify", ""}, core.StageReview: {"Review", ""}, core.StageMerge: {"Merged", "Merge approval"},
	}
	if !reflect.DeepEqual(labels, wantLabels) {
		t.Fatalf("node labels = %v", labels)
	}
	want := FlowDisplay{
		Legend: []FlowLegendEntry{
			{FlowDone, "Done"}, {FlowInProgress, "In progress"}, {FlowWaiting, "Waiting"}, {FlowNeedsOperator, "Your approval"}, {FlowOff, "Skipped"},
		},
		SideLabels: map[string]string{
			FlowSideBlockedBy: "Waiting on another task", FlowSideRetrying: "Auto-retrying", FlowSideParked: "Parked",
			FlowSideClosed: "Closed", FlowSideAgentQuestion: "Agent asks you",
		},
		SkippedLabel: "skipped", PlanBypassLabel: "skipped for this task", FromAnyStepLabel: "from any step",
	}
	if !reflect.DeepEqual(flow.Display, want) {
		t.Fatalf("display = %+v, want %+v", flow.Display, want)
	}
	if flow.CurrentStage != nil {
		t.Fatalf("current_stage before any transition = %v, want null", *flow.CurrentStage)
	}
	body, err := json.Marshal(flow)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Stages []map[string]json.RawMessage `json:"stages"`
		Rest   map[string]json.RawMessage
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(body, &wire.Rest); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"current_stage", "as_of", "display"} {
		if _, ok := wire.Rest[key]; !ok {
			t.Fatalf("flow JSON lacks %q", key)
		}
	}
	for _, stage := range wire.Stages {
		for _, key := range []string{"label", "gate_label", "gate_status", "gate_status_label", "gate_status_text", "traversed", "gate_traversed"} {
			if _, ok := stage[key]; !ok {
				t.Fatalf("stage %s lacks %q", stage["id"], key)
			}
		}
	}
	// A merged task has no current stage; a claimed order's stage is it.
	merged := new(flowLedger).created(approvalTask()).transition(core.StageReview, core.StageMerge).state(core.TaskMerged, core.TaskMergeConfirm)
	if got := merged.fold(nextaction.NextAction{Actor: nextaction.ActorNone, Kind: nextaction.KindTerminal, CurrentOrder: &nextaction.OrderView{ID: "x", Stage: "merge"}}).CurrentStage; got != nil {
		t.Fatalf("merged current_stage = %v, want null", *got)
	}
	ordered := new(flowLedger).created(approvalTask()).transition(core.StageTriage, core.StageImplement)
	if got := ordered.fold(nextaction.NextAction{Actor: nextaction.ActorAgent, CurrentOrder: &nextaction.OrderView{ID: "v", Stage: "verify"}}).CurrentStage; got == nil || *got != core.StageVerify {
		t.Fatalf("current_stage = %v, want the current order's verify", got)
	}
	if got := ordered.fold(nextaction.NextAction{Actor: nextaction.ActorAgent}).CurrentStage; got == nil || *got != core.StageImplement {
		t.Fatalf("current_stage = %v, want the recorded next stage implement", got)
	}
}
