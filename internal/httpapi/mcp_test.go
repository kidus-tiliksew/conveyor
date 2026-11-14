package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kidus-tiliksew/conveyor/internal/testimage"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/kidus-tiliksew/conveyor/internal/config"
	"github.com/kidus-tiliksew/conveyor/internal/core"
	"github.com/kidus-tiliksew/conveyor/internal/dispatch"
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
	githubtrigger "github.com/kidus-tiliksew/conveyor/internal/trigger/github"
	"github.com/kidus-tiliksew/conveyor/internal/workorder"
)

func TestMCPToolCallEnvelopeStrictMetadataBoundary(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		wire string
		ok   bool
	}{
		"plain call":         {wire: `{"name":"list_tasks","arguments":{"workspace_id":"demo"}}`, ok: true},
		"standard metadata":  {wire: `{"name":"list_tasks","arguments":{"workspace_id":"demo"},"_meta":{"progressToken":1,"trace":"fixture"}}`, ok: true},
		"unknown field":      {wire: `{"name":"list_tasks","arguments":{},"extra":true}`},
		"duplicate name":     {wire: `{"name":"list_tasks","name":"get_document","arguments":{}}`},
		"duplicate metadata": {wire: `{"name":"list_tasks","arguments":{},"_meta":{"progressToken":1,"progressToken":2}}`},
		"metadata is scalar": {wire: `{"name":"list_tasks","arguments":{},"_meta":1}`},
		"not an object":      {wire: `[]`},
	} {
		t.Run(name, func(t *testing.T) {
			var call mcpToolCallEnvelope
			err := core.DecodeVerificationRequest([]byte(tc.wire), &call)
			if tc.ok && err != nil {
				t.Fatalf("decode: %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("malformed envelope accepted")
			}
			if tc.ok && call.Name != "list_tasks" {
				t.Fatalf("name=%q", call.Name)
			}
			if tc.ok {
				if _, leaked := call.Arguments["_meta"]; leaked {
					t.Fatal("envelope metadata leaked into tool arguments")
				}
			}
		})
	}
}

func TestMCPReadArtifactSupportsManualSessionsAndEnforcesClaimOwnership(t *testing.T) {
	t.Parallel()
	ctx := store.WithWorkspace(store.WithActor(context.Background(), store.SystemActor()), "demo")
	st := store.NewMemory()
	for _, task := range []core.Task{
		{ID: "task-a", Workspace: "demo", State: core.TaskRunning, CreatedAt: time.Now()},
		{ID: "task-b", Workspace: "demo", State: core.TaskRunning, CreatedAt: time.Now()},
	} {
		if err := st.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
	}
	for _, item := range []struct {
		order, task, session, owner string
	}{
		{order: "order-a", task: "task-a", session: "session-a", owner: "owner-a"},
		{order: "order-b", task: "task-b", session: "session-b", owner: "owner-b"},
	} {
		if err := st.CreateJob(ctx, core.Job{ID: item.order, TaskID: item.task, Stage: core.StageImplement, State: core.JobPending}); err != nil {
			t.Fatal(err)
		}
		if err := storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: item.order, TaskID: item.task, JobID: item.order, Stage: core.StageImplement, State: core.WorkOrderQueued}); err != nil {
			t.Fatal(err)
		}
		if _, err := storetest.For(st).ClaimWorkOrder(ctx, item.order, core.WorkOrderClaim{SessionID: item.session, ClientToken: "secret", ClaimantID: core.TaskRunClaimantID(item.owner), Lease: time.Minute}); err != nil {
			t.Fatal(err)
		}
	}
	artifact, err := st.CreateArtifact(ctx, core.Artifact{Name: "design.png", ContentType: "image/png", TaskID: "task-a"}, testimage.PNG("design"))
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(st)
	server.Workspace = "demo"
	server.WorkOrders = &workorder.Service{Store: st}
	args := map[string]any{"workspace_id": "demo", "work_order_id": "order-a", "session_id": "session-a", "artifact_id": artifact.ID}
	request := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	request = request.WithContext(withCredentialActor(request.Context(), core.AuthenticatedCredential{ID: "manual-pat", OwnerUserID: "owner-a", Kind: core.CredentialUser}))
	if _, err = server.callMCPTool(request, "read_artifact", args); err != nil {
		t.Fatalf("manual read: %v", err)
	}
	audit, err := st.CreateArtifact(ctx, core.Artifact{Name: "planning-audit.json", ContentType: "application/json", Role: core.ArtifactRoleGeneratedAudit, TaskID: "task-a"}, []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	auditArgs := maps.Clone(args)
	auditArgs["artifact_id"] = audit.ID
	if _, err = server.callMCPTool(request, "read_artifact", auditArgs); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("own-task generated audit read error=%v", err)
	}
	otherOwnerRequest := request.WithContext(withCredentialActor(request.Context(), core.AuthenticatedCredential{ID: "other-pat", OwnerUserID: "owner-b", Kind: core.CredentialUser}))
	if _, err = server.callMCPTool(otherOwnerRequest, "read_artifact", args); !errors.Is(err, store.ErrWorkOrderClaimUnauthorized) {
		t.Fatalf("other owner read error=%v", err)
	}
	wrongWorkspace := maps.Clone(args)
	wrongWorkspace["workspace_id"] = "other"
	if _, err = server.callMCPTool(request, "read_artifact", wrongWorkspace); err == nil || !strings.Contains(err.Error(), "workspace_not_found") {
		t.Fatalf("wrong workspace read error=%v", err)
	}
}

func TestMCPRenewReportsSameSessionCheckpointRelease(t *testing.T) {
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	st := store.NewMemory()
	task := core.Task{ID: "mcp-checkpoint", Workspace: "demo", State: core.TaskRunning, CreatedAt: time.Now()}
	job := core.Job{ID: "mcp-checkpoint-implement-1", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued}); err != nil {
		t.Fatal(err)
	}
	claimant := core.TaskRunClaimantID("usr-runner")
	if _, err := storetest.For(st).ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{SessionID: "run-session", ClientToken: "secret", ClaimantID: claimant, Lease: time.Minute}); err != nil {
		t.Fatal(err)
	}
	if _, err := storetest.For(st).ReleaseWorkerClaim(ctx, job.ID, "", core.WorkOrderRelease{SessionID: "run-session", Reason: core.WorkOrderReleaseReasonOperatorCheckpointReached, Cause: core.WorkOrderReleaseCauseOperatorAction, Outcome: core.WorkOrderOutcomeReleased}); err != nil {
		t.Fatal(err)
	}
	server := NewServer(st)
	server.Workspace = "demo"
	server.WorkOrders = &workorder.Service{Store: st}
	request := httptest.NewRequest(http.MethodPost, "/mcp", nil).WithContext(withCredentialActor(ctx, core.AuthenticatedCredential{ID: "user-token", OwnerUserID: "usr-runner", Kind: core.CredentialUser, Scope: core.CredentialScopeUser}))
	args := map[string]any{"workspace_id": "demo", "work_order_id": job.ID, "session_id": "run-session"}
	if _, err := server.callMCPTool(request, "renew_work_order", args); !errors.Is(err, store.ErrWorkOrderReleasedAtCheckpoint) {
		t.Fatalf("renew error=%v", err)
	}
	args["session_id"] = "other-session"
	if _, err := server.callMCPTool(request, "renew_work_order", args); !errors.Is(err, store.ErrWorkOrderClaimLost) {
		t.Fatalf("wrong-session renew error=%v", err)
	}
}

// A proposal made at an approved authority-conflict checkpoint returns
// guidance that directs the checkpoint release; without the flag, or with it
// false, the fire-and-forget guidance is unchanged; a non-boolean flag is
// refused before anything is written (component-mcp-protocol, Governance
// proposal tools; req-260820-6a468a AC-1.3; req-260820-394cac REQ-3).
func TestMCPClaimDefaultsToFiveMinuteLease(t *testing.T) {
	t.Parallel()
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	st := store.NewMemory()
	task := core.Task{ID: "mcp-default-lease", Workspace: "demo", State: core.TaskRunning, CreatedAt: time.Now()}
	job := core.Job{ID: task.ID + "-implement-1", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement}); err != nil {
		t.Fatal(err)
	}
	provider := func(context.Context) (*config.Config, error) {
		return &config.Config{Workspace: "demo", Routing: config.Routing{Stages: map[string]config.StageRoute{
			"implement": {Timeout: time.Hour},
		}}}, nil
	}
	server := NewServer(st)
	server.Workspace = "demo"
	server.WorkOrders = &workorder.Service{Store: st, ConfigProvider: provider}
	request := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	request = request.WithContext(withCredentialActor(request.Context(), core.AuthenticatedCredential{ID: "pat-token", OwnerUserID: "usr-owner", Kind: core.CredentialUser, Scope: core.CredentialScopeUser}))
	result, err := server.callMCPTool(request, "claim_work_order", map[string]any{
		"workspace_id": "demo", "work_order_id": job.ID, "session_id": "session", "client_token": "token", "claimant_id": "run:spoofed", "agent": "codex", "model": "gpt",
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed := result.(workorder.OrderDelivery).WorkOrder
	if got := claimed.LeaseExpiresAt.Sub(claimed.ExecutionStartedAt); got != core.DefaultWorkOrderClaimLease {
		t.Fatalf("MCP default claim lease = %s, want %s", got, core.DefaultWorkOrderClaimLease)
	}
	if claimed.ClaimantID != core.TaskRunClaimantID("usr-owner") {
		t.Fatalf("MCP claim identity = %q", claimed.ClaimantID)
	}
}

func TestMCPClaimWorksWithoutStoredForgeTokens(t *testing.T) {
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	st := store.NewMemory()
	task := core.Task{ID: "mcp-forge-token", Workspace: "demo", State: core.TaskRunning, CreatedAt: time.Now()}
	job := core.Job{ID: task.ID + "-implement-1", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued, Claimable: true}); err != nil {
		t.Fatal(err)
	}
	provider := func(context.Context) (*config.Config, error) {
		return &config.Config{Workspace: "demo", Routing: config.Routing{Stages: map[string]config.StageRoute{
			"implement": {Timeout: time.Hour},
		}}}, nil
	}
	server := NewServer(st)
	server.Workspace = "demo"
	server.WorkOrders = &workorder.Service{Store: st, ConfigProvider: provider}
	request := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	request = request.WithContext(withCredentialActor(request.Context(), core.AuthenticatedCredential{ID: "pat-token", OwnerUserID: "usr-owner", Kind: core.CredentialUser, Scope: core.CredentialScopeUser}))
	args := map[string]any{
		"workspace_id": "demo", "work_order_id": job.ID, "session_id": "session", "client_token": "token", "claimant_id": "run:spoofed", "agent": "codex", "model": "gpt",
	}
	listed, err := server.callMCPTool(request, "list_work_orders", map[string]any{"workspace_id": "demo"})
	if err != nil {
		t.Fatal(err)
	}
	orders := listed.([]core.WorkOrder)
	if len(orders) != 1 || !orders[0].Claimable || orders[0].ClaimRefusalReason != "" {
		t.Fatalf("claim projection=%+v", orders)
	}
	result, err := server.callMCPTool(request, "claim_work_order", args)
	if err != nil {
		t.Fatalf("configured-token MCP claim: %v", err)
	}
	if claimed := result.(workorder.OrderDelivery).WorkOrder; claimed.ClaimantID != core.TaskRunClaimantID("usr-owner") {
		t.Fatalf("MCP claim identity=%q", claimed.ClaimantID)
	}
}

func TestMCPClaimUsesCredentialOwnerForAssigneeEligibility(t *testing.T) {
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	st := store.NewMemory()
	now := time.Now().UTC()
	task := core.Task{ID: "mcp-assigned", Workspace: "demo", State: core.TaskRunning, CreatedAt: now}
	job := core.Job{ID: task.ID + "-implement-1", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued, Claimable: true, QueueEnteredAt: now}); err != nil {
		t.Fatal(err)
	}
	operatorCtx := store.WithActor(ctx, store.Actor{ID: store.UserActorID("operator"), Role: core.ActorUser})
	if err := store.SetMemoryWorkspaceMember(st, "demo", "usr-alice", true); err != nil {
		t.Fatal(err)
	}
	if _, err := taskops.New(st).SetAssignee(operatorCtx, task.ID, "usr-alice"); err != nil {
		t.Fatal(err)
	}
	provider := func(context.Context) (*config.Config, error) {
		return &config.Config{Workspace: "demo", Routing: config.Routing{Stages: map[string]config.StageRoute{
			"implement": {Timeout: time.Hour},
		}}}, nil
	}
	server := NewServer(st)
	server.Workspace = "demo"
	server.WorkOrders = &workorder.Service{Store: st, ConfigProvider: provider}
	args := map[string]any{
		"workspace_id": "demo", "work_order_id": job.ID, "session_id": "session", "client_token": "token", "claimant_id": core.TaskRunClaimantID("usr-alice"), "agent": "codex", "model": "gpt",
	}
	bob := core.AuthenticatedCredential{ID: "pat-bob", OwnerUserID: "usr-bob", Kind: core.CredentialUser, Scope: core.CredentialScopeUser}
	bobRequest := httptest.NewRequest(http.MethodPost, "/mcp", nil).WithContext(withCredentialActor(store.WithActor(t.Context(), store.SystemActor()), bob))
	if _, err := server.callMCPTool(bobRequest, "claim_work_order", args); err == nil || !strings.Contains(err.Error(), "usr-alice") {
		t.Fatalf("non-assignee MCP claim error=%v", err)
	}
	listed, err := server.callMCPTool(bobRequest, "list_work_orders", map[string]any{"workspace_id": "demo"})
	if err != nil {
		t.Fatal(err)
	}
	orders := listed.([]core.WorkOrder)
	if len(orders) != 1 || orders[0].Claimable || orders[0].Assignee == nil || orders[0].Assignee.UserID != "usr-alice" {
		t.Fatalf("non-assignee MCP projection=%+v", orders)
	}
	alice := core.AuthenticatedCredential{ID: "pat-alice", OwnerUserID: "usr-alice", Kind: core.CredentialUser, Scope: core.CredentialScopeUser}
	aliceRequest := httptest.NewRequest(http.MethodPost, "/mcp", nil).WithContext(withCredentialActor(store.WithActor(t.Context(), store.SystemActor()), alice))
	args["claimant_id"] = core.TaskRunClaimantID("usr-bob")
	result, err := server.callMCPTool(aliceRequest, "claim_work_order", args)
	if err != nil {
		t.Fatalf("assignee MCP claim: %v", err)
	}
	claimed := result.(workorder.OrderDelivery).WorkOrder
	if claimed.ClaimantID != core.TaskRunClaimantID("usr-alice") {
		t.Fatalf("assignee MCP claim identity = %q", claimed.ClaimantID)
	}
}

// A valid MCP submission with no verification-evidence artifact is admitted
// and advances to review; no workspace setting refuses it for missing evidence
// (req-review-gates-evidence AC-8.3; DEC-53).
func TestMCPSubmitForReviewAdmitsSubmissionWithoutVerificationEvidence(t *testing.T) {
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	st := store.NewMemory()
	task := core.Task{ID: "mcp-no-evidence", Workspace: "demo", Repo: "api", Title: "Deliver", Branch: "conveyor/task-mcp-no-evidence", BaseBranch: "main", State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: time.Now()}
	job := core.Job{ID: task.ID + "-implement-1", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement}); err != nil {
		t.Fatal(err)
	}
	if _, err := storetest.For(st).ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{SessionID: "session", ClientToken: "token", ClaimantID: core.TaskRunClaimantID("owner"), OwnerUserID: "owner", Lease: time.Minute}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Workspace: "demo",
		Repos:     []config.Repo{{Name: "api", URL: "https://github.com/acme/api.git", GitHub: "acme/api", Base: "main"}},
		Routing:   config.Routing{Stages: map[string]config.StageRoute{"review": {Execution: config.ExecutionMCP}}},
	}
	dispatcher := dispatch.New(st, cfg, nil)
	dispatcher.DisableMemoryQueueForTest()
	prWrites := 0
	prBody := ""
	server := NewServer(st)
	server.Workspace = "demo"
	server.WorkOrders = &workorder.Service{
		Store: st, Dispatcher: dispatcher,
		ConfigProvider: func(context.Context) (*config.Config, error) { return cfg, nil },
		DiscoverDocumentationPolicy: func(_ context.Context, _, revision string) githubtrigger.DocumentationPolicyDiscovery {
			return githubtrigger.DocumentationPolicyDiscovery{Revision: revision, State: "absent"}
		},
		SubmissionPR: func(context.Context, string, string) (githubtrigger.SubmissionPullRequest, error) {
			pr := githubtrigger.SubmissionPullRequest{Number: 7, URL: "https://github.com/acme/api/pull/7"}
			pr.Head.SHA, pr.Head.Ref = "abc123", task.Branch
			pr.Base.SHA, pr.Base.Ref = "base-sha", "main"
			return pr, nil
		},
		SubmissionChangedPaths: func(context.Context, *config.Config, core.Task) ([]string, error) {
			return []string{"internal/change.go"}, nil
		},
		ReviewDiffBetween: func(context.Context, string, string, string) (string, error) { return "diff", nil },
		ReconcileSubmissionPR: func(_ context.Context, _ string, _ githubtrigger.SubmissionPullRequest, body string) error {
			prWrites++
			prBody = body
			return nil
		},
		SubmissionPRWait: func(context.Context, time.Duration) error { return nil },
	}
	request := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	request = request.WithContext(withCredentialActor(request.Context(), core.AuthenticatedCredential{ID: "owner-token", OwnerUserID: "owner", Kind: core.CredentialUser}))
	if _, err := server.callMCPTool(request, "submit_for_review", map[string]any{
		"workspace_id": "demo", "head_sha": "abc123", "work_order_id": job.ID, "session_id": "session",
	}); err != nil {
		t.Fatalf("MCP submission without verification evidence refused: %v", err)
	}
	order, err := st.GetWorkOrder(ctx, job.ID)
	if err != nil || order.State != core.WorkOrderSubmitted {
		t.Fatalf("order after MCP submission: %+v err=%v", order, err)
	}
	current, err := st.GetTask(ctx, task.ID)
	if err != nil || current.NextStage != core.StageReview || prWrites != 1 {
		t.Fatalf("task after MCP submission: %+v pr_writes=%d err=%v", current, prWrites, err)
	}
	if strings.Contains(prBody, "<!-- conveyor:verification-evidence -->") {
		t.Fatalf("PR body lists verification evidence the task does not own: %s", prBody)
	}
}

