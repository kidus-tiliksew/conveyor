package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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

func checkoutCheckpointFixture(t *testing.T, workerStatus int, runStatus int) (*client, *[]string, *map[string]string) {
	t.Helper()
	paths := &[]string{}
	bodies := &map[string]string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		*paths = append(*paths, r.URL.Path)
		(*bodies)[r.URL.Path] = string(body)
		switch r.URL.Path {
		case "/v1/worker/work-orders/order-1/attempt-checkpoint":
			if workerStatus >= 300 {
				http.Error(w, "worker refusal", workerStatus)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]bool{"created": true})
		case "/v1/tasks/task-1/run-orders/order-1/attempt-checkpoint":
			if runStatus >= 300 {
				http.Error(w, "run refusal", runStatus)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]bool{"created": true})
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)
	return &client{base: server.URL}, paths, bodies
}

var checkoutCheckpointRequest = core.WorkOrderAttemptCheckpoint{
	SessionID: "session-1", AttemptID: "attempt-prior",
	TerminationReason: "successor adopted dirty predecessor work", CommitSHA: "5555555555555555555555555555555555555555", PushResult: "pushed",
}

func TestDeliverCheckoutAttemptCheckpointStaysOnWorkerPlaneOnSuccess(t *testing.T) {
	c, paths, _ := checkoutCheckpointFixture(t, http.StatusOK, http.StatusOK)
	if err := c.deliverCheckoutAttemptCheckpointContext(t.Context(), "credential", "task-1", "order-1", checkoutCheckpointRequest); err != nil {
		t.Fatal(err)
	}
	if len(*paths) != 1 || (*paths)[0] != "/v1/worker/work-orders/order-1/attempt-checkpoint" {
		t.Fatalf("paths=%v", *paths)
	}
}

func TestDeliverCheckoutAttemptCheckpointFallsBackToRunPlaneOn401WithIdenticalPayload(t *testing.T) {
	c, paths, bodies := checkoutCheckpointFixture(t, http.StatusUnauthorized, http.StatusOK)
	if err := c.deliverCheckoutAttemptCheckpointContext(t.Context(), "credential", "task-1", "order-1", checkoutCheckpointRequest); err != nil {
		t.Fatal(err)
	}
	if len(*paths) != 2 || (*paths)[1] != "/v1/tasks/task-1/run-orders/order-1/attempt-checkpoint" {
		t.Fatalf("paths=%v", *paths)
	}
	worker := (*bodies)["/v1/worker/work-orders/order-1/attempt-checkpoint"]
	run := (*bodies)["/v1/tasks/task-1/run-orders/order-1/attempt-checkpoint"]
	if worker == "" || worker != run {
		t.Fatalf("fallback payload diverged: worker=%q run=%q", worker, run)
	}
}

func TestDeliverCheckoutAttemptCheckpointDoesNotFallBackOnNon401(t *testing.T) {
	c, paths, _ := checkoutCheckpointFixture(t, http.StatusConflict, http.StatusOK)
	err := c.deliverCheckoutAttemptCheckpointContext(t.Context(), "credential", "task-1", "order-1", checkoutCheckpointRequest)
	if err == nil || !strings.Contains(err.Error(), "worker refusal") {
		t.Fatalf("err=%v", err)
	}
	if len(*paths) != 1 {
		t.Fatalf("non-401 worker failure triggered fallback: paths=%v", *paths)
	}
}

func TestDeliverCheckoutAttemptCheckpointReportsBothRefusals(t *testing.T) {
	c, paths, _ := checkoutCheckpointFixture(t, http.StatusUnauthorized, http.StatusConflict)
	err := c.deliverCheckoutAttemptCheckpointContext(t.Context(), "credential", "task-1", "order-1", checkoutCheckpointRequest)
	if err == nil || !strings.Contains(err.Error(), "worker refusal") || !strings.Contains(err.Error(), "run refusal") {
		t.Fatalf("err=%v", err)
	}
	if len(*paths) != 2 {
		t.Fatalf("paths=%v", *paths)
	}
}
