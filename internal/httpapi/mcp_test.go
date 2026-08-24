package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
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
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
	workerservice "github.com/kidus-tiliksew/conveyor/internal/worker"
	"github.com/kidus-tiliksew/conveyor/internal/workorder"
)

func TestMCPReadArtifactSupportsManualSessionsAndEnforcesWorkerOwnership(t *testing.T) {
	t.Parallel()
	ctx := store.WithWorkspace(context.Background(), "demo")
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
		order, task, session, worker string
	}{
		{order: "order-a", task: "task-a", session: "session-a", worker: "worker-a"},
		{order: "order-b", task: "task-b", session: "session-b", worker: "worker-b"},
	} {
		if err := st.CreateJob(ctx, core.Job{ID: item.order, TaskID: item.task, Stage: core.StageImplement, State: core.JobPending}); err != nil {
			t.Fatal(err)
		}
		if err := storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: item.order, TaskID: item.task, JobID: item.order, Stage: core.StageImplement, State: core.WorkOrderQueued}); err != nil {
			t.Fatal(err)
		}
		if _, err := storetest.For(st).ClaimWorkOrder(ctx, item.order, core.WorkOrderClaim{SessionID: item.session, ClientToken: "secret", ClaimantID: item.worker, WorkerID: item.worker, Lease: time.Minute}); err != nil {
			t.Fatal(err)
		}
	}
	artifact, err := st.CreateArtifact(ctx, core.Artifact{Name: "design.png", ContentType: "image/png", TaskID: "task-a"}, []byte("png"))
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(st)
	server.Workspace = "demo"
	server.WorkOrders = &workorder.Service{Store: st}
	args := map[string]any{"workspace_id": "demo", "work_order_id": "order-a", "session_id": "session-a", "artifact_id": artifact.ID}
	request := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	if _, err = server.callMCPTool(request, "read_artifact", args); err != nil {
		t.Fatalf("manual read: %v", err)
	}
	workerRequest := request.WithContext(context.WithValue(request.Context(), workerContextKey{}, core.Worker{ID: "worker-a", Workspace: "demo"}))
	if _, err = server.callMCPTool(workerRequest, "read_artifact", args); err != nil {
		t.Fatalf("owning worker read: %v", err)
	}
	audit, err := st.CreateArtifact(ctx, core.Artifact{Name: "planning-audit.json", ContentType: "application/json", Role: core.ArtifactRoleGeneratedAudit, TaskID: "task-a"}, []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	auditArgs := maps.Clone(args)
	auditArgs["artifact_id"] = audit.ID
	if _, err = server.callMCPTool(workerRequest, "read_artifact", auditArgs); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("own-task generated audit read error=%v", err)
	}
	otherWorkerRequest := request.WithContext(context.WithValue(request.Context(), workerContextKey{}, core.Worker{ID: "worker-b", Workspace: "demo"}))
	if _, err = server.callMCPTool(otherWorkerRequest, "read_artifact", args); !errors.Is(err, store.ErrWorkOrderClaimLost) {
		t.Fatalf("other worker read error=%v", err)
	}
	wrongWorkspace := maps.Clone(args)
	wrongWorkspace["workspace_id"] = "other"
	if _, err = server.callMCPTool(request, "read_artifact", wrongWorkspace); err == nil || !strings.Contains(err.Error(), "workspace_not_found") {
		t.Fatalf("wrong workspace read error=%v", err)
	}
}

func TestMCPRenewReportsSameSessionCheckpointRelease(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
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
	server.Workers = &workerservice.Service{Store: st, WorkOrders: server.WorkOrders}
	request := httptest.NewRequest(http.MethodPost, "/mcp", nil).WithContext(store.WithCredential(ctx, core.AuthenticatedCredential{ID: "user-token", OwnerUserID: "usr-runner", Kind: core.CredentialUser, Scope: core.CredentialScopeUser}))
	args := map[string]any{"workspace_id": "demo", "work_order_id": job.ID, "session_id": "run-session"}
	if _, err := server.callMCPTool(request, "renew_work_order", args); !errors.Is(err, store.ErrWorkOrderReleasedAtCheckpoint) {
		t.Fatalf("renew error=%v", err)
	}
	args["session_id"] = "other-session"
	if _, err := server.callMCPTool(request, "renew_work_order", args); !errors.Is(err, store.ErrWorkOrderClaimLost) {
		t.Fatalf("wrong-session renew error=%v", err)
	}
}

