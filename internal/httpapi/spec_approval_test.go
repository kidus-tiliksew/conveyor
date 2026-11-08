package httpapi

import (
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

// TestApproveAtStageSourcedBounceLimit records what Approve does at a
// review_bounce_limit gate that planning reached with invalid output: without
// a recorded reviewed head the route refuses it, and with a head recorded by
// an earlier review round it moves the task to approved although no review
// approved the current state. The offered choices stay unchanged
// (component-next-action invariants 7, 20; req-operator-next-action AC-1.4).
func TestApproveAtStageSourcedBounceLimit(t *testing.T) {
	for _, tc := range []struct {
		name     string
		head     string
		status   int
		approved bool
	}{
		{name: "no reviewed head", status: http.StatusConflict},
		{name: "head from an earlier review", head: "earlier-reviewed-head", status: http.StatusAccepted, approved: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
			st := store.NewMemory()
			task := core.Task{ID: "plan-limit", Workspace: "demo", Repo: "conveyor", PolicyVersion: 1, State: core.TaskRunning, NextStage: core.StageSpec, ReviewedHeadSHA: tc.head, CreatedAt: time.Now()}
			if err := st.CreateTask(ctx, task); err != nil {
				t.Fatal(err)
			}
			job := core.Job{ID: task.ID + "-spec-1", TaskID: task.ID, Stage: core.StageSpec, State: core.JobDone}
			if err := st.CreateJob(ctx, job); err != nil {
				t.Fatal(err)
			}
			for _, event := range []core.Event{
				{TaskID: task.ID, JobID: job.ID, Kind: "spec.output_invalid", Payload: core.JSONPayload(map[string]string{"error": "plan markdown is required"})},
				{TaskID: task.ID, JobID: job.ID, Kind: "pipeline.bounce_limit", Payload: core.JSONPayload(map[string]any{"source": "spec.output_invalid", "window": 1, "max_bounces": 1})},
			} {
				if err := st.AppendEvent(ctx, event); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := taskops.New(st).Perform(ctx, task.ID, taskops.Command{Kind: core.TaskStageBounceLimit, RecoveryStage: core.StageSpec, ProjectStages: true}); err != nil {
				t.Fatal(err)
			}
			action, err := store.ReadNextAction(ctx, st, task.ID, store.NextActionOptions{})
			if err != nil || action.Label != "Settle planning output" || len(action.Choices) != 3 || action.Choices[0].ID != "approve" || len(action.Details.Refusals) != 0 {
				t.Fatalf("next action=%+v err=%v, want the unchanged review choices with approve offered", action, err)
			}
			server := NewServer(st)
			server.Workspace, server.BearerToken = "demo", "token"
			server.OnIntervention = dispatch.New(st, &config.Config{Workspace: "demo"}, nil).HandleIntervention
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, authenticatedSpecApprovalRequest(http.MethodPost, "/v1/tasks/"+task.ID+"/review", `{"action":"approve","reason_code":"approved"}`))
			if response.Code != tc.status {
				t.Fatalf("status=%d body=%s, want %d", response.Code, response.Body.String(), tc.status)
			}
			current, err := st.GetTask(ctx, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			if tc.approved != (current.State == core.TaskApproved) || (!tc.approved && current.State != core.TaskAwaiting) {
				t.Fatalf("after approve task=%+v", current)
			}
			if tc.approved && current.ApprovedHeadSHA != tc.head {
				t.Fatalf("approved head=%q want %q", current.ApprovedHeadSHA, tc.head)
			}
		})
	}
}
