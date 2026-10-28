package store_test

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/nextaction"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// countingStore counts every store call the next-action assembler can make
// and records the task IDs each batched read was asked for
// (component-work-orders, Next action, Bounded store calls).
type countingStore struct {
	store.Store
	mu    sync.Mutex
	calls map[string]int
	args  map[string][][]string
}

func newCountingStore(inner store.Store) *countingStore {
	return &countingStore{Store: inner, calls: map[string]int{}, args: map[string][][]string{}}
}

func (c *countingStore) count(name string, ids []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls[name]++
	c.args[name] = append(c.args[name], append([]string(nil), ids...))
}

func (c *countingStore) reset() map[string]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.calls
	c.calls, c.args = map[string]int{}, map[string][][]string{}
	return out
}

func (c *countingStore) GetTask(ctx context.Context, id string) (core.Task, error) {
	c.count("GetTask", []string{id})
	return c.Store.GetTask(ctx, id)
}

func (c *countingStore) ListTasksFiltered(ctx context.Context, filter store.TaskFilter) ([]core.Task, error) {
	c.count("ListTasksFiltered", nil)
	return c.Store.ListTasksFiltered(ctx, filter)
}

func (c *countingStore) ListEvents(ctx context.Context, id string) ([]core.Event, error) {
	c.count("ListEvents", []string{id})
	return c.Store.ListEvents(ctx, id)
}

func (c *countingStore) ListWorkOrdersForTasks(ctx context.Context, ids []string) ([]core.WorkOrder, error) {
	c.count("ListWorkOrdersForTasks", ids)
	return c.Store.ListWorkOrdersForTasks(ctx, ids)
}

func (c *countingStore) ListActivityMarkersForTasks(ctx context.Context, ids []string) ([]store.ActivityMarker, error) {
	c.count("ListActivityMarkersForTasks", ids)
	return c.Store.ListActivityMarkersForTasks(ctx, ids)
}

func (c *countingStore) ListPendingProposals(ctx context.Context) ([]core.PendingProposal, error) {
	c.count("ListPendingProposals", nil)
	return c.Store.ListPendingProposals(ctx)
}

func (c *countingStore) ListDependencyBlockers(ctx context.Context, ids []string) (map[string]store.DependencyBlockers, error) {
	c.count("ListDependencyBlockers", ids)
	return c.Store.ListDependencyBlockers(ctx, ids)
}

func (c *countingStore) ListOpenDependents(ctx context.Context, ids []string) ([]store.TaskHead, error) {
	c.count("ListOpenDependents", ids)
	return c.Store.ListOpenDependents(ctx, ids)
}

func (c *countingStore) ListTaskHeads(ctx context.Context, ids []string) (map[string]store.TaskHead, error) {
	c.count("ListTaskHeads", ids)
	return c.Store.ListTaskHeads(ctx, ids)
}

func (c *countingStore) ListNextActionEvents(ctx context.Context, ids []string) (map[string][]core.Event, error) {
	c.count("ListNextActionEvents", ids)
	return c.Store.ListNextActionEvents(ctx, ids)
}

func (c *countingStore) ListNextActionHistory(ctx context.Context, ids []string) (map[string]store.NextActionHistory, error) {
	c.count("ListNextActionHistory", ids)
	return c.Store.ListNextActionHistory(ctx, ids)
}

func (c *countingStore) ListLatestSpecVersionNumbers(ctx context.Context, ids []string) (map[string]int, error) {
	c.count("ListLatestSpecVersionNumbers", ids)
	return c.Store.ListLatestSpecVersionNumbers(ctx, ids)
}

func (c *countingStore) GetLatestSpecVersion(ctx context.Context, id string) (core.SpecVersion, bool, error) {
	c.count("GetLatestSpecVersion", []string{id})
	return c.Store.GetLatestSpecVersion(ctx, id)
}

func (c *countingStore) ListClaimBlockingProposalsForTasks(ctx context.Context, ids []string) (map[string][]store.ClaimBlockingProposal, error) {
	c.count("ListClaimBlockingProposalsForTasks", ids)
	return c.Store.ListClaimBlockingProposalsForTasks(ctx, ids)
}

func (c *countingStore) ListClaimBlockingProposalsForTask(ctx context.Context, id string) ([]store.ClaimBlockingProposal, error) {
	c.count("ListClaimBlockingProposalsForTask", []string{id})
	return c.Store.ListClaimBlockingProposalsForTask(ctx, id)
}

func (c *countingStore) ListTaskContextProposals(ctx context.Context, taskID string, state core.TaskContextProposalState) ([]core.TaskContextProposal, error) {
	c.count("ListTaskContextProposals", []string{taskID})
	return c.Store.ListTaskContextProposals(ctx, taskID, state)
}

func (c *countingStore) ListTaskContextDesignPins(ctx context.Context, ids, documents []string) (map[string]map[string]int, error) {
	c.count("ListTaskContextDesignPins", ids)
	return c.Store.ListTaskContextDesignPins(ctx, ids, documents)
}

