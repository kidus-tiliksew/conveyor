package httpapi

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
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

// attentionPollComposition names the reads a poll composes its body from. A
// 304 must perform none of them (component-http-api, Activity conditional
// admission).
func assertNoComposition(t *testing.T, before, after map[string]int) {
	t.Helper()
	for name, count := range after {
		// The validation read is the tag's own input, so every poll makes it.
		if name == "ReadActivityValidation" {
			continue
		}
		if count != before[name] {
			t.Fatalf("a 304 poll performed %s (%d -> %d)", name, before[name], count)
		}
	}
}

// TestTaskObservationPollAnswersUnchangedWith304 proves the narrow wait poll
// answers an unchanged task with 304 before any composition, and a changed task
// with 200 and a new tag (req-agent-skills AC-4.3; component-http-api, Activity
// conditional admission).
func TestTaskObservationPollAnswersUnchangedWith304(t *testing.T) {
	f := newActivityConditionalFixture(t)
	f.task("task-obs", core.TaskAwaiting)
	path := "/v1/tasks/task-obs/observation?workspace_id=demo"

	cold := f.read(path, "")
	if cold.Code != http.StatusOK {
		t.Fatalf("first poll status=%d body=%s", cold.Code, cold.Body.String())
	}
	etag := cold.Header().Get("ETag")
	if etag == "" {
		t.Fatal("the first poll states no ETag")
	}
	for _, want := range []string{`"task_id":"task-obs"`, `"state":"awaiting_human"`, `"merge_approval":false`, `"claimable_orders":[]`} {
		if !strings.Contains(cold.Body.String(), want) {
			t.Fatalf("observation body=%s lacks %s", cold.Body.String(), want)
		}
	}
	if f.counted.calls["ListWorkOrders"] != 0 || f.counted.calls["ListWorkOrdersForTasks"] != 0 {
		t.Fatalf("the observation poll read a workspace order listing: %v", f.counted.calls)
	}

	before := maps.Clone(f.counted.calls)
	warm := f.read(path, etag)
	if warm.Code != http.StatusNotModified {
		t.Fatalf("unchanged poll status=%d body=%s, want 304", warm.Code, warm.Body.String())
	}
	if warm.Body.Len() != 0 {
		t.Fatalf("304 body=%q, want empty", warm.Body.String())
	}
	assertNoComposition(t, before, f.counted.calls)

	// A new claimable order changes the observation, so the next poll answers
	// 200 with a tag that no longer admits the old one.
	f.order(core.WorkOrder{ID: "task-obs-implement-1", TaskID: "task-obs", JobID: "job-obs", Stage: core.StageImplement, State: core.WorkOrderQueued, Claimable: true})
	changed := f.read(path, etag)
	if changed.Code != http.StatusOK {
		t.Fatalf("changed poll status=%d body=%s, want 200", changed.Code, changed.Body.String())
	}
	if changed.Header().Get("ETag") == etag {
		t.Fatal("the changed observation kept the old tag")
	}
	if !strings.Contains(changed.Body.String(), `"id":"task-obs-implement-1"`) {
		t.Fatalf("changed observation=%s lacks the claimable order", changed.Body.String())
	}
}

func TestTaskObservationAndNextActionValidatorsAreRouteScoped(t *testing.T) {
	f := newActivityConditionalFixture(t)
	f.task("task-scope", core.TaskRunning)
	observation := "/v1/tasks/task-scope/observation?workspace_id=demo"
	nextAction := "/v1/tasks/task-scope/next-action?workspace_id=demo"
	for _, pair := range [][2]string{{observation, nextAction}, {nextAction, observation}} {
		first := f.read(pair[0], "")
		if first.Code != http.StatusOK || first.Header().Get("ETag") == "" {
			t.Fatalf("first read: status=%d tag=%q", first.Code, first.Header().Get("ETag"))
		}
		second := f.read(pair[1], first.Header().Get("ETag"))
		if second.Code != http.StatusOK {
			t.Fatalf("cross-route read: status=%d body=%s", second.Code, second.Body.String())
		}
	}
}

