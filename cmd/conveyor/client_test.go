package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
		_ = json.NewEncoder(w).Encode(core.Task{ID: "task-1"})
	}))
	defer srv.Close()

	c := &client{base: srv.URL, token: "secret-token", workspace: "engineering"}
	if _, err := c.createTask("fix it", "", "api", "main"); err != nil {
		t.Fatal(err)
	}
}

func TestClientWorkspaceConfigUpdateSendsIfMatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/v1/workspace/config" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("If-Match") != "7" || r.Header.Get("X-Conveyor-Actor") != "cli-operator" {
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
	if _, err := c.createTask("fix it", "", "api", "main"); err == nil {
		t.Fatal("expected missing-token error")
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
