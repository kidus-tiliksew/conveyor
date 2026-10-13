package storetest

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// runReviewAccept is the DEC-64(8) admission matrix
// (component-task-lifecycle v13): an accepted review round leaves running
// through one review.accept edge when the merge gate is off, on any refresh
// round, or for a legacy policy-0 L0 task, and through gate.merge otherwise.
// Neither path writes an intervention.
func runReviewAccept(t *testing.T, x Fixture) {
	type reviewAcceptCase struct {
		name    string
		verdict string
		policy  int
		gate    bool
		level   core.EscalationLevel
		kind    string
		// nonAdvancing makes the seats approve the refresh baseline
		// (store.NonAdvancingRefreshBinding).
		nonAdvancing bool
		want         core.TaskCommand
	}
	cases := []reviewAcceptCase{
		{name: "A1GateOffFirstAcceptance", verdict: "approve", policy: 1, level: core.L2, want: core.TaskReviewAccept},
		{name: "A2RefreshGateOn", verdict: "approve", policy: 1, gate: true, level: core.L2, kind: "refresh", want: core.TaskReviewAccept},
		{name: "A3RefreshGateOff", verdict: "approve", policy: 1, level: core.L2, kind: "refresh", want: core.TaskReviewAccept},
		{name: "A4LegacyL0GateFlagOn", verdict: "approve", policy: 0, gate: true, level: core.L0, want: core.TaskReviewAccept},
		{name: "A5LegacyL0GateOff", verdict: "approve", policy: 0, level: core.L0, want: core.TaskReviewAccept},
		{name: "A6NonAdvancingRefresh", verdict: "approve", policy: 1, gate: true, level: core.L2, kind: "refresh", nonAdvancing: true, want: core.TaskReviewAccept},
		{name: "N1FirstAcceptanceGateOn", verdict: "approve", policy: 1, gate: true, level: core.L2, want: core.TaskGateMerge},
		{name: "N2LegacyNonL0", verdict: "approve", policy: 0, level: core.L2, want: core.TaskGateMerge},
		// The changes_requested bounce requeues through stage.advance (PlanChangesRequested Requeue).
		{name: "N3RefreshChangesRequested", verdict: "changes_requested", policy: 1, gate: true, level: core.L2, kind: "refresh", want: core.TaskStageAdvance},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			st, ctx := x.Backend, x.Context
			const priorHead, baseline, head = "prior-approved-head", "refresh-base", "refresh-head"
			id := core.NewTaskID()
			task := core.Task{ID: id, Workspace: x.Workspace, Repo: "conveyor", Title: id, BaseBranch: "main", Branch: "conveyor/task-" + id,
				State: core.TaskRunning, NextStage: core.StageReview, CreatedAt: time.Now().UTC().Truncate(time.Microsecond)}
			task.PolicyVersion, task.MergeApproval, task.Level = test.policy, test.gate, test.level
			task.ApprovedHeadSHA = priorHead
			if test.kind == "refresh" {
				task.RefreshBaselineSHA, task.RefreshHeadSHA, task.RefreshReviewScope = baseline, head, "delta"
			}
			requireOK(t, st.CreateTask(ctx, task))
			before, err := st.GetTask(ctx, task.ID)
			requireOK(t, err)
			interventionsBefore, err := st.ListInterventions(ctx, task.ID)
			requireOK(t, err)
			eventsBefore, err := st.ListEvents(ctx, task.ID)
			requireOK(t, err)

			reviewed := head
			if test.nonAdvancing {
				reviewed = baseline
			}
			jobs, orders := reviewRound(task.ID, 1)
			requireOK(t, CreateReviewRound(ctx, st, task.ID, jobs, orders))
			for i, order := range orders {
				decision := core.ReviewDecision{
					TaskID: task.ID, JobID: order.JobID, ReviewWorkOrderID: order.ID, ReviewRound: 1, ReviewSeat: i + 1,
					Verdict: test.verdict, ReasonCode: "verified", Summary: "review.accept matrix", MaxBounces: 3,
					ReviewedCommitSHA: reviewed, ReviewKind: test.kind,
					Level: test.level, PolicyVersion: test.policy, MergeApproval: test.gate,
				}
				if test.kind == "refresh" {
					decision.BaselineSHA, decision.HeadSHA = baseline, head
				}
				requireOK(t, taskops.New(st).AcceptReviewDecision(ctx, decision))
			}

			current, err := st.GetTask(ctx, task.ID)
			requireOK(t, err)
			interventions, err := st.ListInterventions(ctx, task.ID)
			requireOK(t, err)
			events, err := st.ListEvents(ctx, task.ID)
			requireOK(t, err)
			var changes []reviewAcceptStateChange
			kinds := map[string]int{}
			for _, event := range events[len(eventsBefore):] {
				kinds[event.Kind]++
				if event.Kind != "task.state_changed" {
					continue
				}
				var change reviewAcceptStateChange
				requireOK(t, json.Unmarshal(event.Payload, &change))
				changes = append(changes, change)
			}
			for _, change := range changes {
				if test.want != core.TaskReviewAccept && change.Command == string(core.TaskReviewAccept) {
					t.Fatalf("non-admitted case recorded review.accept: %+v", changes)
				}
			}

			switch test.want {
			case core.TaskReviewAccept:
				want := reviewAcceptStateChange{From: string(core.TaskRunning), To: string(core.TaskApproved), Command: string(core.TaskReviewAccept)}
				if len(changes) != 1 || changes[0] != want {
					t.Fatalf("state changes=%+v, want exactly [%+v]", changes, want)
				}
				if current.State != core.TaskApproved || current.GateKind != "" {
					t.Fatalf("task state=%s gate_kind=%q, want approved with no gate", current.State, current.GateKind)
				}
				if test.nonAdvancing {
					if current.ReviewedHeadSHA != baseline || current.ApprovedHeadSHA != before.ApprovedHeadSHA || kinds["review.refresh_binding_not_advanced"] != 1 {
						t.Fatalf("non-advancing binding reviewed=%q approved=%q (before %q) not_advanced=%d",
							current.ReviewedHeadSHA, current.ApprovedHeadSHA, before.ApprovedHeadSHA, kinds["review.refresh_binding_not_advanced"])
					}
				} else if current.ApprovedHeadSHA != head || current.ReviewedHeadSHA != head || current.ApprovalStale ||
					current.RefreshBaselineSHA != "" || current.RefreshHeadSHA != "" || current.RefreshReviewScope != "" {
					t.Fatalf("head binding=%+v, want approved=reviewed=%q, fresh, refresh fields cleared", current, head)
				}
				if violations := store.AuditLifecycleHistory(events); len(violations) != 0 {
					t.Fatalf("lifecycle audit violations=%+v", violations)
				}
			case core.TaskGateMerge:
				want := reviewAcceptStateChange{From: string(core.TaskRunning), To: string(core.TaskAwaiting), Command: string(core.TaskGateMerge)}
				if len(changes) != 1 || changes[0] != want {
					t.Fatalf("state changes=%+v, want exactly [%+v]", changes, want)
				}
				if current.State != core.TaskAwaiting || current.GateKind != core.GateKindMergeApproval || current.RecoveryStage != core.StageImplement {
					t.Fatalf("task state=%s gate_kind=%q recovery=%q, want merge_approval gate", current.State, current.GateKind, current.RecoveryStage)
				}
			default:
				if current.State != core.TaskQueued || current.NextStage != core.StageImplement || len(changes) != 1 || changes[0].Command != string(core.TaskStageAdvance) {
					t.Fatalf("changes_requested refresh state=%s next=%s changes=%+v, want one stage.advance bounce to implement", current.State, current.NextStage, changes)
				}
			}
			// Review acceptance never writes an intervention, admitted or not.
			// The changes_requested bounce records its agent review feedback row.
			wantInterventions := len(interventionsBefore)
			if test.want == core.TaskStageAdvance {
				wantInterventions++
			}
			if len(interventions) != wantInterventions {
				t.Fatalf("interventions=%d, want %d", len(interventions), wantInterventions)
			}
		})
	}
}

type reviewAcceptStateChange struct {
	From    string `json:"from"`
	To      string `json:"to"`
	Command string `json:"command"`
}
