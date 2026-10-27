package store_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/httpapi"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

// TestMemoryLineageDependencyRemovalSuppressesEdgeThroughHTTPAndRebuild adds
// a dependency, removes it, reads GET /v1/lineage/task/{id} without the
// depends_on edge, rebuilds with a fresh request ID with nothing projected,
// preserved, or ambiguous for the key, re-adds it, and reads the edge again
// with the re-add's provenance (component-lineage; req-260802-72fc68 AC-1.1,
// AC-1.3).
func TestMemoryLineageDependencyRemovalSuppressesEdgeThroughHTTPAndRebuild(t *testing.T) {
	const workspace, token = "lineage-dependency-memory", "lineage-dependency-token"
	st := store.NewMemoryWithConfig(&config.Config{Workspace: workspace, Repos: []config.Repo{{Name: "conveyor", Base: "main"}}})
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.Actor{ID: "operator", Role: core.ActorHuman}), workspace)
	now := time.Now().UTC()
	for _, id := range []string{"dependent", "dependency"} {
		if err := st.CreateTask(ctx, core.Task{ID: id, Workspace: workspace, Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + id, Title: id, State: core.TaskRunning, CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	server := httpapi.NewServer(st)
	server.Credentials = nil
	server.Workspace, server.BearerToken = workspace, token
	handler := server.Handler()
	readEdge := func(t *testing.T) (core.LineageLink, bool) {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, "/v1/lineage/task/dependent", nil)
		request.Header.Set("Authorization", "Bearer "+token)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("lineage status=%d body=%s", response.Code, response.Body.String())
		}
		var graph core.LineageTraversal
		if err := json.Unmarshal(response.Body.Bytes(), &graph); err != nil {
			t.Fatal(err)
		}
		for _, link := range graph.Links {
			if link.Kind == "depends_on" && link.SrcID == "dependent" && link.DstID == "dependency" {
				return link, true
			}
		}
		return core.LineageLink{}, false
	}
	additionEvent := func(t *testing.T) int64 {
		t.Helper()
		events, err := st.ListEvents(ctx, "dependent")
		if err != nil {
			t.Fatal(err)
		}
		var id int64
		for _, event := range events {
			if event.Kind == "task.dependency_added" {
				id = event.ID
			}
		}
		return id
	}
	rebuild := func(t *testing.T, requestID string) core.LineageRebuildResult {
		t.Helper()
		result, err := st.RebuildLineage(ctx, core.LineageRebuildRequest{Reason: "dependency removal", RequestID: requestID})
		if err != nil {
			t.Fatal(err)
		}
		if result.PreservedUnregenerable != 0 || result.Ambiguous != 0 || result.Unsupported != 0 {
			t.Fatalf("rebuild %s classification=%+v, want nothing preserved, ambiguous, or unsupported", requestID, result)
		}
		return result
	}

	if _, err := st.AddTaskDependency(ctx, store.DependencyAdditionRequest{TaskID: "dependent", DependsOnTaskID: "dependency", Reason: "order delivery", RequestID: "add-1"}); err != nil {
		t.Fatal(err)
	}
	if link, ok := readEdge(t); !ok || link.CreatedByEventID != additionEvent(t) {
		t.Fatalf("added edge=%+v present=%v", link, ok)
	}
	withEdge := rebuild(t, "rebuild-with-edge")
	if _, err := st.RemoveTaskDependency(ctx, store.DependencyRemovalRequest{TaskID: "dependent", DependsOnTaskID: "dependency", Reason: "no longer ordered", RequestID: "remove-1"}); err != nil {
		t.Fatal(err)
	}
	if link, ok := readEdge(t); ok {
		t.Fatalf("removed edge still served: %+v", link)
	}
	if removed := rebuild(t, "rebuild-after-removal"); removed.Projected != withEdge.Projected-1 {
		t.Fatalf("rebuild after removal projected=%d, want %d", removed.Projected, withEdge.Projected-1)
	}
	if link, ok := readEdge(t); ok {
		t.Fatalf("rebuild restored the removed edge: %+v", link)
	}
	if _, err := st.AddTaskDependency(ctx, store.DependencyAdditionRequest{TaskID: "dependent", DependsOnTaskID: "dependency", Reason: "ordered again", RequestID: "add-2"}); err != nil {
		t.Fatal(err)
	}
	readded := additionEvent(t)
	if link, ok := readEdge(t); !ok || link.CreatedByEventID != readded {
		t.Fatalf("re-added edge=%+v present=%v want event %d", link, ok, readded)
	}
	if restored := rebuild(t, "rebuild-after-readd"); restored.Projected != withEdge.Projected {
		t.Fatalf("rebuild after re-add projected=%d, want %d", restored.Projected, withEdge.Projected)
	}
	if link, ok := readEdge(t); !ok || link.CreatedByEventID != readded {
		t.Fatalf("re-added edge after rebuild=%+v present=%v want event %d", link, ok, readded)
	}
}