func (c *countingStore) ListDocumentHeads(ctx context.Context, ids []string) (map[string]store.DocumentHead, error) {
	c.count("ListDocumentHeads", ids)
	return c.Store.ListDocumentHeads(ctx, ids)
}

func (c *countingStore) GetRequirement(ctx context.Context, id string) (core.Requirement, error) {
	c.count("GetRequirement", []string{id})
	return c.Store.GetRequirement(ctx, id)
}

func (c *countingStore) GetSystemDesign(ctx context.Context, id string) (core.SystemDesign, error) {
	c.count("GetSystemDesign", []string{id})
	return c.Store.GetSystemDesign(ctx, id)
}

func (c *countingStore) ReadVerificationReview(ctx context.Context, taskID, orderID string) (store.VerificationReviewState, error) {
	c.count("ReadVerificationReview", []string{taskID})
	return c.Store.(store.VerificationReviewReader).ReadVerificationReview(ctx, taskID, orderID)
}

func (c *countingStore) ReadVerificationReviews(ctx context.Context, refs []store.VerificationReviewRef) (map[string]store.VerificationReviewState, error) {
	ids := make([]string, 0, len(refs))
	for _, ref := range refs {
		ids = append(ids, ref.TaskID)
	}
	c.count("ReadVerificationReviews", ids)
	return c.Store.(store.VerificationReviewBatchReader).ReadVerificationReviews(ctx, refs)
}

// maxBatchCalls bounds each batched read: the order read runs once for the
// request and once for dependants outside it; every other read runs at most
// once (component-work-orders, Next action, Bounded store calls).
func maxBatchCalls(name string) int {
	if name == "ListWorkOrdersForTasks" {
		return 2
	}
	return 1
}

// perTaskReads are the reads a batching store never issues from the
// assembler (component-work-orders, Next action, Bounded store calls).
var perTaskReads = []string{"GetTask", "ListEvents", "GetLatestSpecVersion", "GetRequirement", "GetSystemDesign", "ListClaimBlockingProposalsForTask", "ReadVerificationReview", "ListNextActionEvents"}

// fullHistoryStore is the oracle: it replaces the bounded history read with
// the previous fold over each task's full ListEvents history.
type fullHistoryStore struct{ store.Store }

