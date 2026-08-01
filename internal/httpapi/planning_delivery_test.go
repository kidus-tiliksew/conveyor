package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/dispatch"
	"github.com/kidus-tiliksew/conveyor/internal/inprocess"
	"github.com/kidus-tiliksew/conveyor/internal/pipeline"
	"github.com/kidus-tiliksew/conveyor/internal/planning"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

type planningDeliveryAgent struct {
	outputs []string
}

func (a *planningDeliveryAgent) Run(context.Context, string, inprocess.Input) (inprocess.Result, error) {
	if len(a.outputs) == 0 {
		return inprocess.Result{}, fmt.Errorf("planning delivery script exhausted")
	}
	output := a.outputs[0]
	a.outputs = a.outputs[1:]
	return inprocess.Result{Output: output, Model: "planner"}, nil
}

func TestInProductRequirementBlueprintDeliveryPath(t *testing.T) {
	setup := config.ExecutionSetup{Name: "default"}
	cfg := &config.Config{
		Workspace: "demo",
		Repos:     []config.Repo{{Name: "conveyor", Base: "main"}},
		Execution: config.ExecutionPolicy{SpecApproval: true, MergeApproval: true},
		Setups:    []config.ExecutionSetup{setup}, DefaultSetup: setup.Name,
	}
	st := store.NewMemoryWithConfig(cfg)
	dispatcher := dispatch.New(st, cfg, nil)
	dispatcher.DisableMemoryQueueForTest()
	agent := &planningDeliveryAgent{outputs: []string{
		planningFinalizationDecision(t, "finalize_requirement", map[string]any{
			"title": "Planning delivery",
			"prose": "Operators plan delivery inside Conveyor from durable intent.",
			"statements": []map[string]string{{
				"id": "REQ-1", "statement": "A confirmed requirement can produce an approved materialized blueprint.",
			}},
		}),
		planningFinalizationDecision(t, "finalize_blueprint", map[string]any{
			"title":    "Deliver planning flow",
			"repo":     "conveyor",
			"markdown": "## Intent\n\nDeliver confirmed planning intent through the ordinary spec gate.\n\n## Non-goals\n\nNo alternate implementation pipeline.",
			"acceptance": []map[string]string{{
				"id": "AC-1", "criterion": "The approved blueprint materializes its ordered children.", "verify": "test",
			}},
			"decomposition": []map[string]any{
				{"id": "SUB-1", "repo": "conveyor", "summary": "Persist the contract", "depends_on": []string{}},
				{"id": "SUB-2", "repo": "conveyor", "summary": "Run the planning loop", "depends_on": []string{"SUB-1"}},
				{"id": "SUB-3", "repo": "conveyor", "summary": "Present the delivery", "depends_on": []string{"SUB-2"}},
			},
		}),
	}}
	planningService := &planning.Service{
		Store: st, Agent: agent, Model: "planner",
		FinalizeBlueprint: dispatcher.CreatePlanningBlueprint,
	}
	server := NewServer(st)
	server.Workspace, server.BearerToken = "demo", "token"
	server.Planning = planningService
	server.OnIntervention = dispatcher.HandleIntervention
	handler := server.Handler()
	ctx := store.WithWorkspace(t.Context(), "demo")

	requirementSession := createPlanningDeliverySession(t, handler, "Capture intent", "")
	streamPlanningDeliveryMessage(t, handler, requirementSession.ID, "Create the durable requirement.")
	requirementSession, err := st.GetPlanningSession(ctx, requirementSession.ID)
	if err != nil || requirementSession.Status != core.PlanningSessionFinalized ||
		requirementSession.ProducedRequirementID == "" ||
		requirementSession.TranscriptArtifactID == "" {
		t.Fatalf("requirement session=%+v err=%v", requirementSession, err)
	}
	confirm := authenticatedPlanningRequest(
		http.MethodPost,
		"/v1/requirements/"+requirementSession.ProducedRequirementID+"/versions/1/confirm",
		"",
	)
	confirmResponse := httptest.NewRecorder()
	handler.ServeHTTP(confirmResponse, confirm)
	if confirmResponse.Code != http.StatusOK {
		t.Fatalf("confirm status=%d body=%s", confirmResponse.Code, confirmResponse.Body.String())
	}

	blueprintSession := createPlanningDeliverySession(
		t, handler, "Plan delivery", requirementSession.ProducedRequirementID,
	)
	streamPlanningDeliveryMessage(t, handler, blueprintSession.ID, "Finalize the delivery blueprint.")
	blueprintSession, err = st.GetPlanningSession(ctx, blueprintSession.ID)
	if err != nil || blueprintSession.Status != core.PlanningSessionFinalized ||
		blueprintSession.ProducedTaskID == "" || blueprintSession.TranscriptArtifactID == "" {
		t.Fatalf("blueprint session=%+v err=%v", blueprintSession, err)
	}
	blueprintEvents, err := st.ListEvents(ctx, blueprintSession.ProducedTaskID)
	if err != nil {
		t.Fatal(err)
	}
	var requirementSuggested bool
	for _, event := range blueprintEvents {
		if event.Kind != "task.requirement_suggested" {
			continue
		}
		var payload map[string]string
		if json.Unmarshal(event.Payload, &payload) == nil &&
			payload["requirement_id"] == requirementSession.ProducedRequirementID {
			requirementSuggested = true
		}
	}
	if !requirementSuggested {
		t.Fatalf("blueprint events=%+v, want requirement-link proposal", blueprintEvents)
	}

	approve := authenticatedPlanningRequest(
		http.MethodPost,
		"/v1/tasks/"+blueprintSession.ProducedTaskID+"/review",
		`{"action":"approve","reason_code":"approved"}`,
	)
	approveResponse := httptest.NewRecorder()
	handler.ServeHTTP(approveResponse, approve)
	if approveResponse.Code != http.StatusAccepted {
		t.Fatalf("approve status=%d body=%s", approveResponse.Code, approveResponse.Body.String())
	}

	parent, err := st.GetTask(ctx, blueprintSession.ProducedTaskID)
	if err != nil {
		t.Fatal(err)
	}
	spec, exists, err := st.GetLatestSpecVersion(ctx, parent.ID)
	if err != nil || !exists || !spec.Approved || parent.State != core.TaskQueued ||
		parent.NextStage != core.StageImplement || len(parent.Children) != 3 {
		t.Fatalf("parent=%+v spec=%+v exists=%t err=%v", parent, spec, exists, err)
	}
	children := map[string]core.Task{}
	for _, relation := range parent.Children {
		child, getErr := st.GetTask(ctx, relation.ID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		children[child.OriginSubID] = child
		if child.ParentTaskID != parent.ID || child.OriginSpecVersion != spec.Version ||
			child.State != core.TaskQueued || child.NextStage != core.StageImplement ||
			child.FeatureID != "" {
			t.Fatalf("materialized child=%+v", child)
		}
	}
	for subID, wantBlocker := range map[string]string{
		"SUB-1": "",
		"SUB-2": children["SUB-1"].ID,
		"SUB-3": children["SUB-2"].ID,
	} {
		blockers, listErr := st.ListBlockingTaskIDs(ctx, children[subID].ID)
		if listErr != nil {
			t.Fatal(listErr)
		}
		if wantBlocker == "" && len(blockers) != 0 {
			t.Fatalf("%s blockers=%v, want none", subID, blockers)
		}
		if wantBlocker != "" && (len(blockers) != 1 || blockers[0] != wantBlocker) {
			t.Fatalf("%s blockers=%v, want %s", subID, blockers, wantBlocker)
		}
	}

	advanceChildToMergeGate := func(child core.Task) core.Task {
		t.Helper()
		if _, transitionErr := taskops.New(st).Perform(ctx, child.ID, taskops.Command{Kind: core.TaskDispatchStart}); transitionErr != nil {
			t.Fatal(transitionErr)
		}
		if _, transitionErr := taskops.New(st).Perform(ctx, child.ID, taskops.Command{
			Kind: core.TaskStageAdvance, NextStage: core.StageReview, ProjectStages: true,
		}); transitionErr != nil {
			t.Fatal(transitionErr)
		}
		started, transitionErr := taskops.New(st).Perform(ctx, child.ID, taskops.Command{Kind: core.TaskDispatchStart})
		if transitionErr != nil {
			t.Fatal(transitionErr)
		}
		if err = st.AppendEvent(ctx, core.Event{TaskID: child.ID, Kind: "pull_request.opened", Payload: core.JSONPayload(map[string]string{"head_sha": child.ID + "-head"})}); err != nil {
			t.Fatal(err)
		}
		reviewJob := core.Job{ID: child.ID + "-review", TaskID: child.ID, Stage: core.StageReview, State: core.JobDone, ModelTier: "reviewer", StartedAt: time.Now()}
		if err = st.CreateJob(ctx, reviewJob); err != nil {
			t.Fatal(err)
		}
		if err = dispatcher.ApplyExternalReview(ctx, started.Task, reviewJob, pipeline.Review{
			Verdict: "approve", ReasonCode: "approved", Summary: "child review passed",
		}, reviewJob.ID, "review-session", "reviewer"); err != nil {
			t.Fatal(err)
		}
		current, getErr := st.GetTask(ctx, child.ID)
		if getErr != nil || current.State != core.TaskAwaiting || current.RecoveryStage != core.StageImplement {
			t.Fatalf("child at merge gate=%+v err=%v", current, getErr)
		}
		return current
	}

	approvedChild := advanceChildToMergeGate(children["SUB-1"])
	approveChild := authenticatedPlanningRequest(
		http.MethodPost, "/v1/tasks/"+approvedChild.ID+"/review",
		`{"action":"approve","reason_code":"approved"}`,
	)
	approveChildResponse := httptest.NewRecorder()
	handler.ServeHTTP(approveChildResponse, approveChild)
	if approveChildResponse.Code != http.StatusAccepted {
		t.Fatalf("child approve status=%d body=%s", approveChildResponse.Code, approveChildResponse.Body.String())
	}
	approvedChild, err = st.GetTask(ctx, approvedChild.ID)
	if err != nil || approvedChild.State != core.TaskApproved || approvedChild.ApprovedHeadSHA == "" {
		t.Fatalf("approved child=%+v err=%v", approvedChild, err)
	}
	if count, countErr := st.CountEvents(ctx, approvedChild.ID, "intervention.approve"); countErr != nil || count != 1 {
		t.Fatalf("child approve events=%d err=%v", count, countErr)
	}

	redirectedChild := advanceChildToMergeGate(children["SUB-2"])
	redirectChild := authenticatedPlanningRequest(
		http.MethodPost, "/v1/tasks/"+redirectedChild.ID+"/review",
		`{"action":"redirect","reason_code":"changes_requested"}`,
	)
	redirectChildResponse := httptest.NewRecorder()
	handler.ServeHTTP(redirectChildResponse, redirectChild)
	if redirectChildResponse.Code != http.StatusAccepted {
		t.Fatalf("child redirect status=%d body=%s", redirectChildResponse.Code, redirectChildResponse.Body.String())
	}
	redirectedChild, err = st.GetTask(ctx, redirectedChild.ID)
	if err != nil || redirectedChild.State != core.TaskQueued || redirectedChild.NextStage != core.StageImplement {
		t.Fatalf("redirected child=%+v err=%v", redirectedChild, err)
	}
	if count, countErr := st.CountEvents(ctx, redirectedChild.ID, "intervention.redirect"); countErr != nil || count != 1 {
		t.Fatalf("child redirect events=%d err=%v", count, countErr)
	}

	requirementsResponse := httptest.NewRecorder()
	handler.ServeHTTP(requirementsResponse, httptest.NewRequest(http.MethodGet, "/v1/requirements", nil))
	if requirementsResponse.Code != http.StatusOK {
		t.Fatalf("requirements status=%d body=%s", requirementsResponse.Code, requirementsResponse.Body.String())
	}
	var views []requirementView
	if err = json.Unmarshal(requirementsResponse.Body.Bytes(), &views); err != nil ||
		len(views) != 1 || views[0].CurrentVersion == nil ||
		!views[0].CurrentVersion.Confirmed ||
		len(views[0].ServingBlueprints) != 1 ||
		views[0].ServingBlueprints[0].Task.ID != parent.ID {
		t.Fatalf("requirement views=%+v err=%v", views, err)
	}
}

func TestSpecApprovalBypassMergeGateInterventions(t *testing.T) {
	for _, action := range []core.InterventionAction{core.InterventionApprove, core.InterventionRedirect} {
		t.Run(string(action), func(t *testing.T) {
			ctx := store.WithWorkspace(t.Context(), "demo")
			st := store.NewMemory()
			task := core.Task{
				ID: "spec-bypass-" + string(action), Workspace: "demo", Repo: "conveyor",
				PolicyVersion: 1, SpecApproval: false, MergeApproval: true,
				State: core.TaskRunning, NextStage: core.StageReview, ReviewedHeadSHA: "reviewed-head", CreatedAt: time.Now(),
			}
			if err := st.CreateTask(ctx, task); err != nil {
				t.Fatal(err)
			}
			job := core.Job{ID: task.ID + "-review", TaskID: task.ID, Stage: core.StageReview, State: core.JobDone}
			if err := st.CreateJob(ctx, job); err != nil {
				t.Fatal(err)
			}
			gate, err := taskops.New(st).Perform(ctx, task.ID, taskops.Command{
				Kind: core.TaskGateMerge, RecoveryStage: core.StageImplement, ProjectStages: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			task = gate.Task
			cfg := &config.Config{Workspace: "demo"}
			dispatcher := dispatch.New(st, cfg, nil)
			server := NewServer(st)
			server.Workspace, server.BearerToken = "demo", "token"
			server.OnIntervention = dispatcher.HandleIntervention
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, authenticatedPlanningRequest(
				http.MethodPost, "/v1/tasks/"+task.ID+"/review",
				fmt.Sprintf(`{"action":%q,"reason_code":"operator-action"}`, action),
			))
			if response.Code != http.StatusAccepted {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			current, err := st.GetTask(ctx, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			if action == core.InterventionApprove {
				if current.State != core.TaskApproved || current.ApprovedHeadSHA != "reviewed-head" {
					t.Fatalf("approved task=%+v", current)
				}
			} else if current.State != core.TaskQueued || current.NextStage != core.StageImplement {
				t.Fatalf("redirected task=%+v", current)
			}
			if count, countErr := st.CountEvents(ctx, task.ID, "intervention."+string(action)); countErr != nil || count != 1 {
				t.Fatalf("intervention events=%d err=%v", count, countErr)
			}
		})
	}
}

func planningFinalizationDecision(t *testing.T, name string, arguments any) string {
	t.Helper()
	argumentsJSON, err := json.Marshal(arguments)
	if err != nil {
		t.Fatal(err)
	}
	output, err := json.Marshal(map[string]any{
		"response_text": "",
		"tool_calls": []map[string]string{{
			"id": "call-" + name, "name": name, "arguments_json": string(argumentsJSON),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(output)
}

func createPlanningDeliverySession(
	t *testing.T,
	handler http.Handler,
	title string,
	requirementID string,
) core.PlanningSession {
	t.Helper()
	body, err := json.Marshal(map[string]string{
		"title": title, "requirement_context_id": requirementID,
	})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, authenticatedPlanningRequest(
		http.MethodPost, "/v1/planning-sessions", string(body),
	))
	if response.Code != http.StatusCreated {
		t.Fatalf("create planning session status=%d body=%s", response.Code, response.Body.String())
	}
	var session core.PlanningSession
	if err = json.Unmarshal(response.Body.Bytes(), &session); err != nil {
		t.Fatal(err)
	}
	return session
}

func streamPlanningDeliveryMessage(t *testing.T, handler http.Handler, sessionID, content string) {
	t.Helper()
	body, err := json.Marshal(map[string]string{"content": content})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, authenticatedPlanningRequest(
		http.MethodPost, "/v1/planning-sessions/"+sessionID+"/messages", string(body),
	))
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), `"type":"error"`) {
		t.Fatalf("planning stream status=%d body=%s", response.Code, response.Body.String())
	}
}

func authenticatedPlanningRequest(method, path, body string) *http.Request {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer token")
	request.Header.Set("Content-Type", "application/json")
	return request
}
