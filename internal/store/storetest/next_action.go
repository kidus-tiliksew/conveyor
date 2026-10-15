package storetest

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/nextaction"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// RunNextActionConformance pins the factory log and the shared attention count.
func RunNextActionConformance(t *testing.T, st store.Store, ctx context.Context) {
	t.Helper()
	workspace, _ := store.WorkspaceFromContext(ctx)
	now := time.Now().UTC().Truncate(time.Second)
	task := core.Task{ID: "factory-acts-" + core.NewTaskID(), Workspace: workspace, Repo: "conveyor", Branch: "factory-" + core.NewTaskID(), State: core.TaskRunning, CreatedAt: now}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	system := core.Event{TaskID: task.ID, Kind: "pipeline.bounced", ActorID: "dispatcher", ActorRole: core.ActorSystem, At: now, Payload: core.JSONPayload(map[string]any{"source": "mcp-review"})}
	agent := core.Event{TaskID: task.ID, Kind: "pipeline.bounced", ActorID: "agent", ActorRole: core.ActorAgent, At: now.Add(time.Second), Payload: core.JSONPayload(map[string]any{"source": "request-changes", "reason_code": "user_changes_requested"})}
	old := core.Event{TaskID: task.ID, Kind: "merge.conflict_fix_dispatched", ActorID: "dispatcher", ActorRole: core.ActorSystem, At: now.Add(-25 * time.Hour)}
	for _, event := range []core.Event{old, system, agent} {
		if err := st.AppendEvent(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	acts, err := st.ListFactoryActs(ctx, now.Add(-24*time.Hour), 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(acts) != 1 || acts[0].Kind != "pipeline.bounced" || !acts[0].Automatic || acts[0].TaskID != task.ID {
		t.Fatalf("factory acts=%+v", acts)
	}
	bounded, err := st.ListFactoryActs(ctx, now.Add(-24*time.Hour), 1)
	if err != nil || len(bounded) != 1 {
		t.Fatalf("limit acts=%+v err=%v", bounded, err)
	}
	isolated := workspace + "-attention-" + core.NewTaskID()
	iso := store.WithWorkspace(ctx, isolated)
	bootstrap, ok := st.(interface {
		BootstrapWorkspaceConfig(context.Context, *config.Config) (bool, error)
	})
	if !ok {
		t.Fatal("store cannot bootstrap an isolated workspace")
	}
	if _, err = bootstrap.BootstrapWorkspaceConfig(iso, &config.Config{Workspace: isolated, Repos: []config.Repo{{Name: "conveyor", Base: "main"}}}); err != nil {
		t.Fatal(err)
	}
	awaiting := core.Task{ID: "attention-" + core.NewTaskID(), Workspace: isolated, Repo: "conveyor", Branch: "attention-" + core.NewTaskID(), State: core.TaskAwaiting, GateKind: core.GateKindPlanApproval, CreatedAt: now}
	running := core.Task{ID: "running-" + core.NewTaskID(), Workspace: isolated, Repo: "conveyor", Branch: "running-" + core.NewTaskID(), State: core.TaskRunning, CreatedAt: now}
	if err = st.CreateTask(iso, awaiting); err != nil {
		t.Fatal(err)
	}
	if err = st.CreateTask(iso, running); err != nil {
		t.Fatal(err)
	}
	listed, err := st.ListTasksFiltered(iso, store.TaskFilter{})
	if err != nil {
		t.Fatal(err)
	}
	candidates := make([]core.Task, 0, len(listed))
	for _, task := range listed {
		if task.Workspace == isolated {
			candidates = append(candidates, task)
		}
	}
	projected, err := store.ReadNextActions(iso, st, candidates, store.NextActionOptions{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	want := 0
	for _, action := range projected {
		if store.TaskNeedsAttention(action) {
			want++
		}
	}
	count, err := st.CountOperatorAttention(iso)
	if err != nil || count != want {
		t.Fatalf("attention count=%d projected=%d err=%v", count, want, err)
	}
	page, err := store.PageOperatorAttention(iso, st, candidates, 0, 10)
	if err != nil || page.Total != want {
		t.Fatalf("page total=%d want=%d err=%v", page.Total, want, err)
	}
	runNextActionReadFixtures(t, st, ctx, workspace)
	runAgentQuestionCheckpoint(t, st, ctx, workspace)
	runClaimWaitRow8(t, st, ctx, workspace)
	runContextProposalRow(t, st, ctx, workspace)
}

// runAgentQuestionCheckpoint assembles an authority_conflict agent question
// from the stores: cited pending versions of any origin, deduplicated and
// ordered, with uncited proposals excluded, and free-text recovery admitted
// without a proposal decision (component-work-orders, Next action;
// req-operator-next-action AC-1.6; req-260820-394cac AC-2.3, AC-2.5, AC-3.2).
func runAgentQuestionCheckpoint(t *testing.T, st store.Store, ctx context.Context, workspace string) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	suffix := core.NewTaskID()
	design := func(id string) string {
		t.Helper()
		created, initial, err := st.CreateSystemDesign(ctx, core.SystemDesign{ID: id, Title: "Agent question " + id, Category: "Component design"}, core.SystemDesignVersion{Content: designContent("initial"), Origin: core.SystemDesignOriginOperator})
		requireOK(t, err)
		_, _, err = st.ConfirmSystemDesignVersion(ctx, created.ID, initial.Version)
		requireOK(t, err)
		return created.ID
	}
	propose := func(id string, origin core.SystemDesignOrigin, taskID, label string) int {
		t.Helper()
		proposed, err := st.ProposeSystemDesignVersion(ctx, core.SystemDesignVersion{DocumentID: id, Content: designContent(label), Origin: origin, OriginTaskID: taskID})
		requireOK(t, err)
		return proposed.Version
	}
	release := func(citations ...core.WorkOrderAuthorityConflictCitation) (string, string) {
		t.Helper()
		taskID := createRunningTask(t, st, ctx, workspace)
		orderID := taskID + "-implement-1"
		requireOK(t, st.CreateJob(ctx, core.Job{ID: orderID, TaskID: taskID, Stage: core.StageImplement, State: core.JobPending}))
		requireOK(t, CreateWorkOrder(ctx, st, core.WorkOrder{ID: orderID, TaskID: taskID, JobID: orderID, Stage: core.StageImplement, State: core.WorkOrderQueued, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour)}))
		worker, session := "worker-question-"+taskID, "session-question-"+taskID
		_, err := ClaimWorkOrder(ctx, st, orderID, core.WorkOrderClaim{WorkerID: worker, ClaimantID: worker, SessionID: session, ClientToken: "secret", Lease: time.Minute, ExecutionTimeout: time.Hour})
		requireOK(t, err)
		_, err = ReleaseWorkerClaim(ctx, st, orderID, worker, core.WorkOrderRelease{
			SessionID: session, Reason: core.WorkOrderReleaseReasonOperatorCheckpointReached, Cause: core.WorkOrderReleaseCauseOperatorAction, Outcome: core.WorkOrderOutcomeReleased,
			Checkpoint: &core.WorkOrderCheckpoint{DecisionRequest: "Confirm the cited revisions.", Class: core.WorkOrderCheckpointClassAuthorityConflict, Citations: citations},
		})
		requireOK(t, err)
		return taskID, orderID
	}
	read := func(taskID string, caps map[core.Capability]bool) nextaction.NextAction {
		t.Helper()
		action, err := store.ReadNextAction(ctx, st, taskID, store.NextActionOptions{Now: now, Capabilities: caps})
		requireOK(t, err)
		if action.Kind != nextaction.KindAgentQuestion || action.Actor != nextaction.ActorOperator {
			t.Fatalf("task %s next action = %s/%s, want operator agent_question", taskID, action.Actor, action.Kind)
		}
		return action
	}

	cited := design("aq-cited-" + suffix[:8])
	uncited := design("aq-uncited-" + suffix[:8])
	other := createRunningTask(t, st, ctx, workspace)
	otherVersion := propose(cited, core.SystemDesignOriginImplementation, other, "another task's revision")
	operatorVersion := propose(cited, core.SystemDesignOriginOperator, "", "operator revision")
	propose(uncited, core.SystemDesignOriginOperator, "", "uncited revision")
	taskID, orderID := release(
		core.WorkOrderAuthorityConflictCitation{DocumentID: cited, CitedVersion: 1, StatementOrSectionID: "Section A"},
		core.WorkOrderAuthorityConflictCitation{DocumentID: cited, CitedVersion: 1, StatementOrSectionID: "Section B"},
	)
	action := read(taskID, map[core.Capability]bool{core.CapabilityConfirmDocuments: true})
	want := []nextaction.ProposalRef{{Tier: "system_design", ID: cited, Version: otherVersion}, {Tier: "system_design", ID: cited, Version: operatorVersion}}
	if !reflect.DeepEqual(action.Details.PendingProposals, want) {
		t.Fatalf("pending_proposals = %+v, want %+v", action.Details.PendingProposals, want)
	}
	if action.Label != "Cited revisions need a decision" || action.Details.Checkpoint != "Confirm the cited revisions." || action.Details.CheckpointClass != core.WorkOrderCheckpointClassAuthorityConflict || len(action.Details.Citations) != 2 {
		t.Fatalf("agent question = label %q details %+v", action.Label, action.Details)
	}
	if len(action.Choices) != 5 || action.Choices[4].ID != "answer" || action.Choices[4].Allowed || !action.Choices[0].Allowed {
		t.Fatalf("choices = %+v, want four allowed proposal choices before a disallowed answer", action.Choices)
	}
	if _, err := RecoverWorkOrderWithDirection(ctx, st, orderID, "recover-question-"+taskID, "Proceed without deciding the proposals.", time.Hour); err != nil {
		t.Fatalf("free-text recovery without a proposal decision: %v", err)
	}

	bare, _ := release(core.WorkOrderAuthorityConflictCitation{DocumentID: design("aq-bare-" + suffix[:8]), CitedVersion: 1})
	authorship := read(bare, map[core.Capability]bool{core.CapabilityRecoverWork: true})
	if authorship.Label != "Cited documents need a revision" || len(authorship.Details.PendingProposals) != 0 || len(authorship.Choices) != 1 || authorship.Choices[0].ID != "answer" || !authorship.Choices[0].Allowed {
		t.Fatalf("authorship agent question = label %q proposals %+v choices %+v", authorship.Label, authorship.Details.PendingProposals, authorship.Choices)
	}
}

// runClaimWaitRow8 pins next-action row 8 to the claim gate: row 8 matches
// exactly when store.ClaimWaitingProposals selects a version for the task,
// which happens only for an undecided task-authored requirement or System
// Design version while a verify or review claim is pending. Each selected
// version yields confirm and dismiss on its version route. A task-context or
// decision proposal never matches, and the 261015-676ea5 shape resolves to
// its agent question (req-operator-next-action AC-1.4, AC-1.7;
// req-260810-70ce2f AC-1.1, AC-1.2, AC-1.3; component-work-orders, Next
// action).
func runClaimWaitRow8(t *testing.T, st store.Store, ctx context.Context, workspace string) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	suffix := core.NewTaskID()[:8]
	confirmDocs := map[core.Capability]bool{core.CapabilityConfirmDocuments: true}
	design := func(id string) string {
		t.Helper()
		created, initial, err := st.CreateSystemDesign(ctx, core.SystemDesign{ID: id, Title: "Claim wait " + id, Category: "Component design"}, core.SystemDesignVersion{Content: designContent("initial"), Origin: core.SystemDesignOriginOperator})
		requireOK(t, err)
		_, _, err = st.ConfirmSystemDesignVersion(ctx, created.ID, initial.Version)
		requireOK(t, err)
		return created.ID
	}
	proposeDesign := func(id string, origin core.SystemDesignOrigin, taskID, label string) int {
		t.Helper()
		proposed, err := st.ProposeSystemDesignVersion(ctx, core.SystemDesignVersion{DocumentID: id, Content: designContent(label), Origin: origin, OriginTaskID: taskID})
		requireOK(t, err)
		return proposed.Version
	}
	requirementVersion := func(statement string) core.RequirementVersion {
		return core.RequirementVersion{Content: "# Claim wait\n\n```conveyor:requirements\n- id: REQ-1\n  statement: " + statement + "\n```", Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: statement}}}
	}
	requirement, initial, err := st.CreateRequirement(ctx, core.Requirement{ID: "req-claim-wait-" + suffix, Title: "Claim wait"}, func() core.RequirementVersion {
		version := requirementVersion("Row 8 follows the claim gate.")
		version.Origin = core.RequirementOriginOperator
		return version
	}())
	requireOK(t, err)
	_, _, err = st.ConfirmRequirementVersion(ctx, requirement.ID, initial.Version)
	requireOK(t, err)
	proposeRequirement := func(taskID, statement string) int {
		t.Helper()
		version := requirementVersion(statement)
		version.RequirementID, version.Origin, version.OriginTaskID = requirement.ID, core.RequirementOriginImplementation, taskID
		proposed, err := st.ProposeRequirementVersion(ctx, version)
		requireOK(t, err)
		return proposed.Version
	}
	proposeContext := func(taskID, target string) {
		t.Helper()
		_, _, err := st.ProposeTaskContext(ctx, core.TaskContextProposalInput{TaskID: taskID, TargetKind: core.TaskContextProposalSystemDesign, TargetID: target, Source: core.TaskContextProposalTriage, Justification: "Triage suggests this design."})
		requireOK(t, err)
	}
	// Window fixtures: a queued review or verify order puts the task inside
	// its claim window; a queued implement order or no order leaves it out.
	stageTask := func(stage core.Stage) string {
		t.Helper()
		if stage == "" {
			return createRunningTask(t, st, ctx, workspace)
		}
		id := core.NewTaskID()
		task := core.Task{ID: id, Workspace: workspace, Repo: "conveyor", Title: id, BaseBranch: "main", Branch: "conveyor/task-" + id, State: core.TaskRunning, NextStage: stage, CreatedAt: now}
		if stage == core.StageVerify {
			task.ReviewedHeadSHA = "submitted-head"
			task.SetupContract.VerifyStage = true
		}
		requireOK(t, st.CreateTask(ctx, task))
		job := core.Job{ID: id + "-" + string(stage) + "-1", TaskID: id, Stage: stage, State: core.JobPending}
		if stage == core.StageReview {
			requireOK(t, st.CreateJob(ctx, job))
			requireOK(t, For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: id, JobID: job.ID, Stage: stage, State: core.WorkOrderQueued}))
			return id
		}
		_, err := CreateStageWorkOrder(ctx, st, job, core.WorkOrder{ID: job.ID, JobID: job.ID, TaskID: id, Stage: stage, State: core.WorkOrderQueued, HeadSHA: task.ReviewedHeadSHA, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour), CreatedAt: now})
		requireOK(t, err)
		return id
	}
	read := func(taskID string, caps map[core.Capability]bool) nextaction.NextAction {
		t.Helper()
		action, err := store.ReadNextAction(ctx, st, taskID, store.NextActionOptions{Now: now, Capabilities: caps})
		requireOK(t, err)
		return action
	}
	// requireRow8 asserts the shared parity: row 8 matches exactly when the
	// claim-wait selection names a version for the task, and then lists those
	// versions in order.
	requireRow8 := func(taskID string, want ...nextaction.ProposalRef) nextaction.NextAction {
		t.Helper()
		orders, err := st.ListWorkOrdersForTasks(ctx, []string{taskID})
		requireOK(t, err)
		selected, err := store.ClaimWaitingProposals(ctx, st, orders)
		requireOK(t, err)
		action := read(taskID, confirmDocs)
		if (action.Kind == nextaction.KindDocumentProposal) != (len(selected[taskID]) > 0) {
			t.Fatalf("task %s kind=%s selection=%+v: row 8 and the claim-wait selection disagree", taskID, action.Kind, selected[taskID])
		}
		if len(want) == 0 {
			if action.Kind == nextaction.KindDocumentProposal {
				t.Fatalf("task %s matched row 8 with %+v", taskID, action.Details.PendingProposals)
			}
			for _, secondary := range action.Details.Secondary {
				if secondary.Kind == nextaction.KindDocumentProposal {
					t.Fatalf("task %s records document_proposal as secondary: %+v", taskID, action.Details.Secondary)
				}
			}
			return action
		}
		if action.Kind != nextaction.KindDocumentProposal || action.Actor != nextaction.ActorOperator || !action.Judgment {
			t.Fatalf("task %s next action=%s/%s, want operator document_proposal", taskID, action.Actor, action.Kind)
		}
		if !reflect.DeepEqual(action.Details.PendingProposals, want) {
			t.Fatalf("task %s pending_proposals=%+v want %+v", taskID, action.Details.PendingProposals, want)
		}
		return action
	}

	// A task-context proposal alone never matches row 8, in any stage.
	contextTarget := design("cw-context-" + suffix)
	for _, stage := range []core.Stage{"", core.StageImplement, core.StageVerify, core.StageReview} {
		var id string
		if stage == core.StageImplement {
			id = orderTask(t, st, ctx, workspace, core.WorkOrder{QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour)})
		} else {
			id = stageTask(stage)
		}
		proposeContext(id, contextTarget)
		if action := requireRow8(id); action.Kind != nextaction.KindContextProposal {
			t.Fatalf("stage %q task %s with a context proposal = %s, want context_proposal", stage, id, action.Kind)
		}
	}

	// A decision proposal alone never matches row 8, inside the window.
	decisionTask := stageTask(core.StageReview)
	_, err = st.ProposeDecision(ctx, core.Decision{Statement: "Decide later.", Context: "Fixture.", AlternativesRejected: "None.", Origin: core.DecisionOriginImplementation, OriginTaskID: decisionTask})
	requireOK(t, err)
	requireRow8(decisionTask)

	// Operator-origin and other-task versions never match row 8.
	shared := design("cw-shared-" + suffix)
	reviewed, other := stageTask(core.StageReview), stageTask(core.StageReview)
	proposeDesign(shared, core.SystemDesignOriginOperator, "", "operator alternative")
	otherVersion := proposeDesign(shared, core.SystemDesignOriginImplementation, other, "another task's revision")
	requireRow8(reviewed)
	requireRow8(other, nextaction.ProposalRef{Tier: "system_design", ID: shared, Version: otherVersion})

	// A withholding version outside the window does not match row 8.
	early := createRunningTask(t, st, ctx, workspace)
	earlyDesign := design("cw-early-" + suffix)
	proposeDesign(earlyDesign, core.SystemDesignOriginImplementation, early, "too early")
	requireRow8(early)

	// Inside a verify window, both tiers match, System Design versions first,
	// each with confirm and dismiss on its version route.
	verified := stageTask(core.StageVerify)
	proposeContext(verified, contextTarget)
	withheld := design("cw-withheld-" + suffix)
	designVersion := proposeDesign(withheld, core.SystemDesignOriginImplementation, verified, "withheld revision")
	requirementVersionNumber := proposeRequirement(verified, "The verified task's alternative.")
	wantDesign := nextaction.ProposalRef{Tier: "system_design", ID: withheld, Version: designVersion}
	wantRequirement := nextaction.ProposalRef{Tier: "requirement", ID: requirement.ID, Version: requirementVersionNumber}
	action := requireRow8(verified, wantDesign, wantRequirement)
	designRoute := fmt.Sprintf("/v1/system-designs/%s/versions/%d", withheld, designVersion)
	requirementRoute := fmt.Sprintf("/v1/requirements/%s/versions/%d", requirement.ID, requirementVersionNumber)
	wantChoices := []nextaction.Choice{
		{ID: fmt.Sprintf("confirm:system_design:%s:%d", withheld, designVersion), Label: fmt.Sprintf("Confirm %s v%d", withheld, designVersion), Effect: "Confirms this pending revision.", Method: "POST", Path: designRoute + "/confirm", Capability: string(core.CapabilityConfirmDocuments), Allowed: true},
		{ID: fmt.Sprintf("dismiss:system_design:%s:%d", withheld, designVersion), Label: fmt.Sprintf("Dismiss %s v%d", withheld, designVersion), Effect: "Dismisses this pending revision.", Method: "POST", Path: designRoute + "/dismiss", Capability: string(core.CapabilityConfirmDocuments), Allowed: true},
		{ID: fmt.Sprintf("confirm:requirement:%s:%d", requirement.ID, requirementVersionNumber), Label: fmt.Sprintf("Confirm %s v%d", requirement.ID, requirementVersionNumber), Effect: "Confirms this pending revision.", Method: "POST", Path: requirementRoute + "/confirm", Capability: string(core.CapabilityConfirmDocuments), Allowed: true},
		{ID: fmt.Sprintf("dismiss:requirement:%s:%d", requirement.ID, requirementVersionNumber), Label: fmt.Sprintf("Dismiss %s v%d", requirement.ID, requirementVersionNumber), Effect: "Dismisses this pending revision.", Method: "POST", Path: requirementRoute + "/dismiss", Capability: string(core.CapabilityConfirmDocuments), Allowed: true},
	}
	if !reflect.DeepEqual(action.Choices, wantChoices) {
		t.Fatalf("row 8 choices=%+v want %+v", action.Choices, wantChoices)
	}
	for _, choice := range read(verified, nil).Choices {
		if choice.Allowed {
			t.Fatalf("choice %s allowed without confirm_documents", choice.ID)
		}
	}

	// A pending version of an archived document still withholds the claim,
	// so row 8 still lists it.
	requireOK(t, st.ArchiveSystemDesign(ctx, withheld, "operator", nil))
	requireRow8(verified, wantDesign, wantRequirement)
	requireOK(t, st.RestoreSystemDesign(ctx, withheld, "operator"))

	// Deciding each version narrows the wait; deciding the last one ends it.
	_, _, err = st.DismissSystemDesignVersion(ctx, withheld, designVersion)
	requireOK(t, err)
	requireRow8(verified, wantRequirement)
	_, _, err = st.ConfirmRequirementVersion(ctx, requirement.ID, requirementVersionNumber)
	requireOK(t, err)
	requireRow8(verified)

	// The 261015-676ea5 shape: a triage task-context proposal plus an
	// authority_conflict checkpoint that cites two System Design documents
	// with pending task-authored versions, while the task's latest order is a
	// released implement order outside the claim window.
	conflictTask := createRunningTask(t, st, ctx, workspace)
	proposeContext(conflictTask, contextTarget)
	first, second := design("cw-cited-a-"+suffix), design("cw-cited-b-"+suffix)
	firstVersion := proposeDesign(first, core.SystemDesignOriginImplementation, conflictTask, "first cited revision")
	secondVersion := proposeDesign(second, core.SystemDesignOriginImplementation, conflictTask, "second cited revision")
	orderID := conflictTask + "-implement-1"
	requireOK(t, st.CreateJob(ctx, core.Job{ID: orderID, TaskID: conflictTask, Stage: core.StageImplement, State: core.JobPending}))
	requireOK(t, CreateWorkOrder(ctx, st, core.WorkOrder{ID: orderID, TaskID: conflictTask, JobID: orderID, Stage: core.StageImplement, State: core.WorkOrderQueued, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour)}))
	worker, session := "worker-claim-wait-"+conflictTask, "session-claim-wait-"+conflictTask
	_, err = ClaimWorkOrder(ctx, st, orderID, core.WorkOrderClaim{WorkerID: worker, ClaimantID: worker, SessionID: session, ClientToken: "secret", Lease: time.Minute, ExecutionTimeout: time.Hour})
	requireOK(t, err)
	_, err = ReleaseWorkerClaim(ctx, st, orderID, worker, core.WorkOrderRelease{
		SessionID: session, Reason: core.WorkOrderReleaseReasonOperatorCheckpointReached, Cause: core.WorkOrderReleaseCauseOperatorAction, Outcome: core.WorkOrderOutcomeReleased,
		Checkpoint: &core.WorkOrderCheckpoint{DecisionRequest: "Confirm or dismiss the two revisions.", Class: core.WorkOrderCheckpointClassAuthorityConflict, Citations: []core.WorkOrderAuthorityConflictCitation{
			{DocumentID: first, CitedVersion: 1, StatementOrSectionID: "Next action"},
			{DocumentID: second, CitedVersion: 1, StatementOrSectionID: "Claim admission"},
		}},
	})
	requireOK(t, err)
	question := requireRow8(conflictTask)
	wantCited := []nextaction.ProposalRef{{Tier: "system_design", ID: first, Version: firstVersion}, {Tier: "system_design", ID: second, Version: secondVersion}}
	contextSecondary := false
	for _, secondary := range question.Details.Secondary {
		contextSecondary = contextSecondary || secondary.Kind == nextaction.KindContextProposal
	}
	if !contextSecondary {
		t.Fatalf("676ea5 shape secondary=%+v, want context_proposal below the agent question", question.Details.Secondary)
	}
	if question.Kind != nextaction.KindAgentQuestion || question.Details.CheckpointClass != core.WorkOrderCheckpointClassAuthorityConflict || len(question.Details.Citations) != 2 || !reflect.DeepEqual(question.Details.PendingProposals, wantCited) {
		t.Fatalf("676ea5 shape next action=%s details=%+v, want agent_question with only the cited versions", question.Kind, question.Details)
	}
	if _, err := RecoverWorkOrderWithDirection(ctx, st, orderID, "recover-claim-wait-"+conflictTask, "Proceed under the current authority.", time.Hour); err != nil {
		t.Fatalf("free-text recovery without a proposal decision: %v", err)
	}
}

