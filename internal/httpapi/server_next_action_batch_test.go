package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/nextaction"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
)

// claimWaitCountingStore counts the claim-blocking reads a request issues
// (component-http-api, Activity and attention reads).
type claimWaitCountingStore struct {
	store.Store
	mu      sync.Mutex
	batched int
	single  int
}

func (c *claimWaitCountingStore) ListClaimBlockingProposalsForTasks(ctx context.Context, ids []string) (map[string][]store.ClaimBlockingProposal, error) {
	c.mu.Lock()
	c.batched++
	c.mu.Unlock()
	return c.Store.ListClaimBlockingProposalsForTasks(ctx, ids)
}

func (c *claimWaitCountingStore) ListClaimBlockingProposalsForTask(ctx context.Context, id string) ([]store.ClaimBlockingProposal, error) {
	c.mu.Lock()
	c.single++
	c.mu.Unlock()
	return c.Store.ListClaimBlockingProposalsForTask(ctx, id)
}

func (c *claimWaitCountingStore) ReadVerificationReviews(ctx context.Context, refs []store.VerificationReviewRef) (map[string]store.VerificationReviewState, error) {
	return c.Store.(store.VerificationReviewBatchReader).ReadVerificationReviews(ctx, refs)
}

func (c *claimWaitCountingStore) ReadVerificationReview(ctx context.Context, taskID, orderID string) (store.VerificationReviewState, error) {
	return c.Store.(store.VerificationReviewReader).ReadVerificationReview(ctx, taskID, orderID)
}

// fullHistoryOracleStore replaces the bounded history read with the
// previous fold over each task's full ListEvents history.
type fullHistoryOracleStore struct{ store.Store }

func (f fullHistoryOracleStore) ListNextActionHistory(ctx context.Context, ids []string) (map[string]store.NextActionHistory, error) {
	out := map[string]store.NextActionHistory{}
	for _, id := range ids {
		events, err := f.Store.ListEvents(ctx, id)
		if err != nil {
			return nil, err
		}
		var kinds []core.Event
		for _, event := range events {
			if store.NextActionEventKind(event.Kind) {
				kinds = append(kinds, event)
			}
		}
		orders, err := f.Store.ListWorkOrdersForTasks(ctx, []string{id})
		if err != nil {
			return nil, err
		}
		out[id] = store.FoldNextActionHistory(kinds, orders)
	}
	return out, nil
}

func (f fullHistoryOracleStore) ReadVerificationReviews(ctx context.Context, refs []store.VerificationReviewRef) (map[string]store.VerificationReviewState, error) {
	return f.Store.(store.VerificationReviewBatchReader).ReadVerificationReviews(ctx, refs)
}

type batchHTTPFixture struct {
	st  store.Store
	ctx context.Context
	now time.Time
}

func newBatchHTTPFixture(t *testing.T) *batchHTTPFixture {
	t.Helper()
	return &batchHTTPFixture{st: store.NewMemory(), ctx: store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo"), now: time.Now().UTC().Truncate(time.Second)}
}

func (f *batchHTTPFixture) task(t *testing.T, name string, state core.TaskState, gate core.GateKind, dependencies ...string) core.Task {
	t.Helper()
	id := core.NewTaskID()
	task := core.Task{ID: id, Workspace: "demo", Title: name + " " + id, Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + id, State: state, GateKind: gate, NextStage: core.StageImplement, CreatedAt: f.now}
	if err := f.st.CreateTaskWithDependencies(f.ctx, task, dependencies); err != nil {
		t.Fatal(err)
	}
	return task
}

func (f *batchHTTPFixture) order(t *testing.T, task core.Task, stage core.Stage, mutate func(*core.WorkOrder)) core.WorkOrder {
	t.Helper()
	id := fmt.Sprintf("%s-%s-1", task.ID, stage)
	if err := f.st.CreateJob(f.ctx, core.Job{ID: id, TaskID: task.ID, Stage: stage, State: core.JobPending}); err != nil {
		t.Fatal(err)
	}
	order := core.WorkOrder{ID: id, TaskID: task.ID, JobID: id, Stage: stage, State: core.WorkOrderQueued, QueueEnteredAt: f.now, QueueDeadline: f.now.Add(time.Hour)}
	if mutate != nil {
		mutate(&order)
	}
	if err := storetest.CreateWorkOrder(f.ctx, f.st, order); err != nil {
		t.Fatal(err)
	}
	return order
}

func (f *batchHTTPFixture) requirement(t *testing.T, title string, confirm bool) string {
	t.Helper()
	id := "req-http-batch-" + core.NewTaskID()
	_, version, err := f.st.CreateRequirement(f.ctx, core.Requirement{ID: id, Title: title + " " + id}, core.RequirementVersion{Content: "# " + title, Origin: core.RequirementOriginOperator, Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "The read states " + title + "."}}})
	if err != nil {
		t.Fatal(err)
	}
	if confirm {
		if _, _, err = f.st.ConfirmRequirementVersion(f.ctx, id, version.Version); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

func batchHTTPGet(t *testing.T, server *Server, path string, out any) {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, path, nil)
	request.Header.Set("Authorization", "Bearer operator-token")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("GET %s status=%d body=%s", path, response.Code, response.Body.String())
	}
	if err := json.Unmarshal(response.Body.Bytes(), out); err != nil {
		t.Fatal(err)
	}
}