func (f fullHistoryStore) ListNextActionHistory(ctx context.Context, ids []string) (map[string]store.NextActionHistory, error) {
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

func (f fullHistoryStore) ReadVerificationReviews(ctx context.Context, refs []store.VerificationReviewRef) (map[string]store.VerificationReviewState, error) {
	return f.Store.(store.VerificationReviewBatchReader).ReadVerificationReviews(ctx, refs)
}

func (f fullHistoryStore) ReadVerificationReview(ctx context.Context, taskID, orderID string) (store.VerificationReviewState, error) {
	return f.Store.(store.VerificationReviewReader).ReadVerificationReview(ctx, taskID, orderID)
}

var batchCapabilities = map[core.Capability]bool{core.CapabilityOperateGates: true, core.CapabilityConfirmDocuments: true, core.CapabilityRecoverWork: true}

type batchFixture struct {
	st        store.Store
	ctx       context.Context
	workspace string
	now       time.Time
	// kinds is the next action every fixture task must project.
	kinds map[string]string
	// requested are the tasks a batch read projects; off-request dependants
	// stay outside it.
	requested []core.Task
	seq       int
}

func newBatchFixture(t *testing.T) *batchFixture {
	t.Helper()
	st := store.NewVolatileBackend()
	t.Cleanup(st.Close)
	workspace := "batch-" + core.NewTaskID()
	ctx := store.WithActor(store.WithWorkspace(t.Context(), workspace), store.SystemActor())
	return &batchFixture{st: st, ctx: ctx, workspace: workspace, now: time.Now().UTC().Truncate(time.Second), kinds: map[string]string{}}
}

func (f *batchFixture) task(t *testing.T, name string, state core.TaskState, gate core.GateKind, dependencies ...string) core.Task {
	t.Helper()
	// A sequence keeps IDs unique across hundreds of tasks in one store,
	// where random task IDs could collide.
	f.seq++
	id := fmt.Sprintf("%s-%04d", core.NewTaskID(), f.seq)
	task := core.Task{ID: id, Workspace: f.workspace, Title: name + " " + id, Body: "body", Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + id, State: state, GateKind: gate, NextStage: core.StageImplement, CreatedAt: f.now, MergeApproval: state == core.TaskApproved}
	if err := f.st.CreateTaskWithDependencies(f.ctx, task, dependencies); err != nil {
		t.Fatal(err)
	}
	return task
}

func (f *batchFixture) order(t *testing.T, task core.Task, stage core.Stage, attempt int, mutate func(*core.WorkOrder)) core.WorkOrder {
	t.Helper()
	id := fmt.Sprintf("%s-%s-%d", task.ID, stage, attempt)
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

func (f *batchFixture) event(t *testing.T, taskID, kind string, at time.Time, payload json.RawMessage) {
	t.Helper()
	if err := f.st.AppendEvent(f.ctx, core.Event{TaskID: taskID, Kind: kind, ActorRole: core.ActorSystem, At: at, Payload: payload}); err != nil {
		t.Fatal(err)
	}
}

// history appends unrelated and superseded history; long multiplies it.
func (f *batchFixture) history(t *testing.T, taskID string, long bool) {
	t.Helper()
	n := 2
	if long {
		n = 120
	}
	at := f.now.Add(-24 * time.Hour)
	for i := 0; i < n; i++ {
		at = at.Add(time.Second)
		f.event(t, taskID, "task.state_changed", at, core.JSONPayload(map[string]any{"from": "queued", "to": "running"}))
		f.event(t, taskID, "dispatch.reconciled", at, core.JSONPayload(map[string]any{"note": "unrelated"}))
		f.event(t, taskID, "pull_request.opened", at, core.JSONPayload(map[string]any{"number": 900 + i}))
	}
}

// add builds one copy of every exceptional row, with short or long history.
func (f *batchFixture) add(t *testing.T, long bool) {
	t.Helper()
	confirmedRequirement := func(title string) string {
		id := "req-batch-" + core.NewTaskID()
		title += " " + id
		_, version, err := f.st.CreateRequirement(f.ctx, core.Requirement{ID: id, Title: title}, core.RequirementVersion{Content: "# " + title, Origin: core.RequirementOriginOperator, Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "The batch reads " + title + "."}}})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err = f.st.ConfirmRequirementVersion(f.ctx, id, version.Version); err != nil {
			t.Fatal(err)
		}
		return id
	}

	parked := f.task(t, "parked", core.TaskParked, "")
	f.history(t, parked.ID, long)
	f.event(t, parked.ID, "task.state_changed", f.now.Add(-time.Hour), core.JSONPayload(map[string]any{"command": "triage.park"}))
	f.event(t, parked.ID, "task.state_changed", f.now.Add(-time.Minute), json.RawMessage(`{"command":`))
	f.event(t, parked.ID, "task.state_changed", f.now.Add(-time.Minute), core.JSONPayload(map[string]any{"command": 7}))
	f.kinds[parked.ID] = nextaction.KindParked

	conflict := f.task(t, "conflict", core.TaskApproved, "")
	f.history(t, conflict.ID, long)
	episode := core.JSONPayload(map[string]any{"approved_head": "a", "new_head": "b"})
	failed := core.JSONPayload(map[string]any{"approved_head": "a", "new_head": "b", "next_retry_at": f.now.Add(time.Hour)})
	f.event(t, conflict.ID, "merge.blocked", f.now.Add(-50*time.Minute), episode)
	f.event(t, conflict.ID, "merge.conflict_dispatch_failed", f.now.Add(-49*time.Minute), failed)
	f.event(t, conflict.ID, "merge.conflict_cleared", f.now.Add(-48*time.Minute), core.JSONPayload(map[string]any{}))
	f.event(t, conflict.ID, "merge.blocked", f.now.Add(-40*time.Minute), episode)
	for i := 0; i < 3; i++ {
		f.event(t, conflict.ID, "merge.conflict_dispatch_failed", f.now.Add(-30*time.Minute), failed)
	}
	f.kinds[conflict.ID] = nextaction.KindMergeConflict

	plan := f.task(t, "plan", core.TaskAwaiting, core.GateKindPlanApproval)
	f.history(t, plan.ID, long)
	for i := 0; i < 2; i++ {
		if _, err := f.st.CreateSpecVersion(f.ctx, core.SpecVersion{TaskID: plan.ID, Content: "plan", Acceptance: core.JSONPayload([]any{}), Decomposition: core.JSONPayload([]any{})}); err != nil {
			t.Fatal(err)
		}
	}
	f.kinds[plan.ID] = nextaction.KindPlanApproval

	waiting := f.task(t, "claim wait", core.TaskRunning, "")
	f.history(t, waiting.ID, long)
	f.order(t, waiting, core.StageVerify, 1, nil)
	served := confirmedRequirement("claim wait")
	if _, err := f.st.ProposeRequirementVersion(f.ctx, core.RequirementVersion{RequirementID: served, Content: "# Claim wait v2", Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "The batch waits."}}, Origin: core.RequirementOriginImplementation, OriginTaskID: waiting.ID}); err != nil {
		t.Fatal(err)
	}
	f.kinds[waiting.ID] = nextaction.KindDocumentProposal

	blocker := f.task(t, "blocker", core.TaskRunning, "")
	f.order(t, blocker, core.StageImplement, 1, nil)
	dependent := f.task(t, "dependent", core.TaskQueued, "", blocker.ID)
	f.order(t, dependent, core.StageImplement, 1, nil)
	offRequest := f.task(t, "off-request dependent", core.TaskQueued, "", blocker.ID)
	f.order(t, offRequest, core.StageImplement, 1, nil)
	f.kinds[dependent.ID] = nextaction.KindBlocked
	outside := f.task(t, "outside blocker", core.TaskRunning, "")
	outsideDependent := f.task(t, "outside dependent", core.TaskQueued, "", outside.ID)
	f.order(t, outsideDependent, core.StageImplement, 1, nil)
	f.kinds[outsideDependent.ID] = nextaction.KindBlocked
	closedBlocker := f.task(t, "closed blocker", core.TaskQueued, "")
	stranded := f.task(t, "stranded", core.TaskQueued, "", closedBlocker.ID)
	f.order(t, stranded, core.StageImplement, 1, nil)
	if _, err := taskops.New(f.st).Perform(f.ctx, closedBlocker.ID, taskops.Command{Kind: core.TaskCancel}); err != nil {
		t.Fatal(err)
	}

	cited := confirmedRequirement("cited")
	checkpoint := f.task(t, "checkpoint", core.TaskRunning, "")
	f.history(t, checkpoint.ID, long)
	f.event(t, checkpoint.ID, store.TaskContextDesignAdded, f.now.Add(-time.Hour), core.JSONPayload(map[string]any{"id": "design-batch-missing", "version": 1}))
	f.order(t, checkpoint, core.StageImplement, 1, func(order *core.WorkOrder) {
		order.LastAttemptOutcome = core.WorkOrderOutcomeReleased
		order.Checkpoint = &core.WorkOrderCheckpoint{DecisionRequest: "Confirm the cited requirement.", Class: core.WorkOrderCheckpointClassAuthorityConflict, Citations: []core.WorkOrderAuthorityConflictCitation{{DocumentID: cited, CitedVersion: 1}, {DocumentID: "design-batch-missing", CitedVersion: 1}}}
	})
	f.kinds[checkpoint.ID] = nextaction.KindAgentQuestion

	disposition := f.task(t, "disposition", core.TaskRunning, "")
	f.history(t, disposition.ID, long)
	f.order(t, disposition, core.StageVerify, 1, func(order *core.WorkOrder) {
		order.Checkpoint = &core.WorkOrderCheckpoint{Verification: &core.WorkOrderVerificationCheckpoint{ContextID: "context-" + disposition.ID, HeadSHA: "head", Reason: "unresolved", RequiredAction: "dispose", OperationIDs: []string{"operation-" + disposition.ID}}}
	})
	f.kinds[disposition.ID] = nextaction.KindVerificationDisposition

	for _, task := range []core.Task{parked, conflict, plan, waiting, blocker, dependent, outsideDependent, stranded, checkpoint, disposition} {
		stored, err := f.st.GetTask(f.ctx, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		f.requested = append(f.requested, stored)
	}
}

