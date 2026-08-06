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