func TestActivityReadsClaimWaitOnce(t *testing.T) {
	f := newBatchHTTPFixture(t)
	waiting := f.task(t, "claim wait", core.TaskRunning, "")
	f.order(t, waiting, core.StageVerify, nil)
	served := f.requirement(t, "claim wait", true)
	if _, err := f.st.ProposeRequirementVersion(f.ctx, core.RequirementVersion{RequirementID: served, Content: "# Claim wait v2", Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "The read waits."}}, Origin: core.RequirementOriginImplementation, OriginTaskID: waiting.ID}); err != nil {
		t.Fatal(err)
	}
	other := f.task(t, "queued", core.TaskQueued, "")
	f.order(t, other, core.StageImplement, nil)

	counter := &claimWaitCountingStore{Store: f.st}
	server := NewServer(counter)
	server.BearerToken, server.Workspace = "operator-token", "demo"
	var items []activityItem
	batchHTTPGet(t, server, "/v1/activity?workspace_id=demo", &items)
	if counter.batched != 1 || counter.single != 0 {
		t.Fatalf("activity claim-blocking reads batched=%d single=%d, want one batched read", counter.batched, counter.single)
	}
	byID := map[string]activityItem{}
	for _, item := range items {
		byID[item.Task.ID] = item
	}
	opts, err := server.nextActionOptionsFor(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range []core.Task{waiting, other} {
		want, err := store.ReadNextAction(f.ctx, f.st, task.ID, opts)
		if err != nil {
			t.Fatal(err)
		}
		item := byID[task.ID]
		if item.NextAction == nil || item.NextAction.Kind != want.Kind || item.NextAction.Actor != want.Actor {
			t.Fatalf("task %s activity next_action=%+v want %s/%s", task.ID, item.NextAction, want.Kind, want.Actor)
		}
	}
	if !byID[waiting.ID].ProposalClaimWaiting || byID[waiting.ID].NextAction.Kind != nextaction.KindDocumentProposal || byID[other.ID].ProposalClaimWaiting {
		t.Fatalf("claim-wait items=%+v", byID)
	}
}

