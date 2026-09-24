package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	workerservice "github.com/kidus-tiliksew/conveyor/internal/worker"
	"github.com/kidus-tiliksew/conveyor/internal/workorder"
)

func TestContextRefreshTransportParityAndConfinement(t *testing.T) {
	for _, channel := range []string{"user", "agent", "worker"} {
		t.Run(channel, func(t *testing.T) {
			ctx := store.WithWorkspace(t.Context(), "demo")
			st := store.NewMemory()
			if e := st.CreateTask(ctx, core.Task{ID: "fresh", Workspace: "demo", State: core.TaskRunning}); e != nil {
				t.Fatal(e)
			}
			if e := st.CreateJob(ctx, core.Job{ID: "order", TaskID: "fresh", State: core.JobPending, Stage: core.StageImplement}); e != nil {
				t.Fatal(e)
			}
			if e := storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: "order", JobID: "order", TaskID: "fresh", Stage: core.StageImplement, State: core.WorkOrderQueued}); e != nil {
				t.Fatal(e)
			}
			claim := core.WorkOrderClaim{ClaimantID: core.TaskRunClaimantID("owner"), SessionID: "session", ClientToken: "token", Lease: time.Minute}
			credential := core.AuthenticatedCredential{ID: "user-token", OwnerUserID: "owner", Kind: core.CredentialUser}
			if channel == "agent" {
				credential.Kind = core.CredentialAgent
				credential.RunWorkspaceID = "demo"
				credential.RunWorkOrderID = "order"
				credential.RunSessionID = "session"
			}
			if channel == "worker" {
				claim.WorkerID = "worker"
				claim.ClaimantID = "worker"
				if e := st.CreateWorker(ctx, core.Worker{ID: "worker", Workspace: "demo", OwnerUserID: "owner"}); e != nil {
					t.Fatal(e)
				}
			}
			if _, e := storetest.For(st).ClaimWorkOrder(ctx, "order", claim); e != nil {
				t.Fatal(e)
			}
			actor := store.Actor{ID: store.UserActorID("owner"), Role: core.ActorUser}
			if channel == "agent" {
				actor = store.Actor{ID: store.AgentActorID(credential.ID), Role: core.ActorAgent}
			}
			ctx = store.WithActor(store.WithCredential(ctx, credential), actor)
			if channel == "worker" {
				ctx = context.WithValue(store.WithActor(ctx, store.Actor{ID: "worker:worker", Role: core.ActorWorker}), workerContextKey{}, core.Worker{ID: "worker", Workspace: "demo"})
			}
			server := NewServer(st)
			server.Workspace = "demo"
			server.WorkOrders = &workorder.Service{Store: st}
			server.Workers = &workerservice.Service{Store: st, WorkOrders: server.WorkOrders}
			request := httptest.NewRequest(http.MethodPost, "/mcp", nil).WithContext(ctx)
			args := map[string]any{"workspace_id": "demo", "work_order_id": "order", "session_id": "session"}
			raw, e := server.callMCPTool(request, "refresh_work_order_context", args)
			if e != nil {
				t.Fatal(e)
			}
			first := raw.(core.ContextFreshness)
			if !first.ObservationRecorded {
				t.Fatal(first)
			}
			artifact, e := st.CreateArtifact(ctx, core.Artifact{Name: "correction.md", ContentType: "text/markdown", TaskID: "fresh"}, []byte("correction"))
			if e != nil {
				t.Fatal(e)
			}
			args["prior_revision"] = first.SelectionRevision
			raw, e = server.callMCPTool(request, "refresh_work_order_context", args)
			if e != nil {
				t.Fatal(e)
			}
			next := raw.(core.ContextFreshness)
			if next.Additions.Count != 1 || next.Additions.Items[0].ArtifactID != artifact.ID || next.UnfetchedAdditions != 1 {
				t.Fatal(next)
			}
			body := maps.Clone(args)
			delete(body, "work_order_id")
			data, _ := json.Marshal(body)
			path := "/v1/work-orders/order/context-refresh"
			if channel == "worker" {
				path = "/v1/worker/work-orders/order/context-refresh"
			}
			r := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(data)).WithContext(ctx)
			route := chi.NewRouteContext()
			route.URLParams.Add("id", "order")
			r = r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, route))
			w := httptest.NewRecorder()
			server.refreshWorkOrderContext(w, r)
			if w.Code != http.StatusOK {
				t.Fatal(w.Code, w.Body.String())
			}
			var httpResult core.ContextFreshness
			if e = json.Unmarshal(w.Body.Bytes(), &httpResult); e != nil {
				t.Fatal(e)
			}
			if httpResult.SelectionRevision != next.SelectionRevision || httpResult.ObservationRevision != next.ObservationRevision || httpResult.Additions.Digest != next.Additions.Digest || !httpResult.ObservedAt.Equal(next.ObservedAt) {
				t.Fatal("transport replay/parity diverged")
			}
			for _, bad := range []map[string]any{{"workspace_id": "foreign"}, {"work_order_id": "foreign"}, {"session_id": "foreign"}} {
				values := maps.Clone(args)
				maps.Copy(values, bad)
				if _, e = server.callMCPTool(request, "refresh_work_order_context", values); e == nil {
					t.Fatal("foreign scope admitted")
				}
			}
		})
	}
}
func TestContextRefreshStrictWireArguments(t *testing.T) {
	valid := map[string]any{"workspace_id": "demo", "work_order_id": "order", "session_id": "session"}
	for _, change := range []map[string]any{{"actor": "injected"}, {"workspace_id": 1}, {"session_id": nil}, {"work_order_id": strings.Repeat("x", 257)}, {"prior_revision": "bad"}, {"prior_revision": strings.Repeat("A", 64)}, {"session_id": " "}} {
		args := maps.Clone(valid)
		maps.Copy(args, change)
		if validateContextRefreshArgs(args) == nil {
			t.Fatal(fmt.Sprint(change))
		}
	}
	server := NewServer(store.NewMemory())
	w := httptest.NewRecorder()
	server.refreshWorkOrderContext(w, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"workspace_id":"demo","workspace_id":"foreign"}`)))
	if w.Code != http.StatusBadRequest {
		t.Fatal("duplicate fields accepted")
	}
}
