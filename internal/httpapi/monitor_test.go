package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
	if response := func() *httptest.ResponseRecorder {
		payload, _ := json.Marshal(monitor.Observation{
			Repository: "conveyor", Kind: monitor.DirectPush, OccurrenceID: "commit:missing",
			SourceURL: "https://github.com/acme/conveyor/commit/missing", CommitSHA: "missing",
			RequirementID: "req-does-not-exist",
		})
		request := httptest.NewRequest(http.MethodPost, "/v1/monitor/observations", bytes.NewReader(payload))
		request.Header.Set("Authorization", "Bearer secret")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}(); response.Code != http.StatusUnprocessableEntity || !strings.Contains(response.Body.String(), "req-does-not-exist") {
		t.Fatalf("unknown requirement status=%d body=%s", response.Code, response.Body.String())
	}
	if _, _, err := st.CreateRequirement(store.WithWorkspace(t.Context(), "demo"), core.Requirement{ID: "req-runtime", Title: "Runtime"}, core.RequirementVersion{
		Content:    "Runtime requirement.\n\n```conveyor:requirements\n- id: REQ-1\n  statement: Runtime references are valid.\n```",
		Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Runtime references are valid."}},
		Origin:     core.RequirementOriginChat, OriginSessionID: "monitor-test",
	}); err != nil {
		t.Fatal(err)
	}
	hints, err := monitor.ParseHints([]byte("version: 1\ntriage_areas: [control-plane]\n"), "abc")
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(monitor.Observation{
		Repository: "conveyor", Kind: monitor.DirectPush, OccurrenceID: "commit:abc",
		SourceURL: "https://github.com/acme/conveyor/commit/abc", CommitSHA: "abc",
		RequirementID: "req-runtime", Hints: &hints,
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
	if err = json.Unmarshal(statusResponse.Body.Bytes(), &status); err != nil ||
		status.DriftCount != 1 || len(status.Drift) != 1 ||
		status.Drift[0].RequirementID != "req-runtime" {
		t.Fatalf("status=%+v err=%v", status, err)
	}
}

func TestMonitorPostMergeAttemptsReuseOneTaskAndKeepDistinctObservations(t *testing.T) {
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
			Execution: config.ExecutionPolicy{SpecApproval: true},
		}, nil
	}
	server.GenerateTaskTitle = func(context.Context, core.Task) (string, error) { return "Repair post-merge failure", nil }
	enqueued := 0
	server.OnCreate = func(context.Context, string) { enqueued++ }
	server.Monitor = &monitor.Service{
		Store: st.(monitor.Store), WorkspaceID: "demo", Enabled: true,
		Repositories: map[string]struct{}{"conveyor": {}},
	}
	server.Monitor.Intake = server.CreateMonitorTask
	handler := server.Handler()
	post := func(attempt, check string) monitor.ObservationRecord {
		t.Helper()
		payload, err := json.Marshal(monitor.Observation{
			Repository: "conveyor", Kind: monitor.PostMergeFailure,
			OccurrenceID: "commit:abc:attempt:" + attempt, CommitSHA: "abc",
			SourceURL: "https://github.com/acme/conveyor/check/" + check,
			Context:   map[string]string{"failed_check_runs": "- ci (check run " + check + ")"},
		})
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPost, "/v1/monitor/observations", bytes.NewReader(payload))
		request.Header.Set("Authorization", "Bearer secret")
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusAccepted {
			t.Fatalf("attempt %s status=%d body=%s", attempt, response.Code, response.Body.String())
		}
		var record monitor.ObservationRecord
		if err = json.Unmarshal(response.Body.Bytes(), &record); err != nil {
			t.Fatal(err)
		}
		return record
	}

	first := post("1", "11")
	second := post("2", "22")
	redelivered := post("2", "22")
	if first.TaskID == "" || second.TaskID != first.TaskID || redelivered.TaskID != first.TaskID ||
		second.TaskOutcome != "reused" || redelivered.DeduplicatedCount != 1 {
		t.Fatalf("first=%+v second=%+v redelivered=%+v", first, second, redelivered)
	}
	tasks, err := st.ListTasks(store.WithWorkspace(t.Context(), "demo"))
	if err != nil || len(tasks) != 1 || enqueued != 1 {
		t.Fatalf("tasks=%+v enqueued=%d err=%v", tasks, enqueued, err)
	}
	if !strings.Contains(tasks[0].Body, "check run 11") || strings.Contains(tasks[0].Body, "check run 22") {
		t.Fatalf("first task body was not preserved: %q", tasks[0].Body)
	}
	status, err := server.Monitor.Status(store.WithWorkspace(t.Context(), "demo"))
	if err != nil || len(status.Observations) != 2 {
		t.Fatalf("status=%+v err=%v", status, err)
	}
	for _, observation := range status.Observations {
		if observation.TaskID != first.TaskID {
			t.Fatalf("observation not linked to shared task: %+v", observation)
		}
	}
	events, err := st.ListEvents(store.WithWorkspace(t.Context(), "demo"), first.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	created, reused := 0, 0
	for _, event := range events {
		switch event.Kind {
		case "monitor.task_created":
			created++
		case "monitor.task_reused":
			reused++
		}
	}
	if created != 1 || reused != 1 {
		t.Fatalf("create/reuse audit counts created=%d reused=%d events=%+v", created, reused, events)
	}
}