func TestMCPReportUsagePersistsOptionalRateLimitWithoutGatingOrClearing(t *testing.T) {
	t.Parallel()
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	st := store.NewMemory()
	task := core.Task{ID: "mcp-rate-limit", Workspace: "demo", State: core.TaskRunning, CreatedAt: time.Now()}
	job := core.Job{ID: task.ID + "-implement-1", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement, RequiredHarness: "codex", RequiredModel: "gpt-5"}); err != nil {
		t.Fatal(err)
	}
	if _, err := storetest.For(st).ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{SessionID: "session", ClientToken: "secret", ClaimantID: core.TaskRunClaimantID("owner"), Lease: time.Minute}); err != nil {
		t.Fatal(err)
	}
	server := NewServer(st)
	server.Workspace = "demo"
	server.WorkOrders = &workorder.Service{Store: st}
	request := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	request = request.WithContext(withCredentialActor(request.Context(), core.AuthenticatedCredential{ID: "owner-token", OwnerUserID: "owner", Kind: core.CredentialUser}))
	reset := "2026-07-28T13:00:00Z"
	result, err := server.callMCPTool(request, "report_usage", map[string]any{
		"workspace_id": "demo", "work_order_id": job.ID, "session_id": "session",
		"tokens_in": 100.0, "tokens_out": 25.0,
		"rate_limit": map[string]any{"status": "limited", "limit": 1000.0, "remaining": 125.0, "reset_at": reset},
	})
	if err != nil {
		t.Fatal(err)
	}
	reported := result.(core.WorkOrder)
	if reported.RateLimit == nil || reported.RateLimit.Status != "limited" || reported.RateLimit.Remaining == nil || *reported.RateLimit.Remaining != 125 || reported.RateLimit.ResetAt == nil || reported.RateLimit.ResetAt.Format(time.RFC3339) != reset || reported.RateLimitObservedAt.IsZero() {
		t.Fatalf("reported rate limit=%+v observed=%s", reported.RateLimit, reported.RateLimitObservedAt)
	}
	result, err = server.callMCPTool(request, "report_usage", map[string]any{
		"workspace_id": "demo", "work_order_id": job.ID, "session_id": "session",
		"tokens_in": 150.0, "tokens_out": 30.0,
	})
	if err != nil {
		t.Fatal(err)
	}
	withoutRateLimit := result.(core.WorkOrder)
	if withoutRateLimit.RateLimit == nil || withoutRateLimit.RateLimit.Status != "limited" || withoutRateLimit.RateLimitObservedAt != reported.RateLimitObservedAt {
		t.Fatalf("rate limit was cleared by absent field: before=%+v after=%+v", reported, withoutRateLimit)
	}
	if _, err = server.callMCPTool(request, "report_usage", map[string]any{
		"workspace_id": "demo", "work_order_id": job.ID, "session_id": "session",
		"tokens_in": 150.0, "tokens_out": 30.0,
		"rate_limit": map[string]any{"status": "limited", "reset_at": "not-rfc3339"},
	}); err == nil || !strings.Contains(err.Error(), "rate_limit") {
		t.Fatalf("invalid reset_at error=%v", err)
	}
}

func TestMCPUsageSurfacesForImplementationAndReviewOrders(t *testing.T) {
	t.Parallel()
	for _, stage := range []core.Stage{core.StageImplement, core.StageReview} {
		stage := stage
		t.Run(string(stage), func(t *testing.T) {
			t.Parallel()
			ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
			st := store.NewMemory()
			task := core.Task{ID: "usage-" + string(stage), Workspace: "demo", State: core.TaskRunning, CreatedAt: time.Now()}
			job := core.Job{ID: task.ID + "-1", TaskID: task.ID, Stage: stage, State: core.JobPending}
			if err := st.CreateTask(ctx, task); err != nil {
				t.Fatal(err)
			}
			if err := st.CreateJob(ctx, job); err != nil {
				t.Fatal(err)
			}
			if err := storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: stage}); err != nil {
				t.Fatal(err)
			}
			if _, err := storetest.For(st).ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{SessionID: "session", ClientToken: "secret", ClaimantID: core.TaskRunClaimantID("owner"), Lease: time.Minute}); err != nil {
				t.Fatal(err)
			}
			server := NewServer(st)
			server.Workspace = "demo"
			server.WorkOrders = &workorder.Service{Store: st}
			request := httptest.NewRequest(http.MethodPost, "/mcp", nil)
			request = request.WithContext(withCredentialActor(request.Context(), core.AuthenticatedCredential{ID: "owner-token", OwnerUserID: "owner", Kind: core.CredentialUser}))
			result, err := server.callMCPTool(request, "report_usage", map[string]any{
				"workspace_id": "demo", "work_order_id": job.ID, "session_id": "session",
				"tokens_in": 1200.0, "tokens_out": 300.0,
			})
			if err != nil {
				t.Fatal(err)
			}
			reported := result.(core.WorkOrder)
			if reported.TokensIn != 1200 || reported.TokensOut != 300 || reported.CostUSD != 0 || !reported.UsageReported || !reported.SelfReported {
				t.Fatalf("reported %s usage=%+v", stage, reported)
			}
		})
	}
}

// report_usage is token telemetry only: the schema neither lists nor requires
// cost, and a legacy cost_usd argument is accepted and ignored, including a
// negative or malformed value (req-usage-telemetry REQ-2, AC-2.1; DEC-1).
func TestMCPUsageSchemaOmitsCost(t *testing.T) {
	t.Parallel()
	for _, tool := range mcpTools() {
		if tool["name"] != "report_usage" {
			continue
		}
		schema := tool["inputSchema"].(map[string]any)
		properties := schema["properties"].(map[string]any)
		if _, ok := properties["cost_usd"]; ok {
			t.Fatalf("report_usage still lists cost_usd: %+v", properties)
		}
		if required := schema["required"]; !slices.Equal(required.([]string), []string{"work_order_id", "session_id", "tokens_in", "tokens_out"}) {
			t.Fatalf("report_usage required = %v", required)
		}
		if description, _ := tool["description"].(string); strings.Contains(strings.ToLower(description), "cost") {
			t.Fatalf("report_usage description still asks for cost: %s", description)
		}
		return
	}
	t.Fatal("report_usage tool not found")
}

func TestMCPUsageAcceptsAndIgnoresLegacyCost(t *testing.T) {
	t.Parallel()
	for name, cost := range map[string]any{"positive": 1.5, "negative": -2.0, "malformed": "free"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
			st := store.NewMemory()
			task := core.Task{ID: "legacy-cost-" + name, Workspace: "demo", State: core.TaskRunning, CreatedAt: time.Now()}
			job := core.Job{ID: task.ID + "-1", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
			if err := st.CreateTask(ctx, task); err != nil {
				t.Fatal(err)
			}
			if err := st.CreateJob(ctx, job); err != nil {
				t.Fatal(err)
			}
			if err := storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement}); err != nil {
				t.Fatal(err)
			}
			if _, err := storetest.For(st).ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{SessionID: "session", ClientToken: "secret", ClaimantID: core.TaskRunClaimantID("owner"), Lease: time.Minute}); err != nil {
				t.Fatal(err)
			}
			server := NewServer(st)
			server.Workspace = "demo"
			server.WorkOrders = &workorder.Service{Store: st}
			request := httptest.NewRequest(http.MethodPost, "/mcp", nil)
			request = request.WithContext(withCredentialActor(request.Context(), core.AuthenticatedCredential{ID: "owner-token", OwnerUserID: "owner", Kind: core.CredentialUser}))
			result, err := server.callMCPTool(request, "report_usage", map[string]any{
				"workspace_id": "demo", "work_order_id": job.ID, "session_id": "session",
				"tokens_in": 70.0, "tokens_out": 11.0, "cost_usd": cost,
			})
			if err != nil {
				t.Fatalf("legacy cost_usd rejected: %v", err)
			}
			if reported := result.(core.WorkOrder); reported.CostUSD != 0 || reported.TokensIn != 70 || reported.TokensOut != 11 || !reported.UsageReported {
				t.Fatalf("reported=%+v", reported)
			}
			stored, err := st.GetWorkOrder(ctx, job.ID)
			if err != nil || stored.CostUSD != 0 {
				t.Fatalf("stored order cost=%v err=%v", stored.CostUSD, err)
			}
			storedJob, ok, err := st.GetLatestJob(ctx, task.ID)
			if err != nil || !ok || storedJob.CostUSD != nil || storedJob.TokensIn != 70 {
				t.Fatalf("stored job=%+v ok=%v err=%v", storedJob, ok, err)
			}
			events, err := st.ListEvents(ctx, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			for _, event := range events {
				if event.Kind == "work_order.usage_reported" && strings.Contains(string(event.Payload), "cost_usd") {
					t.Fatalf("usage event carries cost: %s", event.Payload)
				}
			}
			if _, err = server.callMCPTool(request, "report_usage", map[string]any{
				"workspace_id": "demo", "work_order_id": job.ID, "session_id": "session",
				"tokens_in": -1.0, "tokens_out": 11.0, "cost_usd": cost,
			}); err == nil || !strings.Contains(err.Error(), "negative") {
				t.Fatalf("negative tokens error=%v", err)
			}
		})
	}
}

func TestMCPUsageToolDescribesBestEffortObservationalPosture(t *testing.T) {
	t.Parallel()
	for _, tool := range mcpTools() {
		if tool["name"] != "report_usage" {
			continue
		}
		description, _ := tool["description"].(string)
		for _, required := range []string{"cumulative", "natural checkpoints", "immediately before", "missing usage never blocks", "DEC-1"} {
			if !strings.Contains(description, required) {
				t.Fatalf("report_usage description is missing %q: %s", required, description)
			}
		}
		return
	}
	t.Fatal("report_usage tool not found")
}

