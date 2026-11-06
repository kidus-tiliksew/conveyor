package httpapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/dispatch"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"github.com/kidus-tiliksew/conveyor/internal/trigger/github"
	"github.com/kidus-tiliksew/conveyor/internal/workorder"
)

// The tests in this file walk DEC-105 over the task and gate routes
// (component-http-api invariants 15 to 17): a store failure answers the logged
// generic 500 with no error text, a refusal keeps its 4xx, a forge failure keeps
// its remedy, and an oversized body answers 413.

const (
	failureTask      = "failure-task"
	failureGateTask  = "failure-gate"
	failureOrder     = "failure-order"
	failureOtherTask = "failure-other-workspace"
)

// failureClassesFixture seeds a running task with a queued order, a task at the
// merge gate, and a task in another workspace.
func failureClassesFixture(t *testing.T) (*Server, store.Store) {
	t.Helper()
	base := store.NewMemory()
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	now := time.Now().UTC().Truncate(time.Second)
	for _, task := range []core.Task{
		{ID: failureTask, Workspace: "demo", Repo: "conveyor", Branch: failureTask, State: core.TaskParked, NextStage: core.StageImplement, RecoveryStage: core.StageImplement, CreatedAt: now},
		{ID: failureGateTask, Workspace: "demo", Repo: "conveyor", Branch: failureGateTask, State: core.TaskAwaiting, GateKind: core.GateKindPlanApproval, RecoveryStage: core.StageImplement, CreatedAt: now},
	} {
		if err := base.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
	}
	otherCtx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "other")
	if err := base.CreateTask(otherCtx, core.Task{ID: failureOtherTask, Workspace: "other", Repo: "conveyor", Branch: failureOtherTask, State: core.TaskRunning, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := base.CreateJob(ctx, core.Job{ID: failureOrder, TaskID: failureTask, Stage: core.StageImplement, State: core.JobPending}); err != nil {
		t.Fatal(err)
	}
	if err := storetest.For(base).CreateWorkOrder(ctx, core.WorkOrder{ID: failureOrder, JobID: failureOrder, TaskID: failureTask, Stage: core.StageImplement, State: core.WorkOrderQueued, CreatedAt: now, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	server := NewServer(base)
	server.BearerToken, server.Workspace = "token", "demo"
	server.Credentials = staticCredentialVerifier{"token": {ID: "pat_1", OwnerUserID: "usr_owner", Kind: core.CredentialUser, Scope: core.CredentialScopeOperator}}
	server.WorkOrders = &workorder.Service{Store: base, ConfigProvider: failureClassesConfig}
	return server, base
}

// failureClassesConfig is the work-order service's configuration, so a recovery
// reaches its first store read.
func failureClassesConfig(context.Context) (*config.Config, error) {
	return &config.Config{Workspace: "demo", WorkOrderQueueTimeout: time.Hour}, nil
}

// failWith points the server and its work-order service at failing.
func failWith(server *Server, failing store.Store) {
	server.Store = failing
	server.WorkOrders = &workorder.Service{Store: failing, ConfigProvider: failureClassesConfig}
}

func failureRequest(t *testing.T, server *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	separator := "?"
	if strings.Contains(path, "?") {
		separator = "&"
	}
	request := httptest.NewRequest(method, path+separator+"workspace_id=demo", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer token")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Workspace-ID", "demo")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	return response
}

// requireGenericFailure asserts the DEC-105(1) answer and its log line.
func requireGenericFailure(t *testing.T, response *httptest.ResponseRecorder, captured *lockedLogBuffer) {
	t.Helper()
	if response.Code != http.StatusInternalServerError || strings.TrimSpace(response.Body.String()) != "internal server error" {
		t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
	}
	logged := captured.String()
	if !strings.Contains(logged, "httpapi: store failure: request_id=") || strings.Contains(logged, `request_id=""`) || !strings.Contains(logged, "does not exist") {
		t.Fatalf("log = %q", logged)
	}
	if strings.Contains(logged, failingStoreCredential) {
		t.Fatalf("log carries the credential: %q", logged)
	}
}

type failureRoute struct {
	name, method, path, body string
	// failing names the store methods that fail. hook wires a dependency that is
	// not the store, and prepare seeds anything else the route reads first.
	failing []string
	hook    func(*Server)
	prepare func(*testing.T, store.Store)
}

func failingOperationRoutes() []failureRoute {
	storeFailure := errors.New(failingStoreDriverText)
	cleanup := `{"repository":"conveyor","branch":"failure-task","worktree":"removed","branch_result":"retained","path":"/tmp/x"}`
	prepareCleanup := func(t *testing.T, st store.Store) {
		t.Helper()
		ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
		if err := st.AppendEvent(ctx, core.Event{TaskID: failureTask, Kind: "work_order.claimed", ActorID: store.UserActorID("usr_owner"), ActorRole: core.ActorUser}); err != nil {
			t.Fatal(err)
		}
	}
	return []failureRoute{
		// Item 1: any GetTask failure read as 404.
		{name: "redispatchTask read", method: http.MethodPost, path: "/v1/tasks/failure-task/redispatch", failing: []string{"GetTask"}},
		{name: "closeTask read", method: http.MethodPost, path: "/v1/tasks/failure-task/close", body: `{"reason":"stop"}`, failing: []string{"GetTask"}},
		{name: "reviewTask read", method: http.MethodPost, path: "/v1/tasks/failure-task/review", body: `{}`, failing: []string{"GetTask"}},
		{name: "mergeTask read", method: http.MethodPost, path: "/v1/tasks/failure-task/merge", failing: []string{"GetTask"}},
		{name: "fixMergeConflict read", method: http.MethodPost, path: "/v1/tasks/failure-task/merge-conflict-fix", failing: []string{"GetTask"}},
		{name: "getTask", method: http.MethodGet, path: "/v1/tasks/failure-task", failing: []string{"GetTask"}},
		{name: "streamEvents", method: http.MethodGet, path: "/v1/tasks/failure-task/events/stream", failing: []string{"GetTask"}},
		{name: "getTaskActivity", method: http.MethodGet, path: "/v1/tasks/failure-task/activity", failing: []string{"GetTask"}},
		{name: "requestTaskChanges read", method: http.MethodPost, path: "/v1/tasks/failure-task/request-changes", body: `{"feedback":"fix"}`, failing: []string{"GetTask"}},
		{name: "getTaskAudit read", method: http.MethodGet, path: "/v1/tasks/failure-task/audit/event/1", failing: []string{"GetTask"}},
		{name: "getTaskAudit work order", method: http.MethodGet, path: "/v1/tasks/failure-task/audit/work-order/failure-order", failing: []string{"GetWorkOrder"}},
		{name: "getTaskHistory read", method: http.MethodGet, path: "/v1/tasks/failure-task/history", failing: []string{"GetTask"}},
		// Item 2: any failure read as another 4xx.
		{name: "setTaskAssignee", method: http.MethodPut, path: "/v1/tasks/failure-task/assignee", body: `{"assignee_user_id":"usr_x"}`, failing: []string{"SetTaskAssigneeCommand"}},
		{name: "addTaskDependency", method: http.MethodPost, path: "/v1/tasks/failure-task/dependencies", body: `{"depends_on_task_id":"failure-gate","reason":"r","request_id":"q"}`, failing: []string{"AddTaskDependency"}},
		{name: "removeTaskDependency", method: http.MethodDelete, path: "/v1/tasks/failure-task/dependencies/failure-gate", body: `{"reason":"r","request_id":"q"}`, failing: []string{"RemoveTaskDependency"}},
		{name: "preemptWorkOrder", method: http.MethodPost, path: "/v1/work-orders/failure-order/preempt", body: `{"reason":"r","request_id":"q"}`, failing: []string{"GetWorkOrder"}},
		{name: "changeTaskPolicy", method: http.MethodPost, path: "/v1/tasks/failure-task/policy", body: `{"verify_stage":true,"reason":"r","request_id":"q"}`, failing: []string{"ChangeTaskPolicyCommand"}},
		{name: "recoverWorkOrder", method: http.MethodPost, path: "/v1/work-orders/failure-order/recover", body: `{"request_id":"q"}`, failing: []string{"GetWorkOrder"}},
		{name: "retryReviewRound", method: http.MethodPost, path: "/v1/tasks/failure-task/review-round/retry", body: `{"request_id":"q","reason":"r"}`, failing: []string{"GetTask"}},
		{name: "recoverInterruptedReviewRound", method: http.MethodPost, path: "/v1/tasks/failure-task/review-round/recover", body: `{"request_id":"q"}`, failing: []string{"ListTaskWorkOrders", "GetTask"}},
		{name: "mergeTask OnMerge", method: http.MethodPost, path: "/v1/tasks/failure-approved/merge", prepare: seedApprovedTask, hook: func(s *Server) {
			s.OnMerge = func(context.Context, core.Task, core.Actor) error { return storeFailure }
		}},
		{name: "mergeTaskAtGate OnGateMerge", method: http.MethodPost, path: "/v1/tasks/failure-merge-gate/merge", prepare: seedMergeGateTask, hook: func(s *Server) {
			s.OnGateMerge = func(context.Context, core.Task, core.Actor) (core.Task, error) { return core.Task{}, storeFailure }
		}},
		{name: "fixMergeConflict OnConflictFix", method: http.MethodPost, path: "/v1/tasks/failure-task/merge-conflict-fix", hook: func(s *Server) {
			s.OnConflictFix = func(context.Context, core.Task) (core.WorkOrder, error) { return core.WorkOrder{}, storeFailure }
		}},
		{name: "writeWorkOrderToolError submit", method: http.MethodPost, path: "/v1/work-orders/failure-order/submit-for-review", body: `{"session_id":"s","head_sha":"h","workspace_id":"demo"}`, failing: []string{"GetWorkOrderWithoutSnapshots"}},
		{name: "writeWorkOrderToolError template", method: http.MethodGet, path: "/v1/work-orders/failure-order/pull-request-template?session_id=s", failing: []string{"GetWorkOrderWithoutSnapshots"}},
		{name: "writeWorkOrderToolError context refresh", method: http.MethodPost, path: "/v1/work-orders/failure-order/context-refresh", body: `{"session_id":"s","workspace_id":"demo"}`, failing: []string{"GetWorkOrderWithoutSnapshots"}},
		// Owned routes whose failure answer was already generic now log the cause
		// redacted, with the request ID.
		{name: "attachTaskBranch", method: http.MethodPost, path: "/v1/tasks/failure-task/branch", body: `{"branch":"conveyor/other"}`, failing: []string{"AttachTaskBranch"}},
		{name: "updateTaskContext", method: http.MethodPost, path: "/v1/tasks/failure-task/context", body: `{}`, failing: []string{"UpdateTaskContext"}},
		{name: "confirmTaskContextProposal", method: http.MethodPost, path: "/v1/tasks/failure-task/context/proposals/requirement/req-x/confirm", failing: []string{"ConfirmTaskContextProposal"}},
		// Item 3: error text at 5xx.
		{name: "requestTaskChanges job read", method: http.MethodPost, path: "/v1/tasks/failure-task/request-changes", body: `{"feedback":"fix"}`, failing: []string{"GetLatestJob"}},
		{name: "requestTaskChanges configuration", method: http.MethodPost, path: "/v1/tasks/failure-task/request-changes", body: `{"feedback":"fix"}`, prepare: seedReviewJob, hook: func(s *Server) {
			s.ConfigProvider = func(context.Context) (*config.Config, error) { return nil, storeFailure }
		}},
		{name: "requestTaskChanges command", method: http.MethodPost, path: "/v1/tasks/failure-task/request-changes", body: `{"feedback":"fix"}`, prepare: seedReviewJob, failing: []string{"RequestChangesCommand"}},
		{name: "redispatchTask command", method: http.MethodPost, path: "/v1/tasks/failure-task/redispatch", failing: []string{"ApplyTaskCommand"}},
		{name: "rebuildLineage", method: http.MethodPost, path: "/v1/lineage/rebuild", body: `{"reason":"r","request_id":"q"}`, failing: []string{"RebuildLineage"}},
		{name: "writeTaskCreateError", method: http.MethodPost, path: "/v1/tasks", body: `{"body":"work","repo":"conveyor"}`, failing: []string{"CreateTaskWithDependenciesAndContext"}, hook: func(s *Server) {
			s.Repos = []string{"conveyor"}
			s.GenerateTaskTitle = func(context.Context, core.Task) (string, error) { return "A title", nil }
		}},
		{name: "getWorktreeCleanupStatus task", method: http.MethodGet, path: "/v1/tasks/failure-task/worktree-cleanup", failing: []string{"GetTask"}},
		{name: "getWorktreeCleanupStatus events", method: http.MethodGet, path: "/v1/tasks/failure-task/worktree-cleanup", failing: []string{"ListCompactEvents"}},
		{name: "getWorktreeCleanupStatus count", method: http.MethodGet, path: "/v1/tasks/failure-task/worktree-cleanup", prepare: prepareCleanup, failing: []string{"CountEvents"}},
		{name: "recordWorktreeCleanup task", method: http.MethodPost, path: "/v1/tasks/failure-task/worktree-cleanup", body: cleanup, failing: []string{"GetTask"}},
		{name: "recordWorktreeCleanup append", method: http.MethodPost, path: "/v1/tasks/failure-closed/worktree-cleanup", body: strings.Replace(cleanup, "failure-task", "failure-closed", 1), prepare: seedClosedTask, failing: []string{"AppendEvent"}},
	}
}

func seedApprovedTask(t *testing.T, st store.Store) {
	t.Helper()
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	if err := st.CreateTask(ctx, core.Task{ID: "failure-approved", Workspace: "demo", Repo: "conveyor", Branch: "failure-approved", State: core.TaskApproved, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
}

func seedMergeGateTask(t *testing.T, st store.Store) {
	t.Helper()
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	if err := st.CreateTask(ctx, core.Task{ID: "failure-merge-gate", Workspace: "demo", Repo: "conveyor", Branch: "failure-merge-gate", State: core.TaskAwaiting, GateKind: core.GateKindMergeApproval, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
}

func seedClosedTask(t *testing.T, st store.Store) {
	t.Helper()
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	if err := st.CreateTask(ctx, core.Task{ID: "failure-closed", Workspace: "demo", Repo: "conveyor", Branch: "failure-closed", State: core.TaskClosed, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendEvent(ctx, core.Event{TaskID: "failure-closed", Kind: "work_order.claimed", ActorID: store.UserActorID("usr_owner"), ActorRole: core.ActorUser}); err != nil {
		t.Fatal(err)
	}
}

func seedReviewJob(t *testing.T, st store.Store) {
	t.Helper()
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	if err := st.CreateJob(ctx, core.Job{ID: "failure-review", TaskID: failureTask, Stage: core.StageReview, State: core.JobPending}); err != nil {
		t.Fatal(err)
	}
}

// TestTaskRoutesAnswerStoreFailureGenerically proves each item 1 to 3 route
// answers a store failure with the logged generic 500 and no error text
// (DEC-105(1); component-http-api invariant 16).
func TestTaskRoutesAnswerStoreFailureGenerically(t *testing.T) {
	for _, route := range failingOperationRoutes() {
		t.Run(route.name, func(t *testing.T) {
			server, base := failureClassesFixture(t)
			if route.prepare != nil {
				route.prepare(t, base)
			}
			captured := captureStandardLog(t)
			failWith(server, newFailingStore(base, route.failing...))
			if route.hook != nil {
				route.hook(server)
			}
			requireGenericFailure(t, failureRequest(t, server, route.method, route.path, route.body), captured)
		})
	}
}

// TestTaskRoutesAnswerBackendOperationGenerically covers the translated form of
// a store failure, whose chain must never read as a refusal.
func TestTaskRoutesAnswerBackendOperationGenerically(t *testing.T) {
	backend := fmt.Errorf("PostgreSQL %w", store.ErrBackendOperation)
	for _, route := range []struct {
		name, method, path, body string
		hook                     func(*Server)
	}{
		{"merge marked refusal", http.MethodPost, "/v1/tasks/failure-approved/merge", "", func(s *Server) {
			s.OnMerge = func(context.Context, core.Task, core.Actor) error { return store.Refuse(backend) }
		}},
		{"conflict fix forge credential read", http.MethodPost, "/v1/tasks/failure-approved/merge-conflict-fix", "", func(s *Server) {
			s.OnConflictFix = func(context.Context, core.Task) (core.WorkOrder, error) {
				return core.WorkOrder{}, github.PermissionError(backend)
			}
		}},
	} {
		t.Run(route.name, func(t *testing.T) {
			server, base := failureClassesFixture(t)
			seedApprovedTask(t, base)
			captureStandardLog(t)
			route.hook(server)
			response := failureRequest(t, server, route.method, route.path, route.body)
			if response.Code != http.StatusInternalServerError || strings.TrimSpace(response.Body.String()) != "internal server error" {
				t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
			}
		})
	}
}

// TestTaskRoutesKeepNamedRefusals proves each refusal keeps its 4xx. Every
// status here is the one the route answered before the sweep (DEC-105(2);
// component-http-api invariant 15).
func TestTaskRoutesKeepNamedRefusals(t *testing.T) {
	assignee := `{"assignee_user_id":"usr_x"}`
	add := `{"depends_on_task_id":"failure-gate","reason":"r","request_id":"q"}`
	remove := `{"reason":"r","request_id":"q"}`
	preempt := `{"reason":"r","request_id":"q"}`
	policy := `{"verify_stage":true,"reason":"r","request_id":"q"}`
	for _, tc := range []struct {
		name, method, path, body string
		failing                  string
		err                      error
		status                   int
		contains                 string
	}{
		{"assignee task missing", http.MethodPut, "/v1/tasks/failure-task/assignee", assignee, "SetTaskAssigneeCommand", fmt.Errorf("task t: %w", store.ErrNotFound), http.StatusBadRequest, "task not found"},
		{"assignee not a member", http.MethodPut, "/v1/tasks/failure-task/assignee", assignee, "SetTaskAssigneeCommand", fmt.Errorf("assignee usr_x: %w", store.ErrAssigneeNotMember), http.StatusBadRequest, "not an active member"},
		{"assignee lacks capability", http.MethodPut, "/v1/tasks/failure-task/assignee", assignee, "SetTaskAssigneeCommand", fmt.Errorf("assignee usr_x: %w", store.ErrAssigneeLacksCapability), http.StatusBadRequest, "lacks the claim_work capability"},
		{"add dependency invalid", http.MethodPost, "/v1/tasks/failure-task/dependencies", add, "AddTaskDependency", store.ErrTaskDependencyInvalid, http.StatusBadRequest, "are required"},
		{"add dependency missing", http.MethodPost, "/v1/tasks/failure-task/dependencies", add, "AddTaskDependency", fmt.Errorf("task t: %w", store.ErrNotFound), http.StatusNotFound, "not found"},
		{"add dependency terminal", http.MethodPost, "/v1/tasks/failure-task/dependencies", add, "AddTaskDependency", store.ErrTaskTerminal, http.StatusConflict, "terminal"},
		{"add dependency cycle", http.MethodPost, "/v1/tasks/failure-task/dependencies", add, "AddTaskDependency", store.ErrTaskDependencyCycle, http.StatusConflict, "cycle"},
		{"add dependency conflict", http.MethodPost, "/v1/tasks/failure-task/dependencies", add, "AddTaskDependency", store.ErrTaskDependencyConflict, http.StatusConflict, "conflicts"},
		{"remove edge missing", http.MethodDelete, "/v1/tasks/failure-task/dependencies/failure-gate", remove, "RemoveTaskDependency", fmt.Errorf("%w: a -> b", store.ErrDependencyEdgeNotFound), http.StatusBadRequest, "dependency edge not found"},
		{"remove invalid", http.MethodDelete, "/v1/tasks/failure-task/dependencies/failure-gate", remove, "RemoveTaskDependency", store.ErrTaskDependencyInvalid, http.StatusBadRequest, "are required"},
		{"remove reused request", http.MethodDelete, "/v1/tasks/failure-task/dependencies/failure-gate", remove, "RemoveTaskDependency", fmt.Errorf("%w: request_id q", store.ErrTaskDependencyConflict), http.StatusBadRequest, "already used"},
		{"preempt missing", http.MethodPost, "/v1/work-orders/failure-order/preempt", preempt, "GetWorkOrder", fmt.Errorf("work order x: %w", store.ErrNotFound), http.StatusBadRequest, "not found"},
		{"policy missing", http.MethodPost, "/v1/tasks/failure-task/policy", policy, "ChangeTaskPolicyCommand", fmt.Errorf("task t: %w", store.ErrNotFound), http.StatusBadRequest, "not found"},
		{"policy conflict", http.MethodPost, "/v1/tasks/failure-task/policy", policy, "ChangeTaskPolicyCommand", fmt.Errorf("%w: task has a claimed attempt", store.ErrSetupChangeConflict), http.StatusConflict, "claimed attempt"},
		{"policy marked refusal", http.MethodPost, "/v1/tasks/failure-task/policy", policy, "ChangeTaskPolicyCommand", store.Refusef("a frozen-policy change is required"), http.StatusBadRequest, "frozen-policy"},
		{"recover missing", http.MethodPost, "/v1/work-orders/failure-order/recover", `{"request_id":"q"}`, "GetWorkOrder", fmt.Errorf("work order x: %w", store.ErrNotFound), http.StatusConflict, "not found"},
		{"retry conflict", http.MethodPost, "/v1/tasks/failure-task/review-round/retry", `{"request_id":"q","reason":"r"}`, "GetTask", fmt.Errorf("%w: no round", store.ErrReviewRetryConflict), http.StatusConflict, "no round"},
		{"submit claim lost", http.MethodPost, "/v1/work-orders/failure-order/submit-for-review", `{"session_id":"s","head_sha":"h","workspace_id":"demo"}`, "", nil, http.StatusConflict, "no longer held"},
		{"template claim lost", http.MethodGet, "/v1/work-orders/failure-order/pull-request-template?session_id=s", "", "", nil, http.StatusConflict, "no longer held"},
		{"context refresh claim lost", http.MethodPost, "/v1/work-orders/failure-order/context-refresh", `{"session_id":"s","workspace_id":"demo"}`, "", nil, http.StatusConflict, "no longer held"},
		{"submit missing order", http.MethodPost, "/v1/work-orders/absent/submit-for-review", `{"session_id":"s","head_sha":"h","workspace_id":"demo"}`, "", nil, http.StatusConflict, "no longer held"},
		{"context refresh invalid argument", http.MethodPost, "/v1/work-orders/failure-order/context-refresh", `{"workspace_id":"demo"}`, "", nil, http.StatusBadRequest, "required"},
		{"recover state refusal", http.MethodPost, "/v1/work-orders/failure-order/recover", `{"request_id":"q"}`, "", nil, http.StatusConflict, "retry-suppressed"},
		{"preempt missing order", http.MethodPost, "/v1/work-orders/absent/preempt", `{"reason":"r","request_id":"q"}`, "", nil, http.StatusBadRequest, "not found"},
		{"preempt missing fields", http.MethodPost, "/v1/work-orders/failure-order/preempt", `{}`, "", nil, http.StatusBadRequest, "are required"},
		{"retry no round", http.MethodPost, "/v1/tasks/failure-task/review-round/retry", `{"request_id":"q","reason":"r"}`, "", nil, http.StatusConflict, "does not have a recoverable"},
		{"interrupted no round", http.MethodPost, "/v1/tasks/failure-task/review-round/recover", `{"request_id":"q"}`, "", nil, http.StatusConflict, "no interrupted review round"},
		{"lineage validation", http.MethodPost, "/v1/lineage/rebuild", `{"reason":"r","request_id":"q"}`, "RebuildLineage", fmt.Errorf("%w: bad", store.ErrLineageRebuildValidation), http.StatusBadRequest, "bad"},
		{"lineage conflict", http.MethodPost, "/v1/lineage/rebuild", `{"reason":"r","request_id":"q"}`, "RebuildLineage", fmt.Errorf("%w: reused", store.ErrLineageRebuildConflict), http.StatusConflict, "reused"},
		{"request changes off the gate", http.MethodPost, "/v1/tasks/failure-gate/request-changes", `{"feedback":"fix"}`, "", nil, http.StatusConflict, "not at the merge gate"},
		{"redispatch held", http.MethodPost, "/v1/tasks/failure-task/redispatch", "", "ApplyTaskCommand", &core.ErrInvalidTransition{Space: core.TaskLifecycle, From: "parked", Command: "recover"}, http.StatusConflict, "invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, base := failureClassesFixture(t)
			captured := captureStandardLog(t)
			if tc.failing != "" {
				failing := newFailingStore(base, tc.failing)
				failing.err = tc.err
				failWith(server, failing)
			}
			response := failureRequest(t, server, tc.method, tc.path, tc.body)
			if response.Code != tc.status || !strings.Contains(response.Body.String(), tc.contains) {
				t.Fatalf("status=%d body=%q, want %d containing %q", response.Code, response.Body.String(), tc.status, tc.contains)
			}
			if strings.Contains(captured.String(), "httpapi: store failure") {
				t.Fatalf("a refusal logged as a store failure: %q", captured.String())
			}
		})
	}
}

// TestItemOneRoutesAnswerMissingTaskNotFound keeps the real refusal the outage
// used to hide: a missing task is still 404 on each route.
func TestItemOneRoutesAnswerMissingTaskNotFound(t *testing.T) {
	server, _ := failureClassesFixture(t)
	for _, route := range []struct{ method, path, body string }{
		{http.MethodPost, "/v1/tasks/absent/redispatch", ""},
		{http.MethodPost, "/v1/tasks/absent/close", `{"reason":"stop"}`},
		{http.MethodPost, "/v1/tasks/absent/review", `{}`},
		{http.MethodPost, "/v1/tasks/absent/merge", ""},
		{http.MethodPost, "/v1/tasks/absent/merge-conflict-fix", ""},
		{http.MethodGet, "/v1/tasks/absent", ""},
		{http.MethodGet, "/v1/tasks/absent/events/stream", ""},
		{http.MethodGet, "/v1/tasks/absent/activity", ""},
		{http.MethodPost, "/v1/tasks/absent/request-changes", `{"feedback":"fix"}`},
		{http.MethodGet, "/v1/tasks/absent/audit/event/1", ""},
		{http.MethodGet, "/v1/tasks/absent/history", ""},
	} {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			if response := failureRequest(t, server, route.method, route.path, route.body); response.Code != http.StatusNotFound {
				t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
			}
		})
	}
}

// TestHistoryAndAuditKeepUniformNotFound keeps one answer for a task in another
// workspace and a missing task (req-accounts-and-membership AC-4.2).
func TestHistoryAndAuditKeepUniformNotFound(t *testing.T) {
	server, _ := failureClassesFixture(t)
	for _, suffix := range []string{"history", "audit/event/1"} {
		missing := failureRequest(t, server, http.MethodGet, "/v1/tasks/absent/"+suffix, "")
		foreign := failureRequest(t, server, http.MethodGet, "/v1/tasks/"+failureOtherTask+"/"+suffix, "")
		if missing.Code != http.StatusNotFound || foreign.Code != http.StatusNotFound || missing.Body.String() != foreign.Body.String() {
			t.Fatalf("%s: missing %d %q, foreign %d %q", suffix, missing.Code, missing.Body.String(), foreign.Code, foreign.Body.String())
		}
	}
}

// TestMergeRoutesKeepRefusalsAndForgeRemedies keeps 409 and the remedy for a
// refusal or a forge failure (req-delivery-and-forge AC-1.4, AC-1.8).
func TestMergeRoutesKeepRefusalsAndForgeRemedies(t *testing.T) {
	remedy := "connect the GitHub App in workspace settings"
	for _, tc := range []struct {
		name     string
		err      error
		contains string
	}{
		{"permission", github.PermissionError(errors.New(remedy)), remedy},
		{"categorized request", github.CategorizeError(errors.New("connection reset")), "connection reset"},
		{"marked remedy", store.Refusef("no pull request found for branch b; push it"), "push it"},
		{"conflict fix pending", fmt.Errorf("%w: work order o is queued", dispatch.ErrConflictFixPending), "conflict fix pending"},
		{"readiness pending", fmt.Errorf("pull request r#1 %w", dispatch.ErrMergeReadinessPending), "readiness"},
		{"invalid transition", &core.ErrInvalidTransition{Space: core.TaskLifecycle, From: "merged", Command: "c"}, "invalid"},
		{"not conflicting", fmt.Errorf("%w (MERGEABLE)", dispatch.ErrPullRequestNotConflicting), "not conflicting (MERGEABLE)"},
		{"blocked by recovery", dispatch.ErrConflictFixNotDispatched, "not dispatched"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, base := failureClassesFixture(t)
			seedApprovedTask(t, base)
			seedMergeGateTask(t, base)
			server.OnMerge = func(context.Context, core.Task, core.Actor) error { return tc.err }
			server.OnGateMerge = func(context.Context, core.Task, core.Actor) (core.Task, error) { return core.Task{}, tc.err }
			server.OnConflictFix = func(context.Context, core.Task) (core.WorkOrder, error) { return core.WorkOrder{}, tc.err }
			for _, path := range []string{"/v1/tasks/failure-approved/merge", "/v1/tasks/failure-merge-gate/merge", "/v1/tasks/failure-approved/merge-conflict-fix"} {
				response := failureRequest(t, server, http.MethodPost, path, "")
				if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), tc.contains) {
					t.Fatalf("%s status=%d body=%q", path, response.Code, response.Body.String())
				}
			}
		})
	}
}

// TestFixMergeConflictRefusesEmptyOrder replaces the empty 202 with a 409.
func TestFixMergeConflictRefusesEmptyOrder(t *testing.T) {
	server, _ := failureClassesFixture(t)
	server.OnConflictFix = func(context.Context, core.Task) (core.WorkOrder, error) {
		return core.WorkOrder{}, dispatch.ErrConflictFixNotDispatched
	}
	response := failureRequest(t, server, http.MethodPost, "/v1/tasks/failure-task/merge-conflict-fix", "")
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "interrupted review recovery") {
		t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
	}
}

// TestTaskIntakeFailureClasses separates intake's answers: a store failure is the
// generic 500, a store refusal stays 409, and unavailable configuration stays
// 503 without its error text (component-task-lifecycle invariant 14).
func TestTaskIntakeFailureClasses(t *testing.T) {
	body := `{"body":"work","repo":"conveyor"}`
	titled := func(s *Server) {
		s.Repos = []string{"conveyor"}
		s.GenerateTaskTitle = func(context.Context, core.Task) (string, error) { return "A title", nil }
	}
	t.Run("refusal", func(t *testing.T) {
		server, base := failureClassesFixture(t)
		failing := newFailingStore(base, "CreateTaskWithDependenciesAndContext")
		failing.err = store.ErrTaskBranchConflict
		failWith(server, failing)
		titled(server)
		if response := failureRequest(t, server, http.MethodPost, "/v1/tasks", body); response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "create task:") {
			t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
		}
	})
	t.Run("configuration", func(t *testing.T) {
		server, _ := failureClassesFixture(t)
		captured := captureStandardLog(t)
		titled(server)
		server.ConfigProvider = func(context.Context) (*config.Config, error) { return nil, errors.New(failingStoreDriverText) }
		response := failureRequest(t, server, http.MethodPost, "/v1/tasks", body)
		if response.Code != http.StatusServiceUnavailable || strings.Contains(response.Body.String(), "does not exist") {
			t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
		}
		if strings.Contains(captured.String(), failingStoreCredential) {
			t.Fatalf("log carries the credential: %q", captured.String())
		}
	})
}

// TestTaskDetailReadinessStoreFailureIsGeneric keeps the forge's 503 for a
// failed read and answers a store failure in the readiness path generically.
func TestTaskDetailReadinessStoreFailureIsGeneric(t *testing.T) {
	server, base := failureClassesFixture(t)
	seedApprovedTask(t, base)
	captured := captureStandardLog(t)
	server.OnMergeReadiness = func(context.Context, core.Task) (dispatch.MergeReadiness, error) {
		return dispatch.MergeReadiness{}, errors.New(failingStoreDriverText)
	}
	requireGenericFailure(t, failureRequest(t, server, http.MethodGet, "/v1/tasks/failure-approved/activity", ""), captured)
}

// TestCloseReasonCountsRunes applies component-task-lifecycle invariant 31: a
// reason of at most 255 characters, counted in runes, or with no reason a
// reason_code of at most 64 bytes. Each refusal names its own limit and echoes no
// caller text.
func TestCloseReasonCountsRunes(t *testing.T) {
	for _, tc := range []struct {
		name, field, value string
		status             int
		refusal            string
	}{
		{"reason of 255 two-byte runes", "reason", strings.Repeat("é", 255), http.StatusOK, ""},
		{"reason of 256 two-byte runes", "reason", strings.Repeat("é", 256), http.StatusBadRequest, "reason must be at most 255 characters"},
		{"reason of 255 ASCII", "reason", strings.Repeat("a", 255), http.StatusOK, ""},
		{"reason of 256 ASCII", "reason", strings.Repeat("a", 256), http.StatusBadRequest, "reason must be at most 255 characters"},
		{"reason_code over 64 bytes", "reason_code", strings.Repeat("é", 33), http.StatusBadRequest, "reason_code must be at most 64 bytes"},
		{"reason_code at 64 bytes", "reason_code", strings.Repeat("a", 64), http.StatusOK, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, _ := failureClassesFixture(t)
			response := failureRequest(t, server, http.MethodPost, "/v1/tasks/failure-task/close", fmt.Sprintf(`{%q:%q}`, tc.field, tc.value))
			if response.Code != tc.status {
				t.Fatalf("status=%d body=%q, want %d", response.Code, response.Body.String(), tc.status)
			}
			if tc.status != http.StatusOK && strings.TrimSpace(response.Body.String()) != tc.refusal {
				t.Fatalf("body=%q, want %q", response.Body.String(), tc.refusal)
			}
		})
	}
}