func (f *batchFixture) openTasks(t *testing.T) []core.Task {
	t.Helper()
	open, err := f.st.ListTasksFiltered(f.ctx, store.TaskFilter{States: []core.TaskState{core.TaskClaiming, core.TaskQueued, core.TaskRunning, core.TaskAwaiting, core.TaskApproved, core.TaskParked}})
	if err != nil {
		t.Fatal(err)
	}
	return open
}

// oracle projects the batch through the previous full-history fold and the
// previous whole-workspace open-task dependency read.
func (f *batchFixture) oracle(t *testing.T) map[string]nextaction.NextAction {
	t.Helper()
	want, err := store.ReadNextActions(f.ctx, fullHistoryStore{f.st}, f.requested, store.NextActionOptions{Now: f.now, Capabilities: batchCapabilities, OpenTasksLoaded: true, OpenTasks: f.openTasks(t)})
	if err != nil {
		t.Fatal(err)
	}
	return want
}

func TestReadNextActionsStoreCallsDoNotGrowWithTasksOrHistory(t *testing.T) {
	var baseline map[string]int
	for _, copies := range []int{1, 4, 16} {
		for _, long := range []bool{false, true} {
			t.Run(fmt.Sprintf("copies=%d long=%t", copies, long), func(t *testing.T) {
				f := newBatchFixture(t)
				for i := 0; i < copies; i++ {
					f.add(t, long)
				}
				counter := newCountingStore(f.st)
				got, err := store.ReadNextActions(f.ctx, counter, f.requested, store.NextActionOptions{Now: f.now, Capabilities: batchCapabilities})
				if err != nil {
					t.Fatal(err)
				}
				calls := counter.reset()
				for id, kind := range f.kinds {
					if got[id].Kind != kind {
						t.Fatalf("task %s kind=%s want %s", id, got[id].Kind, kind)
					}
				}
				for name, n := range calls {
					if n > maxBatchCalls(name) {
						t.Fatalf("%s called %d times, want at most %d: %v", name, n, maxBatchCalls(name), calls)
					}
				}
				for _, name := range perTaskReads {
					if calls[name] != 0 {
						t.Fatalf("per-task read %s issued: %v", name, calls)
					}
				}
				if calls["ListTasksFiltered"] != 0 {
					t.Fatalf("dependency view listed workspace tasks: %v", calls)
				}
				if baseline == nil {
					baseline = calls
				} else if !maps.Equal(calls, baseline) {
					t.Fatalf("store calls grew: %v, baseline %v", calls, baseline)
				}
			})
		}
	}
	for _, name := range []string{"ListNextActionHistory", "ListOpenDependents", "ListDocumentHeads", "ListTaskContextDesignPins", "ReadVerificationReviews", "ListClaimBlockingProposalsForTasks", "ListTaskHeads"} {
		if baseline[name] != 1 {
			t.Fatalf("fixture did not reach %s: %v", name, baseline)
		}
	}
}

