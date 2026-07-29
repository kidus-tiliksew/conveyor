package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/monitor"
	"github.com/kidus-tiliksew/conveyor/internal/store"
)

func TestMonitorObservationUsesNormalIntakeAndExposesDrift(t *testing.T) {
	st := store.NewMemory()
	server := NewServer(st)
	server.Workspace, server.Repos, server.BearerToken = "demo", []string{"conveyor"}, "secret"
	frozen := config.ExecutionSetup{
		Name: "monitor-default",
		ExecutionSettings: config.ContextualExecutionSettings{
			Implementation: config.ImplementationSettings{Harness: "codex", Model: "gpt-5.6", ModelPolicy: config.ModelPolicyExplicit, TimeoutText: "4h"},
			Review:         config.ReviewExecutionSettings{Execution: config.ExecutionMCP, TimeoutText: "1h"},
		},
		Review: config.ReviewPanel{Seats: []config.ReviewSeat{{Model: "gpt-review"}}},
	}
	server.ConfigProvider = func(context.Context) (*config.Config, error) {
		return &config.Config{
			Workspace: "demo", Repos: []config.Repo{{Name: "conveyor", Base: "main"}},
			DefaultSetup: frozen.Name, Setups: []config.ExecutionSetup{frozen},
			Execution: config.ExecutionPolicy{SpecApproval: true, MergeApproval: true},
		}, nil
	}
	server.GenerateTaskTitle = func(context.Context, core.Task) (string, error) { return "Reconcile outside change", nil }
	enqueued := 0
	server.OnCreate = func(context.Context, string) { enqueued++ }
	server.Monitor = &monitor.Service{
		Store: st.(monitor.Store), WorkspaceID: "demo", Enabled: true,
		Repositories: map[string]struct{}{"conveyor": {}},
	}
	server.Monitor.Intake = server.CreateMonitorTask
	handler := server.Handler()
	hints, err := monitor.ParseHints([]byte("version: 1\ntriage_areas: [control-plane]\n"), "abc")
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(monitor.Observation{
		Repository: "conveyor", Kind: monitor.DirectPush, OccurrenceID: "commit:abc",
		SourceURL: "https://github.com/acme/conveyor/commit/abc", CommitSHA: "abc", Hints: &hints,
	})
	post := func() *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/v1/monitor/observations", bytes.NewReader(payload))
		request.Header.Set("Authorization", "Bearer secret")
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	first, second := post(), post()
	if first.Code != http.StatusAccepted || second.Code != http.StatusAccepted {
		t.Fatalf("first=%d %s second=%d %s", first.Code, first.Body.String(), second.Code, second.Body.String())
	}
	tasks, err := st.ListTasks(store.WithWorkspace(context.Background(), "demo"))
	if err != nil || len(tasks) != 1 {
		t.Fatalf("tasks=%+v err=%v", tasks, err)
	}
	if enqueued != 1 || tasks[0].NextStage != core.StageTriage || tasks[0].Source != "monitor:direct_push" ||
		tasks[0].SetupName != frozen.Name || tasks[0].SetupContract.Name != frozen.Name ||
		!tasks[0].SpecApproval || !tasks[0].MergeApproval {
		t.Fatalf("enqueued=%d task=%+v", enqueued, tasks[0])
	}
	events, err := st.ListEvents(store.WithWorkspace(context.Background(), "demo"), tasks[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]bool{}
	for _, event := range events {
		kinds[event.Kind] = true
	}
	for _, kind := range []string{"repository_hints.loaded", "monitor.observed", "monitor.classified", "monitor.drift_detected", "monitor.observation_deduplicated"} {
		if !kinds[kind] {
			t.Fatalf("missing audit event %s in %+v", kind, events)
		}
	}
	statusRequest := httptest.NewRequest(http.MethodGet, "/v1/monitor", nil)
	statusResponse := httptest.NewRecorder()
	handler.ServeHTTP(statusResponse, statusRequest)
	if statusResponse.Code != http.StatusOK {
		t.Fatalf("status=%d %s", statusResponse.Code, statusResponse.Body.String())
	}
	var status monitor.Status
	if err = json.Unmarshal(statusResponse.Body.Bytes(), &status); err != nil || status.DriftCount != 1 {
		t.Fatalf("status=%+v err=%v", status, err)
	}
}
