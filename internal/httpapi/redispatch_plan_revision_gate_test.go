package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func postRedispatchAtGate(server *Server, taskID, requestID string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "/v1/tasks/"+taskID+"/redispatch", strings.NewReader(`{}`))
	request.Header.Set("Authorization", "Bearer token")
	request.Header.Set("X-Workspace-ID", "demo")
	request.Header.Set("X-Request-Id", requestID)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	return response
}

// requireGateUntouched proves a refused redispatch moved nothing and appended
// nothing, and that the route requested no dispatch.
func requireGateUntouched(t *testing.T, ctx context.Context, st store.Store, taskID string, eventsBefore, dispatched int) {
	t.Helper()
	task, err := st.GetTask(ctx, taskID)
	if err != nil || task.State != core.TaskAwaiting || task.GateKind != core.GateKindPlanRevision {
		t.Fatalf("task = %+v err=%v", task, err)
	}
	events, err := st.ListEvents(ctx, taskID)
	if err != nil || len(events) != eventsBefore {
		t.Fatalf("events = %d err=%v, want %d", len(events), err, eventsBefore)
	}
	if dispatched != 0 {
		t.Fatalf("refused redispatch dispatched %d tasks", dispatched)
	}
}

// TestRedispatchAtUnreadableGateKeepsTheCauseInTheLog proves the refusal's
// fixed text reaches the caller while the cause reaches the log with the request
// ID, so the failure is not lost and no event text is echoed
// (component-work-orders, State and failure; DEC-105(1)).
func TestRedispatchAtUnreadableGateKeepsTheCauseInTheLog(t *testing.T) {
	const canary = "fake-redispatch-event-canary"
	ctx, st, server, taskID, _ := newPlanRevisionReviewServer(t)
	dispatched := 0
	server.OnCreate = func(context.Context, string) { dispatched++ }
	if err := st.AppendEvent(ctx, core.Event{TaskID: taskID, Kind: "task.state_changed", At: time.Now().UTC().Add(time.Minute), Payload: json.RawMessage(`{"command":7,"detail":"` + canary + `"}`)}); err != nil {
		t.Fatal(err)
	}
	events, err := st.ListEvents(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	captured := captureStandardLog(t)
	response := postRedispatchAtGate(server, taskID, "gate-request-7")
	if response.Code != http.StatusConflict || strings.TrimSpace(response.Body.String()) != core.TaskRedispatchPlanRevisionUnreadableRefusal {
		t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
	}
	requireGateUntouched(t, ctx, st, taskID, len(events), dispatched)
	logged := captured.String()
	if !strings.Contains(logged, `request_id="gate-request-7"`) || !strings.Contains(logged, store.ErrPlanRevisionGateUnreadable.Error()) {
		t.Fatalf("log lacks the request ID or cause: %q", logged)
	}
	if strings.Contains(logged, canary) {
		t.Fatalf("log carries event text: %q", logged)
	}
}

// TestRedispatchGateReadStoreFailureIsTheGeneric500 proves a store failure while
// reading the gate writes nothing and is not reported as a refusal, and that no
// driver text reaches the response (DEC-105(1); component-http-api invariant
// 16).
func TestRedispatchGateReadStoreFailureIsTheGeneric500(t *testing.T) {
	ctx, st, server, taskID, _ := newPlanRevisionReviewServer(t)
	dispatched := 0
	server.OnCreate = func(context.Context, string) { dispatched++ }
	server.Store = newFailingStore(st, "ListCompactEvents")
	events, err := st.ListEvents(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	captured := captureStandardLog(t)
	response := postRedispatchAtGate(server, taskID, "gate-request-8")
	if response.Code != http.StatusInternalServerError || strings.TrimSpace(response.Body.String()) != "internal server error" {
		t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
	}
	requireGateUntouched(t, ctx, st, taskID, len(events), dispatched)
	if logged := captured.String(); !strings.Contains(logged, `request_id="gate-request-8"`) || strings.Contains(logged, failingStoreCredential) {
		t.Fatalf("log = %q, want the request ID and no credential", logged)
	}
}

// gateReadFailsAfterFirstGetTask passes the route's own task read and fails the
// gate's later one, as a task lost between the two reads does.
type gateReadFailsAfterFirstGetTask struct {
	store.Store
	err   error
	reads int
}

func (s *gateReadFailsAfterFirstGetTask) GetTask(ctx context.Context, id string) (core.Task, error) {
	s.reads++
	if s.reads > 1 {
		return core.Task{}, s.err
	}
	return s.Store.GetTask(ctx, id)
}

// TestRedispatchGateReadClassifiesItsTaskRead proves the gate's own task read
// answers not-found as a refusal and any other failure as the generic 500, with
// a fixed text and no error text in either.
func TestRedispatchGateReadClassifiesItsTaskRead(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		code int
		body string
	}{
		{"not found", store.ErrNotFound, http.StatusNotFound, "task not found"},
		{"driver failure", errors.New(failingStoreDriverText), http.StatusInternalServerError, "internal server error"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, st, server, taskID, _ := newPlanRevisionReviewServer(t)
			dispatched := 0
			server.OnCreate = func(context.Context, string) { dispatched++ }
			server.Store = &gateReadFailsAfterFirstGetTask{Store: st, err: test.err}
			events, err := st.ListEvents(ctx, taskID)
			if err != nil {
				t.Fatal(err)
			}
			response := postRedispatchAtGate(server, taskID, "gate-request-9")
			if response.Code != test.code || strings.TrimSpace(response.Body.String()) != test.body {
				t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
			}
			requireGateUntouched(t, ctx, st, taskID, len(events), dispatched)
		})
	}
}
