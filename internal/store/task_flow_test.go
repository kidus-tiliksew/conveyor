package store

import (
	"encoding/json"
	"reflect"
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

func (l *flowLedger) fold(action nextaction.NextAction) TaskFlow {
	return FoldTaskFlow(l.events, action, config.DefaultRecoveryPolicy())
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
		task := approvalTask()
		task.MergeApproval = false
		ledger := new(flowLedger).created(task).transition(core.StageReview, core.StageMerge).state(core.TaskApproved, core.TaskInterventionApproveReview)
		got := flowStage(t, ledger.fold(nextaction.NextAction{Actor: nextaction.ActorFactory, Kind: nextaction.KindMerging}), core.StageMerge).Status
		if got != FlowInProgress {
			t.Fatalf("merge = %s, want in_progress", got)
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
	if got := ledger.fold(action).Loops; !reflect.DeepEqual(got, []FlowLoop{{From: "review", To: "implement", Count: 2}}) {
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
	if got := ledger.fold(action).Loops; !reflect.DeepEqual(got, []FlowLoop{{From: "review", To: "implement", Count: 1}}) {
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
	retrying := FoldTaskFlow(retry.events, nextaction.NextAction{Actor: nextaction.ActorFactory, Kind: nextaction.KindRetrying, CurrentOrder: &nextaction.OrderView{ID: "impl-1"}}, policy)
	if retrying.Side.Retrying == nil || *retrying.Side.Retrying != (FlowRetrying{Attempt: 2, Of: 3}) {
		t.Fatalf("retrying = %+v, want 2 of 3", retrying.Side.Retrying)
	}
	if got := FoldTaskFlow(retry.events, nextaction.NextAction{Actor: nextaction.ActorFactory, Kind: nextaction.KindQueued}, policy).Side.Retrying; got != nil {
		t.Fatalf("retrying without a retrying next action: %+v", got)
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
	first, err := json.Marshal(FoldTaskFlow(reversed, action, config.DefaultRecoveryPolicy()))
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
	if want := []FlowLoop{{From: "review", To: "implement", Count: 2}, {From: "verify", To: "implement", Count: 1}}; !reflect.DeepEqual(flow.Loops, want) {
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
