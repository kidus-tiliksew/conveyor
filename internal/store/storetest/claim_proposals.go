package storetest

import (
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// runClaimsProceedWithPendingProposals pins on every backend that a pending
// document proposal holds no task's work: verify and review claims proceed
// while a task-origin version of either tier is pending, on a live or archived
// document, and that version stays pending and decidable afterward. The other
// claim restrictions still refuse (req-260810-23b69f REQ-3; DEC-102(6)).
func runClaimsProceedWithPendingProposals(t *testing.T, x Fixture) {
	st, ctx := x.Backend, x.Context
	now := time.Now().UTC().Truncate(time.Microsecond)
	newTask := func(stage core.Stage, dependencies ...string) core.Task {
		t.Helper()
		id := core.NewTaskID()
		task := core.Task{ID: id, Workspace: x.Workspace, Repo: "conveyor", Title: id, BaseBranch: "main", Branch: "conveyor/task-" + id, State: core.TaskRunning, NextStage: stage, CreatedAt: now}
		job := core.Job{ID: id + "-" + string(stage) + "-1", TaskID: id, Stage: stage, State: core.JobPending}
		if stage == core.StageReview {
			requireOK(t, st.CreateTask(ctx, task))
			requireOK(t, st.CreateJob(ctx, job))
			requireOK(t, For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: id, JobID: job.ID, Stage: stage, State: core.WorkOrderQueued}))
			return task
		}
		task.ReviewedHeadSHA = "submitted-head"
		task.SetupContract.VerifyStage = true
		requireOK(t, st.CreateTaskWithDependencies(ctx, task, dependencies))
		_, err := CreateStageWorkOrder(ctx, st, job, core.WorkOrder{ID: job.ID, JobID: job.ID, TaskID: id, Stage: stage, State: core.WorkOrderQueued, HeadSHA: task.ReviewedHeadSHA, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now})
		requireOK(t, err)
		return task
	}
	claim := func(task core.Task, stage core.Stage) (core.WorkOrder, error) {
		t.Helper()
		session := "pending-" + core.NewTaskID()
		return For(st).ClaimWorkOrder(ctx, task.ID+"-"+string(stage)+"-1", core.WorkOrderClaim{SessionID: session, ClientToken: session + "-token", Lease: time.Minute, ExecutionTimeout: time.Hour})
	}
	requireClaimed := func(t *testing.T, task core.Task, stage core.Stage) {
		t.Helper()
		order, err := claim(task, stage)
		if err != nil {
			t.Fatalf("%s claim refused while a proposal is pending: %v", stage, err)
		}
		if order.State != core.WorkOrderClaimed {
			t.Fatalf("%s claim state=%s", stage, order.State)
		}
	}
	requirementVersion := func(statement string) core.RequirementVersion {
		return core.RequirementVersion{Content: "# Claims proceed\n\n```conveyor:requirements\n- id: REQ-1\n  statement: " + statement + "\n```", Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: statement}}}
	}
	newRequirement := func(t *testing.T) string {
		t.Helper()
		version := requirementVersion("Claims proceed past pending proposals.")
		version.Origin = core.RequirementOriginOperator
		requirement, initial, err := st.CreateRequirement(ctx, core.Requirement{ID: "req-claim-" + core.NewTaskID(), Title: "Claims proceed " + core.NewTaskID()}, version)
		requireOK(t, err)
		_, _, err = st.ConfirmRequirementVersion(ctx, requirement.ID, initial.Version)
		requireOK(t, err)
		return requirement.ID
	}
	newDesign := func(t *testing.T) string {
		t.Helper()
		design, initial, err := st.CreateSystemDesign(ctx, core.SystemDesign{ID: "design-claim-" + core.NewTaskID(), Title: "Claims proceed " + core.NewTaskID(), Category: "Component design"}, core.SystemDesignVersion{Content: designContent("initial"), Origin: core.SystemDesignOriginOperator})
		requireOK(t, err)
		_, _, err = st.ConfirmSystemDesignVersion(ctx, design.ID, initial.Version)
		requireOK(t, err)
		return design.ID
	}
	proposeRequirement := func(t *testing.T, id, taskID string) int {
		t.Helper()
		version := requirementVersion("A task-authored revision recorded before DEC-102.")
		version.RequirementID, version.Origin, version.OriginTaskID = id, core.RequirementOriginImplementation, taskID
		proposed, err := st.ProposeRequirementVersion(ctx, version)
		requireOK(t, err)
		return proposed.Version
	}
	proposeDesign := func(t *testing.T, id, taskID string) int {
		t.Helper()
		proposed, err := st.ProposeSystemDesignVersion(ctx, core.SystemDesignVersion{DocumentID: id, Content: designContent("task-authored revision"), Origin: core.SystemDesignOriginImplementation, OriginTaskID: taskID})
		requireOK(t, err)
		return proposed.Version
	}
	requireRequirementPending := func(t *testing.T, id, taskID string, version int) {
		t.Helper()
		got, err := st.GetRequirementVersion(ctx, id, version)
		requireOK(t, err)
		if got.Confirmed || got.Retired || got.Origin != core.RequirementOriginImplementation || got.OriginTaskID != taskID {
			t.Fatalf("requirement version after claim=%+v, want pending with its task origin", got)
		}
	}
	requireDesignPending := func(t *testing.T, id, taskID string, version int) {
		t.Helper()
		got, err := st.GetSystemDesignVersion(ctx, id, version)
		requireOK(t, err)
		if got.Confirmed || got.Dismissed || got.Origin != core.SystemDesignOriginImplementation || got.OriginTaskID != taskID {
			t.Fatalf("design version after claim=%+v, want pending with its task origin", got)
		}
	}

	for _, stage := range []core.Stage{core.StageVerify, core.StageReview} {
		t.Run(string(stage)+"_claim_proceeds_with_pending_requirement", func(t *testing.T) {
			task, id := newTask(stage), newRequirement(t)
			version := proposeRequirement(t, id, task.ID)
			requireClaimed(t, task, stage)
			requireRequirementPending(t, id, task.ID, version)
			_, _, err := st.ConfirmRequirementVersion(ctx, id, version)
			requireOK(t, err)
		})
		t.Run(string(stage)+"_claim_proceeds_with_pending_system_design", func(t *testing.T) {
			task, id := newTask(stage), newDesign(t)
			version := proposeDesign(t, id, task.ID)
			requireClaimed(t, task, stage)
			requireDesignPending(t, id, task.ID, version)
			_, _, err := st.DismissSystemDesignVersion(ctx, id, version)
			requireOK(t, err)
		})
		t.Run(string(stage)+"_claim_proceeds_with_pending_version_of_archived_document", func(t *testing.T) {
			task, id := newTask(stage), newDesign(t)
			version := proposeDesign(t, id, task.ID)
			requireOK(t, st.ArchiveSystemDesign(ctx, id, "operator", nil))
			requireClaimed(t, task, stage)
			requireDesignPending(t, id, task.ID, version)
			requireOK(t, st.RestoreSystemDesign(ctx, id, "operator"))
		})
		t.Run(string(stage)+"_claim_proceeds_with_every_other_origin_pending", func(t *testing.T) {
			task, other := newTask(stage), newTask(stage)
			requirement, design := newRequirement(t), newDesign(t)
			_, err := st.ProposeDecision(ctx, core.Decision{Statement: "Decide later.", Context: "Fixture.", AlternativesRejected: "None.", Origin: core.DecisionOriginImplementation, OriginTaskID: task.ID})
			requireOK(t, err)
			operator := requirementVersion("An operator alternative.")
			operator.RequirementID, operator.Origin = requirement, core.RequirementOriginOperator
			_, err = st.ProposeRequirementVersion(ctx, operator)
			requireOK(t, err)
			session := requirementVersion("A planning-session alternative.")
			session.RequirementID, session.Origin, session.OriginSessionID = requirement, core.RequirementOriginChat, "session-"+core.NewTaskID()
			_, err = st.ProposeRequirementVersion(ctx, session)
			requireOK(t, err)
			_, err = st.ProposeSystemDesignVersion(ctx, core.SystemDesignVersion{DocumentID: design, Content: designContent("operator alternative"), Origin: core.SystemDesignOriginOperator})
			requireOK(t, err)
			otherVersion := proposeRequirement(t, requirement, other.ID)
			requireClaimed(t, task, stage)
			requireClaimed(t, other, stage)
			requireRequirementPending(t, requirement, other.ID, otherVersion)
		})
		t.Run(string(stage)+"_claim_still_refuses_an_already_claimed_order", func(t *testing.T) {
			task, id := newTask(stage), newDesign(t)
			proposeDesign(t, id, task.ID)
			requireClaimed(t, task, stage)
			if _, err := claim(task, stage); err == nil || !strings.Contains(err.Error(), "already claimed") {
				t.Fatalf("second %s claim err=%v, want already claimed", stage, err)
			}
		})
	}
	t.Run("verify_claim_still_refuses_an_unmerged_dependency", func(t *testing.T) {
		dependency := newTask(core.StageReview)
		task, id := newTask(core.StageVerify, dependency.ID), newDesign(t)
		proposeDesign(t, id, task.ID)
		if _, err := claim(task, core.StageVerify); err == nil || !strings.Contains(err.Error(), "unmerged") {
			t.Fatalf("blocked verify claim err=%v, want the dependency refusal", err)
		}
	})
	t.Run("pending_proposals_stay_listed_after_a_claim", func(t *testing.T) {
		task, id := newTask(core.StageReview), newDesign(t)
		version := proposeDesign(t, id, task.ID)
		requireClaimed(t, task, core.StageReview)
		pending, err := st.ListPendingProposals(ctx)
		requireOK(t, err)
		for _, proposal := range pending {
			if proposal.Tier == "system_design" && proposal.ID == id && proposal.Version == version && proposal.OriginType == "task" && proposal.OriginID == task.ID {
				return
			}
		}
		t.Fatalf("pending list lost %s v%d: %+v", id, version, pending)
	})
}