func TestMCPImplementationGovernanceProposalsBindToClaimedTask(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	task := core.Task{ID: "task-governance", Workspace: "demo", Repo: "conveyor", State: core.TaskRunning, CreatedAt: time.Now()}
	job := core.Job{ID: "order-governance", TaskID: task.ID, Stage: core.StageImplement, State: core.JobPending}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: job.ID, TaskID: task.ID, JobID: job.ID, Stage: core.StageImplement, State: core.WorkOrderQueued}); err != nil {
		t.Fatal(err)
	}
	if _, err := storetest.For(st).ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{SessionID: "session-governance", ClientToken: "secret", ClaimantID: "implementer", WorkerID: "implementer", Lease: time.Minute}); err != nil {
		t.Fatal(err)
	}
	document, _, err := st.CreateSystemDesign(ctx, core.SystemDesign{ID: "design-dispatch", Title: "Dispatch", Category: "Architecture"}, core.SystemDesignVersion{Content: "# Dispatch\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/dispatch/**\n```", Origin: core.SystemDesignOriginOperator})
	if err != nil {
		t.Fatal(err)
	}
	requirement, _, err := st.CreateRequirement(ctx, core.Requirement{ID: "req-dispatch", Title: "Dispatch requirements"}, core.RequirementVersion{
		Content:    "# Dispatch\n\n```conveyor:requirements\n- id: REQ-1\n  statement: Dispatch work safely.\n```",
		Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Dispatch work safely."}}, Origin: core.RequirementOriginOperator,
	})
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(st)
	server.Workspace = "demo"
	server.WorkOrders = &workorder.Service{Store: st}
	request := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	request = request.WithContext(context.WithValue(request.Context(), workerContextKey{}, core.Worker{ID: "implementer", Workspace: "demo"}))
	identity := map[string]any{"workspace_id": "demo", "work_order_id": job.ID, "session_id": "session-governance"}
	revisionArgs := maps.Clone(identity)
	revisionArgs["document_id"] = document.ID
	revisionArgs["content"] = "# Dispatch v2\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/dispatch/**\n    - internal/workorder/**\n```"
	result, err := server.callMCPTool(request, "propose_system_design_revision", revisionArgs)
	if err != nil {
		t.Fatal(err)
	}
	revisionResult := result.(systemDesignProposalResult)
	revision := revisionResult.SystemDesignVersion
	if revision.Origin != core.SystemDesignOriginImplementation || revision.OriginTaskID != task.ID || revision.Confirmed || revision.Deduplicated {
		t.Fatalf("revision=%+v", revision)
	}
	if revisionResult.Guidance != systemDesignProposalGuidance {
		t.Fatalf("revision guidance=%q", revisionResult.Guidance)
	}
	assertProposalResultJSON(t, revisionResult, map[string]any{
		"document_id": revision.DocumentID,
		"version":     float64(revision.Version),
		"origin":      string(core.SystemDesignOriginImplementation),
		"guidance":    systemDesignProposalGuidance,
	})
	repeatedArgs := maps.Clone(revisionArgs)
	repeatedArgs["content"] = " \r\n" + strings.ReplaceAll(revisionArgs["content"].(string), "\n", "\r\n") + "\r\n"
	result, err = server.callMCPTool(request, "propose_system_design_revision", repeatedArgs)
	if err != nil {
		t.Fatal(err)
	}
	reusedResult := result.(systemDesignProposalResult)
	reused := reusedResult.SystemDesignVersion
	if !reused.Deduplicated || reused.Version != revision.Version {
		t.Fatalf("reused revision=%+v original=%+v", reused, revision)
	}
	if reusedResult.Guidance != systemDesignProposalGuidance {
		t.Fatalf("reused revision guidance=%q", reusedResult.Guidance)
	}
	assertProposalResultJSON(t, reusedResult, map[string]any{
		"document_id":  reused.DocumentID,
		"version":      float64(reused.Version),
		"deduplicated": true,
		"guidance":     systemDesignProposalGuidance,
	})
	requirementArgs := maps.Clone(identity)
	requirementArgs["document_id"] = requirement.ID
	requirementArgs["content"] = "# Dispatch\n\n```conveyor:requirements\n- id: REQ-1\n  statement: Dispatch work safely with exact claims.\n```"
	result, err = server.callMCPTool(request, "propose_requirement_revision", requirementArgs)
	if err != nil {
		t.Fatal(err)
	}
	requirementResult := result.(requirementProposalResult)
	proposedRequirement := requirementResult.RequirementVersion
	if proposedRequirement.Origin != core.RequirementOriginImplementation || proposedRequirement.OriginTaskID != task.ID || proposedRequirement.Confirmed || proposedRequirement.Deduplicated {
		t.Fatalf("requirement revision=%+v", proposedRequirement)
	}
	if requirementResult.Guidance != requirementProposalGuidance {
		t.Fatalf("requirement guidance=%q", requirementResult.Guidance)
	}
	result, err = server.callMCPTool(request, "propose_requirement_revision", requirementArgs)
	if err != nil {
		t.Fatal(err)
	}
	reusedRequirement := result.(requirementProposalResult).RequirementVersion
	if !reusedRequirement.Deduplicated || reusedRequirement.Version != proposedRequirement.Version {
		t.Fatalf("reused requirement=%+v original=%+v", reusedRequirement, proposedRequirement)
	}
	invalidRequirementArgs := maps.Clone(requirementArgs)
	invalidRequirementArgs["content"] = "missing the conveyor:requirements fence"
	if _, err = server.callMCPTool(request, "propose_requirement_revision", invalidRequirementArgs); err == nil {
		t.Fatal("invalid requirement proposal was accepted")
	}
	versions, listErr := st.ListRequirementVersions(ctx, requirement.ID)
	if listErr != nil || len(versions) != 2 {
		t.Fatalf("invalid proposal partially wrote versions=%+v err=%v", versions, listErr)
	}
	staleRequirementArgs := maps.Clone(requirementArgs)
	staleRequirementArgs["session_id"] = "stale-session"
	if _, err = server.callMCPTool(request, "propose_requirement_revision", staleRequirementArgs); err == nil {
		t.Fatal("stale implementation session proposed a requirement revision")
	}
	decisionArgs := maps.Clone(identity)
	decisionArgs["statement"] = "Project governance from events."
	decisionArgs["context"] = "Lineage must rebuild."
	decisionArgs["alternatives_rejected"] = "Mutable edges drift."
	result, err = server.callMCPTool(request, "propose_decision", decisionArgs)
	if err != nil {
		t.Fatal(err)
	}
	decisionResult := result.(decisionProposalResult)
	decision := decisionResult.Decision
	if decision.ID != "DEC-1" || decision.Origin != core.DecisionOriginImplementation || decision.OriginTaskID != task.ID || decision.Status != core.DecisionProposed {
		t.Fatalf("decision=%+v", decision)
	}
	if decisionResult.Guidance != decisionProposalGuidance {
		t.Fatalf("decision guidance=%q", decisionResult.Guidance)
	}
	assertProposalResultJSON(t, decisionResult, map[string]any{
		"id":       decision.ID,
		"status":   string(core.DecisionProposed),
		"origin":   string(core.DecisionOriginImplementation),
		"guidance": decisionProposalGuidance,
	})
	wrongSession := maps.Clone(decisionArgs)
	wrongSession["session_id"] = "stale-session"
	if _, err = server.callMCPTool(request, "propose_decision", wrongSession); err == nil {
		t.Fatal("stale implementation session proposed a decision")
	}
	reviewOrderID := "order-governance-review"
	reviewTask := core.Task{ID: "task-governance-review", Workspace: "demo", Repo: "conveyor", State: core.TaskRunning, CreatedAt: time.Now()}
	if err = st.CreateTask(ctx, reviewTask); err != nil {
		t.Fatal(err)
	}
	if err = st.CreateJob(ctx, core.Job{ID: reviewOrderID, TaskID: reviewTask.ID, Stage: core.StageReview, State: core.JobPending}); err != nil {
		t.Fatal(err)
	}
	if err = storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: reviewOrderID, TaskID: reviewTask.ID, JobID: reviewOrderID, Stage: core.StageReview, State: core.WorkOrderQueued}); err != nil {
		t.Fatal(err)
	}
	if _, err = storetest.For(st).ClaimWorkOrder(ctx, reviewOrderID, core.WorkOrderClaim{SessionID: "review-session", ClientToken: "review-secret", ClaimantID: "reviewer", WorkerID: "implementer", Lease: time.Minute}); err != nil {
		t.Fatal(err)
	}
	reviewRequirementArgs := maps.Clone(requirementArgs)
	reviewRequirementArgs["work_order_id"], reviewRequirementArgs["session_id"] = reviewOrderID, "review-session"
	if _, err = server.callMCPTool(request, "propose_requirement_revision", reviewRequirementArgs); err == nil || !strings.Contains(err.Error(), "claimed implement work order") {
		t.Fatalf("review-stage requirement proposal error=%v", err)
	}
}

func assertProposalResultJSON(t *testing.T, result any, want map[string]any) {
	t.Helper()
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	for field, value := range want {
		if got[field] != value {
			t.Errorf("result %s=%v, want %v; JSON=%s", field, got[field], value, data)
		}
	}
}

