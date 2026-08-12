package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	workerservice "github.com/kidus-tiliksew/conveyor/internal/worker"
)

func TestReleaseTaskRunOrderUsesServerWireFormat(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/tasks/task-1/run-orders/order-1/release" {
			t.Fatalf("path=%q", r.URL.Path)
		}
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		for key, want := range map[string]string{
			"session_id": "session-1", "reason": "done", "release_cause": "operator_action",
			"outcome": "released", "failure_detail": "detail",
		} {
			if payload[key] != want {
				t.Fatalf("%s=%v, want %q (payload=%v)", key, payload[key], want, payload)
			}
		}
		_ = json.NewEncoder(w).Encode(core.WorkOrder{ID: "order-1"})
	}))
	defer server.Close()

	c := &client{base: server.URL}
	item := workerservice.DispatchOrder{Task: core.Task{ID: "task-1"}, Order: core.WorkOrder{ID: "order-1"}}
	if err := c.releaseTaskRunOrderContext(t.Context(), "credential", item, core.WorkOrderRelease{
		SessionID: "session-1", Reason: "done", Cause: core.WorkOrderReleaseCauseOperatorAction,
		Outcome: core.WorkOrderOutcomeReleased, FailureDetail: "detail",
	}); err != nil {
		t.Fatal(err)
	}
}