// oversizedBody is a JSON object larger than limit with valid syntax, so only
// the bound can refuse it.
func oversizedBody(limit int) string {
	return `{"padding":"` + strings.Repeat("x", limit) + `"}`
}

// TestTaskRoutesBoundBodies proves each bounded route answers 413 above its
// bound, for a declared length and for an unknown length (DEC-105(3);
// component-http-api invariant 17).
func TestTaskRoutesBoundBodies(t *testing.T) {
	for _, route := range []struct {
		name, method, path string
		limit              int
		prepare            func(*testing.T, store.Store)
	}{
		{name: "createTask", method: http.MethodPost, path: "/v1/tasks", limit: taskCreateBodyLimit},
		{name: "setTaskAssignee", method: http.MethodPut, path: "/v1/tasks/failure-task/assignee", limit: taskRouteBodyLimit},
		{name: "addTaskDependency", method: http.MethodPost, path: "/v1/tasks/failure-task/dependencies", limit: taskRouteBodyLimit},
		{name: "removeTaskDependency", method: http.MethodDelete, path: "/v1/tasks/failure-task/dependencies/failure-gate", limit: taskRouteBodyLimit},
		{name: "closeTask", method: http.MethodPost, path: "/v1/tasks/failure-task/close", limit: taskRouteBodyLimit},
		{name: "reviewTask", method: http.MethodPost, path: "/v1/tasks/failure-gate/review", limit: taskRouteBodyLimit},
		{name: "requestTaskChanges", method: http.MethodPost, path: "/v1/tasks/failure-task/request-changes", limit: taskRouteBodyLimit},
		{name: "preemptWorkOrder", method: http.MethodPost, path: "/v1/work-orders/failure-order/preempt", limit: taskRouteBodyLimit},
		{name: "rebuildLineage", method: http.MethodPost, path: "/v1/lineage/rebuild", limit: taskRouteBodyLimit},
		{name: "retryReviewRound", method: http.MethodPost, path: "/v1/tasks/failure-task/review-round/retry", limit: taskRouteBodyLimit},
		{name: "recoverInterruptedReviewRound", method: http.MethodPost, path: "/v1/tasks/failure-task/review-round/recover", limit: taskRouteBodyLimit},
		{name: "submitWorkOrderReview", method: http.MethodPost, path: "/v1/work-orders/failure-order/submit-for-review", limit: taskRouteBodyLimit},
		{name: "attachTaskBranch", method: http.MethodPost, path: "/v1/tasks/failure-task/branch", limit: taskRouteBodyLimit},
		{name: "updateTaskContext", method: http.MethodPost, path: "/v1/tasks/failure-task/context", limit: taskRouteBodyLimit},
		{name: "recordWorktreeCleanup", method: http.MethodPost, path: "/v1/tasks/failure-task/worktree-cleanup", limit: taskRouteBodyLimit},
		{name: "changeTaskPolicy", method: http.MethodPost, path: "/v1/tasks/failure-task/policy", limit: 16384},
		{name: "recoverWorkOrder", method: http.MethodPost, path: "/v1/work-orders/failure-order/recover", limit: 1 << 20},
		{name: "restartTask", method: http.MethodPost, path: "/v1/tasks/failure-task/restart", limit: 65536},
	} {
		t.Run(route.name, func(t *testing.T) {
			server, base := failureClassesFixture(t)
			if route.prepare != nil {
				route.prepare(t, base)
			}
			body := oversizedBody(route.limit)
			if response := failureRequest(t, server, route.method, route.path, body); response.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("declared length: status=%d body=%.200q", response.Code, response.Body.String())
			}
			request := httptest.NewRequest(route.method, route.path+"?workspace_id=demo", io.NopCloser(strings.NewReader(body)))
			request.ContentLength = -1
			request.Header.Set("Authorization", "Bearer token")
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, request)
			if response.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("unknown length: status=%d body=%.200q", response.Code, response.Body.String())
			}
		})
	}
}