func TestBatchedProjectionMatchesFullHistoryOracle(t *testing.T) {
	f := newBatchFixture(t)
	f.add(t, true)
	f.add(t, false)
	got, err := store.ReadNextActions(f.ctx, f.st, f.requested, store.NextActionOptions{Now: f.now, Capabilities: batchCapabilities})
	if err != nil {
		t.Fatal(err)
	}
	want := f.oracle(t)
	if len(got) != len(f.requested) {
		t.Fatalf("projected %d tasks, want %d", len(got), len(f.requested))
	}
	for _, task := range f.requested {
		if !reflect.DeepEqual(got[task.ID], want[task.ID]) {
			gotJSON, _ := json.Marshal(got[task.ID])
			wantJSON, _ := json.Marshal(want[task.ID])
			t.Fatalf("task %s batched=%s\noracle=%s", task.ID, gotJSON, wantJSON)
		}
		one, err := store.ReadNextAction(f.ctx, f.st, task.ID, store.NextActionOptions{Now: f.now, Capabilities: batchCapabilities})
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(one, want[task.ID]) {
			t.Fatalf("task %s single=%+v oracle=%+v", task.ID, one, want[task.ID])
		}
	}
	for id, kind := range f.kinds {
		action := got[id]
		switch kind {
		case nextaction.KindParked:
			if action.Details.ReasonCode != "triage.park" {
				t.Fatalf("parked reason=%q", action.Details.ReasonCode)
			}
		case nextaction.KindVerificationDisposition:
			if len(action.Details.Refusals) == 0 && action.Details.Disposition == nil {
				t.Fatalf("disposition carries neither a binding nor a refusal: %+v", action.Details)
			}
		case nextaction.KindBlocked:
			if len(action.BlockedBy) != 1 {
				t.Fatalf("dependent blocked_by=%+v", action.BlockedBy)
			}
		}
	}
}

func TestCountOperatorAttentionStoreCallsDoNotGrowWithTasks(t *testing.T) {
	var baseline map[string]int
	for _, copies := range []int{1, 4, 16} {
		f := newBatchFixture(t)
		for i := 0; i < copies; i++ {
			f.add(t, copies == 16)
		}
		counter := newCountingStore(f.st)
		total, err := store.CountOperatorAttention(f.ctx, counter)
		if err != nil {
			t.Fatal(err)
		}
		calls := counter.reset()
		open := f.openTasks(t)
		full, err := store.ReadNextActions(f.ctx, fullHistoryStore{f.st}, open, store.NextActionOptions{OpenTasksLoaded: true, OpenTasks: open})
		if err != nil {
			t.Fatal(err)
		}
		want := 0
		for _, task := range open {
			if store.TaskNeedsAttention(full[task.ID]) {
				want++
			}
		}
		if total != want || want == 0 {
			t.Fatalf("copies=%d attention=%d want %d", copies, total, want)
		}
		if calls["ListTasksFiltered"] != 1 || calls["ReadVerificationReviews"] != 0 || calls["ListOpenDependents"] != 0 {
			t.Fatalf("copies=%d calls=%v", copies, calls)
		}
		for name, n := range calls {
			if n > maxBatchCalls(name) || slices.Contains(perTaskReads, name) {
				t.Fatalf("copies=%d %s called %d times: %v", copies, name, n, calls)
			}
		}
		if baseline == nil {
			baseline = calls
		} else if !maps.Equal(calls, baseline) {
			t.Fatalf("copies=%d store calls grew: %v, baseline %v", copies, calls, baseline)
		}
	}
}

