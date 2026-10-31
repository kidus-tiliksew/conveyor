package singlestore

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
	"github.com/kidus-tiliksew/conveyor/internal/workorder"
)

// Claims must pin the complete corpus without copying document bodies into the
// audit log. Exercise the service's live authority resolution and real SQL writes.
func TestSingleStoreLargeGovernanceClaimIntegration(t *testing.T) {
	st := integrationStore(t)
	ctx := store.WithActor(store.WithWorkspace(t.Context(), "large-claim"), store.Actor{ID: "operator", Role: core.ActorHuman})
	cfg := &config.Config{Workspace: "large-claim", Routing: config.Routing{Stages: map[string]config.StageRoute{
		"review": {Execution: config.ExecutionMCP, Timeout: time.Hour},
		"verify": {Execution: config.ExecutionMCP, Timeout: time.Hour},
	}}}
	if _, err := st.CreateWorkspace(ctx, "large-claim", "Large claim", cfg); err != nil {
		t.Fatal(err)
	}
	statement := strings.Repeat("Preserve complete requirement authority. ", 1000)
	requirement, version, err := st.CreateRequirement(ctx, core.Requirement{ID: "large-requirement", Title: "Large requirement"}, core.RequirementVersion{Content: "# Requirement\n\n" + statement, Origin: core.RequirementOriginOperator, Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: statement}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.ConfirmRequirementVersion(ctx, requirement.ID, version.Version); err != nil {
		t.Fatal(err)
	}
	content := strings.Repeat("Complete design authority, not an abbreviated prompt.\n", 3500) + "\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/**\n```"
	for i := range 7 {
		design, version, err := st.CreateSystemDesign(ctx, core.SystemDesign{ID: fmt.Sprintf("large-%d", i), Title: fmt.Sprintf("Large %d", i), Category: "component"}, core.SystemDesignVersion{Content: content, Origin: core.SystemDesignOriginOperator})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := st.ConfirmSystemDesignVersion(ctx, design.ID, version.Version); err != nil {
			t.Fatal(err)
		}
	}
	decision, err := st.ProposeDecision(ctx, core.Decision{Statement: "Keep the complete governance corpus.", Context: strings.Repeat("Decision context.\n", 3000), AlternativesRejected: "Truncation loses authority.", Origin: core.DecisionOriginOperator})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ConfirmDecision(ctx, decision.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := storetest.EnsureWorkerEnrollment(ctx, st, "large-claim-worker"); err != nil {
		t.Fatal(err)
	}
	service := &workorder.Service{Store: st, ConfigProvider: func(_ context.Context) (*config.Config, error) { return cfg, nil }}
	for _, stage := range []core.Stage{core.StageReview, core.StageVerify} {
		t.Run(string(stage), func(t *testing.T) {
			id := "large-" + string(stage)
			task := core.Task{ID: id, Workspace: "large-claim", Repo: "conveyor", Title: id, BaseBranch: "main", Branch: "task/" + id, State: core.TaskRunning, NextStage: stage, ReviewedHeadSHA: "submitted-head", CreatedAt: time.Now().UTC()}
			task.SetupContract.VerifyStage = stage == core.StageVerify
			if err := st.CreateTask(ctx, task); err != nil {
				t.Fatal(err)
			}
			if _, _, err := st.ProposeTaskContext(ctx, core.TaskContextProposalInput{TaskID: id, TargetKind: core.TaskContextProposalRequirement, TargetID: requirement.ID, Source: core.TaskContextProposalTriage, Justification: "Govern this task's claim."}); err != nil {
				t.Fatal(err)
			}
			if _, err := st.ConfirmTaskContextProposal(ctx, id, core.TaskContextProposalRequirement, requirement.ID); err != nil {
				t.Fatal(err)
			}
			job := core.Job{ID: id + "-1", TaskID: id, Stage: stage, State: core.JobPending}
			if err := st.CreateJob(ctx, job); err != nil {
				t.Fatal(err)
			}
			if err := storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, JobID: job.ID, TaskID: id, Stage: stage, HeadSHA: task.ReviewedHeadSHA, State: core.WorkOrderQueued}); err != nil {
				t.Fatal(err)
			}
			claimed, err := service.Claim(ctx, job.ID, core.WorkOrderClaim{ClaimantID: "large-claim-worker", WorkerID: "large-claim-worker", SessionID: id, ClientToken: id + "-token", Lease: time.Minute})
			if err != nil {
				t.Fatalf("claim %s with complete governance: %v", stage, err)
			}
			if claimed.GovernanceSnapshot == nil || len(claimed.GovernanceSnapshot.Designs) != 7 || len(claimed.GovernanceSnapshot.Decisions) != 1 {
				t.Fatal("claim lost governance authority")
			}
			if len(claimed.ServedRequirementSnapshot) != 1 || len(claimed.ServedRequirementSnapshot[0].Statements) != 1 || claimed.ServedRequirementSnapshot[0].Statements[0].Statement != statement {
				t.Fatal("claim lost served requirement authority")
			}
			for _, design := range claimed.GovernanceSnapshot.Designs {
				if design.Content != content {
					t.Fatal("claim truncated a design")
				}
			}
			var stored []byte
			if err := st.db.QueryRowContext(ctx, "SELECT governance_snapshot FROM work_orders WHERE workspace_id=? AND id=?", "large-claim", job.ID).Scan(&stored); err != nil {
				t.Fatal(err)
			}
			if len(stored) <= 1<<20 || !json.Valid(stored) {
				t.Fatalf("stored corpus must remain valid and exceed 1 MiB: %d bytes", len(stored))
			}
			claimed.Progress = "Pinned complete authority"
			if err := storetest.UpdateWorkOrder(ctx, st, claimed); err != nil {
				t.Fatal(err)
			}
			if _, err := taskops.New(st).TickOrderClock(ctx, claimed.ExecutionDeadline.Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			staleJob := core.Job{ID: id + "-stale", TaskID: id, Stage: stage, State: core.JobPending}
			if err := st.CreateJob(ctx, staleJob); err != nil {
				t.Fatal(err)
			}
			if err := storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: staleJob.ID, JobID: staleJob.ID, TaskID: id, Stage: stage, HeadSHA: task.ReviewedHeadSHA, State: core.WorkOrderQueued, GovernanceSnapshot: claimed.GovernanceSnapshot, QueueDeadline: time.Now().Add(-time.Minute)}); err != nil {
				t.Fatal(err)
			}
			if _, err := taskops.New(st).TickOrderClock(ctx, time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			events, err := st.ListEvents(ctx, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			found := map[string]bool{}
			maxEventBytes := 0
			for _, event := range events {
				if !strings.HasPrefix(event.Kind, "work_order.") {
					continue
				}
				maxEventBytes = max(maxEventBytes, len(event.Payload))
				if len(event.Payload) > 16<<10 || strings.Contains(string(event.Payload), "governance_snapshot") || strings.Contains(string(event.Payload), "served_requirement_snapshot") || strings.Contains(string(event.Payload), content[:100]) {
					t.Fatalf("%s duplicated authority: %d bytes", event.Kind, len(event.Payload))
				}
				if event.Kind == "work_order.claimed" {
					var recorded core.WorkOrder
					if err := json.Unmarshal(event.Payload, &recorded); err != nil || recorded.ID != claimed.ID || recorded.AttemptID != claimed.AttemptID || recorded.SessionID != claimed.SessionID {
						t.Fatalf("claim event lost attempt identity: %v", err)
					}
					found[event.Kind] = true
				}
				if event.Kind == "work_order.updated" || event.Kind == "work_order.timed_out" || event.Kind == "work_order.stale" {
					found[event.Kind] = true
				}
			}
			for _, kind := range []string{"work_order.claimed", "work_order.updated", "work_order.timed_out", "work_order.stale"} {
				if !found[kind] {
					t.Fatalf("%s audit event missing", kind)
				}
			}
			t.Logf("%s: stored governance=%d bytes; largest work-order event=%d bytes", stage, len(stored), maxEventBytes)
		})
	}
}

// A large non-JSON string parameter must also round-trip through the actual
// aggregate writer; this is a protocol fix, not a snapshot-specific workaround.
func TestSingleStoreLargeWorkOrderDirectionIntegration(t *testing.T) {
	st := integrationStore(t)
	ctx := store.WithActor(store.WithWorkspace(t.Context(), "large-direction"), store.SystemActor())
	if _, err := st.CreateWorkspace(ctx, "large-direction", "Large direction", &config.Config{Workspace: "large-direction"}); err != nil {
		t.Fatal(err)
	}
	task := core.Task{ID: "large-direction", Workspace: "large-direction", Repo: "conveyor", Title: "Large direction", Branch: "task/large-direction", State: core.TaskRunning, NextStage: core.StageImplement}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: task.ID + "-1", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	direction := strings.Repeat("Operator's quoted 'direction' \\\\ \\x00 — 完整\n", 60000)
	if err := storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, JobID: job.ID, TaskID: task.ID, Stage: job.Stage, OperatorDirection: direction}); err != nil {
		t.Fatal(err)
	}
	persisted, err := st.GetWorkOrder(ctx, job.ID)
	if err != nil || persisted.OperatorDirection != direction {
		t.Fatalf("large string parameter was not preserved: %v", err)
	}
	t.Logf("non-JSON string parameter round-tripped %d bytes", len(direction))
}
