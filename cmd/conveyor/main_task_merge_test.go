package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
)

type taskMergeRequest struct {
	method, path, authorization, workspace string
}

func runTaskMergeCommand(t *testing.T, handler http.HandlerFunc) (string, error, []taskMergeRequest) {
	t.Helper()
	isolateLocalAuthTest(t)
	var requests []taskMergeRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, taskMergeRequest{r.Method, r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("X-Workspace-ID")})
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	t.Setenv("CONVEYOR_ADDR", server.URL)
	t.Setenv("CONVEYOR_API_TOKEN", "operator-secret-token")
	t.Setenv("CONVEYOR_WORKSPACE", "demo")
	var output bytes.Buffer
	command := mergeTaskCmd()
	command.SetArgs([]string{"task-1"})
	command.SetOut(&output)
	command.SetErr(&output)
	command.SilenceUsage, command.SilenceErrors = true, true
	err := command.Execute()
	return output.String(), err, requests
}

func TestTaskMergeCommandPostsExplicitMergeAndPrintsMergedState(t *testing.T) {
	output, err, requests := runTaskMergeCommand(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(core.Task{ID: "task-1", State: core.TaskMerged})
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []taskMergeRequest{{http.MethodPost, "/v1/tasks/task-1/merge", "Bearer operator-secret-token", "demo"}}
	if len(requests) != 1 || requests[0] != want[0] {
		t.Fatalf("requests = %+v, want %+v", requests, want)
	}
	if output != "merged task task-1 (state merged)\n" {
		t.Fatalf("output = %q", output)
	}
}

func TestTaskMergeCommandSurfacesConflictMessagesAndFails(t *testing.T) {
	for _, message := range []string{"task is not approved for merge", "merge was not confirmed by the forge"} {
		t.Run(message, func(t *testing.T) {
			output, err, requests := runTaskMergeCommand(t, func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, message, http.StatusConflict)
			})
			if err == nil || !strings.Contains(err.Error(), "409") || !strings.Contains(err.Error(), message) {
				t.Fatalf("err = %v, want the 409 message %q", err, message)
			}
			if len(requests) != 1 || requests[0].path != "/v1/tasks/task-1/merge" {
				t.Fatalf("requests = %+v", requests)
			}
			if strings.Contains(output, "merged task") {
				t.Fatalf("refusal printed success: %q", output)
			}
		})
	}
}

func TestTaskMergeCommandRejectsUnconfirmedResponse(t *testing.T) {
	output, err, _ := runTaskMergeCommand(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(core.Task{ID: "task-1", State: core.TaskApproved})
	})
	if err == nil || !strings.Contains(err.Error(), `state "approved"`) {
		t.Fatalf("err = %v", err)
	}
	if output != "" {
		t.Fatalf("output = %q", output)
	}
}
