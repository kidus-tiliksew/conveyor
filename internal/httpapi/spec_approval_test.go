package httpapi

import (
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
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
)

func TestSpecApprovalBypassMergeGateInterventions(t *testing.T) {
	for _, action := range []core.InterventionAction{core.InterventionApprove, core.InterventionRedirect} {
		t.Run(string(action), func(t *testing.T) {
			ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
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
			server.Handler().ServeHTTP(response, authenticatedSpecApprovalRequest(
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

func authenticatedSpecApprovalRequest(method, path, body string) *http.Request {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer token")
	request.Header.Set("Content-Type", "application/json")
	return request
}

// TestTriageRetryRouteOpensFreshBounceWindow proves the Retry at a triage job
// failure, a redirect posted to POST /v1/tasks/{id}/review, returns the task
// to triage and records a human intervention, so the invalid outputs before
// it no longer count toward the bounce limit (component-triage invariant 26;
// req-operator-next-action AC-2.2).
func TestTriageRetryRouteOpensFreshBounceWindow(t *testing.T) {
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	st := store.NewMemory()
	task := core.Task{ID: "triage-retry", Workspace: "demo", Repo: "conveyor", PolicyVersion: 1, State: core.TaskRunning, NextStage: core.StageTriage, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: task.ID + "-triage-2", TaskID: task.ID, Stage: core.StageTriage, State: core.JobDone}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	for _, event := range []core.Event{
		{TaskID: task.ID, JobID: job.ID, Kind: "triage.output_invalid", Payload: core.JSONPayload(map[string]string{"error": "triage summary is required"})},
		{TaskID: task.ID, JobID: job.ID, Kind: "triage.output_invalid", Payload: core.JSONPayload(map[string]string{"error": "triage summary is required"})},
		{TaskID: task.ID, JobID: job.ID, Kind: "pipeline.bounce_limit", Payload: core.JSONPayload(map[string]any{"source": "triage.output_invalid", "window": 2, "max_bounces": 2})},
		{TaskID: task.ID, JobID: job.ID, Kind: "job.failed", Payload: core.JSONPayload(map[string]any{"error": "Triage output stayed invalid after 2 attempts, so triage has no outcome. Last validation error: triage summary is required", "source": "triage.output_invalid"})},
	} {
		if err := st.AppendEvent(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := taskops.New(st).Perform(ctx, task.ID, taskops.Command{Kind: core.TaskJobFail, RecoveryStage: core.StageTriage, ProjectStages: true}); err != nil {
		t.Fatal(err)
	}
	if count, err := st.CountEventsSinceHumanIntervention(ctx, task.ID, "triage.output_invalid"); err != nil || count != 2 {
		t.Fatalf("window before retry=%d err=%v", count, err)
	}
	server := NewServer(st)
	server.Workspace, server.BearerToken = "demo", "token"
	server.OnIntervention = dispatch.New(st, &config.Config{Workspace: "demo"}, nil).HandleIntervention
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, authenticatedSpecApprovalRequest(http.MethodPost, "/v1/tasks/"+task.ID+"/review",
		`{"action":"redirect","reason_code":"changes-requested","comment":"Retry triage from its recorded recovery stage after the failed attempt."}`))
	if response.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	current, err := st.GetTask(ctx, task.ID)
	if err != nil || current.State != core.TaskQueued || current.NextStage != core.StageTriage {
		t.Fatalf("after retry task=%+v err=%v", current, err)
	}
	if count, err := st.CountEventsSinceHumanIntervention(ctx, task.ID, "triage.output_invalid"); err != nil || count != 0 {
		t.Fatalf("window after retry=%d err=%v, want a fresh window", count, err)
	}
}

// TestApproveAtStageSourcedBounceLimit proves the review route refuses an
// approve at a gate no review reached with the predicate the next action uses
// to withhold it. A review_bounce_limit gate that planning reached with
// invalid output, and a job_failure gate, answer 409 with the
// approve_without_review code whether or not an earlier review round recorded
// a head. The task stays at its gate with no approved head, no intervention,
// and no event written, and the next action still withholds Approve and
// offers redirect and reject. A review-sourced limit, recorded without a
// source or from review's invalid output, keeps today's approve
// (component-next-action invariants 7, 18; component-task-lifecycle invariant
// 54; req-operator-next-action AC-1.4).
func TestApproveAtStageSourcedBounceLimit(t *testing.T) {
	for _, tc := range []struct {
		name     string
		gate     core.TaskCommand
		recovery core.Stage
		events   []core.Event
		head     string
		refused  bool
	}{
		{name: "planning limit without a reviewed head", gate: core.TaskStageBounceLimit, recovery: core.StageSpec, events: planningLimitEvents(), refused: true},
		{name: "planning limit with a head from an earlier review", gate: core.TaskStageBounceLimit, recovery: core.StageSpec, events: planningLimitEvents(), head: "earlier-reviewed-head", refused: true},
		{name: "job failure with a head from an earlier review", gate: core.TaskJobFail, recovery: core.StageImplement, events: []core.Event{{Kind: "job.failed", Payload: core.JSONPayload(map[string]any{"error": "implementation failed"})}}, head: "earlier-reviewed-head", refused: true},
		{name: "triage job failure", gate: core.TaskJobFail, recovery: core.StageTriage, events: []core.Event{{Kind: "job.failed", Payload: core.JSONPayload(map[string]any{"error": "invalid", "source": "triage.output_invalid"})}}, refused: true},
		{name: "review round limit without a source", gate: core.TaskStageBounceLimit, recovery: core.StageImplement, events: []core.Event{{Kind: "pipeline.bounce_limit", Payload: core.JSONPayload(map[string]any{"count": 3, "window": 3, "max_bounces": 3})}}, head: "reviewed-head"},
		{name: "review invalid output limit", gate: core.TaskStageBounceLimit, recovery: core.StageReview, events: []core.Event{{Kind: "pipeline.bounce_limit", Payload: core.JSONPayload(map[string]any{"source": core.ReviewOutputInvalidSource, "window": 3, "max_bounces": 3})}}, head: "reviewed-head"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
			st := store.NewMemory()
			task := core.Task{ID: "gate-approve", Workspace: "demo", Repo: "conveyor", PolicyVersion: 1, State: core.TaskRunning, NextStage: tc.recovery, ReviewedHeadSHA: tc.head, CreatedAt: time.Now()}
			if err := st.CreateTask(ctx, task); err != nil {
				t.Fatal(err)
			}
			job := core.Job{ID: task.ID + "-1", TaskID: task.ID, Stage: tc.recovery, State: core.JobDone}
			if err := st.CreateJob(ctx, job); err != nil {
				t.Fatal(err)
			}
			for _, event := range tc.events {
				event.TaskID, event.JobID = task.ID, job.ID
				if err := st.AppendEvent(ctx, event); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := taskops.New(st).Perform(ctx, task.ID, taskops.Command{Kind: tc.gate, RecoveryStage: tc.recovery, ProjectStages: true}); err != nil {
				t.Fatal(err)
			}
			action, err := store.ReadNextAction(ctx, st, task.ID, store.NextActionOptions{})
			if err != nil {
				t.Fatal(err)
			}
			offered := len(action.Choices) > 0 && action.Choices[0].ID == "approve"
			if offered == tc.refused {
				t.Fatalf("next action choices=%+v refusals=%+v, want approve offered=%t", action.Choices, action.Details.Refusals, !tc.refused)
			}
			eventsBefore, _ := st.ListEvents(ctx, task.ID)
			server := NewServer(st)
			server.Workspace, server.BearerToken = "demo", "token"
			server.OnIntervention = dispatch.New(st, &config.Config{Workspace: "demo"}, nil).HandleIntervention
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, authenticatedSpecApprovalRequest(http.MethodPost, "/v1/tasks/"+task.ID+"/review", `{"action":"approve","reason_code":"approved"}`))
			current, err := st.GetTask(ctx, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !tc.refused {
				if response.Code != http.StatusAccepted || current.State != core.TaskApproved || current.ApprovedHeadSHA != tc.head {
					t.Fatalf("status=%d body=%s task=%+v, want today's approve", response.Code, response.Body.String(), current)
				}
				return
			}
			var body map[string]string
			if response.Code != http.StatusConflict || json.Unmarshal(response.Body.Bytes(), &body) != nil ||
				body["error"] != core.ApproveWithoutReviewCode || body["message"] != core.ApproveWithoutReviewRefusal {
				t.Fatalf("status=%d body=%s, want 409 %s", response.Code, response.Body.String(), core.ApproveWithoutReviewCode)
			}
			if strings.Contains(body["message"], task.ID) || (tc.head != "" && strings.Contains(body["message"], tc.head)) || strings.Contains(body["message"], core.ApproveWithoutReviewCode) {
				t.Fatalf("refusal %q interpolates an identifier", body["message"])
			}
			if current.State != core.TaskAwaiting || current.ApprovedHeadSHA != "" || current.GateKind == "" {
				t.Fatalf("after a refused approve task=%+v, want it at its gate", current)
			}
			if interventions, err := st.ListInterventions(ctx, task.ID); err != nil || len(interventions) != 0 {
				t.Fatalf("interventions=%+v err=%v, want none recorded", interventions, err)
			}
			if eventsAfter, _ := st.ListEvents(ctx, task.ID); len(eventsAfter) != len(eventsBefore) {
				t.Fatalf("events grew from %d to %d on a refused approve", len(eventsBefore), len(eventsAfter))
			}
			again, err := store.ReadNextAction(ctx, st, task.ID, store.NextActionOptions{})
			if err != nil || len(again.Choices) != 2 || again.Choices[0].ID != "redirect" || again.Choices[1].ID != "reject" {
				t.Fatalf("next action after the refusal=%+v err=%v, want redirect and reject still offered", again.Choices, err)
			}
		})
	}
}

func planningLimitEvents() []core.Event {
	return []core.Event{
		{Kind: "spec.output_invalid", Payload: core.JSONPayload(map[string]string{"error": "plan markdown is required"})},
		{Kind: "pipeline.bounce_limit", Payload: core.JSONPayload(map[string]any{"source": "spec.output_invalid", "window": 1, "max_bounces": 1})},
	}
}