// TestNextActionPollAnswersUnchangedWith304 proves the next-action read keeps
// its body while an unchanged poll costs no projection, and a changed task
// answers 200 with a new tag (req-agent-skills AC-4.3).
func TestNextActionPollAnswersUnchangedWith304(t *testing.T) {
	f := newActivityConditionalFixture(t)
	f.task("task-next", core.TaskRunning)
	path := "/v1/tasks/task-next/next-action?workspace_id=demo"

	cold := f.read(path, "")
	if cold.Code != http.StatusOK {
		t.Fatalf("first read status=%d body=%s", cold.Code, cold.Body.String())
	}
	etag := cold.Header().Get("ETag")
	if etag == "" {
		t.Fatal("the first read states no ETag")
	}
	if !strings.Contains(cold.Body.String(), `"actor"`) {
		t.Fatalf("next action body=%s", cold.Body.String())
	}

	before := maps.Clone(f.counted.calls)
	warm := f.read(path, etag)
	if warm.Code != http.StatusNotModified {
		t.Fatalf("unchanged read status=%d body=%s, want 304", warm.Code, warm.Body.String())
	}
	assertNoComposition(t, before, f.counted.calls)

	f.order(core.WorkOrder{ID: "task-next-implement-1", TaskID: "task-next", JobID: "job-next", Stage: core.StageImplement, State: core.WorkOrderQueued, Claimable: true})
	changed := f.read(path, etag)
	if changed.Code != http.StatusOK {
		t.Fatalf("changed read status=%d body=%s, want 200", changed.Code, changed.Body.String())
	}
	if changed.Header().Get("ETag") == etag {
		t.Fatal("the changed next action kept the old tag")
	}
}

// ListLiveWorkOrders counts the workspace live-order listing, so a test can
// prove the observation never reads it.
func (s *activityConditionalStore) ListLiveWorkOrders(ctx context.Context) ([]core.WorkOrder, error) {
	s.count("ListLiveWorkOrders")
	return s.Store.ListLiveWorkOrders(ctx)
}

// taskObservationClaimableOrders ranks spec, implement, verify, review, then
// risk, breaks a stage tie by queue entry and then by ID, and leaves out an
// unknown stage and orders that are not queued or not claimable. A risk order
// ranks last beside ordinary work and is reported when it stands alone. The
// list is empty, never null, when nothing is claimable
// (component-cli-onboarding invariant 22).
func TestTaskObservationClaimableOrdersRankAndExclusions(t *testing.T) {
	base := time.Date(2026, 10, 4, 8, 0, 0, 0, time.UTC)
	at := func(minutes int) time.Time { return base.Add(time.Duration(minutes) * time.Minute) }
	order := func(id string, stage core.Stage, entered time.Time) core.WorkOrder {
		return core.WorkOrder{ID: id, Stage: stage, State: core.WorkOrderQueued, Claimable: true, QueueEnteredAt: entered}
	}
	claimed := order("verify-claimed", core.StageVerify, at(0))
	claimed.State, claimed.Claimable = core.WorkOrderClaimed, false
	completed := order("review-done", core.StageReview, at(0))
	completed.State = core.WorkOrderCompleted
	unclaimable := order("spec-held", core.StageSpec, at(0))
	unclaimable.Claimable = false
	for _, tc := range []struct {
		name   string
		orders []core.WorkOrder
		want   []string
	}{
		{"empty", nil, []string{}},
		{"lone risk", []core.WorkOrder{order("risk", core.StageRisk, at(0))}, []string{"risk"}},
		{"older risk beside implementation", []core.WorkOrder{order("risk", core.StageRisk, at(0)), order("implement", core.StageImplement, at(5))}, []string{"implement", "risk"}},
		{"older risk beside review", []core.WorkOrder{order("risk", core.StageRisk, at(0)), order("review", core.StageReview, at(5))}, []string{"review", "risk"}},
		{"older risk beside verification", []core.WorkOrder{order("risk", core.StageRisk, at(0)), order("verify", core.StageVerify, at(5))}, []string{"verify", "risk"}},
		{"older risk beside plan", []core.WorkOrder{order("risk", core.StageRisk, at(0)), order("spec", core.StageSpec, at(5))}, []string{"spec", "risk"}},
		{"two risk orders take the oldest", []core.WorkOrder{order("risk-2", core.StageRisk, at(5)), order("risk-1", core.StageRisk, at(0))}, []string{"risk-1", "risk-2"}},
		{"stage precedence over queue entry", []core.WorkOrder{order("review", core.StageReview, at(0)), order("verify", core.StageVerify, at(1)), order("implement", core.StageImplement, at(2)), order("spec", core.StageSpec, at(3))}, []string{"spec", "implement", "verify", "review"}},
		{"queue entry then ID", []core.WorkOrder{order("c-implement", core.StageImplement, at(1)), order("b-implement", core.StageImplement, at(0)), order("a-implement", core.StageImplement, at(1))}, []string{"b-implement", "a-implement", "c-implement"}},
		{"exclusions", []core.WorkOrder{claimed, completed, unclaimable, order("deploy", core.Stage("deploy"), at(0))}, []string{}},
		{"full rank", []core.WorkOrder{
			order("risk-1", core.StageRisk, at(0)),
			order("review-new", core.StageReview, at(4)),
			order("review-old", core.StageReview, at(1)),
			order("implement-b", core.StageImplement, at(2)),
			order("implement-a", core.StageImplement, at(2)),
			order("verify-1", core.StageVerify, at(3)),
			order("spec-1", core.StageSpec, at(9)),
			order("deploy-1", core.Stage("deploy"), at(0)),
			claimed, completed, unclaimable,
		}, []string{"spec-1", "implement-a", "implement-b", "verify-1", "review-old", "review-new", "risk-1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := taskObservationClaimableOrders(tc.orders)
			if got == nil {
				t.Fatal("claimable orders = nil, want a list")
			}
			ids := make([]string, 0, len(got))
			for _, entry := range got {
				ids = append(ids, entry.ID)
			}
			if !slices.Equal(ids, tc.want) {
				t.Fatalf("claimable orders = %v, want %v", ids, tc.want)
			}
		})
	}
}