func TestResolveDriftAtomicallyProposesRequirementAmendment(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	requirement, _, err := st.CreateRequirement(ctx, core.Requirement{ID: "req-runtime", Title: "Runtime contract"}, core.RequirementVersion{
		Content:    "Runtime changes remain aligned.",
		Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Out-of-pipeline changes are reconciled."}},
		Origin:     core.RequirementOriginChat, OriginSessionID: "session-runtime",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ConfirmRequirementVersion(ctx, requirement.ID, 1); err != nil {
		t.Fatal(err)
	}
	task := core.Task{ID: "drift-task", Workspace: "demo", Title: "Reconcile runtime", State: core.TaskQueued}
	if err = st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	drift := monitor.Drift{
		ID: "drift-runtime", WorkspaceID: "demo", Repository: "conveyor", Kind: monitor.DirectPush,
		SourceURL: "https://github.com/acme/conveyor/commit/abc", CommitSHA: "abc",
		RequirementID: requirement.ID, TaskID: task.ID, DetectedAt: time.Now().UTC(),
	}
	if _, _, err = st.(monitor.Store).RecordDrift(ctx, drift); err != nil {
		t.Fatal(err)
	}
	server := NewServer(st)
	server.Workspace, server.BearerToken = "demo", "token"
	server.Monitor = &monitor.Service{Store: st.(monitor.Store), WorkspaceID: "demo", Enabled: true}
	resolve := func(id string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/v1/monitor/drift/"+id+"/resolve", strings.NewReader(`{"outcome":"requirements_amended"}`))
		request.Header.Set("Authorization", "Bearer token")
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		return response
	}
	if response := resolve(drift.ID); response.Code != http.StatusOK {
		t.Fatalf("resolve status=%d body=%s", response.Code, response.Body.String())
	}
	versions, err := st.ListRequirementVersions(ctx, requirement.ID)
	if err != nil || len(versions) != 2 || versions[1].Origin != core.RequirementOriginDriftAmendment ||
		versions[1].OriginDriftID != drift.ID || versions[1].Confirmed || !strings.Contains(versions[1].Content, drift.SourceURL) {
		t.Fatalf("drift versions=%+v err=%v", versions, err)
	}
	if response := resolve(drift.ID); response.Code != http.StatusOK {
		t.Fatalf("repeat status=%d body=%s", response.Code, response.Body.String())
	}
	if versions, err = st.ListRequirementVersions(ctx, requirement.ID); err != nil || len(versions) != 2 {
		t.Fatalf("retry versions=%+v err=%v", versions, err)
	}
	events, err := st.ListEvents(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]int{}
	for _, event := range events {
		kinds[event.Kind]++
	}
	if kinds["monitor.drift_reconciled"] != 1 {
		t.Fatalf("drift events=%+v", events)
	}

	missing := monitor.Drift{ID: "drift-no-requirement", WorkspaceID: "demo", Repository: "conveyor", Kind: monitor.Revert,
		SourceURL: "https://github.com/acme/conveyor/commit/def", TaskID: task.ID, DetectedAt: time.Now().UTC()}
	if _, _, err = st.(monitor.Store).RecordDrift(ctx, missing); err != nil {
		t.Fatal(err)
	}
	if response := resolve(missing.ID); response.Code != http.StatusUnprocessableEntity || !strings.Contains(response.Body.String(), "requirement_id is missing") {
		t.Fatalf("missing requirement status=%d body=%s", response.Code, response.Body.String())
	}
	status, err := st.(monitor.Store).MonitorStatus(ctx, true, time.Now().UTC())
	if err != nil || status.DriftCount != 1 || status.Drift[0].ID != missing.ID {
		t.Fatalf("unresolved missing-reference drift=%+v err=%v", status.Drift, err)
	}
}
