package store

import (
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/nextaction"
)

// TestTaskFlowStageCopy pins the server-stated sentence the fold gives each
// stage status (feature-operator-next-action NA-7): the chart prints these and
// carries no copy of its own.
func TestTaskFlowStageCopy(t *testing.T) {
	operator := nextaction.NextAction{Actor: nextaction.ActorOperator}
	factory := nextaction.NextAction{Actor: nextaction.ActorFactory}
	planGate := func() *flowLedger {
		return new(flowLedger).created(approvalTask()).transition(core.StageTriage, core.StageSpec).
			order("spec-1", core.StageSpec, core.ActorSystem).claim("spec-1", core.StageSpec, "a1").
			add("work_order.updated", core.ActorAgent, "job-spec-1", map[string]any{"work_order_id": "spec-1", "new_state": core.WorkOrderSubmitted}).
			state(core.TaskAwaiting, core.TaskGateSpec)
	}
	queued := func() *flowLedger {
		return new(flowLedger).created(approvalTask()).transition(core.StageTriage, core.StageImplement).
			order("impl-1", core.StageImplement, core.ActorSystem)
	}
	jobFailure := new(flowLedger).created(approvalTask()).transition(core.StageImplement, core.StageReview).
		order("review-1", core.StageReview, core.ActorSystem).claim("review-1", core.StageReview, "a1").
		add("work_order.child_failed", core.ActorSystem, "job-review-1", map[string]any{"work_order_id": "review-1"}).
		transition(core.StageReview, core.StageMerge).state(core.TaskAwaiting, core.TaskJobFail)
	mergeGate := new(flowLedger).created(approvalTask()).transition(core.StageReview, core.StageMerge).
		state(core.TaskAwaiting, core.TaskGateMerge)
	off := new(flowLedger).created(core.Task{})

	for name, tc := range map[string]struct {
		ledger       *flowLedger
		action       nextaction.NextAction
		stage        core.Stage
		status       string
		label, text  string
		missedNotice bool
	}{
		"done":                          {planGate(), operator, core.StageTriage, FlowDone, "Done", "Triage is done.", false},
		"needs operator at plan gate":   {planGate(), operator, core.StageSpec, FlowNeedsOperator, "Needs you", "Plan is waiting for you to approve the plan.", false},
		"needs operator at merge gate":  {mergeGate, operator, core.StageMerge, FlowNeedsOperator, "Needs you", "Merge is waiting for you to approve the merge.", false},
		"needs operator without a gate": {jobFailure, operator, core.StageReview, FlowNeedsOperator, "Needs you", "Code review is waiting on you; the next action says what to decide.", false},
		"upcoming":                      {planGate(), operator, core.StageImplement, FlowUpcoming, "Upcoming", "Implement has not started yet.", false},
		"waiting":                       {queued(), factory, core.StageImplement, FlowWaiting, "Waiting", "Implement is queued, waiting for a worker to claim it.", false},
		"in progress":                   {queued().claim("impl-1", core.StageImplement, "a1"), factory, core.StageImplement, FlowInProgress, "In progress", "Implement is running now.", false},
		"off":                           {off, factory, core.StageVerify, FlowOff, "Off for this task", "This task's policy turned Verify off.", false},
	} {
		t.Run(name, func(t *testing.T) {
			stage := flowStage(t, tc.ledger.fold(tc.action), tc.stage)
			if stage.Status != tc.status {
				t.Fatalf("%s status = %s, want %s", tc.stage, stage.Status, tc.status)
			}
			if stage.StatusLabel != tc.label || stage.StatusText != tc.text {
				t.Fatalf("%s copy = (%q, %q), want (%q, %q)", tc.stage, stage.StatusLabel, stage.StatusText, tc.label, tc.text)
			}
			if stage.MissedCheckIn != "" {
				t.Fatalf("%s missed_check_in = %q before any lease ran out", tc.stage, stage.MissedCheckIn)
			}
		})
	}

	t.Run("descriptions", func(t *testing.T) {
		want := map[core.Stage]string{
			core.StageTriage:    "Triage reads the task and writes the brief the later stages work from.",
			core.StageSpec:      "An agent writes the execution plan, which the operator approves when the plan gate is on.",
			core.StageImplement: "An agent makes the change on the task branch and opens the pull request.",
			core.StageVerify:    "A verifier runs the task's verification kit against the change and records the evidence.",
			core.StageReview:    "Independent reviewers judge the pull request and approve it or send it back.",
			core.StageMerge:     "The pull request merges into the base branch, after the operator approves when the merge gate is on.",
		}
		for _, stage := range new(flowLedger).created(approvalTask()).fold(factory).Stages {
			if stage.Description != want[stage.ID] {
				t.Fatalf("%s description = %q, want %q", stage.ID, stage.Description, want[stage.ID])
			}
		}
	})

	// Every status the fold can assign and every stage it draws has copy, so
	// a new status or stage cannot reach the chart as a bare token.
	t.Run("complete", func(t *testing.T) {
		for _, status := range []string{FlowDone, FlowInProgress, FlowWaiting, FlowNeedsOperator, FlowOff, FlowUpcoming} {
			if entry, ok := flowStatusCopy[status]; !ok || entry.label == "" || entry.sentence("Stage") == "" {
				t.Fatalf("status %s has no copy", status)
			}
		}
		for _, stage := range taskFlowStages {
			if flowStageNames[stage] == "" || flowStageDescriptions[stage] == "" {
				t.Fatalf("stage %s has no name or description", stage)
			}
		}
	})
}