func TestNextActionReadsStayEqualThroughBatchedLoaders(t *testing.T) {
	f := newBatchHTTPFixture(t)
	blocker := f.task(t, "blocker", core.TaskRunning, "")
	f.order(t, blocker, core.StageImplement, nil)
	dependent := f.task(t, "dependent", core.TaskQueued, "", blocker.ID)
	f.order(t, dependent, core.StageImplement, nil)
	offPage := f.task(t, "off-page dependent", core.TaskQueued, "", blocker.ID)
	f.order(t, offPage, core.StageImplement, nil)
	cited := f.requirement(t, "cited", true)
	checkpoint := f.task(t, "checkpoint", core.TaskRunning, "")
	f.order(t, checkpoint, core.StageImplement, func(order *core.WorkOrder) {
		order.LastAttemptOutcome = core.WorkOrderOutcomeReleased
		order.Checkpoint = &core.WorkOrderCheckpoint{DecisionRequest: "Confirm the cited requirement.", Class: core.WorkOrderCheckpointClassAuthorityConflict, Citations: []core.WorkOrderAuthorityConflictCitation{{DocumentID: cited, CitedVersion: 1}}}
	})
	disposition := f.task(t, "disposition", core.TaskRunning, "")
	f.order(t, disposition, core.StageVerify, func(order *core.WorkOrder) {
		order.LastAttemptID, order.LastAttemptOutcome = "attempt-"+disposition.ID, core.WorkOrderOutcomeReleased
		order.Checkpoint = &core.WorkOrderCheckpoint{Verification: &core.WorkOrderVerificationCheckpoint{ContextID: "context-" + disposition.ID, HeadSHA: "head", Reason: "unresolved", RequiredAction: "dispose", OperationIDs: []string{"operation-" + disposition.ID}}}
	})
	parked := f.task(t, "parked", core.TaskParked, "")
	for i := 0; i < 50; i++ {
		if err := f.st.AppendEvent(f.ctx, core.Event{TaskID: parked.ID, Kind: "task.state_changed", ActorRole: core.ActorSystem, At: f.now.Add(time.Duration(i) * time.Second), Payload: core.JSONPayload(map[string]any{"from": "queued", "to": "running"})}); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.st.AppendEvent(f.ctx, core.Event{TaskID: parked.ID, Kind: "task.state_changed", ActorRole: core.ActorSystem, At: f.now.Add(-time.Hour), Payload: core.JSONPayload(map[string]any{"command": "dispatch.fail_final"})}); err != nil {
		t.Fatal(err)
	}

	server := NewServer(f.st)
	server.BearerToken, server.Workspace = "operator-token", "demo"
	// The operator bearer credential is granted every next-action
	// capability, so the oracle projects with all of them.
	opts := store.NextActionOptions{Now: f.now, Capabilities: map[core.Capability]bool{}}
	for _, capability := range nextActionCapabilities {
		opts.Capabilities[capability] = true
	}
	open, err := f.st.ListTasksFiltered(f.ctx, store.TaskFilter{States: []core.TaskState{core.TaskQueued, core.TaskRunning, core.TaskParked}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		task core.Task
		kind string
	}{{blocker, ""}, {dependent, nextaction.KindBlocked}, {checkpoint, nextaction.KindAgentQuestion}, {disposition, nextaction.KindVerificationDisposition}, {parked, nextaction.KindDispatchExhausted}} {
		task, kind := tc.task, tc.kind
		var got nextaction.NextAction
		batchHTTPGet(t, server, "/v1/tasks/"+task.ID+"/next-action?workspace_id=demo", &got)
		oracleOpts := opts
		oracleOpts.OpenTasksLoaded, oracleOpts.OpenTasks = true, open
		projected, err := store.ReadNextActions(f.ctx, fullHistoryOracleStore{f.st}, []core.Task{task}, oracleOpts)
		if err != nil {
			t.Fatal(err)
		}
		want := projected[task.ID]
		gotJSON, _ := json.Marshal(struct {
			Kind, Actor, Label, Question string
			Choices                      []nextaction.Choice
			BlockedBy, Blocks            []nextaction.BlockerRef
			Details                      nextaction.Details
		}{got.Kind, got.Actor, got.Label, got.Question, got.Choices, got.BlockedBy, got.Blocks, got.Details})
		wantJSON, _ := json.Marshal(struct {
			Kind, Actor, Label, Question string
			Choices                      []nextaction.Choice
			BlockedBy, Blocks            []nextaction.BlockerRef
			Details                      nextaction.Details
		}{want.Kind, want.Actor, want.Label, want.Question, want.Choices, want.BlockedBy, want.Blocks, want.Details})
		if string(gotJSON) != string(wantJSON) {
			t.Fatalf("task %s next-action=%s\noracle=%s", task.ID, gotJSON, wantJSON)
		}
		if kind != "" && got.Kind != kind {
			t.Fatalf("task %s kind=%s want %s", task.ID, got.Kind, kind)
		}
	}
	var blockerAction nextaction.NextAction
	batchHTTPGet(t, server, "/v1/tasks/"+blocker.ID+"/next-action?workspace_id=demo", &blockerAction)
	if len(blockerAction.Blocks) != 2 {
		t.Fatalf("blocker blocks=%+v, want the dependent and the off-page dependent", blockerAction.Blocks)
	}

	// The attention total stays the operator-actor task count; pending
	// document proposals are counted apart from it.
	standalone := f.requirement(t, "standalone proposal", false)
	design, designVersion, err := f.st.CreateSystemDesign(f.ctx, core.SystemDesign{ID: "design-http-batch", Title: "Batch design", Category: "Component design"}, core.SystemDesignVersion{Content: "# Batch design\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/httpapi/**\n```", Origin: core.SystemDesignOriginOperator})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = f.st.ConfirmSystemDesignVersion(f.ctx, design.ID, designVersion.Version); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err = f.st.ProposeSystemDesignVersion(f.ctx, core.SystemDesignVersion{DocumentID: design.ID, Content: fmt.Sprintf("# Batch design v%d\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/httpapi/**\n```", i+2), Origin: core.SystemDesignOriginOperator}); err != nil {
			t.Fatal(err)
		}
	}
	var projection pendingProposalsResponse
	batchHTTPGet(t, server, "/v1/pending-proposals?workspace_id=demo", &projection)
	all, err := f.st.ListTasksFiltered(f.ctx, store.TaskFilter{States: []core.TaskState{core.TaskQueued, core.TaskRunning, core.TaskParked}})
	if err != nil {
		t.Fatal(err)
	}
	full, err := store.ReadNextActions(f.ctx, fullHistoryOracleStore{f.st}, all, store.NextActionOptions{OpenTasksLoaded: true, OpenTasks: all})
	if err != nil {
		t.Fatal(err)
	}
	operatorTasks := 0
	for _, task := range all {
		if store.TaskNeedsAttention(full[task.ID]) {
			operatorTasks++
		}
	}
	proposalIDs := map[string]int{}
	for _, item := range projection.Items {
		proposalIDs[item.ID]++
	}
	if projection.Attention.TaskCount != operatorTasks || projection.Attention.Total != operatorTasks || operatorTasks == 0 {
		t.Fatalf("attention=%+v want task total %d", projection.Attention, operatorTasks)
	}
	if proposalIDs[standalone] != 1 || proposalIDs[design.ID] != 2 || projection.Attention.PendingProposalCount != len(projection.Items) {
		t.Fatalf("pending proposals=%+v attention=%+v", proposalIDs, projection.Attention)
	}
}