func TestMCPToolsListRequiresAuthAndPublishesLifecycle(t *testing.T) {
	t.Parallel()
	server := NewServer(store.NewMemory())
	server.BearerToken = "operator-token"
	handler := server.Handler()

	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d", unauthorized.Code)
	}

	request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	request.Header.Set("Authorization", "Bearer operator-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
	var envelope struct {
		Result struct {
			Tools []struct {
				Name        string `json:"name"`
				Description string `json:"description"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	want := []string{"list_workspaces", "list_repositories", "list_tasks", "get_task", "list_operator_inbox", "list_task_events", "get_task_context", "list_documents", "get_document", "search_documents", "list_document_events", "list_decisions", "get_decision", "get_lineage", "create_task", "add_task_dependency", "set_assignee", "attach_task_branch", "get_task_status", "list_work_orders", "claim_work_order", "renew_work_order", "release_work_order", "request_plan_revision", "get_work_order", "refresh_work_order_context", "read_artifact", "report_progress", "report_usage", "upload_transcript", "submit_plan", "submit_for_review", "await_review", "submit_review_verdict", "submit_risk_rating", "submit_review_summary", "get_stage_preferences", "set_stage_preference"}
	want = append(append(append([]string{}, want[:14]...), verificationTools...), want[14:]...)
	if len(envelope.Result.Tools) != len(want) {
		t.Fatalf("tools = %d, want %d", len(envelope.Result.Tools), len(want))
	}
	for i, name := range want {
		if envelope.Result.Tools[i].Name != name {
			t.Fatalf("tool[%d] = %q, want %q", i, envelope.Result.Tools[i].Name, name)
		}
		if strings.Contains(name, "preempt") {
			t.Fatalf("operator-only preempt leaked into MCP tool %q", name)
		}
		if name == "create_task" && !strings.Contains(envelope.Result.Tools[i].Description, "invalid_agent_gate_override") {
			t.Fatalf("create_task description omits the mistyped gate rule: %q", envelope.Result.Tools[i].Description)
		}
		if name == "claim_work_order" {
			description := envelope.Result.Tools[i].Description
			if !strings.Contains(description, "claimant_id") || !strings.Contains(description, "ignored") || !strings.Contains(description, "authenticated credential") {
				t.Fatalf("claim_work_order description=%q", description)
			}
		}
	}
}

func TestMCPAddTaskDependencyRunsUnderAPersonsCredential(t *testing.T) {
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	st := store.NewMemory()
	for _, task := range []core.Task{
		{ID: "mcp-link-dependent", Workspace: "demo", State: core.TaskRunning, CreatedAt: time.Now().UTC()},
		{ID: "mcp-link-dependency", Workspace: "demo", State: core.TaskRunning, CreatedAt: time.Now().UTC()},
	} {
		if err := st.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
	}
	server := NewServer(st)
	server.Workspace = "demo"
	args := map[string]any{
		"workspace_id": "demo", "task_id": "mcp-link-dependent", "depends_on_task_id": "mcp-link-dependency",
		"reason": "operator ordering", "request_id": "mcp-link-request",
	}
	humanRequest := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	humanRequest = humanRequest.WithContext(withCredentialActor(humanRequest.Context(), core.AuthenticatedCredential{ID: "pat", OwnerUserID: "operator", Kind: core.CredentialUser}))
	result, err := server.callMCPTool(humanRequest, "add_task_dependency", args)
	if err != nil || !result.(store.DependencyAdditionResult).Added {
		t.Fatalf("human result=%+v err=%v", result, err)
	}

}

func TestMCPAttachTaskBranchRunsUnderAPersonsCredential(t *testing.T) {
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	st := store.NewMemory()
	task := core.Task{ID: "mcp-attach", Workspace: "demo", Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-mcp-attach", State: core.TaskRunning, CreatedAt: time.Now().UTC()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	server := NewServer(st)
	server.Workspace = "demo"
	args := map[string]any{"workspace_id": "demo", "task_id": task.ID, "branch": "feature/mcp"}
	humanRequest := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	humanRequest = humanRequest.WithContext(withCredentialActor(humanRequest.Context(), core.AuthenticatedCredential{ID: "pat", OwnerUserID: "operator", Kind: core.CredentialUser}))
	result, err := server.callMCPTool(humanRequest, "attach_task_branch", args)
	got, _ := result.(core.Task)
	if err != nil || got.Branch != "feature/mcp" {
		t.Fatalf("human result=%+v err=%v", result, err)
	}
}

func TestMCPAttachTaskBranchRefusals(t *testing.T) {
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	st := store.NewMemory()
	task := core.Task{ID: "mcp-attach-refuse", Workspace: "demo", Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-mcp-attach-refuse", State: core.TaskRunning, CreatedAt: time.Now().UTC()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	server := NewServer(st)
	server.Workspace = "demo"
	request := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	request = request.WithContext(withCredentialActor(request.Context(), core.AuthenticatedCredential{ID: "pat", OwnerUserID: "operator", Kind: core.CredentialUser}))
	call := func(branch string) error {
		t.Helper()
		_, err := server.callMCPTool(request, "attach_task_branch", map[string]any{"workspace_id": "demo", "task_id": task.ID, "branch": branch})
		return err
	}
	if err := call("HEAD"); err == nil || !strings.Contains(err.Error(), "invalid_branch") {
		t.Fatalf("illegal=%v", err)
	}
	if err := call("main"); err == nil || !strings.Contains(err.Error(), "branch_not_attachable") {
		t.Fatalf("base=%v", err)
	}
}

func TestEveryRegisteredMCPToolHasExplicitCapability(t *testing.T) {
	seen := make(map[string]bool, len(mcpCapabilities))
	for _, tool := range mcpTools() {
		name, _ := tool["name"].(string)
		capability, ok := mcpCapabilities[name]
		if !ok || capability == "" {
			t.Fatalf("registered MCP tool %q lacks an explicit capability", name)
		}
		seen[name] = true
	}
	// Task intake is gated by its dedicated capability on REST and MCP alike
	// (DEC-60; component-identity-membership).
	if got := mcpCapabilities["create_task"]; got != core.CapabilityCreateTasks {
		t.Fatalf("create_task capability = %q, want %q", got, core.CapabilityCreateTasks)
	}
	for name := range mcpCapabilities {
		if !seen[name] {
			t.Fatalf("capability mapping names unregistered MCP tool %q", name)
		}
	}
}

// reservedMCPToolViolations names every tool whose capability AC-1.2 does not
// admit on the list: each tool maps to view_workspace, claim_work, create_tasks,
// or set_assignee, and only add_task_dependency and attach_task_branch may map
// to operate_gates. An unmapped tool has no admitted capability, so it fails too.
func reservedMCPToolViolations(tools []map[string]any, capabilities map[string]core.Capability) []string {
	admitted := map[core.Capability]bool{
		core.CapabilityViewWorkspace: true,
		core.CapabilityClaimWork:     true,
		core.CapabilityCreateTasks:   true,
		core.CapabilitySetAssignee:   true,
	}
	operateGatesExceptions := map[string]bool{"add_task_dependency": true, "attach_task_branch": true}
	var violations []string
	for _, tool := range tools {
		name, _ := tool["name"].(string)
		capability := capabilities[name]
		if admitted[capability] || (operateGatesExceptions[name] && capability == core.CapabilityOperateGates) {
			continue
		}
		violations = append(violations, fmt.Sprintf("MCP tool %q maps to capability %q", name, capability))
	}
	return violations
}

// TestMCPToolListHoldsNoReservedTool enforces that no listed tool decides a
// gate, cancels a task, writes configuration, recovers a work order, unlinks a
// dependency, manages users or grants, or issues a token. It iterates mcpTools()
// itself, so a tool added later is held to the same rule, and its controls prove
// the check fails and names the tool (req-security-boundaries AC-1.2;
// component-mcp-protocol invariant 7).
func TestMCPToolListHoldsNoReservedTool(t *testing.T) {
	tools := mcpTools()
	if len(tools) == 0 {
		t.Fatal("mcpTools() is empty, so the check would hold vacuously")
	}
	if violations := reservedMCPToolViolations(tools, mcpCapabilities); len(violations) != 0 {
		t.Fatalf("the tool list holds reserved tools:\n%s", strings.Join(violations, "\n"))
	}

	const controlTool = "control_recover_tool"
	for name, tc := range map[string]struct {
		tool       string
		capability core.Capability
	}{
		"tool mapped to recover_work":                  {controlTool, core.CapabilityRecoverWork},
		"exception tool mapped to recover_work":        {"add_task_dependency", core.CapabilityRecoverWork},
		"tool outside the exceptions on operate_gates": {controlTool, core.CapabilityOperateGates},
		"tool with no capability":                      {controlTool, ""},
	} {
		t.Run("control/"+name, func(t *testing.T) {
			controlTools := mcpTools()
			if tc.tool == controlTool {
				controlTools = append(controlTools, map[string]any{"name": controlTool})
			}
			capabilities := maps.Clone(mcpCapabilities)
			capabilities[tc.tool] = tc.capability
			violations := reservedMCPToolViolations(controlTools, capabilities)
			if len(violations) != 1 || !strings.Contains(violations[0], `"`+tc.tool+`"`) {
				t.Fatalf("violations = %q, want one naming %s", violations, tc.tool)
			}
		})
	}
}

// TestMCPRedispatchWorkOrderIsNotOffered proves over the wire that tools/list
// omits the retired tool and that a call to its name answers a refusal and
// changes nothing, on an order the store command would still redispatch
// (req-security-boundaries AC-1.2; component-mcp-protocol invariants 6 and 7).
func TestMCPRedispatchWorkOrderIsNotOffered(t *testing.T) {
	const retiredTool = "redispatch_work_order"
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	st := store.NewMemory()
	now := time.Now().UTC()
	task := core.Task{ID: "retired-tool-task", Workspace: "demo", Repo: "conveyor", State: core.TaskQueued, NextStage: core.StageImplement, CreatedAt: now}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	order := core.WorkOrder{ID: task.ID + "-implement-1", TaskID: task.ID, JobID: task.ID + "-implement-1", Stage: core.StageImplement, State: core.WorkOrderQueued, QueueEnteredAt: now.Add(-2 * time.Hour), QueueDeadline: now.Add(-time.Hour)}
	if err := st.CreateJob(ctx, core.Job{ID: order.JobID, TaskID: task.ID, Stage: order.Stage, State: core.JobPending}); err != nil {
		t.Fatal(err)
	}
	if err := storetest.For(st).CreateWorkOrder(ctx, order); err != nil {
		t.Fatal(err)
	}
	provider := func(context.Context) (*config.Config, error) {
		return &config.Config{Workspace: "demo", WorkOrderQueueTimeout: time.Hour}, nil
	}
	server := NewServer(st)
	server.BearerToken, server.Workspace = "operator-token", "demo"
	server.WorkOrders = &workorder.Service{Store: st, ConfigProvider: provider}
	handler := server.Handler()
	post := func(payload map[string]any) []byte {
		t.Helper()
		body, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
		request.Header.Set("Authorization", "Bearer operator-token")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
		return response.Body.Bytes()
	}

	var listed struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(post(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"}), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Result.Tools) == 0 {
		t.Fatal("tools/list returned no tools")
	}
	for _, tool := range listed.Result.Tools {
		if tool.Name == retiredTool {
			t.Fatalf("tools/list still offers %s", retiredTool)
		}
	}
	if _, mapped := mcpCapabilities[retiredTool]; mapped {
		t.Fatalf("%s still has a capability", retiredTool)
	}

	beforeOrder, err := st.GetWorkOrder(ctx, order.ID)
	if err != nil || beforeOrder.State != core.WorkOrderStale {
		t.Fatalf("fixture order=%+v err=%v, want stale", beforeOrder, err)
	}
	beforeEvents, err := st.ListEvents(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var called struct {
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(post(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": map[string]any{
		"name": retiredTool, "arguments": map[string]any{"workspace_id": "demo", "work_order_id": order.ID},
	}}), &called); err != nil {
		t.Fatal(err)
	}
	if !called.Result.IsError || len(called.Result.Content) != 1 || !strings.Contains(called.Result.Content[0].Text, "unknown tool") {
		t.Fatalf("call result = %+v, want the unknown-tool refusal", called.Result)
	}
	afterOrder, err := st.GetWorkOrder(ctx, order.ID)
	if err != nil {
		t.Fatal(err)
	}
	afterEvents, err := st.ListEvents(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(beforeOrder, afterOrder) || len(afterEvents) != len(beforeEvents) {
		t.Fatalf("refused call changed state: order %+v -> %+v, events %d -> %d", beforeOrder, afterOrder, len(beforeEvents), len(afterEvents))
	}
	// Control: the order was redispatchable, so the name alone caused the refusal.
	if redispatched, err := storetest.RedispatchWorkOrder(ctx, st, order.ID, time.Hour); err != nil || redispatched.RedispatchCount != 1 {
		t.Fatalf("store redispatch after the refusal = %+v err=%v", redispatched, err)
	}
}

func TestMCPClaimantBoundToolsRejectForeignUsers(t *testing.T) {
	tools := []string{"report_progress", "report_usage", "upload_transcript", "submit_plan", "submit_for_review", "await_review", "submit_review_verdict"}
	for _, tool := range tools {
		tool := tool
		t.Run(tool, func(t *testing.T) {
			setup := func(t *testing.T) (*Server, string) {
				t.Helper()
				ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
				st := store.NewMemory()
				taskID := "claim-bound-" + tool + "-" + core.NewTaskID()
				stage := core.StageImplement
				if tool == "submit_plan" {
					stage = core.StageSpec
				} else if tool == "submit_review_verdict" {
					stage = core.StageReview
				}
				jobID := taskID + "-" + string(stage) + "-1"
				task := core.Task{ID: taskID, Workspace: "demo", Repo: "conveyor", Branch: "conveyor/" + taskID, BaseBranch: "main", State: core.TaskRunning, NextStage: stage, SpecApproval: tool == "submit_plan", PolicyVersion: 1, CreatedAt: time.Now()}
				if err := st.CreateTask(ctx, task); err != nil {
					t.Fatal(err)
				}
				if err := st.CreateJob(ctx, core.Job{ID: jobID, TaskID: taskID, Stage: stage, State: core.JobPending}); err != nil {
					t.Fatal(err)
				}
				order := core.WorkOrder{ID: jobID, TaskID: taskID, JobID: jobID, Stage: stage, State: core.WorkOrderQueued}
				if stage == core.StageReview {
					order.ReviewRound, order.ReviewSeat = 1, 1
					order.ServedRequirementSnapshot = []core.ServedRequirementContext{}
					order.GovernanceSnapshot = &core.GovernanceSnapshot{Designs: []core.GovernanceDesignContext{}, Decisions: []core.Decision{}, PendingDesignProposals: []core.PendingSystemDesignProposal{}}
				}
				if err := storetest.For(st).CreateWorkOrder(ctx, order); err != nil {
					t.Fatal(err)
				}
				claim := core.WorkOrderClaim{SessionID: "victim-session", ClientToken: "secret", ClaimantID: core.TaskRunClaimantID("owner"), Lease: time.Minute}
				if _, err := storetest.For(st).ClaimWorkOrder(ctx, jobID, claim); err != nil {
					t.Fatal(err)
				}
				cfg := &config.Config{Workspace: "demo", MaxBounces: 2, Repos: []config.Repo{{Name: "conveyor", Base: "main"}}, Routing: config.Routing{Stages: map[string]config.StageRoute{
					"spec": {Execution: config.ExecutionMCP, Timeout: time.Hour}, "implement": {Execution: config.ExecutionMCP, Timeout: time.Hour}, "review": {Execution: config.ExecutionMCP, Timeout: time.Hour},
				}}}
				dispatcher := dispatch.New(st, cfg, nil)
				dispatcher.DisableMemoryQueueForTest()
				server := NewServer(st)
				server.Workspace = "demo"
				server.WorkOrders = &workorder.Service{Store: st, Dispatcher: dispatcher, ConfigProvider: func(context.Context) (*config.Config, error) { return cfg, nil }}
				if tool == "await_review" {
					claimed, err := st.GetWorkOrder(ctx, jobID)
					if err != nil {
						t.Fatal(err)
					}
					claimed.State = core.WorkOrderSubmitted
					if err = storetest.For(st).UpdateWorkOrder(ctx, claimed, core.WorkOrderCmdSubmitForReview); err != nil {
						t.Fatal(err)
					}
				}
				return server, jobID
			}
			args := func(orderID string) map[string]any {
				return map[string]any{
					"workspace_id": "demo", "work_order_id": orderID, "session_id": "victim-session", "head_sha": "named-head",
					"message": "progress", "tokens_in": 1.0, "tokens_out": 1.0, "cost_usd": 0.0,
					"transcript":    "redacted",
					"markdown":      "## Approach\nUse the approved path.\n\n## Files touched\n- internal/httpapi/mcp.go\n\n## Ordering\n1. Implement.\n\n## Risks\n- Drift.\n\n## Done criteria\n- The change is tested.",
					"decomposition": []any{}, "timeout_seconds": 0.001,
					"verdict": "changes_requested", "reason_code": "tests", "summary": "claimant admission succeeds", "feedback": "add coverage",
					"requirement_citations":  map[string]any{"applicable": false, "cited_ids": []any{}, "unknown_ids": []any{}, "unserved_ids": []any{}, "conflicts": []any{}},
					"done_criteria_coverage": map[string]any{"applicable": false, "summary": "No execution plan is available", "satisfied": []any{}, "unsatisfied": []any{}, "unverified": []any{}, "conflicts": []any{}},
					"governance_assessment":  map[string]any{"design_applicable": false, "decision_citable": false, "cited_ids": []any{}, "unknown_ids": []any{}, "ungoverned_ids": []any{}, "superseded_ids": []any{}, "conflicts": []any{}},
				}
			}

			userServer, userOrderID := setup(t)
			foreignUser := httptest.NewRequest(http.MethodPost, "/mcp", nil)
			foreignUser = foreignUser.WithContext(withCredentialActor(foreignUser.Context(), core.AuthenticatedCredential{ID: "foreign", OwnerUserID: "foreign", Kind: core.CredentialUser}))
			if _, err := userServer.callMCPTool(foreignUser, tool, args(userOrderID)); !errors.Is(err, store.ErrWorkOrderClaimUnauthorized) {
				t.Fatalf("foreign user error=%v", err)
			}

			owner := httptest.NewRequest(http.MethodPost, "/mcp", nil)
			owner = owner.WithContext(withCredentialActor(owner.Context(), core.AuthenticatedCredential{ID: "owner", OwnerUserID: "owner", Kind: core.CredentialUser}))
			if _, err := userServer.callMCPTool(owner, tool, args(userOrderID)); err != nil {
				t.Fatalf("owning user call failed: %v", err)
			}
		})
	}
}

func TestWorkOrderListSurfacesRedactForeignSessionIDs(t *testing.T) {
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	st := store.NewMemory()
	for _, owner := range []string{"owner", "other"} {
		taskID := "list-session-" + owner
		jobID := taskID + "-implement-1"
		if err := st.CreateTask(ctx, core.Task{ID: taskID, Workspace: "demo", State: core.TaskRunning, CreatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
		if err := st.CreateJob(ctx, core.Job{ID: jobID, TaskID: taskID, Stage: core.StageImplement, State: core.JobPending}); err != nil {
			t.Fatal(err)
		}
		if err := storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: jobID, TaskID: taskID, JobID: jobID, Stage: core.StageImplement, State: core.WorkOrderQueued}); err != nil {
			t.Fatal(err)
		}
		if _, err := storetest.For(st).ClaimWorkOrder(ctx, jobID, core.WorkOrderClaim{SessionID: owner + "-session", ClientToken: "secret", ClaimantID: core.TaskRunClaimantID(owner), Lease: time.Minute}); err != nil {
			t.Fatal(err)
		}
	}
	server := NewServer(st)
	server.Workspace = "demo"
	server.WorkOrders = &workorder.Service{Store: st}
	ownerRequest := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	ownerRequest = ownerRequest.WithContext(withCredentialActor(ownerRequest.Context(), core.AuthenticatedCredential{ID: "owner", OwnerUserID: "owner", Kind: core.CredentialUser}))
	result, err := server.callMCPTool(ownerRequest, "list_work_orders", map[string]any{"workspace_id": "demo"})
	if err != nil {
		t.Fatal(err)
	}
	assertProjection := func(t *testing.T, orders []core.WorkOrder) {
		t.Helper()
		if len(orders) != 2 {
			t.Fatalf("orders=%+v", orders)
		}
		for _, order := range orders {
			switch order.TaskID {
			case "list-session-owner":
				if order.SessionID != "owner-session" {
					t.Fatalf("owner session=%q", order.SessionID)
				}
			case "list-session-other":
				if order.SessionID != "" {
					t.Fatalf("foreign session disclosed=%q", order.SessionID)
				}
			}
		}
	}
	assertProjection(t, result.([]core.WorkOrder))

	restRequest := httptest.NewRequest(http.MethodGet, "/v1/work-orders?workspace_id=demo", nil)
	restRequest = restRequest.WithContext(withCredentialActor(store.WithWorkspace(restRequest.Context(), "demo"), core.AuthenticatedCredential{ID: "owner", OwnerUserID: "owner", Kind: core.CredentialUser}))
	restResponse := httptest.NewRecorder()
	server.listWorkOrders(restResponse, restRequest)
	if restResponse.Code != http.StatusOK {
		t.Fatalf("REST status=%d body=%s", restResponse.Code, restResponse.Body.String())
	}
	var restOrders []core.WorkOrder
	if err := json.Unmarshal(restResponse.Body.Bytes(), &restOrders); err != nil {
		t.Fatal(err)
	}
	assertProjection(t, restOrders)
}

func TestMCPRequestPlanRevisionEndToEnd(t *testing.T) {
	t.Parallel()
	type fixture struct {
		server  *Server
		request *http.Request
		args    map[string]any
		store   store.Store
		taskID  string
	}
	setup := func(t *testing.T, stage core.Stage, approved, claimed bool, planState ...core.PlanState) fixture {
		t.Helper()
		ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
		st := store.NewMemory()
		taskID := "plan-revision-" + core.NewTaskID()
		now := time.Now().UTC()
		// An approved plan is planning completed; a task without one
		// requires no plan unless the case states otherwise.
		state := core.PlanNotRequired
		if approved {
			state = core.PlanAwaitingApproval
		}
		if len(planState) > 0 {
			state = planState[0]
		}
		if err := st.CreateTask(ctx, core.Task{ID: taskID, Workspace: "demo", Repo: "conveyor", State: core.TaskRunning, NextStage: stage, PlanState: state, CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
		if approved {
			plan, err := st.CreateSpecVersion(ctx, core.SpecVersion{TaskID: taskID, Content: "approved plan"})
			if err != nil {
				t.Fatal(err)
			}
			if err = st.ApproveSpecVersion(ctx, taskID, plan.Version); err != nil {
				t.Fatal(err)
			}
		}
		orderID := taskID + "-order"
		if err := st.CreateJob(ctx, core.Job{ID: orderID, TaskID: taskID, Stage: stage, State: core.JobPending}); err != nil {
			t.Fatal(err)
		}
		if err := storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: orderID, TaskID: taskID, JobID: orderID, Stage: stage, State: core.WorkOrderQueued}); err != nil {
			t.Fatal(err)
		}
		if claimed {
			if _, err := storetest.For(st).ClaimWorkOrder(ctx, orderID, core.WorkOrderClaim{SessionID: "session-a", ClientToken: "secret", ClaimantID: core.TaskRunClaimantID("owner-a"), Agent: "codex", Model: "model", Lease: time.Minute, ExecutionTimeout: time.Hour}); err != nil {
				t.Fatal(err)
			}
		}
		server := NewServer(st)
		server.Workspace = "demo"
		server.WorkOrders = &workorder.Service{Store: st}
		request := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		request = request.WithContext(withCredentialActor(request.Context(), core.AuthenticatedCredential{ID: "pat-owner-a", OwnerUserID: "owner-a", Kind: core.CredentialUser, Scope: core.CredentialScopeUser}))
		return fixture{server: server, request: request, args: map[string]any{"workspace_id": "demo", "work_order_id": orderID, "session_id": "session-a", "rationale": "  plan conflicts with the API  "}, store: st, taskID: taskID}
	}

	t.Run("happy path", func(t *testing.T) {
		item := setup(t, core.StageImplement, true, true)
		result, err := item.server.callMCPTool(item.request, "request_plan_revision", item.args)
		if err != nil {
			t.Fatal(err)
		}
		got := result.(store.PlanRevisionRequestResult)
		if got.Rationale != "plan conflicts with the API" || got.PlanVersion != 1 || got.Task.State != core.TaskAwaiting || got.WorkOrder.State != core.WorkOrderQueued || !got.WorkOrder.RetrySuppressed {
			t.Fatalf("result=%+v", got)
		}
	})

	// A no-plan-required task's implement claim is admitted: the call asks
	// that the task be planned and contests no plan version
	// (component-mcp-protocol invariant 39; component-work-orders invariant
	// 57; req-task-plan-state AC-3.3).
	t.Run("no plan required asks for planning", func(t *testing.T) {
		item := setup(t, core.StageImplement, false, true, core.PlanNotRequired)
		result, err := item.server.callMCPTool(item.request, "request_plan_revision", item.args)
		if err != nil {
			t.Fatal(err)
		}
		got := result.(store.PlanRevisionRequestResult)
		if got.PlanVersion != 0 || got.Task.State != core.TaskAwaiting || got.Task.GateKind != core.GateKindPlanRevision || got.Task.PlanState != core.PlanNotRequired || got.WorkOrder.State != core.WorkOrderQueued || !got.WorkOrder.RetrySuppressed {
			t.Fatalf("result=%+v", got)
		}
	})

	for _, test := range []struct {
		name                 string
		stage                core.Stage
		approved, claimed    bool
		sessionID, rationale string
		planState            core.PlanState
	}{
		{name: "blank rationale", stage: core.StageImplement, approved: true, claimed: true, sessionID: "session-a", rationale: " "},
		{name: "wrong stage", stage: core.StageReview, approved: true, claimed: true, sessionID: "session-a", rationale: "wrong"},
		{name: "missing approved plan", stage: core.StageImplement, claimed: true, sessionID: "session-a", rationale: "wrong", planState: core.PlanCompleted},
		{name: "planning needed", stage: core.StageImplement, claimed: true, sessionID: "session-a", rationale: "wrong", planState: core.PlanNeeded},
		{name: "stale session", stage: core.StageImplement, approved: true, claimed: true, sessionID: "stale", rationale: "wrong"},
		{name: "unclaimed", stage: core.StageImplement, approved: true, sessionID: "session-a", rationale: "wrong"},
	} {
		t.Run(test.name+" is in-band and non-mutating", func(t *testing.T) {
			var states []core.PlanState
			if test.planState != "" {
				states = append(states, test.planState)
			}
			item := setup(t, test.stage, test.approved, test.claimed, states...)
			item.args["session_id"], item.args["rationale"] = test.sessionID, test.rationale
			beforeOrder, _ := item.store.GetWorkOrder(store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo"), item.args["work_order_id"].(string))
			beforeTask, _ := item.store.GetTask(store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo"), item.taskID)
			beforeEvents, _ := item.store.ListEvents(store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo"), item.taskID)
			if _, err := item.server.callMCPTool(item.request, "request_plan_revision", item.args); err == nil {
				t.Fatal("request unexpectedly succeeded")
			}
			afterOrder, _ := item.store.GetWorkOrder(store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo"), item.args["work_order_id"].(string))
			afterTask, _ := item.store.GetTask(store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo"), item.taskID)
			afterEvents, _ := item.store.ListEvents(store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo"), item.taskID)
			if beforeOrder.State != afterOrder.State || beforeOrder.SessionID != afterOrder.SessionID || beforeTask.State != afterTask.State || len(beforeEvents) != len(afterEvents) {
				t.Fatalf("request mutated rejected projection")
			}
		})
	}
}

// TestMCPRenewHonorsLeaseSeconds proves non-worker renewal applies the
// session's lease_seconds within the claim bounds, falls back to the default
// otherwise, and never moves the lease past the execution deadline, while
// worker renewal keeps the default (req-agent-skills AC-2.2;
// req-delegated-execution AC-1.1).
func TestMCPRenewHonorsLeaseSeconds(t *testing.T) {
	t.Parallel()
	type fixture struct {
		server   *Server
		store    store.Store
		orderID  string
		deadline time.Time
	}
	setup := func(t *testing.T, claimant, workerID string, executionTimeout time.Duration) fixture {
		t.Helper()
		ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
		st := store.NewMemory()
		taskID, orderID := "lease-task", "lease-task-implement-1"
		if err := st.CreateTask(ctx, core.Task{ID: taskID, Workspace: "demo", Repo: "conveyor", State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
		if err := st.CreateJob(ctx, core.Job{ID: orderID, TaskID: taskID, Stage: core.StageImplement, State: core.JobPending}); err != nil {
			t.Fatal(err)
		}
		if err := storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: orderID, TaskID: taskID, JobID: orderID, Stage: core.StageImplement, State: core.WorkOrderQueued}); err != nil {
			t.Fatal(err)
		}
		claimed, err := storetest.For(st).ClaimWorkOrder(ctx, orderID, core.WorkOrderClaim{SessionID: "session-a", ClientToken: "secret-a", ClaimantID: claimant, Agent: "codex", Model: "model", Lease: time.Minute, ExecutionTimeout: executionTimeout})
		if err != nil {
			t.Fatal(err)
		}
		server := NewServer(st)
		server.Workspace = "demo"
		server.WorkOrders = &workorder.Service{Store: st}
		membership := &membershipFixture{
			workspaces: []core.Workspace{{ID: "demo"}},
			roles:      map[string]map[string]core.WorkspaceRole{"owner-a": {"demo": core.WorkspaceRoleExecutor}},
		}
		server.Workspaces, server.Memberships = membership, membership
		return fixture{server: server, store: st, orderID: orderID, deadline: claimed.ExecutionDeadline}
	}
	requestFor := func(credential core.AuthenticatedCredential) *http.Request {
		request := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		return request.WithContext(withCredentialActor(request.Context(), credential))
	}
	user := core.AuthenticatedCredential{ID: "pat_owner", OwnerUserID: "owner-a", Kind: core.CredentialUser, Scope: core.CredentialScopeUser}
	renew := func(t *testing.T, f fixture, request *http.Request, leaseSeconds any) (core.WorkOrder, time.Time, time.Time) {
		t.Helper()
		args := map[string]any{"workspace_id": "demo", "work_order_id": f.orderID, "session_id": "session-a"}
		if leaseSeconds != nil {
			args["lease_seconds"] = leaseSeconds
		}
		before := time.Now()
		result, err := f.server.callMCPTool(request, "renew_work_order", args)
		after := time.Now()
		if err != nil {
			t.Fatalf("renew lease_seconds=%v: %v", leaseSeconds, err)
		}
		order, ok := result.(workorder.OrderDelivery)
		if !ok || order.State != core.WorkOrderClaimed || order.SessionID != "session-a" {
			t.Fatalf("renewed=%+v", result)
		}
		if !order.ExecutionDeadline.Equal(f.deadline) {
			t.Fatalf("execution deadline moved from %s to %s", f.deadline, order.ExecutionDeadline)
		}
		return order.WorkOrder, before, after
	}
	assertLease := func(t *testing.T, order core.WorkOrder, before, after time.Time, want time.Duration) {
		t.Helper()
		if order.LeaseExpiresAt.Before(before.Add(want)) || order.LeaseExpiresAt.After(after.Add(want)) {
			t.Fatalf("lease_expires_at=%s, want %s after renewal in [%s, %s]", order.LeaseExpiresAt, want, before, after)
		}
	}

	for _, test := range []struct {
		name         string
		leaseSeconds any
		want         time.Duration
	}{
		{name: "custom 1800", leaseSeconds: float64(1800), want: 30 * time.Minute},
		{name: "omitted", want: core.DefaultWorkOrderClaimLease},
		{name: "zero", leaseSeconds: float64(0), want: core.DefaultWorkOrderClaimLease},
		{name: "negative", leaseSeconds: float64(-60), want: core.DefaultWorkOrderClaimLease},
		{name: "over 3600", leaseSeconds: float64(3601), want: core.DefaultWorkOrderClaimLease},
		{name: "lower bound 1", leaseSeconds: float64(1), want: time.Second},
		{name: "upper bound 3600", leaseSeconds: float64(3600), want: time.Hour},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := setup(t, core.TaskRunClaimantID("owner-a"), "", 4*time.Hour)
			order, before, after := renew(t, f, requestFor(user), test.leaseSeconds)
			assertLease(t, order, before, after, test.want)
		})
	}

	t.Run("custom lease caps at execution deadline", func(t *testing.T) {
		t.Parallel()
		f := setup(t, core.TaskRunClaimantID("owner-a"), "", 10*time.Minute)
		order, _, _ := renew(t, f, requestFor(user), float64(1800))
		if !order.LeaseExpiresAt.Equal(f.deadline) {
			t.Fatalf("lease_expires_at=%s, want execution deadline %s", order.LeaseExpiresAt, f.deadline)
		}
	})

	t.Run("another token of the claiming person renews", func(t *testing.T) {
		t.Parallel()
		f := setup(t, core.TaskRunClaimantID("owner-a"), "", 4*time.Hour)
		second := core.AuthenticatedCredential{ID: "owner-pat-second", OwnerUserID: "owner-a", Kind: core.CredentialUser, Scope: core.CredentialScopeUser}
		args := map[string]any{"workspace_id": "demo", "work_order_id": f.orderID, "session_id": "session-a", "lease_seconds": float64(1800)}
		if _, err := f.server.callMCPTool(requestFor(second), "renew_work_order", args); err != nil {
			t.Fatalf("person's second token renew error=%v", err)
		}
	})

	t.Run("another person is refused the claim", func(t *testing.T) {
		t.Parallel()
		f := setup(t, core.TaskRunClaimantID("owner-a"), "", 4*time.Hour)
		before, err := f.server.Store.GetWorkOrder(store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo"), f.orderID)
		if err != nil {
			t.Fatal(err)
		}
		other := core.AuthenticatedCredential{ID: "other-pat", OwnerUserID: "owner-b", Kind: core.CredentialUser, Scope: core.CredentialScopeUser}
		args := map[string]any{"workspace_id": "demo", "work_order_id": f.orderID, "session_id": "session-a", "lease_seconds": float64(1800)}
		if _, err := f.server.callMCPTool(requestFor(other), "renew_work_order", args); err == nil {
			t.Fatal("another person's renewal was accepted")
		}
		after, err := f.server.Store.GetWorkOrder(store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo"), f.orderID)
		if err != nil {
			t.Fatal(err)
		}
		if !after.LeaseExpiresAt.Equal(before.LeaseExpiresAt) {
			t.Fatalf("refused renewal moved the lease %s -> %s", before.LeaseExpiresAt, after.LeaseExpiresAt)
		}
	})

	t.Run("foreign session refused", func(t *testing.T) {
		t.Parallel()
		f := setup(t, core.TaskRunClaimantID("owner-a"), "", 4*time.Hour)
		args := map[string]any{"workspace_id": "demo", "work_order_id": f.orderID, "session_id": "session-b", "lease_seconds": float64(1800)}
		if _, err := f.server.callMCPTool(requestFor(user), "renew_work_order", args); !errors.Is(err, store.ErrWorkOrderClaimLost) {
			t.Fatalf("foreign-session renew error=%v", err)
		}
		order, err := f.store.GetWorkOrder(store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo"), f.orderID)
		if err != nil || order.SessionID != "session-a" || order.LeaseExpiresAt.After(time.Now().Add(time.Minute)) {
			t.Fatalf("foreign-session renewal changed order=%+v err=%v", order, err)
		}
	})

}

func TestMCPRenewToolPublishesOptionalLeaseSeconds(t *testing.T) {
	t.Parallel()
	for _, tool := range mcpTools() {
		if tool["name"] != "renew_work_order" {
			continue
		}
		schema := tool["inputSchema"].(map[string]any)
		properties := schema["properties"].(map[string]any)
		if lease, ok := properties["lease_seconds"].(map[string]string); !ok || lease["type"] != "integer" || len(lease) != 1 {
			t.Fatalf("renew_work_order lease_seconds schema=%#v", properties["lease_seconds"])
		}
		for _, field := range schema["required"].([]string) {
			if field == "lease_seconds" {
				t.Fatal("renew_work_order requires lease_seconds")
			}
		}
		description, _ := tool["description"].(string)
		for _, requiredText := range []string{"lease_seconds", "1-3600", "five-minute default", "execution deadline"} {
			if !strings.Contains(description, requiredText) {
				t.Fatalf("renew_work_order description lacks %q: %s", requiredText, description)
			}
		}
		return
	}
	t.Fatal("renew_work_order is not registered")
}

func TestMCPUserRunExecutorClaimGovernance(t *testing.T) {
	t.Parallel()
	setup := func(t *testing.T, ownLease time.Duration) (*Server, *http.Request, *membershipFixture, string, string) {
		t.Helper()
		ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
		st := store.NewMemory()
		var orderIDs []string
		for _, suffix := range []string{"a", "b"} {
			taskID, orderID := "user-run-task-"+suffix, "user-run-order-"+suffix
			if err := st.CreateTask(ctx, core.Task{ID: taskID, Workspace: "demo", Repo: "conveyor", State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: time.Now()}); err != nil {
				t.Fatal(err)
			}
			plan, err := st.CreateSpecVersion(ctx, core.SpecVersion{TaskID: taskID, Content: "approved plan"})
			if err != nil {
				t.Fatal(err)
			}
			if err = st.ApproveSpecVersion(ctx, taskID, plan.Version); err != nil {
				t.Fatal(err)
			}
			if err = st.CreateJob(ctx, core.Job{ID: orderID, TaskID: taskID, Stage: core.StageImplement, State: core.JobPending}); err != nil {
				t.Fatal(err)
			}
			if err = storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: orderID, TaskID: taskID, JobID: orderID, Stage: core.StageImplement, State: core.WorkOrderQueued}); err != nil {
				t.Fatal(err)
			}
			lease := time.Minute
			if suffix == "a" {
				lease = ownLease
			}
			if suffix == "b" || ownLease <= time.Nanosecond {
				if _, err = storetest.For(st).ClaimWorkOrder(ctx, orderID, core.WorkOrderClaim{SessionID: "run-session-" + suffix, ClientToken: "run-secret-" + suffix, ClaimantID: core.TaskRunClaimantID("user-" + suffix), OwnerUserID: "user-" + suffix, Agent: "codex", Model: "model", Lease: lease, ExecutionTimeout: time.Hour}); err != nil {
					t.Fatal(err)
				}
			}
			orderIDs = append(orderIDs, orderID)
		}
		_, _, err := st.CreateSystemDesign(ctx, core.SystemDesign{ID: "design-run-claim", Title: "Run", Category: "Architecture"}, core.SystemDesignVersion{Content: "# Run\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/httpapi/**\n```", Origin: core.SystemDesignOriginOperator})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err = st.CreateRequirement(ctx, core.Requirement{ID: "req-run-claim", Title: "Run claim"}, core.RequirementVersion{Content: "# Run claim\n\n```conveyor:requirements\n- id: REQ-1\n  statement: Keep run claims exact.\n```", Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Keep run claims exact."}}, Origin: core.RequirementOriginOperator}); err != nil {
			t.Fatal(err)
		}
		server := NewServer(st)
		server.Workspace = "demo"
		provider := func(context.Context) (*config.Config, error) {
			return &config.Config{Workspace: "demo", Routing: config.Routing{Stages: map[string]config.StageRoute{"implement": {Timeout: time.Hour}}}}, nil
		}
		server.WorkOrders = &workorder.Service{Store: st, ConfigProvider: provider}
		membership := &membershipFixture{
			workspaces: []core.Workspace{{ID: "demo"}},
			roles: map[string]map[string]core.WorkspaceRole{
				"user-a": {"demo": core.WorkspaceRoleExecutor},
				"user-b": {"demo": core.WorkspaceRoleExecutor},
			},
		}
		server.Workspaces, server.Memberships = membership, membership
		request := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		request = request.WithContext(withCredentialActor(request.Context(), core.AuthenticatedCredential{ID: "user-token", OwnerUserID: "user-a", Kind: core.CredentialUser, Scope: core.CredentialScopeUser}))
		if ownLease > time.Nanosecond {
			_, err = server.callMCPTool(request, "claim_work_order", map[string]any{
				"workspace_id": "demo", "work_order_id": orderIDs[0], "session_id": "run-session-a", "client_token": "run-secret-a",
				"claimant_id": core.TaskRunClaimantID("user-b"), "agent": "codex", "model": "model", "lease_seconds": 60,
			})
			if err != nil {
				t.Fatal(err)
			}
			claimed, getErr := st.GetWorkOrder(ctx, orderIDs[0])
			if getErr != nil || claimed.ClaimantID != core.TaskRunClaimantID("user-a") {
				t.Fatalf("MCP governance claim=%+v err=%v", claimed, getErr)
			}
		}
		membership.capabilityCalls = nil
		if ownLease <= time.Nanosecond {
			time.Sleep(time.Millisecond)
		}
		return server, request, membership, orderIDs[0], orderIDs[1]
	}
	argsFor := func(orderID, sessionID string) map[string]any {
		return map[string]any{"workspace_id": "demo", "work_order_id": orderID, "session_id": sessionID, "rationale": "plan changed"}
	}
	governanceTools := []string{"request_plan_revision"}
	for _, test := range []struct {
		name            string
		lease           time.Duration
		otherOrder      bool
		otherCredential bool
		wantErr         bool
	}{
		{name: "own live claim", lease: time.Minute},
		{name: "different order", lease: time.Minute, otherOrder: true, wantErr: true},
		{name: "different credential", lease: time.Minute, otherCredential: true, wantErr: true},
		{name: "expired claim", lease: time.Nanosecond, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, tool := range governanceTools {
				t.Run(tool, func(t *testing.T) {
					server, request, membership, ownOrder, otherOrder := setup(t, test.lease)
					if test.otherCredential {
						request = request.WithContext(withCredentialActor(request.Context(), core.AuthenticatedCredential{ID: "other-token", OwnerUserID: "user-b", Kind: core.CredentialUser, Scope: core.CredentialScopeUser}))
					}
					target := ownOrder
					if test.otherOrder {
						target = otherOrder
					}
					_, err := server.callMCPTool(request, tool, argsFor(target, "run-session-a"))
					wantErr := store.ErrWorkOrderClaimUnauthorized
					if test.lease <= time.Nanosecond {
						wantErr = store.ErrWorkOrderClaimLost
					}
					if test.wantErr && !errors.Is(err, wantErr) {
						t.Fatalf("claim authorization error=%v", err)
					}
					if !test.wantErr && err != nil {
						t.Fatal(err)
					}
					if len(membership.capabilityCalls) != 1 || membership.capabilityCalls[0] != core.CapabilityClaimWork {
						t.Fatalf("membership capabilities=%v", membership.capabilityCalls)
					}
				})
			}
		})
	}
}

