package storetest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// runTaskObservationClaimability proves on every backend that the wait
// observation's claimable_orders lists exactly the queued, known-stage orders
// of the task that GET /v1/work-orders reports claimable to the same caller,
// ranked spec, implement, verify, review, then risk. Each case reaches its
// projection through store commands: a dependency added after the order was
// queued, a task assigned to another workspace member, an older same-stage
// order that recovery refuses, and two claimable orders created in reverse
// rank order. Removing the dependency and clearing the assignment return the
// order to the observation (component-next-action invariant 57;
// component-cli-onboarding invariant 22).
func runTaskObservationClaimability(t *testing.T, x Fixture) {
	if x.WorkOrderHTTP == nil {
		t.Fatal("conformance fixture must provide WorkOrderHTTP for the task observation claimability case")
	}
	handler := x.WorkOrderHTTP(t)
	st, ctx := x.Backend, x.Context
	now := time.Now().UTC().Truncate(time.Microsecond)
	newTask := func(t *testing.T, label string) string {
		t.Helper()
		id := core.NewTaskID()
		requireOK(t, st.CreateTask(ctx, core.Task{
			ID: id, Workspace: x.Workspace, Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + id,
			Title: "Observation " + label, State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: now.Add(-time.Hour),
		}))
		return id
	}
	newOrder := func(t *testing.T, taskID string, stage core.Stage, number int, created time.Time) string {
		t.Helper()
		id := taskID + "-" + string(stage) + "-" + string(rune('0'+number))
		requireOK(t, st.CreateJob(ctx, core.Job{ID: id, TaskID: taskID, Stage: stage, State: core.JobPending}))
		requireOK(t, CreateWorkOrder(ctx, st, core.WorkOrder{
			ID: id, JobID: id, TaskID: taskID, Stage: stage,
			CreatedAt: created, QueueEnteredAt: created, QueueDeadline: created.Add(24 * time.Hour),
		}))
		return id
	}
	// storedClaimable reads the order through the store's own task read, so
	// a case proves that the projection, not the stored row, refuses it.
	storedClaimable := func(t *testing.T, taskID, orderID string) bool {
		t.Helper()
		orders, err := st.ListTaskWorkOrders(ctx, taskID)
		requireOK(t, err)
		for _, order := range orders {
			if order.ID == orderID {
				return order.State == core.WorkOrderQueued && order.Claimable
			}
		}
		t.Fatalf("task %s has no order %s", taskID, orderID)
		return false
	}

	t.Run("dependency blocked implementation", func(t *testing.T) {
		blocker := newTask(t, "blocker")
		dependent := newTask(t, "dependent")
		order := newOrder(t, dependent, core.StageImplement, 1, now)
		operator := store.WithActor(ctx, store.Actor{ID: "operator-observation", Role: core.ActorHuman})
		_, err := st.AddTaskDependency(operator, store.DependencyAdditionRequest{TaskID: dependent, DependsOnTaskID: blocker, Reason: "deliver the blocker first", RequestID: "observation-" + dependent})
		requireOK(t, err)
		listed := requireObservationParity(t, handler, x.Workspace, dependent, nil)
		if got := listed[order]; got.Claimable || !slices.Equal(got.BlockingTaskIDs, []string{blocker}) {
			t.Fatalf("listed blocked order=%+v, want unclaimable behind %s", got, blocker)
		}
		// Removing the dependency releases the order, and the next poll
		// reports it.
		_, err = st.RemoveTaskDependency(operator, store.DependencyRemovalRequest{TaskID: dependent, DependsOnTaskID: blocker, Reason: "the blocker is not needed", RequestID: "observation-remove-" + dependent})
		requireOK(t, err)
		requireObservationParity(t, handler, x.Workspace, dependent, []string{order})
	})

	t.Run("assigned to another user", func(t *testing.T) {
		owner, _ := bootstrapOwner(t, x)
		email := "observation-" + strings.ToLower(core.NewTaskID()) + "@example.test"
		other, err := st.ProvisionIdentityUser(owner, email, "Observation Assignee")
		requireOK(t, err)
		_, err = st.GrantWorkspaceRole(owner, email, x.Workspace, core.WorkspaceRoleExecutor)
		requireOK(t, err)
		taskID := newTask(t, "assigned")
		order := newOrder(t, taskID, core.StageImplement, 1, now)
		_, err = taskops.New(st).SetAssignee(owner, taskID, other.ID)
		requireOK(t, err)
		if !storedClaimable(t, taskID, order) {
			t.Fatal("the assigned order is stored unclaimable, so the assignee projection is untested")
		}
		listed := requireObservationParity(t, handler, x.Workspace, taskID, nil)
		if got := listed[order]; got.Claimable || !strings.Contains(got.ClaimRefusalReason, other.ID) {
			t.Fatalf("listed assigned order=%+v, want unclaimable for the caller", got)
		}
		// Clearing the assignment makes the order claimable for the caller
		// again, and the next poll reports it.
		_, err = taskops.New(st).SetAssignee(owner, taskID, "")
		requireOK(t, err)
		requireObservationParity(t, handler, x.Workspace, taskID, []string{order})
	})

	t.Run("recovery refused older order", func(t *testing.T) {
		taskID := newTask(t, "superseded")
		// The successor is written first, because creating an order retires
		// its older open siblings.
		newer := newOrder(t, taskID, core.StageImplement, 2, now.Add(time.Second))
		older := newOrder(t, taskID, core.StageImplement, 1, now)
		if !storedClaimable(t, taskID, older) {
			t.Fatal("the superseded order is stored unclaimable, so the recovery projection is untested")
		}
		listed := requireObservationParity(t, handler, x.Workspace, taskID, []string{newer})
		if got := listed[older]; got.Claimable || !strings.Contains(got.RecoveryRefusal, "superseded by same-stage successor "+newer) {
			t.Fatalf("listed superseded order=%+v, want the recovery refusal", got)
		}
	})

	t.Run("stages created in reverse rank order", func(t *testing.T) {
		taskID := newTask(t, "ranked")
		review := newOrder(t, taskID, core.StageReview, 1, now)
		implement := newOrder(t, taskID, core.StageImplement, 1, now.Add(time.Second))
		requireObservationParity(t, handler, x.Workspace, taskID, []string{implement, review})
	})
}