// runContextProposalRow pins the context_proposal row on every backend: a
// task's own pending task-context proposals, of both target kinds and in
// target order, keep it in operator attention with confirm and dismiss on the
// task-context routes; another task's and decided proposals never count; and
// deciding the last one removes the row (req-intake-and-triage AC-7.3;
// feature-operator-next-action NA-3; component-work-orders, Next action).
func runContextProposalRow(t *testing.T, st store.Store, ctx context.Context, workspace string) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	suffix := core.NewTaskID()[:8]
	gates := map[core.Capability]bool{core.CapabilityOperateGates: true}
	requirement, initial, err := st.CreateRequirement(ctx, core.Requirement{ID: "req-context-row-" + suffix, Title: "Context row"}, core.RequirementVersion{
		Content: "# Context row\n\n```conveyor:requirements\n- id: REQ-1\n  statement: Context proposals need attention.\n```", Origin: core.RequirementOriginOperator,
		Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Context proposals need attention."}},
	})
	requireOK(t, err)
	_, _, err = st.ConfirmRequirementVersion(ctx, requirement.ID, initial.Version)
	requireOK(t, err)
	design, designInitial, err := st.CreateSystemDesign(ctx, core.SystemDesign{ID: "cw-context-row-" + suffix, Title: "Context row", Category: "Component design"}, core.SystemDesignVersion{Content: designContent("initial"), Origin: core.SystemDesignOriginOperator})
	requireOK(t, err)
	_, _, err = st.ConfirmSystemDesignVersion(ctx, design.ID, designInitial.Version)
	requireOK(t, err)
	propose := func(taskID string, kind core.TaskContextProposalTargetKind, target string) {
		t.Helper()
		_, _, err := st.ProposeTaskContext(ctx, core.TaskContextProposalInput{TaskID: taskID, TargetKind: kind, TargetID: target, Source: core.TaskContextProposalTriage, Justification: "Triage suggests this context."})
		requireOK(t, err)
	}
	read := func(taskID string, caps map[core.Capability]bool) nextaction.NextAction {
		t.Helper()
		action, err := store.ReadNextAction(ctx, st, taskID, store.NextActionOptions{Now: now, Capabilities: caps})
		requireOK(t, err)
		return action
	}
	count := func() int {
		t.Helper()
		total, err := st.CountOperatorAttention(ctx)
		requireOK(t, err)
		return total
	}

	other := createRunningTask(t, st, ctx, workspace)
	propose(other, core.TaskContextProposalRequirement, requirement.ID)
	taskID := createRunningTask(t, st, ctx, workspace)
	if action := read(taskID, gates); action.Kind == nextaction.KindContextProposal {
		t.Fatalf("another task's context proposal selected the row for %s", taskID)
	}
	before := count()
	propose(taskID, core.TaskContextProposalSystemDesign, design.ID)
	propose(taskID, core.TaskContextProposalRequirement, requirement.ID)
	if after := count(); after != before+1 {
		t.Fatalf("attention count %d -> %d, want one more for the context-only task", before, after)
	}
	action := read(taskID, gates)
	if action.Kind != nextaction.KindContextProposal || action.Actor != nextaction.ActorOperator || !action.Judgment || !store.TaskNeedsAttention(action) || len(action.Details.PendingProposals) != 0 {
		t.Fatalf("context-only next action = %s/%s details %+v", action.Actor, action.Kind, action.Details)
	}
	route := func(kind core.TaskContextProposalTargetKind, target string) string {
		return "/v1/tasks/" + taskID + "/context/proposals/" + string(kind) + "/" + target
	}
	wantPaths := []string{
		route(core.TaskContextProposalRequirement, requirement.ID) + "/confirm",
		route(core.TaskContextProposalRequirement, requirement.ID) + "/dismiss",
		route(core.TaskContextProposalSystemDesign, design.ID) + "/confirm",
		route(core.TaskContextProposalSystemDesign, design.ID) + "/dismiss",
	}
	var paths []string
	for _, choice := range action.Choices {
		paths = append(paths, choice.Path)
		if !choice.Allowed || choice.Capability != string(core.CapabilityOperateGates) {
			t.Fatalf("choice %+v, want allowed under operate_gates", choice)
		}
	}
	if !reflect.DeepEqual(paths, wantPaths) {
		t.Fatalf("context choice paths=%v want %v", paths, wantPaths)
	}
	for _, choice := range read(taskID, map[core.Capability]bool{core.CapabilityConfirmDocuments: true}).Choices {
		if choice.Allowed {
			t.Fatalf("choice %s allowed by confirm_documents", choice.ID)
		}
	}

	// Deciding one leaves the row; deciding the last removes it.
	_, err = st.ConfirmTaskContextProposal(ctx, taskID, core.TaskContextProposalSystemDesign, design.ID)
	requireOK(t, err)
	if action := read(taskID, gates); action.Kind != nextaction.KindContextProposal || len(action.Choices) != 2 {
		t.Fatalf("after one decision = %s choices %+v, want one remaining context proposal", action.Kind, action.Choices)
	}
	_, err = st.DismissTaskContextProposal(ctx, taskID, core.TaskContextProposalRequirement, requirement.ID)
	requireOK(t, err)
	if action := read(taskID, gates); action.Kind == nextaction.KindContextProposal || store.TaskNeedsAttention(action) {
		t.Fatalf("after the last decision = %s/%s, want no operator attention", action.Actor, action.Kind)
	}
	if after := count(); after != before {
		t.Fatalf("attention count after decisions=%d want %d", after, before)
	}
}