func TestMCPClaimedPlanSubmissionStillUsesSpecLifecycle(t *testing.T) {
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	st := store.NewMemory()
	task := core.Task{ID: "mcp-plan-submit", Workspace: "demo", Repo: "conveyor", PolicyVersion: 1, SpecApproval: true, State: core.TaskRunning, NextStage: core.StageSpec, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: task.ID + "-spec-1", TaskID: task.ID, Stage: core.StageSpec, State: core.JobPending}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageSpec, State: core.WorkOrderQueued, QueueEnteredAt: time.Now(), QueueDeadline: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	provider := func(context.Context) (*config.Config, error) {
		return &config.Config{Workspace: "demo", Routing: config.Routing{Stages: map[string]config.StageRoute{"spec": {Timeout: time.Hour}}}}, nil
	}
	server := NewServer(st)
	server.Workspace = "demo"
	server.WorkOrders = &workorder.Service{Store: st, Dispatcher: &dispatch.Dispatcher{Store: st}, ConfigProvider: provider}
	request := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	request = request.WithContext(withCredentialActor(request.Context(), core.AuthenticatedCredential{ID: "pat-token", OwnerUserID: "usr-planner", Kind: core.CredentialUser, Scope: core.CredentialScopeUser}))
	claimArgs := map[string]any{
		"workspace_id": "demo", "work_order_id": job.ID, "session_id": "plan-session", "client_token": "secret",
		"claimant_id": "run:spoofed", "agent": "codex", "model": "gpt",
	}
	if _, err := server.callMCPTool(request, "claim_work_order", claimArgs); err != nil {
		t.Fatal(err)
	}
	markdown := "## Approach\nReuse it.\n\n## Files touched\n- internal/httpapi/mcp.go\n\n## Ordering\n1. Submit.\n\n## Risks\n- Drift.\n\n## Done criteria\n- The plan is gated."
	if _, err := server.callMCPTool(request, "submit_plan", map[string]any{
		"workspace_id": "demo", "work_order_id": job.ID, "session_id": "plan-session", "markdown": markdown, "decomposition": []any{},
	}); err != nil {
		t.Fatal(err)
	}
	order, err := st.GetWorkOrder(ctx, job.ID)
	if err != nil || order.State != core.WorkOrderCompleted || order.ClaimantID != core.TaskRunClaimantID("usr-planner") {
		t.Fatalf("submitted MCP plan order=%+v err=%v", order, err)
	}
}

