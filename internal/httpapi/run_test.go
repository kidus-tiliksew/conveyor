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
	"github.com/kidus-tiliksew/conveyor/internal/store"
	"github.com/kidus-tiliksew/conveyor/internal/store/storetest"
	"github.com/kidus-tiliksew/conveyor/internal/taskops"
	workerservice "github.com/kidus-tiliksew/conveyor/internal/worker"
	"github.com/kidus-tiliksew/conveyor/internal/workorder"
)

func taskRunHTTPFixture(t *testing.T) (*Server, store.Store, http.Handler) {
	t.Helper()
	st := store.NewMemory()
	cfg := &config.Config{
		Workspace: "demo",
		Routing: config.Routing{Stages: map[string]config.StageRoute{
			"spec":      {Execution: config.ExecutionMCP, Timeout: time.Hour, TimeoutText: "1h"},
			"implement": {Execution: config.ExecutionMCP, Timeout: time.Hour, TimeoutText: "1h"},
			"review":    {Execution: config.ExecutionMCP, Timeout: time.Hour, TimeoutText: "1h"},
		}},
		Repos: []config.Repo{{Name: "conveyor", URL: "https://example.test/conveyor.git", Base: "main"}},
	}
	provider := func(context.Context) (*config.Config, error) { return cfg, nil }
	orders := &workorder.Service{Store: st, ConfigProvider: provider}
	workers := &workerservice.Service{Store: st, WorkOrders: orders, ConfigProvider: provider}
	server := NewServer(st)
	server.Workspace = "demo"
	server.BearerToken = "user-token"
	server.ConfigProvider = provider
	server.WorkOrders = orders
	server.Workers = workers
	server.AgentCredentials = &taskRunAgentCredentialFixture{}
	return server, st, server.Handler()
}

type taskRunAgentCredentialFixture struct {
	issued  []store.IssuedAgentCredential
	revoked []string
}

func (f *taskRunAgentCredentialFixture) IssueAgentCredential(_ context.Context, userID, label string) (store.IssuedAgentCredential, error) {
	issued := store.IssuedAgentCredential{ID: fmt.Sprintf("agent-%d", len(f.issued)+1), UserID: userID, Label: label, Value: "child-agent-secret"}
	f.issued = append(f.issued, issued)
	return issued, nil
}

func (f *taskRunAgentCredentialFixture) RevokeAgentCredential(_ context.Context, userID, credentialID string) error {
	if userID != "local-operator" {
		return store.ErrNotFound
	}
	f.revoked = append(f.revoked, credentialID)
	return nil
}

func createTaskRunOrder(t *testing.T, st store.Store, taskID string) core.WorkOrder {
	return createTaskRunOrderAtStage(t, st, taskID, core.StageImplement, time.Now().UTC())
}

func createTaskRunOrderAtStage(t *testing.T, st store.Store, taskID string, stage core.Stage, enteredAt time.Time) core.WorkOrder {
	t.Helper()
	ctx := store.WithWorkspace(t.Context(), "demo")
	if _, err := st.GetTask(ctx, taskID); err != nil {
		task := core.Task{ID: taskID, Workspace: "demo", Repo: "conveyor", BaseBranch: "main", Branch: "conveyor/task-" + taskID, State: core.TaskRunning, NextStage: stage, CreatedAt: enteredAt}
		if err = st.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
	}
	job := core.Job{ID: taskID + "-" + string(stage) + "-1", TaskID: taskID, Stage: stage, State: core.JobPending}
	if err := st.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	order := core.WorkOrder{ID: job.ID, TaskID: taskID, JobID: job.ID, Stage: stage, State: core.WorkOrderQueued, QueueEnteredAt: enteredAt, QueueDeadline: enteredAt.Add(time.Hour), CreatedAt: enteredAt}
	if err := storetest.For(st).CreateWorkOrder(ctx, order); err != nil {
		t.Fatal(err)
	}
	return order
}

