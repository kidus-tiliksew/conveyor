package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func TestWorktreeCleanupRecordsCredentialActorOnce(t *testing.T) {
	for _, actor := range []store.Actor{
		{ID: store.UserActorID("runner"), Role: core.ActorUser},
		{ID: store.WorkerActorID("worker-a"), Role: core.ActorWorker},
	} {
		t.Run(string(actor.Role), func(t *testing.T) {
			st := store.NewMemory()
			ctx := store.WithActor(store.WithWorkspace(t.Context(), "demo"), actor)
			task := core.Task{ID: "terminal-" + string(actor.Role), Workspace: "demo", Repo: "conveyor", Branch: "conveyor/task-terminal", State: core.TaskMerged}
			if err := st.CreateTask(ctx, task); err != nil {
				t.Fatal(err)
			}
			if err := st.AppendEvent(ctx, core.Event{TaskID: task.ID, Kind: "work_order.claimed"}); err != nil {
				t.Fatal(err)
			}
			srv := NewServer(st)
			body := []byte(`{"repository":"conveyor","branch":"conveyor/task-terminal","worktree":"removed","branch_result":"retained","path":"/task/worktree","actor":"user:spoofed"}`)
			var wait sync.WaitGroup
			wait.Add(2)
			responses := make(chan *httptest.ResponseRecorder, 2)
			for range 2 {
				go func() {
					defer wait.Done()
					request := worktreeCleanupRequest(ctx, http.MethodPost, task.ID, body)
					response := httptest.NewRecorder()
					srv.recordWorktreeCleanup(response, request)
					responses <- response
				}()
			}
			wait.Wait()
			close(responses)
			for response := range responses {
				if response.Code != http.StatusOK {
					t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
				}
			}
			if count, err := st.CountEvents(ctx, task.ID, worktreeCleanupCompletedEvent); err != nil || count != 1 {
				t.Fatalf("completion count=%d err=%v", count, err)
			}
			events, err := st.ListEvents(ctx, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			completed := events[len(events)-1]
			if completed.Kind != worktreeCleanupCompletedEvent || completed.ActorID != actor.ID || completed.ActorRole != actor.Role {
				t.Fatalf("completion event=%+v actor=%+v", completed, actor)
			}
			statusResponse := httptest.NewRecorder()
			srv.getWorktreeCleanupStatus(statusResponse, worktreeCleanupRequest(ctx, http.MethodGet, task.ID, nil))
			var status worktreeCleanupStatus
			if statusResponse.Code != http.StatusOK || json.NewDecoder(statusResponse.Body).Decode(&status) != nil || !status.Terminal || !status.Completed {
				t.Fatalf("status response=%d body=%s decoded=%+v", statusResponse.Code, statusResponse.Body.String(), status)
			}
		})
	}
}

func TestWorktreeCleanupRejectsForeignNonterminalAndMismatchedRequests(t *testing.T) {
	st := store.NewMemory()
	owner := store.Actor{ID: store.UserActorID("owner"), Role: core.ActorUser}
	ctx := store.WithActor(store.WithWorkspace(t.Context(), "demo"), owner)
	task := core.Task{ID: "running", Workspace: "demo", Repo: "conveyor", Branch: "conveyor/task-running", State: core.TaskRunning}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendEvent(ctx, core.Event{TaskID: task.ID, Kind: "work_order.claimed"}); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(st)

	foreignCtx := store.WithActor(store.WithWorkspace(t.Context(), "demo"), store.Actor{ID: store.UserActorID("foreign"), Role: core.ActorUser})
	foreign := httptest.NewRecorder()
	srv.getWorktreeCleanupStatus(foreign, worktreeCleanupRequest(foreignCtx, http.MethodGet, task.ID, nil))
	if foreign.Code != http.StatusNotFound {
		t.Fatalf("foreign status=%d body=%s", foreign.Code, foreign.Body.String())
	}

	statusResponse := httptest.NewRecorder()
	srv.getWorktreeCleanupStatus(statusResponse, worktreeCleanupRequest(ctx, http.MethodGet, task.ID, nil))
	var status worktreeCleanupStatus
	_ = json.NewDecoder(statusResponse.Body).Decode(&status)
	if statusResponse.Code != http.StatusOK || status.Terminal || status.Completed {
		t.Fatalf("nonterminal status=%d decoded=%+v", statusResponse.Code, status)
	}

	body, _ := json.Marshal(worktreeCleanupRecord{Repository: "other", Branch: task.Branch, Worktree: "removed", BranchResult: "retained", Path: "/task/worktree"})
	mismatch := httptest.NewRecorder()
	srv.recordWorktreeCleanup(mismatch, worktreeCleanupRequest(ctx, http.MethodPost, task.ID, body))
	if mismatch.Code != http.StatusConflict {
		t.Fatalf("mismatch status=%d body=%s", mismatch.Code, mismatch.Body.String())
	}

	body, _ = json.Marshal(worktreeCleanupRecord{Repository: task.Repo, Branch: task.Branch, Worktree: "removed", BranchResult: "retained", Path: "/task/worktree"})
	nonterminal := httptest.NewRecorder()
	srv.recordWorktreeCleanup(nonterminal, worktreeCleanupRequest(ctx, http.MethodPost, task.ID, body))
	if nonterminal.Code != http.StatusConflict {
		t.Fatalf("nonterminal status=%d body=%s", nonterminal.Code, nonterminal.Body.String())
	}
}

func TestWorktreeCleanupRejectsMalformedResultPayload(t *testing.T) {
	st := store.NewMemory()
	actor := store.Actor{ID: store.UserActorID("owner"), Role: core.ActorUser}
	ctx := store.WithActor(store.WithWorkspace(t.Context(), "demo"), actor)
	task := core.Task{ID: "terminal", Workspace: "demo", Repo: "conveyor", Branch: "conveyor/task-terminal", State: core.TaskMerged}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendEvent(ctx, core.Event{TaskID: task.ID, Kind: "work_order.claimed"}); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(st)
	for _, test := range []struct {
		name   string
		record worktreeCleanupRecord
	}{
		{name: "worktree result", record: worktreeCleanupRecord{Repository: task.Repo, Branch: task.Branch, Worktree: "deleted", BranchResult: "retained", Path: "/task/worktree"}},
		{name: "branch result", record: worktreeCleanupRecord{Repository: task.Repo, Branch: task.Branch, Worktree: "removed", BranchResult: "deleted", Path: "/task/worktree"}},
		{name: "path", record: worktreeCleanupRecord{Repository: task.Repo, Branch: task.Branch, Worktree: "removed", BranchResult: "retained"}},
		{name: "warning", record: worktreeCleanupRecord{Repository: task.Repo, Branch: task.Branch, Worktree: "removed", BranchResult: "retained", Path: "/task/worktree", ProcessWarnings: []string{" "}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			body, _ := json.Marshal(test.record)
			response := httptest.NewRecorder()
			srv.recordWorktreeCleanup(response, worktreeCleanupRequest(ctx, http.MethodPost, task.ID, body))
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}

func worktreeCleanupRequest(ctx context.Context, method, taskID string, body []byte) *http.Request {
	request := httptest.NewRequest(method, "/", bytes.NewReader(body)).WithContext(ctx)
	route := chi.NewRouteContext()
	route.URLParams.Add("id", taskID)
	return request.WithContext(context.WithValue(request.Context(), chi.RouteCtxKey, route))
}
