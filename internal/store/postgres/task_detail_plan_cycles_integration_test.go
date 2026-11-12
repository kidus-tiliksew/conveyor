package postgres

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/dispatch"
	"github.com/kidus-tiliksew/conveyor/internal/httpapi"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

// TestBoundedDetailPlanApprovalNamesInterventionIntegration approves a plan
// through the review route, the dispatcher, and the PostgreSQL store. The
// dispatcher records the version approval and the plan-state move before the
// route records the approve intervention, so the intervention event follows
// the step it decides. The bounded detail's plan_approved step still names the
// persisted intervention and its person, and the intervention event keeps the
// record's exact time (component-web-task-surfaces invariants 67 to 69;
// req-260809-a40c75 AC-4.2; DEC-130(1)).
func TestBoundedDetailPlanApprovalNamesInterventionIntegration(t *testing.T) {
	st, err := Open(store.WithActor(t.Context(), store.SystemActor()), integrationDatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	workspace := "plan-cycle-approval-" + core.NewTaskID()
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), workspace)
	cfg := &config.Config{Workspace: workspace, Repos: []config.Repo{{Name: "conveyor", URL: "https://example.test/conveyor", Base: "main"}}}
	if _, err = st.BootstrapWorkspaceConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	task := core.Task{ID: core.NewTaskID(), Workspace: workspace, Title: "Plan approval", Repo: "conveyor", BaseBranch: "main",
		State: core.TaskRunning, NextStage: core.StageSpec, PlanState: core.PlanNeeded, PolicyVersion: 1, CreatedAt: time.Now().UTC()}
	task.Branch = "conveyor/task-" + task.ID
	if err = st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err = st.CreateJob(ctx, core.Job{ID: task.ID + "-spec-1", TaskID: task.ID, Stage: core.StageSpec, State: core.JobDone}); err != nil {
		t.Fatal(err)
	}
	spec, err := st.CreateSpecVersion(ctx, core.SpecVersion{TaskID: task.ID, Content: "## Approach\n\n## Done criteria\n",
		Acceptance: core.JSONPayload([]any{}), Decomposition: core.JSONPayload([]any{})})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = taskops.New(st).Perform(ctx, task.ID, taskops.Command{Kind: core.TaskGateSpec, RecoveryStage: core.StageImplement, ProjectStages: true,
		PlanState: &taskops.PlanStateMove{To: core.PlanAwaitingApproval, Cause: core.PlanCausePlanSubmitted}}); err != nil {
		t.Fatal(err)
	}

	server := httpapi.NewServer(st)
	server.Credentials = nil // the shared-token operator fixture
	server.Workspace, server.BearerToken = workspace, "token"
	dispatcher := dispatch.New(st, cfg, nil)
	dispatcher.DisableMemoryQueueForTest()
	server.OnIntervention = dispatcher.HandleIntervention
	serve := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer token")
		if body != "" {
			request.Header.Set("Content-Type", "application/json")
		}
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		return response
	}
	if response := serve(http.MethodPost, "/v1/tasks/"+task.ID+"/review", `{"action":"approve","reason_code":"approved","comment":"Looks right."}`); response.Code != http.StatusAccepted {
		t.Fatalf("approve: status=%d body=%s", response.Code, response.Body.String())
	}

	interventions, err := st.ListInterventions(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(interventions) != 1 || interventions[0].Action != core.InterventionApprove {
		t.Fatalf("interventions = %+v", interventions)
	}
	approval := interventions[0]
	events, err := st.ListEvents(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var approvedEvent, interventionEvent *core.Event
	for i := range events {
		switch events[i].Kind {
		case "spec.version_approved":
			approvedEvent = &events[i]
		case "intervention.approve":
			interventionEvent = &events[i]
		}
	}
	if approvedEvent == nil || interventionEvent == nil {
		t.Fatalf("approval events missing: %+v", events)
	}
	// The intervention event keeps its record's time, so the fold joins them.
	if !interventionEvent.At.Equal(approval.At) {
		t.Fatalf("intervention event at %s, record at %s", interventionEvent.At, approval.At)
	}
	// The order this regression guards: the version approval precedes the
	// intervention event.
	if !approvedEvent.At.Before(interventionEvent.At) || approvedEvent.ID > interventionEvent.ID {
		t.Fatalf("version approval %d at %s does not precede intervention event %d at %s",
			approvedEvent.ID, approvedEvent.At, interventionEvent.ID, interventionEvent.At)
	}

	response := serve(http.MethodGet, "/v1/tasks/"+task.ID+"/activity?view=bounded", "")
	if response.Code != http.StatusOK {
		t.Fatalf("bounded detail: status=%d body=%s", response.Code, response.Body.String())
	}
	var body struct {
		Summaries struct {
			PlanCycles []struct {
				Outcome string `json:"outcome"`
				Steps   []struct {
					Kind           string `json:"kind"`
					EventID        string `json:"event_id"`
					PlanVersion    *int   `json:"plan_version"`
					UserID         string `json:"user_id"`
					InterventionID *int64 `json:"intervention_id"`
					Text           string `json:"text"`
				} `json:"steps"`
			} `json:"plan_cycles"`
		} `json:"summaries"`
	}
	if err = json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	cycles := body.Summaries.PlanCycles
	if len(cycles) != 1 || cycles[0].Outcome != "approved" {
		t.Fatalf("plan cycles = %+v", cycles)
	}
	approved := 0
	for _, step := range cycles[0].Steps {
		if step.Kind != "plan_approved" {
			continue
		}
		approved++
		wantUser := strings.TrimPrefix(approval.ActorID, "user:")
		if step.InterventionID == nil || *step.InterventionID != approval.ID || wantUser == approval.ActorID || step.UserID != wantUser ||
			step.PlanVersion == nil || *step.PlanVersion != spec.Version || step.Text != "Looks right." {
			t.Fatalf("plan_approved step = %+v, intervention = %+v", step, approval)
		}
	}
	if approved != 1 {
		t.Fatalf("plan_approved steps = %d in %+v", approved, cycles[0].Steps)
	}
}
