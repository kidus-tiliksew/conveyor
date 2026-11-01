package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

// doneCleanupServer serves the reads conveyor done makes and records every
// cleanup request; status answers the recording request.
type doneCleanupServer struct {
	t        *testing.T
	task     core.Task
	holders  []core.Task
	status   int
	mu       sync.Mutex
	paths    []string
	recorded []terminalCleanupRecord
}

func newDoneCleanupServer(t *testing.T, task core.Task, status int, holders ...core.Task) *doneCleanupServer {
	t.Helper()
	server := &doneCleanupServer{t: t, task: task, holders: holders, status: status}
	httpServer := httptest.NewServer(http.HandlerFunc(server.serve))
	t.Cleanup(httpServer.Close)
	t.Setenv("CONVEYOR_ADDR", httpServer.URL)
	t.Setenv("CONVEYOR_API_TOKEN", "done-token")
	t.Setenv("CONVEYOR_WORKSPACE", "demo")
	return server
}

func (s *doneCleanupServer) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.paths = append(s.paths, r.Method+" "+r.URL.Path)
	s.mu.Unlock()
	if got := r.Header.Get("Authorization"); got != "Bearer done-token" {
		s.t.Errorf("Authorization = %q", got)
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/tasks/"+s.task.ID:
		_ = json.NewEncoder(w).Encode(s.task)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/tasks":
		_ = json.NewEncoder(w).Encode(append([]core.Task{}, s.holders...))
	case r.Method == http.MethodPost && r.URL.Path == "/v1/tasks/"+s.task.ID+"/worktree-cleanup":
		var record terminalCleanupRecord
		if err := json.NewDecoder(r.Body).Decode(&record); err != nil {
			s.t.Error(err)
		}
		s.mu.Lock()
		s.recorded = append(s.recorded, record)
		s.mu.Unlock()
		if s.status != http.StatusOK {
			http.Error(w, "worktree cleanup requires a claimed order", s.status)
			return
		}
		_ = json.NewEncoder(w).Encode(terminalCleanupReceipt{Completed: true, Recorded: true})
	default:
		s.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	}
}

func runDone(t *testing.T, taskID string) (string, string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	cmd := doneCmd()
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{taskID})
	err := cmd.Execute()
	return stdout.String(), stderr.String(), err
}

// conveyor done removes the worktree, then records the result on the user
// route only (DEC-95; component-git-delivery).
func TestDoneRecordsCleanupOnUserRouteOnly(t *testing.T) {
	fixture := newGitFixture(t)
	branch := "conveyor/task-recorded"
	destination, err := checkoutTask(context.Background(), branch, "main", "conveyor", fixture.origin, "recorded", "")
	if err != nil {
		t.Fatal(err)
	}
	server := newDoneCleanupServer(t, core.Task{ID: "recorded", Repo: "conveyor", Branch: branch, State: core.TaskMerged}, http.StatusOK)
	stdout, stderr, err := runDone(t, "recorded")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "worktree=removed branch=retained path="+destination) || strings.Contains(stderr, "not recorded") {
		t.Fatalf("stdout=%q stderr=%q", stdout, stderr)
	}
	if len(server.recorded) != 1 || server.recorded[0].Repository != "conveyor" || server.recorded[0].Branch != branch || server.recorded[0].Worktree != "removed" || server.recorded[0].BranchResult != "retained" || server.recorded[0].Path != destination {
		t.Fatalf("recorded = %+v", server.recorded)
	}
	for _, path := range server.paths {
		if strings.Contains(path, "/v1/worker/") {
			t.Fatalf("done called a worker route: %s", path)
		}
	}
	if _, err := os.Stat(destination); !os.IsNotExist(err) {
		t.Fatalf("worktree still exists: %v", err)
	}
}

// A refused recording leaves the local result and the exit status unchanged
// and prints one warning line.
func TestDoneRecordingRefusalWarnsWithoutFailing(t *testing.T) {
	fixture := newGitFixture(t)
	branch := "conveyor/task-refused"
	if _, err := checkoutTask(context.Background(), branch, "main", "conveyor", fixture.origin, "refused", ""); err != nil {
		t.Fatal(err)
	}
	server := newDoneCleanupServer(t, core.Task{ID: "refused", Repo: "conveyor", Branch: branch, State: core.TaskClosed}, http.StatusNotFound)
	stdout, stderr, err := runDone(t, "refused")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "worktree=removed") || strings.Count(stderr, "warning: worktree cleanup not recorded:") != 1 {
		t.Fatalf("stdout=%q stderr=%q", stdout, stderr)
	}
	if len(server.recorded) != 1 {
		t.Fatalf("recorded = %+v", server.recorded)
	}
}

// A branch that another open task holds is skipped, removes nothing, and is
// recorded as skipped (req-task-branch-assignment AC-3.5).
func TestDoneRecordsSkippedOccupiedBranch(t *testing.T) {
	fixture := newGitFixture(t)
	branch := "conveyor/task-shared"
	destination, err := checkoutTask(context.Background(), branch, "main", "conveyor", fixture.origin, "shared", "")
	if err != nil {
		t.Fatal(err)
	}
	holder := core.Task{ID: "holder", Repo: "conveyor", Branch: branch, State: core.TaskRunning}
	server := newDoneCleanupServer(t, core.Task{ID: "shared", Repo: "conveyor", Branch: branch, State: core.TaskMerged}, http.StatusOK, holder)
	stdout, _, err := runDone(t, "shared")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "worktree=skipped branch="+branch+" path=- other_task=holder") {
		t.Fatalf("stdout=%q", stdout)
	}
	if len(server.recorded) != 1 || server.recorded[0].Worktree != "skipped" {
		t.Fatalf("recorded = %+v", server.recorded)
	}
	if _, err := os.Stat(destination); err != nil {
		t.Fatalf("occupied worktree was removed: %v", err)
	}
}
