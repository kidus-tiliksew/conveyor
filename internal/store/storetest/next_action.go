package storetest

import (
	"context"
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