// TestTaskObservationAppliesWorkOrderProjectionsWithoutWorkspaceListing
// serves the observation through a configured work-order service. A recovery
// refused order and a dependency blocked order drop out, the claimable orders
// come in server rank, and no poll reads a workspace order listing
// (component-next-action invariant 57; req-agent-skills AC-4.3).
func TestTaskObservationAppliesWorkOrderProjectionsWithoutWorkspaceListing(t *testing.T) {
	f := newActivityConditionalFixture(t)
	f.server.WorkOrders = &workorder.Service{Store: f.counted}
	created := f.now.Add(-30 * time.Minute)
	f.task("task-ranked", core.TaskRunning)
	// The review order is created first, so creation order is the reverse of
	// rank order.
	f.order(core.WorkOrder{ID: "task-ranked-review-1", TaskID: "task-ranked", JobID: "job-ranked-review", Stage: core.StageReview, CreatedAt: created, QueueEnteredAt: created, QueueDeadline: created.Add(time.Hour)})
	f.order(core.WorkOrder{ID: "task-ranked-implement-2", TaskID: "task-ranked", JobID: "job-ranked-implement-2", Stage: core.StageImplement, CreatedAt: created.Add(2 * time.Second), QueueEnteredAt: created.Add(2 * time.Second), QueueDeadline: created.Add(time.Hour)})
	// An older implementation order created after its successor is refused
	// by recovery while its stored row stays claimable.
	f.order(core.WorkOrder{ID: "task-ranked-implement-1", TaskID: "task-ranked", JobID: "job-ranked-implement-1", Stage: core.StageImplement, CreatedAt: created.Add(time.Second), QueueEnteredAt: created.Add(time.Second), QueueDeadline: created.Add(time.Hour)})

	f.counted.reset()
	response := f.read("/v1/tasks/task-ranked/observation?workspace_id=demo", "")
	if response.Code != http.StatusOK {
		t.Fatalf("observation status=%d body=%s", response.Code, response.Body.String())
	}
	var observation taskObservation
	if err := json.Unmarshal(response.Body.Bytes(), &observation); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, order := range observation.ClaimableOrders {
		ids = append(ids, order.ID)
	}
	if want := []string{"task-ranked-implement-2", "task-ranked-review-1"}; !slices.Equal(ids, want) {
		t.Fatalf("claimable orders = %v, want %v", ids, want)
	}
	for _, listing := range []string{"ListWorkOrders", "ListLiveWorkOrders", "ListWorkOrdersForTasks"} {
		if f.counted.calls[listing] != 0 {
			t.Fatalf("the observation read the workspace order listing %s: %v", listing, f.counted.calls)
		}
	}
}

// TestTaskObservationFailsWhenAProjectionReadFails proves a projection
// failure fails the observation instead of reporting the stored
// claimability (component-next-action invariant 57; DEC-105(1)).
func TestTaskObservationFailsWhenAProjectionReadFails(t *testing.T) {
	f := newActivityConditionalFixture(t)
	failing := &failingBlockerStore{activityConditionalStore: f.counted}
	f.server.WorkOrders = &workorder.Service{Store: failing}
	f.task("task-failing", core.TaskRunning)
	f.order(core.WorkOrder{ID: "task-failing-implement-1", TaskID: "task-failing", JobID: "job-failing", Stage: core.StageImplement, State: core.WorkOrderQueued, Claimable: true})
	response := f.read("/v1/tasks/task-failing/observation?workspace_id=demo", "")
	if response.Code != http.StatusInternalServerError || strings.Contains(response.Body.String(), "task-failing-implement-1") {
		t.Fatalf("observation status=%d body=%s, want 500 without the order", response.Code, response.Body.String())
	}
}