// observationListedOrder is the part of a GET /v1/work-orders entry the
// claimability comparison reads.
type observationListedOrder struct {
	ID                 string              `json:"id"`
	TaskID             string              `json:"task_id"`
	Stage              core.Stage          `json:"stage"`
	State              core.WorkOrderState `json:"state"`
	Claimable          bool                `json:"claimable"`
	ClaimRefusalReason string              `json:"claim_refusal_reason"`
	RecoveryRefusal    string              `json:"recovery_refusal"`
	BlockingTaskIDs    []string            `json:"blocking_task_ids"`
}

// requireObservationParity reads both routes as the same caller. It requires
// the observation's claimable orders to hold the same members as the
// listing's queued, claimable, known-stage orders of the task, and to come in
// the order want names. It returns the listing's orders of the task by ID.
func requireObservationParity(t *testing.T, handler http.Handler, workspace, taskID string, want []string) map[string]observationListedOrder {
	t.Helper()
	get := func(path string, into any) {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Header.Set("Authorization", "Bearer "+LineageHTTPToken)
		request.Header.Set("X-Workspace-ID", workspace)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("GET %s status=%d body=%s", path, response.Code, response.Body.String())
		}
		if err := json.Unmarshal(response.Body.Bytes(), into); err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
	}
	var listing []observationListedOrder
	get("/v1/work-orders", &listing)
	var observation struct {
		ClaimableOrders []struct {
			ID    string     `json:"id"`
			Stage core.Stage `json:"stage"`
		} `json:"claimable_orders"`
	}
	get("/v1/tasks/"+taskID+"/observation", &observation)
	if observation.ClaimableOrders == nil {
		t.Fatal("the observation's claimable_orders is null, want a list")
	}
	listed := map[string]observationListedOrder{}
	var claimable []string
	for _, order := range listing {
		if order.TaskID != taskID {
			continue
		}
		listed[order.ID] = order
		if order.State == core.WorkOrderQueued && order.Claimable && core.ValidWorkOrderStage(order.Stage) {
			claimable = append(claimable, order.ID)
		}
	}
	observed := make([]string, 0, len(observation.ClaimableOrders))
	for _, order := range observation.ClaimableOrders {
		observed = append(observed, order.ID)
		if listed[order.ID].Stage != order.Stage {
			t.Fatalf("observation order %s stage=%s, listing stage=%s", order.ID, order.Stage, listed[order.ID].Stage)
		}
	}
	members := slices.Clone(observed)
	slices.Sort(members)
	slices.Sort(claimable)
	if !slices.Equal(members, claimable) {
		t.Fatalf("observation claimable orders=%v, work-order listing claimable orders=%v", observed, claimable)
	}
	if !slices.Equal(observed, want) && (len(observed) != 0 || len(want) != 0) {
		t.Fatalf("observation claimable orders=%v, want %v in rank order", observed, want)
	}
	return listed
}
