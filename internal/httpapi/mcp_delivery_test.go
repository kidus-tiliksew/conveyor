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
	"github.com/kidus-tiliksew/conveyor/internal/dispatch"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	githubtrigger "github.com/kidus-tiliksew/conveyor/internal/trigger/github"
	"github.com/kidus-tiliksew/conveyor/internal/workorder"
)

// TestSubmissionChannelsValidateHeadAndClaimBeforeSideEffects drives the REST
// delivery route and the MCP submit_for_review tool through one admission:
// each refusal writes nothing, and the accepted head replays without a second
// write (component-mcp-protocol, Claim-bound delivery endpoints).
func TestSubmissionChannelsValidateHeadAndClaimBeforeSideEffects(t *testing.T) {
	for _, channel := range []string{"rest", "mcp"} {
		for _, mode := range []string{"missing-sha", "missing-pr", "head-mismatch", "base-mismatch", "foreign-session", "matching"} {
			t.Run(channel+"/"+mode, func(t *testing.T) {
				ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
				st := store.NewMemory()
				task := core.Task{ID: "delivery", Workspace: "demo", Repo: "app", Title: "Deliver", Branch: "conveyor/task-delivery", BaseBranch: "main", State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: time.Now()}
				if err := st.CreateTask(ctx, task); err != nil {
					t.Fatal(err)
				}
				job := core.Job{ID: "delivery-implement", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
				if err := st.CreateJob(ctx, job); err != nil {
					t.Fatal(err)
				}
				if err := storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement}); err != nil {
					t.Fatal(err)
				}
				cfg := &config.Config{Workspace: "demo", Repos: []config.Repo{{Name: "app", URL: "https://github.com/acme/app.git", GitHub: "acme/app", Base: "main"}}, Routing: config.Routing{Stages: map[string]config.StageRoute{"review": {Execution: config.ExecutionMCP}}}}
				dispatcher := dispatch.New(st, cfg, nil)
				dispatcher.DisableMemoryQueueForTest()
				reads, writes := 0, 0
				orders := &workorder.Service{Store: st, Dispatcher: dispatcher, ConfigProvider: func(context.Context) (*config.Config, error) { return cfg, nil },
					DiscoverDocumentationPolicy: func(_ context.Context, _, revision string) githubtrigger.DocumentationPolicyDiscovery {
						return githubtrigger.DocumentationPolicyDiscovery{Revision: revision, State: "absent"}
					},
					SubmissionPR: func(context.Context, string, string) (githubtrigger.SubmissionPullRequest, error) {
						pr := githubtrigger.SubmissionPullRequest{Number: 7, URL: "https://github.com/acme/app/pull/7"}
						pr.Head.SHA, pr.Head.Ref = "named-head", task.Branch
						pr.Base.SHA, pr.Base.Ref = "base-sha", "main"
						switch mode {
						case "missing-pr":
							pr.Number = 0
						case "head-mismatch":
							pr.Head.SHA = "observed-head"
						case "base-mismatch":
							pr.Base.Ref = "wrong-base"
						}
						return pr, nil
					},
					SubmissionChangedPaths: func(_ context.Context, _ *config.Config, comparison core.Task) ([]string, error) {
						reads++
						if comparison.ReviewedHeadSHA != "named-head" || comparison.BaseBranch != "base-sha" {
							t.Errorf("comparison not SHA-bound: %+v", comparison)
						}
						return []string{"internal/change.go"}, nil
					},
					ReviewDiffBetween:     func(context.Context, string, string, string) (string, error) { return "diff", nil },
					ReconcileSubmissionPR: func(context.Context, string, githubtrigger.SubmissionPullRequest, string) error { writes++; return nil },
					// A permanent head mismatch exhausts the bounded re-read
					// schedule without wall-clock waits.
					SubmissionPRWait: func(context.Context, time.Duration) error { return nil },
				}
				claim := core.WorkOrderClaim{SessionID: "session", ClientToken: "claim-token", ClaimantID: core.TaskRunClaimantID("owner"), OwnerUserID: "owner", Lease: time.Minute}
				credential := "agent-bearer"
				if _, err := storetest.For(st).ClaimWorkOrder(ctx, job.ID, claim); err != nil {
					t.Fatal(err)
				}
				server := NewServer(st)
				server.Workspace = "demo"
				server.WorkOrders = orders
				server.Credentials = staticCredentialVerifier{"agent-bearer": {ID: "owner-pat", OwnerUserID: "owner", Kind: core.CredentialUser}}
				handler := server.Handler()
				prefix := "/v1/work-orders/"
				templateRequest := httptest.NewRequest(http.MethodGet, prefix+job.ID+"/pull-request-template?workspace_id=demo&session_id=session", nil)
				templateRequest.Header.Set("Authorization", "Bearer "+credential)
				templateResponse := httptest.NewRecorder()
				handler.ServeHTTP(templateResponse, templateRequest)
				if templateResponse.Code != 200 || !strings.Contains(templateResponse.Body.String(), "conveyor:task") {
					t.Fatalf("template status=%d body=%s", templateResponse.Code, templateResponse.Body.String())
				}
				head, session := "named-head", "session"
				if mode == "missing-sha" {
					head = ""
				}
				if mode == "foreign-session" {
					session = "another"
				}
				args := map[string]any{"workspace_id": "demo", "work_order_id": job.ID, "session_id": session, "head_sha": head}
				endpoint := prefix + job.ID + "/submit-for-review"
				var payload any = args
				if channel == "mcp" {
					endpoint = "/mcp"
					payload = map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "submit_for_review", "arguments": args}}
				}
				raw, _ := json.Marshal(payload)
				request := httptest.NewRequest(http.MethodPost, endpoint, bytes.NewReader(raw))
				request.Header.Set("Authorization", "Bearer "+credential)
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, request)
				order, _ := st.GetWorkOrder(ctx, job.ID)
				current, _ := st.GetTask(ctx, task.ID)
				if mode == "matching" {
					if response.Code != 200 || order.State != core.WorkOrderSubmitted || current.NextStage != core.StageReview || reads != 1 || writes != 1 {
						t.Fatalf("status=%d body=%s state=%s reads=%d writes=%d", response.Code, response.Body.String(), order.State, reads, writes)
					}
					events, _ := st.ListEvents(ctx, task.ID)
					found := false
					for _, event := range events {
						if event.Kind == "pull_request.opened" && strings.Contains(string(event.Payload), `"head_sha":"named-head"`) && strings.Contains(string(event.Payload), `"base_sha":"base-sha"`) {
							found = true
						}
					}
					if !found {
						t.Fatal("PR commit pair not recorded")
					}
					// The same session replays its accepted handoff through every
					// channel: the template rereads and the identical head returns
					// the recorded result with no PR, order, or event write.
					eventsBefore := len(events)
					replayTemplate := httptest.NewRecorder()
					handler.ServeHTTP(replayTemplate, templateRequest.Clone(store.WithActor(t.Context(), store.SystemActor())))
					if replayTemplate.Code != 200 {
						t.Fatalf("replay template status=%d body=%s", replayTemplate.Code, replayTemplate.Body.String())
					}
					for _, replay := range []struct {
						head string
						ok   bool
					}{{head: "named-head", ok: true}, {head: "later-head", ok: false}} {
						args["head_sha"] = replay.head
						payload = args
						if channel == "mcp" {
							payload = map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": map[string]any{"name": "submit_for_review", "arguments": args}}
						}
						raw, _ = json.Marshal(payload)
						request = httptest.NewRequest(http.MethodPost, endpoint, bytes.NewReader(raw))
						request.Header.Set("Authorization", "Bearer "+credential)
						response = httptest.NewRecorder()
						handler.ServeHTTP(response, request)
						body := response.Body.String()
						accepted := response.Code == 200 && strings.Contains(body, "replayed") && !strings.Contains(body, "already submitted")
						if accepted != replay.ok || (!replay.ok && !strings.Contains(body, "already submitted")) {
							t.Fatalf("replay head=%s status=%d body=%s", replay.head, response.Code, body)
						}
					}
					events, _ = st.ListEvents(ctx, task.ID)
					if len(events) != eventsBefore || reads != 1 || writes != 1 {
						t.Fatalf("replay wrote state: events %d→%d reads=%d writes=%d", eventsBefore, len(events), reads, writes)
					}
				} else {
					codes := map[string]string{"missing-sha": "head_sha is required", "missing-pr": "pull_request_missing", "head-mismatch": "pull_request_head_mismatch", "base-mismatch": "pull_request_base_mismatch"}
					if code := codes[mode]; code != "" && !strings.Contains(response.Body.String(), code) {
						t.Fatalf("missing %s: %s", code, response.Body.String())
					}
					if order.State != core.WorkOrderClaimed || current.NextStage != core.StageImplement || reads != 0 || writes != 0 {
						t.Fatalf("refusal had side effects: %s %s %d %d", order.State, current.NextStage, reads, writes)
					}
				}
			})
		}
	}
}
