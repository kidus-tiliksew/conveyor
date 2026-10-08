package storetest

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// runClaimBlockingProposals pins the claim-blocking read against both stage
// claims on every backend: the read returns exactly the versions the locked
// verify and review claims refuse on, for both proposal tiers, raw origins,
// archived documents, resolution, other tasks, and other workspaces
// (req-260810-70ce2f REQ-1; component-work-orders).
func runClaimBlockingProposals(t *testing.T, x Fixture) {
	st, ctx := x.Backend, x.Context
	now := time.Now().UTC().Truncate(time.Microsecond)
	reviewTask := func() core.Task {
		t.Helper()
		id := core.NewTaskID()
		task := core.Task{ID: id, Workspace: x.Workspace, Repo: "conveyor", Title: id, BaseBranch: "main", Branch: "conveyor/task-" + id, State: core.TaskRunning, NextStage: core.StageReview, CreatedAt: now}
		requireOK(t, st.CreateTask(ctx, task))
		job := core.Job{ID: id + "-review-1", TaskID: id, Stage: core.StageReview, State: core.JobPending}
		requireOK(t, st.CreateJob(ctx, job))
		requireOK(t, For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: id, JobID: job.ID, Stage: core.StageReview, State: core.WorkOrderQueued}))
		return task
	}
	verifyTask := func() core.Task {
		t.Helper()
		id := core.NewTaskID()
		task := core.Task{ID: id, Workspace: x.Workspace, Repo: "conveyor", Title: id, BaseBranch: "main", Branch: "conveyor/task-" + id, State: core.TaskRunning, NextStage: core.StageVerify, ReviewedHeadSHA: "submitted-head", CreatedAt: now}
		task.SetupContract.VerifyStage = true
		requireOK(t, st.CreateTask(ctx, task))
		job := core.Job{ID: id + "-verify-1", TaskID: id, Stage: core.StageVerify, State: core.JobPending}
		_, err := CreateStageWorkOrder(ctx, st, job, core.WorkOrder{ID: job.ID, JobID: job.ID, TaskID: id, Stage: core.StageVerify, State: core.WorkOrderQueued, HeadSHA: task.ReviewedHeadSHA, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now})
		requireOK(t, err)
		return task
	}
	blocking := func(taskID string) []store.ClaimBlockingProposal {
		t.Helper()
		items, err := st.ListClaimBlockingProposalsForTask(ctx, taskID)
		requireOK(t, err)
		return items
	}
	requireBlocking := func(taskID string, want ...store.ClaimBlockingProposal) {
		t.Helper()
		got := blocking(taskID)
		if len(want) == 0 {
			want = []store.ClaimBlockingProposal{}
		}
		if len(got) == 0 {
			got = []store.ClaimBlockingProposal{}
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("claim-blocking proposals for %s=%+v want %+v", taskID, got, want)
		}
	}
	claim := func(task core.Task, stage core.Stage, session string) error {
		t.Helper()
		_, err := For(st).ClaimWorkOrder(ctx, task.ID+"-"+string(stage)+"-1", core.WorkOrderClaim{SessionID: session, ClientToken: session + "-token", Lease: time.Minute, ExecutionTimeout: time.Hour})
		return err
	}
	requireRefusedOn := func(task core.Task, stage core.Stage, first store.ClaimBlockingProposal) {
		t.Helper()
		err := claim(task, stage, "blocked-"+core.NewTaskID()[:6])
		if err == nil || !strings.Contains(err.Error(), store.ClaimBlockingProposalError(task.ID, first).Error()) {
			t.Fatalf("%s claim err=%v, want refusal naming %+v", stage, err, first)
		}
	}
	requirementContent := func(statement string) core.RequirementVersion {
		return core.RequirementVersion{Content: "# Claim gate\n\n```conveyor:requirements\n- id: REQ-1\n  statement: " + statement + "\n```", Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: statement}}}
	}
	proposeRequirement := func(id string, origin core.RequirementOrigin, taskID, statement string) core.RequirementVersion {
		t.Helper()
		version := requirementContent(statement)
		version.RequirementID, version.Origin, version.OriginTaskID = id, origin, taskID
		if origin == core.RequirementOriginChat {
			version.OriginTaskID, version.OriginSessionID = "", "session-"+core.NewTaskID()
		}
		proposed, err := st.ProposeRequirementVersion(ctx, version)
		requireOK(t, err)
		return proposed
	}
	proposeDesign := func(id string, origin core.SystemDesignOrigin, taskID, label string) core.SystemDesignVersion {
		t.Helper()
		proposed, err := st.ProposeSystemDesignVersion(ctx, core.SystemDesignVersion{DocumentID: id, Content: designContent(label), Origin: origin, OriginTaskID: taskID})
		requireOK(t, err)
		return proposed
	}

	reviewed, other, verified := reviewTask(), reviewTask(), verifyTask()
	requirement, initial, err := st.CreateRequirement(ctx, core.Requirement{ID: "req-claim-" + core.NewTaskID(), Title: "Claim gate"}, func() core.RequirementVersion {
		version := requirementContent("Claims wait for task-authored proposals.")
		version.Origin = core.RequirementOriginOperator
		return version
	}())
	requireOK(t, err)
	_, _, err = st.ConfirmRequirementVersion(ctx, requirement.ID, initial.Version)
	requireOK(t, err)
	design, designInitial, err := st.CreateSystemDesign(ctx, core.SystemDesign{ID: "design-claim-" + core.NewTaskID()[:8], Title: "Claim gate", Category: "Component design"}, core.SystemDesignVersion{Content: designContent("initial"), Origin: core.SystemDesignOriginOperator})
	requireOK(t, err)
	_, _, err = st.ConfirmSystemDesignVersion(ctx, design.ID, designInitial.Version)
	requireOK(t, err)
	requireBlocking(reviewed.ID)

	// Nonblocking proposals: a task-authored decision, operator and session
	// origins, and another task's implementation versions.
	_, err = st.ProposeDecision(ctx, core.Decision{Statement: "Decide later.", Context: "Fixture.", AlternativesRejected: "None.", Origin: core.DecisionOriginImplementation, OriginTaskID: reviewed.ID})
	requireOK(t, err)
	proposeRequirement(requirement.ID, core.RequirementOriginOperator, "", "An operator alternative.")
	proposeRequirement(requirement.ID, core.RequirementOriginChat, "", "A planning-session alternative.")
	otherRequirement := proposeRequirement(requirement.ID, core.RequirementOriginImplementation, other.ID, "Another task's alternative.")
	proposeDesign(design.ID, core.SystemDesignOriginOperator, "", "operator alternative")
	requireBlocking(reviewed.ID)
	requireOK(t, claim(reviewed, core.StageReview, "nonblocking"))
	requireBlocking(other.ID, store.ClaimBlockingProposal{Tier: store.ClaimBlockingTierRequirement, ID: requirement.ID, Version: otherRequirement.Version})
	requireRefusedOn(other, core.StageReview, store.ClaimBlockingProposal{Tier: store.ClaimBlockingTierRequirement, ID: requirement.ID, Version: otherRequirement.Version})

	// Both tiers block the verify claim; the System Design tier is named first.
	verifiedRequirement := proposeRequirement(requirement.ID, core.RequirementOriginImplementation, verified.ID, "The verified task's alternative.")
	verifiedDesign := proposeDesign(design.ID, core.SystemDesignOriginImplementation, verified.ID, "verified task revision")
	wantDesign := store.ClaimBlockingProposal{Tier: store.ClaimBlockingTierSystemDesign, ID: design.ID, Version: verifiedDesign.Version}
	wantRequirement := store.ClaimBlockingProposal{Tier: store.ClaimBlockingTierRequirement, ID: requirement.ID, Version: verifiedRequirement.Version}
	requireBlocking(verified.ID, wantDesign, wantRequirement)
	requireRefusedOn(verified, core.StageVerify, wantDesign)

	// Another workspace reads none of this workspace's proposals.
	foreign := x.Workspace + "-claims-" + core.NewTaskID()[:6]
	foreignCtx := store.WithWorkspace(x.Context, foreign)
	_, err = st.BootstrapWorkspaceConfig(foreignCtx, &config.Config{Workspace: foreign, Repos: x.Config.Repos})
	requireOK(t, err)
	foreignItems, err := st.ListClaimBlockingProposalsForTask(foreignCtx, verified.ID)
	requireOK(t, err)
	if len(foreignItems) != 0 {
		t.Fatalf("another workspace read claim-blocking proposals: %+v", foreignItems)
	}

	// A pending version of an archived document still blocks.
	requireOK(t, st.ArchiveSystemDesign(ctx, design.ID, "operator", nil))
	requireBlocking(verified.ID, wantDesign, wantRequirement)
	requireRefusedOn(verified, core.StageVerify, wantDesign)
	requireOK(t, st.RestoreSystemDesign(ctx, design.ID, "operator"))

	// Deciding each version releases the claim: dismissal of the design,
	// then confirmation of the requirement.
	_, _, err = st.DismissSystemDesignVersion(ctx, design.ID, verifiedDesign.Version)
	requireOK(t, err)
	requireBlocking(verified.ID, wantRequirement)
	requireRefusedOn(verified, core.StageVerify, wantRequirement)
	_, _, err = st.ConfirmRequirementVersion(ctx, requirement.ID, verifiedRequirement.Version)
	requireOK(t, err)
	requireBlocking(verified.ID)
	if err := claim(verified, core.StageVerify, "released"); err != nil && strings.Contains(err.Error(), "waiting on") {
		t.Fatalf("verify claim still waits after both decisions: %v", err)
	}
	// Confirming the newer requirement version retired the other task's
	// pending version, which no longer blocks.
	requireBlocking(other.ID)
}