func TestLoadedClaimWaitingSkipsRead(t *testing.T) {
	f := newBatchFixture(t)
	f.add(t, false)
	counter := newCountingStore(f.st)
	computed, err := store.ReadNextActions(f.ctx, counter, f.requested, store.NextActionOptions{Now: f.now, Capabilities: batchCapabilities})
	if err != nil {
		t.Fatal(err)
	}
	if calls := counter.reset(); calls["ListClaimBlockingProposalsForTasks"] != 1 {
		t.Fatalf("unloaded selection calls=%v", calls)
	}
	orders, err := f.st.ListWorkOrdersForTasks(f.ctx, taskIDs(f.requested))
	if err != nil {
		t.Fatal(err)
	}
	selection, err := store.ClaimWaitingProposals(f.ctx, f.st, orders)
	if err != nil {
		t.Fatal(err)
	}
	supplied, err := store.ReadNextActions(f.ctx, counter, f.requested, store.NextActionOptions{Now: f.now, Capabilities: batchCapabilities, ClaimWaitingLoaded: true, ClaimWaiting: selection})
	if err != nil {
		t.Fatal(err)
	}
	if calls := counter.reset(); calls["ListClaimBlockingProposalsForTasks"] != 0 || calls["ListClaimBlockingProposalsForTask"] != 0 {
		t.Fatalf("loaded selection read again: %v", calls)
	}
	if !reflect.DeepEqual(supplied, computed) {
		t.Fatal("a supplied selection projected differently from the computed one")
	}
	empty, err := store.ReadNextActions(f.ctx, counter, f.requested, store.NextActionOptions{Now: f.now, Capabilities: batchCapabilities, ClaimWaitingLoaded: true, ClaimWaiting: map[string][]store.ClaimBlockingProposal{}})
	if err != nil {
		t.Fatal(err)
	}
	if calls := counter.reset(); calls["ListClaimBlockingProposalsForTasks"] != 0 || calls["ListClaimBlockingProposalsForTask"] != 0 {
		t.Fatalf("loaded empty selection read again: %v", calls)
	}
	for id, kind := range f.kinds {
		if kind == nextaction.KindDocumentProposal && empty[id].Kind == nextaction.KindDocumentProposal {
			t.Fatalf("task %s used a read selection instead of the supplied empty one", id)
		}
	}
}

func TestSingleTaskDependencyReadLoadsOnlyNeighborhood(t *testing.T) {
	f := newBatchFixture(t)
	f.add(t, false)
	unrelated := f.task(t, "unrelated", core.TaskQueued, "")
	f.order(t, unrelated, core.StageImplement, 1, nil)
	var blocker, dependent core.Task
	for _, task := range f.requested {
		switch {
		case strings.HasPrefix(task.Title, "dependent "):
			dependent = task
		case strings.HasPrefix(task.Title, "blocker "):
			blocker = task
		}
	}
	counter := newCountingStore(f.st)
	action, err := store.ReadNextAction(f.ctx, counter, blocker.ID, store.NextActionOptions{Now: f.now, Capabilities: batchCapabilities})
	if err != nil {
		t.Fatal(err)
	}
	want := f.oracle(t)[blocker.ID]
	if !reflect.DeepEqual(action, want) {
		t.Fatalf("single blocker=%+v oracle=%+v", action, want)
	}
	if len(action.Blocks) != 2 {
		t.Fatalf("blocker blocks=%+v, want the requested dependent and the off-request one", action.Blocks)
	}
	if counter.calls["ListTasksFiltered"] != 0 {
		t.Fatalf("single-task read listed workspace tasks: %v", counter.calls)
	}
	for _, name := range []string{"ListOpenDependents", "ListDependencyBlockers", "ListWorkOrdersForTasks", "ListTaskHeads"} {
		for _, ids := range counter.args[name] {
			if slices.Contains(ids, unrelated.ID) {
				t.Fatalf("%s read the unrelated task: %v", name, counter.args)
			}
		}
	}
	read := map[string]bool{}
	for _, ids := range counter.args["ListWorkOrdersForTasks"] {
		for _, id := range ids {
			read[id] = true
		}
	}
	if !read[dependent.ID] {
		t.Fatalf("dependent orders were not read: %v", counter.args["ListWorkOrdersForTasks"])
	}
	single, err := store.ReadNextAction(f.ctx, newCountingStore(f.st), dependent.ID, store.NextActionOptions{Now: f.now, Capabilities: batchCapabilities})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(single, f.oracle(t)[dependent.ID]) || len(single.BlockedBy) != 1 || single.BlockedBy[0].TaskID != blocker.ID {
		t.Fatalf("single dependent=%+v", single)
	}
}

func taskIDs(tasks []core.Task) []string {
	out := make([]string, 0, len(tasks))
	for _, task := range tasks {
		out = append(out, task.ID)
	}
	sort.Strings(out)
	return out
}

// previousMergeConflictFold is the merge-conflict fold the assembler applied
// to the full history before the bounded summary.
func previousMergeConflictFold(events []core.Event, orders []core.WorkOrder) nextaction.MergeConflict {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Kind != "merge.blocked" {
			continue
		}
		var heads struct {
			ApprovedHead string `json:"approved_head"`
			NewHead      string `json:"new_head"`
		}
		if json.Unmarshal(events[i].Payload, &heads) != nil {
			return nextaction.MergeConflict{}
		}
		state := store.MergeConflictDispatchState(events, heads.ApprovedHead, heads.NewHead, orders)
		return nextaction.MergeConflict{Open: true, Exhausted: state.Exhausted || state.Failures >= 3, NextRetryAt: state.NextRetryAt}
	}
	return nextaction.MergeConflict{}
}