func TestMCPClaimDefaultsToFiveMinuteLease(t *testing.T) {
	t.Parallel()
	ctx := store.WithWorkspace(t.Context(), "demo")
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
	request = request.WithContext(store.WithCredential(request.Context(), core.AuthenticatedCredential{ID: "agent-token", OwnerUserID: "usr-owner", Kind: core.CredentialAgent, Scope: core.CredentialScopeUser}))
	result, err := server.callMCPTool(request, "claim_work_order", map[string]any{
		"workspace_id": "demo", "work_order_id": job.ID, "session_id": "session", "client_token": "token", "claimant_id": "run:spoofed", "agent": "codex", "model": "gpt",
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed := result.(core.WorkOrder)
	if got := claimed.LeaseExpiresAt.Sub(claimed.ExecutionStartedAt); got != core.DefaultWorkOrderClaimLease {
		t.Fatalf("MCP default claim lease = %s, want %s", got, core.DefaultWorkOrderClaimLease)
	}
	if claimed.ClaimantID != core.TaskRunClaimantID("usr-owner") {
		t.Fatalf("MCP claim identity = %q", claimed.ClaimantID)
	}
}

func TestMCPClaimRequiresCredentialOwnersStoredForgeToken(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
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
	tokens := &forgeTokenFixture{}
	provider := func(context.Context) (*config.Config, error) {
		return &config.Config{Workspace: "demo", Routing: config.Routing{Stages: map[string]config.StageRoute{
			"implement": {Timeout: time.Hour},
		}}}, nil
	}
	server := NewServer(st)
	server.Workspace = "demo"
	server.ForgeTokens = tokens
	server.WorkOrders = &workorder.Service{Store: st, ConfigProvider: provider, ForgeTokens: tokens}
	request := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	request = request.WithContext(store.WithCredential(request.Context(), core.AuthenticatedCredential{ID: "agent-token", OwnerUserID: "usr-owner", Kind: core.CredentialAgent, Scope: core.CredentialScopeUser}))
	args := map[string]any{
		"workspace_id": "demo", "work_order_id": job.ID, "session_id": "session", "client_token": "token", "claimant_id": "run:spoofed", "agent": "codex", "model": "gpt",
	}
	listed, err := server.callMCPTool(request, "list_work_orders", map[string]any{"workspace_id": "demo"})
	if err != nil {
		t.Fatal(err)
	}
	orders := listed.([]core.WorkOrder)
	if len(orders) != 1 || orders[0].Claimable || orders[0].ClaimRefusalReason != store.ForgeTokenRequiredMessage {
		t.Fatalf("missing-token MCP projection=%+v", orders)
	}
	if _, err = server.callMCPTool(request, "claim_work_order", args); !errors.Is(err, store.ErrForgeTokenRequired) {
		t.Fatalf("missing-token MCP claim error=%v", err)
	}
	queued, err := st.GetWorkOrder(ctx, job.ID)
	if err != nil || queued.State != core.WorkOrderQueued {
		t.Fatalf("refused MCP order=%+v err=%v", queued, err)
	}
	tokens.status.Configured = true
	result, err := server.callMCPTool(request, "claim_work_order", args)
	if err != nil {
		t.Fatalf("configured-token MCP claim: %v", err)
	}
	if claimed := result.(core.WorkOrder); claimed.ClaimantID != core.TaskRunClaimantID("usr-owner") {
		t.Fatalf("MCP claim identity=%q", claimed.ClaimantID)
	}
}

func TestMCPClaimUsesCredentialOwnerForAssigneeEligibility(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
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
	bobRequest := httptest.NewRequest(http.MethodPost, "/mcp", nil).WithContext(store.WithCredential(t.Context(), bob))
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
	aliceRequest := httptest.NewRequest(http.MethodPost, "/mcp", nil).WithContext(store.WithCredential(t.Context(), alice))
	args["claimant_id"] = core.TaskRunClaimantID("usr-bob")
	result, err := server.callMCPTool(aliceRequest, "claim_work_order", args)
	if err != nil {
		t.Fatalf("assignee MCP claim: %v", err)
	}
	claimed := result.(core.WorkOrder)
	if claimed.ClaimantID != core.TaskRunClaimantID("usr-alice") {
		t.Fatalf("assignee MCP claim identity = %q", claimed.ClaimantID)
	}
}

func TestMCPSubmitForReviewReturnsActionableEvidenceGateError(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	task := core.Task{ID: "mcp-evidence-gate", Workspace: "demo", Repo: "api", State: core.TaskRunning, NextStage: core.StageImplement, CreatedAt: time.Now()}
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
	if _, err := storetest.For(st).ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{SessionID: "session", ClientToken: "token", ClaimantID: core.TaskRunClaimantID("owner"), Lease: time.Minute}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Workspace: "demo", Execution: config.ExecutionPolicy{RequireVerificationEvidence: true},
		Repos: []config.Repo{{Name: "api", Base: "main"}},
	}
	server := NewServer(st)
	server.Workspace = "demo"
	server.WorkOrders = &workorder.Service{Store: st, ConfigProvider: func(context.Context) (*config.Config, error) { return cfg, nil }}
	request := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	request = request.WithContext(store.WithCredential(request.Context(), core.AuthenticatedCredential{ID: "owner-token", OwnerUserID: "owner", Kind: core.CredentialUser}))
	_, err := server.callMCPTool(request, "submit_for_review", map[string]any{
		"workspace_id": "demo", "work_order_id": job.ID, "session_id": "session",
	})
	if err == nil || !strings.Contains(err.Error(), "POST /v1/artifacts") || !strings.Contains(err.Error(), "role=verification_evidence") {
		t.Fatalf("MCP evidence gate error=%v", err)
	}
	order, getErr := st.GetWorkOrder(ctx, job.ID)
	if getErr != nil || order.State != core.WorkOrderClaimed {
		t.Fatalf("order advanced after MCP rejection: %+v err=%v", order, getErr)
	}
}

func TestMCPReportUsagePersistsOptionalRateLimitWithoutGatingOrClearing(t *testing.T) {
	t.Parallel()
	ctx := store.WithWorkspace(t.Context(), "demo")
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
	request = request.WithContext(store.WithCredential(request.Context(), core.AuthenticatedCredential{ID: "owner-token", OwnerUserID: "owner", Kind: core.CredentialUser}))
	reset := "2026-07-28T13:00:00Z"
	result, err := server.callMCPTool(request, "report_usage", map[string]any{
		"workspace_id": "demo", "work_order_id": job.ID, "session_id": "session",
		"tokens_in": 100.0, "tokens_out": 25.0, "cost_usd": 0.5,
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
		"tokens_in": 150.0, "tokens_out": 30.0, "cost_usd": 0.75,
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
		"tokens_in": 150.0, "tokens_out": 30.0, "cost_usd": 0.75,
		"rate_limit": map[string]any{"status": "limited", "reset_at": "not-rfc3339"},
	}); err == nil || !strings.Contains(err.Error(), "rate_limit") {
		t.Fatalf("invalid reset_at error=%v", err)
	}
}

func TestMCPReportContinuationIsLaunchingClientOnlyAndReplacesCapture(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	task := core.Task{ID: "mcp-continuation", Workspace: "demo", State: core.TaskRunning, CreatedAt: time.Now()}
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
	claimed, err := storetest.For(st).ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{
		SessionID: "claim-session", ClientToken: "secret", ClaimantID: "worker-a", WorkerID: "worker-a", Agent: "codex", Lease: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(st)
	server.Workspace = "demo"
	server.WorkOrders = &workorder.Service{Store: st}
	args := map[string]any{
		"workspace_id": "demo", "work_order_id": job.ID, "session_id": claimed.SessionID,
		"continuation_session_id": "native-1", "attempt_id": claimed.AttemptID,
		"harness": "codex", "launch_environment": "worker-a/env-1",
	}
	request := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	agentRequest := request.WithContext(store.WithCredential(request.Context(), core.AuthenticatedCredential{
		ID: "agent", OwnerUserID: "owner", Kind: core.CredentialAgent,
	}))
	if _, err = server.callMCPTool(agentRequest, "report_continuation", args); err == nil || !strings.Contains(err.Error(), "operator-scoped user credential") {
		t.Fatalf("agent report error=%v", err)
	}
	workerRequest := request.WithContext(context.WithValue(request.Context(), workerContextKey{}, core.Worker{ID: "worker-a", Workspace: "demo"}))
	result, err := server.callMCPTool(workerRequest, "report_continuation", args)
	if err != nil {
		t.Fatal(err)
	}
	reported := result.(core.WorkOrder)
	if reported.ContinuationSessionID != "native-1" || reported.ContinuationAttemptID != claimed.AttemptID ||
		reported.ContinuationHarness != "codex" || reported.ContinuationLaunchEnvironment != "worker-a/env-1" || reported.CanResumeContinuation() {
		t.Fatalf("first capture=%+v", reported)
	}
	args["continuation_session_id"] = "native-2"
	result, err = server.callMCPTool(workerRequest, "report_continuation", args)
	if err != nil || result.(core.WorkOrder).ContinuationSessionID != "native-2" {
		t.Fatalf("replacement=%+v err=%v", result, err)
	}
	beforeEvents, _ := st.ListEvents(ctx, task.ID)
	args["attempt_id"] = "wrong-attempt"
	if _, err = server.callMCPTool(workerRequest, "report_continuation", args); err == nil || !strings.Contains(err.Error(), "active work-order attempt") {
		t.Fatalf("wrong attempt error=%v", err)
	}
	after, getErr := st.GetWorkOrder(ctx, job.ID)
	afterEvents, _ := st.ListEvents(ctx, task.ID)
	if getErr != nil || after.State != core.WorkOrderClaimed || after.SessionID != claimed.SessionID ||
		after.ContinuationSessionID != "native-2" || len(afterEvents) != len(beforeEvents) {
		t.Fatalf("failed report changed lifecycle or capture: order=%+v events=%d/%d err=%v", after, len(beforeEvents), len(afterEvents), getErr)
	}

	runTask := core.Task{ID: "mcp-run-continuation", Workspace: "demo", State: core.TaskRunning, CreatedAt: time.Now()}
	runJob := core.Job{ID: runTask.ID + "-implement-1", TaskID: runTask.ID, Stage: core.StageImplement, State: core.JobPending}
	if err = st.CreateTask(ctx, runTask); err != nil {
		t.Fatal(err)
	}
	if err = st.CreateJob(ctx, runJob); err != nil {
		t.Fatal(err)
	}
	if err = storetest.For(st).CreateWorkOrder(ctx, core.WorkOrder{ID: runJob.ID, TaskID: runTask.ID, JobID: runJob.ID, Stage: core.StageImplement}); err != nil {
		t.Fatal(err)
	}
	runClaim, err := storetest.For(st).ClaimWorkOrder(ctx, runJob.ID, core.WorkOrderClaim{
		SessionID: "run-session", ClientToken: "run-secret", ClaimantID: core.TaskRunClaimantID("owner-a"), OwnerUserID: "owner-a", Agent: "codex", Lease: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	runRequest := request.WithContext(store.WithCredential(request.Context(), core.AuthenticatedCredential{
		ID: "user", OwnerUserID: "owner-a", Kind: core.CredentialUser,
	}))
	runArgs := map[string]any{
		"workspace_id": "demo", "work_order_id": runJob.ID, "session_id": runClaim.SessionID,
		"continuation_session_id": "native-run", "attempt_id": runClaim.AttemptID,
		"harness": "codex", "launch_environment": "laptop-a",
	}
	if result, err = server.callMCPTool(runRequest, "report_continuation", runArgs); err != nil || result.(core.WorkOrder).ContinuationSessionID != "native-run" {
		t.Fatalf("conveyor run capture=%+v err=%v", result, err)
	}
}

func TestMCPWorkerFallbackDoesNotReplaceAgentUsage(t *testing.T) {
	t.Parallel()
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	task := core.Task{ID: "mcp-worker-fallback", Workspace: "demo", State: core.TaskRunning, CreatedAt: time.Now()}
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
	if _, err := storetest.For(st).ClaimWorkOrder(ctx, job.ID, core.WorkOrderClaim{
		SessionID: "session", ClientToken: "secret", ClaimantID: "worker", WorkerID: "worker", Lease: time.Minute,
	}); err != nil {
		t.Fatal(err)
	}
	server := NewServer(st)
	server.Workspace = "demo"
	server.WorkOrders = &workorder.Service{Store: st}
	request := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	args := map[string]any{
		"workspace_id": "demo", "work_order_id": job.ID, "session_id": "session",
		"tokens_in": 100.0, "tokens_out": 25.0, "cost_usd": 0.5,
	}
	workerRequest := request.WithContext(context.WithValue(request.Context(), workerContextKey{}, core.Worker{ID: "worker", Workspace: "demo"}))
	fallback := maps.Clone(args)
	fallback["tokens_in"] = 500.0
	fallback["tokens_out"] = 125.0
	fallback["cost_usd"] = 0.0
	fallback["source"] = "worker_fallback"
	result, err := server.callMCPTool(workerRequest, "report_usage", fallback)
	if err != nil {
		t.Fatal(err)
	}
	reported := result.(core.WorkOrder)
	if reported.TokensIn != 500 || reported.TokensOut != 125 || reported.CostUSD != 0 || !reported.UsageReported || reported.SelfReported {
		t.Fatalf("worker fallback was not classified as fallback usage: %+v", reported)
	}
	result, err = server.callMCPTool(workerRequest, "report_usage", args)
	if err != nil {
		t.Fatal(err)
	}
	reported = result.(core.WorkOrder)
	if reported.TokensIn != 100 || reported.TokensOut != 25 || reported.CostUSD != 0.5 || !reported.UsageReported || !reported.SelfReported {
		t.Fatalf("self-reported usage was not claimant-bound agent usage: %+v", reported)
	}
	fallback["tokens_in"] = 700.0
	fallback["tokens_out"] = 175.0
	result, err = server.callMCPTool(workerRequest, "report_usage", fallback)
	if err != nil {
		t.Fatal(err)
	}
	reported = result.(core.WorkOrder)
	if reported.TokensIn != 100 || reported.TokensOut != 25 || reported.CostUSD != 0.5 || !reported.UsageReported || !reported.SelfReported {
		t.Fatalf("fallback replaced agent usage: %+v", reported)
	}
}

func TestMCPUsageSurfacesForImplementationAndReviewOrders(t *testing.T) {
	t.Parallel()
	for _, stage := range []core.Stage{core.StageImplement, core.StageReview} {
		stage := stage
		t.Run(string(stage), func(t *testing.T) {
			t.Parallel()
			ctx := store.WithWorkspace(t.Context(), "demo")
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
			request = request.WithContext(store.WithCredential(request.Context(), core.AuthenticatedCredential{ID: "owner-token", OwnerUserID: "owner", Kind: core.CredentialUser}))
			result, err := server.callMCPTool(request, "report_usage", map[string]any{
				"workspace_id": "demo", "work_order_id": job.ID, "session_id": "session",
				"tokens_in": 1200.0, "tokens_out": 300.0, "cost_usd": 1.25,
			})
			if err != nil {
				t.Fatal(err)
			}
			reported := result.(core.WorkOrder)
			if reported.TokensIn != 1200 || reported.TokensOut != 300 || reported.CostUSD != 1.25 || !reported.UsageReported || !reported.SelfReported {
				t.Fatalf("reported %s usage=%+v", stage, reported)
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

func TestMCPWorkerListIncludesOnlyOwnActiveOrdersAndClaimableWork(t *testing.T) {
	t.Parallel()
	ctx := store.WithWorkspace(t.Context(), "demo")
	st := store.NewMemory()
	now := time.Now().UTC()
	cfg := &config.Config{
		Workspace: "demo",
		Harnesses: []config.Harness{{Name: "codex"}},
		Routing: config.Routing{Stages: map[string]config.StageRoute{
			"implement": {Harness: "codex"},
			"review":    {Execution: config.ExecutionInProcess},
		}},
		Repos: []config.Repo{{Name: "api", Base: "main"}},
	}
	provider := func(context.Context) (*config.Config, error) { return cfg, nil }
	workOrders := &workorder.Service{Store: st, ConfigProvider: provider}
	workers := &workerservice.Service{Store: st, WorkOrders: workOrders, ConfigProvider: provider, Now: func() time.Time { return now }}
	worker := core.Worker{
		ID:             "worker-owner",
		Workspace:      "demo",
		LeaseExpiresAt: now.Add(time.Minute),
		Probes:         []core.HarnessProbe{{Harness: "codex", Healthy: true, CheckedAt: now}},
	}

	createOrder := func(id string, stage core.Stage) core.WorkOrder {
		t.Helper()
		task := core.Task{ID: id, Workspace: "demo", Repo: "api", State: core.TaskRunning, CreatedAt: now}
		if err := st.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
		job := core.Job{ID: id + "-" + string(stage) + "-1", TaskID: id, Stage: stage, State: core.JobPending}
		if err := st.CreateJob(ctx, job); err != nil {
			t.Fatal(err)
		}
		order := core.WorkOrder{ID: job.ID, TaskID: id, JobID: job.ID, Stage: stage, State: core.WorkOrderQueued, QueueEnteredAt: now, QueueDeadline: now.Add(time.Hour)}
		if err := storetest.For(st).CreateWorkOrder(ctx, order); err != nil {
			t.Fatal(err)
		}
		created, err := st.GetWorkOrder(ctx, order.ID)
		if err != nil {
			t.Fatal(err)
		}
		return created
	}
	claim := func(order core.WorkOrder, workerID string, executionTimeout time.Duration) core.WorkOrder {
		t.Helper()
		claimed, err := storetest.For(st).ClaimWorkOrder(ctx, order.ID, core.WorkOrderClaim{
			SessionID: order.ID + "-session", ClientToken: order.ID + "-token",
			ClaimantID: workerID, WorkerID: workerID, Lease: time.Minute, ExecutionTimeout: executionTimeout,
		})
		if err != nil {
			t.Fatal(err)
		}
		return claimed
	}
	transition := func(order core.WorkOrder, state core.WorkOrderState, command core.WorkOrderCommand) {
		t.Helper()
		order.State = state
		if err := storetest.For(st).UpdateWorkOrder(ctx, order, command); err != nil {
			t.Fatal(err)
		}
	}

	claimable := createOrder("claimable", core.StageImplement)
	ownClaimed := claim(createOrder("own-claimed", core.StageImplement), worker.ID, time.Hour)
	ownSubmitted := claim(createOrder("own-submitted", core.StageImplement), worker.ID, time.Hour)
	transition(ownSubmitted, core.WorkOrderSubmitted, core.WorkOrderCmdSubmitForReview)
	claim(createOrder("other-claimed", core.StageImplement), "worker-other", time.Hour)
	otherSubmitted := claim(createOrder("other-submitted", core.StageImplement), "worker-other", time.Hour)
	transition(otherSubmitted, core.WorkOrderSubmitted, core.WorkOrderCmdSubmitForReview)
	ownCompleted := claim(createOrder("own-completed", core.StageSpec), worker.ID, time.Hour)
	transition(ownCompleted, core.WorkOrderCompleted, core.WorkOrderCmdSubmitSpec)
	ownCancelled := claim(createOrder("own-cancelled", core.StageImplement), worker.ID, time.Hour)
	transition(ownCancelled, core.WorkOrderCancelled, core.WorkOrderCmdCancel)
	claim(createOrder("own-timed-out", core.StageImplement), worker.ID, time.Nanosecond)
	stale := createOrder("stale", core.StageImplement)
	stale.QueueDeadline = now.Add(-time.Minute)
	if err := storetest.For(st).UpdateWorkOrder(ctx, stale); err != nil {
		t.Fatal(err)
	}

	server := NewServer(st)
	server.Workspace, server.ConfigProvider, server.WorkOrders, server.Workers = "demo", provider, workOrders, workers
	request := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	request = request.WithContext(context.WithValue(request.Context(), workerContextKey{}, worker))
	result, err := server.callMCPTool(request, "list_work_orders", map[string]any{"workspace_id": "demo"})
	if err != nil {
		t.Fatal(err)
	}
	listed := result.([]core.WorkOrder)
	got := make(map[string]core.WorkOrderState, len(listed))
	for _, order := range listed {
		got[order.ID] = order.State
	}
	want := map[string]core.WorkOrderState{
		claimable.ID:    core.WorkOrderQueued,
		ownClaimed.ID:   core.WorkOrderClaimed,
		ownSubmitted.ID: core.WorkOrderSubmitted,
	}
	if !maps.Equal(got, want) {
		t.Fatalf("listed=%v want=%v", got, want)
	}
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
	want := []string{"create_task", "set_assignee", "list_work_orders", "claim_work_order", "redispatch_work_order", "renew_work_order", "release_work_order", "request_plan_revision", "get_work_order", "read_artifact", "report_progress", "report_usage", "report_continuation", "propose_system_design_revision", "propose_requirement_revision", "propose_decision", "upload_transcript", "submit_plan", "submit_for_review", "await_review", "submit_review_verdict"}
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
		if name == "propose_system_design_revision" || name == "propose_requirement_revision" || name == "propose_decision" {
			description := envelope.Result.Tools[i].Description
			if !strings.Contains(description, "operator alone confirms after submission") || !strings.Contains(description, "confirmation never blocks implementation") {
				t.Fatalf("%s description=%q", name, description)
			}
		}
		if name == "claim_work_order" {
			description := envelope.Result.Tools[i].Description
			if !strings.Contains(description, "claimant_id") || !strings.Contains(description, "ignored") || !strings.Contains(description, "authenticated credential") {
				t.Fatalf("claim_work_order description=%q", description)
			}
		}
	}
}

func TestMCPAgentCredentialCannotInvokeHumanReservedTools(t *testing.T) {
	server := NewServer(store.NewMemory())
	request := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	credential := core.AuthenticatedCredential{ID: "agt_1", OwnerUserID: "usr_1", Kind: core.CredentialAgent, Scope: core.CredentialScopeUser}
	request = request.WithContext(store.WithCredential(request.Context(), credential))
	if err := validateMCPAgentSafety(mcpAgentSafeReasons); err != nil {
		t.Fatal(err)
	}
	for _, tool := range mcpTools() {
		name, _ := tool["name"].(string)
		if _, agentSafe := mcpAgentSafeReasons[name]; agentSafe {
			continue
		}
		if _, err := server.callMCPTool(request, name, map[string]any{"workspace_id": "demo"}); err == nil || !strings.Contains(err.Error(), "operator-scoped user credential") {
			t.Fatalf("%s error=%v", name, err)
		}
	}
}

func TestMCPHumanReservedClassificationRejectsOmittedReservedTool(t *testing.T) {
	mutated := maps.Clone(mcpAgentSafeReasons)
	// Reclassifying create_task as agent-safe removes it from the reserved set
	// derived by exclusion. The independent production guard must reject that
	// mutation, proving this test does not share production's literal list.
	mutated["create_task"] = "mutation: agent may create durable operator work"
	if err := validateMCPAgentSafety(mutated); err == nil || !strings.Contains(err.Error(), "create_task") {
		t.Fatalf("reserved-set mutation error=%v", err)
	}
}

var mcpAgentSafeReasons = map[string]string{
	"list_work_orders":               "read-only discovery of work the caller may claim",
	"claim_work_order":               "begins only an eligible bounded execution lease",
	"renew_work_order":               "extends only the caller's exact execution lease",
	"release_work_order":             "releases only the caller's exact execution lease",
	"request_plan_revision":          "requests an operator-gated plan decision without deciding it",
	"get_work_order":                 "reads only context authorized for the caller's claimed order",
	"read_artifact":                  "reads only an artifact authorized by claimed-order context",
	"report_progress":                "records self-reported progress only after claimant-bound admission",
	"report_usage":                   "records self-reported observational usage only after claimant-bound admission",
	"propose_system_design_revision": "creates an unconfirmed proposal that grants no authority",
	"propose_requirement_revision":   "creates an unconfirmed proposal that grants no authority",
	"propose_decision":               "creates an unconfirmed proposal that grants no authority",
	"upload_transcript":              "attaches redacted evidence only after claimant-bound admission",
	"submit_plan":                    "submits a plan-stage deliverable only after claimant-bound admission",
	"submit_for_review":              "submits an implementation only after claimant-bound admission",
	"await_review":                   "observes review state only after claimant-bound admission",
	"submit_review_verdict":          "acts only after claimant-bound admission to an independently claimed review order",
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
	for name := range mcpCapabilities {
		if !seen[name] {
			t.Fatalf("capability mapping names unregistered MCP tool %q", name)
		}
	}
}

func TestMCPClaimantBoundToolsRejectForeignUsersAndWorkers(t *testing.T) {
	tools := []string{"report_progress", "report_usage", "upload_transcript", "submit_plan", "submit_for_review", "await_review", "submit_review_verdict"}
	for _, tool := range tools {
		tool := tool
		t.Run(tool, func(t *testing.T) {
			setup := func(t *testing.T, workerID string) (*Server, string) {
				t.Helper()
				ctx := store.WithWorkspace(t.Context(), "demo")
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
				claim := core.WorkOrderClaim{SessionID: "victim-session", ClientToken: "secret", Lease: time.Minute}
				if workerID == "" {
					claim.ClaimantID = core.TaskRunClaimantID("owner")
				} else {
					claim.ClaimantID, claim.WorkerID = workerID, workerID
				}
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
					"workspace_id": "demo", "work_order_id": orderID, "session_id": "victim-session",
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

			userServer, userOrderID := setup(t, "")
			foreignUser := httptest.NewRequest(http.MethodPost, "/mcp", nil)
			foreignUser = foreignUser.WithContext(store.WithCredential(foreignUser.Context(), core.AuthenticatedCredential{ID: "foreign", OwnerUserID: "foreign", Kind: core.CredentialUser}))
			if _, err := userServer.callMCPTool(foreignUser, tool, args(userOrderID)); !errors.Is(err, store.ErrWorkOrderClaimUnauthorized) {
				t.Fatalf("foreign user error=%v", err)
			}

			owner := httptest.NewRequest(http.MethodPost, "/mcp", nil)
			owner = owner.WithContext(store.WithCredential(owner.Context(), core.AuthenticatedCredential{ID: "owner", OwnerUserID: "owner", Kind: core.CredentialUser}))
			if _, err := userServer.callMCPTool(owner, tool, args(userOrderID)); err != nil {
				t.Fatalf("owning user call failed: %v", err)
			}

			workerServer, workerOrderID := setup(t, "worker-a")
			foreignWorker := httptest.NewRequest(http.MethodPost, "/mcp", nil)
			foreignWorker = foreignWorker.WithContext(context.WithValue(foreignWorker.Context(), workerContextKey{}, core.Worker{ID: "worker-b", Workspace: "demo"}))
			if _, err := workerServer.callMCPTool(foreignWorker, tool, args(workerOrderID)); !errors.Is(err, store.ErrWorkOrderClaimLost) {
				t.Fatalf("foreign worker error=%v", err)
			}
			owningWorker := httptest.NewRequest(http.MethodPost, "/mcp", nil)
			owningWorker = owningWorker.WithContext(context.WithValue(owningWorker.Context(), workerContextKey{}, core.Worker{ID: "worker-a", Workspace: "demo"}))
			if _, err := workerServer.callMCPTool(owningWorker, tool, args(workerOrderID)); err != nil {
				t.Fatalf("owning worker call failed: %v", err)
			}
		})
	}
}

func TestWorkOrderListSurfacesRedactForeignSessionIDs(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
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
	ownerRequest = ownerRequest.WithContext(store.WithCredential(ownerRequest.Context(), core.AuthenticatedCredential{ID: "owner", OwnerUserID: "owner", Kind: core.CredentialUser}))
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
	restRequest = restRequest.WithContext(store.WithCredential(store.WithWorkspace(restRequest.Context(), "demo"), core.AuthenticatedCredential{ID: "owner", OwnerUserID: "owner", Kind: core.CredentialUser}))
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

func validateMCPAgentSafety(agentSafe map[string]string) error {
	seen := make(map[string]bool)
	for _, tool := range mcpTools() {
		name, _ := tool["name"].(string)
		seen[name] = true
		reason, safe := agentSafe[name]
		reserved := humanReservedMCPTool(name)
		switch {
		case safe && strings.TrimSpace(reason) == "":
			return fmt.Errorf("agent-safe MCP tool %s lacks a security justification", name)
		case safe && reserved:
			return fmt.Errorf("human-reserved MCP tool %s was omitted from the reserved set by agent-safe classification", name)
		case !safe && !reserved:
			return fmt.Errorf("registered MCP tool %s is neither justified as agent-safe nor guarded as human-reserved", name)
		}
	}
	for name := range agentSafe {
		if !seen[name] {
			return fmt.Errorf("agent-safe justification names unregistered MCP tool %s", name)
		}
	}
	return nil
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
	setup := func(t *testing.T, stage core.Stage, approved, claimed bool) fixture {
		t.Helper()
		ctx := store.WithWorkspace(t.Context(), "demo")
		st := store.NewMemory()
		taskID := "plan-revision-" + core.NewTaskID()
		now := time.Now().UTC()
		if err := st.CreateTask(ctx, core.Task{ID: taskID, Workspace: "demo", Repo: "conveyor", State: core.TaskRunning, NextStage: stage, CreatedAt: now}); err != nil {
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
			if _, err := storetest.For(st).ClaimWorkOrder(ctx, orderID, core.WorkOrderClaim{SessionID: "session-a", ClientToken: "secret", WorkerID: "worker-a", Agent: "codex", Model: "model", Lease: time.Minute, ExecutionTimeout: time.Hour}); err != nil {
				t.Fatal(err)
			}
		}
		server := NewServer(st)
		server.Workspace = "demo"
		server.WorkOrders = &workorder.Service{Store: st}
		server.Workers = &workerservice.Service{Store: st, WorkOrders: server.WorkOrders}
		request := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		request = request.WithContext(context.WithValue(request.Context(), workerContextKey{}, core.Worker{ID: "worker-a", Workspace: "demo"}))
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

	for _, test := range []struct {
		name                 string
		stage                core.Stage
		approved, claimed    bool
		sessionID, rationale string
	}{
		{name: "blank rationale", stage: core.StageImplement, approved: true, claimed: true, sessionID: "session-a", rationale: " "},
		{name: "wrong stage", stage: core.StageReview, approved: true, claimed: true, sessionID: "session-a", rationale: "wrong"},
		{name: "missing approved plan", stage: core.StageImplement, claimed: true, sessionID: "session-a", rationale: "wrong"},
		{name: "stale session", stage: core.StageImplement, approved: true, claimed: true, sessionID: "stale", rationale: "wrong"},
		{name: "unclaimed", stage: core.StageImplement, approved: true, sessionID: "session-a", rationale: "wrong"},
	} {
		t.Run(test.name+" is in-band and non-mutating", func(t *testing.T) {
			item := setup(t, test.stage, test.approved, test.claimed)
			item.args["session_id"], item.args["rationale"] = test.sessionID, test.rationale
			beforeOrder, _ := item.store.GetWorkOrder(store.WithWorkspace(t.Context(), "demo"), item.args["work_order_id"].(string))
			beforeTask, _ := item.store.GetTask(store.WithWorkspace(t.Context(), "demo"), item.taskID)
			beforeEvents, _ := item.store.ListEvents(store.WithWorkspace(t.Context(), "demo"), item.taskID)
			if _, err := item.server.callMCPTool(item.request, "request_plan_revision", item.args); err == nil {
				t.Fatal("request unexpectedly succeeded")
			}
			afterOrder, _ := item.store.GetWorkOrder(store.WithWorkspace(t.Context(), "demo"), item.args["work_order_id"].(string))
			afterTask, _ := item.store.GetTask(store.WithWorkspace(t.Context(), "demo"), item.taskID)
			afterEvents, _ := item.store.ListEvents(store.WithWorkspace(t.Context(), "demo"), item.taskID)
			if beforeOrder.State != afterOrder.State || beforeOrder.SessionID != afterOrder.SessionID || beforeTask.State != afterTask.State || len(beforeEvents) != len(afterEvents) {
				t.Fatalf("request mutated rejected projection")
			}
		})
	}
}

func TestMCPWorkerDispatchedExecutorClaimGovernance(t *testing.T) {
	t.Parallel()
	setup := func(t *testing.T, ownLease time.Duration) (*Server, *http.Request, *membershipFixture, string, string, string) {
		t.Helper()
		ctx := store.WithWorkspace(t.Context(), "demo")
		st := store.NewMemory()
		for _, worker := range []core.Worker{
			{ID: "worker-a", Workspace: "demo", OwnerUserID: "owner-a", Name: "Worker A", CredentialHash: "hash-a", CreatedAt: time.Now()},
			{ID: "worker-b", Workspace: "demo", OwnerUserID: "owner-b", Name: "Worker B", CredentialHash: "hash-b", CreatedAt: time.Now()},
		} {
			if err := st.CreateWorker(ctx, worker); err != nil {
				t.Fatal(err)
			}
		}
		var orderIDs []string
		for _, suffix := range []string{"a", "b"} {
			taskID, orderID := "agent-claim-task-"+suffix, "agent-claim-order-"+suffix
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
			if _, err = storetest.For(st).ClaimWorkOrder(ctx, orderID, core.WorkOrderClaim{SessionID: "session-" + suffix, ClientToken: "secret-" + suffix, ClaimantID: "worker-" + suffix, WorkerID: "worker-" + suffix, Agent: "codex", Model: "model", Lease: lease, ExecutionTimeout: time.Hour}); err != nil {
				t.Fatal(err)
			}
			orderIDs = append(orderIDs, orderID)
		}
		document, _, err := st.CreateSystemDesign(ctx, core.SystemDesign{ID: "design-agent-claim", Title: "Agent claim", Category: "Architecture"}, core.SystemDesignVersion{Content: "# Agent claim\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/httpapi/**\n```", Origin: core.SystemDesignOriginOperator})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err = st.CreateRequirement(ctx, core.Requirement{ID: "req-agent-claim", Title: "Agent claim"}, core.RequirementVersion{Content: "# Agent claim\n\n```conveyor:requirements\n- id: REQ-1\n  statement: Keep claims exact.\n```", Statements: []core.RequirementStatement{{ID: "REQ-1", Statement: "Keep claims exact."}}, Origin: core.RequirementOriginOperator}); err != nil {
			t.Fatal(err)
		}
		server := NewServer(st)
		server.Workspace = "demo"
		server.WorkOrders = &workorder.Service{Store: st}
		server.Workers = &workerservice.Service{Store: st, WorkOrders: server.WorkOrders}
		membership := &membershipFixture{
			workspaces: []core.Workspace{{ID: "demo"}},
			roles: map[string]map[string]core.WorkspaceRole{
				"owner-a": {"demo": core.WorkspaceRoleExecutor},
				"owner-b": {"demo": core.WorkspaceRoleExecutor},
			},
		}
		server.Workspaces, server.Memberships = membership, membership
		request := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		request = request.WithContext(store.WithCredential(request.Context(), core.AuthenticatedCredential{ID: "agent-a", OwnerUserID: "owner-a", Kind: core.CredentialAgent, Scope: core.CredentialScopeUser}))
		if ownLease <= time.Nanosecond {
			time.Sleep(time.Millisecond)
		}
		return server, request, membership, orderIDs[0], orderIDs[1], document.ID
	}

	argsFor := func(tool, orderID, sessionID, documentID string) map[string]any {
		args := map[string]any{"workspace_id": "demo", "work_order_id": orderID, "session_id": sessionID, "rationale": "the approved plan conflicts with the API"}
		switch tool {
		case "propose_system_design_revision":
			args["document_id"] = documentID
			args["content"] = "# Agent claim revision\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/httpapi/**\n```"
		case "propose_requirement_revision":
			args["document_id"] = "req-agent-claim"
			args["content"] = "# Agent claim\n\n```conveyor:requirements\n- id: REQ-1\n  statement: Keep executor claims exact.\n```"
		case "propose_decision":
			args["statement"] = "Keep worker claims exact."
			args["context"] = "Worker claims may propose task-local governance."
			args["alternatives_rejected"] = "Grant freestanding proposal authority."
		}
		return args
	}
	governanceTools := []string{"request_plan_revision", "propose_system_design_revision", "propose_requirement_revision", "propose_decision"}

	t.Run("own live claim authorizes all governance tools as executor", func(t *testing.T) {
		for _, tool := range governanceTools {
			t.Run(tool, func(t *testing.T) {
				server, request, membership, ownOrder, _, documentID := setup(t, time.Minute)
				if _, err := server.callMCPTool(request, tool, argsFor(tool, ownOrder, "session-a", documentID)); err != nil {
					t.Fatal(err)
				}
				if len(membership.capabilityCalls) != 1 || membership.capabilityCalls[0] != core.CapabilityViewWorkspace {
					t.Fatalf("membership capabilities=%v", membership.capabilityCalls)
				}
			})
		}
	})

	t.Run("different order is refused for all governance tools", func(t *testing.T) {
		for _, tool := range governanceTools {
			t.Run(tool, func(t *testing.T) {
				server, request, _, _, otherOrder, documentID := setup(t, time.Minute)
				if _, err := server.callMCPTool(request, tool, argsFor(tool, otherOrder, "session-a", documentID)); !errors.Is(err, store.ErrWorkOrderClaimUnauthorized) {
					t.Fatalf("cross-order error=%v", err)
				}
			})
		}
	})

	t.Run("expired claim is refused for all governance tools", func(t *testing.T) {
		for _, tool := range governanceTools {
			t.Run(tool, func(t *testing.T) {
				server, request, _, ownOrder, _, documentID := setup(t, time.Nanosecond)
				if _, err := server.callMCPTool(request, tool, argsFor(tool, ownOrder, "session-a", documentID)); !errors.Is(err, store.ErrWorkOrderClaimLost) {
					t.Fatalf("expired-claim error=%v", err)
				}
			})
		}
	})

	t.Run("non-governance release remains claim-bound", func(t *testing.T) {
		server, request, _, ownOrder, _, _ := setup(t, time.Minute)
		args := map[string]any{"workspace_id": "demo", "work_order_id": ownOrder, "session_id": "session-a", "reason": "agent handoff"}
		if _, err := server.callMCPTool(request, "release_work_order", args); err != nil {
			t.Fatal(err)
		}
	})
}

func TestMCPUserRunExecutorClaimGovernance(t *testing.T) {
	t.Parallel()
	setup := func(t *testing.T, ownLease time.Duration) (*Server, *http.Request, *membershipFixture, string, string, string) {
		t.Helper()
		ctx := store.WithWorkspace(t.Context(), "demo")
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
		document, _, err := st.CreateSystemDesign(ctx, core.SystemDesign{ID: "design-run-claim", Title: "Run", Category: "Architecture"}, core.SystemDesignVersion{Content: "# Run\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/httpapi/**\n```", Origin: core.SystemDesignOriginOperator})
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
		server.Workers = &workerservice.Service{Store: st, WorkOrders: server.WorkOrders}
		membership := &membershipFixture{
			workspaces: []core.Workspace{{ID: "demo"}},
			roles: map[string]map[string]core.WorkspaceRole{
				"user-a": {"demo": core.WorkspaceRoleExecutor},
				"user-b": {"demo": core.WorkspaceRoleExecutor},
			},
		}
		server.Workspaces, server.Memberships = membership, membership
		request := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		request = request.WithContext(store.WithCredential(request.Context(), core.AuthenticatedCredential{ID: "user-token", OwnerUserID: "user-a", Kind: core.CredentialUser, Scope: core.CredentialScopeUser}))
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
		return server, request, membership, orderIDs[0], orderIDs[1], document.ID
	}
	argsFor := func(tool, orderID, sessionID, documentID string) map[string]any {
		args := map[string]any{"workspace_id": "demo", "work_order_id": orderID, "session_id": sessionID, "rationale": "plan changed"}
		switch tool {
		case "propose_system_design_revision":
			args["document_id"] = documentID
			args["content"] = "# Run claim revision\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/httpapi/**\n```"
		case "propose_requirement_revision":
			args["document_id"] = "req-run-claim"
			args["content"] = "# Run claim\n\n```conveyor:requirements\n- id: REQ-1\n  statement: Keep user run claims exact.\n```"
		case "propose_decision":
			args["statement"] = "Keep run claims exact."
			args["context"] = "Run claims may propose task-local governance."
			args["alternatives_rejected"] = "Grant freestanding proposal authority."
		}
		return args
	}
	governanceTools := []string{"request_plan_revision", "propose_system_design_revision", "propose_requirement_revision", "propose_decision"}
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
					server, request, membership, ownOrder, otherOrder, documentID := setup(t, test.lease)
					if test.otherCredential {
						request = request.WithContext(store.WithCredential(request.Context(), core.AuthenticatedCredential{ID: "other-token", OwnerUserID: "user-b", Kind: core.CredentialUser, Scope: core.CredentialScopeUser}))
					}
					target := ownOrder
					if test.otherOrder {
						target = otherOrder
					}
					_, err := server.callMCPTool(request, tool, argsFor(tool, target, "run-session-a", documentID))
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
					if len(membership.capabilityCalls) != 1 || membership.capabilityCalls[0] != core.CapabilityViewWorkspace {
						t.Fatalf("membership capabilities=%v", membership.capabilityCalls)
					}
				})
			}
		})
	}
}

func TestMCPClaimedPlanSubmissionStillUsesSpecLifecycle(t *testing.T) {
	ctx := store.WithWorkspace(t.Context(), "demo")
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
	request = request.WithContext(store.WithCredential(request.Context(), core.AuthenticatedCredential{ID: "agent-token", OwnerUserID: "usr-planner", Kind: core.CredentialAgent, Scope: core.CredentialScopeUser}))
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

// A nil Go slice marshals to `"required": null`, which the official MCP
// SDK's validation rejects — taking every tool down with it as a
// "tools fetch failed" at connection time.
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
		payload, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "create_task", "arguments": map[string]any{"body": taskBody, "repo": "api", "source": "mcp:test-issue", "hold": true, "spec_approval": true, "merge_approval": true, "idempotency_key": "issue-42"}}})
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
	tasks, err := st.ListTasks(t.Context())
	if err != nil || len(tasks) != 1 {
		t.Fatalf("tasks=%+v err=%v", tasks, err)
	}
	events, err := st.ListEvents(t.Context(), first.ID)
	if err != nil || len(events) != 1 || events[0].ActorID != "user:local-operator" || events[0].ActorRole != core.ActorUser {
		t.Fatalf("events=%+v err=%v", events, err)
	}
}

func TestMCPDependencyValidationPrecedesTitleAndIdempotencyIsSymmetric(t *testing.T) {
	st := store.NewMemory()
	ctx := store.WithWorkspace(t.Context(), "demo")
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
		Execution: config.ExecutionPolicy{DefaultMode: "auto", SpecApproval: true, MergeApproval: true},
		Harnesses: []config.Harness{{Name: "codex"}},
		Routing: config.Routing{Stages: map[string]config.StageRoute{
			"implement": {Harness: "codex"},
			"review":    {Harness: "codex", Execution: config.ExecutionMCP},
		}},
		Repos: []config.Repo{{Name: "api", Base: "main"}},
	}
	provider := func(context.Context) (*config.Config, error) { return cfg, nil }
	workers := &workerservice.Service{Store: st, ConfigProvider: provider, Now: func() time.Time { return now }}
	ctx := store.WithWorkspace(t.Context(), "demo")
	if err := st.CreateWorker(ctx, core.Worker{ID: "worker", Workspace: "demo", Name: "worker", CredentialHash: "hash", LeaseExpiresAt: now.Add(15 * time.Second), Probes: []core.HarnessProbe{{Harness: "codex", Healthy: true}}, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	server := NewServer(st)
	server.Workspace, server.ConfigProvider, server.Workers = "demo", provider, workers
	server.GenerateTaskTitle = func(context.Context, core.Task) (string, error) { return "Stable retry", nil }
	request := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	arguments := map[string]any{"workspace_id": "demo", "body": "Keep retry policy stable", "repo": "api", "idempotency_key": "stable-policy"}
	firstResult, err := server.callMCPTool(request, "create_task", arguments)
	if err != nil {
		t.Fatal(err)
	}
	first := firstResult.(map[string]any)["task"].(core.Task)
	if first.Mode != "" || first.Hold || !first.SpecApproval || !first.MergeApproval {
		t.Fatalf("first=%+v", first)
	}

	// Both live availability and omitted workspace gate defaults change after
	// intake. An exact retry must still return the persisted resolution.
	now = now.Add(time.Minute)
	cfg.Execution.SpecApproval = false
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
	if _, err := server.resolveMCPWorkspace(t.Context(), ""); err == nil || !strings.Contains(err.Error(), "workspace_unavailable") {
		t.Fatalf("zero-workspace omission error = %v", err)
	}
	if _, err := server.resolveMCPWorkspace(t.Context(), "unknown"); err == nil || !strings.Contains(err.Error(), "workspace_not_found") {
		t.Fatalf("zero-workspace explicit error = %v", err)
	}

	server.Workspace = "alpha"
	if got, err := server.resolveMCPWorkspace(t.Context(), ""); err != nil || got != "alpha" {
		t.Fatalf("singleton omission = %q, %v", got, err)
	}
	if got, err := server.resolveMCPWorkspace(t.Context(), "alpha"); err != nil || got != "alpha" {
		t.Fatalf("singleton explicit = %q, %v", got, err)
	}
	if _, err := server.resolveMCPWorkspace(t.Context(), "beta"); err == nil || !strings.Contains(err.Error(), "workspace_not_found") {
		t.Fatalf("unknown singleton explicit error = %v", err)
	}

	server.Deployment = &config.Config{Workspace: "beta"}
	if _, err := server.resolveMCPWorkspace(t.Context(), ""); err == nil || !strings.Contains(err.Error(), "workspace_required") {
		t.Fatalf("ambiguous omission error = %v", err)
	}
}