// failingBlockerStore fails the dependency blocker read that the work-order
// projection makes for a queued implementation order.
type failingBlockerStore struct {
	*activityConditionalStore
}

func (s *failingBlockerStore) ListDependencyBlockers(context.Context, []string) (map[string]store.DependencyBlockers, error) {
	return nil, store.ErrBackendOperation
}

// assigneeFreeOrderStore answers the task order read without the task
// assignee, as the PostgreSQL and SingleStore task order reads do.
type assigneeFreeOrderStore struct {
	store.Backend
}

func (s assigneeFreeOrderStore) ListTaskWorkOrders(ctx context.Context, taskID string) ([]core.WorkOrder, error) {
	orders, err := s.Backend.ListTaskWorkOrders(ctx, taskID)
	for i := range orders {
		orders[i].Assignee = nil
	}
	return orders, err
}

// TestTaskObservationTakesTheAssigneeFromTheTaskRead proves an order of a
// task assigned to another workspace member is not claimable for the caller,
// even when the task order read carries no assignee, and is claimable again
// once the assignment is cleared (component-next-action invariant 57).
func TestTaskObservationTakesTheAssigneeFromTheTaskRead(t *testing.T) {
	backend := store.NewVolatileBackend()
	t.Cleanup(backend.Close)
	ctx := store.WithActor(store.WithWorkspace(t.Context(), "demo"), store.SystemActor())
	if _, err := backend.BootstrapWorkspaceConfig(ctx, &config.Config{Workspace: "demo", Repos: []config.Repo{{Name: "conveyor", URL: "https://example.test/conveyor", Base: "main"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.BootstrapIdentity(ctx, config.FirstOperatorIdentity{OrganizationName: "Observation", Email: "owner@example.test", DisplayName: "Owner"}, "observation-bootstrap"); err != nil {
		t.Fatal(err)
	}
	owner, err := backend.VerifyPersonalAccessToken(ctx, "observation-bootstrap")
	if err != nil {
		t.Fatal(err)
	}
	ownerCtx := store.WithActor(store.WithCredential(ctx, core.AuthenticatedCredential{ID: "bootstrap", OwnerUserID: owner.ID, Kind: core.CredentialUser, Scope: core.CredentialScopeOperator}), store.Actor{ID: store.UserActorID(owner.ID), Role: core.ActorUser})
	if _, err = backend.GrantWorkspaceRole(ownerCtx, owner.Email, "demo", core.WorkspaceRoleOperator); err != nil {
		t.Fatal(err)
	}
	other, err := backend.ProvisionIdentityUser(ownerCtx, "other@example.test", "Other")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = backend.GrantWorkspaceRole(ownerCtx, other.Email, "demo", core.WorkspaceRoleExecutor); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err = backend.CreateTask(ctx, core.Task{ID: "task-assigned", Workspace: "demo", Repo: "conveyor", Branch: "conveyor/task-assigned", Title: "Assigned", State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err = backend.CreateJob(ctx, core.Job{ID: "task-assigned-implement-1", TaskID: "task-assigned", Stage: core.StageImplement, State: core.JobPending}); err != nil {
		t.Fatal(err)
	}
	if err = storetest.For(backend).CreateWorkOrder(ctx, core.WorkOrder{ID: "task-assigned-implement-1", JobID: "task-assigned-implement-1", TaskID: "task-assigned", Stage: core.StageImplement, CreatedAt: now, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	server := NewServer(assigneeFreeOrderStore{backend})
	server.Credentials = nil
	server.Workspace, server.BearerToken = "demo", "observation-token"
	server.WorkOrders = &workorder.Service{Store: backend}
	handler := server.Handler()
	read := func() string {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, "/v1/tasks/task-assigned/observation", nil)
		request.Header.Set("Authorization", "Bearer observation-token")
		request.Header.Set("X-Workspace-ID", "demo")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("observation status=%d body=%s", response.Code, response.Body.String())
		}
		return response.Body.String()
	}
	if _, err = taskops.New(backend).SetAssignee(ownerCtx, "task-assigned", other.ID); err != nil {
		t.Fatal(err)
	}
	if body := read(); !strings.Contains(body, `"claimable_orders":[]`) {
		t.Fatalf("observation of a task assigned to another user=%s, want no claimable order", body)
	}
	if _, err = taskops.New(backend).SetAssignee(ownerCtx, "task-assigned", ""); err != nil {
		t.Fatal(err)
	}
	if body := read(); !strings.Contains(body, `"id":"task-assigned-implement-1"`) {
		t.Fatalf("observation of an unassigned task=%s lacks the order", body)
	}
}