// TestOptionalBodiesStayOptional keeps the empty body these routes accept, so
// bounding them changes no successful request.
func TestOptionalBodiesStayOptional(t *testing.T) {
	server, _ := failureClassesFixture(t)
	for _, tc := range []struct {
		path     string
		status   int
		contains string
	}{
		{"/v1/work-orders/failure-order/preempt", http.StatusBadRequest, "are required"},
		{"/v1/tasks/failure-task/review-round/retry", http.StatusBadRequest, "are required"},
		{"/v1/tasks/failure-task/review-round/recover", http.StatusBadRequest, "is required"},
		{"/v1/tasks/failure-task/close", http.StatusBadRequest, "reason is required"},
	} {
		response := failureRequest(t, server, http.MethodPost, tc.path, "")
		if response.Code != tc.status || !strings.Contains(response.Body.String(), tc.contains) {
			t.Fatalf("%s status=%d body=%q", tc.path, response.Code, response.Body.String())
		}
	}
}

// TestInterventionRoutesClassifyFailures covers the gate decision and close acts
// through their shared hook: a refusal is 409, a store failure the generic 500
// even when marked, and a forge failure on review keeps its answer.
func TestInterventionRoutesClassifyFailures(t *testing.T) {
	review := `{"action":"reject","reason_code":"nope"}`
	backend := fmt.Errorf("PostgreSQL %w", store.ErrBackendOperation)
	remedy := "connect the GitHub App in workspace settings"
	for _, tc := range []struct {
		name, path, body string
		err              error
		status           int
		contains         string
	}{
		{"review marked refusal", "/v1/tasks/failure-gate/review", review, store.Refusef("work order o was superseded"), http.StatusConflict, "superseded"},
		{"review invalid transition", "/v1/tasks/failure-gate/review", review, &core.ErrInvalidTransition{Space: core.TaskLifecycle, From: "closed", Command: "c"}, http.StatusConflict, "invalid"},
		{"review head unavailable", "/v1/tasks/failure-gate/review", review, dispatch.ErrReviewedHeadUnavailable, http.StatusConflict, "unavailable"},
		{"review forge failure", "/v1/tasks/failure-gate/review", review, github.PermissionError(errors.New(remedy)), http.StatusInternalServerError, remedy},
		{"review store failure", "/v1/tasks/failure-gate/review", review, errors.New(failingStoreDriverText), http.StatusInternalServerError, "internal server error"},
		{"review marked backend failure", "/v1/tasks/failure-gate/review", review, store.Refuse(backend), http.StatusInternalServerError, "internal server error"},
		{"close marked refusal", "/v1/tasks/failure-task/close", `{"reason":"stop"}`, store.Refusef("task is changing"), http.StatusConflict, "changing"},
		{"close terminal", "/v1/tasks/failure-task/close", `{"reason":"stop"}`, store.ErrTaskTerminal, http.StatusConflict, "terminal"},
		{"close store failure", "/v1/tasks/failure-task/close", `{"reason":"stop"}`, errors.New(failingStoreDriverText), http.StatusInternalServerError, "internal server error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, _ := failureClassesFixture(t)
			captureStandardLog(t)
			server.OnIntervention = func(context.Context, core.Task, core.Job, core.Intervention) error { return tc.err }
			response := failureRequest(t, server, http.MethodPost, tc.path, tc.body)
			if response.Code != tc.status || !strings.Contains(response.Body.String(), tc.contains) || strings.Contains(response.Body.String(), "does not exist") {
				t.Fatalf("status=%d body=%q, want %d containing %q", response.Code, response.Body.String(), tc.status, tc.contains)
			}
		})
	}
}

// TestCloseBoundsBytesAfterTheJSONValue counts every byte of the body, so a valid
// object followed by whitespace past the bound answers 413 and closes nothing.
func TestCloseBoundsBytesAfterTheJSONValue(t *testing.T) {
	server, base := failureClassesFixture(t)
	body := `{"reason":"stop"}` + strings.Repeat(" ", taskRouteBodyLimit)
	request := httptest.NewRequest(http.MethodPost, "/v1/tasks/failure-task/close?workspace_id=demo", io.NopCloser(strings.NewReader(body)))
	request.ContentLength = -1
	request.Header.Set("Authorization", "Bearer token")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d body=%.200q", response.Code, response.Body.String())
	}
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	task, err := base.GetTask(ctx, failureTask)
	if err != nil {
		t.Fatal(err)
	}
	if core.TaskTerminal(task.State) {
		t.Fatalf("the oversized request closed the task: state=%s", task.State)
	}
}
