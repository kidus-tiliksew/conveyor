package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
)

func TestClientSendsBearerTokenOnCreate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/tasks" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer secret-token" {
			t.Fatalf("Authorization = %q", got)
		}
		if got := r.Header.Get("X-Workspace-ID"); got != "engineering" {
			t.Fatalf("X-Workspace-ID = %q", got)
		}
		var input map[string]any
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Fatal(err)
		}
		if input["body"] != "fix it" {
			t.Fatalf("body = %#v", input)
		}
		if input["setup"] != "backend" {
			t.Fatalf("setup = %#v", input)
		}
		if _, supplied := input["title"]; supplied {
			t.Fatalf("CLI still sends title: %#v", input)
		}
		_ = json.NewEncoder(w).Encode(core.Task{ID: "task-1"})
	}))
	defer srv.Close()

	c := &client{base: srv.URL, token: "secret-token", workspace: "engineering"}
	if _, err := c.createTaskWithSetup("fix it", "api", "main", false, nil, nil, "backend"); err != nil {
		t.Fatal(err)
	}
}

func TestClientWorkspaceConfigUpdateSendsIfMatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/v1/workspace/config" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("If-Match") != "7" || r.Header.Get("X-Conveyor-Actor") != "" {
			t.Fatalf("headers = %#v", r.Header)
		}
		_ = json.NewEncoder(w).Encode(config.UpdateReceipt{VersionedDocument: config.VersionedDocument{Version: 8}, EventID: 9})
	}))
	defer srv.Close()
	c := &client{base: srv.URL, token: "secret-token"}
	receipt, err := c.updateWorkspaceConfig(config.WorkspaceDocument{Workspace: "demo"}, 7)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Version != 8 || receipt.EventID != 9 {
		t.Fatalf("receipt = %+v", receipt)
	}
}

func TestClientRefusesCreateWithoutToken(t *testing.T) {
	c := &client{base: "http://unused"}
	if _, err := c.createTask("fix it", "api", "main"); err == nil {
		t.Fatal("expected missing-token error")
	}
}

func TestTaskNewDoesNotAcceptATitleArgument(t *testing.T) {
	command := taskCmd()
	newCommand, _, err := command.Find([]string{"new"})
	if err != nil {
		t.Fatal(err)
	}
	if newCommand.Use != "new" {
		t.Fatalf("use = %q", newCommand.Use)
	}
	if err := newCommand.Args(newCommand, []string{"manual title"}); err == nil {
		t.Fatal("task new accepted a title argument")
	}
}

func TestClientRedispatchUsesAuthenticatedEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/tasks/task-1/redispatch" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer secret-token" {
			t.Fatalf("Authorization = %q", got)
		}
		_ = json.NewEncoder(w).Encode(core.Task{ID: "task-1", State: core.TaskQueued})
	}))
	defer srv.Close()
	c := &client{base: srv.URL, token: "secret-token"}
	task, err := c.redispatchTask("task-1")
	if err != nil {
		t.Fatal(err)
	}
	if task.State != core.TaskQueued {
		t.Fatalf("state = %s", task.State)
	}
}

func TestClientReviewUsesAuthenticatedEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/tasks/task-1/review" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer secret-token" {
			t.Fatalf("Authorization = %q", got)
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["action"] != "redirect" || body["reason_code"] != "spec-wrong" {
			t.Fatalf("body = %v", body)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"task": core.Task{ID: "task-1", State: core.TaskQueued}})
	}))
	defer srv.Close()
	c := &client{base: srv.URL, token: "secret-token"}
	task, err := c.reviewTask("task-1", core.InterventionRedirect, "spec-wrong", "retry")
	if err != nil {
		t.Fatal(err)
	}
	if task.State != core.TaskQueued {
		t.Fatalf("state = %s", task.State)
	}
}

func TestClientCloseTaskUsesAuthenticatedEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/tasks/task-1/close" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer secret-token" {
			t.Fatalf("Authorization = %q", got)
		}
		if got := r.Header.Get("X-Workspace-ID"); got != "demo" {
			t.Fatalf("X-Workspace-ID = %q", got)
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["reason"] != "obsolete" {
			t.Fatalf("body = %v", body)
		}
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(core.Task{ID: "task-1", State: core.TaskClosed})
	}))
	defer srv.Close()
	c := &client{base: srv.URL, token: "secret-token", workspace: "demo"}
	task, err := c.closeTask("task-1", "obsolete")
	if err != nil {
		t.Fatal(err)
	}
	if task.State != core.TaskClosed {
		t.Fatalf("state = %s", task.State)
	}
}

func TestClientAttachTaskBranchSuccessAndSameNameNoop(t *testing.T) {
	var posts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/tasks/task-1/branch" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer secret-token" {
			t.Fatalf("Authorization = %q", got)
		}
		if got := r.Header.Get("X-Workspace-ID"); got != "demo" {
			t.Fatalf("X-Workspace-ID = %q", got)
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		posts++
		_ = json.NewEncoder(w).Encode(core.Task{ID: "task-1", Branch: body["branch"]})
	}))
	defer srv.Close()
	c := &client{base: srv.URL, token: "secret-token", workspace: "demo"}
	task, err := c.attachTaskBranch("task-1", "feature/adopt")
	if err != nil || task.Branch != "feature/adopt" {
		t.Fatalf("attach = %+v err=%v", task, err)
	}
	same, err := c.attachTaskBranch("task-1", "feature/adopt")
	if err != nil || same.Branch != "feature/adopt" || posts != 2 {
		t.Fatalf("same-name = %+v posts=%d err=%v", same, posts, err)
	}
}

func TestClientAttachTaskBranchNamedRefusals(t *testing.T) {
	for _, test := range []struct {
		code        string
		message     string
		otherTaskID string
		status      int
		want        string
	}{
		{code: "invalid_branch", message: "invalid_branch", status: http.StatusBadRequest, want: "invalid_branch"},
		{code: "task_terminal", message: "task is terminal", status: http.StatusConflict, want: "task_terminal"},
		{code: "work_order_claimed", message: "work order claimed", status: http.StatusConflict, want: "work_order_claimed"},
		{code: "pull_request_recorded", message: "pull request recorded", status: http.StatusConflict, want: "pull_request_recorded"},
		{code: "branch_not_attachable", message: "branch not attachable", status: http.StatusConflict, want: "branch_not_attachable"},
		{code: "branch_in_use", message: "already held", otherTaskID: "other-task", status: http.StatusConflict, want: "branch_in_use: branch already belongs to task other-task"},
	} {
		t.Run(test.code, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/tasks/task-1/branch" {
					t.Fatalf("path = %s", r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(test.status)
				payload := map[string]string{"error": test.code, "message": test.message}
				if test.otherTaskID != "" {
					payload["other_task_id"] = test.otherTaskID
				}
				_ = json.NewEncoder(w).Encode(payload)
			}))
			defer srv.Close()
			c := &client{base: srv.URL, token: "secret-token", workspace: "demo"}
			_, err := c.attachTaskBranch("task-1", "feature/x")
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v want %q", err, test.want)
			}
			var named *attachBranchError
			if !errors.As(err, &named) || named.Code != test.code || named.OtherTaskID != test.otherTaskID {
				t.Fatalf("named = %+v err=%v", named, err)
			}
		})
	}
}
