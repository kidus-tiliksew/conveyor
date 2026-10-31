package singlestore

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

// case: EVC261031445A96-008
func TestSingleStoreAcceptedReviewClearsSubmittedImplementationContinuationIntegration(t *testing.T) {
	st := integrationStore(t)
	workspace := "continuation-review-" + core.NewTaskID()
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), workspace)
	if _, err := st.BootstrapWorkspaceConfig(ctx, &config.Config{Workspace: workspace, Repos: []config.Repo{{Name: "app", URL: "https://example.test/app.git", Base: "main"}}}); err != nil {
		t.Fatal(err)
	}
	for _, verdict := range []string{"approve", "changes_requested"} {
		t.Run(verdict, func(t *testing.T) {
			now := time.Now().UTC()
			task := core.Task{ID: core.NewTaskID(), Workspace: workspace, Repo: "app", Branch: "conveyor/task-review-continuation-" + verdict, State: core.TaskRunning, NextStage: core.StageImplement, PolicyVersion: 1, CreatedAt: now}
			if err := st.CreateTask(ctx, task); err != nil {
				t.Fatal(err)
			}
			implementJob := core.Job{ID: task.ID + "-implement-1", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
			if err := st.CreateJob(ctx, implementJob); err != nil {
				t.Fatal(err)
			}
			requirements := []core.ServedRequirementContext{{ID: "req-continuation", Version: 3, Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Retain original implementation authority."}}}}
			governance := &core.GovernanceSnapshot{Designs: []core.GovernanceDesignContext{{ID: "design-continuation", Version: 4, Content: "Original claim-time design body."}}, Decisions: []core.Decision{{ID: "DEC-1", Status: core.DecisionConfirmed, Statement: "Keep claim-time authority."}}}
			if err := storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: implementJob.ID, TaskID: task.ID, JobID: implementJob.ID, Stage: core.StageImplement, State: core.WorkOrderQueued, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now, ServedRequirementSnapshot: requirements, GovernanceSnapshot: governance}); err != nil {
				t.Fatal(err)
			}
			claimed, err := storetest.For(st).ClaimWorkOrder(ctx, implementJob.ID, core.WorkOrderClaim{SessionID: "implement-session-" + verdict, ClientToken: "implement-token-" + verdict, ClaimantID: "run:implementer", WorkerID: "worker-implement-" + workspace, Agent: "codex", Lease: time.Minute, ExecutionTimeout: time.Hour})
			if err != nil {
				t.Fatal(err)
			}
			identity := core.WorkOrderClaimIdentity{WorkerID: claimed.WorkerID, ClaimantID: claimed.ClaimantID, SessionID: claimed.SessionID}
			if _, err = st.RecordWorkOrderContinuation(ctx, claimed.ID, identity, core.WorkOrderContinuation{SessionID: "native-session", AttemptID: claimed.AttemptID, Harness: "codex", LaunchEnvironment: "worker-implement/env"}); err != nil {
				t.Fatal(err)
			}
			implement, err := st.GetWorkOrder(ctx, claimed.ID)
			if err != nil {
				t.Fatal(err)
			}
			implement.State = core.WorkOrderSubmitted
			if err = storetest.For(st).UpdateWorkOrder(ctx, implement, core.WorkOrderCmdSubmitForReview); err != nil {
				t.Fatal(err)
			}
			submitted, err := st.GetWorkOrder(ctx, implement.ID)
			if err != nil {
				t.Fatal(err)
			}
			if submitted.State != core.WorkOrderSubmitted || submitted.ContinuationSessionID != "native-session" || submitted.ContinuationAttemptID != claimed.AttemptID ||
				submitted.ContinuationHarness != "codex" || submitted.ContinuationLaunchEnvironment != "worker-implement/env" {
				t.Fatalf("submitted implementation lost continuation before review acceptance: %+v", submitted)
			}
			before, err := st.ListEvents(ctx, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			reviewJob := core.Job{ID: task.ID + "-review-1-seat-1", TaskID: task.ID, Stage: core.StageReview, State: core.JobPending}
			reviewOrder := core.WorkOrder{ID: reviewJob.ID, TaskID: task.ID, JobID: reviewJob.ID, Stage: core.StageReview, State: core.WorkOrderQueued, ReviewRound: 1, ReviewSeat: 1, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now}
			if err = st.CreateJob(ctx, reviewJob); err != nil {
				t.Fatal(err)
			}
			if err = storetest.For(st).CreateWorkOrder(ctx, reviewOrder); err != nil {
				t.Fatal(err)
			}
			claimedReview, err := storetest.For(st).ClaimWorkOrder(ctx, reviewOrder.ID, core.WorkOrderClaim{SessionID: "review-session-" + verdict, ClientToken: "review-token-" + verdict, ClaimantID: "run:reviewer", WorkerID: "worker-review-" + workspace, Agent: "codex", Lease: time.Minute, ExecutionTimeout: time.Hour})
			if err != nil {
				t.Fatal(err)
			}
			decision := core.ReviewDecision{TaskID: task.ID, JobID: reviewJob.ID, ReviewWorkOrderID: reviewOrder.ID, ClaimSession: claimedReview.SessionID, ReviewRound: 1, ReviewSeat: 1, Verdict: verdict, ReasonCode: "approved", Summary: "accepted", PolicyVersion: 1, MaxBounces: 3}
			if verdict == "changes_requested" {
				decision.ReasonCode, decision.Feedback = "defect", "Fix the defect"
			}
			if err = storetest.For(st).AcceptReviewDecision(ctx, decision); err != nil {
				t.Fatal(err)
			}
			persisted, err := st.GetWorkOrder(ctx, implement.ID)
			if err != nil {
				t.Fatal(err)
			}
			if persisted.State != core.WorkOrderSubmitted || persisted.ContinuationSessionID != "" || persisted.ContinuationAttemptID != "" || persisted.ContinuationHarness != "" || persisted.ContinuationLaunchEnvironment != "" {
				t.Fatalf("submitted implementation continuation was not cleared: %+v", persisted)
			}
			events, err := st.ListEvents(ctx, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(events) < len(before) || !reflect.DeepEqual(events[:len(before)], before) {
				t.Fatal("review acceptance changed historical events")
			}
			clearingEvents := 0
			for _, event := range events[len(before):] {
				if event.Kind != "work_order.updated" || event.JobID != implement.JobID {
					continue
				}
				clearingEvents++
				var payload map[string]json.RawMessage
				if err := json.Unmarshal(event.Payload, &payload); err != nil {
					t.Fatal(err)
				}
				if _, exists := payload["served_requirement_snapshot"]; exists {
					t.Fatal("continuation-clear event duplicated served requirement authority")
				}
				if _, exists := payload["governance_snapshot"]; exists {
					t.Fatal("continuation-clear event duplicated governance authority")
				}
				var recorded core.WorkOrder
				if err := json.Unmarshal(event.Payload, &recorded); err != nil {
					t.Fatal(err)
				}
				if recorded.ID != implement.ID || recorded.TaskID != task.ID || recorded.AttemptID != implement.AttemptID ||
					recorded.SessionID != implement.SessionID || recorded.State != core.WorkOrderSubmitted ||
					recorded.ContinuationSessionID != "" || recorded.ContinuationAttemptID != "" ||
					recorded.ContinuationHarness != "" || recorded.ContinuationLaunchEnvironment != "" {
					t.Fatalf("continuation-clear event lost lifecycle identity or retained continuation: %+v", recorded)
				}
			}
			if clearingEvents != 1 {
				t.Fatalf("continuation-clear events=%d, want 1", clearingEvents)
			}
			persisted, err = st.GetWorkOrder(ctx, implement.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(persisted.ServedRequirementSnapshot, requirements) || !reflect.DeepEqual(persisted.GovernanceSnapshot, governance) {
				t.Fatal("reading continuation-clear history changed canonical claim-time authority")
			}
		})
	}
}
