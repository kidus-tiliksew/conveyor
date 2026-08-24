package postgres

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/httpapi"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func TestRequirementStalenessAcknowledgmentSurvivesRestart(t *testing.T) {
	databaseURL := integrationDatabaseURL(t)
	st, baseCtx, workspace := newPhase61IntegrationStore(t)
	ctx := store.WithActor(baseCtx, store.Actor{ID: "restart-operator", Role: core.ActorHuman})
	requirement, v1, err := st.CreateRequirement(ctx, core.Requirement{ID: "req-restart-" + core.NewTaskID(), Title: "Restart-safe acknowledgment"}, core.RequirementVersion{
		Content: "First intent.", Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Acknowledgments survive restart."}}, Origin: core.RequirementOriginOperator,
	})
	if err != nil {
		st.Close()
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmRequirementVersion(ctx, requirement.ID, v1.Version); err != nil {
		st.Close()
		t.Fatal(err)
	}
	task := core.Task{ID: "restart-delivery-" + core.NewTaskID(), Workspace: workspace, Title: "Restart delivery", Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-restart-delivery", State: core.TaskMerged, CreatedAt: time.Now().UTC()}
	if err = st.CreateTask(ctx, task); err != nil {
		st.Close()
		t.Fatal(err)
	}
	// Legacy context events did not record a version. The classifier reconstructs
	// the confirmed version effective at this append-only event.
	if err = st.AppendEvent(ctx, core.Event{TaskID: task.ID, Kind: store.TaskContextRequirementAdded, Payload: core.JSONPayload(map[string]any{"id": requirement.ID})}); err != nil {
		st.Close()
		t.Fatal(err)
	}
	v2, err := st.ProposeRequirementVersion(ctx, core.RequirementVersion{RequirementID: requirement.ID, Content: "Second intent.", Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Acknowledgments survive restart and later delivery."}}, Origin: core.RequirementOriginOperator})
	if err != nil {
		st.Close()
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmRequirementVersion(ctx, requirement.ID, v2.Version); err != nil {
		st.Close()
		t.Fatal(err)
	}
	firstMerge := time.Now().UTC().Add(time.Minute)
	if err = st.AppendEvent(ctx, core.Event{TaskID: task.ID, Kind: "merge.confirmed", At: firstMerge, Payload: core.JSONPayload(map[string]any{"url": "https://example.test/pull/restart"})}); err != nil {
		st.Close()
		t.Fatal(err)
	}

	type view struct {
		Staleness struct {
			DeliveryAfterIntent bool `json:"delivery_after_intent"`
			Deliveries          []struct {
				SignalID       string `json:"signal_id"`
				Label          string `json:"label"`
				URL            string `json:"url"`
				PinnedVersion  int    `json:"pinned_version"`
				CurrentVersion int    `json:"current_version"`
			} `json:"deliveries"`
		} `json:"staleness"`
	}
	read := func(server *httpapi.Server) view {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/v1/requirements/"+requirement.ID, nil)
		request.Header.Set("Authorization", "Bearer token")
		server.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("read status=%d body=%s", response.Code, response.Body.String())
		}
		var result view
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	server := httpapi.NewServer(st)
	server.Credentials = nil // exercise the explicit memory-style shared-token fixture
	server.Workspace, server.BearerToken = workspace, "token"
	before := read(server)
	if !before.Staleness.DeliveryAfterIntent || len(before.Staleness.Deliveries) != 1 ||
		before.Staleness.Deliveries[0].Label != task.Title ||
		before.Staleness.Deliveries[0].URL != "https://example.test/pull/restart" ||
		before.Staleness.Deliveries[0].PinnedVersion != v1.Version ||
		before.Staleness.Deliveries[0].CurrentVersion != v2.Version {
		st.Close()
		t.Fatalf("initial staleness=%+v", before.Staleness)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/requirements/"+requirement.ID+"/staleness/"+before.Staleness.Deliveries[0].SignalID+"/acknowledge", nil)
	request.Header.Set("Authorization", "Bearer token")
	request.Header.Set("X-Conveyor-Actor", "restart-operator")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		st.Close()
		t.Fatalf("acknowledge status=%d body=%s", response.Code, response.Body.String())
	}
	st.Close()

	restarted, err := Open(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restarted.Close)
	restartedCtx := store.WithActor(store.WithWorkspace(t.Context(), workspace), store.Actor{ID: "restart-operator", Role: core.ActorHuman})
	restartedServer := httpapi.NewServer(restarted)
	restartedServer.Credentials = nil // preserve the fixture's synthetic shared token after restart
	restartedServer.Workspace, restartedServer.BearerToken = workspace, "token"
	after := read(restartedServer)
	if after.Staleness.DeliveryAfterIntent || len(after.Staleness.Deliveries) != 0 {
		t.Fatalf("acknowledgment was lost after restart: %+v", after.Staleness)
	}
	if err = restarted.AppendEvent(restartedCtx, core.Event{TaskID: task.ID, Kind: "merge.confirmed", At: firstMerge}); err != nil {
		t.Fatal(err)
	}
	later := read(restartedServer)
	if !later.Staleness.DeliveryAfterIntent || len(later.Staleness.Deliveries) != 1 || later.Staleness.Deliveries[0].SignalID == before.Staleness.Deliveries[0].SignalID {
		t.Fatalf("later delivery did not re-raise after restart: %+v", later.Staleness)
	}
}

func TestRequirementReviewedReconciliationSurvivesRestart(t *testing.T) {
	databaseURL := integrationDatabaseURL(t)
	st, ctx, workspace := newPhase61IntegrationStore(t)
	requirement, version, err := st.CreateRequirement(ctx, core.Requirement{ID: "req-reconciled-" + core.NewTaskID(), Title: "Reviewed reconciliation"}, core.RequirementVersion{
		Content: "Reviewed delivery remains trusted after recovery.", Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Classify delivery from review provenance."}}, Origin: core.RequirementOriginOperator,
	})
	if err != nil {
		st.Close()
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmRequirementVersion(ctx, requirement.ID, version.Version); err != nil {
		st.Close()
		t.Fatal(err)
	}
	task := core.Task{ID: "reviewed-reconciliation-" + core.NewTaskID(), Workspace: workspace, Title: "Reviewed recovery", Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-reviewed-recovery", State: core.TaskMerged, CreatedAt: time.Now().UTC()}
	if err = st.CreateTask(ctx, task); err != nil {
		st.Close()
		t.Fatal(err)
	}
	confirmedAt := time.Now().UTC().Add(time.Second)
	if err = st.AppendEvent(ctx, core.Event{TaskID: task.ID, Kind: store.TaskContextRequirementAdded, At: confirmedAt.Add(time.Second), Payload: core.JSONPayload(map[string]any{"id": requirement.ID, "version": version.Version})}); err != nil {
		st.Close()
		t.Fatal(err)
	}
	if err = st.AppendEvent(ctx, core.Event{TaskID: task.ID, Kind: "review.round_completed", At: confirmedAt.Add(2 * time.Second), Payload: core.JSONPayload(map[string]any{
		"review_round": 1, "verdict": "approve", "approved_head_sha": "reviewed-head",
	})}); err != nil {
		st.Close()
		t.Fatal(err)
	}
	if err = st.AppendEvent(ctx, core.Event{TaskID: task.ID, Kind: "merge.reconciled", At: confirmedAt.Add(3 * time.Second), Payload: core.JSONPayload(map[string]any{
		"url": "https://example.test/pull/642", "head_sha": "reviewed-head", "result": "already_merged",
	})}); err != nil {
		st.Close()
		t.Fatal(err)
	}
	st.Close()

	restarted, err := Open(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restarted.Close)
	restartedCtx := store.WithWorkspace(t.Context(), workspace)
	eventsByTask, err := restarted.ListRequirementDeliveryEventsForTasks(restartedCtx, []string{task.ID})
	if err != nil {
		t.Fatal(err)
	}
	kinds := make([]string, 0, len(eventsByTask[task.ID]))
	for _, event := range eventsByTask[task.ID] {
		kinds = append(kinds, event.Kind)
	}
	if !slices.Contains(kinds, "review.round_completed") || !slices.Contains(kinds, "merge.reconciled") {
		t.Fatalf("persisted delivery evidence kinds=%v", kinds)
	}

	server := httpapi.NewServer(restarted)
	server.Credentials = nil
	server.Workspace, server.BearerToken = workspace, "token"
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/v1/requirements/"+requirement.ID, nil)
	request.Header.Set("Authorization", "Bearer token")
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("requirements status=%d body=%s", response.Code, response.Body.String())
	}
	var view struct {
		Staleness struct {
			DeliveryAfterIntent bool `json:"delivery_after_intent"`
			Deliveries          []struct {
				NeedsAttention bool     `json:"needs_attention"`
				Reasons        []string `json:"reasons"`
			} `json:"deliveries"`
		} `json:"staleness"`
	}
	if err = json.Unmarshal(response.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.Staleness.DeliveryAfterIntent || len(view.Staleness.Deliveries) != 1 || view.Staleness.Deliveries[0].NeedsAttention || len(view.Staleness.Deliveries[0].Reasons) != 0 {
		t.Fatalf("reviewed reconciliation staleness=%+v", view.Staleness)
	}
}
