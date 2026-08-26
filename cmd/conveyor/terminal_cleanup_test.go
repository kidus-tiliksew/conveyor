package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	workerservice "github.com/kidus-tiliksew/conveyor/internal/worker"
)

func TestTerminalWorktreeCleanupUsesRunAndWorkerRoutes(t *testing.T) {
	for _, test := range []struct {
		dispatch string
		path     string
	}{
		{dispatch: "run", path: "/v1/tasks/task-a/worktree-cleanup"},
		{dispatch: "worker", path: "/v1/worker/tasks/task-a/worktree-cleanup"},
	} {
		t.Run(test.dispatch, func(t *testing.T) {
			cleanupCalls := 0
			recordCalls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer credential" || r.Header.Get("X-Workspace-ID") != "demo" || r.URL.Path != test.path {
					http.Error(w, "wrong cleanup request", http.StatusBadRequest)
					return
				}
				switch r.Method {
				case http.MethodGet:
					_ = json.NewEncoder(w).Encode(terminalCleanupStatus{Terminal: true})
				case http.MethodPost:
					recordCalls++
					var record terminalCleanupRecord
					_ = json.NewDecoder(r.Body).Decode(&record)
					if record.Repository != "conveyor" || record.Branch != "conveyor/task-a" || record.Worktree != "removed" || record.BranchResult != "retained" || record.Path != "/worktrees/task-a" {
						http.Error(w, "wrong cleanup record", http.StatusBadRequest)
						return
					}
					_ = json.NewEncoder(w).Encode(terminalCleanupReceipt{Completed: true, Recorded: true})
				default:
					http.Error(w, "wrong method", http.StatusMethodNotAllowed)
				}
			}))
			defer server.Close()

			previous := cleanupTerminalTaskWorktree
			cleanupTerminalTaskWorktree = func(_ context.Context, _ *config.Config, _ workerservice.DispatchOrder) (worktreeCleanupResult, error) {
				cleanupCalls++
				return worktreeCleanupResult{Worktree: "removed", Branch: "retained", Path: "/worktrees/task-a"}, nil
			}
			t.Cleanup(func() { cleanupTerminalTaskWorktree = previous })

			item := workerservice.DispatchOrder{
				Task:     core.Task{ID: "task-a", Repo: "conveyor", Branch: "conveyor/task-a", State: core.TaskMerged},
				Dispatch: test.dispatch,
			}
			attempt, err := attemptTerminalWorktreeCleanup(t.Context(), &client{base: server.URL, workspace: "demo"}, "credential", item, &config.Config{})
			if err != nil || !attempt.Completed || cleanupCalls != 1 || recordCalls != 1 {
				t.Fatalf("attempt=%+v cleanupCalls=%d recordCalls=%d err=%v", attempt, cleanupCalls, recordCalls, err)
			}
		})
	}
}

func TestTerminalWorktreeCleanupSkipsRecordedAndRetriesNonterminal(t *testing.T) {
	completed := true
	terminal := true
	cleanupCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "recording must not run", http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(terminalCleanupStatus{Terminal: terminal, Completed: completed})
	}))
	defer server.Close()
	previous := cleanupTerminalTaskWorktree
	cleanupTerminalTaskWorktree = func(_ context.Context, _ *config.Config, _ workerservice.DispatchOrder) (worktreeCleanupResult, error) {
		cleanupCalls++
		return worktreeCleanupResult{}, nil
	}
	t.Cleanup(func() { cleanupTerminalTaskWorktree = previous })
	item := workerservice.DispatchOrder{Task: core.Task{ID: "task-a", Repo: "conveyor", Branch: "conveyor/task-a", State: core.TaskMerged}, Dispatch: "run"}
	c := &client{base: server.URL, workspace: "demo"}
	attempt, err := attemptTerminalWorktreeCleanup(t.Context(), c, "credential", item, &config.Config{})
	if err != nil || !attempt.Completed || cleanupCalls != 0 {
		t.Fatalf("recorded attempt=%+v calls=%d err=%v", attempt, cleanupCalls, err)
	}
	completed = false
	terminal = false
	attempt, err = attemptTerminalWorktreeCleanup(t.Context(), c, "credential", item, &config.Config{})
	if err != nil || !attempt.Pending || cleanupCalls != 0 {
		t.Fatalf("nonterminal attempt=%+v calls=%d err=%v", attempt, cleanupCalls, err)
	}
}

func TestTerminalWorktreeCleanupRecordingFailureRemainsRetryable(t *testing.T) {
	cleanupCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(terminalCleanupStatus{Terminal: true})
			return
		}
		http.Error(w, "recording unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	previous := cleanupTerminalTaskWorktree
	cleanupTerminalTaskWorktree = func(_ context.Context, _ *config.Config, _ workerservice.DispatchOrder) (worktreeCleanupResult, error) {
		cleanupCalls++
		return worktreeCleanupResult{Worktree: "skipped", Branch: "retained", Path: "-"}, nil
	}
	t.Cleanup(func() { cleanupTerminalTaskWorktree = previous })
	item := workerservice.DispatchOrder{Task: core.Task{ID: "task-a", Repo: "conveyor", Branch: "conveyor/task-a", State: core.TaskClosed}, Dispatch: "worker"}
	_, firstErr := attemptTerminalWorktreeCleanup(t.Context(), &client{base: server.URL, workspace: "demo"}, "credential", item, &config.Config{})
	_, secondErr := attemptTerminalWorktreeCleanup(t.Context(), &client{base: server.URL, workspace: "demo"}, "credential", item, &config.Config{})
	if firstErr == nil || secondErr == nil || !strings.Contains(firstErr.Error(), "record completion") || cleanupCalls != 2 {
		t.Fatalf("first=%v second=%v cleanupCalls=%d", firstErr, secondErr, cleanupCalls)
	}
}

func TestWorkerCleanupPassRemovesCompletedTasksAndRetainsFailures(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "retry-task") {
			http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
			return
		}
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(terminalCleanupStatus{Terminal: true})
			return
		}
		_ = json.NewEncoder(w).Encode(terminalCleanupReceipt{Completed: true, Recorded: true})
	}))
	defer server.Close()
	previous := cleanupTerminalTaskWorktree
	cleanupTerminalTaskWorktree = func(_ context.Context, _ *config.Config, item workerservice.DispatchOrder) (worktreeCleanupResult, error) {
		return worktreeCleanupResult{Worktree: "removed", Branch: "retained", Path: "/worktrees/" + item.Task.ID}, nil
	}
	t.Cleanup(func() { cleanupTerminalTaskWorktree = previous })
	tasks := map[string]workerservice.DispatchOrder{
		"done-task":  {Task: core.Task{ID: "done-task", Repo: "conveyor", Branch: "conveyor/task-done", State: core.TaskMerged}, Dispatch: "worker"},
		"retry-task": {Task: core.Task{ID: "retry-task", Repo: "conveyor", Branch: "conveyor/task-retry", State: core.TaskRunning}, Dispatch: "worker"},
	}
	var output bytes.Buffer
	cleanupWorkerTaskWorktrees(t.Context(), &client{base: server.URL, workspace: "demo"}, "credential", tasks, &config.Config{}, &output)
	if _, ok := tasks["done-task"]; ok {
		t.Fatalf("completed task remained tracked: %+v", tasks)
	}
	if _, ok := tasks["retry-task"]; !ok || !strings.Contains(output.String(), "worker worktree cleanup task retry-task") {
		t.Fatalf("retry task was not retained and logged: tasks=%+v output=%q", tasks, output.String())
	}
}
