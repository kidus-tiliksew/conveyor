package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
	"github.com/kidus-tiliksew/conveyor/internal/workorder"
)

// seedWorkOrder creates a queued order on a new running task and moves it to
// state through the lifecycle commands the store admits.
func seedWorkOrder(t *testing.T, ctx context.Context, st store.Store, taskID string, number int, state core.WorkOrderState) core.WorkOrder {
	t.Helper()
	if _, err := st.GetTask(ctx, taskID); err != nil {
		workspace, _ := store.WorkspaceFromContext(ctx)
		if err = st.CreateTask(ctx, core.Task{ID: taskID, Workspace: workspace, Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + taskID, State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
	}
	id := fmt.Sprintf("%s-implement-%d", taskID, number)
	if err := st.CreateJob(ctx, core.Job{ID: id, TaskID: taskID, Stage: core.StageImplement, State: core.JobPending}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: id, TaskID: taskID, JobID: id, Stage: core.StageImplement, CreatedAt: now, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	move := func(next core.WorkOrderState, command core.WorkOrderCommand) {
		t.Helper()
		current, err := st.GetWorkOrder(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		current.State = next
		if err = storetest.UpdateWorkOrder(ctx, st, current, command); err != nil {
			t.Fatalf("move %s to %s: %v", id, next, err)
		}
	}
	switch state {
	case core.WorkOrderQueued:
	case core.WorkOrderCancelled:
		move(core.WorkOrderCancelled, core.WorkOrderCmdCancel)
	case core.WorkOrderTimedOut:
		move(core.WorkOrderTimedOut, core.WorkOrderCmdTimeout)
	case core.WorkOrderStale:
		if _, err := taskops.ExecuteWorkOrder(ctx, st, taskID, core.WorkOrderCmdMarkStale, func(lease taskops.TaskLease) (int, error) {
			return st.ApplyWorkOrderClock(ctx, lease, taskID, now.Add(time.Hour+time.Second))
		}); err != nil {
			t.Fatal(err)
		}
	case core.WorkOrderClaimed, core.WorkOrderSubmitted:
		if _, err := storetest.ClaimWorkOrder(ctx, st, id, core.WorkOrderClaim{ClaimantID: "worker", WorkerID: "worker", SessionID: "session-" + id, ClientToken: "token-" + id, Lease: time.Hour, ExecutionTimeout: time.Hour}); err != nil {
			t.Fatal(err)
		}
		if state == core.WorkOrderSubmitted {
			move(core.WorkOrderSubmitted, core.WorkOrderCmdSubmitForReview)
		}
	default:
		t.Fatalf("unsupported seeded state %s", state)
	}
	order, err := st.GetWorkOrder(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if order.State != state {
		t.Fatalf("seeded %s state=%s, want %s", id, order.State, state)
	}
	return order
}

// workOrderListFixture seeds one live order in each live state and history
// tasks holding only submitted and cancelled orders.
func workOrderListFixture(t *testing.T, history int) (*taskListCountingStore, http.Handler, map[core.WorkOrderState]core.WorkOrder, []core.WorkOrder) {
	t.Helper()
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	memory := store.NewMemory()
	live := map[core.WorkOrderState]core.WorkOrder{}
	for _, state := range []core.WorkOrderState{core.WorkOrderQueued, core.WorkOrderClaimed, core.WorkOrderStale, core.WorkOrderTimedOut} {
		live[state] = seedWorkOrder(t, ctx, memory, "live-"+string(state), 1, state)
	}
	var terminal []core.WorkOrder
	for index := range history {
		taskID := fmt.Sprintf("history-%03d", index)
		terminal = append(terminal, seedWorkOrder(t, ctx, memory, taskID, 1, core.WorkOrderSubmitted))
		terminal = append(terminal, seedWorkOrder(t, ctx, memory, taskID, 2, core.WorkOrderCancelled))
	}
	counter := newTaskListCountingStore(memory)
	server := NewServer(counter)
	server.Workspace = "demo"
	server.WorkOrders = &workorder.Service{Store: counter}
	return counter, authenticatedMemoryHandler(server), live, terminal
}

func getWorkOrders(t *testing.T, handler http.Handler, target string) (*httptest.ResponseRecorder, []core.WorkOrder) {
	t.Helper()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, target, nil))
	if response.Code != http.StatusOK {
		t.Fatalf("%s status=%d body=%s", target, response.Code, response.Body.String())
	}
	var orders []core.WorkOrder
	if err := json.Unmarshal(response.Body.Bytes(), &orders); err != nil {
		t.Fatal(err)
	}
	return response, orders
}

func TestWorkOrderListReadsLiveOrdersOnly(t *testing.T) {
	listedStates := func(counter *taskListCountingStore, handler http.Handler) (map[string]int, map[string]core.WorkOrderState) {
		t.Helper()
		counter.reset()
		_, orders := getWorkOrders(t, handler, "/v1/work-orders")
		calls, _, _ := counter.snapshot()
		states := map[string]core.WorkOrderState{}
		for _, order := range orders {
			states[order.ID] = order.State
		}
		return calls, states
	}
	small, smallHandler, _, _ := workOrderListFixture(t, 3)
	large, largeHandler, live, terminal := workOrderListFixture(t, 40)
	smallCalls, _ := listedStates(small, smallHandler)
	largeCalls, states := listedStates(large, largeHandler)
	want := map[string]core.WorkOrderState{}
	for state, order := range live {
		want[order.ID] = state
	}
	if !reflect.DeepEqual(states, want) {
		t.Fatalf("listed=%v, want exactly the queued, claimed, stale, and timed-out orders %v", states, want)
	}
	for _, order := range terminal {
		if _, listed := states[order.ID]; listed {
			t.Fatalf("terminal order %s (%s) listed", order.ID, order.State)
		}
	}
	if !reflect.DeepEqual(smallCalls, largeCalls) {
		t.Fatalf("store calls grew with terminal history: 6 terminal=%v, 80 terminal=%v", smallCalls, largeCalls)
	}
	if largeCalls["ListWorkOrders"] != 0 || largeCalls["ListLiveWorkOrders"] != 1 || largeCalls["ListTaskWorkOrders"] != 0 {
		t.Fatalf("work-order list reads=%v, want one ListLiveWorkOrders and no whole-history read", largeCalls)
	}
	_, _, historyIDs := large.snapshot()
	for _, id := range historyIDs {
		if !slices.Contains([]string{"live-queued", "live-claimed", "live-stale", "live-timed_out"}, id) {
			t.Fatalf("projection read history of task %s, which has no live order", id)
		}
	}
}

func TestWorkOrderExactRead(t *testing.T) {
	counter, handler, live, terminal := workOrderListFixture(t, 1)
	read := func(id string) (*httptest.ResponseRecorder, core.WorkOrder) {
		t.Helper()
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/work-orders/"+id, nil))
		var order core.WorkOrder
		if response.Code == http.StatusOK {
			if err := json.Unmarshal(response.Body.Bytes(), &order); err != nil {
				t.Fatal(err)
			}
		}
		return response, order
	}
	for _, want := range terminal {
		response, got := read(want.ID)
		if response.Code != http.StatusOK || got.ID != want.ID || got.TaskID != want.TaskID || got.State != want.State {
			t.Fatalf("exact read of %s order: status=%d order=%+v", want.State, response.Code, got)
		}
	}
	// The list's projections apply: another principal's session is stripped,
	// exactly as the list shows the same order.
	claimed := live[core.WorkOrderClaimed]
	response, got := read(claimed.ID)
	if response.Code != http.StatusOK || got.State != core.WorkOrderClaimed || got.SessionID != "" || claimed.SessionID == "" {
		t.Fatalf("claimed exact read status=%d session=%q stored session=%q", response.Code, got.SessionID, claimed.SessionID)
	}
	_, listed := getWorkOrders(t, handler, "/v1/work-orders")
	for _, order := range listed {
		if order.ID != claimed.ID {
			continue
		}
		listedJSON, _ := json.Marshal(order)
		readJSON, _ := json.Marshal(got)
		if string(listedJSON) != string(readJSON) {
			t.Fatalf("exact read=%s, list=%s", readJSON, listedJSON)
		}
	}

	foreignCtx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "other")
	foreign := seedWorkOrder(t, foreignCtx, counter.Store, "foreign-task", 1, core.WorkOrderQueued)
	for _, missing := range []string{"absent-implement-1", foreign.ID} {
		if response, _ := read(missing); response.Code != http.StatusNotFound {
			t.Fatalf("exact read of %s status=%d, want 404", missing, response.Code)
		}
	}
}