func TestMCPSubmitSpecNameIsRetiredWithPlanRedirect(t *testing.T) {
	st := store.NewMemory()
	server := NewServer(st)
	server.Workspace = "demo"
	server.WorkOrders = &workorder.Service{Store: st}
	_, err := server.callMCPTool(httptest.NewRequest(http.MethodPost, "/mcp", nil), "submit_spec", map[string]any{"workspace_id": "demo"})
	if err == nil || !strings.Contains(err.Error(), "not found") || !strings.Contains(err.Error(), "submit_plan") {
		t.Fatalf("retired submit_spec error=%v", err)
	}
}

// TestMCPSubmitPlanReviewSeatsSchema keeps submit_plan's optional planned
// review seat count published as an integer of at least 1 without adding it
// to the required arguments (feature-review-seat-minimum RS-3).
func TestMCPSubmitPlanReviewSeatsSchema(t *testing.T) {
	t.Parallel()
	var tool map[string]any
	for _, candidate := range mcpTools() {
		if candidate["name"] == "submit_plan" {
			tool = candidate
		}
	}
	if tool == nil {
		t.Fatal("submit_plan is not registered")
	}
	schema, _ := tool["inputSchema"].(map[string]any)
	properties, _ := schema["properties"].(map[string]any)
	seats, _ := properties["review_seats"].(map[string]any)
	if seats["type"] != "integer" || seats["minimum"] != 1 {
		t.Fatalf("review_seats schema=%#v", seats)
	}
	if required, _ := schema["required"].([]string); !slices.Equal(required, []string{"work_order_id", "session_id", "markdown", "decomposition"}) {
		t.Fatalf("submit_plan required=%v", required)
	}
	if description, _ := tool["description"].(string); !strings.Contains(description, "review_seats_below_minimum") || !strings.Contains(description, "leave the order claimed") {
		t.Fatalf("submit_plan description=%q", description)
	}
	if _, err := json.Marshal(tool); err != nil {
		t.Fatal(err)
	}
}

// TestMCPPlanStateToolSchemas covers the submit_plan base_commit argument and
// the request_plan_revision description: base_commit is an optional string
// of 40 lowercase hexadecimal characters described as the planner
// checkout's HEAD, and request_plan_revision states both uses
// (component-mcp-protocol invariants 39 and 40).
func TestMCPPlanStateToolSchemas(t *testing.T) {
	t.Parallel()
	tools := map[string]map[string]any{}
	for _, candidate := range mcpTools() {
		name, _ := candidate["name"].(string)
		tools[name] = candidate
	}
	plan := tools["submit_plan"]
	schema, _ := plan["inputSchema"].(map[string]any)
	properties, _ := schema["properties"].(map[string]any)
	base, _ := properties["base_commit"].(map[string]any)
	description, _ := base["description"].(string)
	if base["type"] != "string" || base["pattern"] != "^[0-9a-f]{40}$" || !strings.Contains(description, "HEAD") || !strings.Contains(description, "40 lowercase hexadecimal characters") {
		t.Fatalf("base_commit schema=%#v", base)
	}
	if required, _ := schema["required"].([]string); slices.Contains(required, "base_commit") {
		t.Fatalf("base_commit is required: %v", required)
	}
	if text, _ := plan["description"].(string); !strings.Contains(text, "base_commit is your checkout's HEAD") || !strings.Contains(text, "record no plan version") || !strings.Contains(text, "waits for a person's approval") {
		t.Fatalf("submit_plan description=%q", text)
	}
	revision, _ := tools["request_plan_revision"]["description"].(string)
	for _, want := range []string{"revise the approved execution plan", "no plan required", "asks that the task be planned", "planning needed"} {
		if !strings.Contains(revision, want) {
			t.Fatalf("request_plan_revision description lacks %q: %q", want, revision)
		}
	}
}

// TestMCPSubmitPlanBaseCommit drives submit_plan's base_commit through the
// JSON-RPC envelope: a valid value is recorded on the plan version, and a
// malformed or mistyped value is an in-band tool error that keeps the claim
// and records no version (component-mcp-protocol invariant 40;
// component-work-orders invariant 56).
func TestMCPSubmitPlanBaseCommit(t *testing.T) {
	t.Parallel()
	markdown := "## Approach\nPlan it.\n\n## Files touched\n- internal/httpapi/mcp.go\n\n## Ordering\n1. Submit.\n\n## Risks\n- Drift.\n\n## Done criteria\n- The base commit persists."
	const commit = "0123456789abcdef0123456789abcdef01234567"
	for _, tc := range []struct {
		name  string
		value any
		valid bool
	}{
		{name: "valid", value: commit, valid: true},
		{name: "uppercase", value: strings.ToUpper(commit)},
		{name: "short", value: commit[:12]},
		{name: "number", value: 42},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
			st := store.NewMemory()
			task := core.Task{ID: "mcp-plan-base-" + tc.name, Workspace: "demo", Repo: "conveyor", PlanState: core.PlanNeeded, State: core.TaskRunning, NextStage: core.StageSpec, CreatedAt: time.Now()}
			if err := st.CreateTask(ctx, task); err != nil {
				t.Fatal(err)
			}
			job := core.Job{ID: task.ID + "-spec-1", TaskID: task.ID, Stage: core.StageSpec, State: core.JobPending}
			if err := st.CreateJob(ctx, job); err != nil {
				t.Fatal(err)
			}
			if err := storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageSpec, State: core.WorkOrderQueued, QueueEnteredAt: time.Now(), QueueDeadline: time.Now().Add(time.Hour)}); err != nil {
				t.Fatal(err)
			}
			provider := func(context.Context) (*config.Config, error) {
				return &config.Config{Workspace: "demo", Routing: config.Routing{Stages: map[string]config.StageRoute{"spec": {Timeout: time.Hour}}}}, nil
			}
			server := NewServer(st)
			server.Workspace = "demo"
			server.WorkOrders = &workorder.Service{Store: st, Dispatcher: &dispatch.Dispatcher{Store: st}, ConfigProvider: provider}
			request := httptest.NewRequest(http.MethodPost, "/mcp", nil)
			request = request.WithContext(withCredentialActor(request.Context(), core.AuthenticatedCredential{ID: "pat-token", OwnerUserID: "usr-planner", Kind: core.CredentialUser, Scope: core.CredentialScopeUser}))
			if _, err := server.callMCPTool(request, "claim_work_order", map[string]any{
				"workspace_id": "demo", "work_order_id": job.ID, "session_id": "plan-session", "client_token": "secret", "agent": "codex", "model": "gpt",
			}); err != nil {
				t.Fatal(err)
			}
			payload, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "submit_plan", "arguments": map[string]any{
				"workspace_id": "demo", "work_order_id": job.ID, "session_id": "plan-session", "markdown": markdown, "decomposition": []any{}, "base_commit": tc.value,
			}}})
			if err != nil {
				t.Fatal(err)
			}
			response := httptest.NewRecorder()
			server.handleMCP(response, httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(payload)).WithContext(request.Context()))
			var envelope struct {
				Result struct {
					IsError bool `json:"isError"`
					Content []struct {
						Text string `json:"text"`
					} `json:"content"`
				} `json:"result"`
				Error *rpcError `json:"error"`
			}
			if err = json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
				t.Fatal(err)
			}
			if response.Code != http.StatusOK || envelope.Error != nil || len(envelope.Result.Content) != 1 || envelope.Result.IsError == tc.valid {
				t.Fatalf("submit_plan status=%d body=%s", response.Code, response.Body.String())
			}
			order, err := st.GetWorkOrder(ctx, job.ID)
			if err != nil {
				t.Fatal(err)
			}
			spec, recorded, err := st.GetLatestSpecVersion(ctx, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !tc.valid {
				if order.State != core.WorkOrderClaimed || order.SessionID != "plan-session" || recorded {
					t.Fatalf("refused base_commit: order=%+v version recorded=%v", order, recorded)
				}
				return
			}
			if order.State != core.WorkOrderCompleted || !recorded || spec.BaseCommit == nil || *spec.BaseCommit != commit || spec.Origin != core.PlanOriginPlanOrder || spec.Approved {
				t.Fatalf("accepted plan: order=%+v spec=%+v", order, spec)
			}
			stored, err := st.GetTask(ctx, task.ID)
			if err != nil || stored.PlanState != core.PlanAwaitingApproval {
				t.Fatalf("task plan state=%s err=%v", stored.PlanState, err)
			}
		})
	}
}

// TestMCPSubmitPlanReviewSeats drives submit_plan through the JSON-RPC tool
// envelope: omitted and null counts store none, malformed values and counts
// below the task's minimum are in-band tool errors that leave the order
// claimed and write no plan version, and a corrected retry on the same claim
// stores the accepted count (req-review-gates-evidence AC-2.8;
// feature-review-seat-minimum RS-3).
func TestMCPSubmitPlanReviewSeats(t *testing.T) {
	markdown := "## Approach\nPlan seats.\n\n## Files touched\n- internal/httpapi/mcp.go\n\n## Ordering\n1. Submit.\n\n## Risks\n- Loss.\n\n## Done criteria\n- The count persists."
	setup := func(t *testing.T, id string) (context.Context, store.Store, *Server, *http.Request, string) {
		t.Helper()
		ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
		st := store.NewMemory()
		task := core.Task{ID: id, Workspace: "demo", Repo: "conveyor", PolicyVersion: 1, SpecApproval: true, State: core.TaskRunning, NextStage: core.StageSpec, CreatedAt: time.Now()}
		if err := st.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
		job := core.Job{ID: task.ID + "-spec-1", TaskID: task.ID, Stage: core.StageSpec, State: core.JobPending}
		if err := st.CreateJob(ctx, job); err != nil {
			t.Fatal(err)
		}
		if err := storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageSpec, State: core.WorkOrderQueued, QueueEnteredAt: time.Now(), QueueDeadline: time.Now().Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
		// The task freezes no seat list, so its minimum is the workspace's two seats.
		provider := func(context.Context) (*config.Config, error) {
			return &config.Config{Workspace: "demo", Review: config.ReviewPanel{Seats: make([]config.ReviewSeat, 2)}, Routing: config.Routing{Stages: map[string]config.StageRoute{"spec": {Timeout: time.Hour}}}}, nil
		}
		server := NewServer(st)
		server.Workspace = "demo"
		server.WorkOrders = &workorder.Service{Store: st, Dispatcher: &dispatch.Dispatcher{Store: st}, ConfigProvider: provider}
		request := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		request = request.WithContext(withCredentialActor(request.Context(), core.AuthenticatedCredential{ID: "pat-token", OwnerUserID: "usr-planner", Kind: core.CredentialUser, Scope: core.CredentialScopeUser}))
		if _, err := server.callMCPTool(request, "claim_work_order", map[string]any{
			"workspace_id": "demo", "work_order_id": job.ID, "session_id": "plan-session", "client_token": "secret", "agent": "codex", "model": "gpt",
		}); err != nil {
			t.Fatal(err)
		}
		return ctx, st, server, request, job.ID
	}
	call := func(t *testing.T, server *Server, request *http.Request, orderID string, seats ...any) (string, bool) {
		t.Helper()
		args := map[string]any{"workspace_id": "demo", "work_order_id": orderID, "session_id": "plan-session", "markdown": markdown, "decomposition": []any{}}
		if len(seats) > 0 {
			args["review_seats"] = seats[0]
		}
		payload, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "submit_plan", "arguments": args}})
		if err != nil {
			t.Fatal(err)
		}
		response := httptest.NewRecorder()
		server.handleMCP(response, httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(payload)).WithContext(request.Context()))
		var envelope struct {
			Result struct {
				IsError bool `json:"isError"`
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"result"`
			Error *rpcError `json:"error"`
		}
		if err = json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
			t.Fatal(err)
		}
		if response.Code != http.StatusOK || envelope.Error != nil || len(envelope.Result.Content) != 1 {
			t.Fatalf("submit_plan status=%d body=%s", response.Code, response.Body.String())
		}
		return envelope.Result.Content[0].Text, envelope.Result.IsError
	}
	requireClaimedWithoutPlan := func(t *testing.T, ctx context.Context, st store.Store, orderID, label string) {
		t.Helper()
		order, err := st.GetWorkOrder(ctx, orderID)
		if err != nil || order.State != core.WorkOrderClaimed || order.SessionID != "plan-session" {
			t.Fatalf("%s: order=%+v err=%v", label, order, err)
		}
		if _, ok, err := st.GetLatestSpecVersion(ctx, order.TaskID); err != nil || ok {
			t.Fatalf("%s wrote a plan version ok=%t err=%v", label, ok, err)
		}
		task, err := st.GetTask(ctx, order.TaskID)
		if err != nil || task.State != core.TaskRunning || task.NextStage != core.StageSpec {
			t.Fatalf("%s changed the task=%+v err=%v", label, task, err)
		}
	}
	requireStored := func(t *testing.T, ctx context.Context, st store.Store, text string, isError bool, taskID string, want *int) {
		t.Helper()
		if isError {
			t.Fatalf("submit_plan refused: %s", text)
		}
		var result map[string]any
		if err := json.Unmarshal([]byte(text), &result); err != nil || result["version"] != float64(1) {
			t.Fatalf("submit_plan result=%s err=%v", text, err)
		}
		version, ok, err := st.GetLatestSpecVersion(ctx, taskID)
		if err != nil || !ok || (version.ReviewSeats == nil) != (want == nil) || (want != nil && *version.ReviewSeats != *want) {
			t.Fatalf("plan version=%+v ok=%t err=%v, want review_seats %v", version, ok, err, want)
		}
	}

	for _, tc := range []struct {
		name  string
		seats []any
	}{{name: "omitted"}, {name: "null", seats: []any{nil}}} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, st, server, request, orderID := setup(t, "mcp-plan-seats-"+strings.ReplaceAll(tc.name, " ", "-"))
			text, isError := call(t, server, request, orderID, tc.seats...)
			requireStored(t, ctx, st, text, isError, strings.TrimSuffix(orderID, "-spec-1"), nil)
		})
	}

	ctx, st, server, request, orderID := setup(t, "mcp-plan-seats-refusals")
	for _, tc := range []struct {
		value any
		want  string
	}{
		{value: "3", want: "invalid structured plan"},
		{value: 2.5, want: "invalid structured plan"},
		{value: true, want: "invalid structured plan"},
		{value: map[string]any{"count": 3}, want: "invalid structured plan"},
		{value: 0, want: "review_seats must be at least 1"},
		{value: -1, want: "review_seats must be at least 1"},
		{value: 1, want: "review_seats_below_minimum: plan review_seats 1 is below the task's minimum review seat count 2 (minimum_seat_count=2)"},
	} {
		text, isError := call(t, server, request, orderID, tc.value)
		if !isError || !strings.Contains(text, tc.want) {
			t.Fatalf("review_seats %#v result=%q isError=%t, want %q", tc.value, text, isError, tc.want)
		}
		requireClaimedWithoutPlan(t, ctx, st, orderID, fmt.Sprintf("review_seats %#v", tc.value))
	}
	text, isError := call(t, server, request, orderID, 3)
	requireStored(t, ctx, st, text, isError, "mcp-plan-seats-refusals", planReviewSeatPointer(3))
	if order, err := st.GetWorkOrder(ctx, orderID); err != nil || order.State != core.WorkOrderCompleted {
		t.Fatalf("corrected plan order=%+v err=%v", order, err)
	}
}

func planReviewSeatPointer(value int) *int { return &value }

// A nil Go slice marshals to `"required": null`, which the official MCP
// SDK's validation rejects — taking every tool down with it as a
// "tools fetch failed" at connection time.
// TestMCPCreateTaskToolDescribesItsRules keeps the published description in step
// with DEC-116(5) and component-task-lifecycle invariant 16, and keeps the schema
// title-less with the retired execution fields and the retired hold (DEC-125(3))
// absent.
func TestMCPCreateTaskToolDescribesItsRules(t *testing.T) {
	t.Parallel()
	var tool map[string]any
	for _, candidate := range mcpTools() {
		if candidate["name"] == "create_task" {
			tool = candidate
		}
	}
	if tool == nil {
		t.Fatal("create_task is not registered")
	}
	description, _ := tool["description"].(string)
	for _, phrase := range []string{
		"idempotency_key is required",
		"create_tasks",
		"The credential's owner needs create_tasks in the workspace",
		"records MCP as its source and that person as its creator",
		"enters triage like any other task",
		"grants the session no further capability",
		"takes the workspace default",
		"a supplied boolean applies",
		"invalid_agent_gate_override",
		"DEC-116(5)",
	} {
		if !strings.Contains(description, phrase) {
			t.Fatalf("create_task description lacks %q: %s", phrase, description)
		}
	}
	if strings.Contains(description, "DEC-5)") || strings.Contains(description, "DEC-5 ") {
		t.Fatalf("create_task description cites the retired DEC-5: %s", description)
	}
	schema := tool["inputSchema"].(map[string]any)
	properties := schema["properties"].(map[string]any)
	if strings.Contains(strings.ToLower(description), "hold") {
		t.Fatalf("create_task description still mentions hold: %s", description)
	}
	for _, field := range []string{"hold", "title", "mode", "setup", "setup_contract", "policy_contract", "execution_settings", "routing", "harness", "model", "effort", "argv", "verify_stage"} {
		if _, present := properties[field]; present {
			t.Fatalf("create_task publishes %s", field)
		}
	}
	for _, gate := range []string{"spec_approval", "merge_approval"} {
		if spec, _ := properties[gate].(map[string]string); spec["type"] != "boolean" {
			t.Fatalf("%s schema = %#v", gate, properties[gate])
		}
	}
}

func TestMCPToolSchemasNeverEmitNullRequired(t *testing.T) {
	t.Parallel()
	for _, tool := range mcpTools() {
		data, err := json.Marshal(tool)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), `"required":null`) {
			t.Fatalf("tool %v emits required:null", tool["name"])
		}
		if tool["name"] == "create_task" {
			schema := tool["inputSchema"].(map[string]any)
			properties := schema["properties"].(map[string]any)
			if _, present := properties["title"]; present {
				t.Fatalf("create_task still publishes a title field: %s", data)
			}
			if _, present := properties["mode"]; present {
				t.Fatalf("create_task still publishes the retired execution-mode field: %s", data)
			}
			bodyRequired := false
			for _, field := range schema["required"].([]string) {
				bodyRequired = bodyRequired || field == "body"
			}
			if required := schema["required"].([]string); !slices.Equal(required, []string{"body", "repo", "idempotency_key"}) {
				t.Fatalf("create_task required = %v", required)
			}
			if !bodyRequired {
				t.Fatalf("create_task does not require body: %s", data)
			}
			body, ok := properties["body"].(map[string]any)
			if !ok || body["type"] != "string" {
				t.Fatalf("create_task body is not a string schema: %s", data)
			}
			description, _ := body["description"].(string)
			if !strings.Contains(description, "GitHub-flavored Markdown") || !strings.Contains(description, "headings and lists") {
				t.Fatalf("create_task body does not publish structured GFM guidance: %s", data)
			}
		}
		if tool["name"] == "get_work_order" {
			description, _ := tool["description"].(string)
			for _, requiredText := range []string{"authority_source", "live", "provisional", "pinned", "claim-time"} {
				if !strings.Contains(description, requiredText) {
					t.Fatalf("get_work_order description lacks %q: %s", requiredText, description)
				}
			}
		}
		if tool["name"] == "submit_review_verdict" {
			description, _ := tool["description"].(string)
			for _, requiredText := range []string{"REQ-n", "AC-n.m", "pinned"} {
				if !strings.Contains(description, requiredText) {
					t.Fatalf("submit_review_verdict description lacks %q: %s", requiredText, description)
				}
			}
			schema := tool["inputSchema"].(map[string]any)
			properties := schema["properties"].(map[string]any)
			assessment, ok := properties["requirement_citations"].(map[string]any)
			if !ok || assessment["additionalProperties"] != false {
				t.Fatalf("submit_review_verdict lacks strict requirement_citations: %s", data)
			}
			required := map[string]bool{}
			for _, field := range schema["required"].([]string) {
				required[field] = true
			}
			if !required["requirement_citations"] {
				t.Fatalf("submit_review_verdict does not require requirement_citations: %s", data)
			}
			governance, ok := properties["governance_assessment"].(map[string]any)
			if !ok || governance["additionalProperties"] != false || governance["anyOf"] == nil {
				t.Fatalf("submit_review_verdict lacks strict compatible governance assessment: %s", data)
			}
			governanceProperties, _ := governance["properties"].(map[string]any)
			for _, field := range []string{"applicable", "design_applicable", "decision_citable"} {
				if _, exists := governanceProperties[field]; !exists {
					t.Fatalf("submit_review_verdict governance assessment lacks %s: %s", field, data)
				}
			}
		}
	}
}

// Streamable-HTTP clients probe GET /mcp for an SSE stream after initialize;
// a server without one must answer 405, not fall through to the SPA catch-all
// as 200 HTML — that response wedges the client at "connecting".
func TestMCPNonPostReturnsMethodNotAllowedNotSPA(t *testing.T) {
	t.Parallel()
	server := NewServer(store.NewMemory())
	server.BearerToken = "operator-token"
	handler := server.Handler()
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		request := httptest.NewRequest(method, "/mcp", nil)
		request.Header.Set("Authorization", "Bearer operator-token")
		request.Header.Set("Accept", "text/event-stream")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s /mcp status = %d body=%s", method, response.Code, response.Body.String())
		}
		if allow := response.Header().Get("Allow"); allow != "POST" {
			t.Fatalf("%s /mcp Allow = %q", method, allow)
		}
	}
}

func TestMCPCreateTaskEnqueuesTriageIdempotently(t *testing.T) {
	t.Parallel()
	st := store.NewMemory()
	server := NewServer(st)
	server.BearerToken = "operator-token"
	server.Workspace = "demo"
	server.Repos = []string{"api"}
	enqueued := 0
	generated := 0
	server.GenerateTaskTitle = func(context.Context, core.Task) (string, error) {
		generated++
		return "Triage this issue", nil
	}
	server.OnCreate = func(context.Context, string) { enqueued++ }
	handler := server.Handler()

	call := func(taskBody string) (core.Task, bool, bool) {
		t.Helper()
		payload, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "create_task", "arguments": map[string]any{"body": taskBody, "repo": "api", "source": "mcp:test-issue", "spec_approval": true, "merge_approval": true, "idempotency_key": "issue-42"}}})
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(string(payload)))
		request.Header.Set("Authorization", "Bearer operator-token")
		request.Header.Set("X-Conveyor-Actor", "issue-triage-agent")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
		}
		var envelope struct {
			Result struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
				IsError bool `json:"isError"`
			} `json:"result"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
			t.Fatal(err)
		}
		if envelope.Result.IsError {
			return core.Task{}, false, true
		}
		var result struct {
			Task    core.Task `json:"task"`
			Created bool      `json:"created"`
		}
		if len(envelope.Result.Content) != 1 {
			t.Fatalf("content = %+v", envelope.Result.Content)
		}
		if err := json.Unmarshal([]byte(envelope.Result.Content[0].Text), &result); err != nil {
			t.Fatal(err)
		}
		return result.Task, result.Created, false
	}

	first, created, failed := call("from an MCP issue")
	if failed || !created || first.Title != "Triage this issue" || first.State != core.TaskQueued || first.NextStage != core.StageTriage {
		t.Fatalf("first task=%+v created=%t failed=%t", first, created, failed)
	}
	second, created, failed := call("from an MCP issue")
	if failed || created || second.ID != first.ID || enqueued != 1 || generated != 1 {
		t.Fatalf("retry task=%+v created=%t failed=%t enqueued=%d generated=%d", second, created, failed, enqueued, generated)
	}
	if _, _, failed = call("Different issue"); !failed {
		t.Fatal("reusing the idempotency key for different input succeeded")
	}
	tasks, err := st.ListTasks(store.WithActor(t.Context(), store.SystemActor()))
	if err != nil || len(tasks) != 1 {
		t.Fatalf("tasks=%+v err=%v", tasks, err)
	}
	events, err := st.ListEvents(store.WithActor(t.Context(), store.SystemActor()), first.ID)
	if err != nil || len(events) != 2 || events[0].ActorID != "user:local-operator" || events[0].ActorRole != core.ActorUser || events[1].Kind != core.EventTaskPlanStateChanged {
		t.Fatalf("events=%+v err=%v", events, err)
	}
}

