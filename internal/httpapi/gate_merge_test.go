package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/dispatch"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func gateMergeServer(t *testing.T) *Server {
	t.Helper()
	st := store.NewMemory()
	if err := st.CreateTask(store.WithActor(context.Background(), store.SystemActor()), core.Task{ID: "gate", State: core.TaskAwaiting, GateKind: core.GateKindMergeApproval}); err != nil {
		t.Fatal(err)
	}
	s := NewServer(st)
	s.BearerToken = "token"
	s.OnMerge = func(context.Context, core.Task) error { t.Error("approved-task merge used at the gate"); return nil }
	return s
}

// TestMergeTaskAtGateRoutesToGateMerge pins DEC-64(1): a merge POST at the
// merge_approval gate runs the one-act merge as the authenticated operator and
// answers readiness refusals as 409 {error, readiness}.
func TestMergeTaskAtGateRoutesToGateMerge(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		answer     error
		wantStatus int
		wantBody   string
	}{
		{name: "merged", wantStatus: http.StatusOK, wantBody: `"merged"`},
		{name: "not ready", answer: &dispatch.ErrGateMergeNotReady{State: "STALE"}, wantStatus: http.StatusConflict, wantBody: `"readiness":"STALE"`},
		{name: "unconfirmed", answer: dispatch.ErrGateMergeUnconfirmed, wantStatus: http.StatusConflict, wantBody: "not confirmed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := gateMergeServer(t)
			var actors []core.Actor
			s.OnGateMerge = func(_ context.Context, task core.Task, actor core.Actor) (core.Task, error) {
				actors = append(actors, actor)
				if tc.answer != nil {
					return task, tc.answer
				}
				task.State = core.TaskMerged
				return task, nil
			}
			request := httptest.NewRequest(http.MethodPost, "/v1/tasks/gate/merge", nil)
			request.Header.Set("Authorization", "Bearer token")
			response := httptest.NewRecorder()
			s.Handler().ServeHTTP(response, request)
			if response.Code != tc.wantStatus || !strings.Contains(response.Body.String(), tc.wantBody) || len(actors) != 1 || actors[0].ID == "" {
				t.Fatalf("status=%d body=%s actors=%+v", response.Code, response.Body.String(), actors)
			}
		})
	}
	s := gateMergeServer(t)
	s.OnGateMerge = func(context.Context, core.Task, core.Actor) (core.Task, error) {
		t.Error("unauthenticated merge reached the dispatcher")
		return core.Task{}, nil
	}
	response := httptest.NewRecorder()
	s.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/tasks/gate/merge", nil))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status=%d", response.Code)
	}
}

// TestNextActionMergeChoiceUsesLastKnownReadiness pins DEC-64(3): the
// next-action read offers merge from the cached gate peek and never calls the
// forge itself.
func TestNextActionMergeChoiceUsesLastKnownReadiness(t *testing.T) {
	t.Parallel()
	s := gateMergeServer(t)
	reads := 0
	s.OnMergeReadiness = func(context.Context, core.Task) (dispatch.MergeReadiness, error) {
		reads++
		return dispatch.MergeReadiness{State: "MERGEABLE", HeadSHA: "head", Number: 7}, nil
	}
	s.Workspace = "demo"
	ctx := store.WithWorkspace(context.Background(), "demo")
	task, err := s.Store.GetTask(store.WithActor(ctx, store.SystemActor()), "gate")
	if err != nil {
		t.Fatal(err)
	}
	refusal := func() string {
		opts := store.NextActionOptions{MergeReadiness: s.taskRunReadiness.lastKnown}
		action, err := store.ReadNextAction(store.WithActor(ctx, store.SystemActor()), s.Store, "gate", opts)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range action.Details.Refusals {
			if r.Choice == "merge" {
				return r.Text
			}
		}
		return ""
	}
	if got := refusal(); got != "merge readiness UNKNOWN" || reads != 0 {
		t.Fatalf("before peek refusal=%q reads=%d", got, reads)
	}
	// A peek cached for another approved head is not last-known for this one.
	if _, err := s.taskRunReadiness.read(ctx, task, "other-head", s.OnMergeReadiness); err != nil {
		t.Fatal(err)
	}
	if got := refusal(); got != "merge readiness UNKNOWN" || reads != 1 {
		t.Fatalf("other-key refusal=%q reads=%d", got, reads)
	}
	// The task detail read caches the gate peek for the next-action read.
	response := httptest.NewRecorder()
	s.Handler().ServeHTTP(response, authenticatedMemoryRead(s, httptest.NewRequest(http.MethodGet, "/v1/tasks/gate/activity", nil)))
	if response.Code != http.StatusOK {
		t.Fatalf("detail status=%d body=%s", response.Code, response.Body.String())
	}
	if got := refusal(); got != "" || reads != 2 {
		t.Fatalf("after peek refusal=%q reads=%d", got, reads)
	}
}