// TestTaskFlowMissedCheckIn pins the missed check-in sentence: present only on
// a stage whose current attempt's lease is strictly before now.
func TestTaskFlowMissedCheckIn(t *testing.T) {
	agent := nextaction.NextAction{Actor: nextaction.ActorAgent}
	claimed := new(flowLedger).created(approvalTask()).transition(core.StageTriage, core.StageImplement).
		order("impl-1", core.StageImplement, core.ActorSystem).claim("impl-1", core.StageImplement, "a1")
	lease := claimed.events[len(claimed.events)-1].At.Add(time.Hour)
	missed := "Missed check-in: the lease expired at " + lease.Format("2006-01-02 15:04") + " UTC and the agent has not renewed it."
	if got := lease.Format("2006-01-02 15:04 UTC"); got != "2026-10-11 10:04 UTC" {
		t.Fatalf("lease = %s, want the ledger's 10:04 UTC", got)
	}
	for name, tc := range map[string]struct {
		now  time.Time
		want string
	}{
		"before the lease": {lease.Add(-time.Second), ""},
		"at the lease":     {lease, ""},
		"past the lease":   {lease.Add(time.Second), missed},
	} {
		t.Run(name, func(t *testing.T) {
			flow := claimed.foldAt(agent, tc.now)
			if got := flowStage(t, flow, core.StageImplement).MissedCheckIn; got != tc.want {
				t.Fatalf("missed_check_in = %q, want %q", got, tc.want)
			}
			for _, stage := range flow.Stages {
				if stage.ID != core.StageImplement && stage.MissedCheckIn != "" {
					t.Fatalf("%s without a current attempt has missed_check_in %q", stage.ID, stage.MissedCheckIn)
				}
			}
		})
	}

	// A released order keeps its lease in the ledger but has no current
	// attempt, so nothing is missed.
	released := new(flowLedger).created(approvalTask()).transition(core.StageTriage, core.StageImplement).
		order("impl-1", core.StageImplement, core.ActorSystem).claim("impl-1", core.StageImplement, "a1").
		add("work_order.released", core.ActorAgent, "job-impl-1", map[string]any{"work_order_id": "impl-1"})
	stage := flowStage(t, released.foldAt(agent, lease.Add(time.Hour)), core.StageImplement)
	if stage.CurrentAttempt != nil || stage.MissedCheckIn != "" {
		t.Fatalf("released order: current_attempt = %+v, missed_check_in = %q; want neither", stage.CurrentAttempt, stage.MissedCheckIn)
	}
}