// TestMCPCreateTaskRefusesRetiredHold covers the compatibility input for the
// retired hold (DEC-125(3); component-mcp-protocol): a new true and a mistyped
// value are refused in-band with no side effect, omitted, null, and false are
// accepted, and a retry whose key names a task stored with hold before the
// retirement returns that original task.
func TestMCPCreateTaskRefusesRetiredHold(t *testing.T) {
	st := store.NewMemory()
	server := NewServer(st)
	server.Workspace, server.Repos = "demo", []string{"api"}
	generated := 0
	server.GenerateTaskTitle = func(context.Context, core.Task) (string, error) {
		generated++
		return "Retired hold", nil
	}
	enqueued := 0
	server.OnCreate = func(context.Context, string) { enqueued++ }
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	request := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	request = request.WithContext(store.WithActor(request.Context(), store.SystemActor()))
	arguments := func(key string, extra map[string]any) map[string]any {
		values := map[string]any{"workspace_id": "demo", "body": "Retire the hold", "repo": "api", "idempotency_key": key}
		maps.Copy(values, extra)
		return values
	}
	countTasks := func() int {
		tasks, err := st.ListTasks(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return len(tasks)
	}
	for name, value := range map[string]any{"true": true, "string": "yes", "number": 1} {
		if _, err := server.callMCPTool(request, "create_task", arguments("refused-"+name, map[string]any{"hold": value})); err == nil || !strings.Contains(err.Error(), "task_hold_retired") || !strings.Contains(err.Error(), "DEC-125") {
			t.Fatalf("%s hold error=%v", name, err)
		}
	}
	if countTasks() != 0 || generated != 0 || enqueued != 0 {
		t.Fatalf("refused hold created tasks=%d titles=%d enqueued=%d", countTasks(), generated, enqueued)
	}
	for name, extra := range map[string]map[string]any{"omitted": nil, "null": {"hold": nil}, "false": {"hold": false}} {
		result, err := server.callMCPTool(request, "create_task", arguments("accepted-"+name, extra))
		if err != nil {
			t.Fatalf("%s hold: %v", name, err)
		}
		task := result.(map[string]any)["task"].(core.Task)
		if wire, _ := json.Marshal(task); task.Hold || bytes.Contains(wire, []byte(`"hold"`)) {
			t.Fatalf("%s hold task=%s", name, wire)
		}
	}

	original, err := server.callMCPTool(request, "create_task", arguments("historical-held", map[string]any{"level": "L3"}))
	if err != nil {
		t.Fatal(err)
	}
	first := original.(map[string]any)["task"].(core.Task)
	if !first.SpecApproval || !first.MergeApproval || first.Hold {
		t.Fatalf("L3 task=%+v, want both gates on and no reservation", first)
	}
	titles, notified := generated, enqueued
	// The keyed task now reads as stored with hold before the retirement. A
	// retry repeating the original L3 and hold: true returns it unchanged.
	server.Store = historicalHoldLookup{Store: st}
	replayed, err := server.callMCPTool(request, "create_task", arguments("historical-held", map[string]any{"level": "L3", "hold": true}))
	if err != nil {
		t.Fatalf("historical held retry: %v", err)
	}
	replay := replayed.(map[string]any)
	if task := replay["task"].(core.Task); task.ID != first.ID || replay["created"] != false {
		t.Fatalf("replay=%+v first=%s", replay, first.ID)
	}
	if created, err := st.CountEvents(ctx, first.ID, "task.created"); err != nil || created != 1 || generated != titles || enqueued != notified {
		t.Fatalf("task.created=%d titles=%d enqueued=%d err=%v", created, generated, enqueued, err)
	}
}

func TestMCPDependencyValidationPrecedesTitleAndIdempotencyIsSymmetric(t *testing.T) {
	st := store.NewMemory()
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	dependency := core.Task{ID: "dependency-api", Workspace: "demo", Repo: "api", State: core.TaskRunning, CreatedAt: time.Now().UTC()}
	if err := st.CreateTask(ctx, dependency); err != nil {
		t.Fatal(err)
	}
	server := NewServer(st)
	server.Workspace = "demo"
	server.Repos = []string{"api", "ui"}
	generated := 0
	server.GenerateTaskTitle = func(context.Context, core.Task) (string, error) {
		generated++
		return "Cross-repository dependent", nil
	}
	request := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	request = request.WithContext(store.WithActor(request.Context(), store.SystemActor()))
	base := map[string]any{
		"workspace_id": "demo", "body": "ship the UI after API", "repo": "ui",
		"depends_on": []any{dependency.ID}, "idempotency_key": "dependency-intake",
	}
	result, err := server.callMCPTool(request, "create_task", base)
	if err != nil {
		t.Fatalf("cross-repository dependency: %v", err)
	}
	created := result.(map[string]any)["task"].(core.Task)
	if len(created.Dependencies) != 0 {
		t.Fatalf("create response unexpectedly hydrated relations: %+v", created.Dependencies)
	}
	persisted, err := st.GetTask(ctx, created.ID)
	if err != nil || len(persisted.Dependencies) != 1 || persisted.Dependencies[0].ID != dependency.ID {
		t.Fatalf("persisted dependencies=%+v err=%v", persisted.Dependencies, err)
	}
	withoutDependency := maps.Clone(base)
	delete(withoutDependency, "depends_on")
	if _, err = server.callMCPTool(request, "create_task", withoutDependency); err == nil || !strings.Contains(err.Error(), "different task") {
		t.Fatalf("removed dependency idempotency error=%v", err)
	}
	if generated != 1 {
		t.Fatalf("idempotency conflict regenerated title %d times", generated)
	}
	invalid := maps.Clone(base)
	invalid["idempotency_key"] = "missing-dependency"
	invalid["depends_on"] = []any{"missing"}
	if _, err = server.callMCPTool(request, "create_task", invalid); err == nil || !strings.Contains(err.Error(), "invalid depends_on") {
		t.Fatalf("invalid dependency error=%v", err)
	}
	if generated != 1 {
		t.Fatalf("invalid dependency called title generation; count=%d", generated)
	}
}

func TestMCPCreateTaskRetryUsesPersistedPolicyBeforeLiveHealth(t *testing.T) {
	st := store.NewMemory()
	now := time.Now().UTC()
	cfg := &config.Config{
		Workspace: "demo",
		Execution: config.ExecutionPolicy{DefaultMode: "auto", DefaultPlanState: "needed", MergeApproval: true},

		Routing: config.Routing{Stages: map[string]config.StageRoute{
			"implement": {},
			"review":    {Execution: config.ExecutionMCP},
		}},
		Repos: []config.Repo{{Name: "api", Base: "main"}},
	}
	provider := func(context.Context) (*config.Config, error) { return cfg, nil }
	server := NewServer(st)
	server.Workspace, server.ConfigProvider = "demo", provider
	server.GenerateTaskTitle = func(context.Context, core.Task) (string, error) { return "Stable retry", nil }
	request := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	request = request.WithContext(store.WithActor(request.Context(), store.SystemActor()))
	arguments := map[string]any{"workspace_id": "demo", "body": "Keep retry policy stable", "repo": "api", "idempotency_key": "stable-policy"}
	firstResult, err := server.callMCPTool(request, "create_task", arguments)
	if err != nil {
		t.Fatal(err)
	}
	first := firstResult.(map[string]any)["task"].(core.Task)
	if wire, err := json.Marshal(first); err != nil || first.Mode != "" || bytes.Contains(wire, []byte(`"hold"`)) || !first.SpecApproval || !first.MergeApproval {
		t.Fatalf("first=%s err=%v", wire, err)
	}

	// Both live availability and omitted workspace gate defaults change after
	// intake. An exact retry must still return the persisted resolution.
	now = now.Add(time.Minute)
	cfg.Execution.DefaultPlanState = "not_required"
	cfg.Execution.MergeApproval = false
	secondResult, err := server.callMCPTool(request, "create_task", arguments)
	if err != nil {
		t.Fatalf("exact retry after health/default change: %v", err)
	}
	second := secondResult.(map[string]any)["task"].(core.Task)
	if second.ID != first.ID || !second.SpecApproval || !second.MergeApproval {
		t.Fatalf("second=%+v first=%+v", second, first)
	}

	conflict := maps.Clone(arguments)
	conflict["spec_approval"] = false
	if _, err = server.callMCPTool(request, "create_task", conflict); err == nil || !strings.Contains(err.Error(), "different task") {
		t.Fatalf("explicit conflicting gate error=%v", err)
	}

	withTitle := maps.Clone(arguments)
	withTitle["title"] = "Caller title"
	if _, err = server.callMCPTool(request, "create_task", withTitle); err == nil || !strings.Contains(err.Error(), "must not be supplied") {
		t.Fatalf("title input error=%v", err)
	}
}

func TestResolveMCPWorkspaceFallbackFailsClosed(t *testing.T) {
	t.Parallel()
	server := NewServer(store.NewMemory())
	if _, err := server.resolveMCPWorkspace(store.WithActor(t.Context(), store.SystemActor()), ""); err == nil || !strings.Contains(err.Error(), "workspace_unavailable") {
		t.Fatalf("zero-workspace omission error = %v", err)
	}
	if _, err := server.resolveMCPWorkspace(store.WithActor(t.Context(), store.SystemActor()), "unknown"); err == nil || !strings.Contains(err.Error(), "workspace_not_found") {
		t.Fatalf("zero-workspace explicit error = %v", err)
	}

	server.Workspace = "alpha"
	if got, err := server.resolveMCPWorkspace(store.WithActor(t.Context(), store.SystemActor()), ""); err != nil || got != "alpha" {
		t.Fatalf("singleton omission = %q, %v", got, err)
	}
	if got, err := server.resolveMCPWorkspace(store.WithActor(t.Context(), store.SystemActor()), "alpha"); err != nil || got != "alpha" {
		t.Fatalf("singleton explicit = %q, %v", got, err)
	}
	if _, err := server.resolveMCPWorkspace(store.WithActor(t.Context(), store.SystemActor()), "beta"); err == nil || !strings.Contains(err.Error(), "workspace_not_found") {
		t.Fatalf("unknown singleton explicit error = %v", err)
	}

	server.Deployment = &config.Config{Workspace: "beta"}
	if _, err := server.resolveMCPWorkspace(store.WithActor(t.Context(), store.SystemActor()), ""); err == nil || !strings.Contains(err.Error(), "workspace_required") {
		t.Fatalf("ambiguous omission error = %v", err)
	}
}

func TestMCPApprovalCoverageGuardForUser(t *testing.T) {
	for _, transport := range []string{"user"} {
		t.Run(transport, func(t *testing.T) {
			ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
			st := store.NewMemory()
			task := core.Task{ID: "coverage-" + transport, Workspace: "demo", Repo: "conveyor", State: core.TaskRunning, NextStage: core.StageReview, PolicyVersion: 1, MergeApproval: true, CreatedAt: time.Now()}
			if err := st.CreateTask(ctx, task); err != nil {
				t.Fatal(err)
			}
			spec, err := st.CreateSpecVersion(ctx, core.SpecVersion{TaskID: task.ID, Content: storetest.ReviewDoneCriteriaPlan})
			if err != nil {
				t.Fatal(err)
			}
			if err = st.ApproveSpecVersion(ctx, task.ID, spec.Version); err != nil {
				t.Fatal(err)
			}
			job := core.Job{ID: task.ID + "-review-1", TaskID: task.ID, Stage: core.StageReview, State: core.JobRunning}
			if err = st.CreateJob(ctx, job); err != nil {
				t.Fatal(err)
			}
			if err = storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageReview, ReviewRound: 1, ReviewSeat: 1, ServedRequirementSnapshot: []core.ServedRequirementContext{}, GovernanceSnapshot: &core.GovernanceSnapshot{}}); err != nil {
				t.Fatal(err)
			}
			claim := core.WorkOrderClaim{SessionID: "coverage-session", ClientToken: "token", ClaimantID: core.TaskRunClaimantID("owner"), Lease: time.Minute}
			if _, err = storetest.For(st).ClaimWorkOrder(ctx, job.ID, claim); err != nil {
				t.Fatal(err)
			}
			cfg := &config.Config{Workspace: "demo", MaxBounces: 2, Repos: []config.Repo{{Name: "conveyor"}}}
			d := dispatch.New(st, cfg, nil)
			d.DisableMemoryQueueForTest()
			server := NewServer(st)
			server.Workspace = "demo"
			server.WorkOrders = &workorder.Service{Store: st, Dispatcher: d, ConfigProvider: func(context.Context) (*config.Config, error) { return cfg, nil }}
			request := httptest.NewRequest(http.MethodPost, "/mcp", nil)
			credential := core.AuthenticatedCredential{ID: "owner", OwnerUserID: "owner", Kind: core.CredentialUser}
			request = request.WithContext(withCredentialActor(request.Context(), credential))
			args := map[string]any{"workspace_id": "demo", "work_order_id": job.ID, "session_id": claim.SessionID, "verdict": "approve", "reason_code": "approved", "summary": "focused checks pass", "feedback": "", "requirement_citations": map[string]any{"applicable": false}, "governance_assessment": map[string]any{"design_applicable": false, "decision_citable": false}, "done_criteria_coverage": map[string]any{"applicable": true, "summary": "mandatory validation unresolved", "unverified": []string{storetest.PR907MandatoryValidation}}}
			before, _ := st.ListEvents(ctx, task.ID)
			unresolved := args["done_criteria_coverage"]
			for _, assessment := range []struct {
				value any
				want  string
			}{
				{nil, "assessment is required"},
				{map[string]any{"summary": "omitted applicability"}, "does not match"},
				{map[string]any{"summary": "false applicability", "applicable": false}, "does not match"},
				{unresolved, "blocks approve: unresolved criteria in unverified"},
			} {
				args["done_criteria_coverage"] = assessment.value
				if _, err = server.callMCPTool(request, "submit_review_verdict", args); err == nil || !strings.Contains(err.Error(), assessment.want) {
					t.Fatalf("error=%v, want %s", err, assessment.want)
				}
			}
			order, _ := st.GetWorkOrder(ctx, job.ID)
			after, _ := st.ListEvents(ctx, task.ID)
			// The claimant's verdict refresh records its context observation
			// before the verdict is validated; the request now carries the
			// credential-derived actor the middleware binds, so that
			// observation is attributed and kept. No lifecycle event may follow.
			lifecycle := func(events []core.Event) []core.Event {
				var kept []core.Event
				for _, event := range events {
					if event.Kind != "context.verdict_refresh_observed" {
						kept = append(kept, event)
					}
				}
				return kept
			}
			if order.State != core.WorkOrderClaimed || string(core.JSONPayload(lifecycle(before))) != string(core.JSONPayload(lifecycle(after))) {
				t.Fatalf("MCP refusal consumed claim or appended events: state=%s events %d->%d", order.State, len(before), len(after))
			}
			args["verdict"], args["reason_code"], args["feedback"] = "changes_requested", "validation", "Complete required validation"
			if _, err = server.callMCPTool(request, "submit_review_verdict", args); err != nil {
				t.Fatalf("corrected verdict: %v", err)
			}
			order, _ = st.GetWorkOrder(ctx, job.ID)
			if order.State != core.WorkOrderCompleted {
				t.Fatalf("corrected verdict order=%+v", order)
			}
		})
	}
}