func TestMergeConflictHistoryMatchesFullFold(t *testing.T) {
	base := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	orders := []core.WorkOrder{
		{ID: "fix-terminal", State: core.WorkOrderCancelled},
		{ID: "fix-released", State: core.WorkOrderQueued, LastAttemptOutcome: core.WorkOrderOutcomeReleased},
		{ID: "fix-live", State: core.WorkOrderClaimed},
		{ID: "fix-early", State: core.WorkOrderStale},
	}
	blocked := func(approved, fresh string) string {
		return fmt.Sprintf(`{"approved_head":%q,"new_head":%q}`, approved, fresh)
	}
	dispatched := func(order string) string {
		return fmt.Sprintf(`{"approved_head":"a","new_head":"b","work_order_id":%q}`, order)
	}
	failed := func(retry string) string {
		return `{"approved_head":"a","new_head":"b","next_retry_at":` + retry + `}`
	}
	type step struct{ kind, payload string }
	cases := map[string][]step{
		"no block":            {{"merge.conflict_dispatch_failed", failed(`"2026-01-01T00:00:00Z"`)}},
		"undecodable latest":  {{"merge.blocked", blocked("a", "b")}, {"merge.blocked", `{"approved_head":5}`}},
		"array block":         {{"merge.blocked", `[1]`}},
		"null heads":          {{"merge.blocked", `null`}, {"merge.conflict_dispatch_failed", `{}`}, {"merge.conflict_dispatch_failed", `null`}, {"merge.conflict_dispatch_failed", `{"new_head":null}`}},
		"three failures":      {{"merge.blocked", blocked("a", "b")}, {"merge.conflict_dispatch_failed", failed(`"2026-01-01T00:00:00Z"`)}, {"merge.conflict_dispatch_failed", failed(`null`)}, {"merge.conflict_dispatch_failed", failed(`"2026-01-02T00:00:00Z"`)}},
		"undecodable retries": {{"merge.blocked", blocked("a", "b")}, {"merge.conflict_dispatch_failed", failed(`"tomorrow"`)}, {"merge.conflict_dispatch_failed", failed(`5`)}, {"merge.conflict_dispatch_failed", failed(`"2026-13-01T00:00:00Z"`)}},
		"dispatch clears retry": {{"merge.blocked", blocked("a", "b")}, {"merge.conflict_dispatch_failed", failed(`"2026-01-01T00:00:00Z"`)},
			{"merge.conflict_fix_dispatched", dispatched("fix-live")}},
		"retry after dispatch": {{"merge.blocked", blocked("a", "b")}, {"merge.conflict_fix_dispatched", dispatched("fix-live")},
			{"merge.conflict_dispatch_failed", failed(`"2026-01-01T00:00:00Z"`)}},
		"terminal orders across episodes": {{"merge.conflict_fix_dispatched", dispatched("fix-early")}, {"merge.blocked", blocked("a", "b")},
			{"merge.conflict_fix_dispatched", dispatched("fix-terminal")}, {"merge.conflict_cleared", `{}`}, {"merge.conflict_fix_dispatched", dispatched("fix-early")},
			{"merge.blocked", blocked("x", "b")}, {"merge.conflict_fix_dispatched", dispatched("fix-early")},
			{"merge.blocked", blocked("a", "b")}, {"merge.conflict_fix_dispatched", dispatched("fix-released")}, {"merge.conflict_fix_dispatched", `{"approved_head":"a","new_head":"b","work_order_id":5}`},
			{"merge.conflict_fix_dispatched", `{"approved_head":"a","new_head":"c","work_order_id":"fix-early"}`}, {"merge.conflict_dispatch_failed", failed(`"2026-01-01T00:00:00Z"`)}},
		"cleared latest": {{"merge.blocked", blocked("a", "b")}, {"merge.conflict_fix_dispatched", dispatched("fix-terminal")},
			{"merge.conflict_dispatch_failed", failed(`"2026-01-01T00:00:00Z"`)}, {"merge.conflict_dispatch_exhausted", blocked("a", "b")}, {"merge.conflict_cleared", `{}`}},
		"exhausted episode": {{"merge.blocked", blocked("a", "b")}, {"merge.conflict_dispatch_exhausted", blocked("a", "b")}, {"merge.conflict_recovery_blocked", blocked("a", "b")}},
		"other heads only":  {{"merge.blocked", blocked("a", "b")}, {"merge.conflict_dispatch_exhausted", blocked("a", "c")}, {"merge.conflict_dispatch_failed", `{"approved_head":"a","new_head":"c","next_retry_at":"2026-01-01T00:00:00Z"}`}},
		"lenient retry forms": {{"merge.blocked", blocked("a", "b")}, {"merge.conflict_dispatch_failed", failed(`"2026-01-01T1:00:00Z"`)},
			{"merge.conflict_dispatch_failed", failed(`"2026-01-01T00:00:00,5+24:60"`)}, {"merge.conflict_dispatch_failed", failed(`"0000-02-29T00:00:00.123456789012345678901234567890Z"`)}},
		"rejected retry forms": {{"merge.blocked", blocked("a", "b")}, {"merge.conflict_dispatch_failed", failed(`"2026-01-01T00:00:00Z"`)},
			{"merge.conflict_dispatch_failed", failed(`"1900-02-29T00:00:00Z"`)}, {"merge.conflict_dispatch_failed", failed(`"2026-01-01T00:00:00+25:00"`)},
			{"merge.conflict_dispatch_failed", failed(`"2026-01-01T00:00:00+24:61"`)}, {"merge.conflict_dispatch_failed", failed(`"2026-01-01T24:00:00Z"`)},
			{"merge.conflict_dispatch_failed", failed(`"` + strings.Repeat("9", 4096) + `"`)}},
		"repeated dispatches and failures": func() []step {
			var out []step
			for _, s := range repeatedEpisodeSteps(40) {
				out = append(out, step(s))
			}
			return out
		}(),
	}
	for name, steps := range cases {
		t.Run(name, func(t *testing.T) {
			var events []core.Event
			for i, s := range steps {
				// Pairs share a timestamp, so (at, id) order decides.
				events = append(events, core.Event{ID: int64(i + 1), TaskID: "task", Kind: s.kind, At: base.Add(time.Duration(i/2) * time.Second), Payload: json.RawMessage(s.payload)})
			}
			summary := store.SummarizeMergeConflict(events, orders)
			got, want := store.MergeConflictFromHistory(summary, orders), previousMergeConflictFold(events, orders)
			if !sameConflict(got, want) {
				t.Fatalf("summary %+v folds to %+v, previous fold %+v", summary, got, want)
			}
		})
	}
	// The fixtures reach every outcome the fold distinguishes.
	outcomes := map[string]bool{}
	for _, steps := range cases {
		var events []core.Event
		for i, s := range steps {
			events = append(events, core.Event{ID: int64(i + 1), Kind: s.kind, At: base, Payload: json.RawMessage(s.payload)})
		}
		result := previousMergeConflictFold(events, orders)
		outcomes[fmt.Sprintf("open=%t exhausted=%t retry=%t", result.Open, result.Exhausted, !result.NextRetryAt.IsZero())] = true
	}
	for _, want := range []string{"open=false exhausted=false retry=false", "open=true exhausted=true retry=true", "open=true exhausted=false retry=true", "open=true exhausted=false retry=false", "open=true exhausted=true retry=false"} {
		if !outcomes[want] {
			t.Fatalf("fixtures never reach %s: %v", want, outcomes)
		}
	}
}