func TestTaskRunHTTPSelectsSpecImplementReviewInPipelineOrder(t *testing.T) {
	_, st, handler := taskRunHTTPFixture(t)
	now := time.Now().UTC()
	for _, stage := range []core.Stage{core.StageReview, core.StageImplement, core.StageSpec, core.StageTriage, core.StageVerify, core.StageGate, core.StageMerge, core.StageMonitor} {
		createTaskRunOrderAtStage(t, st, "target", stage, now.Add(-time.Duration(taskRunStageOrder(stage))*time.Minute))
	}

	next := taskRunHTTPCall(handler, http.MethodGet, "/v1/tasks/target/run-order", "")
	if next.Code != http.StatusOK || !strings.Contains(next.Body.String(), `"stage":"spec"`) || !strings.Contains(next.Body.String(), `"id":"target-spec-1"`) {
		t.Fatalf("next status=%d body=%s", next.Code, next.Body.String())
	}
}

func taskRunHTTPCall(handler http.Handler, method, path, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer user-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestTaskRunHTTPIsExplicitlyTaskScopedAndUsesUserLeaseLifecycle(t *testing.T) {
	_, st, handler := taskRunHTTPFixture(t)
	target := createTaskRunOrder(t, st, "target")
	createTaskRunOrder(t, st, "other")

	next := taskRunHTTPCall(handler, http.MethodGet, "/v1/tasks/target/run-order", "")
	if next.Code != http.StatusOK || !strings.Contains(next.Body.String(), `"id":"`+target.ID+`"`) || strings.Contains(next.Body.String(), `other-implement-1`) {
		t.Fatalf("next status=%d body=%s", next.Code, next.Body.String())
	}
	claim := taskRunHTTPCall(handler, http.MethodPost, "/v1/tasks/target/run-orders/"+target.ID+"/claim", `{"session_id":"run-session","client_token":"run-secret","agent":"local-codex","model":"local-model"}`)
	if claim.Code != http.StatusOK {
		t.Fatalf("claim status=%d body=%s", claim.Code, claim.Body.String())
	}
	var claimed core.WorkOrder
	if err := json.Unmarshal(claim.Body.Bytes(), &claimed); err != nil {
		t.Fatal(err)
	}
	if claimed.TaskID != "target" || claimed.WorkerID != "" || claimed.ClaimantID != "run:local-operator" || claimed.Agent != "local-codex" || claimed.Model != "local-model" {
		t.Fatalf("claimed=%+v", claimed)
	}
	renew := taskRunHTTPCall(handler, http.MethodPost, "/v1/tasks/target/run-orders/"+target.ID+"/renew", `{"session_id":"run-session"}`)
	if renew.Code != http.StatusOK {
		t.Fatalf("renew status=%d body=%s", renew.Code, renew.Body.String())
	}
	malformedSnapshot := taskRunHTTPCall(handler, http.MethodPost, "/v1/tasks/target/run-orders/"+target.ID+"/renew", `{"session_id":"run-session","activity_snapshot":"malformed"}`)
	if malformedSnapshot.Code != http.StatusOK {
		t.Fatalf("malformed snapshot changed renewal status=%d body=%s", malformedSnapshot.Code, malformedSnapshot.Body.String())
	}
	snapshotRenew := taskRunHTTPCall(handler, http.MethodPost, "/v1/tasks/target/run-orders/"+target.ID+"/renew", `{"session_id":"run-session","activity_snapshot":{"content":"latest output"}}`)
	if snapshotRenew.Code != http.StatusOK {
		t.Fatalf("snapshot renewal status=%d body=%s", snapshotRenew.Code, snapshotRenew.Body.String())
	}
	if snapshot, exists, snapshotErr := st.GetWorkOrderActivitySnapshot(store.WithWorkspace(t.Context(), "demo"), target.ID); snapshotErr != nil || !exists || snapshot.Content != "latest output" {
		t.Fatalf("snapshot=%+v exists=%v err=%v", snapshot, exists, snapshotErr)
	}
	checkpoint := taskRunHTTPCall(handler, http.MethodPost, "/v1/tasks/target/run-orders/"+target.ID+"/attempt-checkpoint", `{"session_id":"run-session","attempt_id":"`+claimed.AttemptID+`","termination_reason":"harness exited","commit_sha":"1111111111111111111111111111111111111111","push_result":"pushed","transcript":"malformed"}`)
	if checkpoint.Code != http.StatusOK || !strings.Contains(checkpoint.Body.String(), `"created":true`) {
		t.Fatalf("malformed transcript changed checkpoint status=%d body=%s", checkpoint.Code, checkpoint.Body.String())
	}
	if captures, captureErr := st.ListWorkOrderTranscriptCaptures(store.WithWorkspace(t.Context(), "demo"), target.ID); captureErr != nil || len(captures) != 0 {
		t.Fatalf("malformed transcript captures=%+v err=%v", captures, captureErr)
	}
	events, err := st.ListEvents(store.WithWorkspace(t.Context(), "demo"), "target")
	if err != nil {
		t.Fatal(err)
	}
	foundUserRenewal := false
	for _, event := range events {
		foundUserRenewal = foundUserRenewal || (event.Kind == "work_order.lease_renewed" && event.ActorID == "user:local-operator" && event.ActorRole == core.ActorUser)
	}
	if !foundUserRenewal {
		t.Fatalf("renewal did not retain credential-derived user actor: %+v", events)
	}
	if crossTask := taskRunHTTPCall(handler, http.MethodPost, "/v1/tasks/other/run-orders/"+target.ID+"/renew", `{"session_id":"run-session"}`); crossTask.Code != http.StatusConflict {
		t.Fatalf("cross-task renewal status=%d body=%s", crossTask.Code, crossTask.Body.String())
	}
	release := taskRunHTTPCall(handler, http.MethodPost, "/v1/tasks/target/run-orders/"+target.ID+"/release", `{"session_id":"run-session","reason":"local run ended","outcome":"released"}`)
	if release.Code != http.StatusOK {
		t.Fatalf("release status=%d body=%s", release.Code, release.Body.String())
	}
	var released core.WorkOrder
	if err = json.Unmarshal(release.Body.Bytes(), &released); err != nil {
		t.Fatal(err)
	}
	if released.State != core.WorkOrderQueued || released.SessionID != "" || released.ClaimantID != "" {
		t.Fatalf("released=%+v", released)
	}
}

func TestTaskRunHTTPIssuesClaimBoundAgentCredentialAndRevokesAfterSubmission(t *testing.T) {
	server, st, handler := taskRunHTTPFixture(t)
	order := createTaskRunOrder(t, st, "target")
	claim := taskRunHTTPCall(handler, http.MethodPost, "/v1/tasks/target/run-orders/"+order.ID+"/claim", `{"session_id":"run-session","client_token":"run-secret","agent":"codex","model":"gpt"}`)
	if claim.Code != http.StatusOK {
		t.Fatalf("claim status=%d body=%s", claim.Code, claim.Body.String())
	}
	issued := taskRunHTTPCall(handler, http.MethodPost, "/v1/tasks/target/run-orders/"+order.ID+"/agent-credential", `{"session_id":"run-session"}`)
	if issued.Code != http.StatusCreated || !strings.Contains(issued.Body.String(), `"credential":"child-agent-secret"`) || strings.Contains(issued.Body.String(), "local-operator") {
		t.Fatalf("issue status=%d body=%s", issued.Code, issued.Body.String())
	}
	fixture := server.AgentCredentials.(*taskRunAgentCredentialFixture)
	if len(fixture.issued) != 1 {
		t.Fatalf("issued=%+v", fixture.issued)
	}
	binding, bound := store.ParseRunAgentCredentialLabel(fixture.issued[0].Label)
	if fixture.issued[0].UserID != "local-operator" || !bound || binding.WorkspaceID != "demo" || binding.WorkOrderID != order.ID || binding.SessionID != "run-session" {
		t.Fatalf("issued=%+v", fixture.issued)
	}
	// A terminal stage clears claim authority before launcher cleanup. The
	// authenticated owner must still be able to revoke the exact agent token.
	release := taskRunHTTPCall(handler, http.MethodPost, "/v1/tasks/target/run-orders/"+order.ID+"/release", `{"session_id":"run-session","reason":"done","outcome":"released"}`)
	if release.Code != http.StatusOK {
		t.Fatalf("release status=%d body=%s", release.Code, release.Body.String())
	}
	revoked := taskRunHTTPCall(handler, http.MethodDelete, "/v1/tasks/target/run-orders/"+order.ID+"/agent-credential", `{"session_id":"run-session","credential_id":"agent-1"}`)
	if revoked.Code != http.StatusNoContent || len(fixture.revoked) != 1 || fixture.revoked[0] != "agent-1" {
		t.Fatalf("revoke status=%d body=%s revoked=%v", revoked.Code, revoked.Body.String(), fixture.revoked)
	}
}

func TestTaskRunHTTPRenewReportsSameSessionCheckpointRelease(t *testing.T) {
	_, st, handler := taskRunHTTPFixture(t)
	order := createTaskRunOrder(t, st, "checkpoint-run")
	claim := taskRunHTTPCall(handler, http.MethodPost, "/v1/tasks/checkpoint-run/run-orders/"+order.ID+"/claim", `{"session_id":"run-session","client_token":"run-secret","agent":"codex","model":"gpt"}`)
	if claim.Code != http.StatusOK {
		t.Fatalf("claim status=%d body=%s", claim.Code, claim.Body.String())
	}
	release := taskRunHTTPCall(handler, http.MethodPost, "/v1/tasks/checkpoint-run/run-orders/"+order.ID+"/release", `{"session_id":"run-session","reason":"operator checkpoint reached","checkpoint":{"decision_request":"Choose whether to proceed."},"release_cause":"operator_action","outcome":"released"}`)
	if release.Code != http.StatusOK {
		t.Fatalf("release status=%d body=%s", release.Code, release.Body.String())
	}
	var checkpointReleased core.WorkOrder
	if err := json.Unmarshal(release.Body.Bytes(), &checkpointReleased); err != nil || checkpointReleased.Checkpoint == nil || checkpointReleased.Checkpoint.DecisionRequest != "Choose whether to proceed." {
		t.Fatalf("checkpoint release body=%s err=%v", release.Body.String(), err)
	}
	reconcile := taskRunHTTPCall(handler, http.MethodGet, "/v1/tasks/checkpoint-run/run-orders/"+order.ID+"/reconcile?session_id=run-session", "")
	if reconcile.Code != http.StatusOK || !strings.Contains(reconcile.Body.String(), `"released_at_checkpoint":true`) ||
		!strings.Contains(reconcile.Body.String(), `"last_failure_message":"operator checkpoint reached"`) {
		t.Fatalf("reconcile status=%d body=%s", reconcile.Code, reconcile.Body.String())
	}
	wrongReconcile := taskRunHTTPCall(handler, http.MethodGet, "/v1/tasks/checkpoint-run/run-orders/"+order.ID+"/reconcile?session_id=other-session", "")
	if wrongReconcile.Code != http.StatusConflict || !strings.Contains(wrongReconcile.Body.String(), "claim expired or order reassigned") {
		t.Fatalf("wrong reconcile status=%d body=%s", wrongReconcile.Code, wrongReconcile.Body.String())
	}
	renew := taskRunHTTPCall(handler, http.MethodPost, "/v1/tasks/checkpoint-run/run-orders/"+order.ID+"/renew", `{"session_id":"run-session"}`)
	if renew.Code != http.StatusConflict || renew.Header().Get("X-Conveyor-Error-Code") != "work_order_released_checkpoint" ||
		!strings.Contains(renew.Body.String(), "released by this session at an operator checkpoint") {
		t.Fatalf("renew status=%d code=%q body=%s", renew.Code, renew.Header().Get("X-Conveyor-Error-Code"), renew.Body.String())
	}
	wrong := taskRunHTTPCall(handler, http.MethodPost, "/v1/tasks/checkpoint-run/run-orders/"+order.ID+"/renew", `{"session_id":"other-session"}`)
	if wrong.Code != http.StatusConflict || wrong.Header().Get("X-Conveyor-Error-Code") != "" || !strings.Contains(wrong.Body.String(), "claim expired or order reassigned") {
		t.Fatalf("wrong-session status=%d code=%q body=%s", wrong.Code, wrong.Header().Get("X-Conveyor-Error-Code"), wrong.Body.String())
	}
}

func TestTaskRunHTTPReturnsNoWorkAndSurfacesAssigneeRefusal(t *testing.T) {
	_, st, handler := taskRunHTTPFixture(t)
	if response := taskRunHTTPCall(handler, http.MethodGet, "/v1/tasks/missing/run-order", ""); response.Code != http.StatusNotFound {
		t.Fatalf("missing task status=%d body=%s", response.Code, response.Body.String())
	}
	order := createTaskRunOrderAtStage(t, st, "assigned", core.StageSpec, time.Now().UTC())
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.Actor{ID: "user:operator", Role: core.ActorUser}), "demo")
	if err := store.SetMemoryWorkspaceMember(st, "demo", "usr-alice", true); err != nil {
		t.Fatal(err)
	}
	if _, err := taskops.New(st).SetAssignee(ctx, "assigned", "usr-alice"); err != nil {
		t.Fatal(err)
	}
	claim := taskRunHTTPCall(handler, http.MethodPost, "/v1/tasks/assigned/run-orders/"+order.ID+"/claim", `{"session_id":"bob","client_token":"secret","agent":"codex","model":"gpt"}`)
	if claim.Code != http.StatusConflict || !strings.Contains(claim.Body.String(), "task assigned is assigned to usr-alice; only that assignee may claim its work orders") {
		t.Fatalf("assignment status=%d body=%s", claim.Code, claim.Body.String())
	}
	claimed, err := storetest.For(st).ClaimWorkOrder(store.WithWorkspace(t.Context(), "demo"), order.ID, core.WorkOrderClaim{SessionID: "done", ClientToken: "done", OwnerUserID: "usr-alice", Lease: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	claimed.State = core.WorkOrderCompleted
	if err = storetest.For(st).UpdateWorkOrder(store.WithWorkspace(t.Context(), "demo"), claimed, core.WorkOrderCmdSubmitReviewVerdict); err != nil {
		t.Fatal(err)
	}
	if response := taskRunHTTPCall(handler, http.MethodGet, "/v1/tasks/assigned/run-order", ""); response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"state":"running"`) || !strings.Contains(response.Body.String(), `"work_order":{"id":""`) {
		t.Fatalf("no-work status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestTaskRunHTTPProjectsSpecGateCapabilitiesWithoutClaim(t *testing.T) {
	server, st, _ := taskRunHTTPFixture(t)
	server.Memberships = &membershipFixture{roles: map[string]map[string]core.WorkspaceRole{
		"local-operator": {"demo": core.WorkspaceRoleMaintainer},
	}}
	order := createTaskRunOrderAtStage(t, st, "gated", core.StageSpec, time.Now().UTC())
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.Actor{ID: "system", Role: core.ActorSystem}), "demo")
	claimed, err := storetest.For(st).ClaimWorkOrder(ctx, order.ID, core.WorkOrderClaim{SessionID: "finished", ClientToken: "secret", ClaimantID: core.TaskRunClaimantID("local-operator"), OwnerUserID: "local-operator", Lease: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	claimed.State = core.WorkOrderCompleted
	if err = storetest.For(st).UpdateWorkOrder(ctx, claimed, core.WorkOrderCmdSubmitSpec); err != nil {
		t.Fatal(err)
	}
	spec, err := st.CreateSpecVersion(ctx, core.SpecVersion{TaskID: "gated", Content: "## Done criteria\n- attached"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = taskops.New(st).Perform(ctx, "gated", taskops.Command{Kind: core.TaskGateSpec, RecoveryStage: core.StageImplement, ProjectStages: true}); err != nil {
		t.Fatal(err)
	}

	response := taskRunHTTPCall(server.Handler(), http.MethodGet, "/v1/tasks/gated/run-order", "")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var projection workerservice.DispatchOrder
	if err = json.Unmarshal(response.Body.Bytes(), &projection); err != nil {
		t.Fatal(err)
	}
	if projection.Order.ID != "" || projection.Task.State != core.TaskAwaiting || projection.Gate == nil || projection.Gate.Kind != "spec" || projection.Gate.SpecVersion != spec.Version || !projection.Gate.CanOperate || !projection.Gate.CanRequestChanges {
		t.Fatalf("projection=%+v", projection)
	}
	unchanged, err := st.GetWorkOrder(ctx, order.ID)
	if err != nil || unchanged.State != core.WorkOrderCompleted {
		t.Fatalf("waiting changed work order=%+v err=%v", unchanged, err)
	}
	if count, countErr := st.CountEvents(ctx, "gated", "work_order.lease_renewed"); countErr != nil || count != 0 {
		t.Fatalf("waiting renewed claim count=%d err=%v", count, countErr)
	}
}

func TestTaskRunHTTPHidesGateActionsWithoutCapabilities(t *testing.T) {
	server, st, _ := taskRunHTTPFixture(t)
	server.Memberships = &membershipFixture{roles: map[string]map[string]core.WorkspaceRole{
		"local-operator": {"demo": core.WorkspaceRoleViewer},
	}}
	order := createTaskRunOrderAtStage(t, st, "viewer-gate", core.StageImplement, time.Now().UTC())
	ctx := store.WithWorkspace(store.WithActor(t.Context(), store.Actor{ID: "system", Role: core.ActorSystem}), "demo")
	claimed, err := storetest.For(st).ClaimWorkOrder(ctx, order.ID, core.WorkOrderClaim{SessionID: "finished", ClientToken: "secret", Lease: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	claimed.State = core.WorkOrderCompleted
	if err = storetest.For(st).UpdateWorkOrder(ctx, claimed, core.WorkOrderCmdSubmitReviewVerdict); err != nil {
		t.Fatal(err)
	}
	if _, err = taskops.New(st).Perform(ctx, "viewer-gate", taskops.Command{Kind: core.TaskGateSpec, RecoveryStage: core.StageImplement, ProjectStages: true}); err != nil {
		t.Fatal(err)
	}
	response := taskRunHTTPCall(server.Handler(), http.MethodGet, "/v1/tasks/viewer-gate/run-order", "")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"can_operate":false`) || !strings.Contains(response.Body.String(), `"can_request_changes":false`) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestTaskRunHTTPProjectsOnlyTaskAuthoredPendingProposalsWithCapabilities(t *testing.T) {
	server, st, _ := taskRunHTTPFixture(t)
	server.Memberships = &membershipFixture{roles: map[string]map[string]core.WorkspaceRole{
		"local-operator": {"demo": core.WorkspaceRoleOperator},
	}}
	order := createTaskRunOrder(t, st, "proposal-task")
	createTaskRunOrder(t, st, "other-task")
	ctx := store.WithWorkspace(t.Context(), "demo")
	if _, _, err := st.CreateSystemDesign(ctx, core.SystemDesign{ID: "design-run-context", Title: "Run context", Category: "Architecture"}, core.SystemDesignVersion{
		Content: "# Run context\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/httpapi/run.go\n```",
		Origin:  core.SystemDesignOriginImplementation, OriginTaskID: "proposal-task",
	}); err != nil {
		t.Fatal(err)
	}
	decision, err := st.ProposeDecision(ctx, core.Decision{
		Statement: "Keep confirmation authority server-derived.", Context: "The run response is advisory.", AlternativesRejected: "Client-provided authority is spoofable.",
		Origin: core.DecisionOriginImplementation, OriginTaskID: "proposal-task",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.CreateSystemDesign(ctx, core.SystemDesign{ID: "design-other", Title: "Other", Category: "Architecture"}, core.SystemDesignVersion{
		Content: "# Other\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/other/**\n```",
		Origin:  core.SystemDesignOriginImplementation, OriginTaskID: "other-task",
	}); err != nil {
		t.Fatal(err)
	}
	sibling := store.WithWorkspace(t.Context(), "sibling")
	if _, _, err = st.CreateSystemDesign(sibling, core.SystemDesign{ID: "design-sibling", Title: "Sibling", Category: "Architecture"}, core.SystemDesignVersion{
		Content: "# Sibling\n\n```conveyor:governs\n- repo: conveyor\n  paths:\n    - internal/sibling/**\n```",
		Origin:  core.SystemDesignOriginImplementation, OriginTaskID: "proposal-task",
	}); err != nil {
		t.Fatal(err)
	}

	response := taskRunHTTPCall(server.Handler(), http.MethodGet, "/v1/tasks/proposal-task/run-order", "")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var projection workerservice.DispatchOrder
	if err = json.Unmarshal(response.Body.Bytes(), &projection); err != nil {
		t.Fatal(err)
	}
	if projection.Order.ID != order.ID || len(projection.PendingProposals) != 2 {
		t.Fatalf("projection=%+v", projection)
	}
	got := map[string]workerservice.TaskRunProposal{}
	for _, proposal := range projection.PendingProposals {
		got[proposal.Kind] = proposal
		if !proposal.CanConfirm || proposal.ActorHint == "" || proposal.Version < 1 {
			t.Fatalf("incomplete operator proposal=%+v", proposal)
		}
	}
	if got["design"].DocumentID != "design-run-context" || got["decision"].DocumentID != decision.ID || got["decision"].Version != 1 {
		t.Fatalf("proposals=%+v", projection.PendingProposals)
	}
	if strings.Contains(response.Body.String(), "design-other") || strings.Contains(response.Body.String(), "design-sibling") {
		t.Fatalf("cross-task or cross-workspace proposal leaked: %s", response.Body.String())
	}

	server.Memberships = &membershipFixture{roles: map[string]map[string]core.WorkspaceRole{
		"local-operator": {"demo": core.WorkspaceRoleExecutor},
	}}
	response = taskRunHTTPCall(server.Handler(), http.MethodGet, "/v1/tasks/proposal-task/run-order", "")
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &projection) != nil {
		t.Fatalf("executor status=%d body=%s", response.Code, response.Body.String())
	}
	for _, proposal := range projection.PendingProposals {
		if proposal.CanConfirm || proposal.ActorHint != "an operator can confirm" {
			t.Fatalf("executor gained confirmation authority: %+v", proposal)
		}
	}
	unchanged, err := st.GetWorkOrder(ctx, order.ID)
	if err != nil || unchanged.State != core.WorkOrderQueued || unchanged.SessionID != "" {
		t.Fatalf("read mutated order=%+v err=%v", unchanged, err)
	}
}

func TestTaskRunHTTPProjectsPendingPlanRevisionWithoutClaimMutation(t *testing.T) {
	ctx, st, server, taskID, orderID := newPlanRevisionReviewServer(t)
	cfg := &config.Config{Workspace: "demo", Routing: config.Routing{Stages: map[string]config.StageRoute{
		"spec": {Execution: config.ExecutionMCP, Timeout: time.Hour, TimeoutText: "1h"}, "implement": {Execution: config.ExecutionMCP, Timeout: time.Hour, TimeoutText: "1h"},
	}}, Repos: []config.Repo{{Name: "conveyor", URL: "https://example.test/conveyor.git", Base: "main"}}}
	provider := func(context.Context) (*config.Config, error) { return cfg, nil }
	server.ConfigProvider = provider
	server.BearerToken = "user-token"
	server.WorkOrders = &workorder.Service{Store: st, ConfigProvider: provider}
	server.Workers = &workerservice.Service{Store: st, WorkOrders: server.WorkOrders, ConfigProvider: provider}
	server.Memberships = &membershipFixture{roles: map[string]map[string]core.WorkspaceRole{
		"local-operator": {"demo": core.WorkspaceRoleMaintainer},
	}}
	before, err := st.GetWorkOrder(ctx, orderID)
	if err != nil {
		t.Fatal(err)
	}
	response := taskRunHTTPCall(server.Handler(), http.MethodGet, "/v1/tasks/"+taskID+"/run-order", "")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var projection workerservice.DispatchOrder
	if err = json.Unmarshal(response.Body.Bytes(), &projection); err != nil {
		t.Fatal(err)
	}
	if len(projection.PendingProposals) != 1 {
		t.Fatalf("proposals=%+v", projection.PendingProposals)
	}
	proposal := projection.PendingProposals[0]
	if proposal.Kind != "plan_revision" || proposal.DocumentID != taskID || proposal.Title != "Execution plan" || proposal.Version != 1 || !proposal.CanConfirm || proposal.ActorHint == "" {
		t.Fatalf("proposal=%+v", proposal)
	}
	after, err := st.GetWorkOrder(ctx, orderID)
	if err != nil || after.State != before.State || after.SessionID != before.SessionID || !after.LeaseExpiresAt.Equal(before.LeaseExpiresAt) {
		t.Fatalf("projection mutated claim before=%+v after=%+v err=%v", before, after, err)
	}
}

func TestTaskRunHTTPMapsConfigurationAndRepositoryConditions(t *testing.T) {
	for _, endpoint := range []struct {
		name   string
		method string
		path   func(core.WorkOrder) string
		body   string
	}{
		{name: "read", method: http.MethodGet, path: func(core.WorkOrder) string { return "/v1/tasks/target/run-order" }},
		{name: "claim", method: http.MethodPost, path: func(order core.WorkOrder) string { return "/v1/tasks/target/run-orders/" + order.ID + "/claim" }, body: `{"session_id":"run-session","client_token":"secret","agent":"codex","model":"gpt"}`},
	} {
		t.Run(endpoint.name+"/configuration unavailable", func(t *testing.T) {
			server, st, _ := taskRunHTTPFixture(t)
			order := createTaskRunOrder(t, st, "target")
			server.ConfigProvider = func(context.Context) (*config.Config, error) { return nil, fmt.Errorf("configuration exploded") }
			response := taskRunHTTPCall(server.Handler(), endpoint.method, endpoint.path(order), endpoint.body)
			if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "task run configuration unavailable") {
				t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
			}
		})
		t.Run(endpoint.name+"/repository unconfigured", func(t *testing.T) {
			server, st, _ := taskRunHTTPFixture(t)
			order := createTaskRunOrder(t, st, "target")
			server.ConfigProvider = func(context.Context) (*config.Config, error) { return &config.Config{Workspace: "demo"}, nil }
			response := taskRunHTTPCall(server.Handler(), endpoint.method, endpoint.path(order), endpoint.body)
			if response.Code != http.StatusConflict || response.Body.String() != "task repository is not configured\n" {
				t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
			}
		})
	}
}

func TestTaskRunAbandonedInvocationBecomesClaimableAfterLeaseExpiry(t *testing.T) {
	_, st, handler := taskRunHTTPFixture(t)
	order := createTaskRunOrder(t, st, "abandoned")
	claim := taskRunHTTPCall(handler, http.MethodPost, "/v1/tasks/abandoned/run-orders/"+order.ID+"/claim", `{"session_id":"dead-process","client_token":"secret","agent":"codex","model":"gpt","lease_seconds":1}`)
	if claim.Code != http.StatusOK {
		t.Fatalf("claim status=%d body=%s", claim.Code, claim.Body.String())
	}
	time.Sleep(1100 * time.Millisecond)
	next := taskRunHTTPCall(handler, http.MethodGet, "/v1/tasks/abandoned/run-order", "")
	if next.Code != http.StatusOK || !strings.Contains(next.Body.String(), `"id":"`+order.ID+`"`) {
		t.Fatalf("expired run status=%d body=%s", next.Code, next.Body.String())
	}
}