// withCredentialActor binds a credential and the actor requireMCPAuth derives
// from it. Tests that call callMCPTool or a store directly skip the
// middleware, so they bind the same explicit actor themselves
// (component-persistence, Actor context).
func withCredentialActor(ctx context.Context, credential core.AuthenticatedCredential) context.Context {
	actor := store.Actor{ID: store.UserActorID(credential.OwnerUserID), Role: core.ActorUser}
	if credential.Kind == retiredAgentKind {
		actor = store.Actor{ID: store.AgentActorID(credential.ID), Role: core.ActorAgent}
	}
	return store.WithActor(store.WithCredential(ctx, credential), actor)
}

// REQ-9/AC-9.5: MCP preserves documentation assessment and refuses missing/unresolved.
func TestMCPDocumentationAssessmentAdmission(t *testing.T) {
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	st := store.NewMemory()
	head := strings.Repeat("a", 40)
	task := core.Task{ID: "documentation-mcp", Workspace: "demo", Repo: "conveyor", State: core.TaskRunning, NextStage: core.StageReview, ReviewedHeadSHA: head, CreatedAt: time.Now()}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	evidence := core.DocumentationGateEvidence{TaskID: task.ID, HeadSHA: head, BaseSHA: strings.Repeat("b", 40), PolicyJSON: json.RawMessage(`{"schema_version":1,"docs":[{"path":"docs/api.md","governs":["internal/**"]}],"rule":{"none_statement":"docs: none"}}`), ChangedPaths: []string{"internal/api.go", "docs/api.md"}}
	if err := st.RecordDocumentationGateEvidence(ctx, evidence); err != nil {
		t.Fatal(err)
	}
	job := core.Job{ID: task.ID + "-review-1", TaskID: task.ID, Stage: core.StageReview, State: core.JobRunning}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageReview, ServedRequirementSnapshot: []core.ServedRequirementContext{}, GovernanceSnapshot: &core.GovernanceSnapshot{}}); err != nil {
		t.Fatal(err)
	}
	if _, err := storetest.For(st).ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{SessionID: "docs-review", ClientToken: "secret", ClaimantID: core.TaskRunClaimantID("owner"), Lease: time.Minute}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Workspace: "demo", MaxBounces: 2, Repos: []config.Repo{{Name: "conveyor"}}}
	d := dispatch.New(st, cfg, nil)
	d.DisableMemoryQueueForTest()
	server := NewServer(st)
	server.Workspace = "demo"
	server.WorkOrders = &workorder.Service{Store: st, Dispatcher: d, ConfigProvider: func(context.Context) (*config.Config, error) { return cfg, nil }}
	request := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	request = request.WithContext(withCredentialActor(request.Context(), core.AuthenticatedCredential{ID: "owner", OwnerUserID: "owner", Kind: core.CredentialUser}))
	args := map[string]any{"workspace_id": "demo", "work_order_id": job.ID, "session_id": "docs-review", "verdict": "approve", "reason_code": "verified", "summary": "docs cover behavior", "requirement_citations": map[string]any{"applicable": false}, "done_criteria_coverage": map[string]any{"applicable": false, "summary": "no approved plan"}, "governance_assessment": map[string]any{"design_applicable": false, "decision_citable": false}}
	before, _ := st.ListEvents(ctx, task.ID)
	for _, assessment := range []any{nil, map[string]any{"applicable": true, "summary": "missing API", "updated_paths": []string{"docs/api.md"}, "unresolved": []string{"missing new error"}, "conflicts": []string{}}} {
		args["documentation_assessment"] = assessment
		if _, err := server.callMCPTool(request, "submit_review_verdict", args); err == nil || !strings.Contains(err.Error(), "documentation") {
			t.Fatalf("assessment=%v err=%v", assessment, err)
		}
	}
	after, _ := st.ListEvents(ctx, task.ID)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("MCP refusal wrote events")
	}
	args["documentation_assessment"] = map[string]any{"applicable": true, "summary": "API docs updated", "updated_paths": []string{"docs/api.md"}, "unresolved": []string{}, "conflicts": []string{}}
	if _, err := server.callMCPTool(request, "submit_review_verdict", args); err != nil {
		t.Fatal(err)
	}
	events, _ := st.ListEvents(ctx, task.ID)
	found := false
	for _, event := range events {
		if event.Kind == "review.completed" {
			var payload map[string]json.RawMessage
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(payload["documentation_assessment"]), "API docs updated") {
				t.Fatalf("MCP dropped assessment: %s", event.Payload)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("no review completion")
	}
}

// TestMCPSubmitForReviewReviewSeatsSchema proves submit_for_review publishes
// an optional integer review_seats of at least 1 and names the floor refusal
// (feature-review-seat-minimum RS-4).
func TestMCPSubmitForReviewReviewSeatsSchema(t *testing.T) {
	for _, tool := range mcpTools() {
		if tool["name"] != "submit_for_review" {
			continue
		}
		if description, _ := tool["description"].(string); !strings.Contains(description, "review_seats_below_floor") {
			t.Fatalf("description=%q", description)
		}
		schema := tool["inputSchema"].(map[string]any)
		properties := schema["properties"].(map[string]any)
		if !reflect.DeepEqual(properties["review_seats"], map[string]any{"type": "integer", "minimum": 1}) {
			t.Fatalf("review_seats schema=%#v", properties["review_seats"])
		}
		if !reflect.DeepEqual(schema["required"], []string{"work_order_id", "session_id", "head_sha"}) {
			t.Fatalf("required=%#v", schema["required"])
		}
		for _, field := range []string{"harness", "model", "effort"} {
			if _, present := properties[field]; present {
				t.Fatalf("submit_for_review publishes %s", field)
			}
		}
		if _, err := json.Marshal(tool); err != nil {
			t.Fatal(err)
		}
		return
	}
	t.Fatal("submit_for_review tool missing")
}

// TestMCPSubmitForReviewReviewSeatsArgumentValidation proves the shared
// parser judges the exact JSON literal: it accepts only json.Number literals
// that denote an integer from 1 through the INT bound, including integral
// forms such as 4.0 and 4e0, and refuses fractions hidden by float64 rounding,
// float64 values, and every non-number without truncating, rounding, or
// treating a present value as absent (feature-review-seat-minimum RS-4).
func TestMCPSubmitForReviewReviewSeatsArgumentValidation(t *testing.T) {
	if seats, err := reviewSeatsArg(map[string]any{"head_sha": "abc"}); seats != nil || err != nil {
		t.Fatalf("absent=%v err=%v", seats, err)
	}
	for literal, want := range map[string]int{
		"1": 1, "3": 3, "4.0": 4, "4.000": 4, "4e0": 4, "4E+0": 4, "40e-1": 4, "0.4e1": 4, "0.04E+2": 4,
		"2147483647": 2147483647, "2147483647.0": 2147483647, "2.147483647e9": 2147483647,
	} {
		seats, err := reviewSeatsArg(map[string]any{"review_seats": json.Number(literal)})
		if err != nil || seats == nil || *seats != want {
			t.Fatalf("%s => %v err=%v", literal, seats, err)
		}
	}
	refused := []any{nil, "3", true, float64(3), float64(2.5), map[string]any{}, []any{json.Number("3")}}
	for _, literal := range []string{
		"2.99999999999999999", "0.99999999999999999", "2147483647.00000001", "2.0000000000000001", "3.5", "35e-1",
		"0", "0.0", "-0", "-1", "-4.0", "2147483648", "2147483648.0", "2.147483648e9", "1e10", "1e1001", "1e-1001",
		"00", "01", "+3", "3.", ".5", "0x10", "NaN", "Infinity", "", "3 ", "1e400",
	} {
		refused = append(refused, json.Number(literal))
	}
	for _, value := range refused {
		seats, err := reviewSeatsArg(map[string]any{"review_seats": value})
		if seats != nil || err == nil || err.Error() != "review_seats must be an integer from 1 to 2147483647" {
			t.Fatalf("%#v => %v err=%v", value, seats, err)
		}
	}
}

// TestMCPExactReviewSeatsArgument proves the tools/call re-read keeps
// submit_for_review's raw review_seats literal and leaves other arguments as
// the envelope decoded them (feature-review-seat-minimum RS-4).
func TestMCPExactReviewSeatsArgument(t *testing.T) {
	params := json.RawMessage(`{"name":"submit_for_review","arguments":{"head_sha":"abc","lease_seconds":30,"review_seats":2.99999999999999999}}`)
	var call mcpToolCallEnvelope
	if err := core.DecodeVerificationRequest(params, &call); err != nil {
		t.Fatal(err)
	}
	if call.Arguments["review_seats"] != float64(3) {
		t.Fatalf("envelope decoding changed: %#v", call.Arguments["review_seats"])
	}
	if err := mcpExactReviewSeatsArgument(params, call.Arguments); err != nil {
		t.Fatal(err)
	}
	if call.Arguments["review_seats"] != json.Number("2.99999999999999999") || call.Arguments["lease_seconds"] != float64(30) || call.Arguments["head_sha"] != "abc" {
		t.Fatalf("arguments=%#v", call.Arguments)
	}
	absent := map[string]any{"head_sha": "abc"}
	if err := mcpExactReviewSeatsArgument(json.RawMessage(`{"name":"submit_for_review","arguments":{"head_sha":"abc"}}`), absent); err != nil || len(absent) != 1 {
		t.Fatalf("absent=%#v err=%v", absent, err)
	}
	null := map[string]any{"review_seats": nil}
	if err := mcpExactReviewSeatsArgument(json.RawMessage(`{"name":"submit_for_review","arguments":{"review_seats":null}}`), null); err != nil {
		t.Fatal(err)
	}
	if value, present := null["review_seats"]; !present || value != nil {
		t.Fatalf("null=%#v", null)
	}
}

// The release schema states the decision request's rune limit and asks for
// confirmed citation versions (component-mcp-protocol; req-260820-394cac REQ-1,
// AC-2.6).
func TestMCPReleaseSchemaStatesCheckpointContract(t *testing.T) {
	properties := func(tool map[string]any) map[string]any {
		return tool["inputSchema"].(map[string]any)["properties"].(map[string]any)
	}
	seen := map[string]bool{}
	for _, tool := range mcpTools() {
		name := tool["name"].(string)
		switch name {
		case "release_work_order":
			checkpoint := properties(tool)["checkpoint"].(map[string]any)["properties"].(map[string]any)
			request := checkpoint["decision_request"].(map[string]any)
			description, _ := request["description"].(string)
			if request["type"] != "string" || request["maxLength"] != core.WorkOrderDecisionRequestLimit || core.WorkOrderDecisionRequestLimit != 1024 || !strings.Contains(description, "1024") {
				t.Fatalf("decision_request schema = %+v", request)
			}
			citation := checkpoint["citations"].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)
			version := citation["cited_version"].(map[string]any)
			if version["type"] != "number" || !strings.Contains(version["description"].(string), "confirmed version") {
				t.Fatalf("cited_version schema = %+v", version)
			}
			if data, err := json.Marshal(tool); err != nil || !json.Valid(data) {
				t.Fatalf("release schema does not marshal: %v", err)
			}
			seen[name] = true
		}
	}
	if len(seen) != 1 {
		t.Fatalf("schemas checked = %v", seen)
	}
}

// lockedLogBuffer collects standard-logger output written from any goroutine.
type lockedLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lockedLogBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lockedLogBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// captureStandardLog redirects the standard logger for one test. Tests that
// call it do not run in parallel.
func captureStandardLog(t *testing.T) *lockedLogBuffer {
	t.Helper()
	captured := &lockedLogBuffer{}
	previousWriter, previousFlags := log.Writer(), log.Flags()
	log.SetOutput(captured)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(previousWriter)
		log.SetFlags(previousFlags)
	})
	return captured
}

func decodeMCPToolText(t *testing.T, body []byte) (string, bool) {
	t.Helper()
	var envelope struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || len(envelope.Result.Content) != 1 {
		t.Fatalf("tool result %s: %v", body, err)
	}
	return envelope.Result.Content[0].Text, envelope.Result.IsError
}