// sameConflict compares merge-conflict results by instant, because each
// decode of an offset time builds its own fixed zone.
func sameConflict(a, b nextaction.MergeConflict) bool {
	return a.Open == b.Open && a.Exhausted == b.Exhausted && a.NextRetryAt.Equal(b.NextRetryAt)
}

// repeatedEpisodeSteps grows dispatches and accepted, null, missing, and large
// invalid failures inside one uncleared episode, ending on a dispatch and then
// one accepted failure.
func repeatedEpisodeSteps(rounds int) []struct{ kind, payload string } {
	steps := []struct{ kind, payload string }{{"merge.blocked", `{"approved_head":"a","new_head":"b"}`}}
	for i := 0; i < rounds; i++ {
		steps = append(steps,
			struct{ kind, payload string }{"merge.conflict_fix_dispatched", `{"approved_head":"a","new_head":"b","work_order_id":"fix-live"}`},
			struct{ kind, payload string }{"merge.conflict_dispatch_failed", fmt.Sprintf(`{"approved_head":"a","new_head":"b","next_retry_at":"2026-01-%02dT00:00:00Z"}`, i%28+1)},
			struct{ kind, payload string }{"merge.conflict_dispatch_failed", `{"approved_head":"a","new_head":"b","next_retry_at":"` + strings.Repeat("x", 2048) + `"}`},
			struct{ kind, payload string }{"merge.conflict_dispatch_failed", `{"approved_head":"a","new_head":"b","next_retry_at":null}`},
			struct{ kind, payload string }{"merge.conflict_dispatch_failed", `{"approved_head":"a","new_head":"b"}`},
		)
	}
	return append(steps,
		struct{ kind, payload string }{"merge.conflict_fix_dispatched", `{"approved_head":"a","new_head":"b","work_order_id":"fix-terminal"}`},
		struct{ kind, payload string }{"merge.conflict_dispatch_failed", `{"approved_head":"a","new_head":"b","next_retry_at":"2026-03-01T00:00:00.5+01:00"}`})
}