func TestMCPToolErrorLogsRequestID(t *testing.T) {
	captured := captureStandardLog(t)
	server := NewServer(store.NewMemory())
	server.BearerToken = "operator-token"
	request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"submit_spec","arguments":{}}}`))
	request.Header.Set("Authorization", "Bearer operator-token")
	request.Header.Set(middleware.RequestIDHeader, "req-mcp-refusal")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	text, isError := decodeMCPToolText(t, response.Body.Bytes())
	if !isError || text == "" || strings.Contains(text, "request_id") {
		t.Fatalf("refusal result isError=%v text=%q, want the unchanged refusal text", isError, text)
	}
	want := fmt.Sprintf("mcp: tool call failed: request_id=%q tool=%q error=%q", "req-mcp-refusal", "submit_spec", text)
	if logged := captured.String(); !strings.Contains(logged, want) {
		t.Fatalf("log %q lacks %q", logged, want)
	}
}

// backendFailingClaimStore fails the claim path's first store read the way a
// durable backend reports an untranslated driver condition.
type backendFailingClaimStore struct {
	store.Store
}

func (backendFailingClaimStore) GetWorkOrder(context.Context, string) (core.WorkOrder, error) {
	return core.WorkOrder{}, fmt.Errorf("SingleStore %w", store.ErrBackendOperation)
}

func TestMCPBackendFailureCarriesRequestIDWithoutDriverText(t *testing.T) {
	captured := captureStandardLog(t)
	st := backendFailingClaimStore{Store: store.NewMemory()}
	server := NewServer(st)
	server.Workspace = "demo"
	server.WorkOrders = &workorder.Service{Store: st}
	body := `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"claim_work_order","arguments":{"workspace_id":"demo","work_order_id":"wo-1","session_id":"session","client_token":"secret-client-token","agent":"codex","model":"gpt"}}}`
	request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	ctx := withCredentialActor(request.Context(), core.AuthenticatedCredential{ID: "pat-token", OwnerUserID: "usr-owner", Kind: core.CredentialUser, Scope: core.CredentialScopeUser})
	request = request.WithContext(context.WithValue(ctx, middleware.RequestIDKey, "host/abc-000042"))
	response := httptest.NewRecorder()
	server.handleMCP(response, request)
	text, isError := decodeMCPToolText(t, response.Body.Bytes())
	if !isError || text != "SingleStore database operation failed (request_id host/abc-000042)" {
		t.Fatalf("claim failure isError=%v text=%q", isError, text)
	}
	logged := captured.String()
	want := fmt.Sprintf("mcp: tool call failed: request_id=%q tool=%q error=%q", "host/abc-000042", "claim_work_order", "SingleStore database operation failed")
	if !strings.Contains(logged, want) || strings.Contains(logged, "secret-client-token") {
		t.Fatalf("log %q, want %q without the client token", logged, want)
	}
}

// usageReportFixture claims one implement order as run:owner and returns a
// server and the owner's request.
func usageReportFixture(t *testing.T) (*Server, store.Store, *http.Request, string) {
	t.Helper()
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	st := store.NewMemory()
	task := core.Task{ID: "usage-task", Workspace: "demo", Repo: "conveyor", State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: time.Now()}
	job := core.Job{ID: "usage-task-implement-1", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued}); err != nil {
		t.Fatal(err)
	}
	if _, err := storetest.For(st).ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{SessionID: "usage-session", ClientToken: "secret", ClaimantID: core.TaskRunClaimantID("owner"), Agent: "claude-code", Model: "claude-opus-5-5", Lease: time.Minute}); err != nil {
		t.Fatal(err)
	}
	server := NewServer(st)
	server.Workspace = "demo"
	server.WorkOrders = &workorder.Service{Store: st}
	request := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	request = request.WithContext(withCredentialActor(request.Context(), core.AuthenticatedCredential{ID: "pat-owner", OwnerUserID: "owner", Kind: core.CredentialUser, Scope: core.CredentialScopeUser}))
	return server, st, request, job.ID
}

// TestMCPReportContinuationIsRetired proves the tool is absent from tools/list
// and mcpCapabilities and that a call fails closed as an unknown tool with no
// event or order change (DEC-94; component-mcp-protocol, Observability).
func TestMCPReportContinuationIsRetired(t *testing.T) {
	for _, tool := range mcpTools() {
		if name, _ := tool["name"].(string); name == "report_continuation" {
			t.Fatal("tools/list still publishes report_continuation")
		}
	}
	if _, mapped := mcpCapabilities["report_continuation"]; mapped {
		t.Fatal("report_continuation still has a capability")
	}
	server, st, request, orderID := usageReportFixture(t)
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	beforeOrder, _ := st.GetWorkOrder(ctx, orderID)
	beforeEvents, _ := st.ListEvents(ctx, beforeOrder.TaskID)
	args := map[string]any{"workspace_id": "demo", "work_order_id": orderID, "session_id": "usage-session", "attempt_id": beforeOrder.AttemptID, "harness": "claude", "continuation_session_id": "native", "launch_environment": "local"}
	if _, err := server.callMCPTool(request, "report_continuation", args); err == nil || !strings.Contains(err.Error(), "unknown tool") {
		t.Fatalf("report_continuation error=%v, want unknown tool", err)
	}
	afterOrder, _ := st.GetWorkOrder(ctx, orderID)
	afterEvents, _ := st.ListEvents(ctx, beforeOrder.TaskID)
	if !reflect.DeepEqual(beforeOrder, afterOrder) || len(afterEvents) != len(beforeEvents) {
		t.Fatalf("refused call changed state: order %+v -> %+v, events %d -> %d", beforeOrder, afterOrder, len(beforeEvents), len(afterEvents))
	}
}

// TestMCPUsageRefusesWorkerFallback proves a worker_fallback report is refused
// before any usage write (req-usage-telemetry REQ-2; DEC-97(1)).
func TestMCPUsageRefusesWorkerFallback(t *testing.T) {
	server, st, request, orderID := usageReportFixture(t)
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	args := map[string]any{"workspace_id": "demo", "work_order_id": orderID, "session_id": "usage-session", "tokens_in": float64(10), "tokens_out": float64(5), "source": "worker_fallback"}
	if _, err := server.callMCPTool(request, "report_usage", args); err == nil || !strings.Contains(err.Error(), "worker_fallback usage reports are retired") {
		t.Fatalf("worker_fallback error=%v", err)
	}
	order, err := st.GetWorkOrder(ctx, orderID)
	if err != nil || order.UsageReported || order.TokensIn != 0 {
		t.Fatalf("refused report wrote usage: %+v err=%v", order, err)
	}
	if count, _ := st.CountEvents(ctx, order.TaskID, "work_order.usage_reported"); count != 0 {
		t.Fatalf("usage events=%d, want none", count)
	}
}

// TestMCPUsageStoresRateLimitWithReport proves a reported rate_limit is stored
// on the order with the usage record (req-usage-telemetry REQ-3).
func TestMCPUsageStoresRateLimitWithReport(t *testing.T) {
	server, st, request, orderID := usageReportFixture(t)
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	args := map[string]any{"workspace_id": "demo", "work_order_id": orderID, "session_id": "usage-session", "tokens_in": float64(10), "tokens_out": float64(5), "rate_limit": map[string]any{"status": "allowed", "remaining": float64(42)}}
	if _, err := server.callMCPTool(request, "report_usage", args); err != nil {
		t.Fatal(err)
	}
	order, err := st.GetWorkOrder(ctx, orderID)
	if err != nil || !order.UsageReported || !order.SelfReported || order.TokensIn != 10 || order.RateLimit == nil || order.RateLimit.Status != "allowed" || order.RateLimit.Remaining == nil || *order.RateLimit.Remaining != 42 || order.RateLimitObservedAt.IsZero() {
		t.Fatalf("stored usage=%+v rate_limit=%+v err=%v", order, order.RateLimit, err)
	}
}

// claimSessionFixture is a volatile workspace of ClaimSessionScaleOrders
// claimed orders that each pin a 64 KiB governance snapshot, beside the
// owner's named implement order and another user's holder order. The server's
// store and work-order service read through one recorder.
type claimSessionFixture struct {
	memory   store.Store
	recorder *storetest.WorkOrderReadRecorder
	server   *Server
	request  *http.Request
	named    string
}

func newClaimSessionFixture(t *testing.T) claimSessionFixture {
	t.Helper()
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.SystemActor()), "demo")
	memory := store.NewMemory()
	governance := storetest.ClaimSessionGovernance()
	claimed := func(id, claimant, session string) {
		t.Helper()
		task := core.Task{ID: id, Workspace: "demo", Repo: "app", Title: id, Branch: "conveyor/task-" + id, BaseBranch: "main", State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: time.Now()}
		if err := memory.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
		job := core.Job{ID: id + "-implement-1", TaskID: id, Stage: core.StageImplement, State: core.JobPending}
		if err := memory.CreateJob(ctx, job); err != nil {
			t.Fatal(err)
		}
		if err := storetest.For(memory).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: id, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued, QueueEnteredAt: time.Now(), QueueDeadline: time.Now().Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
		if _, err := storetest.For(memory).ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{SessionID: session, ClientToken: session, ClaimantID: claimant, Lease: time.Hour, ExecutionTimeout: time.Hour, Requirements: storetest.ClaimSessionRequirements(), Governance: &governance}); err != nil {
			t.Fatal(err)
		}
	}
	claimed("named", core.TaskRunClaimantID("usr-owner"), "own-session")
	claimed("holder", core.TaskRunClaimantID("usr-other"), "holder-session")
	for i := range storetest.ClaimSessionScaleOrders {
		claimed(fmt.Sprintf("seed-%03d", i), core.TaskRunClaimantID("usr-other"), fmt.Sprintf("seed-session-%03d", i))
	}
	orders, err := memory.ListWorkOrders(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(orders) != storetest.ClaimSessionScaleOrders+2 {
		t.Fatalf("fixture holds %d orders, want %d", len(orders), storetest.ClaimSessionScaleOrders+2)
	}
	recorder := storetest.NewWorkOrderReadRecorder(memory)
	server := NewServer(recorder)
	server.Workspace = "demo"
	cfg := &config.Config{Workspace: "demo", Repos: []config.Repo{{Name: "app", URL: "https://github.com/acme/app.git", GitHub: "acme/app", Base: "main"}}}
	server.WorkOrders = &workorder.Service{Store: recorder, ConfigProvider: func(context.Context) (*config.Config, error) { return cfg, nil }}
	server.Credentials = staticCredentialVerifier{"owner-token": {ID: "pat-owner", OwnerUserID: "usr-owner", Kind: core.CredentialUser, Scope: core.CredentialScopeUser}}
	request := httptest.NewRequest(http.MethodPost, "/mcp", nil).WithContext(withCredentialActor(ctx, core.AuthenticatedCredential{ID: "pat-owner", OwnerUserID: "usr-owner", Kind: core.CredentialUser, Scope: core.CredentialScopeUser}))
	return claimSessionFixture{memory: memory, recorder: recorder, server: server, request: request, named: "named-implement-1"}
}

// TestClaimSessionCheckReadsNoWorkspaceOrders drives each caller of
// Server.authorizeClaimantSession, the claim-bound tool report_progress, the
// live-claim mutation release_work_order, and the submission template, with a
// mismatched, missing-order, foreign, empty, and own session over a
// 200-order workspace whose orders each pin a 64 KiB governance snapshot. Each
// call must return the error the former ListWorkOrders scan returns for the
// same rows, make no ListWorkOrders call, and, when the session check refuses,
// decode zero snapshot bytes. renew_work_order does not use the session check;
// a mismatched, missing-order, or foreign renewal must return its former error,
// make no ListWorkOrders call, and decode zero snapshot bytes
// (component-work-orders, Snapshot-free fold reads; component-mcp-protocol,
// Claim-bound authorization).
func TestClaimSessionCheckReadsNoWorkspaceOrders(t *testing.T) {
	inputs := []struct {
		name, workOrder, session string
	}{
		{"mismatched", "named-implement-1", "unknown-session"},
		{"missing order", "missing-implement-1", "unknown-session"},
		{"missing order with a held session", "missing-implement-1", "holder-session"},
		{"foreign", "named-implement-1", "holder-session"},
		{"empty", "named-implement-1", ""},
		{"own", "named-implement-1", "own-session"},
	}
	callers := map[string]func(f claimSessionFixture, workOrder, session string) error{
		"report_progress": func(f claimSessionFixture, workOrder, session string) error {
			_, err := f.server.callMCPTool(f.request, "report_progress", map[string]any{"workspace_id": "demo", "work_order_id": workOrder, "session_id": session, "message": "progress"})
			return err
		},
		"release_work_order": func(f claimSessionFixture, workOrder, session string) error {
			_, err := f.server.callMCPTool(f.request, "release_work_order", map[string]any{"workspace_id": "demo", "work_order_id": workOrder, "session_id": session, "reason": "done"})
			return err
		},
		"submission_template": func(f claimSessionFixture, workOrder, session string) error {
			request := httptest.NewRequest(http.MethodGet, "/v1/work-orders/"+workOrder+"/pull-request-template?workspace_id=demo&session_id="+session, nil)
			request.Header.Set("Authorization", "Bearer owner-token")
			response := httptest.NewRecorder()
			f.server.Handler().ServeHTTP(response, request)
			if response.Code == http.StatusOK {
				return nil
			}
			return errors.New(strings.TrimSpace(response.Body.String()))
		},
	}
	for _, caller := range slices.Sorted(maps.Keys(callers)) {
		for _, input := range inputs {
			t.Run(caller+"/"+input.name, func(t *testing.T) {
				f := newClaimSessionFixture(t)
				want := storetest.LegacyClaimSessionAnswer(f.request.Context(), f.memory, input.workOrder, input.session)
				f.recorder.Reset()
				err := callers[caller](f, input.workOrder, input.session)
				if f.recorder.Calls("ListWorkOrders") != 0 {
					t.Fatalf("%s made %d ListWorkOrders calls", caller, f.recorder.Calls("ListWorkOrders"))
				}
				if want == nil {
					// The session check admitted the owner and the operation
					// succeeded.
					t.Logf("%s own session: %v", caller, err)
					if err != nil {
						t.Fatalf("%s refused the own session: %v", caller, err)
					}
					return
				}
				if err == nil || (!errors.Is(err, want) && err.Error() != want.Error()) {
					t.Fatalf("%s returned %v, want the former answer %v", caller, err, want)
				}
				if f.recorder.SnapshotBytes() != 0 || f.recorder.Rows() > 1 {
					t.Fatalf("%s decoded %d snapshot bytes over %d rows, want 0 bytes and at most the named order", caller, f.recorder.SnapshotBytes(), f.recorder.Rows())
				}
			})
		}
	}

	for _, test := range []struct {
		name, workOrder, session string
		want                     error
	}{
		{"mismatched", "named-implement-1", "unknown-session", store.ErrWorkOrderClaimLost},
		{"missing order", "missing-implement-1", "unknown-session", store.ErrNotFound},
		{"foreign", "named-implement-1", "holder-session", store.ErrWorkOrderClaimLost},
	} {
		t.Run("renew_work_order/"+test.name, func(t *testing.T) {
			f := newClaimSessionFixture(t)
			f.recorder.Reset()
			_, err := f.server.callMCPTool(f.request, "renew_work_order", map[string]any{"workspace_id": "demo", "work_order_id": test.workOrder, "session_id": test.session})
			if !errors.Is(err, test.want) {
				t.Fatalf("renew returned %v, want %v", err, test.want)
			}
			if f.recorder.Calls("ListWorkOrders") != 0 || f.recorder.Rows() > 1 || f.recorder.SnapshotBytes() != 0 {
				t.Fatalf("renew made %d ListWorkOrders calls and decoded %d snapshot bytes over %d rows, want no workspace read and 0 snapshot bytes", f.recorder.Calls("ListWorkOrders"), f.recorder.SnapshotBytes(), f.recorder.Rows())
			}
		})
	}
	t.Run("renew_work_order/own", func(t *testing.T) {
		f := newClaimSessionFixture(t)
		before, err := f.memory.GetWorkOrder(f.request.Context(), "named-implement-1")
		if err != nil {
			t.Fatal(err)
		}
		result, err := f.server.callMCPTool(f.request, "renew_work_order", map[string]any{"workspace_id": "demo", "work_order_id": "named-implement-1", "session_id": "own-session", "lease_seconds": float64(3600)})
		if err != nil {
			t.Fatal(err)
		}
		renewed, ok := result.(workorder.OrderDelivery)
		if !ok || renewed.State != core.WorkOrderClaimed || renewed.AttemptID != before.AttemptID || !renewed.ExecutionDeadline.Equal(before.ExecutionDeadline) || !renewed.LeaseExpiresAt.Equal(before.ExecutionDeadline) {
			t.Fatalf("own renewal = %+v, want the claimed order with its attempt and the one-hour lease capped at the fixed deadline %s", result, before.ExecutionDeadline)
		}
		if f.recorder.Calls("ListWorkOrders") != 0 {
			t.Fatalf("own renewal made %d ListWorkOrders calls", f.recorder.Calls("ListWorkOrders"))
		}
	})
}

// Every MCP field that carries timeline text publishes the first-sentence
// rules in one sentence under 40 words. The field stays a string with no new
// validation, and each tool's required fields stay as they were
// (req-readable-agent-text AC-1.2 to AC-1.4; DEC-126(4)).
func TestMCPTimelineFieldsPublishFirstSentenceRules(t *testing.T) {
	t.Parallel()
	if words := len(strings.Fields(timelineTextFieldGuidance)); words >= 40 {
		t.Fatalf("timeline field guidance has %d words, want under 40", words)
	}
	for _, fragment := range []string{"first sentence", "plain language", "what happened and what happens next", "work-order or session ID", "file path", "migration number", "commit SHA", "REQ-n, AC-n.m, or DEC-n ID", "later sentences"} {
		if !strings.Contains(timelineTextFieldGuidance, fragment) {
			t.Fatalf("timeline field guidance is missing %q", fragment)
		}
	}
	type field struct {
		name     string
		required []string
	}
	fields := map[string]field{
		"report_progress":       {"message", []string{"work_order_id", "session_id", "message"}},
		"release_work_order":    {"reason", []string{"work_order_id", "session_id"}},
		"submit_review_verdict": {"summary", []string{"work_order_id", "session_id", "verdict", "reason_code", "summary", "requirement_citations", "done_criteria_coverage", "governance_assessment"}},
		"submit_verification":   {"feedback", nil},
	}
	seen := map[string]bool{}
	for _, tool := range mcpTools() {
		name, _ := tool["name"].(string)
		want, ok := fields[name]
		if !ok {
			continue
		}
		schema := tool["inputSchema"].(map[string]any)
		property, _ := schema["properties"].(map[string]any)[want.name].(map[string]any)
		if len(property) != 2 || property["type"] != "string" || property["description"] != timelineTextFieldGuidance {
			t.Errorf("%s %s schema = %+v", name, want.name, property)
		}
		if want.required != nil && !reflect.DeepEqual(schema["required"], want.required) {
			t.Errorf("%s required = %v, want %v", name, schema["required"], want.required)
		}
		if required, _ := schema["required"].([]string); want.required == nil && slices.Contains(required, want.name) {
			t.Errorf("%s now requires %s: %v", name, want.name, required)
		}
		seen[name] = true
	}
	if len(seen) != len(fields) {
		t.Fatalf("timeline fields checked on %v, want %d tools", seen, len(fields))
	}
}
